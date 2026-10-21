package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestReaderOverrideStoreReloadsAndFailsClosed(t *testing.T) {
	path := filepath.Join(t.TempDir(), "carrier-overrides.yaml")
	store := newVoWiFiOverrideStore(path)
	if got, matched, err := store.match("310", "260"); err != nil || matched || got != nil {
		t.Fatalf("missing file got=%#v matched=%t err=%v", got, matched, err)
	}
	if err := os.WriteFile(path, []byte("carrier_overrides:\n  \"310/260\": {id: live}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	got, matched, err := store.match("310", "260")
	if err != nil || !matched || got == nil || got.ID == nil || *got.ID != "live" {
		t.Fatalf("loaded got=%#v matched=%t err=%v", got, matched, err)
	}
	if err := os.WriteFile(path, []byte("carrier_overrides:\n  \"310/260\": {unknown: true}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got, matched, err := store.match("310", "260"); err == nil || matched || got != nil {
		t.Fatalf("invalid update got=%#v matched=%t err=%v", got, matched, err)
	}
	_, lastError := store.status()
	if lastError == "" {
		t.Fatal("invalid reload was not exposed in status")
	}
}
