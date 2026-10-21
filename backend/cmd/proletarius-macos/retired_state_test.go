package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestRemoveRetiredIntegrationStatePreservesDeviceData(t *testing.T) {
	dir := t.TempDir()
	retired := []string{
		"cli-access.json",
		"cli-runtime.json",
		"network-routing.json",
		"network-core.json",
		"network-core.log",
	}
	for _, name := range retired {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("retired"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	preserved := []string{
		"sms-archive.json",
		"call-log.json",
		"reader-vowifi.json",
		"carrier-overrides.yaml",
	}
	for _, name := range preserved {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("keep"), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	removeRetiredIntegrationState(dir)

	for _, name := range retired {
		if _, err := os.Stat(filepath.Join(dir, name)); !os.IsNotExist(err) {
			t.Fatalf("retired state %s still exists: %v", name, err)
		}
	}
	for _, name := range preserved {
		if data, err := os.ReadFile(filepath.Join(dir, name)); err != nil || string(data) != "keep" {
			t.Fatalf("device state %s changed: data=%q err=%v", name, data, err)
		}
	}
}
