package main

import (
	"os"
	"path/filepath"
	"strings"
)

// switchDataDirectory is the shared instance namespace. Internal protocol
// identifiers deliberately retain compatibility with the existing bridges.
func switchDataDirectory() string {
	if configured := strings.TrimSpace(os.Getenv("SWITCH_DATA_DIR")); filepath.IsAbs(configured) {
		return filepath.Clean(configured)
	}
	if dir, err := os.UserConfigDir(); err == nil {
		return filepath.Join(dir, "OneWIFI")
	}
	return filepath.Join(os.Getenv("HOME"), "Library", "Application Support", "OneWIFI")
}
