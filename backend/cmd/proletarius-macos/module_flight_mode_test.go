package main

import (
	"bytes"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestParseModuleSignalLevel(t *testing.T) {
	tests := []struct {
		name     string
		response string
		want     int
		wantErr  bool
	}{
		{name: "measurable signal", response: "AT+CSQ\r\n+CSQ: 22,99\r\nOK", want: 22},
		{name: "radio disabled no signal", response: "+CSQ: 99,99", want: 99},
		{name: "out of range", response: "+CSQ: 32,99", wantErr: true},
		{name: "missing response", response: "OK", wantErr: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := parseModuleSignalLevel(test.response)
			if (err != nil) != test.wantErr {
				t.Fatalf("parseModuleSignalLevel() error = %v, wantErr %v", err, test.wantErr)
			}
			if err == nil && got != test.want {
				t.Fatalf("parseModuleSignalLevel() = %d, want %d", got, test.want)
			}
		})
	}
}

func TestModuleFlightModePreferencesRoundTripAndPrivatePermissions(t *testing.T) {
	dataDir := t.TempDir()
	app := &app{dataDir: dataDir}
	want := moduleFlightModePreferences{Enabled: true, RestoreCFUN: 1, UserConfigured: true}
	if err := app.writeModuleFlightModePreferences(want); err != nil {
		t.Fatalf("writeModuleFlightModePreferences() error = %v", err)
	}
	got, err := app.readModuleFlightModePreferences()
	if err != nil {
		t.Fatalf("readModuleFlightModePreferences() error = %v", err)
	}
	if got != want {
		t.Fatalf("preferences = %#v, want %#v", got, want)
	}
	info, err := os.Stat(filepath.Join(dataDir, "module-flight-mode.json"))
	if err != nil {
		t.Fatalf("stat preferences: %v", err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("preferences permissions = %o, want 600", info.Mode().Perm())
	}
}

func TestDefaultModuleFlightModeRespectsOnlyExplicitCellularOptOut(t *testing.T) {
	tests := []struct {
		name  string
		prefs moduleFlightModePreferences
		want  bool
	}{
		{name: "no preference defaults to airplane mode", want: true},
		{name: "legacy saved airplane mode remains enabled", prefs: moduleFlightModePreferences{Enabled: true, RestoreCFUN: 1}, want: true},
		{name: "explicit airplane mode remains enabled", prefs: moduleFlightModePreferences{Enabled: true, RestoreCFUN: 1, UserConfigured: true}, want: true},
		{name: "explicit cellular opt-out is preserved", prefs: moduleFlightModePreferences{RestoreCFUN: 1, UserConfigured: true}, want: false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := defaultModuleFlightModeShouldRun(test.prefs); got != test.want {
				t.Fatalf("defaultModuleFlightModeShouldRun(%#v) = %v, want %v", test.prefs, got, test.want)
			}
		})
	}
}

func TestModuleFlightModeBlocksCellularScanAndAttachATCommands(t *testing.T) {
	tests := []struct {
		command string
		blocked bool
	}{
		{command: "AT+CSQ", blocked: true},
		{command: "AT+CESQ", blocked: true},
		{command: "AT+CEREG?", blocked: true},
		{command: "AT+CREG?", blocked: true},
		{command: "AT+COPS=?", blocked: true},
		{command: "AT+COPS=0", blocked: true},
		{command: `AT+QENG="servingcell"`, blocked: true},
		{command: "AT+QNWINFO", blocked: true},
		{command: "AT+CGATT=1", blocked: true},
		{command: "AT+CFUN=1", blocked: true},
		{command: "AT+CFUN=4,1", blocked: true},
		{command: "AT+CFUN=4", blocked: false},
		{command: "AT+CFUN?", blocked: false},
		{command: "AT+QCCID", blocked: false},
		{command: "AT+CMGS=23", blocked: false},
		{command: `AT+CSQ; AT+QCCID`, blocked: true},
	}
	for _, test := range tests {
		t.Run(test.command, func(t *testing.T) {
			if got := moduleFlightModeBlocksATCommand(test.command); got != test.blocked {
				t.Fatalf("moduleFlightModeBlocksATCommand(%q) = %v, want %v", test.command, got, test.blocked)
			}
		})
	}
}

func TestExecuteATRejectsCellularScansUnderDefaultFlightMode(t *testing.T) {
	application := &app{dataDir: t.TempDir()}
	request := httptest.NewRequest("POST", "/api/at", bytes.NewBufferString(`{"command":"AT+CSQ"}`))
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()

	application.executeAT(response, request)

	if response.Code != 409 {
		t.Fatalf("executeAT() status = %d, want 409; body=%s", response.Code, response.Body.String())
	}
	if !strings.Contains(response.Body.String(), "module_flight_mode_blocks_cellular_probe") {
		t.Fatalf("executeAT() did not return the flight-mode rejection: %s", response.Body.String())
	}
}
