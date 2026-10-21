package main

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestCommunicationShutdownRunsIndependentCleanupAndReportsFailure(t *testing.T) {
	var calls atomic.Int32
	err := runCommunicationShutdown(context.Background(),
		communicationShutdownTask{"first", func(context.Context) error { calls.Add(1); return errors.New("fixture") }},
		communicationShutdownTask{"second", func(context.Context) error { calls.Add(1); return nil }},
	)
	if calls.Load() != 2 || err == nil || !strings.Contains(err.Error(), "first: fixture") {
		t.Fatalf("calls=%d err=%v", calls.Load(), err)
	}
}

func TestCommunicationShutdownUsesOneCancellableBudget(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	started, finished := make(chan struct{}), make(chan struct{})
	result := make(chan error, 1)
	go func() {
		result <- runCommunicationShutdown(ctx, communicationShutdownTask{"blocked", func(ctx context.Context) error {
			close(started)
			<-ctx.Done()
			close(finished)
			return ctx.Err()
		}})
	}()
	<-started
	cancel()
	select {
	case err := <-result:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("error=%v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("shutdown ignored cancellation")
	}
	<-finished
}

func TestShutdownStopsVoWiFiOnceWithoutHardware(t *testing.T) {
	vowifiSessionMu.Lock()
	previous, previousStartup, previousRetiring, previousIntent := activeSession, startingVoWiFi, retiringSession, vowifiIntentID
	previousShuttingDown := vowifiShuttingDown
	ctx, cancel := context.WithCancel(context.Background())
	var restores atomic.Int32
	session := &vowifiSession{cancel: cancel, runDone: make(chan struct{}), swuDone: make(chan struct{}), radioRestore: func() error { restores.Add(1); return nil }}
	activeSession, startingVoWiFi, retiringSession = session, nil, nil
	vowifiSessionMu.Unlock()
	defer func() {
		vowifiSessionMu.Lock()
		activeSession, startingVoWiFi, retiringSession, vowifiIntentID = previous, previousStartup, previousRetiring, previousIntent
		vowifiShuttingDown = previousShuttingDown
		vowifiSessionMu.Unlock()
	}()
	go func() { <-ctx.Done(); close(session.runDone) }()
	deadline, stop := context.WithTimeout(context.Background(), time.Second)
	defer stop()
	if err := stopVoWiFiForShutdown(deadline); err != nil {
		t.Fatal(err)
	}
	if err := stopVoWiFiForShutdown(deadline); err != nil {
		t.Fatal(err)
	}
	if restores.Load() != 1 || ctx.Err() != context.Canceled {
		t.Fatalf("restores=%d cancellation=%v", restores.Load(), ctx.Err())
	}
	if err := startVoWiFi(nil, "", "", 0); err == nil {
		t.Fatal("queued start was admitted after shutdown")
	}
}

func TestRadioRestoreRespectsFlightPolicy(t *testing.T) {
	for _, test := range []struct {
		name        string
		previous    int
		preferences moduleFlightModePreferences
		err         error
		want        int
	}{
		{"default", 1, moduleFlightModePreferences{}, nil, 4},
		{"manual airplane", 1, moduleFlightModePreferences{Enabled: true, UserConfigured: true}, nil, 4},
		{"manual cellular", 1, moduleFlightModePreferences{UserConfigured: true}, nil, 1},
		{"previous off", 0, moduleFlightModePreferences{UserConfigured: true}, nil, 0},
		{"unreadable preferences", 1, moduleFlightModePreferences{UserConfigured: true}, errors.New("fixture"), 4},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := moduleRadioRestoreMode(test.previous, test.preferences, test.err); got != test.want {
				t.Fatalf("mode=%d want=%d", got, test.want)
			}
		})
	}
}
