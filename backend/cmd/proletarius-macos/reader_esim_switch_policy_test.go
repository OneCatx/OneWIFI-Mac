package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/damonto/euicc-go/driver"
	"proletarius.local/core/internal/esim"
)

// This adapter records only synthetic STORE DATA APDUs. Its factory replaces
// the production factory before NewManager, so no test opens PC/SC or BLE.
type readerSwitchPolicyChannel struct{ commands [][]byte }

func (*readerSwitchPolicyChannel) Connect() error                          { return nil }
func (*readerSwitchPolicyChannel) Disconnect() error                       { return nil }
func (*readerSwitchPolicyChannel) OpenLogicalChannel([]byte) (byte, error) { return 1, nil }
func (*readerSwitchPolicyChannel) CloseLogicalChannel(byte) error          { return nil }
func (c *readerSwitchPolicyChannel) Transmit(command []byte) ([]byte, error) {
	c.commands = append(c.commands, append([]byte(nil), command...))
	if len(command) < 7 || command[1] != 0xe2 || command[5] != 0xbf ||
		(command[6] != 0x31 && command[6] != 0x32) {
		return nil, fmt.Errorf("unexpected synthetic profile APDU: %X", command)
	}
	return []byte{0xbf, command[6], 3, 0x80, 1, 0, 0x90, 0}, nil
}

func TestReaderProfileSwitchNeverRequestsProactiveRefresh(t *testing.T) {
	for _, readerName := range []string{
		"ESTKme Technology Limited ESTKme-RED 00 00",
		"ESTKme Technology Limited ESTKme-RED",
		"BLE:ESTKme Red:synthetic-reader",
		"Generic EMV Smartcard Reader 00 00",
	} {
		for _, disable := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/disable=%t", readerName, disable), func(t *testing.T) {
				rm := &readerManager{}
				opts := rm.readerESIMManagerOptions(readerName)
				if opts.DeviceID != "ccid-reader" || opts.Transport != "custom" || opts.SmartCardChannelFactory == nil {
					t.Fatal("reader manager must preserve its custom transport factory")
				}
				channel := &readerSwitchPolicyChannel{}
				opts.SmartCardChannelFactory = func() (driver.SmartCardChannel, error) { return channel, nil }
				opts.PostSwitchMinDelay = time.Millisecond
				manager, err := esim.NewManager(opts)
				if err != nil {
					t.Fatal(err)
				}
				const target = "8986001234567890123"
				const aid = "A0000005591010FFFFFFFF8900000100"
				if disable {
					err = manager.DisableProfile(context.Background(), target, aid)
				} else {
					err = manager.SwitchProfile(context.Background(), target, aid)
				}
				if err != nil {
					t.Fatal(err)
				}
				if len(channel.commands) != 1 {
					t.Fatalf("writes=%d, want one command without replay", len(channel.commands))
				}
				if opts.SwitchUseRefreshTrue || !bytes.HasSuffix(channel.commands[0], []byte{0x81, 0x01, 0x00}) {
					t.Fatalf("standalone reader must use refresh=false, got policy=%t APDU=%X", opts.SwitchUseRefreshTrue, channel.commands[0])
				}
			})
		}
	}
}

func TestReaderSwitchIdentityUsesActualReadNotReaderBrand(t *testing.T) {
	const target = "8986001234567890123"
	for _, resetSupported := range []bool{false, true} {
		for _, tc := range []struct {
			name      string
			info      *usimInfo
			err       error
			confirmed bool
		}{
			{"target visible", &usimInfo{ICCID: target}, nil, true},
			{"target with card padding", &usimInfo{ICCID: " " + target + "F "}, nil, true},
			{"old profile", &usimInfo{ICCID: "8986001234567890124"}, nil, false},
			{"missing USIM", nil, nil, false},
			{"failed identity read", &usimInfo{ICCID: target}, errors.New("synthetic read failure"), false},
		} {
			t.Run(tc.name, func(t *testing.T) {
				message, code := readerSwitchIdentityWarning(target, tc.info, tc.err, resetSupported)
				if tc.confirmed {
					if message != "" || code != "" {
						t.Fatalf("confirmed target incorrectly warned: %s (%s)", message, code)
					}
				} else {
					if code != "switch_reader_identity_unconfirmed" || !strings.Contains(message, "尚未确认") || !strings.Contains(message, "不要重复切卡") {
						t.Fatalf("unconfirmed identity not clearly reported: %s (%s)", message, code)
					}
					if strings.Contains(message, "切换完成") || strings.Contains(message, "不支持软件复位") {
						t.Fatalf("unverified identity/reset capability declared conclusive: %s", message)
					}
				}
			})
		}
	}
	if message, _ := readerSwitchIdentityWarning("", &usimInfo{}, nil, false); message == "" {
		t.Fatal("empty target falsely confirmed")
	}
}
