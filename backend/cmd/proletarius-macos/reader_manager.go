package main

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ElMostafaIdrassi/goscard"
	"github.com/damonto/euicc-go/driver"
	euiccccid "github.com/damonto/euicc-go/driver/ccid"
	"github.com/damonto/uicc-go/ccid"
	"github.com/damonto/uicc-go/usim"
	usimcard "github.com/damonto/uicc-go/usim/card"
	usimcommand "github.com/damonto/uicc-go/usim/command"
	"github.com/damonto/uicc-go/usim/simfile"
	"proletarius.local/core/internal/esim"
	"proletarius.local/core/internal/estkble"
	"proletarius.local/core/internal/modem"
)

// bleConnectGracePeriod is the window after a BLE reader connect during which
// ReconcilePresence skips its liveness check. The Swift BLEBridge needs this
// time to complete GATT service discovery, characteristic discovery, notify
// enable, and the claim/power-on handshake before it reports connectedID.
const (
	bleConnectGracePeriod         = 15 * time.Second
	readerBLESnapshotProbeTimeout = 1500 * time.Millisecond
)

// ---------------------------------------------------------------------------
// ReaderManager — PC/SC 读卡器管理器

// ---------------------------------------------------------------------------

// readerManager 管理所有 PC/SC 读卡器的连接与 SIM 卡操作。
// 它同时支持两种用途：
//   - eSIM (eUICC) 管理：通过 euicc-go 的 CCIDReader 接入 esim.Manager (custom transport)
//   - USIM 读取 + AKA 鉴权：通过 uicc-go 的 usim.Reader 读取 IMSI/ICCID 等信息并执行 EAP-AKA
type readerManager struct {
	mu sync.Mutex
	// authMu serializes a complete physical-card authentication transaction,
	// including opening PC/SC, selecting the USIM/ISIM application, issuing
	// AUTHENTICATE, and closing the connection.  PC/SC readers commonly allow
	// multiple handles but not concurrent application selection on one card.
	authMu sync.Mutex

	// nativeLeaseMu protects the Rust reader-session lease. authMu stays held
	// except for one explicit, token- and generation-bound Rust AKA hand-off;
	// unrelated Go LPA/AKA operations remain excluded for the lease lifetime.
	nativeLeaseMu sync.Mutex
	nativeLease   *nativeReaderSessionLease

	// 已连接的读卡器名称
	readerName string
	// connectTime records when the current reader was connected. BLE GATT
	// handshake (service/characteristic discovery + claim + power-on) takes
	// several seconds; ReconcilePresence must not tear down the session
	// during this window.
	connectTime time.Time
	// activeBLEGeneration identifies the currently selected BLE reader. A card
	// operation records that generation in bleBusyGeneration rather than a bare
	// boolean, so an old APDU release can never clear the busy state belonging
	// to a reconnect/replacement reader.
	activeBLEGeneration atomic.Uint64
	bleBusyGeneration   atomic.Uint64
	// A read-only BLE status probe holds authMu to keep its bridge request
	// ordered with card APDUs. Record its unique owner so an explicit logical
	// disconnect can invalidate that reader generation without mistaking the
	// probe for a card operation. The probe's eventual response is discarded by
	// the existing generation check.
	blePresenceProbeSequence   atomic.Uint64
	blePresenceProbeID         atomic.Uint64
	blePresenceProbeGeneration atomic.Uint64
	// generation changes whenever the active reader identity changes. It
	// prevents an in-flight AKA transaction from publishing a stale result
	// after disconnect/reconnect or reader replacement.
	generation uint64
	// hotplug* is owned by mu. It records only consecutive, independently
	// observed absences for the background hot-unplug monitor; dashboard
	// snapshots never update it or tear down a session.
	hotplugReader     string
	hotplugGeneration uint64
	hotplugAbsences   int

	// eSIM 通道由 SmartCardChannelFactory 按需创建并管理生命周期，
	// 不再持有持久 euiccReader——避免与 factory 创建的并发 PC/SC 连接冲突。
	esimManager *esim.Manager // esim.Manager 无 Close 方法，断开时置 nil 释放引用即可

	// persistentCCID is the shared PC/SC session reused by all LPA operations
	// on non-ESTKme CCID readers (see ccidPersistentExchanger). Closed on
	// disconnect; may be nil.
	persistentCCID *ccidPersistentExchanger

	// 缓存的 USIM 信息
	usimMu     sync.RWMutex
	cachedUSIM *usimInfo
}

var errReaderCardOperationActive = errors.New("读卡器正由卡片操作使用")

// usimInfo 缓存从物理 SIM 卡读取的信息
type usimInfo struct {
	ICCID           string    `json:"iccid"`
	IMSI            string    `json:"imsi"`
	PhoneNumber     string    `json:"phone_number,omitempty"`
	OperatorName    string    `json:"operator_name,omitempty"`
	OperatorSource  string    `json:"operator_source,omitempty"`
	MCC             string    `json:"mcc"`
	MNC             string    `json:"mnc"`
	GID1            string    `json:"gid1,omitempty"`
	GID2            string    `json:"gid2,omitempty"`
	ApplicationAID  string    `json:"application_aid,omitempty"`
	SMSC            string    `json:"smsc,omitempty"`
	SMSCPSI         string    `json:"-"`
	SMSCTarget      string    `json:"-"`
	PrivateIdentity string    `json:"private_identity,omitempty"` // ISIM IMPI
	PublicIdentity  string    `json:"public_identity,omitempty"`  // ISIM IMPU
	HomeDomain      string    `json:"home_domain,omitempty"`      // ISIM Domain
	LoadedAt        time.Time `json:"loaded_at"`
}

func (info *usimInfo) selectorSPN() string {
	if info != nil && info.OperatorSource == "ef_spn" {
		return strings.TrimSpace(info.OperatorName)
	}
	return ""
}

type usimCarrierSelectors struct {
	applicationAID []byte
	phoneNumber    string
	operatorName   string
	operatorSource string
	gid2           string
}

// readUSIMCarrierSelectors is shared by PC/SC and module-backed SIMs. Carrier
// selection must see the same EF-MSISDN, EF-SPN and EF-GID2 metadata regardless
// of which transport supplied the APDU channel.
func readUSIMCarrierSelectors(ctx context.Context, reader usimcard.Reader) usimCarrierSelectors {
	var result usimCarrierSelectors
	if reader == nil {
		return result
	}
	applications, err := reader.ListApplications(ctx)
	if err != nil {
		return result
	}
	result.applicationAID = findApplicationAID(applications, usimApplicationAIDPrefix)
	if len(result.applicationAID) == 0 {
		return result
	}
	app := usimcommand.App{Reader: reader, AID: result.applicationAID}
	if records, readErr := app.LinearFixed(ctx, []byte{0x6F, 0x40}); readErr == nil {
		result.phoneNumber = firstMSISDN(records)
	}
	if rawSPN, readErr := app.Transparent(ctx, []byte{0x6F, 0x46}); readErr == nil {
		if decoded, decodeErr := modem.DecodeEFSPN(rawSPN); decodeErr == nil {
			result.operatorName = strings.TrimSpace(decoded)
			result.operatorSource = "ef_spn"
		}
	}
	if rawGID2, readErr := app.Transparent(ctx, []byte{0x6F, 0x3F}); readErr == nil {
		result.gid2 = strings.ToUpper(hex.EncodeToString(bytes.TrimRight(rawGID2, "\xFF")))
	}
	if result.operatorName == "" {
		if records, readErr := app.LinearFixed(ctx, []byte{0x6F, 0xC5}); readErr == nil {
			for index, record := range records {
				if decoded, ok := modem.DecodePNNRecord(index+1, record); ok {
					result.operatorName = strings.TrimSpace(decoded.FullName)
					if result.operatorName == "" {
						result.operatorName = strings.TrimSpace(decoded.ShortName)
					}
					if result.operatorName != "" {
						result.operatorSource = "ef_pnn"
						break
					}
				}
			}
		}
	}
	return result
}

// readerStatus 读卡器连接状态
type readerStatus struct {
	Connected  bool   `json:"connected"`
	ReaderName string `json:"reader_name,omitempty"`
	// Generation identifies one immutable reader/card observation. It changes
	// on reconnect, replacement, and an observed ICCID change, so consumers
	// cannot accidentally combine a live session from a previous card with the
	// current reader information.
	Generation  uint64 `json:"reader_generation,omitempty"`
	HasCard     bool   `json:"has_card"`
	CardType    string `json:"card_type,omitempty"` // "euicc" | "physical_sim" | "unknown"
	HasUSIMInfo bool   `json:"has_usim_info"`
	HasESIM     bool   `json:"has_esim"` // esim.Manager 是否可用
}

func newReaderManager() *readerManager {
	return &readerManager{}
}

func (rm *readerManager) markBLEBusy(generation uint64) {
	if generation != 0 && rm.activeBLEGeneration.Load() == generation {
		rm.bleBusyGeneration.Store(generation)
	}
}

func (rm *readerManager) clearBLEBusy(generation uint64) {
	if generation != 0 {
		rm.bleBusyGeneration.CompareAndSwap(generation, 0)
	}
}

func (rm *readerManager) bleOperationBusy() bool {
	active := rm.activeBLEGeneration.Load()
	return active != 0 && rm.bleBusyGeneration.Load() == active
}

// beginCardOperation serializes every complete eUICC management exchange with
// USIM/ISIM authentication and identity reads. The LPA and AKA stacks open
// separate PC/SC handles, but application selection still changes shared card
// state on many readers; allowing them to overlap can strand both sessions
// until the device is physically reconnected.
func (rm *readerManager) beginCardOperation() func() {
	// Snapshot the owner before taking authMu. Re-locking rm.mu after authMu
	// would invert loadUSIMInfoLocked's mu -> authMu order; the generation
	// compare gives the same stale-owner protection without an AB-BA deadlock.
	bleGeneration := rm.activeBLEGeneration.Load()
	rm.authMu.Lock()
	rm.markBLEBusy(bleGeneration)
	return func() {
		rm.clearBLEBusy(bleGeneration)
		rm.authMu.Unlock()
	}
}

// beginCardOperationContext is the cancellable form used by service calls.
// Unlike beginCardOperation, it won't leave a request blocked behind a long
// profile transfer after its caller has timed out or disconnected.
func (rm *readerManager) beginCardOperationContext(ctx context.Context) (func(), error) {
	return rm.beginCardOperationAdmission(ctx, false)
}

// Only the multi-profile owner can bypass the legacy admission gate. It still
// takes the same physical APDU mutex and waits for genuine operation completion.
func (rm *readerManager) beginOwnedCardOperationContext(ctx context.Context) (func(), error) {
	return rm.beginCardOperationAdmission(ctx, true)
}

func (rm *readerManager) beginCardOperationAdmission(ctx context.Context, owner bool) (func(), error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	bleGeneration := rm.activeBLEGeneration.Load()
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		readerVoWiFiMu.Lock()
		if !owner && readerMultiStandbyActive {
			readerVoWiFiMu.Unlock()
			return nil, errors.New("1 卡 N 待正在占用读卡器，请先停止多待")
		}
		acquired := rm.authMu.TryLock()
		readerVoWiFiMu.Unlock()
		if acquired {
			if err := ctx.Err(); err != nil {
				rm.authMu.Unlock()
				return nil, err
			}
			rm.markBLEBusy(bleGeneration)
			return func() {
				rm.clearBLEBusy(bleGeneration)
				rm.authMu.Unlock()
			}, nil
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-ticker.C:
		}
	}
}

// readerESIMManagerOptions is shared by initial connection and session rebuild
// so a reconnected reader cannot silently acquire a different switch policy.
func (rm *readerManager) readerESIMManagerOptions(readerName string) esim.ManagerOptions {
	// Match standalone CCID/BLE LPA behavior: the reader's brand does not
	// identify the inserted eUICC or its proactive REFRESH support. Reports
	// from a 9eSIM in ESTKme-RED returned undefinedError (127) with the old
	// refresh=true policy; this alone does not prove its rejection cause.
	// Keep refresh=false here; readerESIMSwitch separately performs the
	// existing validated reset and confirms the actually presented USIM ICCID.
	// Module AT/QMI managers retain their independent refresh configuration.
	return esim.ManagerOptions{
		DeviceID:             "ccid-reader",
		Transport:            "custom",
		SwitchUseRefreshTrue: false,
		SmartCardChannelFactory: func() (driver.SmartCardChannel, error) {
			return rm.newReaderChannel(readerName)
		},
	}
}

// newReaderChannel returns the SmartCardChannel used by eSIM operations for
// the given reader. For BLE readers it wraps the bridge client.
func (rm *readerManager) newReaderChannel(name string) (driver.SmartCardChannel, error) {
	if isBLEReader(name) {
		return estkble.NewChannel(readerBLEClient()), nil
	}
	if !isESTKmeReader(name) {
		// Generic CCID readers (e.g. "Generic EMV Smartcard Reader") are
		// driven into a permanently unresponsive state by the per-operation
		// connect/TERMINAL-CAPABILITY/disconnect churn of the euicc-go CCID
		// driver. Reuse one persistent PC/SC session instead: the estkble
		// channel provides the MANAGE CHANNEL / SELECT AID logic on top of
		// the shared exchanger, and a desynchronized session is re-opened
		// automatically on the next exchange.
		return estkble.NewChannel(rm.persistentCCIDExchanger(name)), nil
	}
	reader, err := euiccccid.NewWithReader(name)
	if err != nil {
		return nil, err
	}
	if err := reader.Connect(); err != nil {
		if !isUnresponsiveCardError(err) {
			return nil, err
		}
		// Some CCID readers (e.g. "Generic EMV Smartcard Reader") leave the
		// card reporting SCARD_W_UNRESPONSIVE_CARD after an eUICC-internal reset
		// or a prior power-down. Revive it once with a cold reset, then retry.
		log.Printf("reader %s reported unresponsive card, attempting revival", name)
		if reviveErr := reviveUnresponsiveCard(name); reviveErr != nil {
			log.Printf("reader %s revival failed: %v", name, reviveErr)
			return nil, err
		}
		time.Sleep(300 * time.Millisecond)
		reader, err = euiccccid.NewWithReader(name)
		if err != nil {
			return nil, err
		}
		if err := reader.Connect(); err != nil {
			return nil, err
		}
	}
	return ownReaderChannel(reader), nil
}

// persistentCCIDExchanger returns (creating if needed) the shared PC/SC
// session for the given non-ESTKme CCID reader.
func (rm *readerManager) persistentCCIDExchanger(name string) *ccidPersistentExchanger {
	rm.mu.Lock()
	defer rm.mu.Unlock()
	if rm.persistentCCID != nil && rm.persistentCCID.name != name {
		rm.persistentCCID.Close()
		rm.persistentCCID = nil
	}
	if rm.persistentCCID == nil {
		rm.persistentCCID = newCCIDPersistentExchanger(name)
	}
	return rm.persistentCCID
}

// ListReaders 列出所有可用的 PC/SC 读卡器
func (rm *readerManager) ListReaders() ([]string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return rm.listReaders(ctx)
}

func (rm *readerManager) listReaders(ctx context.Context) ([]string, error) {
	readers, err := ccid.ListReaders(ctx)
	if ble := listConnectedBLEReader(ctx); ble != "" {
		readers = append(readers, ble)
	}
	if err != nil && len(readers) == 0 {
		return nil, err
	}
	return readers, nil
}

// ReconcilePresence drops the cached reader session when the selected PC/SC
// device is no longer present in a fresh enumeration. PC/SC handles are opened
// per operation, so readerName is otherwise only a logical selection and would
// remain stale after a USB reader is unplugged.
func (rm *readerManager) ReconcilePresence(readers []string) bool {
	// The explicit Rust lease owns the reader identity for its full lifetime.
	// A transient inventory miss must not tear down that session; the native
	// owner will release the lease after its transport reports a real failure.
	if nativeReaderLeaseWindowActive() {
		return false
	}
	rm.mu.Lock()
	defer rm.mu.Unlock()
	if nativeReaderLeaseWindowActive() {
		return false
	}

	if rm.readerName == "" {
		return false
	}
	if isBLEReader(rm.readerName) {
		// BLE bridge main thread is single-threaded: while a GATT handshake
		// or APDU exchange is in progress, the bridge cannot answer a status
		// probe. Two failure modes:
		//   1. GATT handshake (connect → discover → claim → power-on):
		//      connectedID is nil until the handshake sets it. Use the
		//      connect-time grace period.
		//   2. APDU exchange (switch/download/AKA): bleBusy is set.
		// In both cases, skip the liveness check entirely — tearing down
		// the session would guarantee failure.
		if time.Since(rm.connectTime) < bleConnectGracePeriod {
			return false
		}
		if rm.bleOperationBusy() {
			return false
		}
		ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
		defer cancel()
		reader, err := connectedBLEReader(ctx)
		if err != nil {
			// Bridge availability is not proof of physical removal. Keep the
			// selection until a later, successful status reply says no reader.
			return false
		}
		if reader == rm.readerName {
			return false
		}
		// A BLE identity in a caller's cached inventory is not proof of a live
		// GATT/APDU session. Drop the selected card when the bridge disagrees.
		log.Printf("BLE reader disconnected; clearing session: %s", rm.readerName)
		_ = rm.disconnectLocked()
		return true
	}
	for _, reader := range readers {
		if reader == rm.readerName {
			return false
		}
	}
	log.Printf("CCID reader disappeared; clearing session: %s", rm.readerName)
	_ = rm.disconnectLocked()
	return true
}

// readerPresence is an observation of the selected reader. It deliberately
// has an indeterminate state: a BLE bridge cannot answer a status request
// while its GATT handshake or an APDU exchange owns the main-thread bridge.
// Treating that brief interval as absent would make a read-only status poll
// publish a false disconnect.
type readerPresence uint8

const (
	readerPresenceAbsent readerPresence = iota
	readerPresencePresent
	readerPresenceIndeterminate
)

type readerPresenceObservation struct {
	selected  string
	presence  readerPresence
	bleReader string
}

// observePresence reports whether the currently selected reader is visible
// without changing readerManager state. The Rust device-snapshot endpoint is
// polled frequently and must never close a card session or stop VoWiFi merely
// because one inventory sample is incomplete. Mutation-bearing hot-unplug
// cleanup remains in ReconcilePresence, which is used by /api/reader/list.
func (rm *readerManager) observePresence(ctx context.Context, readers []string) readerPresenceObservation {
	return rm.observePresenceFromInventory(ctx, readers, true)
}

// observeHotplugPresence consumes a completed, source-specific background
// monitor observation. Unlike snapshot observation it does not make a second
// BLE bridge request, so one bounded monitor tick never stacks a PC/SC and a
// GATT status timeout.
func (rm *readerManager) observeHotplugPresence(readers []string) readerPresenceObservation {
	return rm.observePresenceFromInventory(context.Background(), readers, false)
}

func (rm *readerManager) observePresenceFromInventory(ctx context.Context, readers []string, verifyBLE bool) readerPresenceObservation {
	if nativeReaderLeaseWindowActive() {
		rm.mu.Lock()
		selected := rm.readerName
		rm.mu.Unlock()
		if selected == "" {
			return readerPresenceObservation{presence: readerPresenceAbsent}
		}
		return readerPresenceObservation{selected: selected, presence: readerPresenceIndeterminate}
	}
	rm.mu.Lock()
	selected := rm.readerName
	generation := rm.generation
	connectedAt := rm.connectTime
	if selected == "" {
		rm.mu.Unlock()
		return readerPresenceObservation{presence: readerPresenceAbsent}
	}
	// A card-management or authentication exchange owns authMu. Treat every
	// reader type as temporarily indeterminate while it is held: an operation
	// may reset/reselect the card and a competing inventory conclusion must not
	// tear it down. TryLock avoids waiting behind a real APDU transaction.
	if !rm.authMu.TryLock() {
		rm.mu.Unlock()
		return readerPresenceObservation{selected: selected, presence: readerPresenceIndeterminate}
	}
	if !isBLEReader(selected) {
		rm.authMu.Unlock()
		rm.mu.Unlock()
		for _, reader := range readers {
			if reader == selected {
				return readerPresenceObservation{selected: selected, presence: readerPresencePresent}
			}
		}
		return readerPresenceObservation{selected: selected, presence: readerPresenceAbsent}
	}

	if time.Since(connectedAt) < bleConnectGracePeriod || rm.bleOperationBusy() {
		rm.authMu.Unlock()
		rm.mu.Unlock()
		return readerPresenceObservation{selected: selected, presence: readerPresenceIndeterminate}
	}
	if !verifyBLE {
		rm.authMu.Unlock()
		rm.mu.Unlock()
		for _, reader := range readers {
			if reader == selected {
				return readerPresenceObservation{selected: selected, presence: readerPresencePresent}
			}
		}
		return readerPresenceObservation{selected: selected, presence: readerPresenceAbsent}
	}
	// Hold the same physical-card gate used by APDU operations through this
	// short status request. Without it, a new APDU could start immediately
	// after TryLock and force the Swift bridge to queue this probe behind a
	// long exchange. Do not retain rm.mu across bridge I/O: Disconnect and a
	// profile switch must be able to advance the generation while a snapshot
	// request is pending.
	probeID := rm.blePresenceProbeSequence.Add(1)
	rm.blePresenceProbeGeneration.Store(generation)
	rm.blePresenceProbeID.Store(probeID)
	rm.mu.Unlock()
	if ctx == nil {
		ctx = context.Background()
	}
	ctx, cancel := context.WithTimeout(ctx, readerBLESnapshotProbeTimeout)
	reader, err := connectedBLEReader(ctx)
	cancel()
	if rm.blePresenceProbeID.CompareAndSwap(probeID, 0) {
		rm.blePresenceProbeGeneration.CompareAndSwap(generation, 0)
	}
	rm.authMu.Unlock()

	// A card operation may take rm.mu after we release authMu. Never make an
	// expired snapshot wait behind that operation just to re-check identity;
	// preserving this sample as indeterminate is safer than publishing a
	// completed probe for a potentially replaced reader.
	if !rm.mu.TryLock() {
		return readerPresenceObservation{selected: selected, presence: readerPresenceIndeterminate}
	}
	defer rm.mu.Unlock()
	if rm.readerName != selected || rm.generation != generation {
		if rm.readerName == "" {
			return readerPresenceObservation{presence: readerPresenceAbsent}
		}
		return readerPresenceObservation{selected: rm.readerName, presence: readerPresenceIndeterminate}
	}
	if err != nil {
		return readerPresenceObservation{selected: selected, presence: readerPresenceIndeterminate}
	}
	if reader == selected {
		return readerPresenceObservation{selected: selected, presence: readerPresencePresent, bleReader: reader}
	}
	return readerPresenceObservation{selected: selected, presence: readerPresenceAbsent, bleReader: reader}
}

// reconcileHotUnplug consumes one observation from the dedicated background
// monitor. Unlike ReconcilePresence it requires consecutive confirmed misses
// before dropping a session, so a transient PC/SC scan cannot interrupt a
// live VoWiFi call. It deliberately performs no I/O itself; callers provide
// one completed inventory sample per invocation.
func (rm *readerManager) reconcileHotUnplug(readers []string, requiredMisses int) (uint64, bool) {
	if requiredMisses < 1 {
		requiredMisses = 1
	}
	observation := rm.observeHotplugPresence(readers)
	return rm.applyHotplugObservation(observation, requiredMisses)
}

func (rm *readerManager) applyHotplugObservation(observation readerPresenceObservation, requiredMisses int) (uint64, bool) {
	rm.mu.Lock()
	defer rm.mu.Unlock()
	if nativeReaderLeaseWindowActive() {
		rm.clearHotplugObservationLocked()
		return 0, false
	}
	if rm.readerName == "" || observation.selected != rm.readerName {
		rm.clearHotplugObservationLocked()
		return 0, false
	}
	if observation.presence != readerPresenceAbsent {
		rm.clearHotplugObservationLocked()
		return 0, false
	}
	if rm.hotplugReader != rm.readerName || rm.hotplugGeneration != rm.generation {
		rm.hotplugReader = rm.readerName
		rm.hotplugGeneration = rm.generation
		rm.hotplugAbsences = 1
	} else if rm.hotplugAbsences < requiredMisses {
		rm.hotplugAbsences++
	}
	if rm.hotplugAbsences < requiredMisses {
		return 0, false
	}
	// A card exchange can begin after the read-only observation releases its
	// authMu probe. Hold that gate through teardown, or an APDU may be using the
	// selected session while this monitor disconnects it. TryLock avoids an
	// authMu -> rm.mu waiter deadlock and treats the sample as indeterminate.
	if !rm.authMu.TryLock() {
		rm.clearHotplugObservationLocked()
		return 0, false
	}
	defer rm.authMu.Unlock()

	log.Printf("reader hot-unplug confirmed after %d observations: %s", rm.hotplugAbsences, rm.readerName)
	removedGeneration := rm.generation
	rm.clearHotplugObservationLocked()
	_ = rm.disconnectLocked()
	return removedGeneration, true
}

func (rm *readerManager) clearHotplugObservationLocked() {
	rm.hotplugReader = ""
	rm.hotplugGeneration = 0
	rm.hotplugAbsences = 0
}

func (rm *readerManager) hasSelectedReader() bool {
	rm.mu.Lock()
	defer rm.mu.Unlock()
	return rm.readerName != ""
}

// hotplugInventory collects exactly the transport that owns the selected
// reader. This avoids waking PC/SC when only a BLE reader is selected, and
// avoids treating a PC/SC timeout as a BLE disconnect (or vice versa).
func (rm *readerManager) hotplugInventory(ctx context.Context) ([]string, error) {
	rm.mu.Lock()
	selected := rm.readerName
	if selected == "" {
		rm.mu.Unlock()
		return nil, nil
	}
	if !rm.authMu.TryLock() {
		rm.mu.Unlock()
		return nil, errReaderBusy
	}
	rm.authMu.Unlock()
	rm.mu.Unlock()
	if !isBLEReader(selected) {
		return ccid.ListReaders(ctx)
	}
	reader, err := connectedBLEReader(ctx)
	if err != nil {
		return nil, err
	}
	if reader == "" {
		return []string{}, nil
	}
	return []string{reader}, nil
}

// Connect 连接到指定读卡器
// 连接后自动探测卡类型并初始化相应子系统：
//   - eUICC：创建 euicc-go CCIDReader → esim.Manager (custom transport)
//   - USIM：读取 ICCID/IMSI/MCC/MNC 等
func (rm *readerManager) Connect(readerName string) error {
	rm.mu.Lock()
	defer rm.mu.Unlock()

	readerName = strings.TrimSpace(readerName)
	if readerName == "" {
		return errors.New("读卡器名称不能为空")
	}

	if rm.readerName != "" && rm.readerName == readerName {
		return nil // 已连接
	}
	// Card reads can hold authMu and then need rm.mu to open the shared CCID
	// channel. Never wait for authMu with rm.mu held, and do not replace the
	// selected reader while a card operation is using it.
	if !rm.authMu.TryLock() {
		return errReaderCardOperationActive
	}
	defer rm.authMu.Unlock()

	// 先断开旧连接
	if rm.readerName != "" {
		rm.disconnectLocked()
	}

	// 验证读卡器存在
	readers, err := rm.ListReaders()
	if err != nil {
		return fmt.Errorf("枚举读卡器失败: %w", err)
	}
	found := false
	for _, r := range readers {
		if r == readerName {
			found = true
			break
		}
	}
	if !found {
		return fmt.Errorf("读卡器 %q 未找到", readerName)
	}

	// 1. 创建 esim.Manager (custom transport, SmartCardChannelFactory 按需创建独立的 PC/SC 连接)
	//    每次 LPA 操作创建新 CCIDReader + Connect，操作完毕后 Disconnect 释放，
	//    符合 euicc-go 的设计模式（ModemChannel 也是每次 createLPAWithAID 新建通道）。
	esimMgr, err := esim.NewManager(rm.readerESIMManagerOptions(readerName))
	if err != nil {
		// eUICC 不可用不阻断连接——可能是物理 SIM 卡而非 eUICC
		log.Printf("eSIM manager unavailable via CCID reader (may be physical SIM): %v", err)
	} else {
		rm.esimManager = esimMgr
	}

	rm.readerName = readerName
	rm.connectTime = time.Now()
	rm.generation++
	if isBLEReader(readerName) {
		rm.activeBLEGeneration.Store(rm.generation)
		rm.markBLEBusy(rm.generation)
	} else {
		rm.activeBLEGeneration.Store(0)
		rm.bleBusyGeneration.Store(0)
	}

	// 2. 尝试读取 USIM 信息（非致命——可能卡未就绪）
	if err := rm.loadUSIMInfoWithCardLeaseLocked(); err != nil {
		log.Printf("USIM info read failed on connect (may retry later): %v", err)
	}
	// USIM read (if any) has completed; the BLE bridge is idle again.
	if isBLEReader(readerName) {
		rm.clearBLEBusy(rm.generation)
	}

	log.Printf("CCID reader connected: %s (eSIM=%v)", readerName, rm.esimManager != nil)
	return nil
}

// Disconnect 断开读卡器连接
func (rm *readerManager) Disconnect() error {
	rm.mu.Lock()
	defer rm.mu.Unlock()
	if rm.readerName == "" {
		return nil
	}
	if !rm.authMu.TryLock() {
		// The status probe owns authMu only to serialize its read-only bridge
		// request with APDUs. It does not mutate the card; invalidate its reader
		// generation so the late response is dropped rather than blocking the
		// user's explicit disconnect. Real card/APDU owners have no probe ID and
		// still return the normal busy error.
		if isBLEReader(rm.readerName) &&
			rm.blePresenceProbeID.Load() != 0 &&
			rm.blePresenceProbeGeneration.Load() == rm.generation {
			return rm.disconnectLocked()
		}
		return errReaderCardOperationActive
	}
	defer rm.authMu.Unlock()
	return rm.disconnectLocked()
}

// RebuildSession tears down the current eSIM/USIM session state and rebuilds
// it in place for the same reader. Unlike Disconnect+Connect, the reader
// stays logically connected (readerName is preserved), so the UI connection
// state never flickers "disconnected → reconnecting" during a profile
// switch. powerCycle (optional, run with all locks released between teardown
// and rebuild) resets the card so the newly enabled profile becomes visible;
// its error is logged but non-fatal — the post-rebuild identity wait exposes
// any real failure.
func (rm *readerManager) RebuildSession(powerCycle func() error) error {
	rm.mu.Lock()
	readerName := rm.readerName
	if readerName == "" {
		rm.mu.Unlock()
		return errors.New("读卡器未连接")
	}
	// Teardown: drop cached service state so the rebuild below recreates it.
	if rm.persistentCCID != nil {
		rm.persistentCCID.Close()
		rm.persistentCCID = nil
	}
	rm.usimMu.Lock()
	rm.cachedUSIM = nil
	rm.usimMu.Unlock()
	rm.esimManager = nil
	rm.mu.Unlock()

	if powerCycle != nil {
		if err := powerCycle(); err != nil {
			log.Printf("reader %s rebuild power-cycle warning: %v", readerName, err)
		}
		// Let the card discharge before the rebuild re-opens the session.
		time.Sleep(500 * time.Millisecond)
	}

	// Rebuild (same recipe as the tail of Connect, reader unchanged).
	rm.mu.Lock()
	defer rm.mu.Unlock()
	esimMgr, mgrErr := esim.NewManager(rm.readerESIMManagerOptions(readerName))
	if mgrErr != nil {
		log.Printf("eSIM manager unavailable via CCID reader (may be physical SIM): %v", mgrErr)
	} else {
		rm.esimManager = esimMgr
	}
	rm.generation++
	if isBLEReader(readerName) {
		rm.activeBLEGeneration.Store(rm.generation)
		rm.bleBusyGeneration.Store(0)
	} else {
		rm.activeBLEGeneration.Store(0)
		rm.bleBusyGeneration.Store(0)
	}
	// Non-fatal: right after a power cycle the card may not answer yet;
	// the post-switch ICCID wait keeps refreshing until it does.
	if err := rm.loadUSIMInfoLocked(); err != nil {
		log.Printf("USIM info read failed on rebuild (may retry later): %v", err)
	}
	log.Printf("CCID reader session rebuilt: %s (eSIM=%v)", readerName, rm.esimManager != nil)
	return nil
}

// isESTKmeReader reports whether the reader is an ESTKme device (USB CCID or
// BLE). This identifies existing reader-specific reset/setup paths, not the
// inserted eUICC's ability to handle a proactive REFRESH request.
func isESTKmeReader(name string) bool {
	return strings.Contains(strings.ToLower(strings.TrimSpace(name)), "estkme")
}

// isUnresponsiveCardError reports whether err is macOS PC/SC
// SCARD_W_UNRESPONSIVE_CARD (0x80100066): the reader is present, but the card
// is not answering (ATR/PPS failure). goscard surfaces it as a formatted
// error string, so match on both the hex code and the symbolic name.
func isUnresponsiveCardError(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "0x80100066") ||
		strings.Contains(msg, "SCARD_W_UNRESPONSIVE_CARD")
}

// reviveUnresponsiveCard attempts to recover a card that reports
// SCARD_W_UNRESPONSIVE_CARD. An exclusive connect forces smartcardd to
// cold-reset the card, and resetting on disconnect keeps it powered and
// ready for the next reader session.
func reviveUnresponsiveCard(readerName string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	// SCardResetCard keeps VCC on. SCardUnpowerCard would power the card
	// down, and some cheap CCID readers cannot restore VCC afterwards,
	// leaving the card dead until it is physically re-seated.
	return ccid.ResetCard(ctx, readerName, goscard.SCardResetCard)
}

// PowerCycleCard resets the selected card through its reader transport.
// Closing the uicc-go reader alone uses SCARD_LEAVE_CARD and may retain the
// old USIM application. The caller must still confirm the resulting identity.
func (rm *readerManager) PowerCycleCard(readerName string) error {
	readerName = strings.TrimSpace(readerName)
	if readerName == "" {
		return errors.New("读卡器名称不能为空")
	}
	if isBLEReader(readerName) {
		// A delayed profile-switch power cycle must not mark a replacement BLE
		// reader busy merely because it happens to be the current atomic owner.
		rm.mu.Lock()
		bleGeneration := uint64(0)
		if rm.readerName == readerName {
			bleGeneration = rm.generation
		}
		rm.mu.Unlock()
		rm.markBLEBusy(bleGeneration)
		defer rm.clearBLEBusy(bleGeneration)
		ctx, cancel := context.WithTimeout(context.Background(), 35*time.Second)
		defer cancel()
		return readerBLEClient().Reset(ctx)
	}
	// Drop the persistent CCID session first: the exclusive connect below
	// would otherwise fail with a sharing violation while it is open.
	rm.mu.Lock()
	if rm.persistentCCID != nil {
		rm.persistentCCID.Close()
		rm.persistentCCID = nil
	}
	rm.mu.Unlock()
	if !isESTKmeReader(readerName) {
		// A safe PC/SC reset has not been validated for this reader. Refuse
		// speculative power changes; some previously tested combinations
		// required physical reinsertion after reset/unpower operations.
		return errors.New("尚未验证该读卡器的软件复位方式；如卡片身份未更新，请拔插读卡器后重新连接")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 35*time.Second)
	defer cancel()
	// Use uicc-go's own PC/SC reference count so a concurrent enumeration
	// cannot unload the library before this exclusive reset has completed.
	if err := ccid.ResetCard(ctx, readerName, goscard.SCardUnpowerCard); err != nil {
		return fmt.Errorf("断电复位卡片失败: %w", err)
	}
	return nil
}

func (rm *readerManager) disconnectLocked() error {
	var errs []error

	if rm.persistentCCID != nil {
		rm.persistentCCID.Close()
		rm.persistentCCID = nil
	}
	rm.usimMu.Lock()
	rm.cachedUSIM = nil
	rm.usimMu.Unlock()
	rm.esimManager = nil // 无 Close 方法，置 nil 释放引用

	rm.readerName = ""
	rm.connectTime = time.Time{}
	rm.activeBLEGeneration.Store(0)
	rm.bleBusyGeneration.Store(0)
	rm.clearHotplugObservationLocked()
	rm.generation++
	return errors.Join(errs...)
}

// Status 返回读卡器当前状态
func (rm *readerManager) Status() readerStatus {
	status, _ := rm.statusAndUSIMInfo()
	return status
}

// statusAndUSIMInfo returns the reader state and its cached SIM identity from
// one reader-manager generation.  Callers which publish both values (notably
// the snapshot API) must use this instead of Status followed by USIMInfo, as
// a profile switch can otherwise occur between those two independent reads.
func (rm *readerManager) statusAndUSIMInfo() (readerStatus, *usimInfo) {
	rm.mu.Lock()
	defer rm.mu.Unlock()

	status := readerStatus{
		Connected:  rm.readerName != "",
		ReaderName: rm.readerName,
		Generation: rm.generation,
		HasCard:    rm.readerName != "", // 已连接意味着卡已就绪
		CardType:   "unknown",           // 默认值，后续按实际探测结果覆盖
		HasESIM:    rm.esimManager != nil,
	}
	rm.usimMu.RLock()
	var info *usimInfo
	if rm.cachedUSIM != nil {
		copy := *rm.cachedUSIM
		info = &copy
	}
	rm.usimMu.RUnlock()
	if info != nil {
		status.HasUSIMInfo = true
		status.CardType = "physical_sim"
	}
	if rm.esimManager != nil {
		status.CardType = "euicc"
	}
	return status, info
}

// ---------------------------------------------------------------------------
// USIM 文件读取 + AKA 鉴权

// ---------------------------------------------------------------------------

// loadUSIMInfoLocked 读取物理 SIM 卡信息（IMSI/ICCID/MCC/MNC/ISIM 等）
// 调用时需持有 rm.mu
func (rm *readerManager) loadUSIMInfoLocked() error {
	// The caller already holds rm.mu. A blocking authMu lock here would
	// deadlock against an LPA read holding authMu before opening the CCID
	// channel, which takes rm.mu. The caller can retry after that read ends.
	if !rm.authMu.TryLock() {
		return errReaderCardOperationActive
	}
	defer rm.authMu.Unlock()
	return rm.loadUSIMInfoWithCardLeaseLocked()
}

// loadUSIMInfoWithCardLeaseLocked requires both rm.mu and rm.authMu.
func (rm *readerManager) loadUSIMInfoWithCardLeaseLocked() error {
	return rm.loadUSIMInfoWithCardLeasePolicyLocked(true)
}

func (rm *readerManager) loadUSIMInfoWithCardLeasePolicyLocked(allowNative bool) error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	return rm.loadUSIMInfoWithCardLeaseContextLocked(ctx, allowNative)
}

func (rm *readerManager) loadUSIMInfoWithCardLeaseContextLocked(ctx context.Context, allowNative bool) error {

	// 打开 PC/SC reader（uicc-go）
	reader, err := openReaderTransmitter(ctx, rm.readerName)
	if err != nil {
		return fmt.Errorf("打开 PC/SC 读卡器失败: %w", err)
	}
	defer reader.Close()

	// 创建 usim.Reader（需要 card.Transmitter 接口）
	// uicc-go 的 ccid.Reader 实现了 Transmitter 接口 (Transmit + Close)
	usimReader, err := usim.NewReader(reader)
	if err != nil {
		return fmt.Errorf("创建 USIM reader 失败: %w", err)
	}

	// 初始化 USIM Card（读取所有基本信息）
	card, err := usim.New(ctx, usimReader)
	if err != nil {
		return fmt.Errorf("读取 SIM 卡信息失败: %w", err)
	}
	defer card.Close()

	serviceCenter := card.ServiceCenter()
	selectors := readUSIMCarrierSelectors(ctx, usimReader)
	if selectors.phoneNumber == "" {
		log.Printf("EF-MSISDN unavailable")
	}
	info := &usimInfo{
		ICCID:           card.ICCID(),
		IMSI:            card.IMSI(),
		PhoneNumber:     selectors.phoneNumber,
		OperatorName:    selectors.operatorName,
		OperatorSource:  selectors.operatorSource,
		MCC:             card.MCC(),
		MNC:             card.MNC(),
		GID1:            card.GID1(),
		GID2:            selectors.gid2,
		ApplicationAID:  hex.EncodeToString(selectors.applicationAID),
		SMSC:            serviceCenter.Address,
		SMSCPSI:         serviceCenter.PSI,
		SMSCTarget:      serviceCenter.Target(),
		PrivateIdentity: card.PrivateIdentity(),
		PublicIdentity:  card.PublicIdentity(),
		HomeDomain:      card.HomeDomain(),
		LoadedAt:        time.Now(),
	}
	// Rust now owns the optional read-only ICCID verification path. Close the
	// Go card handle before opening a second PC/SC transaction; if the native
	// endpoint is unavailable, retain the already validated Go value.
	if allowNative && !isBLEReader(rm.readerName) && os.Getenv("PROLETARIUS_CARRIER_SOCKET") != "" {
		card.Close()
		_ = reader.Close()
		if nativeIdentity, handled, nativeErr := readRuntimeReaderIdentity(
			ctx, rm.readerName, selectors.applicationAID); handled && nativeErr == nil {
			info.ICCID = nativeIdentity.ICCID
			if nativeIdentity.IMSI != "" {
				info.IMSI = nativeIdentity.IMSI
			}
			if nativeIdentity.MCC != "" {
				info.MCC = nativeIdentity.MCC
			}
			if nativeIdentity.MNC != "" {
				info.MNC = nativeIdentity.MNC
			}
		}
	}

	rm.usimMu.Lock()
	rm.cachedUSIM = info
	rm.usimMu.Unlock()

	return nil
}

// USIMInfo 返回缓存的 SIM 卡信息
func (rm *readerManager) USIMInfo() *usimInfo {
	rm.usimMu.RLock()
	defer rm.usimMu.RUnlock()
	if rm.cachedUSIM == nil {
		return nil
	}
	c := *rm.cachedUSIM
	return &c
}

// UpdatePhoneNumber publishes a trusted IMS-associated subscriber number for
// the still-current profile. A stale registration from a previous reader
// generation is never allowed to overwrite the active card identity.
func (rm *readerManager) UpdatePhoneNumber(expectedGeneration uint64, number string) bool {
	number = strings.TrimSpace(number)
	if number == "" {
		return false
	}
	rm.mu.Lock()
	defer rm.mu.Unlock()
	if rm.readerName == "" || expectedGeneration == 0 || rm.generation != expectedGeneration {
		return false
	}
	rm.usimMu.Lock()
	defer rm.usimMu.Unlock()
	if rm.cachedUSIM == nil {
		return false
	}
	rm.cachedUSIM.PhoneNumber = number
	return true
}

// readerSnapshot returns one immutable reader/SIM observation.  The caller
// must use this reader ID together with the generation when starting any
// reader-bound work; reading Status afterwards could observe a different card
// after a reconnect or profile switch.
func (rm *readerManager) readerSnapshot() (*usimInfo, string, uint64, error) {
	rm.mu.Lock()
	defer rm.mu.Unlock()
	if rm.readerName == "" {
		return nil, "", 0, errors.New("读卡器未连接")
	}
	rm.usimMu.RLock()
	defer rm.usimMu.RUnlock()
	if rm.cachedUSIM == nil {
		return nil, rm.readerName, rm.generation, errors.New("读卡器无 SIM 卡信息")
	}
	info := *rm.cachedUSIM
	return &info, rm.readerName, rm.generation, nil
}

// RefreshUSIMInfo 重新读取 SIM 卡信息
func (rm *readerManager) RefreshUSIMInfo() error {
	rm.mu.Lock()
	defer rm.mu.Unlock()
	if rm.readerName == "" {
		return errors.New("读卡器未连接")
	}
	previousICCID := ""
	if current := rm.USIMInfo(); current != nil {
		previousICCID = normalizeSIMIdentifier(current.ICCID)
	}
	if err := rm.loadUSIMInfoLocked(); err != nil {
		return err
	}
	if current := rm.USIMInfo(); current != nil {
		currentICCID := normalizeSIMIdentifier(current.ICCID)
		if previousICCID != "" && currentICCID != "" && currentICCID != previousICCID {
			rm.generation++
			if isBLEReader(rm.readerName) {
				rm.activeBLEGeneration.Store(rm.generation)
				rm.bleBusyGeneration.Store(0)
			}
		}
	}
	return nil
}

// WaitForUSIMICCID waits until the card has completed its post-switch reset
// and exposes the requested profile identity. A successful refresh alone is
// insufficient because some eUICCs briefly continue serving the old profile.
func (rm *readerManager) WaitForUSIMICCID(ctx context.Context, targetICCID string, interval time.Duration) (*usimInfo, error) {
	target := normalizeSIMIdentifier(targetICCID)
	if target == "" {
		return nil, errors.New("目标 ICCID 不能为空")
	}
	if interval <= 0 {
		interval = time.Second
	}
	// Fast path: the post-switch reconnect has already refreshed the cached
	// USIM info. If the card already presents the target identity, polling
	// (a full USIM application read per round) is pure waste.
	if info := rm.USIMInfo(); info != nil && normalizeSIMIdentifier(info.ICCID) == target {
		return info, nil
	}
	var lastErr error
	for {
		if err := rm.RefreshUSIMInfo(); err != nil {
			lastErr = err
		} else if info := rm.USIMInfo(); info != nil && normalizeSIMIdentifier(info.ICCID) == target {
			return info, nil
		} else if info != nil {
			lastErr = errors.New("读卡器仍在呈现切换前的 profile")
		}
		timer := time.NewTimer(interval)
		select {
		case <-ctx.Done():
			timer.Stop()
			if lastErr != nil {
				return nil, fmt.Errorf("等待目标 profile 生效: %w", lastErr)
			}
			return nil, ctx.Err()
		case <-timer.C:
		}
	}
}

func firstMSISDN(records [][]byte) string {
	for _, record := range records {
		if number := decodeMSISDNRecord(record); number != "" {
			return number
		}
	}
	return ""
}

// decodeMSISDNRecord decodes the ADN footer used by EF-MSISDN. Records that
// require an extension record are ignored to avoid publishing a truncated
// subscriber number.
func decodeMSISDNRecord(record []byte) string {
	if len(record) < 14 {
		return ""
	}
	footer := record[len(record)-14:]
	length := int(footer[0])
	if length == 0 || footer[0] == 0xFF || length > 11 || footer[13] != 0xFF {
		return ""
	}
	tonNPI := footer[1]
	bcdLength := length - 1
	if bcdLength <= 0 || bcdLength > 10 {
		return ""
	}
	var digits strings.Builder
	for _, value := range footer[2 : 2+bcdLength] {
		for _, nibble := range []byte{value & 0x0F, value >> 4} {
			if nibble == 0x0F {
				break
			}
			if nibble > 9 {
				return ""
			}
			digits.WriteByte('0' + nibble)
		}
	}
	if digits.Len() == 0 {
		return ""
	}
	number := digits.String()
	if strings.Trim(number, "0") == "" {
		return ""
	}
	if tonNPI&0x70 == 0x10 {
		return "+" + number
	}
	return number
}

// AKAResult EAP-AKA 鉴权结果
type akaResult struct {
	RES  []byte `json:"res"`
	CK   []byte `json:"ck"`
	IK   []byte `json:"ik"`
	AUTS []byte `json:"auts,omitempty"`
}

var (
	errReaderAKASyncFailure  = errors.New("SIM AKA synchronization failure")
	errReaderAKARejected     = errors.New("SIM 卡拒绝鉴权 (Authentication Reject)")
	errReaderISIMUnavailable = errors.New("ISIM application unavailable")
	errReaderCardAbsent      = errors.New("SIM 卡已移除或未就绪")
	errReaderUnavailable     = errors.New("读卡器已断开或不可用")
	errReaderBusy            = errors.New("读卡器暂时被其他操作占用")
	usimApplicationAIDPrefix = []byte{0xA0, 0x00, 0x00, 0x00, 0x87, 0x10, 0x02}
	isimApplicationAIDPrefix = []byte{0xA0, 0x00, 0x00, 0x00, 0x87, 0x10, 0x04}
)

// authenticateCore performs one explicit USIM or ISIM AKA transaction.  The
// application is selected by its complete EF-DIR AID (prefixes alone are not
// sent to the card), so IMS authentication cannot accidentally run against
// USIM merely because it is the first application returned by the reader.
func (rm *readerManager) authenticateCore(rand, autn, aidPrefix []byte, applicationName string) (*akaResult, error) {
	rm.mu.Lock()
	generation := rm.generation
	connected := rm.readerName != ""
	rm.mu.Unlock()
	if !connected {
		return nil, errors.New("读卡器未连接")
	}
	return rm.authenticateCoreAtGeneration(rand, autn, aidPrefix, applicationName, generation)
}

// validateAuthenticationReader is called with authMu held. Card refresh holds
// mu before waiting for authMu, so this check must never wait for mu.
func (rm *readerManager) validateAuthenticationReader(reader string, generation uint64) error {
	if !rm.mu.TryLock() {
		return errReaderBusy
	}
	defer rm.mu.Unlock()
	if rm.readerName != reader || rm.generation != generation {
		return errors.New("读卡器连接已变更")
	}
	return nil
}

// respondEAPAKAAtGeneration is a retired global-socket migration helper.
// New production code must use the session-frozen EAP capability instead;
// this remains temporarily for differential tests until the real USB carrier
// acceptance gate permits its deletion. The generation check prevents a
// delayed reply from being attached to a newly selected SIM.
func (rm *readerManager) respondEAPAKAAtGeneration(
	eapRequest []byte,
	exchangeID string,
	checkcodeMode string,
	expectedGeneration uint64,
) (result nativeEAPAKAResponse, handled bool, err error) {
	rm.mu.Lock()
	readerName := rm.readerName
	generation := rm.generation
	var expectedIdentity *nativeReaderAuthIdentity
	if info := rm.cachedUSIM; info != nil && info.ICCID != "" && info.IMSI != "" {
		expectedIdentity = &nativeReaderAuthIdentity{ICCID: normalizeSIMIdentifier(info.ICCID), IMSI: normalizeSIMIdentifier(info.IMSI)}
	}
	rm.mu.Unlock()
	if readerName == "" {
		return nativeEAPAKAResponse{}, true, errors.New("读卡器未连接")
	}
	if expectedGeneration != 0 && generation != expectedGeneration {
		return nativeEAPAKAResponse{}, true, errors.New("读卡器连接已变更")
	}
	if isBLEReader(readerName) {
		return nativeEAPAKAResponse{}, false, nil
	}

	releaseCard, gateErr := rm.beginCardOperationContext(context.Background())
	if gateErr != nil {
		return nativeEAPAKAResponse{}, true, gateErr
	}
	defer func() {
		releaseCard()
		if expectedGeneration == 0 || result.Packet == nil {
			return
		}
		rm.mu.Lock()
		valid := rm.readerName == readerName && rm.generation == expectedGeneration
		rm.mu.Unlock()
		if !valid {
			_ = releaseRuntimeEAPIdentityExchange(context.Background(), result.NextExchangeID)
			clear(result.Packet)
			clear(result.MSK)
			clear(result.KAut)
			result = nativeEAPAKAResponse{}
			err = errors.New("读卡器连接已变更")
		}
	}()

	if err := rm.validateAuthenticationReader(readerName, generation); err != nil {
		return nativeEAPAKAResponse{}, true, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Second)
	defer cancel()
	return respondRuntimeReaderEAPAKAForIdentity(
		ctx,
		readerName,
		"usim",
		exchangeID,
		eapRequest,
		checkcodeMode,
		expectedIdentity,
	)
}

func (rm *readerManager) authenticateCoreAtGeneration(rand, autn, aidPrefix []byte, applicationName string, expectedGeneration uint64) (*akaResult, error) {
	return rm.authenticateCoreAtGenerationWithNative(
		rand,
		autn,
		aidPrefix,
		applicationName,
		expectedGeneration,
		true,
	)
}

// authenticateCoreAtGenerationWithNative keeps the existing optional Rust
// PC/SC AKA bridge for established callers. The private reader lease and
// explicit protocol fallbacks pass false so the selected Go transport performs
// the transaction without a second native/global selection decision.
func (rm *readerManager) authenticateCoreAtGenerationWithNative(rand, autn, aidPrefix []byte, applicationName string, expectedGeneration uint64, allowNative bool) (result *akaResult, err error) {
	if len(rand) != 16 || len(autn) != 16 {
		return nil, errors.New("RAND 和 AUTN 必须为 16 字节")
	}

	rm.mu.Lock()
	if rm.readerName == "" {
		rm.mu.Unlock()
		return nil, errors.New("读卡器未连接")
	}
	readerName := rm.readerName
	generation := rm.generation
	var expectedIdentity *nativeReaderAuthIdentity
	if info := rm.cachedUSIM; info != nil && info.ICCID != "" && info.IMSI != "" {
		expectedIdentity = &nativeReaderAuthIdentity{ICCID: normalizeSIMIdentifier(info.ICCID), IMSI: normalizeSIMIdentifier(info.IMSI)}
	}
	rm.mu.Unlock()
	if expectedGeneration != 0 && generation != expectedGeneration {
		return nil, errors.New("读卡器连接已变更")
	}

	releaseCard, gateErr := rm.beginCardOperationContext(context.Background())
	if gateErr != nil {
		return nil, gateErr
	}
	defer func() {
		releaseCard()
		if expectedGeneration == 0 || result == nil {
			return
		}
		rm.mu.Lock()
		valid := rm.readerName == readerName && rm.generation == expectedGeneration
		rm.mu.Unlock()
		if !valid {
			result = nil
			err = errors.New("读卡器连接已变更")
		}
	}()
	if err := rm.validateAuthenticationReader(readerName, generation); err != nil {
		return nil, err
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	// Rust discovers the complete USIM/ISIM AID and executes AKA in one
	// serialized PC/SC transaction. Do not open a competing Go handle first.
	if allowNative && !isBLEReader(readerName) && os.Getenv("PROLETARIUS_CARRIER_SOCKET") != "" {
		application := "usim"
		if bytes.Equal(aidPrefix, isimApplicationAIDPrefix) {
			application = "isim"
		}
		result, handled, err := authenticateRuntimeReaderAKAAuto(ctx, readerName, application, rand, autn, expectedIdentity)
		if handled {
			if err == nil {
				log.Printf("[Reader VoWiFi] %s AKA 已由 Rust PC/SC 执行", applicationName)
			}
			return result, err
		}
	}

	return authenticateReaderUnderLease(ctx, readerName, rand, autn, aidPrefix, applicationName)
}

// Caller must hold the physical card lease for the entire operation.
func authenticateReaderUnderLease(ctx context.Context, readerName string, rand, autn, aidPrefix []byte, applicationName string) (*akaResult, error) {
	return authenticateReaderUnderLeaseIdentity(ctx, readerName, rand, autn, aidPrefix, applicationName, nil)
}

func authenticateReaderUnderLeaseIdentity(ctx context.Context, readerName string, rand, autn, aidPrefix []byte, applicationName string, expected *usimInfo) (*akaResult, error) {
	if len(rand) != 16 || len(autn) != 16 {
		return nil, errors.New("invalid AKA challenge length")
	}
	// 每次鉴权独立打开 PC/SC 连接
	reader, err := openReaderTransmitter(ctx, readerName)
	if err != nil {
		return nil, fmt.Errorf("打开 PC/SC 读卡器失败: %w", err)
	}
	defer reader.Close()
	return authenticateReaderOnHandleIdentity(ctx, reader, rand, autn, aidPrefix, applicationName, expected)
}

// The caller keeps this transmitter and its physical-card lease alive through
// identity verification and AUTHENTICATE. Never reopen or use cached identities.
func authenticateReaderOnHandleIdentity(ctx context.Context, reader usimcard.Transmitter, rand, autn, aidPrefix []byte, applicationName string, expected *usimInfo) (*akaResult, error) {
	usimReader, err := usim.NewReader(reader)
	if err != nil {
		return nil, fmt.Errorf("创建 USIM reader 失败: %w", err)
	}

	applications, err := usimReader.ListApplications(ctx)
	if err != nil {
		return nil, fmt.Errorf("读取 SIM 应用 AID 失败: %w", err)
	}
	if expected != nil {
		// Multi-profile authentication needs only the live ICCID and IMSI,
		// not the full SMSC/ISIM/carrier inventory loaded by usim.New. IMSI
		// belongs to the USIM application even when authenticating through ISIM.
		usimAID := findApplicationAID(applications, usimApplicationAIDPrefix)
		if len(usimAID) == 0 {
			return nil, errors.New("鉴权句柄身份读取失败: USIM AID 未找到")
		}
		root := usimcommand.App{Reader: usimReader}
		iccidData, err := root.Transparent(ctx, []byte{0x2F, 0xE2})
		if err != nil {
			return nil, fmt.Errorf("鉴权句柄 ICCID 读取失败: %w", err)
		}
		var iccid simfile.ICCID
		if err := iccid.UnmarshalBinary(iccidData); err != nil {
			return nil, fmt.Errorf("鉴权句柄 ICCID 解析失败: %w", err)
		}
		app := usimcommand.App{Reader: usimReader, AID: usimAID}
		imsiData, err := app.Transparent(ctx, []byte{0x6F, 0x07})
		if err != nil {
			return nil, fmt.Errorf("鉴权句柄 IMSI 读取失败: %w", err)
		}
		var imsi simfile.IMSI
		if err := imsi.UnmarshalBinary(imsiData); err != nil {
			return nil, fmt.Errorf("鉴权句柄 IMSI 解析失败: %w", err)
		}
		if err := verifyMultiAuthIdentity(iccid.String(), imsi.String(), expected); err != nil {
			return nil, err
		}
	}
	aid := findApplicationAID(applications, aidPrefix)
	if len(aid) == 0 {
		if bytes.Equal(aidPrefix, isimApplicationAIDPrefix) {
			return nil, errReaderISIMUnavailable
		}
		return nil, fmt.Errorf("%s AID 未找到", applicationName)
	}
	payload, err := usimReader.Authenticate3G(ctx, usimcard.AuthenticateRequest{
		AID:    aid,
		Rand:   append([]byte(nil), rand...),
		AUTN:   append([]byte(nil), autn...),
		EAPAKA: true,
	})
	if err != nil {
		return nil, fmt.Errorf("%s AKA 鉴权失败: %w", applicationName, err)
	}
	var parsed usimcommand.Authenticate3GResult
	if err := parsed.UnmarshalBinary(payload); err != nil {
		return nil, fmt.Errorf("解析 %s AKA 返回失败: %w", applicationName, err)
	}
	parsedResult := akaResult{
		RES:  append([]byte(nil), parsed.RES...),
		CK:   append([]byte(nil), parsed.CK...),
		IK:   append([]byte(nil), parsed.IK...),
		AUTS: append([]byte(nil), parsed.AUTS...),
	}

	if parsed.IsAuthenticationReject() {
		return nil, errReaderAKARejected
	}
	if parsed.IsSynchronizationFailure() {
		if len(parsedResult.AUTS) != 14 {
			return nil, fmt.Errorf("SIM AKA 返回的 AUTS 长度无效: %d（期望 14）", len(parsedResult.AUTS))
		}
		return &parsedResult, errReaderAKASyncFailure
	}
	if !parsed.IsSuccess() {
		return nil, errors.New("SIM AKA 返回不完整")
	}
	return &parsedResult, nil
}

func (rm *readerManager) authenticateEAPAKAAtGeneration(rand, autn []byte, generation uint64) (*akaResult, error) {
	return rm.authenticateCoreAtGeneration(rand, autn, usimApplicationAIDPrefix, "USIM", generation)
}

// authenticateEAPAKAAtGenerationLegacy forces the generation-bound Go reader
// transport. It is used by protocol fallbacks and by Rust's private leased
// AKA endpoint; in both cases the bridge honors the selected reader and may
// dispatch through either PC/SC or the ESTK BLE transport.
func (rm *readerManager) authenticateEAPAKAAtGenerationLegacy(rand, autn []byte, generation uint64) (*akaResult, error) {
	return rm.authenticateCoreAtGenerationWithNative(rand, autn, usimApplicationAIDPrefix, "USIM", generation, false)
}

func (rm *readerManager) authenticateISIMAKAAtGeneration(rand, autn []byte, generation uint64) (*akaResult, error) {
	return rm.authenticateCoreAtGeneration(rand, autn, isimApplicationAIDPrefix, "ISIM", generation)
}

// authenticateISIMAKAAtGenerationLegacy deliberately uses the Go reader
// transport, including the ESTK BLE bridge, for a Rust lease-authorized IMS
// AKA transaction. It cannot recurse into the Rust PC/SC endpoint.
func (rm *readerManager) authenticateISIMAKAAtGenerationLegacy(rand, autn []byte, generation uint64) (*akaResult, error) {
	return rm.authenticateCoreAtGenerationWithNative(rand, autn, isimApplicationAIDPrefix, "ISIM", generation, false)
}

func findApplicationAID(applications []usimcard.Application, prefix []byte) []byte {
	for _, application := range applications {
		if bytes.HasPrefix(application.AID, prefix) {
			return append([]byte(nil), application.AID...)
		}
	}
	return nil
}

// AuthenticateAKA 在 SIM 卡上执行 3G AKA 鉴权（AUTHENTICATE APDU, INS=0x88）
// 输入 RAND (16 bytes) 和 AUTN (16 bytes)，返回 RES/CK/IK 或 AUTS（同步失败）
func (rm *readerManager) AuthenticateAKA(rand, autn []byte) (*akaResult, error) {
	return rm.authenticateCore(rand, autn, usimApplicationAIDPrefix, "USIM")
}

// AuthenticateEAPAKA 在 SIM 卡上执行 EAP-AKA 鉴权
// SWu EAP-AKA must use the USIM application. IMS has its own explicit ISIM
// entry point below and must not affect ePDG authentication.
func (rm *readerManager) AuthenticateEAPAKA(rand, autn []byte) (*akaResult, error) {
	return rm.authenticateCore(rand, autn, usimApplicationAIDPrefix, "USIM")
}

// AuthenticateISIMAKA runs IMS AKA against the ISIM application selected by
// its A0000000871004 prefix. It reports errReaderISIMUnavailable only when the
// card genuinely has no matching application, allowing a deliberate USIM
// fallback without hiding ISIM authentication failures.
func (rm *readerManager) AuthenticateISIMAKA(rand, autn []byte) (*akaResult, error) {
	return rm.authenticateCore(rand, autn, isimApplicationAIDPrefix, "ISIM")
}

// AuthenticateGSM runs the USIM GSM security-context authentication used by
// Orange/EE's SIP-Digest fallback. It returns SRES (4 bytes) and Kc (8 bytes).
func (rm *readerManager) AuthenticateGSM(rand []byte) (sres, kc []byte, err error) {
	return rm.authenticateGSMAtGeneration(rand, 0)
}

func (rm *readerManager) authenticateGSMAtGeneration(rand []byte, expectedGeneration uint64) (sres, kc []byte, err error) {
	if len(rand) != 16 {
		return nil, nil, errors.New("GSM RAND 必须为 16 字节")
	}
	rm.mu.Lock()
	readerName := rm.readerName
	generation := rm.generation
	rm.mu.Unlock()
	if readerName == "" {
		return nil, nil, errors.New("读卡器未连接")
	}
	if expectedGeneration != 0 && generation != expectedGeneration {
		return nil, nil, errors.New("读卡器连接已变更")
	}
	releaseCard, gateErr := rm.beginCardOperationContext(context.Background())
	if gateErr != nil {
		return nil, nil, gateErr
	}
	defer func() {
		releaseCard()
		if expectedGeneration == 0 || (sres == nil && kc == nil) {
			return
		}
		rm.mu.Lock()
		valid := rm.readerName == readerName && rm.generation == expectedGeneration
		rm.mu.Unlock()
		if !valid {
			sres, kc = nil, nil
			err = errors.New("读卡器连接已变更")
		}
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	reader, err := openReaderTransmitter(ctx, readerName)
	if err != nil {
		return nil, nil, fmt.Errorf("打开 PC/SC 读卡器失败: %w", err)
	}
	defer reader.Close()
	uiccReader, err := usim.NewReader(reader)
	if err != nil {
		return nil, nil, err
	}
	apps, err := uiccReader.ListApplications(ctx)
	if err != nil {
		return nil, nil, fmt.Errorf("读取 USIM AID: %w", err)
	}
	var aid []byte
	aid = findApplicationAID(apps, usimApplicationAIDPrefix)
	if len(aid) == 0 {
		return nil, nil, errors.New("USIM AID 未找到")
	}
	selectAPDU := append([]byte{0x00, 0xA4, 0x04, 0x04, byte(len(aid))}, aid...)
	if _, err := reader.Transmit(ctx, selectAPDU); err != nil {
		return nil, nil, fmt.Errorf("选择 USIM AID: %w", err)
	}
	authAPDU := append([]byte{0x00, 0x88, 0x00, 0x80, 0x11, 0x10}, rand...)
	authAPDU = append(authAPDU, 0x00)
	response, err := reader.Transmit(ctx, authAPDU)
	if err != nil {
		return nil, nil, fmt.Errorf("GSM AUTHENTICATE: %w", err)
	}
	if len(response) >= 2 && response[len(response)-2] == 0x61 {
		response, err = reader.Transmit(ctx, []byte{0x00, 0xC0, 0x00, 0x00, response[len(response)-1]})
		if err != nil {
			return nil, nil, fmt.Errorf("GSM AUTHENTICATE GET RESPONSE: %w", err)
		}
	}
	if len(response) < 2 || response[len(response)-2] != 0x90 || response[len(response)-1] != 0x00 {
		return nil, nil, fmt.Errorf("SIM 拒绝 GSM 鉴权: %X", response)
	}
	data := response[:len(response)-2]
	if len(data) == 14 && data[0] == 4 && data[5] == 8 {
		return append([]byte(nil), data[1:5]...), append([]byte(nil), data[6:14]...), nil
	}
	if len(data) == 12 {
		sres = append([]byte(nil), data[:4]...)
		kc = append([]byte(nil), data[4:12]...)
		return sres, kc, nil
	}
	return nil, nil, fmt.Errorf("GSM 鉴权返回格式无效: %X", data)
}

// ---------------------------------------------------------------------------
// eSIM Profile 管理（通过 esim.Manager）

// ---------------------------------------------------------------------------

// ESIMManager 返回 eSIM 管理器（如果可用）
func (rm *readerManager) ESIMManager() (*esim.Manager, bool) {
	rm.mu.Lock()
	defer rm.mu.Unlock()
	return rm.esimManager, rm.esimManager != nil
}

// ---------------------------------------------------------------------------
// CCID SIMAuthProvider 适配（供 VoWiFi EAP-AKA 使用）

// ---------------------------------------------------------------------------

func (rm *readerManager) NewSIMAuthProvider() *ccidSIMAuth {
	return &ccidSIMAuth{rm: rm}
}

// newChannel uses the selected card transport, without exposing USB/BLE to IMS.
func (a *ccidSIMAuth) newChannel() (driver.SmartCardChannel, error) {
	rm := a.rm
	rm.mu.Lock()
	if rm.readerName == "" {
		rm.mu.Unlock()
		return nil, errors.New("读卡器未连接")
	}
	readerName := rm.readerName
	rm.mu.Unlock()

	return a.rm.newReaderChannel(readerName)
}

func (a *ccidSIMAuth) OpenLogicalChannel(ctx context.Context, aid string) (int, error) {
	releaseCard, err := a.rm.beginCardOperationContext(ctx)
	if err != nil {
		return 0, err
	}
	keepLease := false
	defer func() {
		if !keepLease {
			releaseCard()
		}
	}()
	reader, err := a.newChannel()
	if err != nil {
		return 0, err
	}

	aidBytes, err := hex.DecodeString(aid)
	if err != nil {
		_ = reader.Disconnect()
		return 0, fmt.Errorf("解析 AID hex 失败: %w", err)
	}
	ch, err := reader.OpenLogicalChannel(aidBytes)
	if err != nil {
		_ = reader.Disconnect()
		return 0, fmt.Errorf("打开逻辑通道失败: %w", err)
	}
	// 将 reader 存入 context 以便后续 CloseLogicalChannel 和 TransmitAPDU 使用
	// CCIDReader 在 Disconnect 时会关闭底层 PC/SC 连接
	// 这里需要保留 reader 引用——使用 map 存储 channel → reader
	a.mu.Lock()
	if a.chans == nil {
		a.chans = make(map[int]driver.SmartCardChannel)
	}
	if a.releases == nil {
		a.releases = make(map[int]func())
	}
	a.chans[int(ch)] = reader
	a.releases[int(ch)] = releaseCard
	a.mu.Unlock()
	keepLease = true
	return int(ch), nil
}

func (a *ccidSIMAuth) CloseLogicalChannel(ctx context.Context, channelID int) error {
	a.mu.Lock()
	reader := a.chans[channelID]
	release := a.releases[channelID]
	delete(a.chans, channelID)
	delete(a.releases, channelID)
	a.mu.Unlock()
	if reader == nil {
		return errors.New("未找到通道对应的 CCID 连接")
	}
	if release != nil {
		defer release()
	}
	defer reader.Disconnect()
	return reader.CloseLogicalChannel(byte(channelID))
}

func (a *ccidSIMAuth) TransmitAPDU(ctx context.Context, channelID int, command string) (string, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	reader := a.chans[channelID]
	if reader == nil {
		return "", errors.New("未找到通道对应的 CCID 连接")
	}

	cmdBytes, err := hex.DecodeString(command)
	if err != nil {
		return "", fmt.Errorf("解析 APDU hex 失败: %w", err)
	}
	resp, err := reader.Transmit(cmdBytes)
	if err != nil {
		return "", fmt.Errorf("APDU 传输失败: %w", err)
	}
	return hex.EncodeToString(resp), nil
}

// channelMap 管理 channelID → CCIDReader 的映射（线程安全）
type ccidSIMAuth struct {
	rm       *readerManager
	mu       sync.Mutex
	chans    map[int]driver.SmartCardChannel
	releases map[int]func()
}

func (a *ccidSIMAuth) storeChannel(id int, reader driver.SmartCardChannel) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.chans == nil {
		a.chans = make(map[int]driver.SmartCardChannel)
	}
	a.chans[id] = reader
}

func (a *ccidSIMAuth) getChannel(id int) driver.SmartCardChannel {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.chans[id]
}

func (a *ccidSIMAuth) popChannel(id int) driver.SmartCardChannel {
	a.mu.Lock()
	defer a.mu.Unlock()
	reader := a.chans[id]
	delete(a.chans, id)
	return reader
}
