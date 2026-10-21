package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func makeMSISDNRecord(length, tonNPI byte, digits []byte, extension byte) []byte {
	record := make([]byte, 32)
	for i := range record {
		record[i] = 0xFF
	}
	footer := record[len(record)-14:]
	footer[0] = length
	footer[1] = tonNPI
	copy(footer[2:], digits)
	footer[13] = extension
	return record
}

func TestDecodeMSISDNRecord(t *testing.T) {
	if got := decodeMSISDNRecord(makeMSISDNRecord(6, 0x91, []byte{0x21, 0x43, 0x65, 0xF7}, 0xFF)); got != "+1234567" {
		t.Fatalf("international number = %q, want +1234567", got)
	}
	if got := decodeMSISDNRecord(makeMSISDNRecord(5, 0x81, []byte{0x21, 0x43, 0xF5}, 0xFF)); got != "12345" {
		t.Fatalf("national number = %q, want 12345", got)
	}
	if got := decodeMSISDNRecord(makeMSISDNRecord(6, 0x91, []byte{0x21, 0x43, 0x65}, 0x01)); got != "" {
		t.Fatalf("extended number = %q, want empty", got)
	}
}

func TestDecodeMSISDNRecordRejectsAllZeroPlaceholder(t *testing.T) {
	if got := decodeMSISDNRecord(makeMSISDNRecord(7, 0x81, []byte{0x00, 0x00, 0x00, 0x00, 0x00, 0xF0}, 0xFF)); got != "" {
		t.Fatalf("decodeMSISDNRecord(all zero) = %q, want empty", got)
	}
}

func TestDecodeMSISDNRecordRejectsInvalidRecords(t *testing.T) {
	for _, record := range [][]byte{nil, make([]byte, 13), make([]byte, 14)} {
		if got := decodeMSISDNRecord(record); got != "" {
			t.Fatalf("invalid record = %q, want empty", got)
		}
	}
}

func TestReconcilePresenceKeepsSelectedReader(t *testing.T) {
	rm := newReaderManager()
	rm.readerName = "Reader A"
	rm.generation = 7
	rm.cachedUSIM = &usimInfo{ICCID: "8988", LoadedAt: time.Now()}

	if removed := rm.ReconcilePresence([]string{"Reader A", "Reader B"}); removed {
		t.Fatal("ReconcilePresence removed a reader that is still present")
	}
	if got := rm.Status().ReaderName; got != "Reader A" {
		t.Fatalf("selected reader = %q, want Reader A", got)
	}
	if rm.USIMInfo() == nil {
		t.Fatal("USIM cache was cleared while reader remained present")
	}
}

func TestCardOperationsAreSerialized(t *testing.T) {
	rm := newReaderManager()
	releaseFirst := rm.beginCardOperation()
	acquired := make(chan struct{})
	done := make(chan struct{})
	go func() {
		releaseSecond := rm.beginCardOperation()
		close(acquired)
		releaseSecond()
		close(done)
	}()

	select {
	case <-acquired:
		t.Fatal("second card operation entered while the first still owned the reader")
	case <-time.After(25 * time.Millisecond):
	}

	releaseFirst()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("second card operation did not resume after the reader was released")
	}
}

func TestStaleBLECardOperationReleaseCannotClearReplacementBusyState(t *testing.T) {
	rm := newReaderManager()
	// Model a connected BLE reader without invoking hardware discovery. The
	// operation lease belongs to generation 7.
	rm.readerName = "BLE:ESTKme-RED:A"
	rm.generation = 7
	rm.activeBLEGeneration.Store(7)
	releaseOld := rm.beginCardOperation()
	if !rm.bleOperationBusy() {
		t.Fatal("first BLE operation did not mark its generation busy")
	}

	// A disconnect/reconnect can replace the active owner while the previous
	// APDU is unwinding. Its busy flag belongs to generation 9, not to the old
	// release closure.
	rm.readerName = "BLE:ESTKme-RED:B"
	rm.generation = 9
	rm.activeBLEGeneration.Store(9)
	rm.bleBusyGeneration.Store(9)
	releaseOld()

	got := rm.bleBusyGeneration.Load()
	if !rm.bleOperationBusy() || got != 9 {
		t.Fatalf("stale release cleared replacement busy lease: generation=%d busy=%d", rm.activeBLEGeneration.Load(), got)
	}
}

func TestNativeReaderSessionLeaseBindsIdentityAndExcludesGoOperations(t *testing.T) {
	rm := newReaderManager()
	rm.readerName = "BLE:ESTKme-RED:fixture"
	rm.generation = 17
	rm.activeBLEGeneration.Store(17)
	rm.usimMu.Lock()
	rm.cachedUSIM = &usimInfo{
		ICCID:           "8985200014631959072",
		IMSI:            "234100896392986",
		MCC:             "234",
		MNC:             "33",
		GID1:            "ABCD",
		PrivateIdentity: "subscriber@ims.example.test",
		PublicIdentity:  "sip:+447700900123@ims.example.test",
	}
	rm.usimMu.Unlock()

	lease, err := rm.acquireNativeReaderSessionLease(context.Background(), rm.readerName)
	if err != nil {
		t.Fatalf("acquire native reader lease: %v", err)
	}
	if lease.ReaderGeneration != 17 || lease.Identity.ICCID != "8985200014631959072" || lease.Identity.IMSI != "234100896392986" {
		t.Fatal("lease identity is not bound to the selected card generation")
	}
	if lease.Identity.PrivateIdentity != "subscriber@ims.example.test" || lease.Identity.PublicIdentity != "sip:+447700900123@ims.example.test" {
		t.Fatal("lease did not preserve cached ISIM identities for the native IMS session")
	}
	if !isHexLeaseID(lease.LeaseID) {
		t.Fatal("lease token is not opaque 256-bit hex")
	}
	if rm.ReconcilePresence(nil) {
		t.Fatal("inventory reconciliation disconnected a leased reader")
	}
	if _, removed := rm.reconcileHotUnplug(nil, 1); removed {
		t.Fatal("hot-unplug monitor disconnected a leased reader")
	}
	if beginReaderVoWiFiOperation() {
		endReaderVoWiFiOperation()
		t.Fatal("Go reader VoWiFi operation started while Rust held the reader lease")
	}
	if rm.authMu.TryLock() {
		rm.authMu.Unlock()
		t.Fatal("Go APDU path entered while Rust held the reader lease")
	}
	if rm.releaseNativeReaderSessionLease(strings.Repeat("0", 64)) {
		t.Fatal("untrusted token released the native reader lease")
	}
	if !rm.releaseNativeReaderSessionLease(lease.LeaseID) {
		t.Fatal("valid token failed to release the reader lease")
	}
	if rm.releaseNativeReaderSessionLease(lease.LeaseID) {
		t.Fatal("duplicate release was accepted")
	}
	if !rm.authMu.TryLock() {
		t.Fatal("Go APDU path remained blocked after lease release")
	}
	rm.authMu.Unlock()
	if !beginReaderVoWiFiOperation() {
		t.Fatal("Go VoWiFi operation remained blocked after lease release")
	}
	endReaderVoWiFiOperation()
}

func TestNativeReaderSessionLeaseHandsAPDUMutexToOneBoundAKAOperation(t *testing.T) {
	rm := newReaderManager()
	rm.readerName = "BLE:ESTKme-RED:fixture"
	rm.generation = 24
	rm.usimMu.Lock()
	rm.cachedUSIM = &usimInfo{
		ICCID: "8985200014631959072",
		IMSI:  "234100896392986",
		MCC:   "234",
		MNC:   "33",
	}
	rm.usimMu.Unlock()
	lease, err := rm.acquireNativeReaderSessionLease(context.Background(), rm.readerName)
	if err != nil {
		t.Fatalf("acquire native reader lease: %v", err)
	}

	started := make(chan struct{})
	continueAKA := make(chan struct{})
	akaDone := make(chan error, 1)
	go func() {
		_, err := rm.withNativeReaderSessionLease(
			lease.LeaseID,
			lease.Reader,
			lease.ReaderGeneration,
			func() (*akaResult, error) {
				if !rm.authMu.TryLock() {
					return nil, errors.New("APDU mutex was not handed to the leased AKA operation")
				}
				rm.authMu.Unlock()
				close(started)
				<-continueAKA
				return &akaResult{RES: make([]byte, 8), CK: make([]byte, 16), IK: make([]byte, 16)}, nil
			},
		)
		akaDone <- err
	}()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("leased AKA operation did not start")
	}

	releaseDone := make(chan bool, 1)
	go func() { releaseDone <- rm.releaseNativeReaderSessionLease(lease.LeaseID) }()
	select {
	case <-releaseDone:
		t.Fatal("lease released while its AKA operation was still using the card")
	case <-time.After(20 * time.Millisecond):
	}
	close(continueAKA)
	if err := <-akaDone; err != nil {
		t.Fatalf("leased AKA operation: %v", err)
	}
	if !<-releaseDone {
		t.Fatal("release did not complete after the AKA operation")
	}
	if !rm.authMu.TryLock() {
		t.Fatal("release left the card mutex held")
	}
	rm.authMu.Unlock()
}

func TestNativeReaderSessionLeaseExpiryReleasesCardAndOperationGates(t *testing.T) {
	rm := newReaderManager()
	rm.readerName = "Reader A"
	rm.generation = 22
	rm.usimMu.Lock()
	rm.cachedUSIM = &usimInfo{
		ICCID: "8985200014631959072",
		IMSI:  "234100896392986",
		MCC:   "234",
		MNC:   "33",
	}
	rm.usimMu.Unlock()
	if _, err := rm.acquireNativeReaderSessionLease(context.Background(), rm.readerName); err != nil {
		t.Fatalf("acquire native reader lease: %v", err)
	}
	rm.nativeLeaseMu.Lock()
	lease := rm.nativeLease
	if lease != nil {
		lease.expiresAt = time.Now().Add(-time.Second)
		if lease.timer != nil {
			lease.timer.Stop()
		}
	}
	rm.nativeLeaseMu.Unlock()
	if lease == nil {
		t.Fatal("native reader lease was not retained")
	}
	rm.expireNativeReaderSessionLease(lease)
	if rm.authMu.TryLock() {
		rm.authMu.Unlock()
	} else {
		t.Fatal("watchdog did not release the APDU mutex")
	}
	if !beginReaderVoWiFiOperation() {
		t.Fatal("watchdog did not reopen the reader operation gate")
	}
	endReaderVoWiFiOperation()
}

func TestNativeReaderSessionLeaseRenewalDefeatsLateExpiryCallback(t *testing.T) {
	rm := newReaderManager()
	rm.readerName = "Reader A"
	rm.generation = 25
	rm.usimMu.Lock()
	rm.cachedUSIM = &usimInfo{
		ICCID: "8985200014631959072",
		IMSI:  "234100896392986",
		MCC:   "234",
		MNC:   "33",
	}
	rm.usimMu.Unlock()
	lease, err := rm.acquireNativeReaderSessionLease(context.Background(), rm.readerName)
	if err != nil {
		t.Fatalf("acquire native reader lease: %v", err)
	}
	defer rm.releaseNativeReaderSessionLease(lease.LeaseID)

	rm.nativeLeaseMu.Lock()
	entry := rm.nativeLease
	oldExpiry := entry.expiresAt
	rm.nativeLeaseMu.Unlock()
	if !rm.renewNativeReaderSessionLease(lease.LeaseID) {
		t.Fatal("valid lease renewal was rejected")
	}
	rm.nativeLeaseMu.Lock()
	newExpiry := entry.expiresAt
	rm.nativeLeaseMu.Unlock()
	if !newExpiry.After(oldExpiry) || !newExpiry.After(time.Now().Add(time.Minute)) {
		t.Fatal("renewal did not extend the lease watchdog deadline")
	}

	// Simulate an expiry callback that was queued before the renewal reset its
	// timer. It must re-check expiresAt and leave the card locked.
	rm.expireNativeReaderSessionLease(entry)
	rm.nativeLeaseMu.Lock()
	retained := rm.nativeLease == entry
	rm.nativeLeaseMu.Unlock()
	if !retained {
		t.Fatal("stale expiry callback removed a renewed reader lease")
	}
	if rm.authMu.TryLock() {
		rm.authMu.Unlock()
		t.Fatal("stale expiry callback released a renewed card lock")
	}
	if rm.renewNativeReaderSessionLease(strings.Repeat("0", 64)) {
		t.Fatal("untrusted token renewed the reader lease")
	}
	if !rm.releaseNativeReaderSessionLease(lease.LeaseID) {
		t.Fatal("valid token failed to release renewed lease")
	}
}

func TestNativeReaderSessionLeaseCancellationReopensOperationGate(t *testing.T) {
	rm := newReaderManager()
	rm.readerName = "Reader A"
	rm.generation = 23
	rm.authMu.Lock()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if _, err := rm.acquireNativeReaderSessionLease(ctx, rm.readerName); err == nil {
		t.Fatal("lease acquisition succeeded while the card lock remained held")
	}
	rm.authMu.Unlock()
	if !beginReaderVoWiFiOperation() {
		t.Fatal("cancelled acquisition left the reader operation gate closed")
	}
	endReaderVoWiFiOperation()
}

func TestNativeReaderSessionLeasePrivateHandlersReleaseAuthenticatedToken(t *testing.T) {
	rm := newReaderManager()
	rm.readerName = "Reader A"
	rm.generation = 21
	rm.usimMu.Lock()
	rm.cachedUSIM = &usimInfo{
		ICCID:           "8985200014631959072",
		IMSI:            "234100896392986",
		MCC:             "234",
		MNC:             "33",
		PrivateIdentity: "subscriber@ims.example.test",
		PublicIdentity:  "sip:+447700900123@ims.example.test",
	}
	rm.usimMu.Unlock()
	a := &app{readerMgr: rm}

	acquire := httptest.NewRequest("POST", "/api/internal/reader/session-lease/acquire", strings.NewReader(`{"reader":"Reader A"}`))
	acquire.Header.Set("Content-Type", "application/json")
	acquired := httptest.NewRecorder()
	a.acquireNativeReaderSessionLease(acquired, acquire)
	if acquired.Code != 200 {
		t.Fatalf("acquire status = %d, want 200", acquired.Code)
	}
	var response nativeReaderSessionLeaseResponse
	if err := json.Unmarshal(acquired.Body.Bytes(), &response); err != nil {
		t.Fatalf("decode lease response: %v", err)
	}
	if !isHexLeaseID(response.LeaseID) || response.ReaderGeneration != 21 {
		t.Fatal("lease response omitted its opaque token or reader generation")
	}
	if response.Identity.PrivateIdentity != "subscriber@ims.example.test" || response.Identity.PublicIdentity != "sip:+447700900123@ims.example.test" {
		t.Fatal("private lease response omitted cached ISIM identities")
	}
	akaBody, err := json.Marshal(nativeReaderSessionLeaseAKARequest{
		LeaseID:          strings.Repeat("0", 64),
		Reader:           "Reader A",
		ReaderGeneration: 21,
		Application:      "usim",
		RAND:             make([]int, 16),
		AUTN:             make([]int, 16),
	})
	if err != nil {
		t.Fatal(err)
	}
	if rm.renewNativeReaderSessionLease(strings.Repeat("0", 64)) {
		t.Fatal("invalid lease token was renewed")
	}
	renewBody, err := json.Marshal(nativeReaderSessionLeaseRenewRequest{LeaseID: response.LeaseID})
	if err != nil {
		t.Fatal(err)
	}
	renew := httptest.NewRequest("POST", "/api/internal/reader/session-lease/renew", strings.NewReader(string(renewBody)))
	renew.Header.Set("Content-Type", "application/json")
	renewed := httptest.NewRecorder()
	a.renewNativeReaderSessionLease(renewed, renew)
	if renewed.Code != http.StatusOK || !strings.Contains(renewed.Body.String(), `"renewed":true`) {
		t.Fatalf("renew response = %d %s", renewed.Code, renewed.Body.String())
	}
	akaRequest := httptest.NewRequest("POST", "/api/internal/reader/session-lease/aka", strings.NewReader(string(akaBody)))
	akaRequest.Header.Set("Content-Type", "application/json")
	akaResponse := httptest.NewRecorder()
	a.authenticateNativeReaderSessionLease(akaResponse, akaRequest)
	if akaResponse.Code != http.StatusBadGateway || !strings.Contains(akaResponse.Body.String(), "reader_session_lease_stale") {
		t.Fatalf("invalid lease AKA response = %d %s", akaResponse.Code, akaResponse.Body.String())
	}
	if rm.authMu.TryLock() {
		rm.authMu.Unlock()
		t.Fatal("invalid AKA token released the card lock")
	}
	refresh := httptest.NewRecorder()
	a.readerRefresh(refresh, httptest.NewRequest("POST", "/api/reader/refresh", nil))
	if refresh.Code != http.StatusConflict {
		t.Fatalf("refresh during native lease = %d, want conflict", refresh.Code)
	}
	auth := httptest.NewRecorder()
	a.readerAuth(auth, httptest.NewRequest("POST", "/api/reader/auth", nil))
	if auth.Code != http.StatusConflict {
		t.Fatalf("public AKA during native lease = %d, want conflict", auth.Code)
	}

	releaseBody, err := json.Marshal(nativeReaderSessionLeaseReleaseRequest{LeaseID: response.LeaseID})
	if err != nil {
		t.Fatal(err)
	}
	release := httptest.NewRequest("POST", "/api/internal/reader/session-lease/release", strings.NewReader(string(releaseBody)))
	release.Header.Set("Content-Type", "application/json")
	released := httptest.NewRecorder()
	a.releaseNativeReaderSessionLease(released, release)
	if released.Code != 200 || !strings.Contains(released.Body.String(), `"released":true`) {
		t.Fatalf("release response = %d %s", released.Code, released.Body.String())
	}
}

func TestNativeReaderSessionLeaseRejectsStaleReaderBeforeLocking(t *testing.T) {
	rm := newReaderManager()
	rm.readerName = "Reader B"
	rm.generation = 18
	if _, err := rm.acquireNativeReaderSessionLease(context.Background(), "Reader A"); err == nil {
		t.Fatal("lease acquired for a reader other than the selected generation")
	}
	if !rm.authMu.TryLock() {
		t.Fatal("failed acquisition left the card mutex held")
	}
	rm.authMu.Unlock()
}

func isHexLeaseID(value string) bool {
	if len(value) != 64 {
		return false
	}
	for _, character := range value {
		if !(character >= '0' && character <= '9') && !(character >= 'a' && character <= 'f') {
			return false
		}
	}
	return true
}

func TestReconcilePresenceClearsDisconnectedReaderState(t *testing.T) {
	rm := newReaderManager()
	rm.readerName = "Reader A"
	rm.generation = 7
	rm.cachedUSIM = &usimInfo{ICCID: "8988", LoadedAt: time.Now()}

	if removed := rm.ReconcilePresence([]string{"Reader B"}); !removed {
		t.Fatal("ReconcilePresence did not report the missing selected reader")
	}
	status := rm.Status()
	if status.Connected || status.ReaderName != "" || status.HasUSIMInfo || status.HasESIM {
		t.Fatalf("stale reader status after unplug: %+v", status)
	}
	if rm.USIMInfo() != nil {
		t.Fatal("USIM cache remains after unplug")
	}
	if rm.generation != 8 {
		t.Fatalf("generation = %d, want 8", rm.generation)
	}
}
