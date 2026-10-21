package main

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	swucore "github.com/1239t/swu-go/pkg/swu"
	"proletarius.local/core/internal/readerapp"
	"proletarius.local/core/internal/vowifi"
)

func TestVoWiFiStatusForSessionCarriesInstanceIdentity(t *testing.T) {
	session := &vowifiSession{
		readerGeneration: 7,
		sessionKey:       readerapp.SessionKey{ReaderID: "reader-1", SIMID: "iccid-1"},
		simInfo: &vowifi.SIMInfo{
			IMSI: "234330123456789",
			MCC:  "234",
			MNC:  "33",
		},
		tunnelState: tunnelConnected,
		imsState:    imsRegistered,
	}
	status := voWiFiStatusForSession(session)
	if !status.Available || status.ReaderID != "reader-1" || status.SIMID != "iccid-1" || status.ReaderGeneration != 7 {
		t.Fatalf("instance identity = available:%t reader:%q sim:%q generation:%d", status.Available, status.ReaderID, status.SIMID, status.ReaderGeneration)
	}
	if status.IMSI != "234330123456789" || status.MCC != "234" || status.MNC != "33" {
		t.Fatalf("SIM identity = IMSI:%q MCC:%q MNC:%q", status.IMSI, status.MCC, status.MNC)
	}
	if status.IncomingReachability != "unverified" {
		t.Fatalf("default incoming reachability=%q, want unverified", status.IncomingReachability)
	}
}

func TestReaderVoWiFiStatusForwardsOnlySanitizedIncomingDiagnostics(t *testing.T) {
	observedAt := time.Date(2026, time.September, 25, 14, 15, 16, 123456789, time.UTC)
	status := &readerVoWiFiStatus{IncomingReachability: "unverified"}
	applyReaderIncomingPortSDiagnostics(status, map[string]interface{}{
		"incoming_listener_started":          true,
		"incoming_accept_count":              uint64(2),
		"incoming_last_sip_at":               observedAt.Format(time.RFC3339Nano),
		"incoming_last_response_write_error": "timeout",
		"incoming_reachability":              "observed",
	})
	if !status.IncomingListenerStarted || status.IncomingAcceptCount != 2 || status.IncomingReachability != "observed" {
		t.Fatalf("incoming diagnostics were not forwarded: %+v", status)
	}
	if status.IncomingLastSIPAt != observedAt.Format(time.RFC3339Nano) || status.IncomingLastResponseWriteError != "timeout" {
		t.Fatalf("incoming diagnostics contents=%+v", status)
	}

	blocked := &readerVoWiFiStatus{IncomingReachability: "unverified"}
	applyReaderIncomingPortSDiagnostics(blocked, map[string]interface{}{
		"incoming_last_sip_at":               "sip:+447700900123",
		"incoming_last_response_write_error": "write to sip:+447700900123 failed",
		"incoming_reachability":              "carrier-confirmed",
	})
	if blocked.IncomingLastSIPAt != "" || blocked.IncomingLastResponseWriteError != "" || blocked.IncomingReachability != "unverified" {
		t.Fatalf("unsanitized diagnostics escaped the imscore boundary: %+v", blocked)
	}
}

func TestReaderVoWiFiStatusRejectsStaleReaderOrSIMGeneration(t *testing.T) {
	reader := readerStatus{Connected: true, ReaderName: "reader-1", Generation: 7}
	info := &usimInfo{ICCID: "8985200014631959072", IMSI: "234330123456789"}
	current := &readerVoWiFiStatus{
		ReaderID:         "reader-1",
		SIMID:            "8985200014631959072",
		ReaderGeneration: 7,
		TunnelState:      tunnelConnected,
		IMSState:         imsRegistered,
		SMSReady:         true,
		CallReady:        true,
	}
	if !readerVoWiFiStatusMatchesCurrentIdentity(current, reader, info) {
		t.Fatal("current reader/SIM session was rejected")
	}

	for _, mismatch := range []string{"reader", "sim", "generation", "missing"} {
		t.Run(mismatch, func(t *testing.T) {
			status := *current
			switch mismatch {
			case "reader":
				status.ReaderID = "reader-2"
			case "sim":
				status.SIMID = "8985200014631959073"
			case "generation":
				status.ReaderGeneration = 6
			case "missing":
				status.ReaderGeneration = 0
			}
			if readerVoWiFiStatusMatchesCurrentIdentity(&status, reader, info) {
				t.Fatalf("stale %s session was accepted", mismatch)
			}
		})
	}
}

func TestReaderVoWiFiStatusFailsClosedWithoutCurrentReaderSnapshot(t *testing.T) {
	status := &readerVoWiFiStatus{
		ReaderID:         "reader-1",
		SIMID:            "8985200014631959072",
		ReaderGeneration: 7,
		TunnelState:      tunnelConnected,
		IMSState:         imsRegistered,
	}
	if readerVoWiFiStatusMatchesCurrentIdentity(status, readerStatus{}, nil) {
		t.Fatal("live VoWiFi state was accepted without a current reader/SIM snapshot")
	}
	stale := staleReaderVoWiFiStatus(readerStatus{}, nil)
	if stale.Available || stale.TunnelState != tunnelDisconnected || stale.IMSState != imsIdle || stale.SMSReady || stale.CallReady {
		t.Fatalf("stale status remains usable: %+v", stale)
	}
}

func TestReaderVoWiFiStatusEndpointDoesNotPublishPreviousGeneration(t *testing.T) {
	application := &app{readerMgr: &readerManager{
		readerName: "reader-1",
		generation: 8,
		cachedUSIM: &usimInfo{ICCID: "8985200014631959072", IMSI: "234330123456789"},
	}}
	staleSession := &vowifiSession{
		readerGeneration: 7,
		sessionKey:       readerapp.SessionKey{ReaderID: "reader-1", SIMID: "8985200014631959072"},
		simInfo:          &vowifi.SIMInfo{IMSI: "234330123456789", MCC: "234", MNC: "33"},
		tunnelState:      tunnelConnected,
		imsState:         imsRegistered,
	}
	vowifiSessionMu.Lock()
	previous := activeSession
	activeSession = staleSession
	vowifiSessionMu.Unlock()
	t.Cleanup(func() {
		vowifiSessionMu.Lock()
		activeSession = previous
		vowifiSessionMu.Unlock()
	})

	recorder := httptest.NewRecorder()
	application.getReaderVoWiFi(recorder, httptest.NewRequest(http.MethodGet, "/api/reader/vowifi", nil))
	if recorder.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	var status readerVoWiFiStatus
	if err := json.NewDecoder(recorder.Body).Decode(&status); err != nil {
		t.Fatal(err)
	}
	if status.Available || status.TunnelState != tunnelDisconnected || status.IMSState != imsIdle {
		t.Fatalf("stale session was published as usable: %+v", status)
	}
	if status.ReaderGeneration != 8 || status.ReaderID != "reader-1" || status.SIMID != "8985200014631959072" {
		t.Fatalf("current identity was not returned with stale state: %+v", status)
	}
}

func TestReaderCallStatusRejectsStaleGeneration(t *testing.T) {
	application := &app{readerMgr: &readerManager{
		readerName: "reader-1",
		generation: 8,
		cachedUSIM: &usimInfo{ICCID: "8985200014631959072", IMSI: "234330123456789"},
	}}
	staleSession := &vowifiSession{
		readerGeneration: 7,
		sessionKey:       readerapp.SessionKey{ReaderID: "reader-1", SIMID: "8985200014631959072"},
	}
	reader, info := application.readerMgr.statusAndUSIMInfo()
	if readerCallSessionMatchesCurrentIdentity(staleSession, reader, info) {
		t.Fatal("stale call session generation was accepted")
	}

	vowifiSessionMu.Lock()
	previous := activeSession
	activeSession = staleSession
	vowifiSessionMu.Unlock()
	t.Cleanup(func() {
		vowifiSessionMu.Lock()
		activeSession = previous
		vowifiSessionMu.Unlock()
	})

	recorder := httptest.NewRecorder()
	application.readerCallStatus(recorder, httptest.NewRequest(http.MethodGet, "/api/reader/vowifi/calls/status", nil))
	if recorder.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	var response readerCallResponse
	if err := json.NewDecoder(recorder.Body).Decode(&response); err != nil {
		t.Fatal(err)
	}
	if response.Generation != 0 || response.Call.State != "unavailable" {
		t.Fatalf("stale call state was exposed: %+v", response)
	}
}

func TestReaderCallActionResolverRejectsStaleGeneration(t *testing.T) {
	application := &app{readerMgr: &readerManager{
		readerName: "reader-1",
		generation: 8,
		cachedUSIM: &usimInfo{ICCID: "8985200014631959072", IMSI: "234330123456789"},
	}}
	vowifiSessionMu.Lock()
	previous := activeSession
	activeSession = &vowifiSession{
		readerGeneration: 7,
		sessionKey:       readerapp.SessionKey{ReaderID: "reader-1", SIMID: "8985200014631959072"},
	}
	vowifiSessionMu.Unlock()
	t.Cleanup(func() {
		vowifiSessionMu.Lock()
		activeSession = previous
		vowifiSessionMu.Unlock()
	})

	_, generation, _, err := application.activeReaderCallService(context.Background(), 7, true)
	if err == nil || !strings.Contains(err.Error(), "eSIM Profile 已变更") {
		t.Fatalf("stale action resolver error = %v", err)
	}
	if generation != 7 {
		t.Fatalf("resolver generation = %d, want 7", generation)
	}

	recorder := httptest.NewRecorder()
	application.readerCallDial(recorder, httptest.NewRequest(
		http.MethodPost,
		"/api/reader/vowifi/calls/dial",
		strings.NewReader(`{"peer":"888","generation":7}`),
	))
	if recorder.Code != http.StatusConflict {
		t.Fatalf("stale action status=%d body=%s", recorder.Code, recorder.Body.String())
	}
}

func TestReaderSnapshotCopiesCachedSIMAndGeneration(t *testing.T) {
	rm := &readerManager{
		readerName: "reader-1",
		generation: 7,
		cachedUSIM: &usimInfo{
			IMSI: "234330123456789",
		},
	}
	info, readerName, generation, err := rm.readerSnapshot()
	if err != nil {
		t.Fatal(err)
	}
	if readerName != "reader-1" || generation != 7 || info.IMSI != "234330123456789" {
		t.Fatalf("snapshot = reader %q, generation %d, IMSI %q", readerName, generation, info.IMSI)
	}
	info.IMSI = "changed"
	if rm.cachedUSIM.IMSI != "234330123456789" {
		t.Fatal("readerSnapshot returned the cached SIM by reference")
	}
}

func TestReaderIMSUserAgentUsesGiffgaffCompatibilityValue(t *testing.T) {
	if got := readerIMSUserAgent("apple_o2_giffgaff_uk", "iphone18,1"); got != "iOS/18.6.2 iPhone" {
		t.Fatalf("giffgaff User-Agent = %q", got)
	}
	if got := readerIMSUserAgent("apple_orange_uk", "iphone18,1"); got != "iOS/26.6 iphone18,1" {
		t.Fatalf("generic User-Agent = %q", got)
	}
}

func TestReaderIMSSIPInstanceKeepsStableReaderIdentity(t *testing.T) {
	const want = "urn:gsma:imei:353936932685355-0"
	for attempt := 0; attempt < 2; attempt++ {
		if got := readerIMSSIPInstance("353936932685355"); got != want {
			t.Fatalf("legacy SIP instance = %q, want %q", got, want)
		}
	}
}

func TestReaderIMSSIPInstanceRejectsInvalidIMEI(t *testing.T) {
	for _, invalid := range []string{
		"", " \t\n", "123", "35123456789012", "3512345678901245",
		"35123456789012x", "35123456 890124", "+51234567890124",
		"３５１２３４５６７８９０１２４",
	} {
		if got := readerIMSSIPInstance(invalid); got != "" {
			t.Fatalf("invalid IMEI %q produced %q", invalid, got)
		}
	}
}

func TestReaderIMSSIPInstanceTrimsSurroundingWhitespace(t *testing.T) {
	const want = "urn:gsma:imei:351234567890124-0"
	if got := readerIMSSIPInstance(" \t351234567890124\n "); got != want {
		t.Fatalf("whitespace-wrapped IMEI produced %q, want %q", got, want)
	}
}

func TestReaderIKEStartupErrorDescribesProxyAndDirectOptions(t *testing.T) {
	for _, mode := range []string{"enhanced-mode-utun", "enhanced-mode-fake-ip", "socks5-udp-domain", "unknown"} {
		err := readerIKEStartupError(mode, context.DeadlineExceeded)
		message := err.Error()
		if !strings.Contains(message, "ePDG") || !strings.Contains(message, "UDP 500/4500") {
			t.Fatalf("mode %q returned incomplete guidance: %q", mode, message)
		}
		if !strings.Contains(message, "清空代理") && mode != "unknown" {
			t.Fatalf("mode %q did not state direct fallback option: %q", mode, message)
		}
		if strings.Contains(message, "Surge") {
			t.Fatalf("mode %q incorrectly names a specific proxy application: %q", mode, message)
		}
	}
}

func TestDisconnectInvalidatesGenerationAndCache(t *testing.T) {
	rm := &readerManager{
		readerName: "reader-1",
		generation: 3,
		cachedUSIM: &usimInfo{
			IMSI: "234330123456789",
		},
	}
	if err := rm.Disconnect(); err != nil {
		t.Fatal(err)
	}
	if rm.generation != 4 {
		t.Fatalf("generation = %d, want 4", rm.generation)
	}
	if rm.USIMInfo() != nil {
		t.Fatal("cached SIM was not cleared")
	}
}

func TestStopVoWiFiWaitsForSessionExit(t *testing.T) {
	vowifiSessionMu.Lock()
	previous := activeSession
	previousRetiring := retiringSession
	ctx, cancel := context.WithCancel(context.Background())
	session := &vowifiSession{
		cancel:  cancel,
		runDone: make(chan struct{}),
		swuDone: make(chan struct{}),
	}
	activeSession = session
	vowifiSessionMu.Unlock()
	t.Cleanup(func() {
		vowifiSessionMu.Lock()
		activeSession = previous
		retiringSession = previousRetiring
		vowifiSessionMu.Unlock()
	})

	go func() {
		<-ctx.Done()
		time.Sleep(20 * time.Millisecond)
		close(session.runDone)
		close(session.swuDone)
	}()
	started := time.Now()
	stopVoWiFi()
	if elapsed := time.Since(started); elapsed < 20*time.Millisecond {
		t.Fatalf("stop returned before session exit: %s", elapsed)
	}
	if activeSession != nil {
		t.Fatal("active session was not cleared")
	}
	if retiringSession != nil {
		t.Fatal("completed session remained in retiring state")
	}
}

func TestStopVoWiFiCancelsPendingStartupBeforeItCanPublish(t *testing.T) {
	vowifiSessionMu.Lock()
	previousSession := activeSession
	previousRetiring := retiringSession
	previousStartup := startingVoWiFi
	previousIntent := vowifiIntentID
	activeSession = nil
	retiringSession = nil
	startingVoWiFi = nil
	vowifiIntentID = 100
	vowifiSessionMu.Unlock()
	t.Cleanup(func() {
		vowifiSessionMu.Lock()
		activeSession = previousSession
		retiringSession = previousRetiring
		startingVoWiFi = previousStartup
		vowifiIntentID = previousIntent
		vowifiSessionMu.Unlock()
	})

	previousPrepare := prepareVoWiFiSession
	started := make(chan struct{})
	prepareVoWiFiSession = func(ctx context.Context, _ *app, _ string, _ string, _ int, _ *vowifiStartup, _ int) (*vowifiSession, error) {
		close(started)
		<-ctx.Done()
		// Deliberately return a publishable session after cancellation. The
		// reservation check, not cooperative preparation, must prevent it from
		// becoming active after Stop has returned.
		return &vowifiSession{
			sessionKey: readerapp.SessionKey{ReaderID: "reader-1", SIMID: "iccid-1"},
			runDone:    make(chan struct{}),
			swuDone:    make(chan struct{}),
		}, nil
	}
	t.Cleanup(func() { prepareVoWiFiSession = previousPrepare })

	result := make(chan error, 1)
	go func() {
		result <- startVoWiFi(&app{readerMgr: newReaderManager()}, "", "epdg.example", 500)
	}()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("startup did not enter mocked slow preparation")
	}

	// If startup still held the session mutex while calling its slow Rust/CLI
	// phase, both this lock acquisition and Stop would block.
	lockAcquired := make(chan struct{})
	go func() {
		vowifiSessionMu.Lock()
		close(lockAcquired)
		vowifiSessionMu.Unlock()
	}()
	select {
	case <-lockAcquired:
	case <-time.After(time.Second):
		t.Fatal("startup held vowifiSessionMu during slow preparation")
	}

	stopVoWiFi()
	select {
	case err := <-result:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("start result=%v, want cancellation", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Stop did not promptly cancel pending startup")
	}

	vowifiSessionMu.Lock()
	defer vowifiSessionMu.Unlock()
	if activeSession != nil || startingVoWiFi != nil {
		t.Fatalf("Stop allowed a pending startup to publish: active=%p starting=%p", activeSession, startingVoWiFi)
	}
}

func TestStopVoWiFiForReaderGenerationCancelsOnlyMatchingPendingStartup(t *testing.T) {
	vowifiSessionMu.Lock()
	previousSession := activeSession
	previousRetiring := retiringSession
	previousStartup := startingVoWiFi
	previousIntent := vowifiIntentID
	startup := newVoWiFiStartup(73)
	startup.readerGeneration = 7
	activeSession = nil
	retiringSession = nil
	startingVoWiFi = startup
	vowifiIntentID = 73
	vowifiSessionMu.Unlock()
	t.Cleanup(func() {
		vowifiSessionMu.Lock()
		activeSession = previousSession
		retiringSession = previousRetiring
		startingVoWiFi = previousStartup
		vowifiIntentID = previousIntent
		vowifiSessionMu.Unlock()
	})

	stopVoWiFiForReaderGeneration(8)
	select {
	case <-startup.ctx.Done():
		t.Fatal("generation-mismatched stop cancelled pending startup")
	default:
	}

	stopVoWiFiForReaderGeneration(7)
	select {
	case <-startup.ctx.Done():
	case <-time.After(time.Second):
		t.Fatal("generation-matched stop did not cancel pending startup")
	}
}

func TestStopVoWiFiForReaderGenerationDoesNotStopNewerSession(t *testing.T) {
	vowifiSessionMu.Lock()
	previous := activeSession
	previousRetiring := retiringSession
	previousIntent := vowifiIntentID
	newer := &vowifiSession{readerGeneration: 8}
	activeSession = newer
	retiringSession = nil
	vowifiIntentID = 41
	vowifiSessionMu.Unlock()
	t.Cleanup(func() {
		vowifiSessionMu.Lock()
		activeSession = previous
		retiringSession = previousRetiring
		vowifiIntentID = previousIntent
		vowifiSessionMu.Unlock()
	})

	stopVoWiFiForReaderGeneration(7)
	vowifiSessionMu.Lock()
	defer vowifiSessionMu.Unlock()
	if activeSession != newer {
		t.Fatal("generation-scoped stop cleared a newer session")
	}
	if vowifiIntentID != 41 {
		t.Fatalf("generation-scoped mismatch changed intent from 41 to %d", vowifiIntentID)
	}
}

func TestDisableVoWiFiDoesNotRequireUSIM(t *testing.T) {
	vowifiSessionMu.Lock()
	previous := activeSession
	previousRetiring := retiringSession
	activeSession = nil
	retiringSession = nil
	vowifiSessionMu.Unlock()
	t.Cleanup(func() {
		vowifiSessionMu.Lock()
		activeSession = previous
		retiringSession = previousRetiring
		vowifiSessionMu.Unlock()
	})

	readerVoWiFiMu.Lock()
	previousOperation := readerVoWiFiOperationActive
	readerVoWiFiOperationActive = true
	readerVoWiFiMu.Unlock()
	t.Cleanup(func() {
		readerVoWiFiMu.Lock()
		readerVoWiFiOperationActive = previousOperation
		readerVoWiFiMu.Unlock()
	})

	application := &app{readerMgr: newReaderManager()}
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/api/reader/vowifi", strings.NewReader(`{"enable":false}`))
	application.setReaderVoWiFi(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("disable without SIM status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	readerVoWiFiMu.Lock()
	operationStillActive := readerVoWiFiOperationActive
	readerVoWiFiMu.Unlock()
	if !operationStillActive {
		t.Fatal("disable modified another in-flight reader operation lease")
	}
}

func TestRetiringSessionBlocksNewSession(t *testing.T) {
	vowifiSessionMu.Lock()
	previous := activeSession
	previousRetiring := retiringSession
	activeSession = nil
	retiringSession = &vowifiSession{}
	vowifiSessionMu.Unlock()
	t.Cleanup(func() {
		vowifiSessionMu.Lock()
		activeSession = previous
		retiringSession = previousRetiring
		vowifiSessionMu.Unlock()
	})

	err := startVoWiFi(&app{readerMgr: newReaderManager()}, "127.0.0.1:1080", "epdg.example", 500)
	if err == nil || !strings.Contains(err.Error(), "正在释放") {
		t.Fatalf("start with retiring session error=%v", err)
	}
}

func TestWaitForVoWiFiSessionExitSkipsUnstartedSWU(t *testing.T) {
	session := &vowifiSession{
		runDone: make(chan struct{}),
		swuDone: make(chan struct{}),
	}
	close(session.runDone)
	started := time.Now()
	if err := waitForVoWiFiSessionExit(session); err != nil {
		t.Fatal(err)
	}
	if elapsed := time.Since(started); elapsed > 100*time.Millisecond {
		t.Fatalf("waited for unstarted SWu session: %s", elapsed)
	}
}

func TestVoWiFiReconnectStopsWhenCardOrReaderIsGone(t *testing.T) {
	for _, terminal := range []error{errReaderCardAbsent, errReaderUnavailable, errReaderISIMUnavailable} {
		if shouldAutoReconnectVoWiFi(fmt.Errorf("startup: %w", terminal)) {
			t.Fatalf("terminal reader error retried: %v", terminal)
		}
	}
	for _, transient := range []error{context.DeadlineExceeded, errReaderBusy, errors.New("network reset")} {
		if !shouldAutoReconnectVoWiFi(fmt.Errorf("startup: %w", transient)) {
			t.Fatalf("transient error not retried: %v", transient)
		}
	}
}

func TestReaderIKEPayloadPreservesFakeIPTransportFraming(t *testing.T) {
	ike := make([]byte, 28)
	ike[17] = 0x20
	ike[18] = 34
	binary.BigEndian.PutUint32(ike[24:28], uint32(len(ike)))

	marked := append([]byte{0, 0, 0, 0}, ike...)
	got, ok := readerIKEPayload(marked)
	if !ok || len(got) != len(ike) {
		t.Fatalf("marked IKE classification = %t len=%d", ok, len(got))
	}
	if _, ok := readerIKEPayload([]byte{0x12, 0x34, 0x56, 0x78, 1, 2, 3, 4}); ok {
		t.Fatal("ESP-shaped packet classified as IKE")
	}
}

func TestSelectIMSPathKeepsAddressFamily(t *testing.T) {
	snapshot := swucore.SessionSnapshot{
		IPv4:    net.ParseIP("10.0.0.2"),
		IPv6:    net.ParseIP("2001:db8::2"),
		PCSCFv4: []net.IP{net.ParseIP("10.0.0.10")},
		PCSCFv6: []net.IP{net.ParseIP("2001:db8::10")},
	}
	local, candidates, err := selectIMSPath(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	if !local.Equal(snapshot.IPv6) {
		t.Fatalf("local IP = %s, want IPv6 %s", local, snapshot.IPv6)
	}
	if len(candidates) != 1 || candidates[0] != "[2001:db8::10]:5060" {
		t.Fatalf("P-CSCF candidates = %v", candidates)
	}
}

func TestSelectIMSPathRejectsMissingPCSCF(t *testing.T) {
	_, _, err := selectIMSPath(swucore.SessionSnapshot{IPv4: net.ParseIP("10.0.0.2")})
	if err == nil {
		t.Fatal("missing P-CSCF unexpectedly accepted")
	}
}

func TestPrioritizeRegistrarUsesActualWinningCandidate(t *testing.T) {
	got := prioritizeRegistrar([]string{"[2001:db8::1]:5060", "[2001:db8::2]:5060"}, " [2001:db8::2]:5060 ")
	want := []string{"[2001:db8::2]:5060", "[2001:db8::1]:5060"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("registrars = %v, want %v", got, want)
	}
}

func TestEAPAKAAdapterWithoutSessionCapabilityKeepsGoPath(t *testing.T) {
	adapter := &swuSIMAdapter{}
	result, handled, err := adapter.RespondEAPAKA(nil, "", "minimal")
	if err != nil || handled || result.Packet != nil || result.MSK != nil || result.KAut != nil {
		t.Fatalf("missing EAP-AKA session capability changed the Go path: %#v handled=%v err=%v", result, handled, err)
	}
	identity, identityHandled, identityErr := adapter.RespondEAPIdentity(nil, nil)
	if identityErr != nil || identityHandled || identity.Packet != nil {
		t.Fatalf("missing EAP Identity session capability changed the Go path: %#v handled=%v err=%v", identity, identityHandled, identityErr)
	}
}
