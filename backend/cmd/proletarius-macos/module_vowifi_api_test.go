package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestModuleVoWiFiRejectsIneffectiveCustomEPDG(t *testing.T) {
	a := &app{}
	request := httptest.NewRequest(http.MethodPost, "/api/vowifi", strings.NewReader(
		`{"enable":true,"wifi_calling_mode":2,"epdg_address":"epdg.example.com"}`))
	response := httptest.NewRecorder()
	a.setModuleVoWiFi(response, request)
	if response.Code != http.StatusBadRequest || !strings.Contains(response.Body.String(), "module_epdg_unsupported") {
		t.Fatalf("custom ePDG was not rejected: status=%d body=%s", response.Code, response.Body.String())
	}
}

func TestModuleVoWiFiStopCancelsPendingStart(t *testing.T) {
	cancelled := false
	a := &app{moduleVoWiFiOp: &moduleVoWiFiOperation{
		Enable: true,
		cancel: func() { cancelled = true },
	}}
	request := httptest.NewRequest(http.MethodPost, "/api/vowifi", strings.NewReader(`{"enable":false}`))
	response := httptest.NewRecorder()
	a.setModuleVoWiFi(response, request)

	if response.Code != http.StatusAccepted || !strings.Contains(response.Body.String(), `"cancelling":true`) {
		t.Fatalf("stop should be accepted as a cancellation: status=%d body=%s", response.Code, response.Body.String())
	}
	if !cancelled || !a.moduleVoWiFiOp.CancelRequested {
		t.Fatalf("pending startup was not cancelled: cancelled=%t op=%+v", cancelled, a.moduleVoWiFiOp)
	}
}

func TestModuleSIMLockHonorsCancellationWhileWaiting(t *testing.T) {
	var mu sync.Mutex
	mu.Lock()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- lockModuleSIMContext(ctx, &mu) }()
	cancel()
	select {
	case err := <-done:
		if err != context.Canceled {
			t.Fatalf("lockModuleSIMContext() error = %v, want context.Canceled", err)
		}
	case <-time.After(time.Second):
		t.Fatal("lockModuleSIMContext() did not return after cancellation")
	}
	mu.Unlock()
}

func TestModuleVoWiFiWithoutSessionReportsDisabled(t *testing.T) {
	status := moduleVoWiFiStatusFromSession(nil, 1)
	if status.State != "disabled" || status.VoWiFiActive || status.IMSRegistered {
		t.Fatalf("missing module session must not appear active: %+v", status)
	}
}

func TestModuleVoWiFiRequiresAirplaneModeForActiveStatus(t *testing.T) {
	session := &vowifiSession{
		sourceKind:  "module",
		tunnelState: tunnelConnected,
		imsState:    imsRegistered,
	}
	cellular := moduleVoWiFiStatusFromSession(session, 1)
	if cellular.VoWiFiActive || cellular.IMSRegistered || cellular.State != "failed" {
		t.Fatalf("cellular RF must never be presented as host-routed VoWiFi: %+v", cellular)
	}
	wifi := moduleVoWiFiStatusFromSession(session, 4)
	if !wifi.VoWiFiActive || !wifi.IMSRegistered || wifi.WiFiCallingMode != 1 {
		t.Fatalf("CFUN=4 host-routed IMS should be active: %+v", wifi)
	}
}

func TestModuleVoWiFiStatusCarriesStartupStageToUI(t *testing.T) {
	session := &vowifiSession{
		sourceKind:   "module",
		tunnelState:  tunnelConnected,
		imsState:     imsRegistering,
		progressStep: progressIMS,
		progressDesc: "正在通过隧道注册 IMS...",
	}
	status := moduleVoWiFiStatusFromSession(session, 4)
	if status.ProgressStep != progressIMS || status.ProgressDesc != "正在通过隧道注册 IMS..." {
		t.Fatalf("module status lost the current startup stage: %+v", status)
	}
}

func TestModuleVoWiFiStatusCarriesOwningSIMIdentity(t *testing.T) {
	session := &vowifiSession{
		sourceKind: "module",
		moduleSIM:  &moduleVoWiFiSIM{info: &usimInfo{ICCID: "8988212345678901234"}},
	}
	status := moduleVoWiFiStatusFromSession(session, 4)
	if status.SIMICCID != "8988212345678901234" {
		t.Fatalf("module status ICCID=%q, want session SIM ICCID", status.SIMICCID)
	}
}

func TestModuleVoWiFiStatusSnapshotDuringIMSStartupAllowsMissingResult(t *testing.T) {
	session := &vowifiSession{
		sourceKind: "module",
		moduleSIM:  &moduleVoWiFiSIM{info: &usimInfo{ICCID: "8988212345678901234"}},
		// The active session is published before IKE and IMS registration; the
		// IMS result is intentionally nil while the dashboard polls /api/status.
	}
	previousActive, previousStarting, previousRetiring := activeSession, startingVoWiFi, retiringSession
	vowifiSessionMu.Lock()
	activeSession, startingVoWiFi, retiringSession = session, nil, nil
	vowifiSessionMu.Unlock()
	t.Cleanup(func() {
		vowifiSessionMu.Lock()
		activeSession, startingVoWiFi, retiringSession = previousActive, previousStarting, previousRetiring
		vowifiSessionMu.Unlock()
	})

	status, held := (&app{}).moduleVoWiFiStatusSnapshot()
	if !held {
		t.Fatal("module session should own the status snapshot during IMS startup")
	}
	if status.ICCID != "8988212345678901234" {
		t.Fatalf("startup snapshot lost active SIM identity: %+v", status)
	}
}

func TestDJISMSUsesModuleIMSWhenModuleVoWiFiSessionOwnsRadio(t *testing.T) {
	session := &vowifiSession{sourceKind: "module"}
	vowifiSessionMu.Lock()
	previous := activeSession
	activeSession = session
	vowifiSessionMu.Unlock()
	t.Cleanup(func() {
		vowifiSessionMu.Lock()
		activeSession = previous
		vowifiSessionMu.Unlock()
	})

	_, err := (&app{}).sendDJISMS("+447700900123", "test")
	if err == nil || !strings.Contains(err.Error(), "SIM 会话不可用") {
		t.Fatalf("DJI SMS must stay on IMS instead of falling through to cellular AT: %v", err)
	}
}

func TestModuleVoWiFiOwnsRadioThroughoutSessionLifecycle(t *testing.T) {
	startup := &vowifiStartup{sourceKind: "module"}
	session := &vowifiSession{sourceKind: "module"}
	previousActive, previousStarting, previousRetiring := activeSession, startingVoWiFi, retiringSession
	t.Cleanup(func() {
		vowifiSessionMu.Lock()
		activeSession, startingVoWiFi, retiringSession = previousActive, previousStarting, previousRetiring
		vowifiSessionMu.Unlock()
	})
	appInstance := &app{}
	for _, stage := range []string{"starting", "active", "retiring"} {
		vowifiSessionMu.Lock()
		activeSession, startingVoWiFi, retiringSession = nil, nil, nil
		switch stage {
		case "starting":
			startingVoWiFi = startup
		case "active":
			activeSession = session
		case "retiring":
			retiringSession = session
		}
		vowifiSessionMu.Unlock()
		if !appInstance.moduleVoWiFiOwnsRadio() {
			t.Fatalf("module VoWiFi did not own the AT/SIM channel in %s stage", stage)
		}
	}
}
