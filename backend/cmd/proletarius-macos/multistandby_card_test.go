package main

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/damonto/euicc-go/driver"
	"proletarius.local/core/internal/esim"
	"proletarius.local/core/internal/multistandby"
)

type multiTestCard struct {
	eid             string
	iccid           string
	switches        int
	notifications   int
	notificationErr error
	pending         bool
	onSwitch        func()
	onEID           func(int)
	eidReads        int
}

func (c *multiTestCard) GetEIDs() ([]esim.EUICCInfo, error) {
	c.eidReads++
	if c.onEID != nil {
		c.onEID(c.eidReads)
	}
	return []esim.EUICCInfo{{EID: c.eid, AIDHex: "AA"}}, nil
}
func (c *multiTestCard) SwitchProfileWithResult(_ context.Context, iccid, _ string) (esim.SwitchProfileResult, error) {
	c.switches++
	c.iccid = iccid
	if c.onSwitch != nil {
		c.onSwitch()
	}
	return esim.SwitchProfileResult{SwitchAccepted: true}, nil
}
func (c *multiTestCard) ListNotifications(string) ([]esim.NotificationItem, error) {
	c.notifications++
	if c.notificationErr != nil {
		return nil, c.notificationErr
	}
	if c.pending {
		return []esim.NotificationItem{{Event: "enable"}}, nil
	}
	return nil, nil
}
func multiTestBroker(t *testing.T) (*multiCardBroker, *multiTestCard, multiProfile) {
	t.Helper()
	card := &multiTestCard{eid: "eid-fixture", iccid: "8988001"}
	rm := &readerManager{readerName: "fixture-reader", generation: 7}
	b := &multiCardBroker{rm: rm, queue: multistandby.NewScheduler(), reader: rm.readerName, generation: 7, eid: card.eid}
	b.ops = &multiCardOps{
		manager: func() (multiCardBackend, error) { return card, nil },
		read: func(context.Context) (*usimInfo, error) {
			return &usimInfo{ICCID: card.iccid, IMSI: "001010000000001"}, nil
		},
		rebuild: func(context.Context, bool) error { return nil },
	}
	t.Cleanup(b.queue.Close)
	p := multiProfile{EID: card.eid, ICCID: "8988002", AID: "AA", Reader: rm.readerName}
	return b, card, p
}

func TestMultiCardReadsOriginalPhysicallyAndPublishesCurrent(t *testing.T) {
	b, card, _ := multiTestBroker(t)
	info, err := b.currentIdentity(context.Background())
	if err != nil || info.ICCID != card.iccid {
		t.Fatalf("original=%v err=%v", info, err)
	}
	if card.switches != 0 || card.eidReads < 2 {
		t.Fatal("original not verified without mutation")
	}
	if b.currentProfileID() != multiID("profile", card.eid, card.iccid) {
		t.Fatal("physical state not published")
	}
}

func TestMultiCardChecksEIDAfterSwitchBeforeAKA(t *testing.T) {
	b, card, p := multiTestBroker(t)
	card.onSwitch = func() { card.eid = "replacement" }
	err := b.withProfile(context.Background(), p, func(context.Context, *usimInfo) error { t.Fatal("AKA reached replacement card"); return nil })
	if !errors.Is(err, errMultiCardIdentityChanged) {
		t.Fatalf("replacement error=%v", err)
	}
	if b.currentProfileID() != "" {
		t.Fatal("stale physical profile remains")
	}
}

func TestMultiCardVerifiesICCIDAgainAfterEIDSelection(t *testing.T) {
	b, card, p := multiTestBroker(t)
	card.onEID = func(count int) {
		if count == 3 {
			card.iccid = "8988999"
		}
	}
	err := b.withProfile(context.Background(), p, func(context.Context, *usimInfo) error { t.Fatal("AKA reached wrong profile"); return nil })
	if err == nil {
		t.Fatal("late ICCID change accepted")
	}
}

func TestMultiCardNotificationFailureIsVisible(t *testing.T) {
	b, card, p := multiTestBroker(t)
	card.pending = true
	err := b.withProfile(context.Background(), p, func(context.Context, *usimInfo) error {
		t.Fatal("AKA preceded required notification completion")
		return nil
	})
	if err == nil || !strings.Contains(err.Error(), "通知") {
		t.Fatalf("notification error=%v", err)
	}
	if card.notifications != 1 {
		t.Fatal("switch notification not processed")
	}
}

func TestMultiCardNotificationRetryOnAlreadyEnabledProfile(t *testing.T) {
	for _, syncError := range []bool{false, true} {
		name := "pending"
		if syncError {
			name = "sync_error"
		}
		t.Run(name, func(t *testing.T) {
			b, card, p := multiTestBroker(t)
			card.pending = true
			if syncError {
				card.notificationErr = errors.New("fixture notification delivery failed")
			}
			akaCalls := 0
			work := func(context.Context, *usimInfo) error { akaCalls++; return nil }
			for attempt := 1; attempt <= 2; attempt++ {
				err := b.withProfile(context.Background(), p, work)
				if err == nil || !strings.Contains(err.Error(), "通知") {
					t.Fatalf("attempt %d: notification failure bypassed: err=%v", attempt, err)
				}
				if card.iccid != p.ICCID || card.switches != 1 {
					t.Fatalf("attempt %d: target must already be enabled without switching again", attempt)
				}
				if card.notifications != attempt || akaCalls != 0 {
					t.Fatalf("attempt %d: notifications=%d AKA=%d", attempt, card.notifications, akaCalls)
				}
				if b.currentProfileID() != "" {
					t.Fatal("notification failure published a verified profile")
				}
			}
			card.pending = false
			card.notificationErr = nil
			if err := b.withProfile(context.Background(), p, work); err != nil {
				t.Fatal(err)
			}
			if card.switches != 1 || card.notifications != 3 || akaCalls != 1 {
				t.Fatalf("recovered: switches=%d notifications=%d AKA=%d", card.switches, card.notifications, akaCalls)
			}
		})
	}
}

func TestMultiCardCanceledSwitchRetainsLeaseUntilRecoveryExits(t *testing.T) {
	b, card, p := multiTestBroker(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	card.onSwitch = cancel
	entered, finish := make(chan struct{}), make(chan struct{})
	b.ops.rebuild = func(recovery context.Context, _ bool) error {
		if recovery.Err() != nil {
			t.Error("recovery inherited caller cancellation")
		}
		close(entered)
		<-finish
		return nil
	}
	done := make(chan error, 1)
	go func() {
		done <- b.withProfile(ctx, p, func(context.Context, *usimInfo) error { t.Error("AKA after cancellation"); return nil })
	}()
	<-entered
	if b.rm.authMu.TryLock() {
		b.rm.authMu.Unlock()
		t.Fatal("lease released before physical recovery")
	}
	select {
	case <-done:
		t.Fatal("caller returned before physical recovery")
	default:
	}
	close(finish)
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled result=%v", err)
	}
	if !b.rm.authMu.TryLock() {
		t.Fatal("lease not released after recovery")
	}
	b.rm.authMu.Unlock()
}

func TestMultiCardLegacyEntriesReturnBusy(t *testing.T) {
	readerVoWiFiMu.Lock()
	prior := readerMultiStandbyActive
	readerMultiStandbyActive = true
	readerVoWiFiMu.Unlock()
	defer func() { readerVoWiFiMu.Lock(); readerMultiStandbyActive = prior; readerVoWiFiMu.Unlock() }()
	rm := &readerManager{readerName: "fixture-reader", generation: 1}
	if release, err := rm.beginCardOperationContext(context.Background()); err == nil {
		release()
		t.Fatal("legacy LPA bypassed reservation")
	}
	if _, err := rm.NewSIMAuthProvider().OpenLogicalChannel(context.Background(), "AA"); err == nil {
		t.Fatal("legacy logical channel bypassed reservation")
	}
	if _, err := rm.authenticateCoreAtGenerationWithNative(make([]byte, 16), make([]byte, 16), usimApplicationAIDPrefix, "USIM", 1, false); err == nil {
		t.Fatal("legacy AKA bypassed reservation")
	}
	release, err := rm.beginOwnedCardOperationContext(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	release()
}

type multiBlockingChannel struct{ entered, finish chan struct{} }

func (*multiBlockingChannel) Connect() error                          { return nil }
func (*multiBlockingChannel) OpenLogicalChannel([]byte) (byte, error) { return 1, nil }
func (*multiBlockingChannel) CloseLogicalChannel(byte) error          { return nil }
func (*multiBlockingChannel) Disconnect() error                       { return nil }
func (c *multiBlockingChannel) Transmit([]byte) ([]byte, error) {
	close(c.entered)
	<-c.finish
	return []byte{0x90, 0}, nil
}

func TestMultiCardLegacyChannelClosesOnlyAfterTransmitCompletes(t *testing.T) {
	rm := newReaderManager()
	release, err := rm.beginCardOperationContext(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	channel := &multiBlockingChannel{make(chan struct{}), make(chan struct{})}
	a := &ccidSIMAuth{rm: rm, chans: map[int]driver.SmartCardChannel{1: channel}, releases: map[int]func(){1: release}}
	txDone := make(chan error, 1)
	go func() { _, err := a.TransmitAPDU(context.Background(), 1, "00"); txDone <- err }()
	<-channel.entered
	closeDone := make(chan error, 1)
	go func() { closeDone <- a.CloseLogicalChannel(context.Background(), 1) }()
	select {
	case <-closeDone:
		t.Fatal("close raced an active APDU")
	case <-time.After(20 * time.Millisecond):
	}
	if rm.authMu.TryLock() {
		rm.authMu.Unlock()
		t.Fatal("APDU lease released early")
	}
	close(channel.finish)
	if err := <-txDone; err != nil {
		t.Fatal(err)
	}
	if err := <-closeDone; err != nil {
		t.Fatal(err)
	}
	if !rm.authMu.TryLock() {
		t.Fatal("closed channel retained lease")
	}
	rm.authMu.Unlock()
}

func TestMultiCardAuthenticationHandleIdentityMustMatch(t *testing.T) {
	expected := &usimInfo{ICCID: "8988001F", IMSI: "001010000000001"}
	if err := verifyMultiAuthIdentity("8988001", expected.IMSI, expected); err != nil {
		t.Fatal(err)
	}
	for _, actual := range []*usimInfo{{ICCID: "8988002", IMSI: expected.IMSI}, {ICCID: expected.ICCID, IMSI: "001010000000002"}, {}} {
		if err := verifyMultiAuthIdentity(actual.ICCID, actual.IMSI, expected); err == nil {
			t.Fatal("changed or missing physical identity accepted")
		}
	}
}
