package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	swusim "github.com/1239t/swu-go/pkg/sim"
	"proletarius.local/core/internal/esim"
	"proletarius.local/core/internal/multistandby"
	imssim "proletarius.local/core/internal/vowifi/sim"
)

func multiID(kind string, pieces ...string) string {
	h := sha256.Sum256([]byte(strings.Join(append([]string{kind}, pieces...), "\x00")))
	return kind + "_" + hex.EncodeToString(h[:16])
}

type multiProfile struct {
	ProfileID         string `json:"profile_id"`
	CardID            string `json:"card_id"`
	EID               string `json:"eid"`
	ICCID             string `json:"iccid"`
	DisplayName       string `json:"display_name"`
	PhoneNumber       string `json:"phone_number"`
	PhysicalEnabled   bool   `json:"physical_enabled"`
	DesiredState      string `json:"desired_state"`
	AuthState         string `json:"auth_state"`
	TunnelState       string `json:"tunnel_state"`
	IMSState          string `json:"ims_state"`
	SMSReady          bool   `json:"sms_ready"`
	Health            string `json:"health"`
	LastError         string `json:"last_error"`
	SessionGeneration uint64 `json:"session_generation"`
	AID               string `json:"-"`
	Reader            string `json:"-"`
}

var errMultiCardIdentityChanged = errors.New("卡片EID或应用已变化，已拒绝鉴权")

type multiCardBackend interface {
	GetEIDs() ([]esim.EUICCInfo, error)
	SwitchProfileWithResult(context.Context, string, string) (esim.SwitchProfileResult, error)
	ListNotifications(string) ([]esim.NotificationItem, error)
}

// Inject only physical operations for deterministic tests; production uses the
// manager's private Go card path and never invokes a native Rust AKA callback.
type multiCardOps struct {
	manager func() (multiCardBackend, error)
	read    func(context.Context) (*usimInfo, error)
	rebuild func(context.Context, bool) error
}
type multiCardBroker struct {
	rm         *readerManager
	queue      *multistandby.Scheduler
	reader     string
	generation uint64
	eid        string
	ops        *multiCardOps
	stateMu    sync.Mutex
	current    string
}

func (b *multiCardBroker) currentProfileID() string {
	b.stateMu.Lock()
	defer b.stateMu.Unlock()
	return b.current
}

// Call only after physical EID/ICCID matching. Catalog identifiers may retain
// ICCID padding that the live USIM reader normalizes away.
func (b *multiCardBroker) markProfile(profile multiProfile) {
	b.stateMu.Lock()
	defer b.stateMu.Unlock()
	b.current = profile.ProfileID
	if b.current == "" {
		b.current = multiID("profile", profile.EID, profile.ICCID)
	}
}
func (b *multiCardBroker) setCurrent(info *usimInfo) {
	b.stateMu.Lock()
	defer b.stateMu.Unlock()
	b.current = ""
	if info != nil && strings.TrimSpace(info.ICCID) != "" {
		b.current = multiID("profile", b.eid, info.ICCID)
	}
}
func (b *multiCardBroker) physicalOps() *multiCardOps {
	if b.ops != nil {
		return b.ops
	}
	return &multiCardOps{
		manager: func() (multiCardBackend, error) {
			if err := b.rm.validateAuthenticationReader(b.reader, b.generation); err != nil {
				return nil, err
			}
			manager, ok := b.rm.ESIMManager()
			if !ok {
				fresh, err := esim.NewManager(b.rm.readerESIMManagerOptions(b.reader))
				if err != nil {
					return nil, err
				}
				if !b.rm.mu.TryLock() {
					return nil, errReaderBusy
				}
				defer b.rm.mu.Unlock()
				if b.rm.readerName != b.reader || b.rm.generation != b.generation {
					return nil, errors.New("读卡器已变化")
				}
				b.rm.esimManager = fresh
				return fresh, nil
			}
			return manager, nil
		},
		read: func(ctx context.Context) (*usimInfo, error) {
			// All mu -> authMu paths use TryLock. Do not retain rm.mu during
			// LPA calls: their persistent-channel factory also needs rm.mu.
			if !b.rm.mu.TryLock() {
				return nil, errReaderBusy
			}
			defer b.rm.mu.Unlock()
			if b.rm.readerName != b.reader || b.rm.generation != b.generation {
				return nil, errors.New("读卡器已变化")
			}
			readCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
			defer cancel()
			if err := b.rm.loadUSIMInfoWithCardLeaseContextLocked(readCtx, false); err != nil {
				b.rm.usimMu.Lock()
				b.rm.cachedUSIM = nil
				b.rm.usimMu.Unlock()
				return nil, err
			}
			return b.rm.USIMInfo(), nil
		},
		rebuild: func(ctx context.Context, reset bool) error {
			if !b.rm.mu.TryLock() {
				return errReaderBusy
			}
			if b.rm.readerName != b.reader || b.rm.generation != b.generation {
				b.rm.mu.Unlock()
				return errors.New("读卡器已变化")
			}
			if b.rm.persistentCCID != nil {
				b.rm.persistentCCID.Close()
				b.rm.persistentCCID = nil
			}
			b.rm.esimManager = nil
			b.rm.usimMu.Lock()
			b.rm.cachedUSIM = nil
			b.rm.usimMu.Unlock()
			b.rm.mu.Unlock()
			if reset && (isESTKmeReader(b.reader) || isBLEReader(b.reader)) {
				if err := b.rm.PowerCycleCard(b.reader); err != nil {
					return err
				}
			}
			if err := waitMultiCardSettle(ctx); err != nil {
				return err
			}
			fresh, err := esim.NewManager(b.rm.readerESIMManagerOptions(b.reader))
			if err != nil {
				return err
			}
			if !b.rm.mu.TryLock() {
				return errReaderBusy
			}
			defer b.rm.mu.Unlock()
			if b.rm.readerName != b.reader || b.rm.generation != b.generation {
				return errors.New("读卡器已变化")
			}
			// A controlled Profile switch retains the logical reader generation.
			b.rm.esimManager = fresh
			return nil
		},
	}
}
func waitMultiCardSettle(ctx context.Context) error {
	timer := time.NewTimer(500 * time.Millisecond)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
func (b *multiCardBroker) verifyEID(manager multiCardBackend, aid string) error {
	eids, err := manager.GetEIDs()
	if err != nil {
		return err
	}
	for _, identity := range eids {
		if identity.EID == b.eid && (aid == "" || strings.EqualFold(identity.AIDHex, aid)) {
			return nil
		}
	}
	return errMultiCardIdentityChanged
}
func (b *multiCardBroker) withCard(ctx context.Context, work func(context.Context, *multiCardOps) error) error {
	if ctx == nil {
		ctx = context.Background()
	}
	return b.queue.Do(ctx, func(ctx context.Context) error {
		release, err := b.rm.beginOwnedCardOperationContext(ctx)
		if err != nil {
			b.setCurrent(nil)
			return err
		}
		defer release()
		if err := b.rm.validateAuthenticationReader(b.reader, b.generation); err != nil {
			b.setCurrent(nil)
			return err
		}
		err = work(ctx, b.physicalOps())
		if err != nil {
			b.setCurrent(nil)
		}
		return err
	})
}

// currentIdentity reads the physical original Profile while the pool owns both
// the process reservation and card lease. Inventory's enabled flags are not proof.
func (b *multiCardBroker) currentIdentity(ctx context.Context) (*usimInfo, error) {
	var info *usimInfo
	err := b.withCard(ctx, func(ctx context.Context, ops *multiCardOps) error {
		manager, err := ops.manager()
		if err != nil {
			return err
		}
		if err := b.verifyEID(manager, ""); err != nil {
			return err
		}
		info, err = ops.read(ctx)
		if err != nil {
			return err
		}
		if info == nil || info.ICCID == "" || info.IMSI == "" {
			return errors.New("当前Profile身份无法确认")
		}
		if err := b.verifyEID(manager, ""); err != nil {
			return err
		}
		b.setCurrent(info)
		return nil
	})
	return info, err
}

func (b *multiCardBroker) withProfile(ctx context.Context, p multiProfile, work func(context.Context, *usimInfo) error) error {
	if p.EID == "" || p.EID != b.eid || p.ICCID == "" {
		return errors.New("Profile不属于已保留的卡片")
	}
	return b.withCard(ctx, func(ctx context.Context, ops *multiCardOps) error {
		manager, err := ops.manager()
		if err != nil {
			return err
		}
		if eidErr := b.verifyEID(manager, p.AID); eidErr != nil {
			if errors.Is(eidErr, errMultiCardIdentityChanged) || ctx.Err() != nil {
				return eidErr
			}
			if err := ops.rebuild(ctx, false); err != nil {
				return err
			}
			manager, err = ops.manager()
			if err != nil {
				return err
			}
			if err := b.verifyEID(manager, p.AID); err != nil {
				return err
			}
		}
		info, readErr := ops.read(ctx)
		if readErr != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			// A retained PC/SC handle may have become invalid after a card reset.
			// Reopen without speculative power changes, then recheck physical EID.
			if err := ops.rebuild(ctx, false); err != nil {
				return err
			}
			manager, err = ops.manager()
			if err != nil {
				return err
			}
			if err := b.verifyEID(manager, p.AID); err != nil {
				return err
			}
			info, readErr = ops.read(ctx)
			if readErr != nil {
				return readErr
			}
		}
		if info == nil || normalizeSIMIdentifier(info.ICCID) != normalizeSIMIdentifier(p.ICCID) {
			b.setCurrent(nil)
			_, switchErr := manager.SwitchProfileWithResult(ctx, p.ICCID, p.AID)
			// Once ENABLE was attempted the card may reset even if its response
			// was lost. Keep ownership through recovery even after cancellation.
			recoverCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 45*time.Second)
			defer cancel()
			if err := ops.rebuild(recoverCtx, true); err != nil {
				return fmt.Errorf("切卡后重建失败: %w", err)
			}
			manager, err = ops.manager()
			if err != nil {
				return err
			}
			if err := b.verifyEID(manager, p.AID); err != nil {
				return err
			}
			info, err = ops.read(recoverCtx)
			if err != nil {
				return err
			}
			if info == nil || normalizeSIMIdentifier(info.ICCID) != normalizeSIMIdentifier(p.ICCID) {
				if switchErr != nil {
					return fmt.Errorf("切卡结果未确认: %w", switchErr)
				}
				return errors.New("切卡后未读到目标Profile，请检查卡片复位兼容性")
			}
		}
		// Physical activation does not prove notification delivery. A previous
		// attempt may have switched successfully but failed while synchronizing,
		// so retry delivery even when the target ICCID is already presented.
		pending, err := manager.ListNotifications(p.AID)
		if err != nil {
			return fmt.Errorf("Profile已启用，但运营商通知同步失败: %w", err)
		}
		for _, notification := range pending {
			if notification.Event == "enable" || notification.Event == "disable" {
				return errors.New("Profile已启用，但运营商通知仍待处理")
			}
		}
		// A slot hot-swap can happen without a logical reader-generation change.
		// Verify both the eUICC and live USIM immediately before private Go AKA.
		if err := b.verifyEID(manager, p.AID); err != nil {
			return err
		}
		info, err = ops.read(ctx)
		if err != nil {
			return err
		}
		if info == nil || normalizeSIMIdentifier(info.ICCID) != normalizeSIMIdentifier(p.ICCID) || strings.TrimSpace(info.IMSI) == "" {
			return errors.New("Profile物理身份验证失败；未执行AKA")
		}
		if err := b.rm.validateAuthenticationReader(b.reader, b.generation); err != nil {
			return err
		}
		b.setCurrent(info)
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := work(ctx, info); err != nil {
			return err
		}
		b.markProfile(p)
		return nil
	})
}

type multiAKA struct {
	broker  *multiCardBroker
	profile multiProfile
	ctx     context.Context
	info    *usimInfo
	imei    string
}

func (a *multiAKA) GetIMSI() (string, error) { return a.info.IMSI, nil }
func (a *multiAKA) GetIMEI() (string, error) { return a.imei, nil }
func (a *multiAKA) Close() error             { return nil }
func (a *multiAKA) authenticate(rand, autn []byte, isim bool) (*akaResult, error) {
	var result *akaResult
	err := a.broker.withProfile(a.ctx, a.profile, func(ctx context.Context, info *usimInfo) error {
		if info.IMSI != a.info.IMSI {
			return errors.New("Profile IMSI已变化，需重建会话")
		}
		prefix, name := usimApplicationAIDPrefix, "USIM"
		if isim {
			prefix, name = isimApplicationAIDPrefix, "ISIM"
		}
		var err error
		result, err = authenticateReaderUnderLeaseIdentity(ctx, a.broker.reader, rand, autn, prefix, name, a.info)
		return err
	})
	return result, err
}
func (a *multiAKA) CalculateAKA(rand, autn []byte) (res, ck, ik, auts []byte, err error) {
	r, err := a.authenticate(rand, autn, false)
	if r == nil {
		return nil, nil, nil, nil, err
	}
	if errors.Is(err, errReaderAKASyncFailure) {
		err = swusim.ErrSyncFailure
	}
	return r.RES, r.CK, r.IK, r.AUTS, err
}

type multiIMSAKA struct{ adapter *multiAKA }

func (a *multiIMSAKA) CalculateAKA(rand, autn []byte) (imssim.AKAResult, error) {
	r, e := a.adapter.authenticate(rand, autn, false)
	return mapReaderIMSAKAResult(r, e)
}
func (a *multiIMSAKA) CalculateISIMAKA(rand, autn []byte) (imssim.AKAResult, error) {
	r, e := a.adapter.authenticate(rand, autn, true)
	return mapReaderIMSAKAResult(r, e)
}
func multiCardError(err error) string {
	if err == nil {
		return ""
	}
	return fmt.Sprintf("%v", err)
}

func verifyMultiAuthIdentity(iccid, imsi string, expected *usimInfo) error {
	if expected == nil || normalizeSIMIdentifier(expected.ICCID) == "" || normalizeSIMIdentifier(expected.IMSI) == "" ||
		normalizeSIMIdentifier(iccid) != normalizeSIMIdentifier(expected.ICCID) || normalizeSIMIdentifier(imsi) != normalizeSIMIdentifier(expected.IMSI) {
		return errors.New("鉴权句柄的ICCID或IMSI已变化，已拒绝AKA")
	}
	return nil
}
