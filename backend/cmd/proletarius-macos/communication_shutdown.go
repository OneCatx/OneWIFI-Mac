package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"
)

// IMS close (5s), the existing radio restore retries (about 40s), and the
// existing two session-exit waits (12s) fit inside this shared bound. The Rust
// supervisor grants 70s; an idle app exits immediately, not after this timer.
const communicationShutdownTimeout = 65 * time.Second

type communicationShutdownTask struct {
	name string
	run  func(context.Context) error
}

func runCommunicationShutdown(ctx context.Context, tasks ...communicationShutdownTask) error {
	results := make(chan error, len(tasks))
	for _, task := range tasks {
		go func() {
			err := task.run(ctx)
			if err != nil {
				err = fmt.Errorf("%s: %w", task.name, err)
			}
			results <- err
		}()
	}
	var failures []error
	for range tasks {
		select {
		case err := <-results:
			if err != nil {
				failures = append(failures, err)
			}
		case <-ctx.Done():
			return errors.Join(append(failures, ctx.Err())...)
		}
	}
	return errors.Join(failures...)
}

func (a *app) shutdownCommunications(ctx context.Context, server *http.Server) error {
	return runCommunicationShutdown(ctx,
		communicationShutdownTask{"HTTP", server.Shutdown},
		communicationShutdownTask{"VoWiFi", stopVoWiFiForShutdown},
		communicationShutdownTask{"multi standby", a.stopMultiStandbyForShutdown},
		communicationShutdownTask{"module audio", func(context.Context) error {
			a.moduleVoiceMu.Lock()
			active := a.moduleVoiceRoute
			a.moduleVoiceMu.Unlock()
			if active {
				return a.stopModuleVoiceRoute(a.runtimeDirectory())
			}
			return nil
		}},
	)
}

func stopVoWiFiForShutdown(ctx context.Context) error {
	vowifiSessionMu.Lock()
	vowifiShuttingDown = true
	session, startup, retiring := activeSession, startingVoWiFi, retiringSession
	var startupLease *moduleRadioLease
	if startup != nil {
		startupLease = startup.radioLease
	}
	vowifiSessionMu.Unlock()
	// Cancels reconnect intents and active/startup work, then uses the same
	// idempotent IMS and radio cleanup as the normal Stop operation.
	stopVoWiFi()
	if retiring != nil {
		retiring.cleanupResources()
	}
	if startupLease != nil {
		if err := startupLease.restore(); err != nil {
			return err
		}
	}
	for _, stopped := range []*vowifiSession{session, retiring} {
		if stopped != nil {
			stopped.imsOpMu.Lock()
			err := stopped.radioRestoreErr
			stopped.imsOpMu.Unlock()
			if err != nil {
				return fmt.Errorf("radio policy restore: %w", err)
			}
		}
	}
	ticker := time.NewTicker(20 * time.Millisecond)
	defer ticker.Stop()
	for {
		vowifiSessionMu.Lock()
		pending := startingVoWiFi != nil || retiringSession != nil
		vowifiSessionMu.Unlock()
		if !pending {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}
