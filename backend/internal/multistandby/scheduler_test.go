package multistandby

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestCancelledWorkerRetainsLeaseUntilExit(t *testing.T) {
	s := NewScheduler()
	ctx, cancel := context.WithCancel(context.Background())
	entered := make(chan struct{})
	release := make(chan struct{})
	returned := make(chan struct{})
	go func() {
		_ = s.Do(ctx, func(context.Context) error { close(entered); <-release; return nil })
		close(returned)
	}()
	<-entered
	cancel()
	select {
	case <-returned:
		t.Fatal("worker relinquished ownership before exit")
	case <-time.After(20 * time.Millisecond):
	}
	drained := make(chan struct{})
	go func() { s.Close(); close(drained) }()
	select {
	case <-drained:
		t.Fatal("close did not drain")
	case <-time.After(20 * time.Millisecond):
	}
	close(release)
	<-drained
	<-returned
	if err := s.Do(context.Background(), func(context.Context) error { return nil }); !errors.Is(err, ErrClosed) {
		t.Fatal(err)
	}
}
func TestCancelledQueuedChallengeNeverTouchesCard(t *testing.T) {
	s := NewScheduler()
	defer s.Close()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	called := false
	err := s.Do(ctx, func(context.Context) error { called = true; return nil })
	if !errors.Is(err, context.Canceled) || called {
		t.Fatalf("err=%v called=%t", err, called)
	}
}
