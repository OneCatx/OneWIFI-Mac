package driver

import (
	"bytes"
	"fmt"
	"testing"
)

func TestStoreDataBoundaryFinalBlock(t *testing.T) {
	const mss = 120
	// Cover exact MSS multiples and byte-sized block-counter wrap. P2 wraps
	// modulo 256 as before; only the actual final payload has P1=0x91.
	for _, length := range []int{119, 120, 121, 240, 120 * 255, 120 * 256, 120 * 257, 120*257 + 1} {
		t.Run(fmt.Sprintf("bytes_%d", length), func(t *testing.T) {
			blocks := (length + mss - 1) / mss
			channel := &fakeSmartCardChannel{logicalChannel: 1}
			for range blocks {
				channel.responses = append(channel.responses, []byte{0x90, 0x00})
			}
			tx, err := NewTransmitter(discardLogger(), channel, []byte{0xa0}, mss)
			if err != nil {
				t.Fatal(err)
			}
			command := bytes.Repeat([]byte{0x5a}, length)
			if _, err := tx.TransmitRaw(command); err != nil {
				t.Fatal(err)
			}
			if len(channel.requests) != blocks {
				t.Fatalf("frames=%d want=%d", len(channel.requests), blocks)
			}
			var restored []byte
			for index, frame := range channel.requests {
				p1 := byte(0x11)
				if index == blocks-1 {
					p1 = 0x91
				}
				if len(frame) < 6 || frame[0] != 0x81 || frame[1] != 0xe2 || frame[2] != p1 || frame[3] != byte(index&0xff) {
					t.Fatalf("block %d header=%x, want P1=%02x P2=%02x", index, frame[:min(5, len(frame))], p1, byte(index&0xff))
				}
				if int(frame[4]) != len(frame)-5 || len(frame)-5 > mss {
					t.Fatalf("invalid short APDU length %d", len(frame))
				}
				restored = append(restored, frame[5:]...)
			}
			if !bytes.Equal(restored, command) {
				t.Fatal("fragmentation changed payload")
			}
		})
	}
}

func TestStoreDataRejectsEmptyAndInvalidMSS(t *testing.T) {
	for _, mss := range []int{-1, 0, 255, 256} {
		channel := &fakeSmartCardChannel{logicalChannel: 1}
		if _, err := NewTransmitter(discardLogger(), channel, []byte{0xa0}, mss); err == nil {
			t.Errorf("accepted invalid MSS=%d", mss)
		}
		if channel.connected || len(channel.requests) != 0 {
			t.Errorf("invalid MSS=%d touched card", mss)
		}
	}
	channel := &fakeSmartCardChannel{logicalChannel: 1}
	tx, err := NewTransmitter(discardLogger(), channel, []byte{0xa0}, 120)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.TransmitRaw(nil); err == nil {
		t.Fatal("empty STORE DATA silently succeeded")
	}
	if len(channel.requests) != 0 {
		t.Fatal("empty input submitted an APDU")
	}
}

func TestStoreDataAcceptsMSSBounds(t *testing.T) {
	for _, mss := range []int{1, 254} {
		channel := &fakeSmartCardChannel{logicalChannel: 1, responses: [][]byte{{0x90, 0}, {0x90, 0}}}
		tx, err := NewTransmitter(discardLogger(), channel, []byte{0xa0}, mss)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := tx.TransmitRaw(bytes.Repeat([]byte{0x5a}, mss*2)); err != nil {
			t.Fatal(err)
		}
		if len(channel.requests) != 2 || channel.requests[0][2] != 0x11 || channel.requests[1][2] != 0x91 {
			t.Fatalf("MSS %d did not preserve two frames and final marker", mss)
		}
		for _, frame := range channel.requests {
			if len(frame) != mss+5 || int(frame[4]) != mss {
				t.Fatalf("MSS %d: wrong APDU length %d", mss, len(frame))
			}
		}
	}
}

func TestStoreDataTransportFailureIsNotReplayed(t *testing.T) {
	channel := &fakeSmartCardChannel{logicalChannel: 1, responses: [][]byte{{0x90, 0}}}
	tx, err := NewTransmitter(discardLogger(), channel, []byte{0xa0}, 120)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.TransmitRaw(bytes.Repeat([]byte{0x5a}, 360)); err == nil {
		t.Fatal("failed second APDU accepted")
	}
	if len(channel.requests) != 2 {
		t.Fatalf("replayed or continued a failed card write: %d requests", len(channel.requests))
	}
}
