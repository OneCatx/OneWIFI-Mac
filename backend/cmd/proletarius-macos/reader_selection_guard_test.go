package main

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestReaderSelectionRejectsActiveESIMOperation(t *testing.T) {
	a := &app{readerMgr: newReaderManager()}
	a.readerMgr.readerName = "selected"
	operation, started := a.beginESIMOperation("download_profile", "downloading", "")
	if !started {
		t.Fatal("could not start test eSIM operation")
	}
	a.updateESIMOperation(operation.ID, esimOperationStateRun, "install", "installing", 80)

	for _, test := range []struct {
		name string
		run  func(http.ResponseWriter)
	}{
		{"connect", func(w http.ResponseWriter) {
			a.readerConnect(w, httptest.NewRequest(http.MethodPost, "/api/reader/connect", strings.NewReader(`{"reader":"other"}`)))
		}},
		{"disconnect", func(w http.ResponseWriter) {
			a.readerDisconnect(w, httptest.NewRequest(http.MethodPost, "/api/reader/disconnect", nil))
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			test.run(recorder)
			var payload codedErrorPayload
			if err := json.Unmarshal(recorder.Body.Bytes(), &payload); err != nil {
				t.Fatal(err)
			}
			if recorder.Code != http.StatusConflict || payload.Code != "esim_operation_busy" {
				t.Fatalf("status=%d payload=%#v", recorder.Code, payload)
			}
			if got := a.readerMgr.Status().ReaderName; got != "selected" {
				t.Fatalf("reader changed during eSIM operation: %q", got)
			}
		})
	}
	a.finishESIMOperation(operation.ID, "done", nil, 0)
	recorder := httptest.NewRecorder()
	a.readerDisconnect(recorder, httptest.NewRequest(http.MethodPost, "/api/reader/disconnect", nil))
	if recorder.Code != http.StatusOK {
		t.Fatalf("disconnect after eSIM completion: status=%d body=%s", recorder.Code, recorder.Body.String())
	}
}

func TestReaderSelectionRejectsActiveVoWiFiSession(t *testing.T) {
	vowifiSessionMu.Lock()
	previousActive, previousStartup, previousRetiring := activeSession, startingVoWiFi, retiringSession
	activeSession = &vowifiSession{}
	startingVoWiFi, retiringSession = nil, nil
	vowifiSessionMu.Unlock()
	t.Cleanup(func() {
		vowifiSessionMu.Lock()
		activeSession, startingVoWiFi, retiringSession = previousActive, previousStartup, previousRetiring
		vowifiSessionMu.Unlock()
	})

	a := &app{readerMgr: newReaderManager()}
	a.readerMgr.readerName = "selected"
	recorder := httptest.NewRecorder()
	a.readerDisconnect(recorder, httptest.NewRequest(http.MethodPost, "/api/reader/disconnect", nil))
	if recorder.Code != http.StatusConflict || a.readerMgr.Status().ReaderName != "selected" {
		t.Fatalf("disconnect interrupted active VoWiFi: status=%d body=%s", recorder.Code, recorder.Body.String())
	}
}

func TestReaderSelectionDoesNotWaitForCardOperationUnderManagerLock(t *testing.T) {
	rm := newReaderManager()
	rm.readerName = "selected"
	rm.authMu.Lock()
	defer rm.authMu.Unlock()
	if err := rm.Connect("other"); !errors.Is(err, errReaderCardOperationActive) {
		t.Fatalf("connect during card operation: %v", err)
	}
	if err := rm.Disconnect(); !errors.Is(err, errReaderCardOperationActive) {
		t.Fatalf("disconnect during card operation: %v", err)
	}
	if rm.readerName != "selected" {
		t.Fatalf("selected reader changed while card lock was held: %q", rm.readerName)
	}
}
