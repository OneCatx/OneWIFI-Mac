package main

import (
	"path/filepath"
	"testing"
)

func TestSwitchDataRootIsSharedByRuntimeState(t *testing.T) {
	root := t.TempDir()
	t.Setenv("SWITCH_DATA_DIR", root)
	application := &app{}
	for _, path := range []string{
		imeiConfigPath(), defaultVoWiFiConfigPath(),
		application.runtimeDirectory(), application.callModeBackupDirectory(),
		application.moduleFlightModePreferencesPath(),
	} {
		rel, err := filepath.Rel(root, path)
		if err != nil || rel == ".." || filepath.IsAbs(rel) || (len(rel) > 3 && rel[:3] == "../") {
			t.Fatalf("state escaped Switch data root: %s", path)
		}
	}
	if err := application.loadProfileNotesLocked(); err != nil {
		t.Fatal(err)
	}
	if application.profileNotesPath != filepath.Join(root, "profile-notes.json") {
		t.Fatal("profile notes did not use Switch data root")
	}
}
