package main

import (
	"context"
	"errors"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

const fixtureNativeEAPICCID = "8901234567890123456"
const fixtureNativeEAPIMSI = "234100896392986"

func fixtureNativeEAPUSIM() *usimInfo {
	return &usimInfo{ICCID: fixtureNativeEAPICCID, IMSI: fixtureNativeEAPIMSI}
}

func enableRustStandardEAPAKAHandoffForTest(t *testing.T) {
	t.Helper()
	t.Setenv("PROLETARIUS_RUST_EAP_AKA", "1")
}

// macOS limits sockaddr_un.sun_path to 104 bytes.  Go's per-test TempDir
// hierarchy is often longer than that, so Unix-socket tests need a short
// explicit parent rather than t.TempDir().
func shortEAPSocketPath(t *testing.T, name string) string {
	t.Helper()
	directory, err := os.MkdirTemp("/tmp", "p-eap-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(directory) })
	return filepath.Join(directory, name)
}

func TestReaderEAPAKASessionCapabilityIsUSBAndIdentityBound(t *testing.T) {
	socket := shortEAPSocketPath(t, "proletariusd.sock")
	t.Setenv("PROLETARIUS_CARRIER_SOCKET", socket)
	info := fixtureNativeEAPUSIM()
	if got := selectReaderEAPAKASessionCapability("ESTKme-RED USB", 7, info); got.usesRustStandardEAP() {
		t.Fatal("native EAP-AKA was enabled without an explicit session handoff permit")
	}
	enableRustStandardEAPAKAHandoffForTest(t)
	capability := selectReaderEAPAKASessionCapability("ESTKme-RED USB", 7, info)
	if !capability.usesRustStandardEAP() {
		t.Fatal("USB reader with a complete identity and configured Unix socket did not select native EAP-AKA")
	}
	if capability.reader != "ESTKme-RED USB" || capability.generation != 7 ||
		capability.identity.ICCID != fixtureNativeEAPICCID || capability.identity.IMSI != fixtureNativeEAPIMSI ||
		capability.socket != socket {
		t.Fatalf("capability did not freeze its session context: %#v", capability)
	}
	info.ICCID = "8901234567890123457"
	if capability.identity.ICCID != fixtureNativeEAPICCID {
		t.Fatal("session capability retained a mutable USIM identity reference")
	}

	for _, test := range []struct {
		name       string
		reader     string
		generation uint64
		info       *usimInfo
		socket     string
	}{
		{name: "ble", reader: "BLE:ESTKme-RED:fixture", generation: 7, info: fixtureNativeEAPUSIM(), socket: socket},
		{name: "relative_socket", reader: "ESTKme-RED USB", generation: 7, info: fixtureNativeEAPUSIM(), socket: "proletariusd.sock"},
		{name: "missing_iccid", reader: "ESTKme-RED USB", generation: 7, info: &usimInfo{IMSI: fixtureNativeEAPIMSI}, socket: socket},
		{name: "missing_imsi", reader: "ESTKme-RED USB", generation: 7, info: &usimInfo{ICCID: fixtureNativeEAPICCID}, socket: socket},
		{name: "zero_generation", reader: "ESTKme-RED USB", generation: 0, info: fixtureNativeEAPUSIM(), socket: socket},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Setenv("PROLETARIUS_CARRIER_SOCKET", test.socket)
			if got := selectReaderEAPAKASessionCapability(test.reader, test.generation, test.info); got.usesRustStandardEAP() {
				t.Fatalf("ineligible session selected Rust EAP-AKA: %#v", got)
			}
		})
	}
}

func TestNativeEAPSessionFailureIsHandledWithoutFallback(t *testing.T) {
	socket := shortEAPSocketPath(t, "missing.sock")
	t.Setenv("PROLETARIUS_CARRIER_SOCKET", socket)
	enableRustStandardEAPAKAHandoffForTest(t)
	capability := selectReaderEAPAKASessionCapability("Fixture Reader", 7, fixtureNativeEAPUSIM())
	if !capability.usesRustStandardEAP() {
		t.Fatal("fixture did not select the native EAP session")
	}
	manager := &readerManager{
		readerName: "Fixture Reader",
		generation: 7,
		cachedUSIM: fixtureNativeEAPUSIM(),
	}
	adapter := &swuSIMAdapter{rm: manager, generation: 7, imsi: fixtureNativeEAPIMSI, eapCapability: capability}

	identity, identityHandled, identityErr := adapter.RespondEAPIdentity(
		[]byte{1, 41, 0, 5, 1},
		[]byte("nai@example"),
	)
	if identityErr == nil || !identityHandled || identity.Packet != nil {
		t.Fatalf("native Identity failure fell through: handled=%t result=%#v err=%v", identityHandled, identity, identityErr)
	}

	aka, akaHandled, akaErr := adapter.RespondEAPAKA(
		[]byte{1, 41, 0, 8, 23, 1, 0, 0},
		testNativeEAPExchangeID,
		"minimal",
	)
	if akaErr == nil || !akaHandled || aka.Packet != nil || aka.MSK != nil || aka.KAut != nil {
		t.Fatalf("native EAP-AKA failure fell through: handled=%t result=%#v err=%v", akaHandled, aka, akaErr)
	}
}

func TestNativeEAPSessionRejectsChangedFrozenSIMBeforeSocketIO(t *testing.T) {
	socket := shortEAPSocketPath(t, "missing.sock")
	t.Setenv("PROLETARIUS_CARRIER_SOCKET", socket)
	enableRustStandardEAPAKAHandoffForTest(t)
	capability := selectReaderEAPAKASessionCapability("Fixture Reader", 7, fixtureNativeEAPUSIM())
	manager := &readerManager{
		readerName: "Fixture Reader",
		generation: 7,
		cachedUSIM: &usimInfo{
			ICCID: fixtureNativeEAPICCID,
			IMSI:  "234100896392987",
		},
	}
	adapter := &swuSIMAdapter{rm: manager, generation: 7, imsi: fixtureNativeEAPIMSI, eapCapability: capability}
	_, handled, err := adapter.RespondEAPIdentity([]byte{1, 41, 0, 5, 1}, []byte("nai@example"))
	if !handled || err == nil || err.Error() != "SIM 卡身份已变更" {
		t.Fatalf("changed SIM was not rejected before native I/O: handled=%t err=%v", handled, err)
	}
}

func TestNativeEAPSessionCancellationStopsIdentityRequest(t *testing.T) {
	socket := shortEAPSocketPath(t, "proletariusd.sock")
	var calls atomic.Int32
	server := newEAPReleaseServer(t, socket, &calls)
	t.Cleanup(func() { _ = server.Close() })
	t.Setenv("PROLETARIUS_CARRIER_SOCKET", socket)
	enableRustStandardEAPAKAHandoffForTest(t)
	capability := selectReaderEAPAKASessionCapability("Fixture Reader", 7, fixtureNativeEAPUSIM())
	manager := &readerManager{readerName: "Fixture Reader", generation: 7, cachedUSIM: fixtureNativeEAPUSIM()}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	adapter := &swuSIMAdapter{
		rm:            manager,
		generation:    7,
		imsi:          fixtureNativeEAPIMSI,
		sessionCtx:    ctx,
		eapCapability: capability,
	}
	_, handled, err := adapter.RespondEAPIdentity([]byte{1, 41, 0, 5, 1}, []byte("nai@example"))
	if !handled || !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled native Identity request did not fail through its session context: handled=%t err=%v", handled, err)
	}
	if calls.Load() != 0 {
		t.Fatalf("canceled native Identity request reached the sidecar %d times", calls.Load())
	}
}

func TestNativeEAPSessionCancellationStopsAKARequest(t *testing.T) {
	socket := shortEAPSocketPath(t, "proletariusd.sock")
	var calls atomic.Int32
	server := newEAPReleaseServer(t, socket, &calls)
	t.Cleanup(func() { _ = server.Close() })
	t.Setenv("PROLETARIUS_CARRIER_SOCKET", socket)
	enableRustStandardEAPAKAHandoffForTest(t)
	capability := selectReaderEAPAKASessionCapability("Fixture Reader", 7, fixtureNativeEAPUSIM())
	manager := &readerManager{readerName: "Fixture Reader", generation: 7, cachedUSIM: fixtureNativeEAPUSIM()}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	adapter := &swuSIMAdapter{
		rm:            manager,
		generation:    7,
		imsi:          fixtureNativeEAPIMSI,
		sessionCtx:    ctx,
		eapCapability: capability,
	}
	_, handled, err := adapter.RespondEAPAKA(
		[]byte{1, 41, 0, 8, 23, 1, 0, 0},
		testNativeEAPExchangeID,
		"minimal",
	)
	if !handled || !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled native EAP-AKA request did not fail through its session context: handled=%t err=%v", handled, err)
	}
	if calls.Load() != 0 {
		t.Fatalf("canceled native EAP-AKA request reached the sidecar %d times", calls.Load())
	}
}

func TestReaderAuthLockRespectsSessionCancellation(t *testing.T) {
	var mutex sync.Mutex
	mutex.Lock()
	defer mutex.Unlock()

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- lockReaderAuthContext(ctx, &mutex) }()

	// Give the contender one poll interval to observe the held lock, then
	// cancel it. It must not remain blocked until the unrelated holder exits.
	time.Sleep(30 * time.Millisecond)
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("cancelled auth-lock wait returned %v, want context.Canceled", err)
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatal("cancelled auth-lock wait remained blocked")
	}
}

func TestNativeEAPSessionReleaseUsesFrozenSocketAfterEnvironmentChanges(t *testing.T) {
	directory, err := os.MkdirTemp("/tmp", "p-eap-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(directory) })
	socketA := filepath.Join(directory, "chosen.sock")
	socketB := filepath.Join(directory, "other.sock")
	var chosenCalls atomic.Int32
	var otherCalls atomic.Int32
	chosen := newEAPReleaseServer(t, socketA, &chosenCalls)
	other := newEAPReleaseServer(t, socketB, &otherCalls)
	t.Cleanup(func() {
		_ = chosen.Close()
		_ = other.Close()
	})

	t.Setenv("PROLETARIUS_CARRIER_SOCKET", socketA)
	enableRustStandardEAPAKAHandoffForTest(t)
	capability := selectReaderEAPAKASessionCapability("Fixture Reader", 7, fixtureNativeEAPUSIM())
	if !capability.usesRustStandardEAP() {
		t.Fatal("fixture did not select native EAP")
	}
	// The process-wide config changes while the session is live.  Cleanup must
	// still reach the sidecar that owns this session's transcript.
	t.Setenv("PROLETARIUS_CARRIER_SOCKET", socketB)
	adapter := &swuSIMAdapter{eapCapability: capability}
	if err := adapter.ReleaseEAPIdentityExchange(testNativeEAPExchangeID); err != nil {
		t.Fatalf("release on the frozen socket failed: %v", err)
	}
	if chosenCalls.Load() != 1 || otherCalls.Load() != 0 {
		t.Fatalf("release routed to the wrong sidecar: chosen=%d other=%d", chosenCalls.Load(), otherCalls.Load())
	}
}

func newEAPReleaseServer(t *testing.T, socket string, calls *atomic.Int32) *http.Server {
	t.Helper()
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	server := &http.Server{Handler: http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if request.Method != http.MethodPost || request.URL.Path != "/api/v2/vowifi/eap-exchange/release" {
			http.NotFound(response, request)
			return
		}
		calls.Add(1)
		_, _ = response.Write([]byte("{}"))
	})}
	go func() { _ = server.Serve(listener) }()
	return server
}
