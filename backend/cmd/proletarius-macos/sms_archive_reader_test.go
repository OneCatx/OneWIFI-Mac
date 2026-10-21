package main

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestSMSArchiveCheckedAddFailsClosed(t *testing.T) {
	for _, failure := range []string{"corrupt", "lock"} {
		t.Run(failure, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "sms-archive.json")
			archive := &smsArchive{path: path}
			if failure == "corrupt" {
				if err := os.WriteFile(path, []byte("{broken"), 0600); err != nil {
					t.Fatal(err)
				}
			} else {
				if err := os.Mkdir(path+".lock", 0700); err != nil {
					t.Fatal(err)
				}
			}
			added, err := archive.addChecked([]receivedSMS{{Sender: "888", Content: "fixture"}})
			if err == nil || added != 0 {
				t.Fatalf("added=%d error=%v", added, err)
			}
		})
	}
}

func TestSMSArchiveCheckedDuplicateIsSuccessful(t *testing.T) {
	archive := newSMSArchive(filepath.Join(t.TempDir(), "sms-archive.json"))
	items := []receivedSMS{{Sender: "888", Content: "fixture", Timestamp: time.Now()}}
	if n, err := archive.addChecked(items); err != nil || n != 1 {
		t.Fatalf("first add: %d %v", n, err)
	}
	if n, err := archive.addChecked(items); err != nil || n != 0 {
		t.Fatalf("duplicate: %d %v", n, err)
	}
}

func TestRecordSMSPersistsIncomingAndDeduplicates(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sms-archive.json")
	application := &app{smsArchive: newSMSArchive(path)}
	at := time.Date(2026, 8, 27, 1, 2, 3, 0, time.UTC)

	application.recordSMS("+447700900123", "hello", at)
	application.recordSMS("+447700900123", "hello", at)

	reloaded := newSMSArchive(path).snapshot()
	if len(reloaded) != 1 {
		t.Fatalf("reloaded archive length = %d, want 1", len(reloaded))
	}
	if reloaded[0].Direction != "in" || !reloaded[0].Archived {
		t.Fatalf("reloaded message = %#v, want persisted incoming archive", reloaded[0])
	}
}

func TestIndependentSMSArchivesSerializeWithoutLostUpdates(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sms-archive.json")
	archives := []*smsArchive{newSMSArchive(path), newSMSArchive(path)}
	var wait sync.WaitGroup
	for index := 0; index < 40; index++ {
		wait.Add(1)
		go func(index int) {
			defer wait.Done()
			archives[index%len(archives)].add([]receivedSMS{{
				Sender:    "888",
				Content:   fmt.Sprintf("message-%d", index),
				Timestamp: time.Unix(int64(index+1), 0).UTC(),
				Direction: "in",
				Device:    "reader",
			}})
		}(index)
	}
	wait.Wait()

	reloaded := newSMSArchive(path).snapshot()
	if len(reloaded) != 40 {
		t.Fatalf("message count = %d, want 40", len(reloaded))
	}
	for _, item := range reloaded {
		if item.ID == "" || item.ID != smsStableID(item) {
			t.Fatalf("persisted stable ID = %q for %#v", item.ID, item)
		}
	}
}

func TestCorruptSMSArchiveIsNotOverwrittenByMutation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sms-archive.json")
	corrupt := []byte("{broken")
	if err := os.WriteFile(path, corrupt, 0o600); err != nil {
		t.Fatal(err)
	}
	archive := newSMSArchive(path)
	archive.add([]receivedSMS{{
		Sender: "888", Content: "must-not-overwrite", Timestamp: time.Now(),
	}})
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(corrupt) {
		t.Fatalf("corrupt archive was overwritten: %q", got)
	}
}

func TestLegacySMSArchiveKeepsOriginalJSONShapeOnLoad(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sms-archive.json")
	data := []byte(`[{"sender":"888","content":"BALANCE","timestamp":"2026-09-01T12:03:52.123456789Z","device":"reader","future_field":"keep"}]`)
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatalf("write legacy: %v", err)
	}
	reloaded := newSMSArchive(path).snapshot()
	if len(reloaded) != 1 || reloaded[0].ID != "" {
		t.Fatalf("legacy archive should remain readable without a disk rewrite: %#v", reloaded)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read legacy after load: %v", err)
	}
	if string(after) != string(data) {
		t.Fatalf("legacy archive changed during read: got %s want %s", after, data)
	}
}

func TestClearSMSReaderLocalOnly(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sms-archive.json")
	archive := newSMSArchive(path)
	archive.add([]receivedSMS{{
		Sender: "+447700900123", Content: "hello", Timestamp: time.Now(), Direction: "in",
	}})
	application := &app{readerMode: true, smsArchive: archive}

	request := httptest.NewRequest(http.MethodPost, "/api/sms/clear",
		strings.NewReader(`{"local":true}`))
	response := httptest.NewRecorder()
	application.clearSMS(response, request)

	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
	if got := len(newSMSArchive(path).snapshot()); got != 0 {
		t.Fatalf("reloaded archive length = %d, want 0", got)
	}
}

func TestClearSMSReaderRejectsModuleWithoutDeletingLocal(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sms-archive.json")
	archive := newSMSArchive(path)
	archive.add([]receivedSMS{{
		Sender: "+447700900123", Content: "keep", Timestamp: time.Now(), Direction: "in",
	}})
	application := &app{readerMode: true, smsArchive: archive}

	request := httptest.NewRequest(http.MethodPost, "/api/sms/clear",
		strings.NewReader(`{"sm":true,"local":true}`))
	response := httptest.NewRecorder()
	application.clearSMS(response, request)

	if response.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
	if got := len(newSMSArchive(path).snapshot()); got != 1 {
		t.Fatalf("reloaded archive length = %d, want 1", got)
	}
}
