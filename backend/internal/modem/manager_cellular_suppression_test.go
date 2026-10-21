package modem

import (
	"errors"
	"testing"
)

func TestSetCellularStatusSuppressedClearsRFMetadata(t *testing.T) {
	manager := &Manager{
		operator:      "carrier",
		signalDBM:     -71,
		signalRSRP:    -93,
		signalRSRQ:    -11,
		regStatus:     5,
		regStatusText: "roaming",
		lac:           "1234",
		cellID:        "5678",
		imsStatus:     1,
		networkMode:   "LTE",
		networkDuplex: "FDD",
	}
	manager.SetCellularStatusSuppressed(true)
	status := manager.GetFullStatus()
	if !manager.cellularStatusIsSuppressed() {
		t.Fatal("cellular status queries were not suppressed")
	}
	if status.Operator != "" || status.SignalDBM != -999 || status.SignalRSRP != 0 || status.SignalRSRQ != 0 {
		t.Fatalf("radio measurements were retained while suppressed: %#v", status)
	}
	if status.RegStatus != 0 || status.RegStatusText != "飞行模式 · 蜂窝射频关闭" || status.NetworkMode != "飞行模式" {
		t.Fatalf("flight-mode state was not reflected: %#v", status)
	}
	if status.LAC != "" || status.CellID != "" || status.IMSStatus != 0 || status.NetworkDuplex != "" {
		t.Fatalf("cellular attachment metadata was retained: %#v", status)
	}
}

func TestCellularProbeAndReattachCommandsRejectedWhileSuppressed(t *testing.T) {
	manager := &Manager{}
	manager.SetCellularStatusSuppressed(true)

	if _, _, err := manager.QueryCSQ(); !errors.Is(err, ErrCellularProbeSuppressed) {
		t.Fatalf("QueryCSQ() error = %v, want suppressed-probe error", err)
	}
	if _, err := manager.QueryOperator(); !errors.Is(err, ErrCellularProbeSuppressed) {
		t.Fatalf("QueryOperator() error = %v, want suppressed-probe error", err)
	}
	if _, _, _, _, err := manager.QueryRegistration(); !errors.Is(err, ErrCellularProbeSuppressed) {
		t.Fatalf("QueryRegistration() error = %v, want suppressed-probe error", err)
	}
	if _, _, err := manager.QueryServingCellLTE(); !errors.Is(err, ErrCellularProbeSuppressed) {
		t.Fatalf("QueryServingCellLTE() error = %v, want suppressed-probe error", err)
	}
	if _, _, _, _, err := manager.QueryNetworkRadio(); !errors.Is(err, ErrCellularProbeSuppressed) {
		t.Fatalf("QueryNetworkRadio() error = %v, want suppressed-probe error", err)
	}
	if _, _, err := manager.QueryNetworkModeFallbackAndDuplex(); !errors.Is(err, ErrCellularProbeSuppressed) {
		t.Fatalf("QueryNetworkModeFallbackAndDuplex() error = %v, want suppressed-probe error", err)
	}
	if _, err := manager.CheckSignal(); !errors.Is(err, ErrCellularProbeSuppressed) {
		t.Fatalf("CheckSignal() error = %v, want suppressed-probe error", err)
	}
	if err := manager.SetAttach(true); !errors.Is(err, ErrCellularProbeSuppressed) {
		t.Fatalf("SetAttach(true) error = %v, want suppressed-probe error", err)
	}
	if err := manager.Rotate(); !errors.Is(err, ErrCellularProbeSuppressed) {
		t.Fatalf("Rotate() error = %v, want suppressed-probe error", err)
	}
}
