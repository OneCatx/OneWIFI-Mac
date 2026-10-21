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

func TestReaderListMissingSampleKeepsSelectedReaderAndVoWiFiSession(t *testing.T) {
	manager := newReaderManager()
	manager.readerName = "Reader A"
	manager.generation = 11
	manager.cachedUSIM = &usimInfo{ICCID: "8988"}
	application := &app{readerMgr: manager}

	vowifiSessionMu.Lock()
	previousSession := activeSession
	session := &vowifiSession{sourceKind: "reader", readerGeneration: 11}
	activeSession = session
	vowifiSessionMu.Unlock()
	t.Cleanup(func() {
		vowifiSessionMu.Lock()
		activeSession = previousSession
		vowifiSessionMu.Unlock()
	})

	recorder := httptest.NewRecorder()
	application.readerListWithInventory(recorder, func() ([]string, error) { return nil, nil })
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	var response readerListResponse
	if err := json.NewDecoder(recorder.Body).Decode(&response); err != nil {
		t.Fatal(err)
	}
	if response.Selected != "Reader A" {
		t.Fatalf("selected = %q, want Reader A", response.Selected)
	}
	if status := manager.Status(); !status.Connected || status.ReaderName != "Reader A" || status.Generation != 11 {
		t.Fatalf("one missing inventory sample changed reader session: %+v", status)
	}
	if manager.USIMInfo() == nil {
		t.Fatal("one missing inventory sample cleared card identity")
	}
	vowifiSessionMu.Lock()
	stillActive := activeSession == session
	vowifiSessionMu.Unlock()
	if !stillActive {
		t.Fatal("one missing inventory sample stopped the active VoWiFi session")
	}
}

func TestReaderReconcileObservesMissingUSBWithoutDisconnectingSession(t *testing.T) {
	manager := newReaderManager()
	manager.readerName = "Reader A"
	manager.generation = 7
	manager.cachedUSIM = &usimInfo{ICCID: "8988", LoadedAt: time.Now()}
	application := &app{readerMgr: manager}

	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/api/internal/reader/reconcile",
		strings.NewReader(`{"readers":["Reader B"]}`))
	application.readerReconcile(recorder, request)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	var response readerListResponse
	if err := json.NewDecoder(recorder.Body).Decode(&response); err != nil {
		t.Fatal(err)
	}
	if response.Selected != "" {
		t.Fatalf("stale selected reader was published: %q", response.Selected)
	}
	if status := manager.Status(); !status.Connected || status.ReaderName != "Reader A" {
		t.Fatalf("read-only reconciliation changed Go session: %+v", status)
	}
	if manager.USIMInfo() == nil || manager.generation != 7 {
		t.Fatal("read-only reconciliation cleared card identity or generation")
	}
}

func TestReaderReconcileKeepsBLESelectionDuringHandshakeGrace(t *testing.T) {
	requests := withCountingBLEBridgeReader(t, fixtureBLEReader)
	manager := newReaderManager()
	manager.readerName = fixtureBLEReader
	manager.connectTime = time.Now()
	application := &app{readerMgr: manager}

	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/api/internal/reader/reconcile",
		strings.NewReader(`{"readers":[]}`))
	application.readerReconcile(recorder, request)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	var response readerListResponse
	if err := json.NewDecoder(recorder.Body).Decode(&response); err != nil {
		t.Fatal(err)
	}
	if response.Selected != fixtureBLEReader {
		t.Fatalf("selected = %q, want %q", response.Selected, fixtureBLEReader)
	}
	if len(response.Readers) != 1 || response.Readers[0] != fixtureBLEReader {
		t.Fatalf("readers = %q, want selected BLE identity", response.Readers)
	}
	if status := manager.Status(); !status.Connected || status.ReaderName != fixtureBLEReader {
		t.Fatalf("read-only reconciliation changed BLE session: %+v", status)
	}
	if got := requests.Load(); got != 0 {
		t.Fatalf("BLE handshake grace issued %d bridge status probes, want 0", got)
	}
}

func TestReaderReconcileUsesOneProbeForEstablishedSelectedBLE(t *testing.T) {
	requests := withCountingBLEBridgeReader(t, fixtureBLEReader)
	manager := newReaderManager()
	manager.readerName = fixtureBLEReader
	manager.connectTime = time.Now().Add(-bleConnectGracePeriod)
	application := &app{readerMgr: manager}

	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/api/internal/reader/reconcile",
		strings.NewReader(`{"readers":[]}`))
	application.readerReconcile(recorder, request)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	var response readerListResponse
	if err := json.NewDecoder(recorder.Body).Decode(&response); err != nil {
		t.Fatal(err)
	}
	if response.Selected != fixtureBLEReader || len(response.Readers) != 1 || response.Readers[0] != fixtureBLEReader {
		t.Fatalf("response = %+v, want selected BLE reader", response)
	}
	if got := requests.Load(); got != 1 {
		t.Fatalf("BLE reconciliation issued %d bridge status probes, want 1", got)
	}
}

func TestReaderReconcileDiscoversBLEWhenNothingIsSelected(t *testing.T) {
	requests := withCountingBLEBridgeReader(t, fixtureBLEReader)
	application := &app{readerMgr: newReaderManager()}

	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/api/internal/reader/reconcile",
		strings.NewReader(`{"readers":[]}`))
	application.readerReconcile(recorder, request)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	var response readerListResponse
	if err := json.NewDecoder(recorder.Body).Decode(&response); err != nil {
		t.Fatal(err)
	}
	if len(response.Readers) != 1 || response.Readers[0] != fixtureBLEReader || response.Selected != "" {
		t.Fatalf("response = %+v, want discovered unselected BLE reader", response)
	}
	if got := requests.Load(); got != 1 {
		t.Fatalf("BLE discovery issued %d bridge status probes, want 1", got)
	}
}

func TestReaderReconcileDiscoversBLEAlongsideSelectedUSBReader(t *testing.T) {
	requests := withCountingBLEBridgeReader(t, fixtureBLEReader)
	manager := newReaderManager()
	manager.readerName = "Reader A"
	manager.generation = 4
	application := &app{readerMgr: manager}

	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/api/internal/reader/reconcile",
		strings.NewReader(`{"readers":["Reader A"]}`))
	application.readerReconcile(recorder, request)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	var response readerListResponse
	if err := json.NewDecoder(recorder.Body).Decode(&response); err != nil {
		t.Fatal(err)
	}
	if response.Selected != "Reader A" || len(response.Readers) != 2 || response.Readers[0] != "Reader A" || response.Readers[1] != fixtureBLEReader {
		t.Fatalf("response = %+v, want USB selection plus discovered BLE reader", response)
	}
	if got := requests.Load(); got != 1 {
		t.Fatalf("BLE discovery issued %d bridge status probes, want 1", got)
	}
	if status := manager.Status(); !status.Connected || status.ReaderName != "Reader A" || manager.generation != 4 {
		t.Fatalf("read-only reconciliation changed USB session: %+v", status)
	}
}

func TestReaderReconcilePublishesReplacementBLEFromSingleProbe(t *testing.T) {
	const replacement = "BLE:AAAAAAAA-BBBB-CCCC-DDDD-EEEEEEEEEEEE"
	requests := withCountingBLEBridgeReader(t, replacement)
	manager := newReaderManager()
	manager.readerName = fixtureBLEReader
	manager.connectTime = time.Now().Add(-bleConnectGracePeriod)
	application := &app{readerMgr: manager}

	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/api/internal/reader/reconcile",
		strings.NewReader(`{"readers":[]}`))
	application.readerReconcile(recorder, request)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	var response readerListResponse
	if err := json.NewDecoder(recorder.Body).Decode(&response); err != nil {
		t.Fatal(err)
	}
	if response.Selected != "" || len(response.Readers) != 1 || response.Readers[0] != replacement {
		t.Fatalf("response = %+v, want replacement BLE reader without stale selection", response)
	}
	if got := requests.Load(); got != 1 {
		t.Fatalf("replacement BLE reconciliation issued %d bridge status probes, want 1", got)
	}
}

func TestReaderReconcileProbeHonorsRequestDeadlineWithoutHoldingManagerLock(t *testing.T) {
	entered, _ := withBlockingBLEBridgeStatus(t, fixtureBLEReader, "connected")
	manager := newReaderManager()
	manager.readerName = fixtureBLEReader
	manager.connectTime = time.Now().Add(-bleConnectGracePeriod)
	application := &app{readerMgr: manager}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Millisecond)
	defer cancel()
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/api/internal/reader/reconcile",
		strings.NewReader(`{"readers":[]}`)).WithContext(ctx)
	completed := make(chan struct{})
	go func() {
		application.readerReconcile(recorder, request)
		close(completed)
	}()

	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("BLE status probe did not reach the bridge")
	}
	statusRead := make(chan struct{})
	go func() {
		_ = manager.Status()
		close(statusRead)
	}()
	select {
	case <-statusRead:
	case <-time.After(100 * time.Millisecond):
		t.Fatal("BLE snapshot probe held reader manager lock")
	}
	if manager.authMu.TryLock() {
		manager.authMu.Unlock()
		t.Fatal("BLE snapshot probe did not retain the card-operation gate")
	}
	select {
	case <-completed:
	case <-time.After(500 * time.Millisecond):
		t.Fatal("reconcile ignored its cancelled request context")
	}
	if !manager.authMu.TryLock() {
		t.Fatal("BLE snapshot probe did not release the card-operation gate")
	}
	manager.authMu.Unlock()
}

func TestObservePresenceDropsStaleBLEProbeAfterDisconnect(t *testing.T) {
	entered, unblock := withBlockingBLEBridgeStatus(t, fixtureBLEReader, "connected")
	manager := newReaderManager()
	manager.readerName = fixtureBLEReader
	manager.connectTime = time.Now().Add(-bleConnectGracePeriod)
	observed := make(chan readerPresenceObservation, 1)
	go func() { observed <- manager.observePresence(context.Background(), nil) }()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("BLE status probe did not reach the bridge")
	}
	disconnected := make(chan error, 1)
	go func() { disconnected <- manager.Disconnect() }()
	select {
	case err := <-disconnected:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(100 * time.Millisecond):
		t.Fatal("BLE probe blocked reader disconnect")
	}
	unblock()
	select {
	case observation := <-observed:
		if observation.presence != readerPresenceAbsent || observation.selected != "" {
			t.Fatalf("stale probe observation = %+v, want disconnected reader", observation)
		}
	case <-time.After(time.Second):
		t.Fatal("BLE probe did not complete after bridge reply")
	}
}

func TestObservePresenceKeepsBLESelectionWhileCardOperationOwnsAuthLock(t *testing.T) {
	manager := newReaderManager()
	manager.readerName = fixtureBLEReader
	manager.connectTime = time.Now().Add(-bleConnectGracePeriod)
	manager.authMu.Lock()
	defer manager.authMu.Unlock()

	observation := manager.observePresence(context.Background(), nil)
	if observation.presence != readerPresenceIndeterminate {
		t.Fatalf("presence = %v, want indeterminate", observation.presence)
	}
	if observation.selected != fixtureBLEReader {
		t.Fatalf("selected = %q, want %q", observation.selected, fixtureBLEReader)
	}
}

func TestReaderHotplugMonitorPreservesBLESessionDuringBridgeReconnect(t *testing.T) {
	requests := withCountingBLEBridgeStatus(t, "", "reconnecting")
	manager := newReaderManager()
	manager.readerName = fixtureBLEReader
	manager.connectTime = time.Now().Add(-bleConnectGracePeriod)
	manager.generation = 12
	stops := 0
	monitor := &readerHotplugMonitor{
		manager:     manager,
		listReaders: manager.hotplugInventory,
		stopSession: func(uint64) { stops++ },
		required:    2,
	}

	monitor.poll(context.Background())
	monitor.poll(context.Background())
	if status := manager.Status(); !status.Connected || status.ReaderName != fixtureBLEReader || stops != 0 {
		t.Fatalf("BLE reconnect cleared live session: status=%+v stops=%d", status, stops)
	}
	if got := requests.Load(); got != 2 {
		t.Fatalf("reconnecting bridge status calls = %d, want 2", got)
	}
}

func TestReaderHotplugMonitorRemovesBLESessionAfterConfirmedBridgeDisconnect(t *testing.T) {
	requests := withCountingBLEBridgeStatus(t, "", "disconnected")
	manager := newReaderManager()
	manager.readerName = fixtureBLEReader
	manager.connectTime = time.Now().Add(-bleConnectGracePeriod)
	manager.generation = 13
	stops := 0
	monitor := &readerHotplugMonitor{
		manager:     manager,
		listReaders: manager.hotplugInventory,
		stopSession: func(generation uint64) {
			if generation != 13 {
				t.Errorf("removed generation = %d, want 13", generation)
			}
			stops++
		},
		required: 2,
	}

	monitor.poll(context.Background())
	monitor.poll(context.Background())
	if status := manager.Status(); status.Connected || stops != 1 {
		t.Fatalf("confirmed BLE disconnect did not retire session: status=%+v stops=%d", status, stops)
	}
	if got := requests.Load(); got != 2 {
		t.Fatalf("disconnected bridge status calls = %d, want 2", got)
	}
}

func TestReaderReconcileKeepsBLESelectionWhenBridgeIsUnavailable(t *testing.T) {
	t.Setenv("PROLETARIUS_BLE_SOCKET", "")
	manager := newReaderManager()
	manager.readerName = fixtureBLEReader
	manager.connectTime = time.Now().Add(-bleConnectGracePeriod)
	manager.generation = 9
	application := &app{readerMgr: manager}

	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/api/internal/reader/reconcile",
		strings.NewReader(`{"readers":[]}`))
	application.readerReconcile(recorder, request)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	var response readerListResponse
	if err := json.NewDecoder(recorder.Body).Decode(&response); err != nil {
		t.Fatal(err)
	}
	if response.Selected != fixtureBLEReader || len(response.Readers) != 1 || response.Readers[0] != fixtureBLEReader {
		t.Fatalf("unavailable bridge should remain indeterminate, got %+v", response)
	}
	if status := manager.Status(); !status.Connected || status.ReaderName != fixtureBLEReader || manager.generation != 9 {
		t.Fatalf("read-only BLE observation changed Go session: %+v", status)
	}
}

func TestReaderHotplugMonitorDebouncesUSBAbsenceBeforeStoppingSession(t *testing.T) {
	manager := newReaderManager()
	manager.readerName = "Reader A"
	manager.generation = 4
	stops := 0
	monitor := &readerHotplugMonitor{
		manager:     manager,
		listReaders: func(context.Context) ([]string, error) { return []string{"Reader B"}, nil },
		stopSession: func(generation uint64) {
			if generation != 4 {
				t.Errorf("removed generation = %d, want 4", generation)
			}
			stops++
		},
		required: 2,
	}

	monitor.poll(context.Background())
	if !manager.Status().Connected || stops != 0 {
		t.Fatalf("first absent sample changed session: connected=%t stops=%d", manager.Status().Connected, stops)
	}
	monitor.poll(context.Background())
	if manager.Status().Connected || stops != 1 {
		t.Fatalf("second absent sample did not retire session: connected=%t stops=%d", manager.Status().Connected, stops)
	}
	monitor.poll(context.Background())
	if stops != 1 {
		t.Fatalf("retired session stopped more than once: %d", stops)
	}
}

func TestReaderHotplugDoesNotDisconnectWhenCardOperationStartsAfterObservation(t *testing.T) {
	manager := newReaderManager()
	manager.readerName = "Reader A"
	manager.generation = 19
	missing := readerPresenceObservation{selected: "Reader A", presence: readerPresenceAbsent}

	// Model the window between observing absence and applying it: a card
	// operation acquired authMu after the inventory probe completed.
	manager.authMu.Lock()
	if _, removed := manager.applyHotplugObservation(missing, 1); removed {
		t.Fatal("hotplug monitor disconnected during an active card operation")
	}
	manager.authMu.Unlock()
	if status := manager.Status(); !status.Connected || status.Generation != 19 {
		t.Fatalf("active card operation lost reader: %+v", status)
	}
	if manager.hotplugAbsences != 0 {
		t.Fatalf("stale absence retained after active operation: %d", manager.hotplugAbsences)
	}
	if generation, removed := manager.applyHotplugObservation(missing, 1); !removed || generation != 19 {
		t.Fatalf("confirmed idle removal = (%d, %t), want (19, true)", generation, removed)
	}
}

func TestReaderHotplugMonitorResetsDebounceAfterPresenceReturns(t *testing.T) {
	manager := newReaderManager()
	manager.readerName = "Reader A"
	samples := [][]string{{"Reader B"}, {"Reader A"}, {"Reader B"}, {"Reader B"}}
	next := 0
	stops := 0
	monitor := &readerHotplugMonitor{
		manager: manager,
		listReaders: func(context.Context) ([]string, error) {
			sample := samples[next]
			next++
			return sample, nil
		},
		stopSession: func(uint64) { stops++ },
		required:    2,
	}

	monitor.poll(context.Background())
	monitor.poll(context.Background())
	monitor.poll(context.Background())
	if !manager.Status().Connected || stops != 0 {
		t.Fatalf("intermittent presence did not reset debounce: connected=%t stops=%d", manager.Status().Connected, stops)
	}
	monitor.poll(context.Background())
	if manager.Status().Connected || stops != 1 {
		t.Fatalf("second consecutive post-reset absence did not retire session: connected=%t stops=%d", manager.Status().Connected, stops)
	}
}

func TestReaderHotplugMonitorDoesNotCountInventoryErrors(t *testing.T) {
	manager := newReaderManager()
	manager.readerName = "Reader A"
	samples := []struct {
		readers []string
		err     error
	}{
		{err: errors.New("fixture inventory unavailable")},
		{readers: []string{"Reader B"}},
		{readers: []string{"Reader B"}},
	}
	next := 0
	stops := 0
	monitor := &readerHotplugMonitor{
		manager: manager,
		listReaders: func(context.Context) ([]string, error) {
			sample := samples[next]
			next++
			return sample.readers, sample.err
		},
		stopSession: func(uint64) { stops++ },
		required:    2,
	}

	monitor.poll(context.Background())
	monitor.poll(context.Background())
	if !manager.Status().Connected || stops != 0 {
		t.Fatalf("scanner error counted as unplug: connected=%t stops=%d", manager.Status().Connected, stops)
	}
	monitor.poll(context.Background())
	if manager.Status().Connected || stops != 1 {
		t.Fatalf("two successful absent samples did not retire session: connected=%t stops=%d", manager.Status().Connected, stops)
	}
}

func TestReaderHotplugMonitorDoesNotEnumerateWithoutSelectedReader(t *testing.T) {
	called := false
	monitor := &readerHotplugMonitor{
		manager: newReaderManager(),
		listReaders: func(context.Context) ([]string, error) {
			called = true
			return nil, nil
		},
	}

	monitor.poll(context.Background())
	if called {
		t.Fatal("monitor enumerated hardware without a selected reader")
	}
}

func TestReaderHotplugMonitorUsesBoundedInventoryContext(t *testing.T) {
	manager := newReaderManager()
	manager.readerName = "Reader A"
	monitor := &readerHotplugMonitor{
		manager: manager,
		listReaders: func(ctx context.Context) ([]string, error) {
			deadline, ok := ctx.Deadline()
			if !ok {
				t.Fatal("hotplug inventory context has no deadline")
			}
			if remaining := time.Until(deadline); remaining > readerHotplugScanTimeout || remaining <= 0 {
				t.Fatalf("hotplug inventory deadline remaining = %s", remaining)
			}
			return []string{"Reader A"}, nil
		},
	}

	monitor.poll(context.Background())
}

func TestHotplugInventoryDefersWhileCardOperationOwnsAuthLock(t *testing.T) {
	manager := newReaderManager()
	manager.readerName = "Reader A"
	manager.authMu.Lock()
	defer manager.authMu.Unlock()

	_, err := manager.hotplugInventory(context.Background())
	if !errors.Is(err, errReaderBusy) {
		t.Fatalf("hotplug inventory error = %v, want reader busy", err)
	}
}

func TestReaderHotplugMonitorDoesNotRunConcurrentPolls(t *testing.T) {
	manager := newReaderManager()
	manager.readerName = "Reader A"
	entered := make(chan struct{})
	release := make(chan struct{})
	completed := make(chan struct{})
	calls := 0
	monitor := &readerHotplugMonitor{
		manager: manager,
		listReaders: func(context.Context) ([]string, error) {
			calls++
			if calls == 1 {
				close(entered)
				<-release
			}
			return []string{"Reader A"}, nil
		},
	}

	go func() {
		monitor.poll(context.Background())
		close(completed)
	}()
	<-entered
	secondDone := make(chan struct{})
	go func() {
		monitor.poll(context.Background())
		close(secondDone)
	}()
	select {
	case <-secondDone:
		t.Fatal("second monitor poll ran before the first completed")
	case <-time.After(25 * time.Millisecond):
	}
	if calls != 1 {
		t.Fatalf("concurrent inventory calls = %d, want 1", calls)
	}
	close(release)
	<-completed
	<-secondDone
}

func TestReaderHotplugMonitorStopsOnContextCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	monitor := &readerHotplugMonitor{manager: newReaderManager()}
	go func() {
		monitor.run(ctx, time.Hour)
		close(done)
	}()
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("hotplug monitor did not stop after context cancellation")
	}
}
