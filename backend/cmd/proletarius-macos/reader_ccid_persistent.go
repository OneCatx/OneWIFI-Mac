package main

import (
	"context"
	"log"
	"sync"
	"time"

	uiccccid "github.com/damonto/uicc-go/ccid"
)

// ccidPersistentExchanger keeps one PC/SC session open and reuses it across
// all LPA operations for the selected reader.
//
// Cheap CCID readers (e.g. "Generic EMV Smartcard Reader") are driven into a
// permanently unresponsive state (SCARD_W_UNRESPONSIVE_CARD on every later
// scardConnect, card only recovers after being physically re-seated) by the
// per-operation session churn of the euicc-go CCID driver: each LPA operation
// opens a fresh PC/SC session, sends a TERMINAL CAPABILITY (80 AA) command,
// exchanges APDUs, and closes the session again. Reusing a single session
// removes that churn entirely; if the session ever desynchronizes, the next
// Transmit error closes it and the following exchange re-opens it once.
type ccidPersistentExchanger struct {
	mu sync.Mutex
	// name is the PC/SC reader this exchanger is bound to.
	name string
	// reader is the open PC/SC session, or nil when it needs re-opening.
	reader *uiccccid.Reader
	// lastReviveAttempt throttles revival attempts: on a dead card every
	// open fails and revival (exclusive connect + reset) adds ~2s latency
	// per call, which turns into a retry storm under UI polling.
	lastReviveAttempt time.Time
}

func newCCIDPersistentExchanger(name string) *ccidPersistentExchanger {
	return &ccidPersistentExchanger{name: name}
}

func (e *ccidPersistentExchanger) lock()   { e.mu.Lock() }
func (e *ccidPersistentExchanger) unlock() { e.mu.Unlock() }

// openLocked opens the underlying PC/SC reader session, reviving the card
// once if it reports SCARD_W_UNRESPONSIVE_CARD. Caller must hold the lock.
func (e *ccidPersistentExchanger) openLocked() error {
	if e.reader != nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	reader, err := uiccccid.Open(ctx, e.name)
	if err != nil {
		if !isUnresponsiveCardError(err) {
			return err
		}
		if time.Since(e.lastReviveAttempt) < 5*time.Second {
			return err
		}
		e.lastReviveAttempt = time.Now()
		log.Printf("reader %s reported unresponsive card, attempting revival", e.name)
		if revErr := reviveUnresponsiveCard(e.name); revErr != nil {
			log.Printf("reader %s revival failed: %v", e.name, revErr)
			return err
		}
		time.Sleep(300 * time.Millisecond)
		reader, err = uiccccid.Open(ctx, e.name)
		if err != nil {
			return err
		}
	}
	e.reader = reader
	return nil
}

// Exchange implements estkble.Exchanger. The first call opens the persistent
// session; subsequent calls reuse it.
func (e *ccidPersistentExchanger) Exchange(ctx context.Context, apdu []byte) ([]byte, error) {
	e.lock()
	defer e.unlock()
	if err := e.openLocked(); err != nil {
		return nil, err
	}
	transmitCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	recv, err := e.reader.Transmit(transmitCtx, apdu)
	if err != nil {
		// The session may have desynchronized (e.g. a glitch on a cheap
		// reader). Drop it so the next exchange re-opens a clean one.
		_ = e.reader.Close()
		e.reader = nil
		return nil, err
	}
	return recv, nil
}

// Close releases the underlying PC/SC session. Safe to call repeatedly.
func (e *ccidPersistentExchanger) Close() {
	e.lock()
	defer e.unlock()
	if e.reader != nil {
		_ = e.reader.Close()
		e.reader = nil
	}
}
