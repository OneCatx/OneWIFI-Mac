package main

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

type deadlineRecorder struct {
	*httptest.ResponseRecorder
	deadline time.Time
	err      error
}

func TestSlowMutationSurvivesDefaultHTTPWriteDeadline(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewUnstartedServer(operationResponseDeadline(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		time.Sleep(40 * time.Millisecond)
		_, _ = w.Write([]byte("accepted"))
	})))
	server.Config.WriteTimeout = 10 * time.Millisecond
	server.Start()
	defer server.Close()
	response, err := server.Client().Post(server.URL+"/api/sms/send", "application/json", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil || string(body) != "accepted" || calls.Load() != 1 {
		t.Fatalf("body=%q err=%v calls=%d", body, err, calls.Load())
	}
}

func (w *deadlineRecorder) SetWriteDeadline(deadline time.Time) error {
	w.deadline = deadline
	return w.err
}

func TestLongOperationResponseDeadline(t *testing.T) {
	for _, path := range []string{"/api/sms/send", "/api/esim/switch", "/api/reader/esim/switch", "/api/esim/profile", "/api/reader/esim/profile", "/api/esim/download", "/api/reader/esim/download", "/api/vowifi", "/api/reader/vowifi"} {
		if responseWriteTimeout(http.MethodPost, path) != longResponseWriteTimeout {
			t.Errorf("missing long operation %s", path)
		}
		if responseWriteTimeout(http.MethodGet, path) != defaultResponseWriteTimeout {
			t.Errorf("read request gained long deadline %s", path)
		}
	}
	for _, path := range []string{"/api/status", "/api/sms/send/other", "/api/call/dial"} {
		if responseWriteTimeout(http.MethodPost, path) != defaultResponseWriteTimeout {
			t.Errorf("unrelated endpoint gained long deadline %s", path)
		}
	}
	calls := 0
	handler := operationResponseDeadline(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls++; w.WriteHeader(http.StatusOK) }))
	w := &deadlineRecorder{ResponseRecorder: httptest.NewRecorder()}
	before := time.Now()
	handler.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/api/sms/send", nil))
	if calls != 1 || w.deadline.Before(before.Add(longResponseWriteTimeout)) || w.deadline.After(time.Now().Add(longResponseWriteTimeout)) {
		t.Fatalf("calls=%d deadline=%s", calls, w.deadline)
	}
	w = &deadlineRecorder{ResponseRecorder: httptest.NewRecorder(), err: errors.New("closed")}
	handler.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/api/sms/send", nil))
	if calls != 1 || w.Code != http.StatusServiceUnavailable {
		t.Fatalf("failed deadline setup submitted operation: calls=%d status=%d", calls, w.Code)
	}
}
