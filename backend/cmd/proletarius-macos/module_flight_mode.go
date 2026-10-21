package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

type moduleFlightModePreferences struct {
	Enabled        bool `json:"enabled"`
	RestoreCFUN    int  `json:"restore_cfun"`
	UserConfigured bool `json:"user_configured"`
}

type moduleFlightModeStatus struct {
	Available          bool   `json:"available"`
	Enabled            bool   `json:"enabled"`
	Verified           bool   `json:"verified"`
	UserConfigured     bool   `json:"user_configured"`
	CFUNMode           int    `json:"cfun_mode"`
	Owner              string `json:"owner"`
	CanToggle          bool   `json:"can_toggle"`
	RegistrationKnown  bool   `json:"registration_known"`
	CellularRegistered *bool  `json:"cellular_registered,omitempty"`
	CEREGState         *int   `json:"cereg_state,omitempty"`
	CREGState          *int   `json:"creg_state,omitempty"`
	SignalKnown        bool   `json:"signal_known"`
	SignalDetected     *bool  `json:"signal_detected,omitempty"`
	SignalLevel        *int   `json:"signal_level,omitempty"`
	Summary            string `json:"summary"`
	Detail             string `json:"detail,omitempty"`
}

type moduleFlightModeRequest struct {
	Enabled       bool `json:"enabled"`
	UserInitiated bool `json:"user_initiated"`
}

func (a *app) moduleFlightModePreferencesPath() string {
	dataDir := strings.TrimSpace(a.dataDir)
	if dataDir == "" {
		dataDir = switchDataDirectory()
	}
	return filepath.Join(dataDir, "module-flight-mode.json")
}

func (a *app) readModuleFlightModePreferences() (moduleFlightModePreferences, error) {
	path := a.moduleFlightModePreferencesPath()
	if path == "" {
		return moduleFlightModePreferences{}, errors.New("flight-mode state directory unavailable")
	}
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return moduleFlightModePreferences{}, nil
	}
	if err != nil {
		return moduleFlightModePreferences{}, err
	}
	var preferences moduleFlightModePreferences
	if err := json.Unmarshal(data, &preferences); err != nil {
		return moduleFlightModePreferences{}, fmt.Errorf("parse flight-mode state: %w", err)
	}
	if preferences.Enabled && (preferences.RestoreCFUN < 0 || preferences.RestoreCFUN > 3) {
		return moduleFlightModePreferences{}, errors.New("invalid saved CFUN restore mode")
	}
	return preferences, nil
}

func (a *app) writeModuleFlightModePreferences(preferences moduleFlightModePreferences) error {
	path := a.moduleFlightModePreferencesPath()
	if path == "" {
		return errors.New("flight-mode state directory unavailable")
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	data, err := json.Marshal(preferences)
	if err != nil {
		return err
	}
	file, err := os.CreateTemp(dir, ".module-flight-mode-*.tmp")
	if err != nil {
		return err
	}
	tmpPath := file.Name()
	defer os.Remove(tmpPath)
	if err := file.Chmod(0o600); err != nil {
		_ = file.Close()
		return err
	}
	if _, err := file.Write(data); err != nil {
		_ = file.Close()
		return err
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	return os.Rename(tmpPath, path)
}

func (a *app) manualModuleFlightModeEnabled() bool {
	preferences, err := a.readModuleFlightModePreferences()
	return err == nil && preferences.Enabled
}

func (a *app) getModuleFlightMode(w http.ResponseWriter, _ *http.Request) {
	status := a.moduleFlightModeSnapshot()
	writeJSON(w, http.StatusOK, status)
}

func (a *app) moduleFlightModeSnapshot() moduleFlightModeStatus {
	status := moduleFlightModeStatus{CFUNMode: -1, Owner: "none", Summary: "模块未连接"}
	if !a.hasModuleForFlightMode() {
		return status
	}
	preferences, preferenceErr := a.readModuleFlightModePreferences()
	if preferenceErr != nil {
		status.Available = true
		status.Summary = "无法读取模块飞行模式偏好"
		status.Detail = preferenceErr.Error()
		return status
	}
	status.UserConfigured = preferences.UserConfigured
	mode, err := readModuleCFUN(a)
	if err != nil {
		status.Available = true
		status.Summary = "无法读取模块射频状态"
		status.Detail = err.Error()
		return status
	}
	status.Available = true
	status.CFUNMode = mode
	status.Enabled = mode == 4
	if status.Enabled {
		status.Summary = "蜂窝射频已关闭，正在核验"
	} else {
		status.Summary = "蜂窝射频已开启"
	}

	if a.moduleVoWiFiOwnsRadio() {
		status.Owner = "vowifi"
		status.CanToggle = false
		status.Summary = "蜂窝射频由 Wi-Fi Calling 管理"
		status.Detail = "当前由模块 VoWiFi 管理蜂窝射频；关闭 VoWiFi 后可独立切换。"
		if mode == 4 {
			status.Verified = true
		}
		return status
	}

	if preferences.Enabled && mode == 4 {
		if preferences.UserConfigured {
			status.Owner = "manual"
		} else {
			status.Owner = "default"
		}
	} else if mode == 4 {
		status.Owner = "external"
	} else {
		status.Owner = "none"
	}

	if mode == 4 {
		// Quectel CFUN=4 disables RF transmit and receive. Do not issue CSQ,
		// CREG/CEREG, COPS, or serving-cell queries while airplane mode is on.
		status.Verified = true
		status.Summary = "飞行模式已开启 · 蜂窝射频关闭"
		status.Detail = "模块已确认 CFUN=4，蜂窝射频收发已关闭；飞行模式期间不会扫描或注册蜂窝基站。"
	} else if !defaultModuleFlightModeShouldRun(preferences) {
		cereg, creg, registrationErr := readModuleRegistrationStates(a)
		if registrationErr == nil {
			status.RegistrationKnown = true
			status.CEREGState = intPointer(cereg)
			status.CREGState = intPointer(creg)
			registered := isRegisteredStatus(cereg) || isRegisteredStatus(creg)
			status.CellularRegistered = boolPointer(registered)
			if registered {
				status.Detail = "蜂窝网络已注册。"
			} else {
				status.Detail = "当前未注册蜂窝网络。"
			}
		}
		if signalLevel, signalErr := readModuleSignalLevel(a); signalErr == nil {
			status.SignalKnown = true
			status.SignalLevel = intPointer(signalLevel)
			detected := signalLevel != 99
			status.SignalDetected = boolPointer(detected)
		}
	} else {
		status.Summary = "正在关闭蜂窝射频"
		status.Detail = "飞行模式已启用；射频关闭确认前也不会查询信号、注册状态或扫描基站。"
	}

	if reason := a.moduleFlightModeBlockReason(); reason == "" {
		status.CanToggle = true
	} else if status.Detail == "" {
		status.Detail = reason
	}
	return status
}

func (a *app) moduleFlightModeBlockReason() string {
	if a == nil {
		return "模块不可用"
	}
	a.moduleVoWiFiMu.Lock()
	moduleVoWiFiOperationPending := a.moduleVoWiFiOp != nil && !a.moduleVoWiFiOp.Done
	a.moduleVoWiFiMu.Unlock()
	if moduleVoWiFiOperationPending {
		return "模块 Wi-Fi Calling 正在启停"
	}
	if a.moduleVoWiFiOwnsRadio() {
		return "模块 VoWiFi 正在管理射频"
	}
	if isActiveESIMOperation(a.currentESIMOperation()) {
		return "eSIM Profile 操作进行中"
	}
	if state := a.currentCallState().State; state != "idle" && state != "ended" && state != "failed" && state != "unavailable" {
		return "通话进行中"
	}
	a.callModeMu.RLock()
	callModeBusy := a.callModeOperation
	a.callModeMu.RUnlock()
	if callModeBusy {
		return "模块通话模式正在变更"
	}
	return ""
}

func (a *app) hasModuleForFlightMode() bool {
	return a != nil && (a.modem != nil || a.currentUSBDevice() != nil)
}

// defaultModuleFlightModeShouldRun preserves an explicit choice to leave
// airplane mode while allowing both an untouched default and an explicit
// choice to enter airplane mode to remain RF-off.
func defaultModuleFlightModeShouldRun(preferences moduleFlightModePreferences) bool {
	return !preferences.UserConfigured || preferences.Enabled
}

// shouldSuppressModuleRadioProbes fails closed to the app's safe default.
// An explicit user choice to use cellular service is the only opt-out.
func (a *app) shouldSuppressModuleRadioProbes() bool {
	preferences, err := a.readModuleFlightModePreferences()
	if err != nil || defaultModuleFlightModeShouldRun(preferences) {
		return true
	}
	mode, err := readModuleCFUN(a)
	return err == nil && mode == 4
}

// moduleFlightModeBlocksATCommand blocks diagnostic/manual AT commands that
// scan, inspect, select, or attach to cellular networks while flight mode is
// active. SIM identity and eSIM/VoWiFi commands remain available.
func moduleFlightModeBlocksATCommand(command string) bool {
	for _, part := range strings.Split(command, ";") {
		normalized := strings.ToUpper(strings.Join(strings.Fields(part), ""))
		if normalized == "" {
			continue
		}
		switch {
		case normalized == "AT+CSQ", normalized == "AT+CESQ", normalized == "AT+QCSQ":
			return true
		case normalized == "AT+CEREG?", normalized == "AT+CREG?", normalized == "AT+CGREG?":
			return true
		case normalized == "AT+COPS?", normalized == "AT+COPS=?", strings.HasPrefix(normalized, "AT+COPS="):
			return true
		case normalized == "AT+QNWINFO", strings.HasPrefix(normalized, "AT+QENG"), strings.HasPrefix(normalized, "AT+QSCAN"):
			return true
		case strings.HasPrefix(normalized, "AT+CFUN="):
			value := strings.TrimPrefix(normalized, "AT+CFUN=")
			if value != "4" {
				return true
			}
		case normalized == "AT+CGATT=1":
			return true
		}
	}
	return false
}

func (a *app) setModuleCellularStatusSuppressed(suppressed bool) {
	if a != nil && a.modem != nil {
		a.modem.SetCellularStatusSuppressed(suppressed)
		if !suppressed {
			go a.modem.RefreshDeviceInfo()
		}
	}
}

func (a *app) setModuleFlightMode(w http.ResponseWriter, r *http.Request) {
	var request moduleFlightModeRequest
	if !decodeJSON(w, r, &request) {
		return
	}
	if !a.hasModuleForFlightMode() {
		writeCodedError(w, http.StatusServiceUnavailable, "module_unavailable", "DJI 模块未连接", true)
		return
	}

	a.moduleFlightModeMu.Lock()
	defer a.moduleFlightModeMu.Unlock()
	if reason := a.moduleFlightModeBlockReason(); reason != "" {
		writeCodedError(w, http.StatusConflict, "module_radio_busy", reason, true)
		return
	}
	mode, err := readModuleCFUN(a)
	if err != nil {
		writeCodedError(w, http.StatusBadGateway, "flight_mode_read_failed", "读取模块射频状态失败："+err.Error(), true)
		return
	}
	preferences, err := a.readModuleFlightModePreferences()
	if err != nil {
		writeCodedError(w, http.StatusBadGateway, "flight_mode_state_read_failed", "读取飞行模式恢复状态失败："+err.Error(), true)
		return
	}
	if !request.UserInitiated && !defaultModuleFlightModeShouldRun(preferences) {
		writeJSON(w, http.StatusOK, a.moduleFlightModeSnapshot())
		return
	}

	if request.Enabled {
		if mode == 4 {
			if !preferences.Enabled {
				// If CFUN=4 was set outside this UI, use normal functionality as
				// the safe recovery target rather than guessing an unknown mode.
				preferences = moduleFlightModePreferences{Enabled: true, RestoreCFUN: 1, UserConfigured: request.UserInitiated}
			} else if request.UserInitiated {
				preferences.UserConfigured = true
			}
			if err := a.writeModuleFlightModePreferences(preferences); err != nil {
				writeCodedError(w, http.StatusInternalServerError, "flight_mode_state_write_failed", "保存飞行模式恢复状态失败", true)
				return
			}
			if err := waitModuleCFUN(a, 4, 3*time.Second); err != nil {
				writeCodedError(w, http.StatusBadGateway, "flight_mode_cfun_unverified", "模块未能确认 CFUN=4："+err.Error(), true)
				return
			}
			a.setModuleCellularStatusSuppressed(true)
			status := a.moduleFlightModeSnapshot()
			if !status.Verified {
				writeCodedError(w, http.StatusBadGateway, "flight_mode_snapshot_unverified", "射频状态刚刚通过核验，但状态复读未能再次确认；请刷新核对后重试", true)
				return
			}
			writeJSON(w, http.StatusOK, status)
			return
		}
		preferences = moduleFlightModePreferences{Enabled: true, RestoreCFUN: mode, UserConfigured: request.UserInitiated}
		if err := a.writeModuleFlightModePreferences(preferences); err != nil {
			writeCodedError(w, http.StatusInternalServerError, "flight_mode_state_write_failed", "保存飞行模式恢复状态失败", true)
			return
		}
		a.setModuleCellularStatusSuppressed(true)
		if err := setAndVerifyModuleCFUN(a, 4); err != nil {
			restoreErr := setAndVerifyModuleCFUN(a, preferences.RestoreCFUN)
			if restoreErr == nil {
				_ = a.writeModuleFlightModePreferences(moduleFlightModePreferences{RestoreCFUN: preferences.RestoreCFUN, UserConfigured: request.UserInitiated})
				a.setModuleCellularStatusSuppressed(false)
			}
			message := "启用模块飞行模式失败：" + err.Error()
			if restoreErr != nil {
				message += "；恢复原无线状态也未确认：" + restoreErr.Error()
			}
			writeCodedError(w, http.StatusBadGateway, "flight_mode_enable_failed", message, true)
			return
		}
		if err := waitModuleCFUN(a, 4, 3*time.Second); err != nil {
			restoreErr := setAndVerifyModuleCFUN(a, preferences.RestoreCFUN)
			if restoreErr == nil {
				_ = a.writeModuleFlightModePreferences(moduleFlightModePreferences{RestoreCFUN: preferences.RestoreCFUN, UserConfigured: request.UserInitiated})
				a.setModuleCellularStatusSuppressed(false)
			}
			message := "模块未能核验进入 CFUN=4：" + err.Error()
			if restoreErr != nil {
				message += "；恢复原无线状态也未确认：" + restoreErr.Error()
			}
			writeCodedError(w, http.StatusBadGateway, "flight_mode_cfun_unverified", message, true)
			return
		}
		status := a.moduleFlightModeSnapshot()
		if !status.Verified {
			restoreErr := setAndVerifyModuleCFUN(a, preferences.RestoreCFUN)
			if restoreErr == nil {
				_ = a.writeModuleFlightModePreferences(moduleFlightModePreferences{RestoreCFUN: preferences.RestoreCFUN, UserConfigured: request.UserInitiated})
				a.setModuleCellularStatusSuppressed(false)
			}
			message := "飞行模式已设置，但复读核验未通过，未报告成功。"
			if restoreErr != nil {
				message += "；恢复原无线状态也未确认：" + restoreErr.Error()
			}
			writeCodedError(w, http.StatusBadGateway, "flight_mode_snapshot_unverified", message, true)
			return
		}
		writeJSON(w, http.StatusOK, status)
		return
	}

	if mode != 4 {
		a.setModuleCellularStatusSuppressed(false)
		if err := a.writeModuleFlightModePreferences(moduleFlightModePreferences{UserConfigured: request.UserInitiated, RestoreCFUN: 1}); err != nil {
			writeCodedError(w, http.StatusInternalServerError, "flight_mode_state_write_failed", "已关闭飞行模式，但未能保存你的选择", true)
			return
		}
		writeJSON(w, http.StatusOK, a.moduleFlightModeSnapshot())
		return
	}
	restoreMode := 1
	if preferences.Enabled {
		restoreMode = preferences.RestoreCFUN
	}
	if err := setAndVerifyModuleCFUN(a, restoreMode); err != nil {
		writeCodedError(w, http.StatusBadGateway, "flight_mode_restore_failed", "关闭飞行模式后未能恢复原无线状态："+err.Error(), true)
		return
	}
	a.setModuleCellularStatusSuppressed(false)
	if err := a.writeModuleFlightModePreferences(moduleFlightModePreferences{UserConfigured: request.UserInitiated, RestoreCFUN: 1}); err != nil {
		writeCodedError(w, http.StatusInternalServerError, "flight_mode_state_write_failed", "无线状态已恢复，但未能保存你的选择", true)
		return
	}
	writeJSON(w, http.StatusOK, a.moduleFlightModeSnapshot())
}

func setAndVerifyModuleCFUN(a *app, expected int) error {
	if a == nil {
		return errors.New("DJI 模块未连接")
	}
	_, commandErr := a.runATCommand(fmt.Sprintf("AT+CFUN=%d", expected), 12*time.Second)
	if commandErr != nil {
		if mode, err := readModuleCFUN(a); err == nil && mode == expected {
			return nil
		}
		return fmt.Errorf("设置 CFUN=%d: %w", expected, commandErr)
	}
	return waitModuleCFUN(a, expected, 10*time.Second)
}

func readModuleSignalLevel(a *app) (int, error) {
	if a == nil {
		return 0, errors.New("DJI 模块未连接")
	}
	response, err := a.runATCommand("AT+CSQ", 4*time.Second)
	if err != nil {
		return 0, fmt.Errorf("读取 CSQ: %w", err)
	}
	return parseModuleSignalLevel(response)
}

func parseModuleSignalLevel(response string) (int, error) {
	match := moduleCSQPattern.FindStringSubmatch(response)
	if len(match) != 2 {
		return 0, errors.New("模块未返回可解析的 CSQ 信号值")
	}
	level, err := strconv.Atoi(match[1])
	if err != nil || (level > 31 && level != 99) {
		return 0, fmt.Errorf("模块返回无效 CSQ 信号值 %q", match[1])
	}
	return level, nil
}

func readModuleRegistrationStates(a *app) (int, int, error) {
	ceregResponse, ceregErr := a.runATCommand("AT+CEREG?", 4*time.Second)
	cregResponse, cregErr := a.runATCommand("AT+CREG?", 4*time.Second)
	if ceregErr != nil {
		return 0, 0, fmt.Errorf("读取 CEREG: %w", ceregErr)
	}
	if cregErr != nil {
		return 0, 0, fmt.Errorf("读取 CREG: %w", cregErr)
	}
	cereg, ceregOK := parseModuleRegistrationState(ceregResponse, moduleCEREGStatePattern)
	creg, cregOK := parseModuleRegistrationState(cregResponse, moduleCREGStatePattern)
	if !ceregOK || !cregOK {
		return 0, 0, errors.New("模块未返回可解析的 CEREG/CREG 状态")
	}
	return cereg, creg, nil
}

var (
	moduleCEREGStatePattern = regexp.MustCompile(`(?m)^\s*\+CEREG:\s*(?:(\d+)\s*,\s*)?(\d+)`)
	moduleCREGStatePattern  = regexp.MustCompile(`(?m)^\s*\+CREG:\s*(?:(\d+)\s*,\s*)?(\d+)`)
	moduleCSQPattern        = regexp.MustCompile(`(?m)^\s*\+CSQ:\s*(\d+)\s*,\s*\d+`)
)

func parseModuleRegistrationState(response string, pattern *regexp.Regexp) (int, bool) {
	match := pattern.FindStringSubmatch(response)
	if len(match) != 3 {
		return 0, false
	}
	statusText := match[2]
	status, err := strconv.Atoi(statusText)
	return status, err == nil && status >= 0 && status <= 10
}

func isRegisteredStatus(status int) bool { return status == 1 || status == 5 }

func boolPointer(value bool) *bool { return &value }

func intPointer(value int) *int { return &value }
