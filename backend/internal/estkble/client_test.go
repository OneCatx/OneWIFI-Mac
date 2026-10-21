package estkble

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// socketDir returns a short temporary directory for unix sockets. macOS caps
// sun_path at 104 bytes, and t.TempDir() lives under the workspace build
// directory, which is long enough on a CI runner to exceed that cap.
func socketDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "estkble-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
}

func TestCancelAPDUWait(t *testing.T) {
	path := filepath.Join(socketDir(t), "ble.sock")
	listener, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	submitted := make(chan struct{})
	peerClosed := make(chan error, 1)
	go func() {
		conn, err := listener.Accept()
		if err != nil {
			peerClosed <- err
			return
		}
		defer conn.Close()
		var request bridgeRequest
		if err := json.NewDecoder(conn).Decode(&request); err != nil {
			peerClosed <- err
			return
		}
		close(submitted)
		// No response: this represents a still-owned card write. The client
		// must release its socket on cancellation, not submit the APDU again.
		_, err = io.Copy(io.Discard, conn)
		peerClosed <- err
	}()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	result := make(chan error, 1)
	client := Client{Socket: path, Token: "0123456789abcdef0123456789abcdef"}
	go func() { _, err := client.Exchange(ctx, []byte{0, 0xa4, 0, 0}); result <- err }()
	select {
	case <-submitted:
	case <-time.After(time.Second):
		t.Fatal("APDU was not submitted")
	}
	started := time.Now()
	cancel()
	select {
	case err := <-result:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("cancellation error=%v", err)
		}
		t.Logf("submitted APDU cancellation returned in %s", time.Since(started))
	case <-time.After(time.Second):
		t.Fatal("cancellation waited for the APDU deadline")
	}
	select {
	case err := <-peerClosed:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("cancelled request leaked its socket")
	}
}

func TestRequestTimeoutBudgets(t *testing.T) {
	if got := requestTimeout("status"); got != 35*time.Second {
		t.Fatalf("status timeout=%s", got)
	}
	if got := requestTimeout("apdu"); got <= 120*time.Second {
		t.Fatalf("APDU timeout=%s must exceed the Swift APDU watchdog", got)
	}
	if got := requestTimeout("reset"); got <= 120*time.Second {
		t.Fatalf("reset timeout=%s must exceed the Swift APDU watchdog", got)
	}
}

func fakeBridge(t *testing.T, respond func(bridgeRequest) bridgeResponse) Client {
	t.Helper()
	path := filepath.Join(socketDir(t), "ble.sock")
	listener, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			go func() {
				defer conn.Close()
				var request bridgeRequest
				if json.NewDecoder(bufio.NewReader(conn)).Decode(&request) == nil {
					_ = json.NewEncoder(conn).Encode(respond(request))
				}
			}()
		}
	}()
	return Client{Socket: path, Token: "0123456789abcdef0123456789abcdef"}
}

func TestClientStatusAndAPDU(t *testing.T) {
	client := fakeBridge(t, func(request bridgeRequest) bridgeResponse {
		if request.Token != "0123456789abcdef0123456789abcdef" {
			return bridgeResponse{ErrorCode: "auth"}
		}
		switch request.Action {
		case "status":
			return bridgeResponse{OK: true, Reader: "BLE:fixture", State: "connected"}
		case "apdu":
			if request.Hex != "00a40000" {
				return bridgeResponse{ErrorCode: "bad_apdu"}
			}
			return bridgeResponse{OK: true, Hex: "9000"}
		default:
			return bridgeResponse{ErrorCode: "unknown"}
		}
	})
	reader, err := client.Reader(context.Background())
	if err != nil || reader != "BLE:fixture" {
		t.Fatalf("reader=%q err=%v", reader, err)
	}
	_, selected, state, code, err := client.Status(context.Background())
	if err != nil || selected != reader || state != "connected" || code != "" {
		t.Fatalf("status reader=%q state=%q code=%q err=%v", selected, state, code, err)
	}
	answer, err := client.Exchange(context.Background(), []byte{0, 0xa4, 0, 0})
	if err != nil || len(answer) != 2 || answer[0] != 0x90 {
		t.Fatalf("answer=%X err=%v", answer, err)
	}
	if _, err := client.Exchange(context.Background(), nil); err == nil {
		t.Fatal("empty APDU accepted")
	}
}

func TestClientRejectsBridgeError(t *testing.T) {
	client := fakeBridge(t, func(bridgeRequest) bridgeResponse {
		return bridgeResponse{OK: false, ErrorCode: "estk_apdu_timeout"}
	})
	if _, err := client.Exchange(context.Background(), []byte{0}); err == nil {
		t.Fatal("bridge timeout accepted")
	}
}
