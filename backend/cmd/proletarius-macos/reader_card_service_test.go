package main

import (
	"context"
	"errors"
	"testing"
	"time"

	"proletarius.local/core/internal/esim"
	"proletarius.local/core/internal/readerapp"
)

type fakeReaderCardBackend struct {
	overview   *esim.EsimOverview
	refreshErr error
	refreshFn  func() error
	overviewFn func() (*esim.EsimOverview, error)
	switchFn   func(context.Context, string, string) (esim.SwitchProfileResult, error)
	switchCall int
}

func (f *fakeReaderCardBackend) RefreshOverview() error {
	if f.refreshFn != nil {
		return f.refreshFn()
	}
	return f.refreshErr
}

func (f *fakeReaderCardBackend) GetEsimOverview() (*esim.EsimOverview, error) {
	if f.overviewFn != nil {
		return f.overviewFn()
	}
	return f.overview, nil
}

func (f *fakeReaderCardBackend) SwitchProfileWithResult(ctx context.Context, iccid, aid string) (esim.SwitchProfileResult, error) {
	f.switchCall++
	if f.switchFn == nil {
		return esim.SwitchProfileResult{}, errors.New("unexpected switch")
	}
	return f.switchFn(ctx, iccid, aid)
}

func selectedReaderManager(name string, generation uint64) *readerManager {
	manager := newReaderManager()
	manager.readerName = name
	manager.generation = generation
	manager.activeBLEGeneration.Store(generation)
	return manager
}

func TestReaderCardServiceFailsClosedWhenDisconnected(t *testing.T) {
	service := &readerCardService{manager: newReaderManager()}
	if present, err := service.Detect(context.Background(), readerapp.ReaderID("ble:missing")); err == nil || present {
		t.Fatalf("detect present=%v err=%v", present, err)
	}
	if _, err := service.Info(context.Background(), ""); err == nil {
		t.Fatal("disconnected reader returned card info")
	}
	if _, err := service.SwitchProfile(context.Background(), "", "123", ""); err == nil {
		t.Fatal("disconnected reader accepted profile switch")
	}
}

func TestReaderCardServiceUsesOneReaderGenerationForProfileReads(t *testing.T) {
	manager := selectedReaderManager("BLE:reader-a", 9)
	backend := &fakeReaderCardBackend{overview: &esim.EsimOverview{Profiles: []esim.EUICCProfiles{{
		Profiles: []esim.ProfileItem{
			{ICCID: "8988000000000000001", Name: "Travel", State: 0},
			{ICCID: "8988000000000000002", Name: "Home", State: 1},
		},
	}}}}
	service := &readerCardService{
		manager: manager,
		backendForReader: func(reader string) (readerCardBackend, bool) {
			return backend, reader == "BLE:reader-a"
		},
	}

	profiles, err := service.Profiles(context.Background(), "BLE:reader-a")
	if err != nil {
		t.Fatal(err)
	}
	if len(profiles) != 2 || profiles[0].Name != "Travel" || profiles[1].ICCID != "8988000000000000002" || !profiles[1].Enabled {
		t.Fatalf("profiles=%+v", profiles)
	}
	active, err := service.ActiveProfile(context.Background(), "BLE:reader-a")
	if err != nil || active.ICCID != "8988000000000000002" {
		t.Fatalf("active=%+v err=%v", active, err)
	}
	if _, err := service.Profiles(context.Background(), "BLE:reader-b"); err == nil {
		t.Fatal("profile read accepted a different reader identity")
	}
}

func TestReaderCardServiceRefreshesAndReadsOverviewWithinLeases(t *testing.T) {
	manager := selectedReaderManager("USB Reader A", 3)
	backend := &fakeReaderCardBackend{
		overview: &esim.EsimOverview{},
		refreshFn: func() error {
			if manager.authMu.TryLock() {
				manager.authMu.Unlock()
				return errors.New("overview refresh ran without card lease")
			}
			return nil
		},
		overviewFn: func() (*esim.EsimOverview, error) {
			if manager.authMu.TryLock() {
				manager.authMu.Unlock()
				return nil, errors.New("overview read ran without card lease")
			}
			return &esim.EsimOverview{}, nil
		},
	}
	service := &readerCardService{
		manager: manager,
		backendForReader: func(reader string) (readerCardBackend, bool) {
			return backend, reader == "USB Reader A"
		},
	}
	if err := service.RefreshOverview(context.Background(), "USB Reader A"); err != nil {
		t.Fatal(err)
	}
	if _, err := service.Overview(context.Background(), "USB Reader A"); err != nil {
		t.Fatal(err)
	}
	backend.refreshFn = nil
	backend.refreshErr = errors.New("card refresh failed")
	if err := service.RefreshOverview(context.Background(), "USB Reader A"); err == nil || err.Error() != "card refresh failed" {
		t.Fatalf("refresh err=%v", err)
	}
	if !manager.authMu.TryLock() {
		t.Fatal("card-operation lease was not released after refresh error")
	}
	manager.authMu.Unlock()
}

func TestReaderCardServiceRejectsOverviewFromReplacedReader(t *testing.T) {
	manager := selectedReaderManager("BLE:reader-a", 21)
	backend := &fakeReaderCardBackend{overviewFn: func() (*esim.EsimOverview, error) {
		manager.mu.Lock()
		manager.readerName = "BLE:reader-b"
		manager.generation++
		manager.mu.Unlock()
		return &esim.EsimOverview{}, nil
	}}
	service := &readerCardService{
		manager:          manager,
		backendForReader: func(string) (readerCardBackend, bool) { return backend, true },
	}
	if _, err := service.Overview(context.Background(), "BLE:reader-a"); err == nil {
		t.Fatal("overview returned after reader generation changed")
	}
	if !manager.authMu.TryLock() {
		t.Fatal("card-operation lease was not released after overview identity mismatch")
	}
	manager.authMu.Unlock()
}

func TestReaderCardServiceProfileSwitchUsesOrchestratedAPDUBackend(t *testing.T) {
	manager := selectedReaderManager("BLE:reader-a", 12)
	backend := &fakeReaderCardBackend{switchFn: func(_ context.Context, iccid, aid string) (esim.SwitchProfileResult, error) {
		if iccid != "8988000000000000002" || aid != "A0000005591010FFFFFFFF8900000100" {
			t.Fatalf("switch iccid=%q aid=%q", iccid, aid)
		}
		return esim.SwitchProfileResult{TargetICCID: iccid, SwitchAccepted: true, Phase: esim.SwitchPhaseAPDUSwitching}, nil
	}}
	service := &readerCardService{
		manager: manager,
		backendForReader: func(reader string) (readerCardBackend, bool) {
			return backend, reader == "BLE:reader-a"
		},
	}

	result, err := service.SwitchProfile(context.Background(), "BLE:reader-a", " 8988000000000000002 ", " A0000005591010FFFFFFFF8900000100 ")
	if err != nil {
		t.Fatal(err)
	}
	if !result.SwitchAccepted || result.TargetICCID != "8988000000000000002" || result.Phase != string(esim.SwitchPhaseAPDUSwitching) {
		t.Fatalf("result=%+v", result)
	}
	if backend.switchCall != 1 {
		t.Fatalf("switch calls=%d want 1", backend.switchCall)
	}
	if _, err := service.SwitchProfile(context.Background(), "BLE:reader-b", "8988000000000000002", ""); err == nil {
		t.Fatal("switch accepted a stale/different reader identity")
	}
	if backend.switchCall != 1 {
		t.Fatalf("mismatched reader reached APDU backend: calls=%d", backend.switchCall)
	}
}

func TestReaderCardServiceRejectsSwitchWhenReaderChangesDuringAPDU(t *testing.T) {
	manager := selectedReaderManager("BLE:reader-a", 12)
	backend := &fakeReaderCardBackend{switchFn: func(_ context.Context, iccid, _ string) (esim.SwitchProfileResult, error) {
		manager.mu.Lock()
		manager.readerName = "BLE:reader-b"
		manager.generation++
		manager.mu.Unlock()
		return esim.SwitchProfileResult{TargetICCID: iccid, SwitchAccepted: true, Phase: esim.SwitchPhaseAPDUSwitching}, nil
	}}
	service := &readerCardService{
		manager:          manager,
		backendForReader: func(string) (readerCardBackend, bool) { return backend, true },
	}

	if _, err := service.SwitchProfile(context.Background(), "BLE:reader-a", "8988000000000000002", ""); err == nil {
		t.Fatal("switch succeeded after reader generation changed during APDU")
	}
	if backend.switchCall != 1 {
		t.Fatalf("switch calls=%d want one APDU attempt", backend.switchCall)
	}
	if !manager.authMu.TryLock() {
		t.Fatal("card-operation lease was not released after identity mismatch")
	}
	manager.authMu.Unlock()
}

func TestReaderCardServiceOperationWaitHonorsContextCancellation(t *testing.T) {
	manager := selectedReaderManager("USB Reader A", 4)
	backend := &fakeReaderCardBackend{switchFn: func(context.Context, string, string) (esim.SwitchProfileResult, error) {
		t.Fatal("cancelled switch reached APDU backend")
		return esim.SwitchProfileResult{}, nil
	}}
	service := &readerCardService{
		manager:          manager,
		backendForReader: func(string) (readerCardBackend, bool) { return backend, true },
	}

	manager.authMu.Lock()
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Millisecond)
	defer cancel()
	started := time.Now()
	_, err := service.SwitchProfile(ctx, "USB Reader A", "8988000000000000002", "")
	manager.authMu.Unlock()
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("switch err=%v, want context deadline", err)
	}
	if time.Since(started) > time.Second {
		t.Fatal("cancelled reader operation did not stop waiting promptly")
	}
}

func TestReaderNetworkServiceDoesNotInferCellularFromReader(t *testing.T) {
	service := &readerNetworkService{reader: newReaderManager()}
	snapshot, err := service.Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.SIM != "absent" || snapshot.Cellular != "unknown" || snapshot.VoLTE != "unknown" {
		t.Fatalf("snapshot=%+v", snapshot)
	}
}
