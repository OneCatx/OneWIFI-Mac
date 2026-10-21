package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"proletarius.local/core/internal/vowifi/carrier"
)

const voWiFiConfigEnvironment = "PROLETARIUS_VOWIFI_CONFIG"

// voWiFiOverrideStore provides one carrier-configuration source to both the
// PC/SC reader and DJI module VoWiFi sessions. It only affects host-routed
// VoWiFi; native cellular, AT/QMI, voice and SMS operation is untouched.
type voWiFiOverrideStore struct {
	mu sync.Mutex

	path      string
	set       *carrier.OverrideSet
	lastError string
}

func newVoWiFiOverrideStore(configPath string) *voWiFiOverrideStore {
	path := strings.TrimSpace(configPath)
	if path == "" {
		path = strings.TrimSpace(os.Getenv(voWiFiConfigEnvironment))
	}
	if path == "" {
		path = defaultVoWiFiConfigPath()
	} else if absolute, err := filepath.Abs(path); err == nil {
		path = absolute
	}
	return &voWiFiOverrideStore{
		path: path,
		set:  carrier.EmptyOverrideSet(),
	}
}

func defaultVoWiFiConfigPath() string {
	return filepath.Join(switchDataDirectory(), "carrier-overrides.yaml")
}

// match reloads the small file before each status/start resolution. A running
// session keeps its already-resolved copy; edits take effect on the next start.
// Invalid new content is never hidden behind a stale last-known-good snapshot.
func (s *voWiFiOverrideStore) match(mcc, mnc string) (*carrier.CarrierOverride, bool, error) {
	if s == nil {
		return nil, false, nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	data, err := os.ReadFile(s.path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			s.set = carrier.EmptyOverrideSet()
			s.lastError = ""
			return nil, false, nil
		}
		s.lastError = err.Error()
		return nil, false, fmt.Errorf("读取 VoWiFi 运营商覆盖配置 %s: %w", s.path, err)
	}
	if len(strings.TrimSpace(string(data))) == 0 {
		s.set = carrier.EmptyOverrideSet()
		s.lastError = ""
		return nil, false, nil
	}
	set, err := carrier.LoadOverrides(data)
	if err != nil {
		s.set = carrier.EmptyOverrideSet()
		s.lastError = err.Error()
		return nil, false, fmt.Errorf("VoWiFi 运营商覆盖配置无效 %s: %w", s.path, err)
	}
	s.set = set
	s.lastError = ""
	override, ok := set.Match(mcc, mnc)
	return override, ok, nil
}

func (s *voWiFiOverrideStore) status() (path, lastError string) {
	if s == nil {
		return defaultVoWiFiConfigPath(), ""
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.path, s.lastError
}
