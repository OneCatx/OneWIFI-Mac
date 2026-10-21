// Package multistandby provides the physical-card admission queue. Network
// sessions never hold this queue while waiting for their carrier.
package multistandby

import (
	"context"
	"errors"
	"sync"
)

var ErrClosed = errors.New("card authentication queue closed")

type task struct {
	ctx    context.Context
	run    func(context.Context) error
	result chan error
}

// Scheduler is FIFO. Cancellation removes a waiter's right to execute, but an
// already running hardware operation retains ownership until it actually exits.
type Scheduler struct {
	mu     sync.Mutex
	queue  []*task
	wake   chan struct{}
	closed bool
	done   chan struct{}
}

func NewScheduler() *Scheduler {
	s := &Scheduler{wake: make(chan struct{}, 1), done: make(chan struct{})}
	go s.loop()
	return s
}
func (s *Scheduler) Do(ctx context.Context, run func(context.Context) error) error {
	t := &task{ctx: ctx, run: run, result: make(chan error, 1)}
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return ErrClosed
	}
	s.queue = append(s.queue, t)
	s.mu.Unlock()
	select {
	case s.wake <- struct{}{}:
	default:
	}
	// Once enqueued, wait for physical completion even on cancellation. This is
	// essential: a caller must not release the cross-process lease prematurely.
	return <-t.result
}
func (s *Scheduler) loop() {
	defer close(s.done)
	for {
		s.mu.Lock()
		if len(s.queue) == 0 {
			closed := s.closed
			s.mu.Unlock()
			if closed {
				return
			}
			<-s.wake
			continue
		}
		t := s.queue[0]
		s.queue = s.queue[1:]
		closed := s.closed
		s.mu.Unlock()
		if closed {
			t.result <- ErrClosed
			continue
		}
		if err := t.ctx.Err(); err != nil {
			t.result <- err
			continue
		}
		t.result <- t.run(t.ctx)
	}
}
func (s *Scheduler) Close() {
	s.mu.Lock()
	s.closed = true
	s.mu.Unlock()
	select {
	case s.wake <- struct{}{}:
	default:
	}
	<-s.done
}
