package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strings"
	"time"

	"proletarius.local/core/internal/modem"
)

// DJI module VoWiFi is deliberately separate from the PC/SC reader session.
// The module supplies SIM/APDU AKA only; host-side SWu/ESP/IMS owns the call
// and SMS path while the module radio is held in CFUN=4.
type moduleVoWiFiStatus struct {
	State                 string       `json:"state"`
	Summary               string       `json:"summary"`
	Detail                string       `json:"detail,omitempty"`
	SIMICCID              string       `json:"sim_iccid,omitempty"`
	AirplaneMode          bool         `json:"airplane_mode"`
	CFUNMode              int          `json:"cfun_mode"`
	USBNetMode            int          `json:"usbnet_mode"`
	USBNetReady           bool         `json:"usbnet_ready"`
	IMSWiFiCallingEnabled bool         `json:"ims_wifi_calling_enabled"`
	IMSCallModeSet        bool         `json:"ims_call_mode_set"`
	WiFiCallingMode       int          `json:"wifi_calling_mode"`
	EPDGAddress           string       `json:"epdg_address,omitempty"`
	EPDGIPAddress         string       `json:"epdg_ip_address,omitempty"`
	EPDGAutoDetect        bool         `json:"epdg_auto_detect"`
	EPDGFQDN              string       `json:"epdg_fqdn,omitempty"`
	OperatorMCC           string       `json:"operator_mcc,omitempty"`
	OperatorMNC           string       `json:"operator_mnc,omitempty"`
	OperatorName          string       `json:"operator_name,omitempty"`
	IMSRegistered         bool         `json:"ims_registered"`
	VoWiFiActive          bool         `json:"vowifi_active"`
	SMSReady              bool         `json:"sms_ready"`
	CallReady             bool         `json:"call_ready"`
	ProgressStep          progressStep `json:"progress_step"`
	ProgressDesc          string       `json:"progress_desc,omitempty"`
	QMIEnabled            bool         `json:"qmi_enabled"`
	QMIWiFiOnly           bool         `json:"qmi_wifi_only"`
	QMIWiFiFirst          bool         `json:"qmi_wifi_first"`
	QMIInUse              bool         `json:"qmi_in_use"`
	QMIWiFiRegistered     bool         `json:"qmi_wifi_registered"`
	QMIError              string       `json:"qmi_error,omitempty"`
}

type moduleVoWiFiRequest struct {
	Enable         *bool  `json:"enable"`
	Mode           *int   `json:"wifi_calling_mode"`
	EPDGAddress    string `json:"epdg_address"`
	EPDGIPAddress  string `json:"epdg_ip_address"`
	EPDGAutoDetect *bool  `json:"epdg_auto_detect"`
}

type moduleVoWiFiOperation struct {
	Done            bool                `json:"done"`
	Error           string              `json:"error,omitempty"`
	Status          *moduleVoWiFiStatus `json:"status,omitempty"`
	Enable          bool                `json:"-"`
	CancelRequested bool                `json:"-"`
	cancel          context.CancelFunc
}

func moduleVoWiFiUnavailable(detail string) *moduleVoWiFiStatus {
	return &moduleVoWiFiStatus{
		State: "unavailable", Summary: "模块 Wi-Fi Calling 不可用", Detail: detail,
		CFUNMode: -1, USBNetMode: -1, EPDGAutoDetect: true, QMIError: detail,
	}
}

// moduleVoWiFiStatusSnapshot prevents the dashboard's two-second device
// snapshot from issuing a dozen AT queries while the module SIM is being used
// for EAP/IMS AKA. AT+CSIM and ordinary status probes share one serial channel;
// status reads during that exchange can delay or invalidate authentication.
func (a *app) moduleVoWiFiStatusSnapshot() (modem.DeviceStatus, bool) {
	if a == nil {
		return modem.DeviceStatus{}, false
	}
	vowifiSessionMu.Lock()
	session, startup, retiring := activeSession, startingVoWiFi, retiringSession
	var sessionForIdentity *vowifiSession
	var sim *moduleVoWiFiSIM
	var lease *moduleRadioLease
	operator := ""
	held := false
	for _, candidate := range []*vowifiSession{session, retiring} {
		if candidate != nil && candidate.sourceKind == "module" {
			held = true
			if sessionForIdentity == nil {
				sessionForIdentity = candidate
			}
			if sim == nil {
				sim, lease = candidate.moduleSIM, candidate.radioLease
			}
			if candidate.carrier != nil {
				operator = candidate.carrier.Name
			}
		}
	}
	if startup != nil && startup.sourceKind == "module" {
		held = true
		if sim == nil {
			sim, lease = startup.moduleSIM, startup.radioLease
		}
	}
	vowifiSessionMu.Unlock()
	if !held {
		return modem.DeviceStatus{}, false
	}

	a.moduleStatusMu.RLock()
	var status modem.DeviceStatus
	if a.moduleStatusCache != nil {
		status = *a.moduleStatusCache
	}
	a.moduleStatusMu.RUnlock()
	if sim != nil && sim.info != nil {
		// The active VoWiFi SIM snapshot is authoritative. A cached cellular
		// status may belong to the profile that was active before the switch.
		if status.ICCID != "" && sim.info.ICCID != "" &&
			normalizeSIMIdentifier(status.ICCID) != normalizeSIMIdentifier(sim.info.ICCID) {
			status.PhoneNumber = ""
		}
		status.ICCID = sim.info.ICCID
		status.IMSI = sim.info.IMSI
		if isMissingPhoneNumber(status.PhoneNumber) {
			status.PhoneNumber = sim.info.PhoneNumber
		}
		status.SimInserted = true
	}
	if isMissingPhoneNumber(status.PhoneNumber) && sessionForIdentity != nil {
		sessionForIdentity.mu.Lock()
		var registeredNumber, activeICCID string
		// A published module session exists before IKE/IMS registration has
		// completed. Dashboard status polling is concurrent with that startup,
		// so both identity results may still be nil here.
		if sessionForIdentity.imsResult != nil {
			registeredNumber = strings.TrimSpace(sessionForIdentity.imsResult.MSISDN)
		}
		if sessionForIdentity.simInfo != nil {
			activeICCID = strings.TrimSpace(sessionForIdentity.simInfo.ICCID)
		}
		sessionForIdentity.mu.Unlock()
		if !isMissingPhoneNumber(registeredNumber) {
			status.PhoneNumber = registeredNumber
		} else if isMissingPhoneNumber(status.PhoneNumber) {
			status.PhoneNumber = a.profilePhoneForICCID(activeICCID)
		}
	}
	radioOff := false
	if lease != nil {
		lease.mu.Lock()
		radioOff = lease.active
		lease.mu.Unlock()
	}
	if radioOff {
		if operator != "" {
			status.Operator = operator
		} else if status.Operator == "" {
			status.Operator = "Wi-Fi Calling"
		}
		status.SignalDBM = -999
		status.RegStatus = 0
		status.RegStatusText = "蜂窝射频已关闭（Wi-Fi Calling）"
		status.PSAttached = false
		status.NetworkMode = "Wi-Fi Calling"
	} else if status.NetworkMode == "" {
		status.NetworkMode = "VoWiFi 正在启动"
	}
	return status, true
}

// moduleVoWiFiOwnsRadio prevents native cellular helpers from touching the
// module's shared AT/SIM channel during startup, an active session or teardown.
func (a *app) moduleVoWiFiOwnsRadio() bool {
	vowifiSessionMu.Lock()
	defer vowifiSessionMu.Unlock()
	for _, session := range []*vowifiSession{activeSession, retiringSession} {
		if session != nil && session.sourceKind == "module" {
			return true
		}
	}
	return startingVoWiFi != nil && startingVoWiFi.sourceKind == "module"
}

func (a *app) getModuleVoWiFi(w http.ResponseWriter, r *http.Request) {
	if a == nil || a.currentUSBDevice() == nil {
		writeJSON(w, http.StatusOK, moduleVoWiFiUnavailable("DJI 模块未连接"))
		return
	}
	vowifiSessionMu.Lock()
	session := activeSession
	startup := startingVoWiFi
	var startupProgressStep progressStep
	var startupProgressDesc string
	if startup != nil && startup.sourceKind == "module" {
		startupProgressStep = startup.progressStep
		startupProgressDesc = startup.progressDesc
	}
	vowifiSessionMu.Unlock()
	if session != nil && session.sourceKind != "module" {
		session = nil
	}
	var sim *moduleVoWiFiSIM
	var lease *moduleRadioLease
	if session != nil {
		sim, lease = session.moduleSIM, session.radioLease
	} else if startup != nil && startup.sourceKind == "module" {
		sim, lease = startup.moduleSIM, startup.radioLease
	}
	mode := -1
	var err error
	switch {
	case startup != nil && startup.sourceKind == "module" && sim == nil:
		// SIM discovery itself uses the AT/APDU channel. Do not race it with a
		// status probe; the five-stage startup view already reports this phase.
		mode = -1
	case lease != nil:
		mode, err = lease.observedMode(sim, 5*time.Second)
	default:
		mode, err = readModuleCFUN(a)
	}
	if err != nil {
		writeJSON(w, http.StatusOK, moduleVoWiFiUnavailable(err.Error()))
		return
	}
	if session != nil && mode != 4 {
		stopVoWiFiMatching(func(candidate *vowifiSession) bool {
			return candidate == session && candidate.sourceKind == "module"
		})
		status := moduleVoWiFiStatusFromSession(nil, mode)
		status.State = "failed"
		status.Summary = "模块已离开飞行模式，VoWiFi 会话已停止"
		status.Detail = "为避免蜂窝与 Wi-Fi Calling 状态冲突，已释放 IMS 隧道并恢复无线状态。"
		writeJSON(w, http.StatusOK, status)
		return
	}
	// Do not issue QCCID/CIMI (or SIM APDUs) on this high-frequency status
	// endpoint. The UI polls it while EAP/IMS AKA is using the same module AT
	// channel; probing identity here used to serialize behind, and sometimes
	// interleave with, authentication. Profile identity is checked at call/SMS
	// boundaries and profile changes initiated in-app stop the session first.
	status := moduleVoWiFiStatusFromSession(session, mode)
	if session == nil && startup != nil && startup.sourceKind == "module" {
		status.State = "registering"
		status.Summary = "正在通过 Mac 网络建立模块 VoWiFi"
		status.IMSWiFiCallingEnabled = true
		status.AirplaneMode = mode == 4 || mode == -1
		status.ProgressStep = startupProgressStep
		status.ProgressDesc = startupProgressDesc
		if sim != nil && sim.info != nil {
			status.SIMICCID = strings.TrimSpace(sim.info.ICCID)
		}
	}
	writeJSON(w, http.StatusOK, status)
}

func activeVoWiFiSession(source string) *vowifiSession {
	vowifiSessionMu.Lock()
	defer vowifiSessionMu.Unlock()
	if activeSession == nil || activeSession.sourceKind != source {
		return nil
	}
	return activeSession
}

func moduleVoWiFiStatusFromSession(session *vowifiSession, mode int) *moduleVoWiFiStatus {
	status := &moduleVoWiFiStatus{
		State: "disabled", Summary: "模块 Wi-Fi Calling 未启用",
		AirplaneMode: mode == 4, CFUNMode: mode, USBNetMode: -1,
		EPDGAutoDetect: true,
	}
	if session == nil {
		return status
	}
	if session.moduleSIM != nil && session.moduleSIM.info != nil {
		status.SIMICCID = strings.TrimSpace(session.moduleSIM.info.ICCID)
	}
	readerStatus := voWiFiStatusForSession(session)
	status.ProgressStep = readerStatus.ProgressStep
	status.ProgressDesc = readerStatus.ProgressDesc
	status.SMSReady = readerStatus.SMSReady
	status.CallReady = readerStatus.CallReady
	status.IMSWiFiCallingEnabled = true
	status.WiFiCallingMode = 1
	status.IMSRegistered = mode == 4 && readerStatus.IMSState == imsRegistered
	status.VoWiFiActive = mode == 4 && readerStatus.TunnelState == tunnelConnected && status.IMSRegistered
	status.EPDGFQDN = readerStatus.EPDGFQDN
	status.EPDGAddress = readerStatus.EPDGFQDN
	status.OperatorMCC, status.OperatorMNC = readerStatus.MCC, readerStatus.MNC
	if session.carrier != nil {
		status.OperatorName = session.carrier.Name
	}
	status.IMSCallModeSet = true
	status.QMIEnabled = false
	status.QMIWiFiOnly = false
	status.QMIWiFiFirst = false
	status.QMIInUse = false
	status.QMIWiFiRegistered = false
	switch {
	case status.VoWiFiActive:
		status.State, status.Summary = "active", "模块 VoWiFi 已通过 Mac 网络完成 IMS 注册"
	case mode != 4:
		status.State, status.Summary = "failed", "模块未处于 VoWiFi 飞行模式"
		status.Detail = "模块蜂窝射频已开启，拒绝将当前状态报告为 Wi-Fi Calling 在线。"
	case readerStatus.TunnelState == tunnelError || readerStatus.IMSState == imsFailed:
		status.State, status.Summary = "failed", "模块 VoWiFi 连接失败"
		status.Detail = readerStatus.Message
	case readerStatus.TunnelState == tunnelDisconnected:
		status.State, status.Summary = "registering", "模块 VoWiFi 正在启动"
	default:
		status.State, status.Summary = "registering", "正在建立 Mac 侧 ePDG 隧道与 IMS 注册"
	}
	if !status.AirplaneMode {
		status.Detail = "VoWiFi 会话存在，但模块未处于 CFUN=4；蜂窝射频状态需要复核"
	}
	return status
}

func startModuleVoWiFi(ctx context.Context, a *app) error {
	if a == nil {
		return errors.New("DJI 模块未连接")
	}
	if isActiveESIMOperation(a.currentESIMOperation()) {
		return errors.New("eSIM Profile 正在切换，请等待完成后再开启模块 VoWiFi")
	}
	if err := a.ensureUSBAT(); err != nil {
		return fmt.Errorf("连接 DJI AT 通道: %w", err)
	}
	vowifiSessionMu.Lock()
	if vowifiShuttingDown {
		vowifiSessionMu.Unlock()
		return errors.New("Switch 正在退出，未启动模块 VoWiFi")
	}
	if activeSession != nil {
		vowifiSessionMu.Unlock()
		return errors.New("已有 VoWiFi 会话正在运行，请先关闭后再切换设备")
	}
	if retiringSession != nil || startingVoWiFi != nil {
		vowifiSessionMu.Unlock()
		return errors.New("上一个 VoWiFi 操作仍在释放资源，请稍后重试")
	}
	vowifiIntentID++
	startup := newVoWiFiStartup(vowifiIntentID)
	startup.sourceKind = "module"
	startup.progressStep = progressSIM
	startup.progressDesc = "正在读取 SIM 身份与运营商信息"
	startingVoWiFi = startup
	vowifiSessionMu.Unlock()

	handedOff := false
	var lease *moduleRadioLease
	stopParentCancellation := context.AfterFunc(ctx, startup.cancel)
	defer func() {
		stopParentCancellation()
		if handedOff {
			return
		}
		startup.cancel()
		clearVoWiFiStartup(startup)
		if lease != nil {
			if err := lease.restore(); err != nil {
				log.Printf("[Module VoWiFi] Failed startup rollback: %v", err)
			}
		}
	}()

	sim, err := openModuleVoWiFiSIM(startup.ctx, a)
	if err != nil {
		return err
	}
	setVoWiFiStartupProgress(startup, progressSIM, "SIM 已识别，正在准备模块射频与 ePDG 接入")
	initialMode, err := readModuleCFUN(a)
	if err != nil {
		return err
	}
	lease = &moduleRadioLease{app: a, restoreMode: initialMode}
	vowifiSessionMu.Lock()
	if startingVoWiFi != startup || startup.ctx.Err() != nil {
		vowifiSessionMu.Unlock()
		return context.Canceled
	}
	startup.moduleSIM = sim
	startup.radioLease = lease
	startup.radioRestore = lease.restore
	vowifiSessionMu.Unlock()
	if err := lease.enter(startup.ctx); err != nil {
		return err
	}
	setVoWiFiStartupProgress(startup, progressAccess, "模块已进入 VoWiFi 射频模式，正在准备安全接入")
	if err := startReservedVoWiFi(a, "", "", 0, startup, 0); err != nil {
		return err
	}
	handedOff = true
	return nil
}

func (a *app) setModuleVoWiFi(w http.ResponseWriter, r *http.Request) {
	var request moduleVoWiFiRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&request); err != nil || request.Enable == nil {
		writeCodedError(w, http.StatusBadRequest, "invalid_vowifi_request", "Wi-Fi Calling 请求格式无效", false)
		return
	}
	if *request.Enable && (strings.TrimSpace(request.EPDGAddress) != "" || strings.TrimSpace(request.EPDGIPAddress) != "") {
		writeCodedError(w, http.StatusBadRequest, "module_epdg_unsupported", "模块固件不支持经此接口指定 ePDG，请使用自动发现；读卡器模式仍可自定义", false)
		return
	}
	// Module VoWiFi is intentionally Wi-Fi-only: the lease below enters CFUN=4
	// and the Mac network owns ePDG/IMS. Ignore old client preference fields.
	// Pair operation admission with manual flight-mode admission so a radio
	// transition cannot race the moment the VoWiFi startup/teardown is recorded.
	a.moduleFlightModeMu.Lock()
	a.moduleVoWiFiMu.Lock()
	if a.moduleVoWiFiOp != nil && !a.moduleVoWiFiOp.Done {
		if !*request.Enable && a.moduleVoWiFiOp.Enable && a.moduleVoWiFiOp.cancel != nil {
			a.moduleVoWiFiOp.CancelRequested = true
			cancel := a.moduleVoWiFiOp.cancel
			a.moduleVoWiFiMu.Unlock()
			a.moduleFlightModeMu.Unlock()
			cancel()
			writeJSON(w, http.StatusAccepted, map[string]any{"accepted": true, "cancelling": true})
			return
		}
		a.moduleVoWiFiMu.Unlock()
		a.moduleFlightModeMu.Unlock()
		writeCodedError(w, http.StatusConflict, "module_vowifi_busy", "模块 Wi-Fi Calling 操作正在进行", true)
		return
	}
	operationCtx := context.Background()
	var cancel context.CancelFunc
	if *request.Enable {
		operationCtx, cancel = context.WithTimeout(context.Background(), 90*time.Second)
	}
	a.moduleVoWiFiOp = &moduleVoWiFiOperation{Enable: *request.Enable, cancel: cancel}
	a.moduleVoWiFiMu.Unlock()
	a.moduleFlightModeMu.Unlock()
	go a.runModuleVoWiFiOperation(*request.Enable, operationCtx, cancel)
	writeJSON(w, http.StatusAccepted, map[string]any{"accepted": true})
}

// stopModuleVoWiFiForProfileSwitch releases the SIM APDU/IMS owner before the
// eUICC switch begins. It refuses to interrupt a live call and never asks the
// module firmware to reboot.
func stopModuleVoWiFiForProfileSwitch() error {
	vowifiSessionMu.Lock()
	startup := startingVoWiFi
	session := activeSession
	retiring := retiringSession
	if startup != nil && startup.sourceKind == "module" {
		vowifiSessionMu.Unlock()
		return errors.New("模块 VoWiFi 正在建立连接，请先等待连接完成或关闭后再切换 Profile")
	}
	if retiring != nil && retiring.sourceKind == "module" {
		vowifiSessionMu.Unlock()
		return stopModuleVoWiFiSession()
	}
	if session == nil || session.sourceKind != "module" {
		vowifiSessionMu.Unlock()
		return nil
	}
	session.mu.Lock()
	service := session.imsService
	session.mu.Unlock()
	vowifiSessionMu.Unlock()
	if service != nil {
		switch service.CallStatus().State {
		case "idle", "ended", "failed", "unavailable":
		default:
			return errors.New("模块 VoWiFi 正在通话，请挂断后切换 Profile")
		}
	}
	return stopModuleVoWiFiSession()
}

func stopModuleVoWiFiSession() error {
	vowifiSessionMu.Lock()
	session := activeSession
	startup := startingVoWiFi
	retiring := retiringSession
	alreadyRetiring := false
	if session != nil && session.sourceKind != "module" {
		session = nil
	}
	if startup != nil && startup.sourceKind != "module" {
		startup = nil
	}
	if retiring != nil && retiring.sourceKind == "module" {
		session = retiring
		startup = nil
		alreadyRetiring = true
	}
	if session == nil && startup == nil {
		vowifiSessionMu.Unlock()
		return nil
	}
	var lease *moduleRadioLease
	if session != nil {
		lease = session.radioLease
	} else {
		lease = startup.radioLease
	}
	vowifiSessionMu.Unlock()

	if !alreadyRetiring {
		stopped := stopVoWiFiMatching(func(candidate *vowifiSession) bool {
			return candidate != nil && candidate.sourceKind == "module"
		})
		_ = stopped // The startup/session may have completed between the snapshot and cancellation.
	}
	deadline := time.Now().Add(30 * time.Second)
	for {
		vowifiSessionMu.Lock()
		stillRunning := startingVoWiFi != nil && startingVoWiFi.sourceKind == "module"
		stillActive := activeSession != nil && activeSession.sourceKind == "module"
		stillRetiring := retiringSession != nil && retiringSession.sourceKind == "module"
		vowifiSessionMu.Unlock()
		if !stillRunning && !stillActive && !stillRetiring {
			break
		}
		if time.Now().After(deadline) {
			return errors.New("模块 VoWiFi 隧道资源释放超时；无线状态正在后台恢复，请稍后刷新")
		}
		time.Sleep(200 * time.Millisecond)
	}
	if lease != nil {
		// cleanupResources performs the first restore attempt. Retry here while
		// the toggle-off operation still owns the lease, so a transient USB AT
		// timeout does not strand the radio in CFUN=4 until a manual reboot.
		if err := lease.restore(); err != nil {
			return fmt.Errorf("关闭模块 VoWiFi 后未能恢复原无线状态: %w", err)
		}
		if err := waitModuleCFUN(lease.app, lease.restoreMode, 20*time.Second); err != nil {
			return fmt.Errorf("模块 VoWiFi 已停止，但无线状态恢复未确认: %w", err)
		}
		lease.mu.Lock()
		active := lease.active
		lease.mu.Unlock()
		if active {
			return errors.New("模块 VoWiFi 已停止，但 CFUN 无线租约尚未释放")
		}
	} else if session != nil && session.radioRestoreErr != nil {
		return fmt.Errorf("关闭模块 VoWiFi 后未能恢复原无线状态: %w", session.radioRestoreErr)
	}
	vowifiSessionMu.Lock()
	stillRetiring := retiringSession != nil && retiringSession.sourceKind == "module"
	vowifiSessionMu.Unlock()
	if stillRetiring {
		return errors.New("模块 VoWiFi 仍在释放隧道资源，请稍后重试")
	}
	return nil
}

func (a *app) runModuleVoWiFiOperation(enable bool, ctx context.Context, cancel context.CancelFunc) {
	if cancel != nil {
		defer cancel()
	}
	var err error
	if enable {
		err = startModuleVoWiFi(ctx, a)
		if a.moduleVoWiFiCancellationRequested() {
			// Cancellation can race with successful session publication. Run the
			// normal teardown path as well, so an accepted Stop never leaves a
			// just-published tunnel or CFUN lease alive.
			if stopErr := stopModuleVoWiFiSession(); stopErr != nil {
				err = stopErr
			} else {
				err = nil
			}
		}
	} else {
		err = stopModuleVoWiFiSession()
	}
	modeNow, modeErr := readModuleCFUN(a)
	var status *moduleVoWiFiStatus
	if modeErr == nil {
		vowifiSessionMu.Lock()
		session := activeSession
		vowifiSessionMu.Unlock()
		if session != nil && session.sourceKind != "module" {
			session = nil
		}
		status = moduleVoWiFiStatusFromSession(session, modeNow)
	}
	if err != nil {
		err = fmt.Errorf("模块 Wi-Fi Calling: %w", err)
	}
	if err != nil && enable {
		if status == nil {
			status = moduleVoWiFiStatusFromSession(nil, -1)
		}
		status.State = "failed"
		status.Summary = "模块 Wi-Fi Calling 启动失败"
		status.Detail = err.Error()
	}
	a.moduleVoWiFiMu.Lock()
	operation := a.moduleVoWiFiOp
	if operation == nil {
		operation = &moduleVoWiFiOperation{}
		a.moduleVoWiFiOp = operation
	}
	operation.Done = true
	operation.cancel = nil
	operation.Status = status
	operation.Error = ""
	if err != nil {
		operation.Error = err.Error()
	}
	a.moduleVoWiFiMu.Unlock()
}

func (a *app) moduleVoWiFiCancellationRequested() bool {
	if a == nil {
		return false
	}
	a.moduleVoWiFiMu.Lock()
	defer a.moduleVoWiFiMu.Unlock()
	return a.moduleVoWiFiOp != nil && a.moduleVoWiFiOp.CancelRequested
}

func (a *app) getModuleVoWiFiResult(w http.ResponseWriter, _ *http.Request) {
	a.moduleVoWiFiMu.Lock()
	operation := a.moduleVoWiFiOp
	if operation == nil {
		operation = &moduleVoWiFiOperation{Done: true}
	}
	copy := *operation
	a.moduleVoWiFiMu.Unlock()
	writeJSON(w, http.StatusOK, copy)
}
