package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"testing"
	"time"

	"proletarius.local/core/internal/vowifi/imscore"
)

// A Go test binary is used as the real Rust supervisor's child. It exercises
// production IPC/serialization and public handlers without calling main(),
// discovering hardware, opening a card, or starting a carrier session.
var multiFixtureListen = flag.String("listen", "", "isolated integration fixture Unix socket")

func TestMultiStandbyProcessFixture(t *testing.T) {
	if os.Getenv("SWITCH_INTEGRATION_FIXTURE") != "1" {
		t.Skip("run scripts/test-multistandby-integration.py")
	}
	path := strings.TrimPrefix(*multiFixtureListen, "unix:")
	root := os.Getenv("SWITCH_DATA_DIR")
	if !strings.HasPrefix(*multiFixtureListen, "unix:") || root == "" || !strings.HasPrefix(path, root+"/") {
		t.Fatal("fixture socket must be within the explicitly isolated data directory")
	}
	if os.Getenv("SWITCH_INTERNAL_TOKEN") == "" || os.Getenv("PROLETARIUS_APP_TOKEN") == "" {
		t.Fatal("fixture requires supervisor-issued credentials")
	}
	listener, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	if err := os.Chmod(path, 0600); err != nil {
		t.Fatal(err)
	}
	a := &app{}
	m := a.multiStandby()
	mux := http.NewServeMux()
	// Do not install cards/start/stop routes: every available route is known
	// to avoid hardware initialization, even for a malformed fixture request.
	mux.HandleFunc("GET /api/multistandby/profiles", a.multiProfiles)
	mux.HandleFunc("POST /api/multistandby/messages/send", a.multiSend)
	mux.HandleFunc("GET /__switch_fixture/info", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, map[string]any{"pid": os.Getpid(), "boot_id": m.bootID})
	})
	mux.HandleFunc("POST /__switch_fixture/rpc", func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			Path  string         `json:"path"`
			Input map[string]any `json:"input"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&request); err != nil {
			writeError(w, 400, "invalid fixture request")
			return
		}
		if !strings.HasPrefix(request.Path, "store/") && !strings.HasPrefix(request.Path, "reader-reservation/") {
			writeError(w, 400, "fixture RPC path denied")
			return
		}
		var input any
		if request.Input != nil {
			input = request.Input
		}
		var result any
		if err := m.store.call(r.Context(), request.Path, input, &result); err != nil {
			var rpc *multiRPCError
			if errors.As(err, &rpc) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(rpc.Status)
				_, _ = w.Write([]byte(rpc.Body))
				return
			}
			writeError(w, 502, err.Error())
			return
		}
		writeJSON(w, 200, result)
	})
	mux.HandleFunc("POST /__switch_fixture/inbound", func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			ProfileID string                   `json:"profile_id"`
			Event     imscore.IncomingSMSEvent `json:"event"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&request); err != nil {
			writeError(w, 400, "invalid fixture inbound")
			return
		}
		if err := m.inbound(r.Context(), multiProfile{ProfileID: request.ProfileID}, request.Event); err != nil {
			writeError(w, 502, err.Error())
			return
		}
		writeJSON(w, 200, map[string]any{"durable": true})
	})
	server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Proletarius-App-Token") != os.Getenv("PROLETARIUS_APP_TOKEN") {
			writeError(w, 401, "fixture app credential required")
			return
		}
		mux.ServeHTTP(w, r)
	}), ReadHeaderTimeout: 3 * time.Second}
	stopped := make(chan os.Signal, 1)
	signal.Notify(stopped, syscall.SIGTERM, syscall.SIGINT)
	defer signal.Stop(stopped)
	done := make(chan error, 1)
	go func() { done <- server.Serve(listener) }()
	select {
	case err := <-done:
		if !errors.Is(err, http.ErrServerClosed) {
			t.Fatal(err)
		}
	case <-stopped:
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		if err := server.Shutdown(ctx); err != nil {
			t.Error(err)
		}
	}
}
