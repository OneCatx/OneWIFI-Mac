package main

import (
	"context"
	"errors"
	"strings"

	"proletarius.local/core/internal/esim"
	"proletarius.local/core/internal/readerapp"
)

// readerCardBackend keeps this service independent of USB/PCSC and BLE. Both
// transports reach the same eSIM manager through readerManager's channel
// factory; the service owns card-operation serialization and identity fencing.
type readerCardBackend interface {
	RefreshOverview() error
	GetEsimOverview() (*esim.EsimOverview, error)
	SwitchProfileWithResult(context.Context, string, string) (esim.SwitchProfileResult, error)
}

// readerCardService is the reader-facing application boundary. The HTTP layer
// retains orchestration (VoWiFi stop, operation progress, notifications and
// post-switch identity refresh), while all card reads and the mutating APDU
// command pass through this service.
type readerCardService struct {
	manager *readerManager

	// backendForReader is an optional deterministic seam for service tests.
	// Production resolves the backend from the currently leased reader.
	backendForReader func(string) (readerCardBackend, bool)
}

var _ readerapp.CardService = (*readerCardService)(nil)

func (s *readerCardService) snapshot(id readerapp.ReaderID) (readerStatus, error) {
	if s == nil || s.manager == nil {
		return readerStatus{}, errors.New("reader service unavailable")
	}
	status := s.manager.Status()
	if !status.Connected {
		return readerStatus{}, errors.New("reader not connected")
	}
	if id != "" && string(id) != status.ReaderName {
		return readerStatus{}, errors.New("selected reader changed")
	}
	return status, nil
}

// leaseSelected verifies identity both before and after acquiring the card
// transaction lock. This closes the gap where a reader could be replaced
// between selection validation and APDU dispatch. The post-lock status read is
// safe because reader replacement paths use TryLock rather than waiting on
// authMu while holding readerManager.mu.
func (s *readerCardService) leaseSelected(
	ctx context.Context,
	id readerapp.ReaderID,
) (readerStatus, func(), error) {
	initial, err := s.snapshot(id)
	if err != nil {
		return readerStatus{}, nil, err
	}
	release, err := s.manager.beginCardOperationContext(ctx)
	if err != nil {
		return readerStatus{}, nil, err
	}
	current := s.manager.Status()
	if !current.Connected || current.ReaderName != initial.ReaderName || current.Generation != initial.Generation {
		release()
		return readerStatus{}, nil, errors.New("reader identity changed before card operation")
	}
	return current, release, nil
}

func (s *readerCardService) backend(reader string) (readerCardBackend, error) {
	if s.backendForReader != nil {
		backend, ok := s.backendForReader(reader)
		if !ok || backend == nil {
			return nil, errors.New("eUICC not available")
		}
		return backend, nil
	}
	backend, ok := s.manager.ESIMManager()
	if !ok || backend == nil {
		return nil, errors.New("eUICC not available")
	}
	return backend, nil
}

func (s *readerCardService) Detect(ctx context.Context, id readerapp.ReaderID) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	status, release, err := s.leaseSelected(ctx, id)
	if err != nil {
		return false, err
	}
	defer release()
	return status.HasCard, ctx.Err()
}

func (s *readerCardService) Info(ctx context.Context, id readerapp.ReaderID) (readerapp.CardInfo, error) {
	if err := ctx.Err(); err != nil {
		return readerapp.CardInfo{}, err
	}
	status, release, err := s.leaseSelected(ctx, id)
	if err != nil {
		return readerapp.CardInfo{}, err
	}
	defer release()
	current, sim := s.manager.statusAndUSIMInfo()
	if !current.Connected || current.ReaderName != status.ReaderName || current.Generation != status.Generation {
		return readerapp.CardInfo{}, errors.New("reader disconnected during card inspection")
	}
	info := readerapp.CardInfo{Present: current.HasCard}
	if sim != nil {
		info.ICCID, info.IMSI, info.PhoneNumber = sim.ICCID, sim.IMSI, sim.PhoneNumber
	}
	if err := ctx.Err(); err != nil {
		return readerapp.CardInfo{}, err
	}
	return info, nil
}

func (s *readerCardService) Profiles(ctx context.Context, id readerapp.ReaderID) ([]readerapp.Profile, error) {
	overview, err := s.Overview(ctx, id)
	if err != nil {
		return nil, err
	}
	profiles := make([]readerapp.Profile, 0)
	if overview == nil {
		return profiles, nil
	}
	for _, group := range overview.Profiles {
		for _, profile := range group.Profiles {
			profiles = append(profiles, readerapp.Profile{
				ICCID: profile.ICCID, Name: profile.Name, Enabled: profile.State == 1,
			})
		}
	}
	return profiles, nil
}

// Overview returns one generation-fenced eUICC snapshot. It is intentionally a
// concrete adapter method because the public CardService exposes normalized
// card/Profile values, while the HTTP API needs eSIM-specific chip metadata.
func (s *readerCardService) Overview(ctx context.Context, id readerapp.ReaderID) (*esim.EsimOverview, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	status, release, err := s.leaseSelected(ctx, id)
	if err != nil {
		return nil, err
	}
	defer release()
	backend, err := s.backend(status.ReaderName)
	if err != nil {
		return nil, err
	}
	overview, err := backend.GetEsimOverview()
	if err != nil {
		return nil, err
	}
	if err := s.verifyLease(status); err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return overview, nil
}

// RefreshOverview refreshes the eSIM-specific snapshot under the same reader
// lease as ordinary reads, preventing refresh from racing with profile writes.
func (s *readerCardService) RefreshOverview(ctx context.Context, id readerapp.ReaderID) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	status, release, err := s.leaseSelected(ctx, id)
	if err != nil {
		return err
	}
	defer release()
	backend, err := s.backend(status.ReaderName)
	if err != nil {
		return err
	}
	if err := backend.RefreshOverview(); err != nil {
		return err
	}
	if err := s.verifyLease(status); err != nil {
		return err
	}
	return ctx.Err()
}

func (s *readerCardService) ActiveProfile(ctx context.Context, id readerapp.ReaderID) (readerapp.Profile, error) {
	profiles, err := s.Profiles(ctx, id)
	if err != nil {
		return readerapp.Profile{}, err
	}
	for _, profile := range profiles {
		if profile.Enabled {
			return profile, nil
		}
	}
	return readerapp.Profile{}, errors.New("no active profile")
}

func (s *readerCardService) SwitchProfile(
	ctx context.Context,
	id readerapp.ReaderID,
	iccid string,
	aid string,
) (readerapp.ProfileSwitchResult, error) {
	if err := ctx.Err(); err != nil {
		return readerapp.ProfileSwitchResult{}, err
	}
	iccid = strings.TrimSpace(iccid)
	if iccid == "" {
		return readerapp.ProfileSwitchResult{}, errors.New("target ICCID required")
	}
	status, release, err := s.leaseSelected(ctx, id)
	if err != nil {
		return readerapp.ProfileSwitchResult{}, err
	}
	defer release()
	backend, err := s.backend(status.ReaderName)
	if err != nil {
		return readerapp.ProfileSwitchResult{}, err
	}
	result, err := backend.SwitchProfileWithResult(ctx, iccid, strings.TrimSpace(aid))
	if err != nil {
		return readerapp.ProfileSwitchResult{}, err
	}
	if err := s.verifyLease(status); err != nil {
		return readerapp.ProfileSwitchResult{}, err
	}
	return readerapp.ProfileSwitchResult{
		TargetICCID:    result.TargetICCID,
		Phase:          string(result.Phase),
		SwitchAccepted: result.SwitchAccepted,
	}, nil
}

func (s *readerCardService) verifyLease(expected readerStatus) error {
	current := s.manager.Status()
	if !current.Connected || current.ReaderName != expected.ReaderName || current.Generation != expected.Generation {
		return errors.New("reader identity changed during card operation")
	}
	return nil
}
