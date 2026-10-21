package main

import (
	"errors"
	"io"
	"log/slog"
	"reflect"
	"sync"
	"testing"

	"github.com/damonto/euicc-go/lpa"
)

type ownedReaderChannelFixture struct {
	closeErr, disconnectErr     error
	closeCalls, disconnectCalls int
	order                       []string
}

func (*ownedReaderChannelFixture) Connect() error                          { return nil }
func (*ownedReaderChannelFixture) OpenLogicalChannel([]byte) (byte, error) { return 1, nil }
func (*ownedReaderChannelFixture) Transmit([]byte) ([]byte, error)         { return []byte{0x90, 0}, nil }
func (f *ownedReaderChannelFixture) CloseLogicalChannel(byte) error {
	f.closeCalls++
	f.order = append(f.order, "logical_close")
	return f.closeErr
}
func (f *ownedReaderChannelFixture) Disconnect() error {
	f.disconnectCalls++
	f.order = append(f.order, "physical_disconnect")
	return f.disconnectErr
}

func newOwnedReaderLPAFixture(t *testing.T, channel *ownedReaderChannelFixture) *lpa.Client {
	t.Helper()
	client, err := lpa.New(&lpa.Options{Channel: ownReaderChannel(channel), AID: []byte{0xA0, 0}, MSS: 120, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	if err != nil {
		t.Fatal(err)
	}
	return client
}

func TestReaderOwnedChannelReleasesPhysicalHandleAfterLogicalCloseFailure(t *testing.T) {
	logicalErr := errors.New("fixture logical channel invalidated by card reset")
	disconnectErr := errors.New("fixture physical disconnect error")
	for _, physicalErr := range []error{nil, disconnectErr} {
		channel := &ownedReaderChannelFixture{closeErr: logicalErr, disconnectErr: physicalErr}
		client := newOwnedReaderLPAFixture(t, channel)
		err := client.Close()
		if !errors.Is(err, logicalErr) || (physicalErr != nil && !errors.Is(err, physicalErr)) {
			t.Fatalf("Close lost cleanup errors: %v", err)
		}
		if channel.closeCalls != 1 || channel.disconnectCalls != 1 {
			t.Fatalf("logical close=%d physical disconnect=%d; leaked card blocks exclusive reset", channel.closeCalls, channel.disconnectCalls)
		}
		if !reflect.DeepEqual(channel.order, []string{"logical_close", "physical_disconnect"}) {
			t.Fatalf("cleanup order=%v", channel.order)
		}
		// The manager retries a failed LPA close. A new client may now own a
		// reused PC/SC handle value; the old wrapper must not disconnect again.
		replacement := &ownedReaderChannelFixture{}
		newClient := newOwnedReaderLPAFixture(t, replacement)
		if retryErr := client.Close(); !errors.Is(retryErr, logicalErr) {
			t.Fatalf("repeated close lost original error: %v", retryErr)
		}
		if channel.closeCalls != 1 || channel.disconnectCalls != 1 || replacement.disconnectCalls != 0 {
			t.Fatal("repeated close reused an old handle or closed the replacement")
		}
		if err := newClient.Close(); err != nil || replacement.disconnectCalls != 1 {
			t.Fatalf("replacement cleanup failed: err=%v disconnect=%d", err, replacement.disconnectCalls)
		}
	}
}

func TestReaderOwnedChannelSuccessfulCloseIsIdempotent(t *testing.T) {
	channel := &ownedReaderChannelFixture{}
	client := newOwnedReaderLPAFixture(t, channel)
	var calls sync.WaitGroup
	for range 8 {
		calls.Add(1)
		go func() {
			defer calls.Done()
			if err := client.Close(); err != nil {
				t.Errorf("Close: %v", err)
			}
		}()
	}
	calls.Wait()
	if channel.closeCalls != 1 || channel.disconnectCalls != 1 {
		t.Fatalf("logical close=%d physical disconnect=%d", channel.closeCalls, channel.disconnectCalls)
	}
}
