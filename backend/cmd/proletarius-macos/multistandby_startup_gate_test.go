package main

import (
	"context"
	"errors"
	"testing"
	"time"
)

func waitMultiStartupPermit(t *testing.T, permits <-chan func()) func() {
	t.Helper()
	select {
	case release := <-permits:
		return release
	case <-time.After(time.Second):
		t.Fatal("startup permit was not released")
		return nil
	}
}

func TestMultiStartupGateSerializesPreparationUntilIMSRegistered(t *testing.T) {
	m := (&app{}).multiStandby()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	firstRelease, err := m.acquireStartup(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer firstRelease()
	next := make(chan func(), 1)
	request := func() {
		if release, err := m.acquireStartup(ctx, nil); err == nil {
			next <- release
		}
	}
	go request()
	select {
	case release := <-next:
		release()
		t.Fatal("second profile prepared while first IMS registration was pending")
	case <-time.After(30 * time.Millisecond):
	}
	// First IMS registration succeeds. Its network lifetime remains active.
	firstRelease()
	secondRelease := waitMultiStartupPermit(t, next)
	defer secondRelease()
	if ctx.Err() != nil {
		t.Fatal("starting next profile stopped the established session")
	}
	go request()
	firstRelease() // Later first-session cleanup must not free the second lease.
	select {
	case release := <-next:
		release()
		t.Fatal("duplicate release admitted a third concurrent startup")
	case <-time.After(30 * time.Millisecond):
	}
	secondRelease()
	thirdRelease := waitMultiStartupPermit(t, next)
	thirdRelease()
}

func TestMultiStartupGateWaitingCanBeCancelled(t *testing.T) {
	for _, byStop := range []bool{false, true} {
		m := (&app{}).multiStandby()
		owner, err := m.acquireStartup(context.Background(), nil)
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithCancel(context.Background())
		stop := make(chan struct{})
		done := make(chan error, 1)
		go func() {
			release, err := m.acquireStartup(ctx, stop)
			if release != nil {
				release()
			}
			done <- err
		}()
		if byStop {
			close(stop)
		} else {
			cancel()
		}
		select {
		case err := <-done:
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("cancelled waiter result=%v", err)
			}
		case <-time.After(time.Second):
			t.Fatal("stopped waiter blocked behind another registration")
		}
		cancel()
		owner()
		check, err := m.acquireStartup(context.Background(), nil)
		if err != nil {
			t.Fatal(err)
		}
		check()
	}
}

func TestMultiRunWaitingForStartupStopsWithoutCardAccess(t *testing.T) {
	m := (&app{}).multiStandby()
	owner, err := m.acquireStartup(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer owner()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	r := &multiRuntime{profile: multiProfile{ProfileID: "queued"}, stop: make(chan struct{}), done: make(chan struct{}), cancel: cancel}
	// A nil broker makes any accidental preparation fail the test immediately.
	go m.run(ctx, r, nil)
	r.requestStop()
	select {
	case <-r.done:
	case <-time.After(time.Second):
		t.Fatal("stopping queued runtime waited for another profile's registration")
	}
}

func TestMultiStartupFailureReleasesGateForNextProfile(t *testing.T) {
	m := (&app{}).multiStandby()
	broker, _, p := multiTestBroker(t)
	failed := make(chan struct{}, 1)
	broker.ops.manager = func() (multiCardBackend, error) {
		failed <- struct{}{}
		return nil, errors.New("fixture preparation failure")
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	r := &multiRuntime{profile: p, stop: make(chan struct{}), done: make(chan struct{}), cancel: cancel}
	go m.run(ctx, r, broker)
	select {
	case <-failed:
	case <-time.After(time.Second):
		t.Fatal("fixture did not reach preparation")
	}
	next := make(chan func(), 1)
	go func() {
		if release, err := m.acquireStartup(ctx, nil); err == nil {
			next <- release
		}
	}()
	release := waitMultiStartupPermit(t, next)
	release()
	r.requestStop()
	select {
	case <-r.done:
	case <-time.After(time.Second):
		t.Fatal("failed runtime did not stop")
	}
}
