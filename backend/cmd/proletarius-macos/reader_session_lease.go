package main

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"strings"
	"time"
)

const (
	nativeReaderLeaseAcquireTimeout = 12 * time.Second
	nativeReaderLeaseLifetime       = 2 * time.Minute
)

var (
	errNativeReaderLeaseBusy       = errors.New("native reader lease is busy")
	errNativeReaderLeaseStale      = errors.New("reader identity changed before native lease acquisition")
	errNativeReaderLeaseNoIdentity = errors.New("reader SIM identity is unavailable")
)

type nativeReaderSessionLease struct {
	tokenHash  [sha256.Size]byte
	reader     string
	generation uint64
	expiresAt  time.Time
	timer      *time.Timer
}

type nativeReaderLeaseIdentity struct {
	ICCID           string `json:"iccid"`
	IMSI            string `json:"imsi"`
	MCC             string `json:"mcc"`
	MNC             string `json:"mnc"`
	GID1            string `json:"gid1,omitempty"`
	GID2            string `json:"gid2,omitempty"`
	SPN             string `json:"spn,omitempty"`
	PrivateIdentity string `json:"private_identity,omitempty"`
	PublicIdentity  string `json:"public_identity,omitempty"`
}

type nativeReaderSessionLeaseResponse struct {
	LeaseID          string                    `json:"lease_id"`
	Reader           string                    `json:"reader"`
	ReaderGeneration uint64                    `json:"reader_generation"`
	Identity         nativeReaderLeaseIdentity `json:"identity"`
}

// acquireNativeReaderSessionLease freezes the Go reader operation gate, waits
// for the current physical-card transaction to finish, and snapshots identity
// while holding authMu. Rust receives only this generation-bound snapshot and
// may request serialized AKA through the private lease endpoint; the lease
// must be released or it expires automatically.
func (rm *readerManager) acquireNativeReaderSessionLease(ctx context.Context, reader string) (nativeReaderSessionLeaseResponse, error) {
	reader = strings.TrimSpace(reader)
	if reader == "" || len(reader) > 512 || strings.ContainsRune(reader, '\x00') {
		return nativeReaderSessionLeaseResponse{}, errNativeReaderLeaseStale
	}
	if !beginNativeReaderSessionLeaseWindow() {
		return nativeReaderSessionLeaseResponse{}, errNativeReaderLeaseBusy
	}
	keepWindow := false
	defer func() {
		if !keepWindow {
			endNativeReaderSessionLeaseWindow()
		}
	}()

	deadline := time.NewTimer(nativeReaderLeaseAcquireTimeout)
	defer deadline.Stop()
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	// Match the reader manager's established rm.mu -> authMu lock order.
	// In particular, refresh can hold rm.mu while waiting for an APDU; never
	// take authMu first and then block on rm.mu or the lease could deadlock it.
	for {
		if err := ctx.Err(); err != nil {
			return nativeReaderSessionLeaseResponse{}, err
		}
		if rm.mu.TryLock() {
			if rm.authMu.TryLock() {
				break // both locks are held; the identity/generation is now stable
			}
			rm.mu.Unlock()
		}
		select {
		case <-ctx.Done():
			return nativeReaderSessionLeaseResponse{}, ctx.Err()
		case <-deadline.C:
			return nativeReaderSessionLeaseResponse{}, errNativeReaderLeaseBusy
		case <-ticker.C:
		}
	}
	locked := true
	defer func() {
		if locked {
			rm.authMu.Unlock()
		}
	}()
	if rm.readerName != reader || rm.generation == 0 {
		rm.mu.Unlock()
		return nativeReaderSessionLeaseResponse{}, errNativeReaderLeaseStale
	}
	generation := rm.generation
	rm.usimMu.RLock()
	var info *usimInfo
	if rm.cachedUSIM != nil {
		copy := *rm.cachedUSIM
		info = &copy
	}
	rm.usimMu.RUnlock()
	rm.mu.Unlock()
	if info == nil || normalizeSIMIdentifier(info.ICCID) == "" || normalizeSIMIdentifier(info.IMSI) == "" || info.MCC == "" || info.MNC == "" {
		return nativeReaderSessionLeaseResponse{}, errNativeReaderLeaseNoIdentity
	}

	var token [32]byte
	if _, err := rand.Read(token[:]); err != nil {
		return nativeReaderSessionLeaseResponse{}, err
	}
	leaseID := hex.EncodeToString(token[:])
	lease := &nativeReaderSessionLease{
		tokenHash:  sha256.Sum256(token[:]),
		reader:     reader,
		generation: generation,
		expiresAt:  time.Now().Add(nativeReaderLeaseLifetime),
	}
	clear(token[:])
	rm.markBLEBusy(generation)

	rm.nativeLeaseMu.Lock()
	if rm.nativeLease != nil {
		rm.nativeLeaseMu.Unlock()
		rm.clearBLEBusy(generation)
		return nativeReaderSessionLeaseResponse{}, errNativeReaderLeaseBusy
	}
	lease.timer = time.AfterFunc(nativeReaderLeaseLifetime, func() {
		rm.expireNativeReaderSessionLease(lease)
	})
	rm.nativeLease = lease
	rm.nativeLeaseMu.Unlock()
	locked = false // authMu remains held until explicit release or expiry.
	keepWindow = true

	return nativeReaderSessionLeaseResponse{
		LeaseID:          leaseID,
		Reader:           reader,
		ReaderGeneration: generation,
		Identity: nativeReaderLeaseIdentity{
			ICCID: normalizeSIMIdentifier(info.ICCID),
			IMSI:  normalizeSIMIdentifier(info.IMSI),
			MCC:   info.MCC,
			MNC:   info.MNC,
			GID1:  info.GID1,
			GID2:  info.GID2,
			SPN:   info.selectorSPN(),
			// ISIM identities cross only this authenticated private Unix socket.
			// They are not included in public snapshots, diagnostics, or logs.
			PrivateIdentity: strings.TrimSpace(info.PrivateIdentity),
			PublicIdentity:  strings.TrimSpace(info.PublicIdentity),
		},
	}, nil
}

// renewNativeReaderSessionLease extends a live Rust-owned reader session.
// Renewal shares nativeLeaseMu with AKA, release and expiry so a late timer
// callback cannot tear down a lease whose heartbeat won the race.
func (rm *readerManager) renewNativeReaderSessionLease(leaseID string) bool {
	provided, err := decodeNativeReaderLeaseID(leaseID)
	if err != nil {
		return false
	}
	defer clear(provided)
	providedHash := sha256.Sum256(provided)

	rm.nativeLeaseMu.Lock()
	defer rm.nativeLeaseMu.Unlock()
	lease := rm.nativeLease
	if lease == nil || subtle.ConstantTimeCompare(providedHash[:], lease.tokenHash[:]) != 1 {
		return false
	}
	lease.expiresAt = time.Now().Add(nativeReaderLeaseLifetime)
	if lease.timer != nil {
		lease.timer.Reset(nativeReaderLeaseLifetime)
	}
	return true
}

func (rm *readerManager) releaseNativeReaderSessionLease(leaseID string) bool {
	leaseID = strings.TrimSpace(leaseID)
	if leaseID == "" || len(leaseID) != 64 {
		return false
	}
	provided, err := decodeNativeReaderLeaseID(leaseID)
	if err != nil {
		return false
	}
	defer clear(provided)
	providedHash := sha256.Sum256(provided)

	rm.nativeLeaseMu.Lock()
	lease := rm.nativeLease
	if lease == nil || subtle.ConstantTimeCompare(providedHash[:], lease.tokenHash[:]) != 1 {
		rm.nativeLeaseMu.Unlock()
		return false
	}
	rm.nativeLease = nil
	if lease.timer != nil {
		lease.timer.Stop()
	}
	rm.nativeLeaseMu.Unlock()
	rm.finishNativeReaderSessionLease(lease)
	return true
}

func decodeNativeReaderLeaseID(leaseID string) ([]byte, error) {
	leaseID = strings.TrimSpace(leaseID)
	if len(leaseID) != 64 {
		return nil, errors.New("invalid reader lease id")
	}
	provided, err := hex.DecodeString(leaseID)
	if err != nil || len(provided) != 32 {
		clear(provided)
		return nil, errors.New("invalid reader lease id")
	}
	return provided, nil
}

// authenticateNativeReaderSessionLease temporarily hands the lease's APDU
// mutex to one generation-bound AKA transaction. The native lease gate stays
// active and nativeLeaseMu serializes this hand-off against release/expiry;
// ordinary reader/card routes must reject work for the duration of the lease.
func (rm *readerManager) authenticateNativeReaderSessionLease(
	leaseID, reader string,
	generation uint64,
	application string,
	rand, autn []byte,
) (*akaResult, error) {
	if len(rand) != 16 || len(autn) != 16 {
		return nil, errors.New("RAND 和 AUTN 必须为 16 字节")
	}
	if application != "usim" && application != "isim" {
		return nil, errors.New("AKA application 无效")
	}
	return rm.withNativeReaderSessionLease(leaseID, reader, generation, func() (*akaResult, error) {
		if application == "isim" {
			return rm.authenticateISIMAKAAtGenerationLegacy(rand, autn, generation)
		}
		return rm.authenticateEAPAKAAtGenerationLegacy(rand, autn, generation)
	})
}

func (rm *readerManager) withNativeReaderSessionLease(
	leaseID, reader string,
	generation uint64,
	operation func() (*akaResult, error),
) (*akaResult, error) {
	if operation == nil {
		return nil, errNativeReaderLeaseStale
	}
	provided, err := hex.DecodeString(strings.TrimSpace(leaseID))
	if err != nil || len(provided) != 32 {
		clear(provided)
		return nil, errNativeReaderLeaseStale
	}
	defer clear(provided)
	providedHash := sha256.Sum256(provided)

	rm.nativeLeaseMu.Lock()
	defer rm.nativeLeaseMu.Unlock()
	lease := rm.nativeLease
	if lease == nil || subtle.ConstantTimeCompare(providedHash[:], lease.tokenHash[:]) != 1 ||
		lease.reader != strings.TrimSpace(reader) || lease.generation != generation {
		return nil, errNativeReaderLeaseStale
	}

	// acquireNativeReaderSessionLease keeps authMu held as the exclusive card
	// owner. Hand it to the existing, generation-checked authentication path,
	// then restore the lease before allowing release or its watchdog to proceed.
	rm.authMu.Unlock()
	defer rm.authMu.Lock()
	return operation()
}

func (rm *readerManager) expireNativeReaderSessionLease(expired *nativeReaderSessionLease) {
	rm.nativeLeaseMu.Lock()
	if rm.nativeLease != expired {
		rm.nativeLeaseMu.Unlock()
		return
	}
	if remaining := time.Until(expired.expiresAt); remaining > 0 {
		// A timer callback may already be waiting on nativeLeaseMu when the
		// heartbeat resets it. Re-check the deadline under the lock and keep the
		// reader reserved instead of honoring a stale callback.
		if expired.timer != nil {
			expired.timer.Reset(remaining)
		}
		rm.nativeLeaseMu.Unlock()
		return
	}
	rm.nativeLease = nil
	if expired.timer != nil {
		expired.timer.Stop()
	}
	rm.nativeLeaseMu.Unlock()
	rm.finishNativeReaderSessionLease(expired)
}

func (rm *readerManager) finishNativeReaderSessionLease(lease *nativeReaderSessionLease) {
	rm.clearBLEBusy(lease.generation)
	endNativeReaderSessionLeaseWindow()
	rm.authMu.Unlock()
	clear(lease.tokenHash[:])
}
