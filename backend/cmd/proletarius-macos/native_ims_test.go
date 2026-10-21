package main

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"testing"
)

func TestNativeIMSRegisterValidationBridge(t *testing.T) {
	dir, err := os.MkdirTemp("/tmp", "pc-ims-validation-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	socket := filepath.Join(dir, "s.sock")
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("PROLETARIUS_CARRIER_SOCKET", socket)
	server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v2/ims/validate-register-response" {
			http.Error(w, "wrong route", http.StatusBadRequest)
			return
		}
		var input struct {
			Response []int  `json:"response"`
			CallID   string `json:"call_id"`
			CSeq     uint32 `json:"cseq"`
		}
		if json.NewDecoder(r.Body).Decode(&input) != nil || input.CallID != "reg-1" || input.CSeq != 7 || len(input.Response) == 0 {
			http.Error(w, "invalid request", http.StatusBadRequest)
			return
		}
		_, _ = w.Write([]byte(`{"status":401,"call_id":"reg-1","cseq":7}`))
	})}
	go func() { _ = server.Serve(listener) }()
	defer server.Close()
	response := []byte("SIP/2.0 401 Unauthorized\r\nCall-ID: reg-1\r\nCSeq: 7 REGISTER\r\nContent-Length: 0\r\n\r\n")
	if err := validateRuntimeIMSRegisterResponse(context.Background(), response, "reg-1", 7); err != nil {
		t.Fatal(err)
	}
}
