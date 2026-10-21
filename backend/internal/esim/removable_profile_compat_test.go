package esim

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/damonto/euicc-go/driver"
	"github.com/damonto/euicc-go/lpa"
	sgp22 "github.com/damonto/euicc-go/v2"
)

// This is an APDU-only adapter, not a PC/SC or Bluetooth connection. The real
// manager, LPA request encoder and transmitter construct each enable/disable.
type removableProfileChannel struct{ writes [][]byte }

func (*removableProfileChannel) Connect() error                          { return nil }
func (*removableProfileChannel) Disconnect() error                       { return nil }
func (*removableProfileChannel) OpenLogicalChannel([]byte) (byte, error) { return 1, nil }
func (*removableProfileChannel) CloseLogicalChannel(byte) error          { return nil }
func (c *removableProfileChannel) Transmit(command []byte) ([]byte, error) {
	c.writes = append(c.writes, append([]byte(nil), command...))
	if len(command) < 7 || command[5] != 0xbf || (command[6] != 0x31 && command[6] != 0x32) {
		return nil, fmt.Errorf("unexpected synthetic APDU: %X", command)
	}
	return []byte{0xbf, command[6], 3, 0x80, 1, 0, 0x90, 0}, nil
}

func TestRemovableProfileEnableAndDisableUseSameRefreshPolicy(t *testing.T) {
	for _, refresh := range []bool{false, true} {
		for _, disable := range []bool{false, true} {
			t.Run(fmt.Sprintf("refresh=%t/disable=%t", refresh, disable), func(t *testing.T) {
				channel := &removableProfileChannel{}
				manager, err := NewManager(ManagerOptions{
					DeviceID: "offline-removable-euicc", Transport: "custom",
					SwitchUseRefreshTrue: refresh, PostSwitchMinDelay: time.Millisecond,
					SmartCardChannelFactory: func() (driver.SmartCardChannel, error) { return channel, nil },
				})
				if err != nil {
					t.Fatal(err)
				}
				manager.overviewLoader = func() (*EsimOverview, error) { return &EsimOverview{}, nil }
				if disable {
					err = manager.DisableProfile(context.Background(), "8986001234567890123", "A0000005591010FFFFFFFF8900000100")
				} else {
					err = manager.SwitchProfile(context.Background(), "8986001234567890123", "A0000005591010FFFFFFFF8900000100")
				}
				if err != nil {
					t.Fatal(err)
				}
				if len(channel.writes) != 1 {
					t.Fatalf("writes=%d, want exactly one", len(channel.writes))
				}
				flag := byte(0)
				if refresh {
					flag = 0xff
				}
				if !bytes.HasSuffix(channel.writes[0], []byte{0x81, 1, flag}) {
					t.Fatalf("APDU refresh flag not honored: %X", channel.writes[0])
				}
			})
		}
	}
}

func TestAlreadyEnabledProfileRequiresFreshExactReadAndNeverReplays(t *testing.T) {
	const target = "8986001234567890123"
	already := &sgp22.ProfileOperationError{
		Operation: sgp22.EnableProfile, Result: sgp22.ProfileOperationResultProfileNotInDisabledState,
	}
	for _, tc := range []struct {
		name          string
		enableErr     error
		profiles      []*sgp22.ProfileInfo
		readFails     bool
		wantConfirmed bool
		wantReads     int32
		nilContext    bool
	}{
		{"exact active target", already, []*sgp22.ProfileInfo{{ICCID: mustTestICCID(t, target), ProfileState: sgp22.ProfileEnabled}}, false, true, 1, false},
		{"nil context exact active target", already, []*sgp22.ProfileInfo{{ICCID: mustTestICCID(t, target), ProfileState: sgp22.ProfileEnabled}}, false, true, 1, true},
		{"target remains disabled", already, []*sgp22.ProfileInfo{{ICCID: mustTestICCID(t, target), ProfileState: sgp22.ProfileDisabled}}, false, false, 1, false},
		{"another target active", already, []*sgp22.ProfileInfo{{ICCID: mustTestICCID(t, "8986001234567890124"), ProfileState: sgp22.ProfileEnabled}}, false, false, 1, false},
		{"empty list", already, nil, false, false, 1, false},
		{"new channel unavailable", already, nil, true, false, 0, false},
		{"proactive status is not assumed success", errors.New("returned an unexpected response with status 910B"), nil, false, false, 0, false},
		{"transport text is not an explicit state result", errors.New("profileNotInDisabledState"), nil, false, false, 0, false},
		{"disable response is not enable confirmation", &sgp22.ProfileOperationError{Operation: sgp22.DisableProfile, Result: sgp22.ProfileOperationResultProfileNotInEnabledState}, nil, false, false, 0, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var writes, reads, opens atomic.Int32
			manager := newManagerWithChannelFactory("offline-removable-euicc", func([]byte) (*lpa.Client, error) {
				if opens.Add(1) == 1 {
					return &lpa.Client{APDU: fakeProfileOperationTransmitter{calls: &writes, err: tc.enableErr}}, nil
				}
				if tc.readFails {
					return nil, errors.New("synthetic reopen unavailable")
				}
				return &lpa.Client{APDU: fakeProfileOperationTransmitter{calls: &writes, listCalls: &reads, profiles: tc.profiles}}, nil
			}, nil, nil, nil)
			manager.closeClient = func(*lpa.Client) error { return nil }
			manager.postSwitchMinDelay = time.Millisecond
			var ctx context.Context = context.Background()
			if tc.nilContext {
				ctx = nil
			}
			result, err := manager.SwitchProfileWithResult(ctx, target, "A0000005591010FFFFFFFF8900000100")
			if (err == nil) != tc.wantConfirmed || result.SwitchAccepted != tc.wantConfirmed {
				t.Fatalf("accepted=%v err=%v, want confirmed=%v", result.SwitchAccepted, err, tc.wantConfirmed)
			}
			if writes.Load() != 1 || reads.Load() != tc.wantReads {
				t.Fatalf("writes=%d reads=%d; want exactly one write and %d reads", writes.Load(), reads.Load(), tc.wantReads)
			}
			if !tc.wantConfirmed && !errors.Is(err, tc.enableErr) {
				t.Fatalf("original error lost: %v", err)
			}
		})
	}
}
