package main

import (
	"bufio"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
)

const fixtureBLEReader = "BLE:12345678-1234-1234-1234-123456789ABC"

func withBLEBridgeReader(t *testing.T, selected string) {
	_ = withCountingBLEBridgeReader(t, selected)
}

func withCountingBLEBridgeReader(t *testing.T, selected string) *atomic.Int32 {
	state := "idle"
	if selected != "" {
		state = "connected"
	}
	return withCountingBLEBridgeStatus(t, selected, state)
}

func withCountingBLEBridgeStatus(t *testing.T, selected, state string) *atomic.Int32 {
	t.Helper()
	directory, err := os.MkdirTemp("/tmp", "estk-bridge-test-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(directory) })
	path := filepath.Join(directory, "ble.sock")
	listener, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	t.Setenv("PROLETARIUS_BLE_SOCKET", path)
	t.Setenv("PROLETARIUS_APP_TOKEN", "0123456789abcdef0123456789abcdef")
	requests := &atomic.Int32{}
	response, err := json.Marshal(struct {
		OK     bool   `json:"ok"`
		Reader string `json:"reader,omitempty"`
		State  string `json:"state"`
	}{OK: true, Reader: selected, State: state})
	if err != nil {
		t.Fatal(err)
	}
	response = append(response, '\n')
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			go func() {
				defer conn.Close()
				var request struct {
					Action string `json:"action"`
				}
				if json.NewDecoder(bufio.NewReader(conn)).Decode(&request) != nil {
					return
				}
				if request.Action == "status" {
					requests.Add(1)
					_, _ = conn.Write(response)
				}
			}()
		}
	}()
	return requests
}

// withBlockingBLEBridgeStatus lets reconciliation tests hold precisely the
// Swift bridge status RPC without involving real CoreBluetooth hardware.
func withBlockingBLEBridgeStatus(t *testing.T, selected, state string) (<-chan struct{}, func()) {
	t.Helper()
	directory, err := os.MkdirTemp("/tmp", "estk-bridge-blocking-test-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(directory) })
	path := filepath.Join(directory, "ble.sock")
	listener, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	t.Setenv("PROLETARIUS_BLE_SOCKET", path)
	t.Setenv("PROLETARIUS_APP_TOKEN", "0123456789abcdef0123456789abcdef")
	response, err := json.Marshal(struct {
		OK     bool   `json:"ok"`
		Reader string `json:"reader,omitempty"`
		State  string `json:"state"`
	}{OK: true, Reader: selected, State: state})
	if err != nil {
		t.Fatal(err)
	}
	response = append(response, '\n')
	entered := make(chan struct{})
	release := make(chan struct{})
	var enteredOnce, releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	t.Cleanup(unblock)
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			go func() {
				defer conn.Close()
				var request struct {
					Action string `json:"action"`
				}
				if json.NewDecoder(bufio.NewReader(conn)).Decode(&request) != nil {
					return
				}
				if request.Action != "status" {
					return
				}
				enteredOnce.Do(func() { close(entered) })
				<-release
				_, _ = conn.Write(response)
			}()
		}
	}()
	return entered, unblock
}

func withFixtureBLEBridge(t *testing.T) {
	withBLEBridgeReader(t, fixtureBLEReader)
}

func TestBLEInventoryDoesNotDisappearDuringPCSCReconcile(t *testing.T) {
	withFixtureBLEBridge(t)
	manager := newReaderManager()
	readers, err := manager.ListReaders()
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(readers, fixtureBLEReader) {
		t.Fatalf("BLE missing from %q", readers)
	}
	manager.readerName = fixtureBLEReader
	if manager.ReconcilePresence(nil) {
		t.Fatal("connected BLE reader cleared by PC/SC-only inventory")
	}
	if !manager.Status().Connected {
		t.Fatal("BLE session no longer selected")
	}
}

func TestBLEReconcileRejectsStaleInventoryAfterBridgeLoss(t *testing.T) {
	withBLEBridgeReader(t, "")
	manager := newReaderManager()
	manager.readerName = fixtureBLEReader
	// Even if a caller passes an old list, a successful bridge status with no
	// selected reader is the source of truth. Bridge transport errors alone do
	// not prove a physical unplug.
	if !manager.ReconcilePresence([]string{fixtureBLEReader}) {
		t.Fatal("stale BLE inventory kept a disconnected card session")
	}
	if manager.Status().Connected {
		t.Fatal("disconnected BLE card still selected")
	}
}
