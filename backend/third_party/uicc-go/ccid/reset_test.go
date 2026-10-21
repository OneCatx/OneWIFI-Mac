package ccid

import (
	"context"
	"errors"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/ElMostafaIdrassi/goscard"
)

// No test in this file opens a PC/SC library, context or card. The fake
// context uses the production acquirePCSC/releasePCSC counter so enumeration
// and reset lifetimes can interleave without depending on attached hardware.
type resetFixture struct {
	mu                            sync.Mutex
	events                        []string
	initialized, finalized        int
	connectErr, disconnectErr     error
	releaseErr, initializationErr error
	onOpen, onConnect             func()
	disposition                   goscard.SCardDisposition
	reader                        string
}

func newResetFixture(t *testing.T) *resetFixture {
	t.Helper()
	pcsc.mu.Lock()
	if pcsc.refs != 0 {
		pcsc.mu.Unlock()
		t.Fatal("fixture requires no live PC/SC references")
	}
	oldInitialize, oldFinalize := initializePCSC, finalizePCSC
	f := &resetFixture{}
	initializePCSC = func() error {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.events = append(f.events, "initialize")
		f.initialized++
		return f.initializationErr
	}
	finalizePCSC = func() {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.events = append(f.events, "finalize")
		f.finalized++
	}
	pcsc.mu.Unlock()
	t.Cleanup(func() {
		pcsc.mu.Lock()
		defer pcsc.mu.Unlock()
		if pcsc.refs != 0 {
			t.Errorf("leaked %d PC/SC references", pcsc.refs)
		}
		pcsc.refs = 0
		initializePCSC, finalizePCSC = oldInitialize, oldFinalize
	})
	return f
}

func (f *resetFixture) record(event string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.events = append(f.events, event)
}

func (f *resetFixture) open() (cardResetContext, error) {
	if err := acquirePCSC(); err != nil {
		return nil, err
	}
	f.record("open_context")
	if f.onOpen != nil {
		f.onOpen()
	}
	return f, nil
}

func (f *resetFixture) connectExclusive(reader string) (cardResetConnection, error) {
	f.record("connect_exclusive")
	f.reader = reader
	if f.onConnect != nil {
		f.onConnect()
	}
	if f.connectErr != nil {
		return nil, f.connectErr
	}
	return f, nil
}

func (f *resetFixture) disconnect(disposition goscard.SCardDisposition) error {
	f.record("disconnect")
	f.disposition = disposition
	return f.disconnectErr
}

func (f *resetFixture) release() error {
	f.record("release_context")
	defer releasePCSC()
	return f.releaseErr
}

func (f *resetFixture) expectCounts(t *testing.T, refs, initialize, finalize int) {
	t.Helper()
	pcsc.mu.Lock()
	actualRefs := pcsc.refs
	pcsc.mu.Unlock()
	f.mu.Lock()
	defer f.mu.Unlock()
	if actualRefs != refs || f.initialized != initialize || f.finalized != finalize {
		t.Errorf("refs=%d initialized=%d finalized=%d; want %d/%d/%d", actualRefs, f.initialized, f.finalized, refs, initialize, finalize)
	}
}

func TestResetRetainsLibraryAfterConcurrentEnumerationFinishes(t *testing.T) {
	f := newResetFixture(t)
	if err := acquirePCSC(); err != nil { // An existing ListReaders context.
		t.Fatal(err)
	}
	entered, resume := make(chan struct{}), make(chan struct{})
	f.onConnect = func() { close(entered); <-resume }
	done := make(chan error, 1)
	go func() { done <- resetCardWithContext(context.Background(), "reader", goscard.SCardUnpowerCard, f.open) }()
	select {
	case <-entered:
	case <-time.After(time.Second):
		close(resume)
		releasePCSC()
		<-done
		t.Fatal("reset did not acquire its context")
	}
	f.expectCounts(t, 2, 1, 0)
	releasePCSC() // Enumeration completes while reset still owns a context.
	f.expectCounts(t, 1, 1, 0)
	close(resume)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	f.expectCounts(t, 0, 1, 1)
	if !reflect.DeepEqual(f.events, []string{"initialize", "open_context", "connect_exclusive", "disconnect", "release_context", "finalize"}) {
		t.Fatalf("cleanup ordering: %v", f.events)
	}
}

func TestEnumerationRetainsLibraryAfterResetFinishes(t *testing.T) {
	f := newResetFixture(t)
	f.onConnect = func() {
		if err := acquirePCSC(); err != nil { // A new concurrent enumeration.
			t.Fatal(err)
		}
		f.expectCounts(t, 2, 1, 0)
	}
	if err := resetCardWithContext(context.Background(), "reader", goscard.SCardResetCard, f.open); err != nil {
		t.Fatal(err)
	}
	if f.disposition != goscard.SCardResetCard {
		t.Error("warm reset disposition changed")
	}
	f.expectCounts(t, 1, 1, 0)
	releasePCSC()
	f.expectCounts(t, 0, 1, 1)
}

func TestResetAlwaysReleasesContextAndPreservesCleanupErrors(t *testing.T) {
	connectErr := errors.New("fixture sharing violation")
	disconnectErr := errors.New("fixture disconnect failed")
	releaseErr := errors.New("fixture release failed")
	for _, tc := range []struct {
		name, cancelAt               string
		connect, disconnect, release error
	}{
		{name: "success"},
		{name: "connect_failure", connect: connectErr},
		{name: "disconnect_failure", disconnect: disconnectErr},
		{name: "release_failure", release: releaseErr},
		{name: "both_cleanup_failures", disconnect: disconnectErr, release: releaseErr},
		{name: "connect_and_release_failures", connect: connectErr, release: releaseErr},
		{name: "canceled_after_open", cancelAt: "open"},
		{name: "canceled_after_connect", cancelAt: "connect"},
		{name: "canceled_cleanup_errors", cancelAt: "connect", disconnect: disconnectErr, release: releaseErr},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newResetFixture(t)
			f.connectErr, f.disconnectErr, f.releaseErr = tc.connect, tc.disconnect, tc.release
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if tc.cancelAt == "open" {
				f.onOpen = cancel
			} else if tc.cancelAt == "connect" {
				f.onConnect = cancel
			}
			err := resetCardWithContext(ctx, "  reader  ", goscard.SCardUnpowerCard, f.open)
			for _, expected := range []error{tc.connect, tc.disconnect, tc.release} {
				if expected != nil && !errors.Is(err, expected) {
					t.Errorf("lost error %v: %v", expected, err)
				}
			}
			if tc.cancelAt != "" && !errors.Is(err, context.Canceled) {
				t.Errorf("lost cancellation: %v", err)
			}
			if tc.connect == nil && tc.disconnect == nil && tc.release == nil && tc.cancelAt == "" && err != nil {
				t.Fatal(err)
			}
			want := []string{"initialize", "open_context"}
			if tc.cancelAt != "open" {
				want = append(want, "connect_exclusive")
				if f.reader != "reader" {
					t.Errorf("reader selection changed: %q", f.reader)
				}
				if tc.connect == nil {
					want = append(want, "disconnect")
					if f.disposition != goscard.SCardUnpowerCard {
						t.Error("reset disposition changed")
					}
				}
			}
			want = append(want, "release_context", "finalize")
			if !reflect.DeepEqual(f.events, want) {
				t.Errorf("cleanup order=%v want=%v", f.events, want)
			}
			f.expectCounts(t, 0, 1, 1)
		})
	}
}

func TestResetRejectsInvalidOrCanceledRequestsBeforeOpening(t *testing.T) {
	for _, tc := range []struct {
		name, reader string
		disposition  goscard.SCardDisposition
		canceled     bool
	}{
		{name: "blank_reader", reader: " ", disposition: goscard.SCardResetCard},
		{name: "leave_is_not_reset", reader: "reader", disposition: goscard.SCardLeaveCard},
		{name: "eject_is_not_reset", reader: "reader", disposition: goscard.SCardEjectCard},
		{name: "canceled", reader: "reader", disposition: goscard.SCardResetCard, canceled: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if tc.canceled {
				cancel()
			}
			err := resetCardWithContext(ctx, tc.reader, tc.disposition, func() (cardResetContext, error) {
				t.Fatal("invalid request opened PC/SC")
				return nil, nil
			})
			if err == nil || (tc.canceled && !errors.Is(err, context.Canceled)) {
				t.Fatalf("unexpected error: %v", err)
			}
		})
	}
}

func TestResetInitializationFailureDoesNotOwnOrFinalizeLibrary(t *testing.T) {
	f := newResetFixture(t)
	f.initializationErr = errors.New("fixture library load failed")
	if err := resetCardWithContext(nil, "reader", goscard.SCardResetCard, f.open); !errors.Is(err, f.initializationErr) {
		t.Fatalf("initialization error=%v", err)
	}
	f.expectCounts(t, 0, 1, 0)
}
