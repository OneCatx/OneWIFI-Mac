package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http/httptest"
	"time"
)

const moduleFlightModePolicyInterval = 3 * time.Second

// applyDefaultModuleFlightMode runs before startup eSIM/SMS work and again
// after startup by the monitor. A stored explicit cellular-mode choice is
// authoritative; otherwise the DJI module is kept out of cellular service.
func (a *app) applyDefaultModuleFlightMode() error {
	if a == nil {
		return nil
	}
	preferences, err := a.readModuleFlightModePreferences()
	if err != nil {
		return fmt.Errorf("read module flight-mode preference: %w", err)
	}
	if !defaultModuleFlightModeShouldRun(preferences) {
		return nil
	}
	if !a.hasModuleForFlightMode() {
		return nil
	}
	if a.modem == nil && a.usbAT == nil {
		if err := a.ensureUSBAT(); err != nil {
			return fmt.Errorf("open module AT channel: %w", err)
		}
	}

	// A verified RF-off state needs no transition. Still check the mode so a
	// module reset or external CFUN change is corrected promptly.
	if mode, modeErr := readModuleCFUN(a); modeErr == nil && mode == 4 && preferences.Enabled {
		if err := waitModuleCFUN(a, 4, 3*time.Second); err == nil {
			return nil
		}
	}

	body, _ := json.Marshal(moduleFlightModeRequest{Enabled: true})
	req := httptest.NewRequest("POST", "/api/module/flight-mode", bytes.NewReader(body))
	response := httptest.NewRecorder()
	a.setModuleFlightMode(response, req)
	if response.Code != 200 {
		return fmt.Errorf("default airplane-mode verification failed (HTTP %d)", response.Code)
	}
	var status moduleFlightModeStatus
	if err := json.Unmarshal(response.Body.Bytes(), &status); err != nil {
		return fmt.Errorf("decode default airplane-mode result: %w", err)
	}
	if !status.Verified || !status.Enabled {
		return fmt.Errorf("module did not confirm RF-off, no signal, and cellular deregistration")
	}
	return nil
}

func (a *app) startModuleFlightModePolicyMonitor(ctx context.Context) {
	if a == nil {
		return
	}
	go func() {
		ticker := time.NewTicker(moduleFlightModePolicyInterval)
		defer ticker.Stop()
		var lastLoggedError string
		var lastErrorAt time.Time
		for {
			if err := a.applyDefaultModuleFlightMode(); err != nil {
				now := time.Now()
				if err.Error() != lastLoggedError || now.Sub(lastErrorAt) >= 30*time.Second {
					log.Printf("module default airplane mode is pending: %v", err)
					lastLoggedError = err.Error()
					lastErrorAt = now
				}
			} else {
				lastLoggedError = ""
			}
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
		}
	}()
}
