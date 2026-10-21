package main

import (
	"context"
	"crypto/sha1"
	"encoding/json"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"sync/atomic"
	"testing"
	"time"

	"github.com/1239t/swu-go/pkg/eap"
)

const (
	e2eReaderName       = "ESTKme-RED USB fixture"
	e2eReaderGeneration = uint64(17)
	e2eICCID            = "8901234567890123456"
	e2eIMSI             = "234100896392986"
	e2eExchangeID       = "cdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcd"
)

// TestReaderEAPSessionFrozenUSBIdentityThenAKA exercises the bounded cutover
// seam without a physical reader or a Rust daemon.  The Unix HTTP server is a
// protocol-faithful fake: it proves that one frozen USB session supplies the
// Identity transcript to the selected sidecar, then sends the exact exchange,
// reader and SIM binding with the standard EAP-AKA Challenge.
func TestReaderEAPSessionFrozenUSBIdentityThenAKA(t *testing.T) {
	directory, err := os.MkdirTemp("/tmp", "p-eap-e2e-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(directory) })
	chosenSocket := filepath.Join(directory, "chosen.sock")
	changedSocket := filepath.Join(directory, "changed.sock")
	t.Setenv("PROLETARIUS_RUST_EAP_AKA", "1")
	t.Setenv("PROLETARIUS_CARRIER_SOCKET", chosenSocket)
	t.Setenv("PROLETARIUS_APP_TOKEN", "eap-e2e-token")

	info := &usimInfo{ICCID: e2eICCID, IMSI: e2eIMSI}
	capability := selectReaderEAPAKASessionCapability(e2eReaderName, e2eReaderGeneration, info)
	if !capability.usesRustStandardEAP() {
		t.Fatal("eligible explicit-test USB session did not select the Rust standard EAP owner")
	}
	if capability.reader != e2eReaderName || capability.generation != e2eReaderGeneration ||
		capability.identity != (nativeReaderAuthIdentity{ICCID: e2eICCID, IMSI: e2eIMSI}) ||
		capability.socket != chosenSocket {
		t.Fatalf("session capability did not freeze the selected context: %#v", capability)
	}

	identity := []byte("0234100896392986@wlan.mnc010.mcc234.3gppnetwork.org")
	identityRequest := (&eap.EAPPacket{
		Code: eap.CodeRequest, Identifier: 19, Type: eap.TypeIdentity,
	}).Encode()
	identityResponse := (&eap.EAPPacket{
		Code: eap.CodeResponse, Identifier: 19, Type: eap.TypeIdentity, Data: identity,
	}).Encode()
	identityTranscript := append(append([]byte(nil), identityRequest...), identityResponse...)
	identityCheckcode := sha1.Sum(identityTranscript)

	challenge := e2eStandardAKAChallenge(41)
	akaResponse := (&eap.EAPPacket{
		Code: eap.CodeResponse, Identifier: 41, Type: eap.TypeAKA, Subtype: eap.SubtypeChallenge,
	}).Encode()
	msk := make([]byte, 64)
	for index := range msk {
		msk[index] = byte(index + 1)
	}
	kAut := make([]byte, 16)
	for index := range kAut {
		kAut[index] = byte(0xf0 + index)
	}

	var identityCalls atomic.Int32
	var akaCalls atomic.Int32
	listener, err := net.Listen("unix", chosenSocket)
	if err != nil {
		t.Fatal(err)
	}
	server := &http.Server{Handler: http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if request.Header.Get("X-Proletarius-App-Token") != "eap-e2e-token" {
			http.Error(response, "missing app token", http.StatusUnauthorized)
			return
		}
		switch request.URL.Path {
		case "/api/v2/vowifi/eap-identity":
			identityCalls.Add(1)
			var input struct {
				Identity []int `json:"identity"`
				Request  []int `json:"request"`
			}
			if err := json.NewDecoder(request.Body).Decode(&input); err != nil ||
				!reflect.DeepEqual(input.Identity, byteInts(identity)) ||
				!reflect.DeepEqual(input.Request, byteInts(identityRequest)) {
				http.Error(response, "invalid identity request", http.StatusBadRequest)
				return
			}
			_ = json.NewEncoder(response).Encode(map[string]any{
				"eap_response": byteInts(identityResponse),
				"exchange_id":  e2eExchangeID,
				"checkcode":    byteInts(identityCheckcode[:]),
			})
		case "/api/v2/reader/eap-aka":
			akaCalls.Add(1)
			var input struct {
				Reader           string                    `json:"reader"`
				Application      string                    `json:"application"`
				ExchangeID       string                    `json:"exchange_id"`
				Challenge        []int                     `json:"challenge"`
				CheckcodeMode    string                    `json:"checkcode_mode"`
				ExpectedIdentity *nativeReaderAuthIdentity `json:"expected_identity"`
			}
			if err := json.NewDecoder(request.Body).Decode(&input); err != nil ||
				input.Reader != e2eReaderName || input.Application != "usim" ||
				input.ExchangeID != e2eExchangeID || input.CheckcodeMode != "checkcode" ||
				input.ExpectedIdentity == nil ||
				*input.ExpectedIdentity != (nativeReaderAuthIdentity{ICCID: e2eICCID, IMSI: e2eIMSI}) ||
				!reflect.DeepEqual(input.Challenge, byteInts(challenge)) {
				http.Error(response, "invalid AKA request", http.StatusBadRequest)
				return
			}
			_ = json.NewEncoder(response).Encode(map[string]any{
				"outcome":      "success",
				"eap_response": byteInts(akaResponse),
				"msk":          byteInts(msk),
				"k_aut":        byteInts(kAut),
			})
		default:
			http.NotFound(response, request)
		}
	})}
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(func() { _ = server.Close() })

	// Change the process configuration after session creation.  A request to
	// changedSocket would fail, so the complete successful flow demonstrates
	// that both calls used capability.socket rather than rereading the env var.
	t.Setenv("PROLETARIUS_CARRIER_SOCKET", changedSocket)
	manager := &readerManager{
		readerName: e2eReaderName,
		generation: e2eReaderGeneration,
		cachedUSIM: info,
	}
	adapter := &swuSIMAdapter{
		rm:            manager,
		generation:    e2eReaderGeneration,
		imsi:          e2eIMSI,
		sessionCtx:    context.Background(),
		eapCapability: capability,
	}

	identityResult, identityHandled, err := adapter.RespondEAPIdentity(identityRequest, identity)
	if err != nil || !identityHandled || !reflect.DeepEqual(identityResult.Packet, identityResponse) ||
		identityResult.ExchangeID != e2eExchangeID || !reflect.DeepEqual(identityResult.Checkcode, identityCheckcode[:]) {
		t.Fatalf("native Identity result did not preserve the transcript: result=%#v handled=%t err=%v", identityResult, identityHandled, err)
	}
	akaResult, akaHandled, err := adapter.RespondEAPAKA(challenge, identityResult.ExchangeID, "checkcode")
	if err != nil || !akaHandled || akaResult.Outcome != "success" ||
		!reflect.DeepEqual(akaResult.Packet, akaResponse) || !reflect.DeepEqual(akaResult.MSK, msk) ||
		!reflect.DeepEqual(akaResult.KAut, kAut) || akaResult.NextExchangeID != "" {
		t.Fatalf("native standard EAP-AKA result was incomplete: result=%#v handled=%t err=%v", akaResult, akaHandled, err)
	}
	if identityCalls.Load() != 1 || akaCalls.Load() != 1 {
		t.Fatalf("unexpected frozen-sidecar call counts: identity=%d aka=%d", identityCalls.Load(), akaCalls.Load())
	}
}

// TestReaderEAPAKASessionCapabilityRequiresExplicitExperiment keeps the
// runtime-safe default clear: merely configuring a local sidecar is not
// permission to migrate a production EAP-AKA transaction.  The one-time
// session selection must receive the explicit test gate.
func TestReaderEAPAKASessionCapabilityRequiresExplicitExperiment(t *testing.T) {
	socket := filepath.Join(t.TempDir(), "proletariusd.sock")
	t.Setenv("PROLETARIUS_CARRIER_SOCKET", socket)
	t.Setenv("PROLETARIUS_RUST_EAP_AKA", "")
	if got := selectReaderEAPAKASessionCapability(e2eReaderName, e2eReaderGeneration, &usimInfo{ICCID: e2eICCID, IMSI: e2eIMSI}); got.usesRustStandardEAP() {
		t.Fatalf("Rust standard EAP selected without the explicit experiment gate: %#v", got)
	}
	t.Setenv("PROLETARIUS_RUST_EAP_AKA", "1")
	if got := selectReaderEAPAKASessionCapability(e2eReaderName, e2eReaderGeneration, &usimInfo{ICCID: e2eICCID, IMSI: e2eIMSI}); !got.usesRustStandardEAP() {
		t.Fatal("eligible explicit-test USB session did not select Rust standard EAP")
	}
}

func TestReaderEAPConversationOwnerIsFrozenAtSessionCreation(t *testing.T) {
	socket := filepath.Join(t.TempDir(), "proletariusd.sock")
	info := &usimInfo{ICCID: e2eICCID, IMSI: e2eIMSI}
	t.Setenv("PROLETARIUS_CARRIER_SOCKET", socket)

	// A default Go conversation must remain Go even if an environment reload
	// enables the next session. The existing Rust PC/SC primitive is outside
	// this provider contract and is intentionally not invoked by this adapter
	// method when the frozen capability is Go.
	t.Setenv("PROLETARIUS_RUST_EAP_AKA", "")
	goCapability := selectReaderEAPAKASessionCapability(e2eReaderName, e2eReaderGeneration, info)
	t.Setenv("PROLETARIUS_RUST_EAP_AKA", "1")
	goAdapter := &swuSIMAdapter{eapCapability: goCapability}
	if _, handled, err := goAdapter.RespondEAPIdentity(nil, nil); handled || err != nil {
		t.Fatalf("Go EAP conversation owner changed after its capability was frozen: handled=%t err=%v", handled, err)
	}

	// Conversely, turning the environment off affects only later sessions. A
	// frozen Rust owner still handles the provider request (and, with no
	// manager, fails at its own explicit initialization boundary).
	rustCapability := selectReaderEAPAKASessionCapability(e2eReaderName, e2eReaderGeneration, info)
	t.Setenv("PROLETARIUS_RUST_EAP_AKA", "")
	rustAdapter := &swuSIMAdapter{eapCapability: rustCapability}
	if _, handled, err := rustAdapter.RespondEAPIdentity(nil, nil); !handled || err == nil {
		t.Fatalf("Rust EAP conversation owner changed after its capability was frozen: handled=%t err=%v", handled, err)
	}
}

// TestNativeEAPAKAHTTPTimeoutLeavesSidecarResponseMargin documents the
// cross-language timeout contract.  Rust's reader_eap_aka worker has a 30 s
// physical-operation budget; Go must retain a fixed transport margin so an
// explicit Rust timeout is not turned into an ambiguous same-deadline client
// cancellation.
func TestNativeEAPAKAHTTPTimeoutLeavesSidecarResponseMargin(t *testing.T) {
	const rustReaderEAPAKAOperationTimeout = 30 * time.Second
	if nativeReaderEAPAKAHTTPTimeout <= rustReaderEAPAKAOperationTimeout {
		t.Fatalf("Go EAP-AKA timeout %s must exceed Rust operation timeout %s", nativeReaderEAPAKAHTTPTimeout, rustReaderEAPAKAOperationTimeout)
	}
}

func e2eStandardAKAChallenge(identifier byte) []byte {
	attributes := make([]byte, 0, 80)
	attributes = append(attributes, (&eap.Attribute{Type: eap.AT_RAND, Value: append([]byte{0, 0}, make([]byte, 16)...)}).Encode()...)
	attributes = append(attributes, (&eap.Attribute{Type: eap.AT_AUTN, Value: append([]byte{0, 0}, make([]byte, 16)...)}).Encode()...)
	attributes = append(attributes, (&eap.Attribute{Type: eap.AT_MAC, Value: append([]byte{0, 0}, make([]byte, 16)...)}).Encode()...)
	return (&eap.EAPPacket{
		Code: eap.CodeRequest, Identifier: identifier, Type: eap.TypeAKA, Subtype: eap.SubtypeChallenge, Data: attributes,
	}).Encode()
}

func TestE2EStandardAKAChallengeIncludesRequiredAttributes(t *testing.T) {
	packet, err := eap.Parse(e2eStandardAKAChallenge(41))
	if err != nil || packet.Code != eap.CodeRequest || packet.Type != eap.TypeAKA || packet.Subtype != eap.SubtypeChallenge ||
		!sameDeclaredEAPLength(e2eStandardAKAChallenge(41)) {
		t.Fatalf("fixture is not an EAP-AKA Challenge: packet=%#v err=%v", packet, err)
	}
	attributes, err := eap.ParseAttributes(packet.Data)
	if err != nil || attributes[eap.AT_RAND] == nil || attributes[eap.AT_AUTN] == nil || attributes[eap.AT_MAC] == nil {
		t.Fatalf("fixture omitted required EAP-AKA attributes: attrs=%#v err=%v", attributes, err)
	}
	if !validNativeEAPExchangeID(e2eExchangeID) {
		t.Fatal("E2E fixture exchange ID is not a valid fixed bearer capability")
	}
}
