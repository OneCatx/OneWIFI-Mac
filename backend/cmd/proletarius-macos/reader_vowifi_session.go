package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"

	swusim "github.com/1239t/swu-go/pkg/sim"
	swucore "github.com/1239t/swu-go/pkg/swu"

	"proletarius.local/core/internal/readerapp"
	"proletarius.local/core/internal/vowifi"
	"proletarius.local/core/internal/vowifi/carrier"
	"proletarius.local/core/internal/vowifi/imscore"
	imssim "proletarius.local/core/internal/vowifi/sim"
)

// swuSIMAdapter adapts the reader to swu-go's EAP-AKA provider. The adapter
// deliberately does not own or close the reader: the reader manager controls
// the physical card lifecycle.
type swuSIMAdapter struct {
	rm         *readerManager
	moduleSIM  *moduleVoWiFiSIM
	generation uint64
	imsi       string
	imei       string
	// sessionCtx is the owning VoWiFi lifecycle context.  EAP provider
	// callbacks have no context parameter, so retaining this exact context is
	// what makes stop/cancel abort a pending native Unix-socket request.
	sessionCtx    context.Context
	eapCapability readerEAPAKASessionCapability
}

func (a *swuSIMAdapter) nativeEAPContext() context.Context {
	if a.sessionCtx != nil {
		return a.sessionCtx
	}
	// Unit-level adapters can be constructed without a live session.  The
	// production constructor always supplies vowifiSession.run's context.
	return context.Background()
}

func (a *swuSIMAdapter) GetIMSI() (string, error) {
	if strings.TrimSpace(a.imsi) == "" {
		return "", fmt.Errorf("SIM IMSI 为空")
	}
	return a.imsi, nil
}

func (a *swuSIMAdapter) GetIMEI() (string, error) {
	return strings.TrimSpace(a.imei), nil
}

func (a *swuSIMAdapter) CalculateAKA(rand, autn []byte) (res, ck, ik, auts []byte, err error) {
	if a.moduleSIM != nil {
		result, authErr := a.moduleSIM.authenticate(a.nativeEAPContext(), "usim", rand, autn)
		if result == nil {
			return nil, nil, nil, nil, authErr
		}
		return append([]byte(nil), result.RES...), append([]byte(nil), result.CK...),
			append([]byte(nil), result.IK...), append([]byte(nil), result.AUTS...), authErr
	}
	if a.rm == nil {
		return nil, nil, nil, nil, errors.New("读卡器 AKA 适配器未初始化")
	}
	var result *akaResult
	if a.eapCapability.usesRustStandardEAP() {
		// Fast Re-authentication, AT_NEXT_REAUTH_ID and EAP-AKA' are explicit
		// legacy branches. They must not consult the process-wide optional
		// native AKA route merely because this session owns the ordinary
		// EAP-AKA conversation in Rust.
		result, err = a.rm.authenticateEAPAKALegacyForNativeSession(a.eapCapability, rand, autn)
	} else {
		// Go still owns the EAP/IKE_AUTH conversation on this path. Its
		// established authentication primitive may use the pre-existing Rust
		// PC/SC bridge for one APDU transaction when available; that does not
		// grant Rust ownership of the Identity transcript or EAP response.
		result, err = a.rm.authenticateEAPAKAAtGeneration(rand, autn, a.generation)
	}
	if result == nil {
		return nil, nil, nil, nil, err
	}
	res = append([]byte(nil), result.RES...)
	ck = append([]byte(nil), result.CK...)
	ik = append([]byte(nil), result.IK...)
	auts = append([]byte(nil), result.AUTS...)
	if errors.Is(err, errReaderAKASyncFailure) {
		return res, ck, ik, auts, swusim.ErrSyncFailure
	}
	return res, ck, ik, auts, err
}

// RespondEAPAKA delegates only a capability-selected ordinary EAP-AKA
// Challenge to Rust.  BLE, incomplete identities, Fast Re-authentication,
// AT_NEXT_REAUTH_ID and EAP-AKA' remain on the established Go path.
func (a *swuSIMAdapter) RespondEAPAKA(
	request []byte,
	exchangeID string,
	checkcodeMode string,
) (swucore.EAPAKAResponse, bool, error) {
	if !a.eapCapability.usesRustStandardEAP() {
		return swucore.EAPAKAResponse{}, false, nil
	}
	if a.rm == nil {
		return swucore.EAPAKAResponse{}, true, errors.New("Rust EAP-AKA 适配器未初始化")
	}
	result, err := a.rm.respondNativeEAPAKAForSession(
		a.nativeEAPContext(),
		a.eapCapability,
		request,
		exchangeID,
		checkcodeMode,
	)
	if err != nil {
		// A selected native session is mandatory for this ordinary EAP-AKA
		// exchange.  `handled=true` prevents swu-go from issuing another
		// physical AKA operation as an implicit fallback.
		return swucore.EAPAKAResponse{}, true, err
	}
	return swucore.EAPAKAResponse{
		NextExchangeID: result.NextExchangeID,
		Outcome:        result.Outcome,
		Packet:         result.Packet,
		MSK:            result.MSK,
		KAut:           result.KAut,
	}, true, nil
}

// RespondEAPIdentity lets Rust own the EAP Identity response and retains only
// its opaque exchange identifier plus RFC 4187 digest in the Go session.
func (a *swuSIMAdapter) RespondEAPIdentity(
	request, identity []byte,
) (swucore.EAPIdentityResponse, bool, error) {
	if !a.eapCapability.usesRustStandardEAP() {
		return swucore.EAPIdentityResponse{}, false, nil
	}
	if a.rm == nil {
		return swucore.EAPIdentityResponse{}, true, errors.New("Rust EAP Identity 适配器未初始化")
	}
	if err := a.rm.validateReaderEAPAKASessionContext(a.eapCapability); err != nil {
		return swucore.EAPIdentityResponse{}, true, err
	}
	result, err := respondRuntimeEAPIdentityAtSocket(a.nativeEAPContext(), a.eapCapability.socket, request, identity)
	if err != nil {
		return swucore.EAPIdentityResponse{}, true, err
	}
	if err := a.rm.validateReaderEAPAKASessionContext(a.eapCapability); err != nil {
		_ = a.releaseEAPIdentityExchange(result.ExchangeID)
		return swucore.EAPIdentityResponse{}, true, err
	}
	return swucore.EAPIdentityResponse{
		Packet: result.Packet, ExchangeID: result.ExchangeID, Checkcode: result.Checkcode,
	}, true, nil
}

func (a *swuSIMAdapter) ReleaseEAPIdentityExchange(exchangeID string) error {
	return a.releaseEAPIdentityExchange(exchangeID)
}

func (a *swuSIMAdapter) releaseEAPIdentityExchange(exchangeID string) error {
	if !a.eapCapability.usesRustStandardEAP() {
		return nil
	}
	// The release is cleanup, not session work: use an independent bounded
	// context so cancellation cannot leak a Rust-owned transcript, while still
	// preventing teardown from blocking indefinitely.
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	return releaseRuntimeEAPIdentityExchangeAtSocket(ctx, a.eapCapability.socket, exchangeID)
}

func (a *swuSIMAdapter) Close() error { return nil }

// imsAKAAdapter is separate because the local IMS core uses a result struct,
// while swu-go exposes the same AKA operation as multiple return values.
type imsAKAAdapter struct {
	rm         *readerManager
	moduleSIM  *moduleVoWiFiSIM
	generation uint64
	sessionCtx context.Context
}

func (a *imsAKAAdapter) context() context.Context {
	if a != nil && a.sessionCtx != nil {
		return a.sessionCtx
	}
	return context.Background()
}

func (a *imsAKAAdapter) CalculateAKA(rand, autn []byte) (imssim.AKAResult, error) {
	if a.moduleSIM != nil {
		result, err := a.moduleSIM.authenticate(a.context(), "usim", rand, autn)
		return mapReaderIMSAKAResult(result, err)
	}
	result, err := a.rm.authenticateEAPAKAAtGeneration(rand, autn, a.generation)
	return mapReaderIMSAKAResult(result, err)
}

// CalculateISIMAKA makes imsAKAAdapter an imssim.ISIMAKAProvider. The IMS core
// explicitly prefers this method and falls back to CalculateAKA only when the
// reader reports that the card has no ISIM application.
func (a *imsAKAAdapter) CalculateISIMAKA(rand, autn []byte) (imssim.AKAResult, error) {
	if a.moduleSIM != nil {
		result, err := a.moduleSIM.authenticate(a.context(), "isim", rand, autn)
		if errors.Is(err, errReaderISIMUnavailable) {
			return imssim.AKAResult{}, imssim.ErrISIMUnavailable
		}
		return mapReaderIMSAKAResult(result, err)
	}
	result, err := a.rm.authenticateISIMAKAAtGeneration(rand, autn, a.generation)
	if errors.Is(err, errReaderISIMUnavailable) {
		return imssim.AKAResult{}, imssim.ErrISIMUnavailable
	}
	return mapReaderIMSAKAResult(result, err)
}

func mapReaderIMSAKAResult(result *akaResult, err error) (imssim.AKAResult, error) {
	if result == nil {
		return imssim.AKAResult{}, err
	}
	mapped := imssim.AKAResult{
		RES:  append([]byte(nil), result.RES...),
		CK:   append([]byte(nil), result.CK...),
		IK:   append([]byte(nil), result.IK...),
		AUTS: append([]byte(nil), result.AUTS...),
	}
	if errors.Is(err, errReaderAKASyncFailure) {
		return mapped, imssim.ErrSyncFailure
	}
	return mapped, err
}

func (a *imsAKAAdapter) CalculateGSM(rand []byte) ([]byte, []byte, error) {
	if a.moduleSIM != nil {
		return nil, nil, errors.New("DJI 模块 VoWiFi 不支持 GSM AKA 回退")
	}
	return a.rm.authenticateGSMAtGeneration(rand, a.generation)
}

// vowifiSession owns only the reader-mode VoWiFi pipeline. It does not share
// state or networking components with the DJI module.
type vowifiSession struct {
	multiAuth     *multiAKA
	multiFailure  func(error)
	multiIncoming func(imscore.IncomingSMSEvent) error

	mu      sync.Mutex
	imsOpMu sync.Mutex

	readerGeneration uint64
	sourceKind       string
	moduleSIM        *moduleVoWiFiSIM
	radioLease       *moduleRadioLease
	radioRestore     func() error
	radioRestoreOnce sync.Once
	radioRestoreErr  error
	// eapCapability is frozen before the session becomes visible.  It carries
	// the USB/PCSC reader generation plus ICCID/IMSI for the small Rust
	// standard EAP-AKA owner slice; all other EAP branches retain Go.
	eapCapability readerEAPAKASessionCapability

	swuSession *swucore.Session
	imsService *imscore.Service

	carrier           *carrier.Profile
	simInfo           *vowifi.SIMInfo
	tunnelCfg         vowifi.TunnelConfig
	effective         *readerVoWiFiEffectiveConfig
	transportFactory  func(string, string) (swucore.Transport, error)
	transportMode     string
	app               *app
	requestedProxy    string
	requestedEPDG     string
	requestedEPDGPort int
	intentID          uint64
	reconnectAttempt  int
	sessionKey        readerapp.SessionKey

	tunnelState  tunnelState
	imsState     imsState
	tunnelResult *vowifi.TunnelResult
	imsResult    *vowifi.IMSRegisterResult
	lastError    string

	progressStep progressStep
	progressDesc string

	cancel      context.CancelFunc
	cancelOnce  sync.Once
	failureOnce sync.Once
	ctx         context.Context
	runDone     chan struct{}
	swuDone     chan struct{}
}

// vowifiStartup reserves one asynchronous startup attempt before it has a
// session to publish.  Its context covers Rust transport selection and the
// optional Surge control path, so Stop can invalidate a slow startup without
// waiting for either external control plane while holding vowifiSessionMu.
type vowifiStartup struct {
	multiInfo       *usimInfo
	multiReader     string
	multiGeneration uint64

	intentID           uint64
	ctx                context.Context
	cancel             context.CancelFunc
	readerGeneration   uint64
	expectedGeneration uint64
	sourceKind         string
	moduleSIM          *moduleVoWiFiSIM
	radioLease         *moduleRadioLease
	radioRestore       func() error
	progressStep       progressStep
	progressDesc       string
}

var (
	vowifiSessionMu sync.Mutex
	activeSession   *vowifiSession
	// Process-lifetime admission gate: queued start requests must not publish
	// a new tunnel after shutdown has already cancelled the previous intent.
	vowifiShuttingDown bool
	// retiringSession keeps exclusive ownership of reader/IMS/SWu resources
	// after the public session has been detached. A new session must not start
	// until the previous goroutines have actually released those resources.
	retiringSession      *vowifiSession
	readerVoWiFiSessions = readerapp.NewSessionRegistry[*vowifiSession]()
	// vowifiIntentID invalidates a pending automatic reconnect whenever the
	// user explicitly starts or stops VoWiFi.
	vowifiIntentID uint64
	// startingVoWiFi serializes preparation before there is an active session.
	// It is deliberately separate from activeSession: the latter remains a
	// published, usable session only.
	startingVoWiFi *vowifiStartup

	// prepareVoWiFiSession is a narrow test seam around the slow preparation
	// phase.  Production always uses prepareReaderVoWiFiSession.
	prepareVoWiFiSession = prepareReaderVoWiFiSession
)

const (
	readerVoWiFiIKEStartupTimeout         = 75 * time.Second
	readerVoWiFiTransportSelectionTimeout = 7 * time.Second
	// The exceptional Surge recovery path includes CLI control plus one fresh
	// Rust selection. Keep that entire path bounded rather than allowing ten
	// independently timed status polls to turn a startup into a minute-long
	// wait.
	readerVoWiFiSurgeRecoveryTimeout = 35 * time.Second
)

// startVoWiFi starts reader-mode SWu and IMS. An empty ePDG value means
// carrier-template/3GPP automatic selection; an explicit value always wins.
func startVoWiFi(a *app, proxyAddr, epdg string, epdgPort int) error {
	validatedProxy := strings.TrimPrefix(strings.TrimPrefix(strings.TrimSpace(proxyAddr), "socks5://"), "socks5h://")
	if err := validateReaderVoWiFiProxy(validatedProxy); err != nil {
		return err
	}
	vowifiSessionMu.Lock()
	if vowifiShuttingDown {
		vowifiSessionMu.Unlock()
		return errors.New("Switch 正在退出，未启动 VoWiFi")
	}
	if activeSession != nil {
		vowifiSessionMu.Unlock()
		return fmt.Errorf("VoWiFi 会话已在运行")
	}
	if retiringSession != nil {
		vowifiSessionMu.Unlock()
		return fmt.Errorf("上一个 VoWiFi 会话正在释放资源，请稍后重试")
	}
	if startingVoWiFi != nil {
		vowifiSessionMu.Unlock()
		return fmt.Errorf("VoWiFi 会话正在启动，请稍后重试")
	}
	vowifiIntentID++
	startup := newVoWiFiStartup(vowifiIntentID)
	startingVoWiFi = startup
	vowifiSessionMu.Unlock()

	return startReservedVoWiFi(a, proxyAddr, epdg, epdgPort, startup, 0)
}

func newVoWiFiStartup(intentID uint64) *vowifiStartup {
	ctx, cancel := context.WithCancel(context.Background())
	return &vowifiStartup{intentID: intentID, ctx: ctx, cancel: cancel}
}

// reserveVoWiFiReconnect replaces only the failed session that initiated this
// recovery.  It releases the global lock before any Rust selection or Surge
// control work and preserves the originating reader generation as a guard
// against reconnecting a different card/profile.
func reserveVoWiFiReconnect(session *vowifiSession) *vowifiStartup {
	if session == nil {
		return nil
	}
	vowifiSessionMu.Lock()
	defer vowifiSessionMu.Unlock()
	if vowifiShuttingDown || vowifiIntentID != session.intentID || activeSession != session || startingVoWiFi != nil {
		return nil
	}
	activeSession = nil
	startup := newVoWiFiStartup(session.intentID)
	startup.expectedGeneration = session.readerGeneration
	startup.sourceKind = session.sourceKind
	startup.moduleSIM = session.moduleSIM
	startup.radioLease = session.radioLease
	startup.radioRestore = session.radioRestore
	startingVoWiFi = startup
	return startup
}

// startReservedVoWiFi performs all reader/Rust/Surge work without holding
// vowifiSessionMu.  Publication is one short, cancellation-aware critical
// section after preparation has finished.
func startReservedVoWiFi(a *app, proxyAddr, epdg string, epdgPort int, startup *vowifiStartup, reconnectAttempt int) error {
	if startup == nil {
		return errors.New("VoWiFi 启动上下文不可用")
	}
	defer clearVoWiFiStartup(startup)
	keepModuleRadio := false
	if startup.sourceKind == "module" && startup.radioLease != nil {
		if err := startup.radioLease.enter(startup.ctx); err != nil {
			return err
		}
		defer func() {
			if !keepModuleRadio {
				if err := startup.radioLease.restore(); err != nil {
					log.Printf("[Module VoWiFi] Failed startup radio rollback: %v", err)
				}
			}
		}()
	}

	var session *vowifiSession
	var err error
	if startup.sourceKind == "module" {
		session, err = prepareModuleVoWiFiSession(startup.ctx, a, proxyAddr, epdg, epdgPort, startup, reconnectAttempt)
	} else {
		session, err = prepareVoWiFiSession(startup.ctx, a, proxyAddr, epdg, epdgPort, startup, reconnectAttempt)
	}
	if err != nil {
		if startup.ctx.Err() != nil {
			return startup.ctx.Err()
		}
		return err
	}
	if err := publishVoWiFiSession(startup, session); err != nil {
		wasCanceled := startup.ctx.Err() != nil || errors.Is(err, context.Canceled)
		startup.cancel()
		if wasCanceled {
			return context.Canceled
		}
		return err
	}
	keepModuleRadio = startup.sourceKind == "module"
	launchVoWiFiSession(session, a)
	return nil
}

// prepareReaderVoWiFiSession is the non-published startup phase.  Automatic
// recovery uses this same complete path, so it cannot reuse a stale SIM,
// carrier profile, proxy route, IKE SA or IMS registration.
func prepareReaderVoWiFiSession(ctx context.Context, a *app, proxyAddr, epdg string, epdgPort int, startup *vowifiStartup, reconnectAttempt int) (*vowifiSession, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if a == nil || a.readerMgr == nil {
		return nil, errors.New("读卡器管理器未初始化")
	}
	requestedProxy := proxyAddr

	proxyAddr = strings.TrimPrefix(strings.TrimPrefix(strings.TrimSpace(proxyAddr), "socks5://"), "socks5h://")
	proxyWasAutomatic := proxyAddr == ""
	if err := validateReaderVoWiFiProxy(proxyAddr); err != nil {
		return nil, err
	}
	info, readerName, generation, err := a.readerMgr.readerSnapshot()
	if startup != nil && startup.multiInfo != nil {
		info, readerName, generation, err = startup.multiInfo, startup.multiReader, startup.multiGeneration, nil
	}
	if err != nil {
		return nil, err
	}
	if startup != nil && startup.expectedGeneration != 0 && startup.expectedGeneration != generation {
		return nil, fmt.Errorf("读卡器或 eSIM Profile 已变更，已取消自动重连")
	}
	setVoWiFiStartupGeneration(startup, generation)
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	eapCapability := selectReaderEAPAKASessionCapability(readerName, generation, info)
	if startup != nil && startup.multiInfo != nil {
		eapCapability = readerEAPAKASessionCapability{}
	}
	if eapCapability.usesRustStandardEAP() {
		log.Printf("[Reader VoWiFi] EAP conversation owner=rust-standard reader_generation=%d", generation)
	} else {
		log.Printf("[Reader VoWiFi] EAP conversation owner=go reader_generation=%d", generation)
	}
	simID := strings.TrimSpace(info.ICCID)
	if simID == "" {
		simID = strings.TrimSpace(info.IMSI)
	}
	sessionKey, err := readerapp.NewSessionKey(readerapp.ReaderID(readerName), simID)
	if err != nil {
		return nil, fmt.Errorf("创建读卡器业务实例: %w", err)
	}

	identity := carrierIdentityFromUSIM(info)
	simInfo := &vowifi.SIMInfo{
		IMSI:            info.IMSI,
		MSISDN:          info.PhoneNumber,
		MCC:             info.MCC,
		MNC:             info.MNC,
		ICCID:           info.ICCID,
		PrivateIdentity: info.PrivateIdentity,
		PublicIdentity:  info.PublicIdentity,
		HomeDomain:      info.HomeDomain,
		SMSC:            info.SMSC,
		SMSCTarget:      info.SMSCTarget,
	}
	if isMissingPhoneNumber(simInfo.MSISDN) {
		simInfo.MSISDN = strings.TrimSpace(a.profilePhoneForICCID(info.ICCID))
	}
	effective, err := resolveSharedVoWiFiEffectiveConfig(ctx, a, identity, simInfo, epdg, epdgPort)
	if err != nil {
		return nil, err
	}
	if err := validateReaderVoWiFiStartCapabilities(effective); err != nil {
		return nil, err
	}
	profile := effective.Profile
	log.Printf("[Reader VoWiFi] Profile: %s id=%s source=%s override=%t (MCC=%s MNC=%s)",
		profile.Name, profile.ID, profile.Source, effective.OverrideMatched, profile.MCC, profile.MNC)
	if len(effective.DNSServers) > 0 {
		log.Printf("[Reader VoWiFi] dns_servers=%v retained as policy metadata; ePDG hostname will not be pre-resolved or replaced", effective.DNSServers)
	}

	tunnelCfg := vowifi.TunnelConfig{
		EPDGFQDN:                effective.EPDG,
		EPDGPort:                effective.EPDGPort,
		EPDGSource:              effective.EPDGSource,
		ProxyAddr:               proxyAddr,
		LocalIdentifier:         profile.ExpandLocalIdentifier(info.IMSI),
		IMEI:                    effective.DeviceIdentityIMEI,
		IMSDomain:               effective.IMSDomain,
		IMSAuthAlgorithm:        profile.IMSAuthAlgorithm,
		UseIPSec:                profile.UseIPSec,
		NATTKeepalive:           effective.NATTKeepaliveSeconds,
		IKEEncryption:           profile.IKEEncryption,
		IKEIntegrity:            profile.IKEIntegrity,
		IKEPRF:                  profile.IKEPRF,
		IKEDHGroup:              profile.IKEDHGroup,
		ChildEncryption:         profile.ChildEncryption,
		ChildIntegrity:          profile.ChildIntegrity,
		ChildDHGroup:            profile.ChildDHGroup,
		DisableSwitchToNATTPort: profile.DisableSwitchToNATTPort,
	}

	// The local Rust control endpoint bounds proxy host resolution and SOCKS
	// verification. Keep an explicit startup deadline here as a final guard so
	// a stalled local socket cannot leave the UI in a perpetual "starting"
	// state before IKE has even begun. A confirmed Surge System Proxy may need
	// one separate, bounded retry after Enhanced Mode is enabled; see
	// selectReaderTransportForStartup for the deliberately narrow guard.
	transportSelection, err := selectReaderTransportForStartup(ctx, proxyAddr, tunnelCfg.EPDGFQDN, proxyWasAutomatic)
	if err != nil {
		return nil, fmt.Errorf("前置代理传输: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if transportSelection.proxyError != "" {
		log.Printf("[reader-vowifi] system_proxy_diagnostic=%s source=%s", transportSelection.proxyError, transportSelection.proxySource)
	}
	transportFactory, transportMode := transportSelection.factory, transportSelection.mode
	log.Printf("[Reader VoWiFi] ePDG=%s:%d source=%s proxy=%t transport=%s",
		tunnelCfg.EPDGFQDN, tunnelCfg.EPDGPort, tunnelCfg.EPDGSource, transportMode == "socks5-udp-domain", transportMode)
	log.Printf("[reader-vowifi] transport=%s proxy_input=%t epdg=%s", transportMode, proxyAddr != "", tunnelCfg.EPDGFQDN)

	session := &vowifiSession{
		readerGeneration:  generation,
		eapCapability:     eapCapability,
		sourceKind:        "reader",
		carrier:           profile,
		simInfo:           simInfo,
		tunnelCfg:         tunnelCfg,
		effective:         effective,
		transportFactory:  transportFactory,
		transportMode:     transportMode,
		app:               a,
		requestedProxy:    requestedProxy,
		requestedEPDG:     epdg,
		requestedEPDGPort: epdgPort,
		intentID:          startup.intentID,
		reconnectAttempt:  reconnectAttempt,
		sessionKey:        sessionKey,
		tunnelState:       tunnelConnecting,
		imsState:          imsIdle,
		runDone:           make(chan struct{}),
		swuDone:           make(chan struct{}),
		ctx:               ctx,
	}
	return session, nil
}

// prepareModuleVoWiFiSession uses the same host-side SWu and IMS stack as the
// reader path, while the DJI module contributes only the selected SIM and AKA.
func prepareModuleVoWiFiSession(ctx context.Context, a *app, proxyAddr, epdg string, epdgPort int, startup *vowifiStartup, reconnectAttempt int) (*vowifiSession, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if a == nil || startup == nil || startup.moduleSIM == nil || startup.moduleSIM.info == nil {
		return nil, errors.New("DJI 模块 VoWiFi SIM 会话未初始化")
	}
	if err := validateReaderVoWiFiProxy(proxyAddr); err != nil {
		return nil, err
	}
	info := startup.moduleSIM.info
	if err := startup.moduleSIM.validateProfile(ctx); err != nil {
		return nil, err
	}
	simID := strings.TrimSpace(info.ICCID)
	if simID == "" {
		simID = strings.TrimSpace(info.IMSI)
	}
	sessionKey, err := readerapp.NewSessionKey("dji-module", simID)
	if err != nil {
		return nil, fmt.Errorf("创建 DJI 模块 VoWiFi 会话标识: %w", err)
	}
	identity := carrierIdentityFromUSIM(info)
	simInfo := &vowifi.SIMInfo{
		IMSI: info.IMSI, MSISDN: info.PhoneNumber, MCC: info.MCC, MNC: info.MNC,
		ICCID: info.ICCID, PrivateIdentity: info.PrivateIdentity,
		PublicIdentity: info.PublicIdentity, HomeDomain: info.HomeDomain,
		SMSC: info.SMSC, SMSCTarget: info.SMSCTarget,
	}
	effective, err := resolveSharedVoWiFiEffectiveConfig(ctx, a, identity, simInfo, epdg, epdgPort)
	if err != nil {
		return nil, err
	}
	if err := validateReaderVoWiFiStartCapabilities(effective); err != nil {
		return nil, err
	}
	profile := effective.Profile
	proxyWasAutomatic := strings.TrimSpace(proxyAddr) == ""
	tunnelCfg := vowifi.TunnelConfig{
		EPDGFQDN: effective.EPDG, EPDGPort: effective.EPDGPort,
		EPDGSource: effective.EPDGSource, ProxyAddr: proxyAddr,
		LocalIdentifier: profile.ExpandLocalIdentifier(info.IMSI),
		IMEI:            effective.DeviceIdentityIMEI, IMSDomain: effective.IMSDomain,
		IMSAuthAlgorithm: profile.IMSAuthAlgorithm, UseIPSec: profile.UseIPSec,
		NATTKeepalive: effective.NATTKeepaliveSeconds,
		IKEEncryption: profile.IKEEncryption, IKEIntegrity: profile.IKEIntegrity,
		IKEPRF: profile.IKEPRF, IKEDHGroup: profile.IKEDHGroup,
		ChildEncryption: profile.ChildEncryption, ChildIntegrity: profile.ChildIntegrity,
		ChildDHGroup:            profile.ChildDHGroup,
		DisableSwitchToNATTPort: profile.DisableSwitchToNATTPort,
	}
	transportSelection, err := selectReaderTransportForStartup(ctx, proxyAddr, tunnelCfg.EPDGFQDN, proxyWasAutomatic)
	if err != nil {
		return nil, fmt.Errorf("前置代理传输: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	log.Printf("[Module VoWiFi] ePDG=%s:%d transport=%s; DJI radio remains in CFUN=4", tunnelCfg.EPDGFQDN, tunnelCfg.EPDGPort, transportSelection.mode)
	return &vowifiSession{
		sourceKind: "module", moduleSIM: startup.moduleSIM,
		radioLease: startup.radioLease, radioRestore: startup.radioLease.restore,
		carrier: profile, simInfo: simInfo, tunnelCfg: tunnelCfg, effective: effective,
		transportFactory: transportSelection.factory, transportMode: transportSelection.mode,
		app: a, requestedProxy: proxyAddr, requestedEPDG: epdg, requestedEPDGPort: epdgPort,
		intentID: startup.intentID, reconnectAttempt: reconnectAttempt, sessionKey: sessionKey,
		tunnelState: tunnelConnecting, imsState: imsIdle, runDone: make(chan struct{}),
		swuDone: make(chan struct{}), ctx: ctx,
	}, nil
}

func setVoWiFiStartupGeneration(startup *vowifiStartup, generation uint64) {
	if startup == nil {
		return
	}
	vowifiSessionMu.Lock()
	if startingVoWiFi == startup {
		startup.readerGeneration = generation
	}
	vowifiSessionMu.Unlock()
}

func setVoWiFiStartupProgress(startup *vowifiStartup, step progressStep, desc string) {
	if startup == nil {
		return
	}
	vowifiSessionMu.Lock()
	if startingVoWiFi == startup {
		startup.progressStep = step
		startup.progressDesc = desc
	}
	vowifiSessionMu.Unlock()
}

func clearVoWiFiStartup(startup *vowifiStartup) {
	if startup == nil {
		return
	}
	vowifiSessionMu.Lock()
	if startingVoWiFi == startup {
		startingVoWiFi = nil
	}
	vowifiSessionMu.Unlock()
}

func publishVoWiFiSession(startup *vowifiStartup, session *vowifiSession) error {
	if startup == nil || session == nil {
		return errors.New("VoWiFi 会话启动结果不可用")
	}
	vowifiSessionMu.Lock()
	defer vowifiSessionMu.Unlock()
	if vowifiShuttingDown || startingVoWiFi != startup || vowifiIntentID != startup.intentID || startup.ctx.Err() != nil {
		return context.Canceled
	}
	if activeSession != nil || retiringSession != nil {
		return errors.New("VoWiFi 会话状态已变化，请重新启动")
	}
	if session.sourceKind == "module" {
		session.progressStep = startup.progressStep
		session.progressDesc = startup.progressDesc
	}
	session.cancel = startup.cancel
	session.ctx = startup.ctx
	if session.sourceKind != "module" {
		if err := readerVoWiFiSessions.Put(session.sessionKey, session); err != nil {
			return err
		}
	}
	activeSession = session
	startingVoWiFi = nil
	return nil
}

func launchVoWiFiSession(session *vowifiSession, a *app) {
	if session == nil {
		return
	}
	ctx := session.ctx
	go func() {
		defer close(session.runDone)
		if err := session.run(ctx, a); err != nil && ctx.Err() == nil {
			log.Printf("[Reader VoWiFi] Session error: %v", err)
			if session.reconnectAttempt > 0 {
				if shouldAutoReconnectVoWiFi(err) {
					session.handleRuntimeFailure(fmt.Errorf("自动恢复会话启动失败: %w", err))
				} else {
					session.fail(err)
					session.cleanupResources()
					scheduleVoWiFiSessionRemoval(session)
				}
			} else {
				session.fail(err)
				session.cleanupResources()
				scheduleVoWiFiSessionRemoval(session)
			}
		}
	}()
}

func shouldAutoReconnectVoWiFi(err error) bool {
	return err != nil &&
		!errors.Is(err, errReaderCardAbsent) &&
		!errors.Is(err, errReaderUnavailable) &&
		!errors.Is(err, errReaderISIMUnavailable)
}

const surgeCLIPath = "/Applications/Surge.app/Contents/Applications/surge-cli"

var (
	surgeCLIAvailable = func() bool {
		_, err := os.Stat(surgeCLIPath)
		return err == nil
	}
	surgeFeatureStatus = surgeFeatureEnabled
	surgeEnableMode    = func(ctx context.Context) ([]byte, error) {
		return exec.CommandContext(ctx, surgeCLIPath, "feature", "set", "enhanced-mode", "on").CombinedOutput()
	}
)

// shouldManageSurgeEnhancedMode intentionally relies on the Rust discovery
// source, rather than matching a loopback port. This prevents a system SOCKS
// listener owned by Clash/mihomo from being mistaken for Surge.
func shouldManageSurgeEnhancedMode(proxySource string) bool {
	return strings.TrimSpace(proxySource) == "surge-effective-profile"
}

// selectReaderTransportForStartup selects one legal ePDG route. It has one
// recoverable exception for an automatic, Rust-verified Surge System Proxy:
// when SOCKS UDP is unavailable, no fake/utun route is yet verified, or Rust
// currently selected a direct route despite owning the system proxy, enable
// Enhanced Mode and make exactly one fresh selection request.
//
// An explicit endpoint, macOS SOCKS endpoint, unknown source, malformed
// response, or any other policy error never enters this branch. In particular
// a Clash listener sharing Surge's historical port cannot make Go inspect or
// mutate Surge.
func selectReaderTransportForStartup(ctx context.Context, proxyAddr, epdgHost string, proxyWasAutomatic bool) (readerSWUTransportSelection, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	selection, firstErr := selectReaderTransportWithStartupTimeout(ctx, proxyAddr, epdgHost)
	if firstErr == nil && !shouldRetrySurgeTransportSelection(selection, proxyWasAutomatic) {
		// A working Rust-selected route needs no control-plane mutation. In
		// particular, a valid Surge SOCKS UDP path is already sufficient for
		// IKE/NAT-T and must not cause an unsolicited Enhanced Mode change.
		return selection, nil
	}
	if firstErr != nil && !shouldRetrySurgeTransportSelection(selection, proxyWasAutomatic) {
		return selection, firstErr
	}

	// The just-observed Rust source is necessary but not sufficient authority
	// to change another application's state. Confirm Surge owns the *current*
	// macOS System Proxy immediately before enabling Enhanced Mode. The sole
	// post-enable Rust reselect below must also retain that ownership source.
	log.Printf("[reader-vowifi] retrying Rust transport selection after Surge Enhanced Mode recovery mode=%s error_code=%s", selection.mode, selection.errorCode)
	recoveryCtx, cancelRecovery := context.WithTimeout(ctx, readerVoWiFiSurgeRecoveryTimeout)
	defer cancelRecovery()
	if err := ensureSurgeUDPMode(recoveryCtx, selection.proxySource, true); err != nil {
		if recoveryCtx.Err() != nil {
			return selection, fmt.Errorf("Surge 增强模式自动恢复在 %s 内未完成；请在 Surge 中手动确认“系统代理”和“增强模式”后重试: %w", readerVoWiFiSurgeRecoveryTimeout, err)
		}
		return selection, err
	}

	// This retry deliberately starts a new bounded request after the local
	// Surge state transition. Reusing the first context would turn a successful
	// CLI operation into a guaranteed timeout; the retry remains capped at the
	// same seven-second selection budget and only runs once.
	retrySelection, retryErr := selectReaderTransportWithStartupTimeout(recoveryCtx, proxyAddr, epdgHost)
	if recoveryCtx.Err() != nil {
		return retrySelection, fmt.Errorf("Surge 增强模式恢复后重新选择 ePDG 传输超时；请在 Surge 中手动确认后重试: %w", recoveryCtx.Err())
	}
	if !shouldManageSurgeEnhancedMode(retrySelection.proxySource) {
		if retryErr != nil {
			return retrySelection, fmt.Errorf("Surge 增强模式恢复后，Rust 未确认当前代理仍由 Surge 接管（source=%q）: %w", retrySelection.proxySource, retryErr)
		}
		return retrySelection, fmt.Errorf("Surge 增强模式恢复后，Rust 未确认当前代理仍由 Surge 接管（source=%q）；请检查 Surge 系统代理后重试", retrySelection.proxySource)
	}
	if retryErr != nil {
		return retrySelection, retryErr
	}
	if retrySelection.proxyError != "" {
		log.Printf("[reader-vowifi] system_proxy_diagnostic=%s source=%s", retrySelection.proxyError, retrySelection.proxySource)
	}
	return retrySelection, nil
}

func selectReaderTransportWithStartupTimeout(ctx context.Context, proxyAddr, epdgHost string) (readerSWUTransportSelection, error) {
	selectionCtx, cancel := context.WithTimeout(ctx, readerVoWiFiTransportSelectionTimeout)
	defer cancel()
	selection, err := selectReaderSWUTransportWithSelection(selectionCtx, proxyAddr, epdgHost)
	if ctx.Err() != nil {
		return selection, ctx.Err()
	}
	return selection, err
}

func shouldRetrySurgeTransportSelection(selection readerSWUTransportSelection, proxyWasAutomatic bool) bool {
	if !proxyWasAutomatic || !shouldManageSurgeEnhancedMode(selection.proxySource) {
		return false
	}
	if selection.mode == "direct" {
		return true
	}
	if selection.mode != "rejected" {
		return false
	}
	switch strings.TrimSpace(selection.errorCode) {
	case "socks5_udp_unavailable", "unverified_public_route":
		return true
	default:
		return false
	}
}

// ensureSurgeUDPMode bridges the gap between a Rust-verified Surge System
// Proxy and IKE/NAT-T UDP. HTTP/PAC endpoints are never passed here, and an
// explicit proxy is never attributed to Surge merely because it uses a shared
// loopback port.
func ensureSurgeUDPMode(ctx context.Context, proxySource string, proxyWasAutomatic bool) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if !shouldManageSurgeEnhancedMode(proxySource) {
		return fmt.Errorf("Rust 未确认当前代理由 Surge 接管，已取消自动开启增强模式")
	}
	if !surgeCLIAvailable() {
		return fmt.Errorf("Rust 选择了 Surge 系统代理，但 surge-cli 不可用；请启动 Surge 并手动开启“增强模式”，或清空代理后重试")
	}
	surgeOwnsSystemProxy, err := surgeFeatureStatus(ctx, "system-proxy")
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return fmt.Errorf("无法确认 Surge 当前系统代理状态；请在 Surge 中确认“系统代理”已开启后重试: %w", err)
	}
	if !surgeOwnsSystemProxy {
		return fmt.Errorf("Rust 选择的 Surge 系统代理当前未开启；请在 Surge 中开启“系统代理”和“增强模式”，或清空代理后重试")
	}

	enabled, err := surgeFeatureStatus(ctx, "enhanced-mode")
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return fmt.Errorf("检测 Surge 增强模式失败: %w", err)
	}
	if enabled {
		return nil
	}

	enableCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	output, err := surgeEnableMode(enableCtx)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if enableCtx.Err() != nil {
			return fmt.Errorf("Surge 仅开启了系统代理，自动开启增强模式超时；请在 Surge 中手动开启“增强模式”后重试")
		}
		return fmt.Errorf("Surge 仅开启了系统代理，自动开启增强模式失败: %s", strings.TrimSpace(string(output)))
	}

	var lastCheckErr error
	for attempt := 0; attempt < 10; attempt++ {
		if attempt > 0 {
			timer := time.NewTimer(500 * time.Millisecond)
			select {
			case <-ctx.Done():
				timer.Stop()
				return ctx.Err()
			case <-timer.C:
			}
		}
		if enabled, checkErr := surgeFeatureStatus(ctx, "enhanced-mode"); checkErr == nil && enabled {
			log.Printf("[reader-vowifi] Surge Enhanced Mode enabled for UDP 500/4500 (automatic_proxy=%t)", proxyWasAutomatic)
			return nil
		} else if checkErr != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			lastCheckErr = checkErr
		}
	}
	if lastCheckErr != nil {
		return fmt.Errorf("无法确认 Surge 增强模式是否已经开启；请在 Surge 中手动确认后重试: %w", lastCheckErr)
	}
	return fmt.Errorf("Surge 仅开启了系统代理，增强模式未能生效；请在 Surge 中手动开启“增强模式”后重试")
}

func surgeFeatureEnabled(ctx context.Context, feature string) (bool, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	statusCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	output, err := exec.CommandContext(statusCtx, surgeCLIPath, "feature", "get", feature).CombinedOutput()
	if err != nil {
		if ctx.Err() != nil {
			return false, ctx.Err()
		}
		if statusCtx.Err() != nil {
			return false, context.DeadlineExceeded
		}
		return false, fmt.Errorf("%s", strings.TrimSpace(string(output)))
	}
	value := strings.ToLower(strings.TrimSpace(string(output)))
	prefix := strings.ToLower(feature) + ": "
	if strings.Contains(value, prefix+"on") {
		return true, nil
	}
	if strings.Contains(value, prefix+"off") {
		return false, nil
	}
	return false, fmt.Errorf("无法识别 Surge 状态 %q", strings.TrimSpace(string(output)))
}

// validateReaderVoWiFiProxy validates an optional user supplied proxy. An
// empty value deliberately means "let Rust inspect a usable macOS SOCKS or
// verified Surge System Proxy first, then use the current network directly if
// no such path exists". HTTP/PAC settings are never reinterpreted as a UDP
// transport, and a proxy remains optional where the current network can route
// ePDG directly.
func validateReaderVoWiFiProxy(proxyAddr string) error {
	if strings.TrimSpace(proxyAddr) == "" {
		return nil
	}
	host, portText, err := net.SplitHostPort(proxyAddr)
	if err != nil || strings.TrimSpace(host) == "" {
		return fmt.Errorf("前置代理地址无效，请手动填写有效的 IP:端口")
	}
	port, err := strconv.Atoi(portText)
	if err != nil || port < 1 || port > 65535 {
		return fmt.Errorf("前置代理端口无效")
	}
	return nil
}

// run establishes IKEv2/EAP-AKA on UDP 500, lets the same transport switch to
// NAT-T UDP 4500, then carries ESP and all IMS packets through the userspace
// dataplane. Transport policy was already validated before the session became
// active, so failures such as a missing Surge fake-IP are returned immediately.
func (s *vowifiSession) run(ctx context.Context, a *app) error {
	if s.sourceKind != "module" {
		s.setProgress(progressSIM, "SIM 卡已就绪")
	}
	s.setProgress(progressAccess, "正在通过 EAP-AKA 建立 IKEv2 隧道...")

	ready := make(chan struct{})
	var readyOnce sync.Once
	deviceIdentityEnabled := s.effective.DeviceIdentityEnabled
	swuCfg := &swucore.Config{
		EpDGAddr:  s.tunnelCfg.EPDGFQDN,
		EpDGPort:  uint16(s.tunnelCfg.EPDGPort),
		APN:       s.effective.APN,
		IPStack:   s.effective.IPStack,
		LocalAddr: "", // 不绑定宿主机本地 IP，让 mihomo/TUN 自然接管。
		LocalPort: 0,
		SIM: &swuSIMAdapter{
			rm:            a.readerMgr,
			moduleSIM:     s.moduleSIM,
			generation:    s.readerGeneration,
			imsi:          s.simInfo.IMSI,
			imei:          s.tunnelCfg.IMEI,
			sessionCtx:    ctx,
			eapCapability: s.eapCapability,
		},
		EnableDriver:            true,
		DataplaneMode:           "netstack",
		MCC:                     s.simInfo.MCC,
		MNC:                     s.simInfo.MNC,
		DisableSwitchToNATTPort: s.tunnelCfg.DisableSwitchToNATTPort,
		ReauthInterval:          s.effective.ReauthIntervalSeconds,
		NATTKeepaliveSeconds:    s.effective.NATTKeepaliveSeconds,
		DPDIntervalSeconds:      s.effective.DPDIntervalSeconds,
		DisableDPD:              s.effective.DisableDPD,
		IKEProposals:            append([]string(nil), s.effective.IKEProposals...),
		ESPProposals:            append([]string(nil), s.effective.ESPProposals...),
		AKAChallengeMode:        s.effective.AKAChallengeMode,
		DeviceIdentityEnabled:   &deviceIdentityEnabled,
		OnReady: func() {
			readyOnce.Do(func() { close(ready) })
		},
	}

	if s.multiAuth != nil {
		swuCfg.SIM = s.multiAuth
		swuCfg.ExternalReauthOwner = true
		swuCfg.SkipInitialContact = true
	}
	if localID := strings.TrimSpace(s.tunnelCfg.LocalIdentifier); localID != "" {
		swuCfg.IKELocalIdentifier = localID
	}
	swuCfg.TransportFactory = s.transportFactory

	swuSession := swucore.NewSession(swuCfg, nil)
	s.mu.Lock()
	s.swuSession = swuSession
	s.mu.Unlock()

	swuErr := make(chan error, 1)
	go func() {
		defer close(s.swuDone)
		swuErr <- swuSession.Connect(ctx)
	}()
	startupTimer := time.NewTimer(readerVoWiFiIKEStartupTimeout)
	defer startupTimer.Stop()

	select {
	case <-ctx.Done():
		return ctx.Err()
	case err := <-swuErr:
		if err == nil {
			err = fmt.Errorf("SWu 会话在隧道就绪前退出")
		}
		if isReaderIKETimeout(err) {
			return readerIKEStartupError(s.transportMode, err)
		}
		return fmt.Errorf("IKE/ESP 隧道: %w", err)
	case <-startupTimer.C:
		return readerIKEStartupError(s.transportMode, context.DeadlineExceeded)
	case <-ready:
	}

	snapshot := swuSession.Snapshot()
	if !snapshot.Established {
		return fmt.Errorf("IKE 已完成但 Child SA 数据面未就绪")
	}
	result := tunnelResultFromSnapshot(snapshot)
	localIP, pcscfCandidates, err := selectIMSPathForConfig(snapshot, s.effective.IPStack, s.effective.IMSPCSCF)
	if err != nil {
		return err
	}
	result.PCSCF = append([]string(nil), pcscfCandidates...)
	s.mu.Lock()
	s.tunnelResult = result
	s.tunnelState = tunnelConnected
	s.mu.Unlock()
	s.setProgress(progressTunnel, "IKEv2、NAT-T 与用户态 ESP 隧道已就绪")
	log.Printf("[Reader VoWiFi] Tunnel ready: IPv4=%s IPv6=%s P-CSCF=%v",
		result.InnerIPv4, result.InnerIPv6, result.PCSCF)

	go s.watchSWU(ctx, swuErr)

	s.setProgress(progressIMS, "正在通过隧道注册 IMS...")
	s.mu.Lock()
	s.imsState = imsRegistering
	s.mu.Unlock()

	impi, impu, err := resolveReaderIMSIdentities(s.effective, s.simInfo)
	if err != nil {
		return err
	}

	registerTemplate := s.effective.IMSRegisterTemplate
	if !s.tunnelCfg.UseIPSec {
		registerTemplate.SecAgreeMode = "disabled"
		registerTemplate.StrictSecurityServerOffer = false
	}
	transport := strings.ToLower(strings.TrimSpace(s.effective.IMSTransport))
	if transport == "" || transport == "auto" {
		transport = "tcp"
	}
	userAgent := readerIMSUserAgent(s.carrier.ID, s.effective.DeviceModel)
	imsCfg := imscore.IMSConfig{
		Enabled:                    true,
		DeviceID:                   s.tunnelCfg.IMEI,
		PCSCF:                      pcscfCandidates[0],
		Registrar:                  pcscfCandidates[0],
		Domain:                     s.effective.IMSDomain,
		Realm:                      s.effective.IMSRealm,
		CallPhoneContext:           s.effective.IMSPhoneContext,
		IMPI:                       impi,
		IMPU:                       impu,
		CarrierPresetID:            s.carrier.ID,
		IMSRegisterTemplate:        registerTemplate,
		IMSRegisterPolicySource:    s.carrier.Source,
		LocalAddr:                  localIP.String(),
		LocalPort:                  s.effective.IMSLocalPort,
		Transport:                  transport,
		UserAgent:                  userAgent,
		SIPInstance:                readerIMSSIPInstance(s.tunnelCfg.IMEI),
		TCPKeepaliveSeconds:        s.effective.IMSTCPKeepaliveSeconds,
		OptionsPingIntervalSeconds: s.effective.IMSOptionsPingIntervalSeconds,
	}
	imsNetwork, err := imscore.NewUserspaceIMSNetwork(localIP, swuSession)
	if err != nil {
		return fmt.Errorf("IMS 用户态网络: %w", err)
	}
	var sessionAKA imssim.AKAProvider = &imsAKAAdapter{rm: a.readerMgr, moduleSIM: s.moduleSIM, generation: s.readerGeneration, sessionCtx: s.ctx}
	if s.multiAuth != nil {
		sessionAKA = &multiIMSAKA{adapter: s.multiAuth}
	}
	imsService, err := imscore.StartSessionIMSCore(ctx, imsCfg, imsNetwork, imscore.StartSessionInput{
		TraceID:             fmt.Sprintf("reader-%d", time.Now().UnixNano()),
		LocalIP:             localIP,
		Dataplane:           swuSession,
		RegistrarCandidates: pcscfCandidates,
		AKA:                 sessionAKA,
		SMSOnly:             s.multiAuth != nil,
		IncomingRawSMS:      s.multiIncoming,
		IMSI:                s.simInfo.IMSI,
		MCC:                 s.simInfo.MCC,
		MNC:                 s.simInfo.MNC,
		DecodeIncomingRPDU: func(body []byte) (byte, string, []byte, error) {
			return decodeRuntimeRPDU(ctx, body)
		},
		DecodeIncomingSMS: func(tpdu []byte) (imscore.InboundSMS, bool, error) {
			return decodeRuntimeSMSDeliver(ctx, tpdu)
		},
		DTMFEvent:                resolveRuntimeDTMF,
		ValidateRegisterResponse: validateRuntimeIMSRegisterResponse,
		IncomingSMS: func(sender, content string, at time.Time) error {
			if s.sourceKind == "module" {
				a.recordSMSFromDevice(sender, content, at, "dji")
				return nil
			}
			return a.recordReaderSMS(sender, content, at)
		},
		RegisterExpirySeconds: registerTemplate.Expires,
	})
	if err != nil {
		s.mu.Lock()
		s.imsState = imsFailed
		s.mu.Unlock()
		return fmt.Errorf("IMS 注册: %w", err)
	}
	securityMode := "plain"
	imsStatus := imsService.Status()
	if value, ok := imsStatus["effective_security"].(string); ok && value != "" {
		securityMode = value
	}
	winningPCSCF := pcscfCandidates[0]
	if value, ok := imsStatus["registrar"].(string); ok && strings.TrimSpace(value) != "" {
		winningPCSCF = strings.TrimSpace(value)
		pcscfCandidates = prioritizeRegistrar(pcscfCandidates, winningPCSCF)
		s.mu.Lock()
		if s.tunnelResult != nil {
			s.tunnelResult.PCSCF = append([]string(nil), pcscfCandidates...)
		}
		s.mu.Unlock()
	}
	registeredMSISDN := s.simInfo.MSISDN
	registrationExpires := 0
	if value, ok := imsStatus["expires_seconds"].(int); ok {
		registrationExpires = value
	} else if value, ok := imsStatus["expires_seconds"].(float64); ok {
		registrationExpires = int(value)
	}
	if value, ok := imsStatus["registered_msisdn"].(string); ok && !isMissingPhoneNumber(value) {
		registeredMSISDN = strings.TrimSpace(value)
		if s.multiAuth == nil && s.sourceKind != "module" && a.readerMgr != nil {
			a.readerMgr.UpdatePhoneNumber(s.readerGeneration, registeredMSISDN)
		}
		if s.multiAuth == nil {
			a.rememberProfilePhone(s.simInfo.ICCID, registeredMSISDN)
		}
	}
	s.imsOpMu.Lock()
	s.mu.Lock()
	if err := ctx.Err(); err != nil {
		s.mu.Unlock()
		s.imsOpMu.Unlock()
		closeCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		_ = imsService.Close(closeCtx)
		cancel()
		return err
	}
	s.imsService = imsService
	s.imsState = imsRegistered
	s.imsResult = &vowifi.IMSRegisterResult{
		Registered:   true,
		MSISDN:       registeredMSISDN,
		Transport:    transport,
		SecurityMode: securityMode,
	}
	s.mu.Unlock()
	s.imsOpMu.Unlock()
	smsRouteReady := s.simInfo != nil && moduleSMSRouteReady(s.simInfo.SMSC, s.simInfo.SMSCTarget)
	transportReady := smsRouteReady && waitForIMSMessagingReady(ctx, imsService, 3*time.Second)
	if transportReady {
		s.setProgress(progressSMS, "IMS 已注册，SMS 安全通道已就绪")
	} else {
		s.setProgress(progressIMS, "IMS 已注册，正在等待 SMS 安全通道")
		log.Printf("[%s VoWiFi] SMS readiness pending: route_metadata=%t transport=%t",
			voWiFiSourceLabel(s.sourceKind), smsRouteReady, imsService.MessagingReady())
	}
	log.Printf("[Reader VoWiFi] IMS registered: P-CSCF=%s security=%s", winningPCSCF, securityMode)
	go s.watchIMS(ctx, imsService)
	delay, err := runtimeRegistrationRenewalDelay(ctx, registrationExpires)
	if err != nil {
		return err
	}
	log.Printf("[Reader VoWiFi] IMS registration lease scheduled (granted=%ds renew_in=%s)", registrationExpires, delay.Round(time.Second))
	go s.watchRegistrationLease(ctx, delay, registrationExpires)
	return nil
}

func waitForIMSMessagingReady(ctx context.Context, service *imscore.Service, timeout time.Duration) bool {
	if service == nil {
		return false
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if timeout <= 0 {
		return service.MessagingReady()
	}
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()
	for {
		if service.MessagingReady() {
			return true
		}
		select {
		case <-ctx.Done():
			return false
		case <-timer.C:
			return service.MessagingReady()
		case <-ticker.C:
		}
	}
}

func prioritizeRegistrar(candidates []string, winning string) []string {
	winning = strings.TrimSpace(winning)
	if winning == "" {
		return append([]string(nil), candidates...)
	}
	result := make([]string, 0, len(candidates)+1)
	result = append(result, winning)
	for _, candidate := range candidates {
		candidate = strings.TrimSpace(candidate)
		if candidate != "" && candidate != winning {
			result = append(result, candidate)
		}
	}
	return result
}

func (s *vowifiSession) watchIMS(ctx context.Context, service *imscore.Service) {
	if service == nil {
		return
	}
	select {
	case <-ctx.Done():
		return
	case err := <-service.TransportFailure():
		if ctx.Err() != nil || err == nil {
			return
		}
		log.Printf("[Reader VoWiFi] IMS transport stopped: %v", err)
		s.handleRuntimeFailure(fmt.Errorf("IMS 安全通道中断，正在恢复连接: %w", err))
	}
}

func (s *vowifiSession) watchRegistrationLease(ctx context.Context, delay time.Duration, expiresSeconds int) {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return
	case <-timer.C:
		log.Printf("[Reader VoWiFi] IMS registration lease renewal due (granted=%ds)", expiresSeconds)
		s.handleRuntimeFailure(fmt.Errorf("IMS 注册有效期即将结束，正在刷新连接"))
	}
}

func isReaderIKETimeout(err error) bool {
	if err == nil {
		return false
	}
	message := strings.ToLower(err.Error())
	return strings.Contains(message, "timeout") || strings.Contains(message, "超时")
}

func readerIKEStartupError(transportMode string, cause error) error {
	if cause == nil {
		cause = context.DeadlineExceeded
	}
	switch strings.TrimSpace(transportMode) {
	case "enhanced-mode-utun", "enhanced-mode-fake-ip":
		return fmt.Errorf("IKE/ESP 隧道: 前置代理透明路由已接管，但 ePDG 在 %s 内未响应；请检查代理分流、UDP 500/4500 与所选线路，或清空代理后尝试网络直连: %w", readerVoWiFiIKEStartupTimeout, cause)
	case "socks5-udp-domain":
		return fmt.Errorf("IKE/ESP 隧道: SOCKS5 UDP 已建立，但 ePDG 在 %s 内未响应；请检查代理分流、UDP 500/4500 与所选线路，或清空代理后尝试网络直连: %w", readerVoWiFiIKEStartupTimeout, cause)
	default:
		return fmt.Errorf("IKE/ESP 隧道: ePDG 在 %s 内未响应；请检查网络或前置代理的 UDP 500/4500 设置: %w", readerVoWiFiIKEStartupTimeout, cause)
	}
}

func readerIMSUserAgent(carrierID, deviceModel string) string {
	if strings.EqualFold(strings.TrimSpace(carrierID), "apple_o2_giffgaff_uk") {
		return "iOS/18.6.2 iPhone"
	}
	if model := strings.TrimSpace(deviceModel); model != "" {
		return "iOS/26.6 " + model
	}
	return "iOS/26.6 iPhone"
}

func readerIMSSIPInstance(imei string) string {
	imei = strings.TrimSpace(imei)
	if len(imei) != 15 {
		return ""
	}
	for _, digit := range imei {
		if digit < '0' || digit > '9' {
			return ""
		}
	}
	// Preserve the established registration identity for all carriers. The
	// CTExcel formatting candidate did not restore terminating calls, so it
	// is not adopted as a routing fix. Historical giffgaff INVITEs selected
	// this exact value through Accept-Contact; do not rotate the underlying IMEI.
	return "urn:gsma:imei:" + imei + "-0"
}

func (s *vowifiSession) watchSWU(ctx context.Context, swuErr <-chan error) {
	select {
	case <-ctx.Done():
		return
	case err := <-swuErr:
		if ctx.Err() != nil {
			return
		}
		if err == nil {
			err = fmt.Errorf("SWu 会话意外退出")
		}
		log.Printf("[Reader VoWiFi] Tunnel stopped: %v", err)
		s.handleRuntimeFailure(fmt.Errorf("IKE/ESP 隧道中断，正在恢复连接: %w", err))
	}
}

func (s *vowifiSession) handleRuntimeFailure(err error) {
	if s == nil || err == nil {
		return
	}
	if s.multiFailure != nil {
		s.failureOnce.Do(func() { s.fail(err); s.multiFailure(err) })
		return
	}
	s.failureOnce.Do(func() {
		s.fail(err)
		go s.reconnectAfterFailure()
	})
}

func (s *vowifiSession) reconnectAfterFailure() {
	s.cleanupResources()
	_ = waitForVoWiFiSessionExit(s)

	attempt := s.reconnectAttempt + 1
	delay, err := runtimeReconnectDelay(attempt)
	if err != nil {
		log.Printf("[Reader VoWiFi] Automatic reconnect policy failed: %v", err)
		s.fail(fmt.Errorf("自动重连策略不可用: %w", err))
		scheduleVoWiFiSessionRemoval(s)
		return
	}
	s.setProgress(progressAccess, fmt.Sprintf("连接已失效，%s 后自动重连（第 %d 次）", delay, attempt))
	timer := time.NewTimer(delay)
	defer timer.Stop()
	<-timer.C

	startup := reserveVoWiFiReconnect(s)
	if startup == nil {
		return
	}
	if err := startReservedVoWiFi(s.app, s.requestedProxy, s.requestedEPDG, s.requestedEPDGPort, startup, attempt); err != nil {
		if startup.ctx.Err() != nil {
			return
		}
		log.Printf("[Reader VoWiFi] Automatic reconnect attempt %d failed: %v", attempt, err)
		// Startup errors remain visible and deliberately stop this recovery
		// chain. Retrying a bad SIM/profile/proxy forever would hide the actual
		// configuration error from the user.
		vowifiSessionMu.Lock()
		if vowifiIntentID == s.intentID && activeSession == nil && startingVoWiFi == nil {
			activeSession = s
		}
		vowifiSessionMu.Unlock()
		s.fail(fmt.Errorf("自动重连失败: %w", err))
		scheduleVoWiFiSessionRemoval(s)
	}
}

func scheduleVoWiFiSessionRemoval(session *vowifiSession) {
	time.AfterFunc(3*time.Second, func() {
		vowifiSessionMu.Lock()
		if activeSession == session {
			activeSession = nil
		}
		unregisterReaderVoWiFiSession(session)
		vowifiSessionMu.Unlock()
	})
}

func unregisterReaderVoWiFiSession(session *vowifiSession) {
	if session == nil || session.sourceKind == "module" {
		return
	}
	if current, ok := readerVoWiFiSessions.Get(session.sessionKey); ok && current == session {
		readerVoWiFiSessions.Delete(session.sessionKey)
	}
}

func tunnelResultFromSnapshot(snapshot swucore.SessionSnapshot) *vowifi.TunnelResult {
	result := &vowifi.TunnelResult{}
	if snapshot.IPv4 != nil {
		result.InnerIPv4 = snapshot.IPv4.String()
	}
	if snapshot.IPv6 != nil {
		result.InnerIPv6 = snapshot.IPv6.String()
	}
	for _, ip := range append(append([]net.IP(nil), snapshot.PCSCFv4...), snapshot.PCSCFv6...) {
		result.PCSCF = append(result.PCSCF, ip.String())
	}
	for _, ip := range append(append([]net.IP(nil), snapshot.DNSv4...), snapshot.DNSv6...) {
		result.DNS = append(result.DNS, ip.String())
	}
	return result
}

// selectIMSPath keeps the tunnel address and P-CSCF in the same IP family.
func selectIMSPath(snapshot swucore.SessionSnapshot) (net.IP, []string, error) {
	return selectIMSPathForConfig(snapshot, "ipv4v6", "")
}

func selectIMSPathForConfig(snapshot swucore.SessionSnapshot, ipStack, configuredPCSCF string) (net.IP, []string, error) {
	ipStack = strings.ToLower(strings.TrimSpace(ipStack))
	if ipStack == "" {
		ipStack = "ipv4v6"
	}
	if configuredPCSCF = strings.TrimSpace(configuredPCSCF); configuredPCSCF != "" {
		host, _, err := net.SplitHostPort(configuredPCSCF)
		if err != nil {
			return nil, nil, fmt.Errorf("ims_pcscf 地址无效: %w", err)
		}
		ip := net.ParseIP(strings.Trim(host, "[]"))
		if ip == nil {
			return nil, nil, fmt.Errorf("ims_pcscf 必须是数字 IP")
		}
		if ip.To4() != nil {
			if ipStack == "ipv6" {
				return nil, nil, fmt.Errorf("ims_pcscf 是 IPv4，但 ip_stack=ipv6")
			}
			if snapshot.IPv4 == nil {
				return nil, nil, fmt.Errorf("ePDG 未下发与 ims_pcscf 匹配的 IPv4 隧道地址")
			}
			return append(net.IP(nil), snapshot.IPv4...), []string{configuredPCSCF}, nil
		}
		if ipStack == "ipv4" {
			return nil, nil, fmt.Errorf("ims_pcscf 是 IPv6，但 ip_stack=ipv4")
		}
		if snapshot.IPv6 == nil {
			return nil, nil, fmt.Errorf("ePDG 未下发与 ims_pcscf 匹配的 IPv6 隧道地址")
		}
		return append(net.IP(nil), snapshot.IPv6...), []string{configuredPCSCF}, nil
	}

	var localIP net.IP
	var pcscf []net.IP
	switch ipStack {
	case "ipv4":
		if snapshot.IPv4 != nil && len(snapshot.PCSCFv4) > 0 {
			localIP, pcscf = snapshot.IPv4, snapshot.PCSCFv4
		}
	case "ipv6":
		if snapshot.IPv6 != nil && len(snapshot.PCSCFv6) > 0 {
			localIP, pcscf = snapshot.IPv6, snapshot.PCSCFv6
		}
	case "ipv4v6":
		switch {
		case snapshot.IPv6 != nil && len(snapshot.PCSCFv6) > 0:
			localIP, pcscf = snapshot.IPv6, snapshot.PCSCFv6
		case snapshot.IPv4 != nil && len(snapshot.PCSCFv4) > 0:
			localIP, pcscf = snapshot.IPv4, snapshot.PCSCFv4
		}
	default:
		return nil, nil, fmt.Errorf("不支持的 ip_stack %q", ipStack)
	}
	if localIP == nil || len(pcscf) == 0 {
		switch {
		case snapshot.IPv4 == nil && snapshot.IPv6 == nil:
			return nil, nil, fmt.Errorf("ePDG 未下发隧道内 IP 地址")
		case ipStack == "ipv4":
			return nil, nil, fmt.Errorf("ip_stack=ipv4，但 ePDG 未下发成对的 IPv4 地址和 P-CSCF")
		case ipStack == "ipv6":
			return nil, nil, fmt.Errorf("ip_stack=ipv6，但 ePDG 未下发成对的 IPv6 地址和 P-CSCF")
		default:
			return nil, nil, fmt.Errorf("ePDG 未下发与隧道地址同族的 P-CSCF")
		}
	}
	candidates := make([]string, 0, len(pcscf))
	for _, ip := range pcscf {
		candidates = append(candidates, net.JoinHostPort(ip.String(), "5060"))
	}
	return append(net.IP(nil), localIP...), candidates, nil
}

func (s *vowifiSession) setProgress(step progressStep, desc string) {
	s.mu.Lock()
	s.progressStep = step
	s.progressDesc = desc
	s.mu.Unlock()
	log.Printf("[Reader VoWiFi] Progress: %d/%s — %s", step, progressStepName(step), desc)
}

func (s *vowifiSession) fail(err error) {
	s.mu.Lock()
	s.tunnelState = tunnelError
	s.imsState = imsFailed
	s.lastError = err.Error()
	s.mu.Unlock()
}

func (s *vowifiSession) cleanupResources() {
	s.cancelOnce.Do(func() {
		if s.cancel != nil {
			s.cancel()
		}
	})
	s.imsOpMu.Lock()
	defer s.imsOpMu.Unlock()
	s.mu.Lock()
	service := s.imsService
	s.imsService = nil
	s.mu.Unlock()
	if service != nil {
		closeCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = service.Close(closeCtx)
	}
	s.radioRestoreOnce.Do(func() {
		if s.radioRestore != nil {
			s.radioRestoreErr = s.radioRestore()
			if s.radioRestoreErr != nil {
				log.Printf("[Module VoWiFi] Failed to restore cellular radio mode: %v", s.radioRestoreErr)
			}
		}
	})
}

func stopVoWiFi() {
	_ = stopVoWiFiMatching(func(_ *vowifiSession) bool { return true })
}

// stopVoWiFiForReaderGeneration retires only the session that belonged to a
// confirmed hot-unplug observation. A user may reconnect a reader and start a
// newer session between the monitor's inventory sample and this teardown; a
// broad stop at that point would incorrectly terminate the new call/SMS path.
func stopVoWiFiForReaderGeneration(generation uint64) {
	_ = stopVoWiFiMatching(func(session *vowifiSession) bool {
		return session != nil && session.sourceKind != "module" && session.readerGeneration == generation
	})
}

func stopVoWiFiMatching(matches func(*vowifiSession) bool) bool {
	vowifiSessionMu.Lock()
	session := activeSession
	startup := startingVoWiFi
	sessionMatches := matches == nil || matches(session)
	startupMatches := startup != nil && (matches == nil || matches(&vowifiSession{
		readerGeneration: startup.readerGeneration,
		sourceKind:       startup.sourceKind,
	}))
	if !sessionMatches && !startupMatches {
		vowifiSessionMu.Unlock()
		return false
	}
	vowifiIntentID++
	if startupMatches {
		// Cancellation is intentionally issued while only the short state lock
		// is held. The slow Rust selection/Surge CLI operation runs outside it,
		// and publishVoWiFiSession will reject this now-stale reservation.
		startup.cancel()
	}
	// Detach first. A slow or wedged IKE/IMS teardown must never leave the
	// public state stuck in "running" and permanently block the next start.
	// Every callback already compares the session pointer before touching the
	// global slot, so a retiring session cannot remove a newer one.
	if sessionMatches && activeSession == session {
		activeSession = nil
	}
	if sessionMatches {
		unregisterReaderVoWiFiSession(session)
	}
	if sessionMatches && session != nil {
		retiringSession = session
	}
	vowifiSessionMu.Unlock()
	if !sessionMatches || session == nil {
		return true
	}
	session.mu.Lock()
	session.tunnelState = tunnelDisconnected
	session.imsState = imsIdle
	session.lastError = ""
	session.mu.Unlock()
	session.cleanupResources()
	if err := waitForVoWiFiSessionExit(session); err != nil {
		// Cancellation has already been issued and the session is no longer
		// externally reachable. Finish any late cleanup in the background; the
		// stop operation itself remains successful and idempotent.
		log.Printf("[Reader VoWiFi] Session teardown continues in background: %v", err)
		go awaitRetiringVoWiFiSession(session)
	} else {
		clearRetiringVoWiFiSession(session)
	}
	log.Printf("[Reader VoWiFi] Session stopped")
	return true
}

func awaitRetiringVoWiFiSession(session *vowifiSession) {
	if session == nil {
		return
	}
	<-session.runDone
	session.mu.Lock()
	swuStarted := session.swuSession != nil
	session.mu.Unlock()
	if swuStarted {
		<-session.swuDone
	}
	session.cleanupResources()
	clearRetiringVoWiFiSession(session)
}

func clearRetiringVoWiFiSession(session *vowifiSession) {
	vowifiSessionMu.Lock()
	if retiringSession == session {
		retiringSession = nil
	}
	vowifiSessionMu.Unlock()
}

func waitForVoWiFiSessionExit(session *vowifiSession) error {
	if session == nil {
		return nil
	}
	timer := time.NewTimer(6 * time.Second)
	defer timer.Stop()
	select {
	case <-session.runDone:
	case <-timer.C:
		return fmt.Errorf("等待 VoWiFi 会话启动退出超时")
	}

	session.mu.Lock()
	swuStarted := session.swuSession != nil
	session.mu.Unlock()
	if !swuStarted {
		return nil
	}

	timer.Reset(6 * time.Second)
	select {
	case <-session.swuDone:
	case <-timer.C:
		return fmt.Errorf("等待 SWu 传输退出超时")
	}
	return nil
}

func getVoWiFiStatus() *readerVoWiFiStatus {
	vowifiSessionMu.Lock()
	session := activeSession
	vowifiSessionMu.Unlock()
	if session != nil && session.sourceKind == "module" {
		return voWiFiStatusForSession(nil)
	}
	return voWiFiStatusForSession(session)
}

func voWiFiStatusForSession(session *vowifiSession) *readerVoWiFiStatus {
	status := &readerVoWiFiStatus{
		TunnelState:          tunnelDisconnected,
		IMSState:             imsIdle,
		IncomingReachability: "unverified",
	}
	if session == nil {
		return status
	}

	session.mu.Lock()
	defer session.mu.Unlock()
	status.ReaderID = string(session.sessionKey.ReaderID)
	status.SIMID = session.sessionKey.SIMID
	status.ReaderGeneration = session.readerGeneration
	status.Available = true
	if session.simInfo != nil {
		status.MCC = session.simInfo.MCC
		status.MNC = session.simInfo.MNC
		status.IMSI = session.simInfo.IMSI
	}
	status.TunnelState = session.tunnelState
	status.IMSState = session.imsState
	status.Message = session.lastError
	status.ProgressStep = session.progressStep
	status.ProgressDesc = session.progressDesc
	if session.tunnelResult != nil {
		status.LocalIPv4 = session.tunnelResult.InnerIPv4
		status.LocalIPv6 = session.tunnelResult.InnerIPv6
		status.PCSCF = append([]string(nil), session.tunnelResult.PCSCF...)
	}
	if session.imsResult != nil && session.imsResult.Registered {
		status.RegisteredMSISDN = session.imsResult.MSISDN
		status.IMSTransport = session.imsResult.Transport
		status.IMSSecurityMode = session.imsResult.SecurityMode
		serviceReady := session.tunnelState == tunnelConnected && session.imsState == imsRegistered && session.imsService != nil && session.imsService.MessagingReady()
		status.SMSReady = serviceReady && session.simInfo != nil && strings.TrimSpace(session.simInfo.SMSC) != "" && strings.TrimSpace(session.simInfo.SMSCTarget) != ""
		status.CallReady = serviceReady && session.imsService.CallReady()
	}
	if service := session.imsService; service != nil {
		applyReaderIncomingPortSDiagnostics(status, service.Status())
	}
	if status.SMSReady {
		status.SMSState = "ready"
	} else {
		status.SMSState = "unavailable"
	}
	if status.CallReady {
		status.CallState = "ready"
	} else {
		status.CallState = "unavailable"
	}
	if session.carrier != nil {
		status.ProfileID = session.carrier.ID
		status.ProfileName = session.carrier.Name
		status.ProfileSource = session.carrier.Source
		status.EPDGFQDN = session.tunnelCfg.EPDGFQDN
		status.EPDGSource = session.tunnelCfg.EPDGSource
		status.IKEEncryption = session.carrier.IKEEncryption
		status.ESPEncryption = session.carrier.ChildEncryption
	}
	if session.effective != nil {
		status.OverrideMatched = session.effective.OverrideMatched
		status.OverrideID = session.effective.OverrideID
		status.IMEI = session.effective.DeviceIdentityIMEI
		status.EffectiveAPN = session.effective.APN
		status.IPStack = session.effective.IPStack
		status.AlgorithmPolicy = session.effective.AlgorithmPolicy
		status.AKAChallengeMode = session.effective.AKAChallengeMode
		status.DeviceModel = session.effective.DeviceModel
		if status.IMSTransport == "" {
			status.IMSTransport = session.effective.IMSTransport
		}
		status.IMSIdentitySource = session.effective.IMSIdentitySource
		status.E911Enabled = session.effective.E911Enabled
		if session.effective.E911Enabled {
			status.E911Status = "unsupported"
		}
	}
	return status
}

// applyReaderIncomingPortSDiagnostics preserves the imscore status boundary:
// only the narrow, non-subscriber Port-S observations are forwarded to the
// reader API. This defensive allowlist prevents a future runtime error string
// or noncanonical timestamp from becoming an accidental identity disclosure.
func applyReaderIncomingPortSDiagnostics(status *readerVoWiFiStatus, imsStatus map[string]interface{}) {
	if status == nil || imsStatus == nil {
		return
	}
	if value, ok := imsStatus["incoming_listener_started"].(bool); ok {
		status.IncomingListenerStarted = value
	}
	if value, ok := imsStatus["incoming_accept_count"].(uint64); ok {
		status.IncomingAcceptCount = value
	}
	if value, ok := imsStatus["incoming_last_sip_at"].(string); ok {
		if at, err := time.Parse(time.RFC3339Nano, value); err == nil {
			status.IncomingLastSIPAt = at.UTC().Format(time.RFC3339Nano)
		}
	}
	if value, ok := imsStatus["incoming_last_response_write_error"].(string); ok {
		switch value {
		case "", "timeout", "connection_closed", "write_failed":
			status.IncomingLastResponseWriteError = value
		}
	}
	if value, ok := imsStatus["incoming_reachability"].(string); ok {
		switch value {
		case "unverified", "observed":
			status.IncomingReachability = value
		}
	}
}

func getAllVoWiFiStatuses() []*readerVoWiFiStatus {
	keys := readerVoWiFiSessions.Keys()
	statuses := make([]*readerVoWiFiStatus, 0, len(keys))
	for _, key := range keys {
		if session, ok := readerVoWiFiSessions.Get(key); ok {
			statuses = append(statuses, voWiFiStatusForSession(session))
		}
	}
	return statuses
}
