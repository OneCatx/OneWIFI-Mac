package main

import (
	"context"
	"time"

	"proletarius.local/core/internal/readerapp"
)

// These read-only adapters intentionally keep cellular and VoLTE as unknown:
// a standalone BLE/PCSC reader has no radio. The DJI modem path owns those
// states independently and must never be inferred from Wi-Fi Calling.
type readerNetworkService struct{ reader *readerManager }
type readerIMSService struct{}

var _ readerapp.NetworkService = (*readerNetworkService)(nil)
var _ readerapp.IMSService = (*readerIMSService)(nil)

func (s *readerNetworkService) Snapshot(ctx context.Context) (readerapp.NetworkSnapshot, error) {
	if err := ctx.Err(); err != nil {
		return readerapp.NetworkSnapshot{}, err
	}
	state := readerapp.NetworkSnapshot{
		SIM: "absent", Cellular: "unknown", VoLTE: "unknown", VoWiFi: "disconnected",
	}
	if s.reader.Status().HasCard {
		state.SIM = "present"
	}
	if status := getVoWiFiStatus(); status != nil {
		state.VoWiFi = readerapp.RegistrationState(status.TunnelState)
	}
	return state, nil
}

func (s *readerIMSService) Snapshot(ctx context.Context) (readerapp.IMSSnapshot, error) {
	if err := ctx.Err(); err != nil {
		return readerapp.IMSSnapshot{}, err
	}
	status := getVoWiFiStatus()
	if status == nil {
		return readerapp.IMSSnapshot{Registration: "idle", UpdatedAt: time.Now().UTC()}, nil
	}
	return readerapp.IMSSnapshot{Registration: readerapp.RegistrationState(status.IMSState), UpdatedAt: time.Now().UTC()}, nil
}
