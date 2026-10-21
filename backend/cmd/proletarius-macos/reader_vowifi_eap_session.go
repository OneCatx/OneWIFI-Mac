package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// readerEAPAKAOwner is intentionally selected once, when a reader VoWiFi
// session is created.  It is not a process-wide feature flag: a USB session
// with a complete card identity can use the bounded Rust standard EAP-AKA
// implementation while BLE and incomplete/legacy sessions retain Go's
// established path.
type readerEAPAKAOwner uint8

const (
	readerEAPAKAOwnerGo readerEAPAKAOwner = iota
	readerEAPAKAOwnerRustStandard
)

// readerEAPAKASessionCapability freezes the only identity which may be used
// by the Rust standard EAP-AKA subpath.  In particular, it deliberately does
// not look up the current reader or current socket again after session start:
// an eSIM profile switch, reconnect, or environment reload must not redirect
// an in-flight EAP exchange to another card or sidecar.
//
// This is limited to ordinary EAP-AKA.  EAP-AKA', Fast Re-authentication, and
// AT_NEXT_REAUTH_ID continue through the mature Go implementation.
type readerEAPAKASessionCapability struct {
	owner      readerEAPAKAOwner
	reader     string
	generation uint64
	identity   nativeReaderAuthIdentity
	socket     string
}

func (capability readerEAPAKASessionCapability) usesRustStandardEAP() bool {
	return capability.owner == readerEAPAKAOwnerRustStandard
}

// selectReaderEAPAKASessionCapability is intentionally side-effect free: a
// sidecar can restart between session construction and its first EAP request.
// The explicit handoff permit is also read exactly once here.  Merely having
// the Rust gateway socket is not evidence that an untested physical reader
// path is ready, because the gateway always provides that socket to the Go
// compatibility backend.  If a permitted session later loses its sidecar, it
// fails closed instead of falling through to a differently owned Go
// transaction.
func selectReaderEAPAKASessionCapability(
	reader string,
	generation uint64,
	info *usimInfo,
) readerEAPAKASessionCapability {
	reader = strings.TrimSpace(reader)
	identity, identityOK := nativeReaderEAPSessionIdentity(info)
	socket := strings.TrimSpace(os.Getenv("PROLETARIUS_CARRIER_SOCKET"))
	if !rustStandardEAPAKAHandoffPermitted() || reader == "" || generation == 0 || isBLEReader(reader) || !identityOK ||
		!validNativeCarrierSocketPath(socket) {
		return readerEAPAKASessionCapability{owner: readerEAPAKAOwnerGo}
	}
	return readerEAPAKASessionCapability{
		owner:      readerEAPAKAOwnerRustStandard,
		reader:     reader,
		generation: generation,
		identity:   identity,
		socket:     socket,
	}
}

// rustStandardEAPAKAHandoffPermitted is a staged migration permit, not a
// mutable request-time feature switch.  It is captured only while a new
// session capability is created, so a running session cannot change owner if
// its environment is later modified.  Keep the production Go EAP transcript
// owner by default until the USB reader path has passed the real carrier
// acceptance gates.  This does not change the pre-existing optional Rust
// PC/SC authentication helper used underneath Go's established path.
func rustStandardEAPAKAHandoffPermitted() bool {
	return strings.TrimSpace(os.Getenv("PROLETARIUS_RUST_EAP_AKA")) == "1"
}

func nativeReaderEAPSessionIdentity(info *usimInfo) (nativeReaderAuthIdentity, bool) {
	if info == nil {
		return nativeReaderAuthIdentity{}, false
	}
	identity := nativeReaderAuthIdentity{
		ICCID: normalizeSIMIdentifier(info.ICCID),
		IMSI:  normalizeSIMIdentifier(info.IMSI),
	}
	return identity, validNativeReaderEAPSessionIdentity(identity)
}

func validNativeReaderEAPSessionIdentity(identity nativeReaderAuthIdentity) bool {
	if !(18 <= len(identity.ICCID) && len(identity.ICCID) <= 22) ||
		!(5 <= len(identity.IMSI) && len(identity.IMSI) <= 15) {
		return false
	}
	for _, value := range identity.ICCID + identity.IMSI {
		if value < '0' || value > '9' {
			return false
		}
	}
	return true
}

// A local Unix socket path is configuration, not an EAP feature toggle.  Do
// not stat or dial it here: sidecar startup can race this snapshot.  Once it
// has selected Rust, all later errors for this exact path are terminal for the
// current standard EAP-AKA exchange.
func validNativeCarrierSocketPath(socket string) bool {
	return socket != "" && filepath.IsAbs(socket) && !strings.ContainsRune(socket, '\x00')
}

// validateReaderEAPAKASessionContext is called while authMu is held.  As with
// validateAuthenticationReader, it must never wait for a refresh that owns
// rm.mu -> authMu; TryLock prevents an AB-BA deadlock.  The cached ICCID/IMSI
// comparison closes the remaining same-reader/profile-switch window which a
// generation-only check cannot describe on its own.
func (rm *readerManager) validateReaderEAPAKASessionContext(capability readerEAPAKASessionCapability) error {
	if !capability.usesRustStandardEAP() || !validNativeReaderEAPSessionIdentity(capability.identity) {
		return errors.New("Rust EAP-AKA 会话上下文无效")
	}
	if !rm.mu.TryLock() {
		return errReaderBusy
	}
	defer rm.mu.Unlock()
	if strings.TrimSpace(rm.readerName) != capability.reader || rm.generation != capability.generation {
		return errors.New("读卡器连接已变更")
	}
	rm.usimMu.RLock()
	info := rm.cachedUSIM
	identity, identityOK := nativeReaderEAPSessionIdentity(info)
	rm.usimMu.RUnlock()
	if !identityOK || identity != capability.identity {
		return errors.New("SIM 卡身份已变更")
	}
	return nil
}

// respondNativeEAPAKAForSession owns one physical PC/SC standard EAP-AKA
// operation.  It only accepts a previously selected native session context;
// callers cannot cause a BLE reader or a changed card to enter this route.
func (rm *readerManager) respondNativeEAPAKAForSession(
	ctx context.Context,
	capability readerEAPAKASessionCapability,
	eapRequest []byte,
	exchangeID string,
	checkcodeMode string,
) (nativeEAPAKAResponse, error) {
	if !capability.usesRustStandardEAP() || !validNativeCarrierSocketPath(capability.socket) {
		return nativeEAPAKAResponse{}, errors.New("Rust EAP-AKA 会话能力不可用")
	}

	if err := lockReaderAuthContext(ctx, &rm.authMu); err != nil {
		return nativeEAPAKAResponse{}, err
	}
	defer rm.authMu.Unlock()
	if err := rm.validateReaderEAPAKASessionContext(capability); err != nil {
		return nativeEAPAKAResponse{}, err
	}

	requestCtx, cancel := context.WithTimeout(ctx, 40*time.Second)
	defer cancel()
	result, err := respondRuntimeReaderEAPAKAForIdentityAtSocket(
		requestCtx,
		capability.socket,
		capability.reader,
		"usim",
		exchangeID,
		eapRequest,
		checkcodeMode,
		&capability.identity,
	)
	if err != nil {
		return nativeEAPAKAResponse{}, err
	}
	if err := rm.validateReaderEAPAKASessionContext(capability); err != nil {
		releaseNativeEAPAKAContinuation(capability.socket, result)
		return nativeEAPAKAResponse{}, err
	}
	return result, nil
}

// lockReaderAuthContext waits for the process-local reader authentication
// mutex without making a stopped VoWiFi session wait behind an unrelated
// long-running APDU exchange.  sync.Mutex has no context-aware Lock method;
// bounded TryLock polling avoids a helper goroutine that could acquire the
// mutex after its caller has already abandoned the exchange.
func lockReaderAuthContext(ctx context.Context, mutex *sync.Mutex) error {
	if mutex == nil {
		return errors.New("读卡器鉴权锁不可用")
	}
	const pollInterval = 20 * time.Millisecond
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		if mutex.TryLock() {
			return nil
		}
		timer := time.NewTimer(pollInterval)
		select {
		case <-ctx.Done():
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			return ctx.Err()
		case <-timer.C:
		}
	}
}

func releaseNativeEAPAKAContinuation(socket string, result nativeEAPAKAResponse) {
	cleanupCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_ = releaseRuntimeEAPIdentityExchangeAtSocket(cleanupCtx, socket, result.NextExchangeID)
	clear(result.Packet)
	clear(result.MSK)
	clear(result.KAut)
}

func (rm *readerManager) authenticateEAPAKALegacyForNativeSession(
	capability readerEAPAKASessionCapability,
	rand, autn []byte,
) (*akaResult, error) {
	if err := rm.validateReaderEAPAKASessionContext(capability); err != nil {
		return nil, err
	}
	result, err := rm.authenticateEAPAKAAtGenerationLegacy(rand, autn, capability.generation)
	if contextErr := rm.validateReaderEAPAKASessionContext(capability); contextErr != nil {
		if result != nil {
			clear(result.RES)
			clear(result.CK)
			clear(result.IK)
			clear(result.AUTS)
		}
		return nil, contextErr
	}
	return result, err
}
