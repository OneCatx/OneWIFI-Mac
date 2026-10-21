package readerapp

import (
	"errors"
	"sort"
	"strings"
	"sync"
)

// SessionKey is the stable isolation boundary for all reader telephony state.
// ReaderID prevents two physical devices from sharing a session, while SIMID
// (normally ICCID, with IMSI only as a temporary fallback) prevents an eSIM
// profile switch on one reader from inheriting the previous profile's IMS,
// SMS or call state.
type SessionKey struct {
	ReaderID ReaderID `json:"reader_id"`
	SIMID    string   `json:"sim_id"`
}

func NewSessionKey(readerID ReaderID, simID string) (SessionKey, error) {
	key := SessionKey{ReaderID: ReaderID(strings.TrimSpace(string(readerID))), SIMID: strings.TrimSpace(simID)}
	if key.ReaderID == "" || key.SIMID == "" {
		return SessionKey{}, errors.New("reader and SIM identity are required")
	}
	return key, nil
}

func (k SessionKey) String() string { return string(k.ReaderID) + "|" + k.SIMID }

// SessionRegistry owns independent per-reader/per-SIM instances. It contains
// no global "current session" slot: API and UI adapters must select a key
// explicitly before operating on telephony state.
type SessionRegistry[T any] struct {
	mu       sync.RWMutex
	sessions map[SessionKey]T
}

func NewSessionRegistry[T any]() *SessionRegistry[T] {
	return &SessionRegistry[T]{sessions: make(map[SessionKey]T)}
}

func (r *SessionRegistry[T]) Put(key SessionKey, value T) error {
	if r == nil || key.ReaderID == "" || key.SIMID == "" {
		return errors.New("invalid reader session key")
	}
	r.mu.Lock()
	r.sessions[key] = value
	r.mu.Unlock()
	return nil
}

func (r *SessionRegistry[T]) Get(key SessionKey) (T, bool) {
	var zero T
	if r == nil {
		return zero, false
	}
	r.mu.RLock()
	value, ok := r.sessions[key]
	r.mu.RUnlock()
	return value, ok
}

func (r *SessionRegistry[T]) Delete(key SessionKey) (T, bool) {
	var zero T
	if r == nil {
		return zero, false
	}
	r.mu.Lock()
	value, ok := r.sessions[key]
	if ok {
		delete(r.sessions, key)
	}
	r.mu.Unlock()
	return value, ok
}

func (r *SessionRegistry[T]) Keys() []SessionKey {
	if r == nil {
		return nil
	}
	r.mu.RLock()
	keys := make([]SessionKey, 0, len(r.sessions))
	for key := range r.sessions {
		keys = append(keys, key)
	}
	r.mu.RUnlock()
	sort.Slice(keys, func(i, j int) bool { return keys[i].String() < keys[j].String() })
	return keys
}
