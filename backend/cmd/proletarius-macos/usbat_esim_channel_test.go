package main

import (
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestUSBATESIMChannelHoldsModuleSIMTransactionLease(t *testing.T) {
	var transactionMu sync.Mutex
	channel := newUSBATESIMChannel(func(command string, _ time.Duration) (string, error) {
		switch {
		case strings.HasPrefix(command, `AT+CCHO=`):
			return "+CCHO: 1\r\nOK", nil
		case strings.HasPrefix(command, "AT+CGLA="):
			return `+CGLA: 1,"9000"` + "\r\nOK", nil
		case strings.HasPrefix(command, "AT+CCHC="):
			return "OK", nil
		default:
			t.Fatalf("unexpected AT command %q", command)
			return "", nil
		}
	}, &transactionMu)

	if err := channel.Connect(); err != nil {
		t.Fatal(err)
	}
	if transactionMu.TryLock() {
		transactionMu.Unlock()
		t.Fatal("eSIM reader failed to hold the shared module SIM/APDU lease")
	}
	if _, err := channel.OpenLogicalChannel([]byte{0xA0, 0x00}); err != nil {
		t.Fatal(err)
	}
	if _, err := channel.Transmit([]byte{0x00, 0xA4, 0x00, 0x00}); err != nil {
		t.Fatal(err)
	}
	if err := channel.CloseLogicalChannel(1); err != nil {
		t.Fatal(err)
	}
	if !transactionMu.TryLock() {
		t.Fatal("closing the eSIM channel did not release the module SIM/APDU lease")
	}
	transactionMu.Unlock()
}

func TestUSBATESIMChannelReleasesLeaseWhenLogicalCloseFails(t *testing.T) {
	var transactionMu sync.Mutex
	closeErr := errors.New("temporary USB AT failure")
	channel := newUSBATESIMChannel(func(command string, _ time.Duration) (string, error) {
		if strings.HasPrefix(command, "AT+CCHC=") {
			return "", closeErr
		}
		return "+CCHO: 1\r\nOK", nil
	}, &transactionMu)
	if err := channel.Connect(); err != nil {
		t.Fatal(err)
	}
	if _, err := channel.OpenLogicalChannel([]byte{0xA0, 0x00}); err != nil {
		t.Fatal(err)
	}
	if err := channel.CloseLogicalChannel(1); !errors.Is(err, closeErr) {
		t.Fatalf("CloseLogicalChannel error=%v, want %v", err, closeErr)
	}
	if !transactionMu.TryLock() {
		t.Fatal("failed CCHC left the shared SIM/APDU lease locked")
	}
	transactionMu.Unlock()
}
