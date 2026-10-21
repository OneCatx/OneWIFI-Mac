package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strings"
	"time"

	"proletarius.local/core/internal/esim"
)

const (
	esimCardTypeEUICC                      = "euicc"
	esimCardTypePhysicalSIM                = "physical_sim"
	esimOperationStateQueued               = "queued"
	esimOperationStateRun                  = "running"
	esimOperationStateAwaitingConfirmation = "awaiting_confirmation"
	esimOperationStateOK                   = "succeeded"
	esimOperationStateWarn                 = "warning"
	esimOperationStateFailed               = "failed"
	esimOperationStateCancelled            = "cancelled"
)

type esimCapabilities struct {
	CanRefresh            bool `json:"can_refresh"`
	CanDownload           bool `json:"can_download"`
	CanSwitch             bool `json:"can_switch"`
	CanRename             bool `json:"can_rename"`
	CanDelete             bool `json:"can_delete"`
	CanProbePhonebook     bool `json:"can_probe_phonebook"`
	VendorSerialAvailable bool `json:"vendor_serial_available"`
}

type esimOverviewResponse struct {
	CardType     string                 `json:"card_type"`
	Message      string                 `json:"message,omitempty"`
	ChipInfo     *esim.EUICCChipInfo    `json:"chip_info,omitempty"`
	Profiles     []esim.EUICCProfiles   `json:"profiles"`
	Capabilities esimCapabilities       `json:"capabilities"`
	UpdatedAt    *time.Time             `json:"updated_at,omitempty"`
	Operation    *esimOperationSnapshot `json:"operation,omitempty"`
}

type esimOperationResult struct {
	Warning     string           `json:"warning,omitempty"`
	WarningCode string           `json:"warning_code,omitempty"`
	SpaceDelta  *esim.SpaceDelta `json:"space_delta,omitempty"`
}

type esimOperationSnapshot struct {
	ID                        string               `json:"id"`
	Kind                      string               `json:"kind"`
	State                     string               `json:"state"`
	Step                      string               `json:"step,omitempty"`
	Message                   string               `json:"message,omitempty"`
	Progress                  int                  `json:"progress"`
	TargetICCID               string               `json:"target_iccid,omitempty"`
	ErrorCode                 string               `json:"error_code,omitempty"`
	Error                     string               `json:"error,omitempty"`
	Diagnostic                string               `json:"diagnostic,omitempty"`
	EstimatedProfileSizeBytes *int64               `json:"estimated_profile_size_bytes,omitempty"`
	Recoverable               bool                 `json:"recoverable,omitempty"`
	RefreshAfterSeconds       int                  `json:"refresh_after_seconds,omitempty"`
	Result                    *esimOperationResult `json:"result,omitempty"`
	StartedAt                 time.Time            `json:"started_at"`
	UpdatedAt                 time.Time            `json:"updated_at"`
	FinishedAt                *time.Time           `json:"finished_at,omitempty"`
}

func cloneESIMOperation(in *esimOperationSnapshot) *esimOperationSnapshot {
	if in == nil {
		return nil
	}
	out := *in
	if in.Result != nil {
		result := *in.Result
		if in.Result.SpaceDelta != nil {
			delta := *in.Result.SpaceDelta
			result.SpaceDelta = &delta
		}
		out.Result = &result
	}
	if in.FinishedAt != nil {
		finished := *in.FinishedAt
		out.FinishedAt = &finished
	}
	return &out
}

func (a *app) currentESIMOperation() *esimOperationSnapshot {
	if a == nil {
		return nil
	}
	a.esimOperationMu.RLock()
	defer a.esimOperationMu.RUnlock()
	return cloneESIMOperation(a.esimOperation)
}

func isActiveESIMOperation(operation *esimOperationSnapshot) bool {
	return operation != nil && (operation.State == esimOperationStateQueued || operation.State == esimOperationStateRun || operation.State == esimOperationStateAwaitingConfirmation)
}

func (a *app) installESIMDownloadConfirmation(id string) chan bool {
	channel := make(chan bool, 1)
	a.esimOperationMu.Lock()
	defer a.esimOperationMu.Unlock()
	if a.esimOperation == nil || a.esimOperation.ID != id || a.esimOperation.Kind != "download_profile" {
		return nil
	}
	a.esimDownloadConfirm = channel
	return channel
}

func (a *app) awaitESIMDownloadConfirmation(ctx context.Context, id string, estimatedSizeBytes int64) bool {
	a.esimOperationMu.Lock()
	if a.esimOperation == nil || a.esimOperation.ID != id || a.esimDownloadConfirm == nil {
		a.esimOperationMu.Unlock()
		return false
	}
	if estimatedSizeBytes > 0 {
		size := estimatedSizeBytes
		a.esimOperation.EstimatedProfileSizeBytes = &size
	} else {
		a.esimOperation.EstimatedProfileSizeBytes = nil
	}
	a.esimOperation.State = esimOperationStateAwaitingConfirmation
	a.esimOperation.Step = "confirm_download"
	a.esimOperation.Message = "已读取 Profile 信息，请确认后开始写入 eUICC"
	a.esimOperation.Progress = 58
	a.esimOperation.UpdatedAt = time.Now()
	channel := a.esimDownloadConfirm
	a.esimOperationMu.Unlock()

	select {
	case confirm := <-channel:
		return confirm
	case <-ctx.Done():
		return false
	}
}

func (a *app) clearESIMDownloadConfirmation(id string) {
	a.esimOperationMu.Lock()
	defer a.esimOperationMu.Unlock()
	if a.esimOperation != nil && a.esimOperation.ID == id {
		a.esimDownloadConfirm = nil
	}
}

func (a *app) respondESIMDownloadConfirmation(operationID string, confirm bool) (*esimOperationSnapshot, bool) {
	a.esimOperationMu.Lock()
	defer a.esimOperationMu.Unlock()
	operation := a.esimOperation
	if operation == nil || operation.ID != operationID || operation.Kind != "download_profile" ||
		operation.State != esimOperationStateAwaitingConfirmation || a.esimDownloadConfirm == nil {
		return cloneESIMOperation(operation), false
	}
	operation.State = esimOperationStateRun
	operation.Step = "confirm_download"
	if confirm {
		operation.Message = "已确认，正在获取 Profile 数据包…"
		operation.Progress = 62
	} else {
		operation.Message = "正在取消下载…"
		operation.Progress = 62
	}
	operation.UpdatedAt = time.Now()
	a.esimDownloadConfirm <- confirm
	return cloneESIMOperation(operation), true
}

func (a *app) cancelESIMOperation(id string, message string) {
	if a == nil {
		return
	}
	now := time.Now()
	a.esimOperationMu.Lock()
	defer a.esimOperationMu.Unlock()
	if a.esimOperation == nil || a.esimOperation.ID != id {
		return
	}
	a.esimOperation.State = esimOperationStateCancelled
	a.esimOperation.Step = "cancelled"
	a.esimOperation.Message = strings.TrimSpace(message)
	a.esimOperation.Progress = 100
	a.esimOperation.UpdatedAt = now
	a.esimOperation.FinishedAt = &now
}

func (a *app) confirmESIMDownload(w http.ResponseWriter, r *http.Request) {
	var body struct {
		OperationID string `json:"operation_id"`
		Confirm     bool   `json:"confirm"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	body.OperationID = strings.TrimSpace(body.OperationID)
	if body.OperationID == "" {
		writeCodedError(w, http.StatusBadRequest, "invalid_operation_id", "下载操作已失效，请重新开始", false)
		return
	}
	operation, accepted := a.respondESIMDownloadConfirmation(body.OperationID, body.Confirm)
	if !accepted {
		writeCodedError(w, http.StatusConflict, "esim_download_confirmation_expired", "下载确认已过期，请重新开始", true)
		return
	}
	writeJSON(w, http.StatusOK, operation)
}

func (a *app) beginESIMOperation(kind, message, targetICCID string) (*esimOperationSnapshot, bool) {
	if a == nil {
		return nil, false
	}
	now := time.Now()
	a.esimOperationMu.Lock()
	defer a.esimOperationMu.Unlock()
	if isActiveESIMOperation(a.esimOperation) {
		return cloneESIMOperation(a.esimOperation), false
	}
	a.esimOperation = &esimOperationSnapshot{
		ID:          fmt.Sprintf("%d", now.UnixNano()),
		Kind:        strings.TrimSpace(kind),
		State:       esimOperationStateQueued,
		Step:        "queued",
		Message:     strings.TrimSpace(message),
		Progress:    0,
		TargetICCID: strings.TrimSpace(targetICCID),
		StartedAt:   now,
		UpdatedAt:   now,
	}
	return cloneESIMOperation(a.esimOperation), true
}

func (a *app) updateESIMOperation(id, state, step, message string, progress int) {
	if a == nil {
		return
	}
	if progress < 0 {
		progress = 0
	}
	if progress > 100 {
		progress = 100
	}
	a.esimOperationMu.Lock()
	defer a.esimOperationMu.Unlock()
	if a.esimOperation == nil || a.esimOperation.ID != id {
		return
	}
	if strings.TrimSpace(state) != "" {
		a.esimOperation.State = state
	}
	if strings.TrimSpace(step) != "" {
		a.esimOperation.Step = step
	}
	if strings.TrimSpace(message) != "" {
		a.esimOperation.Message = message
	}
	a.esimOperation.Progress = progress
	a.esimOperation.UpdatedAt = time.Now()
}

func (a *app) finishESIMOperation(id, message string, result *esimOperationResult, refreshAfterSeconds int) {
	if a == nil {
		return
	}
	now := time.Now()
	a.esimOperationMu.Lock()
	defer a.esimOperationMu.Unlock()
	if a.esimOperation == nil || a.esimOperation.ID != id {
		return
	}
	state := esimOperationStateOK
	if result != nil && strings.TrimSpace(result.WarningCode) != "" {
		state = esimOperationStateWarn
	}
	a.esimOperation.State = state
	a.esimOperation.Step = "done"
	a.esimOperation.Message = strings.TrimSpace(message)
	a.esimOperation.Progress = 100
	a.esimOperation.Result = result
	a.esimOperation.RefreshAfterSeconds = refreshAfterSeconds
	a.esimOperation.Error = ""
	a.esimOperation.ErrorCode = ""
	a.esimOperation.Recoverable = false
	a.esimOperation.UpdatedAt = now
	a.esimOperation.FinishedAt = &now
}

func (a *app) failESIMOperation(id, code, message string, recoverable bool) {
	if a == nil {
		return
	}
	now := time.Now()
	a.esimOperationMu.Lock()
	defer a.esimOperationMu.Unlock()
	if a.esimOperation == nil || a.esimOperation.ID != id {
		return
	}
	a.esimOperation.State = esimOperationStateFailed
	a.esimOperation.Step = "failed"
	a.esimOperation.Message = ""
	a.esimOperation.ErrorCode = strings.TrimSpace(code)
	a.esimOperation.Error = strings.TrimSpace(message)
	a.esimOperation.Recoverable = recoverable
	a.esimOperation.UpdatedAt = now
	a.esimOperation.FinishedAt = &now
}

func (a *app) failESIMOperationDetailed(id, code, message string, recoverable bool, step, diagnostic string) {
	if a == nil {
		return
	}
	now := time.Now()
	a.esimOperationMu.Lock()
	defer a.esimOperationMu.Unlock()
	if a.esimOperation == nil || a.esimOperation.ID != id {
		return
	}
	a.esimOperation.State = esimOperationStateFailed
	a.esimOperation.Step = strings.TrimSpace(step)
	a.esimOperation.Message = ""
	a.esimOperation.ErrorCode = strings.TrimSpace(code)
	a.esimOperation.Error = strings.TrimSpace(message)
	a.esimOperation.Diagnostic = strings.TrimSpace(diagnostic)
	a.esimOperation.Recoverable = recoverable
	a.esimOperation.UpdatedAt = now
	a.esimOperation.FinishedAt = &now
}

func (a *app) rejectIfESIMOperationActive(w http.ResponseWriter) bool {
	operation := a.currentESIMOperation()
	if !isActiveESIMOperation(operation) {
		return false
	}
	writeCodedError(w, http.StatusConflict, "esim_operation_busy", "已有 eSIM 操作正在进行，请等待完成后重试", true)
	return true
}

func (a *app) esimCapabilities(overview *esim.EsimOverview, switchAllowed bool) esimCapabilities {
	available := overview != nil && overview.ChipInfo != nil && len(overview.ChipInfo.EIDs) > 0
	serialAvailable := available && strings.TrimSpace(overview.ChipInfo.SerialNumber) != ""
	return esimCapabilities{
		CanRefresh:            available,
		CanDownload:           available,
		CanSwitch:             available && (switchAllowed || a.modem != nil),
		CanRename:             available,
		CanDelete:             available,
		CanProbePhonebook:     available,
		VendorSerialAvailable: serialAvailable,
	}
}

func physicalSIMOverviewResponse(message string, operation *esimOperationSnapshot) esimOverviewResponse {
	return esimOverviewResponse{
		CardType:     esimCardTypePhysicalSIM,
		Message:      strings.TrimSpace(message),
		Profiles:     []esim.EUICCProfiles{},
		Capabilities: esimCapabilities{},
		Operation:    operation,
	}
}

type codedErrorPayload struct {
	Error       string `json:"error"`
	Code        string `json:"code,omitempty"`
	Recoverable bool   `json:"recoverable,omitempty"`
}

func writeCodedError(w http.ResponseWriter, status int, code, message string, recoverable bool) {
	writeJSON(w, status, codedErrorPayload{
		Error:       strings.TrimSpace(message),
		Code:        strings.TrimSpace(code),
		Recoverable: recoverable,
	})
}

func writeDeleteProfileError(w http.ResponseWriter, err error) {
	code := esim.ClassifyDeleteProfileError(err)
	switch code {
	case esim.DeleteProfileErrorInvalidICCID, esim.DeleteProfileErrorInvalidAIDHex:
		writeCodedError(w, http.StatusBadRequest, strings.ToLower(string(code)), err.Error(), false)
	case esim.DeleteProfileErrorProfileNotFound, esim.DeleteProfileErrorEUICCNotFound:
		writeCodedError(w, http.StatusNotFound, strings.ToLower(string(code)), err.Error(), false)
	case esim.DeleteProfileErrorBusy:
		writeCodedError(w, http.StatusConflict, "esim_operation_busy", "已有 eSIM 操作正在进行，请稍后重试", true)
	default:
		writeCodedError(w, http.StatusBadGateway, "delete_profile_failed", "删除 Profile 失败，请刷新卡片状态后重试", true)
	}
}

func classifyDownloadOperationError(err error) (code, message string, recoverable bool) {
	if err == nil {
		return "", "", false
	}
	if errors.Is(err, esim.ErrOperationInProgress) {
		return "esim_operation_busy", "已有 eSIM 操作正在进行，请稍后重试", true
	}
	var downloadErr *esim.DownloadProfileError
	if errors.As(err, &downloadErr) && downloadErr != nil {
		code = downloadErr.Code
		message = downloadErr.Error()
		switch code {
		case esim.DownloadErrorEUICCIccidAlreadyExists, esim.DownloadErrorEUICCPPRNotAllowed:
			return code, message, false
		case esim.DownloadErrorGeneric:
			return code, "下载 Profile 失败，请检查网络和激活码后重试", true
		default:
			return code, message, true
		}
	}
	return "download_failed", "下载 Profile 失败，请检查网络和激活码后重试", true
}

// downloadStageFailureMessage 返回下载失败阶段的人话描述（仅阶段本身，
// 不包含服务器原始错误或任何激活码数据），用于补充 UI 错误提示。
func downloadStageFailureMessage(step string) string {
	switch strings.TrimSpace(step) {
	case "preflight":
		return "预检阶段失败"
	case "init_auth":
		return "向 SM-DP+ 发起认证阶段失败"
	case "auth_euicc":
		return "eUICC 验证 SM-DP+ 服务器签名阶段失败"
	case "auth_client":
		return "SM-DP+ 客户端身份认证阶段失败"
	case "auth_server":
		return "获取 Profile 数据包阶段失败"
	case "install":
		return "Profile 写入 eUICC 阶段失败"
	case "notify":
		return "安装通知上报阶段失败"
	default:
		return ""
	}
}

// applyDownloadStageToMessage 为笼统的 download_failed / download_failed 类
// UI 提示附加失败阶段描述；有具体错误文案的分类（如空间不足、ICCID 已存在）
// 保持原样，不做重复修饰。
func applyDownloadStageToMessage(code, message, step string) string {
	if code != esim.DownloadErrorGeneric && code != "download_failed" {
		return message
	}
	stageMessage := downloadStageFailureMessage(step)
	if stageMessage == "" {
		return message
	}
	return fmt.Sprintf("%s（%s）", message, stageMessage)
}

// redactText 把文本中出现的敏感值替换为占位符，
// 保证激活码 Matching ID / 确认码不进入本地日志。
func redactText(text string, secrets ...string) string {
	for _, secret := range secrets {
		secret = strings.TrimSpace(secret)
		if len(secret) < 4 {
			continue
		}
		text = strings.ReplaceAll(text, secret, "[redacted]")
	}
	return text
}

// esimDownloadFailureDetail 生成仅写入本地日志的下载失败诊断行。
// 界面使用 esimDownloadDiagnostic 提供相同类别的脱敏原因；两者都不得包含激活凭证。
func esimDownloadFailureDetail(step string, err error, secrets ...string) string {
	if err == nil {
		return ""
	}
	step = strings.TrimSpace(step)
	var downloadErr *esim.DownloadProfileError
	if errors.As(err, &downloadErr) && downloadErr != nil {
		return redactText(fmt.Sprintf(
			"stage=%s code=%s bpp_command_id=0x%02X bpp_error_reason=0x%02X error=%v",
			step, downloadErr.Code,
			downloadErr.BPPCommandID, downloadErr.BPPErrorReason, err,
		), secrets...)
	}
	return redactText(fmt.Sprintf("stage=%s code=download_failed error=%v", step, err), secrets...)
}

// esimDownloadDiagnostic returns actionable, activation-secret-redacted details
// for the local UI. Keep the provider's protocol response when available so
// support can distinguish an EID/server policy rejection from transport errors.
func esimDownloadDiagnostic(step string, err error, secrets ...string) string {
	if err == nil {
		return ""
	}
	parts := make([]string, 0, 4)
	if stage := downloadStageFailureMessage(step); stage != "" {
		parts = append(parts, "阶段："+stage)
	} else if strings.TrimSpace(step) != "" {
		parts = append(parts, "阶段："+strings.TrimSpace(step))
	}
	var downloadErr *esim.DownloadProfileError
	if errors.As(err, &downloadErr) && downloadErr != nil {
		if code := strings.TrimSpace(downloadErr.Code); code != "" {
			parts = append(parts, "错误码："+code)
		}
		if downloadErr.BPPCommandID != 0 || downloadErr.BPPErrorReason != 0 {
			parts = append(parts, fmt.Sprintf("BPP Command ID=0x%02X，Error Reason=0x%02X", downloadErr.BPPCommandID, downloadErr.BPPErrorReason))
		}
		detail := strings.TrimSpace(downloadErr.Details)
		if detail == "" {
			detail = strings.TrimSpace(downloadErr.OriginalMessage)
		}
		if detail == "" && downloadErr.Err != nil {
			detail = strings.TrimSpace(downloadErr.Err.Error())
		}
		if detail != "" {
			parts = append(parts, "底层返回："+redactText(detail, secrets...))
		}
	} else {
		parts = append(parts, "底层返回："+redactText(err.Error(), secrets...))
	}
	diagnostic := strings.Join(parts, "；")
	const maxDiagnosticRunes = 1200
	runes := []rune(diagnostic)
	if len(runes) > maxDiagnosticRunes {
		return string(runes[:maxDiagnosticRunes]) + "…（已截断）"
	}
	return diagnostic
}

func normalizeSIMIdentifier(value string) string {
	value = strings.TrimSpace(strings.Trim(value, "\""))
	return strings.TrimRight(value, "Ff")
}

func moduleProfileICCIDMatches(current, target string) bool {
	current = normalizeSIMIdentifier(current)
	return current != "" && current == normalizeSIMIdentifier(target)
}

func esimOverviewHasActiveProfile(overview *esim.EsimOverview, targetICCID string) bool {
	if overview == nil || strings.TrimSpace(targetICCID) == "" {
		return false
	}
	for _, group := range overview.Profiles {
		for _, profile := range group.Profiles {
			if profile.State == 1 && moduleProfileICCIDMatches(profile.ICCID, targetICCID) {
				return true
			}
		}
	}
	return false
}

func (a *app) moduleProfileMatches(target string) bool {
	if err := a.ensureUSBAT(); err != nil {
		return false
	}
	response, err := a.runATCommand("AT+QCCID", 3*time.Second)
	if err != nil {
		return false
	}
	return moduleProfileICCIDMatches(parseUSBATPrefixed(response, "+QCCID:"), target)
}

func (a *app) refreshModuleRadioAfterProfileSwitch() error {
	originalMode, err := readModuleCFUN(a)
	if err != nil {
		return fmt.Errorf("读取 Profile 切换前的 CFUN 状态失败: %w", err)
	}
	if _, err := a.runATCommand("AT+CFUN=0", 5*time.Second); err != nil {
		return fmt.Errorf("轻量 SIM 网络刷新进入最低功能模式失败: %w", err)
	}
	if err := waitModuleCFUN(a, 0, 8*time.Second); err != nil {
		return fmt.Errorf("模块未确认进入 SIM 网络刷新模式: %w", err)
	}
	time.Sleep(time.Second)
	if _, err := a.runATCommand(fmt.Sprintf("AT+CFUN=%d", originalMode), 8*time.Second); err != nil {
		if current, checkErr := readModuleCFUN(a); checkErr != nil || current != originalMode {
			return fmt.Errorf("轻量 SIM 网络刷新恢复原 CFUN=%d 失败: %w", originalMode, err)
		}
	}
	if err := waitModuleCFUN(a, originalMode, 10*time.Second); err != nil {
		return fmt.Errorf("轻量 SIM 网络刷新未能恢复原 CFUN=%d: %w", originalMode, err)
	}
	return nil
}

func (a *app) monitorESIMSwitch(operationID, targetICCID, initialWarning string, initialDelay time.Duration) {
	if a == nil {
		return
	}
	if initialDelay < 0 {
		initialDelay = 0
	}
	timer := time.NewTimer(initialDelay)
	defer timer.Stop()
	<-timer.C
	a.updateESIMOperation(operationID, esimOperationStateRun, "module_refresh", "正在刷新模块中的 SIM 网络状态…", 76)

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	target := normalizeSIMIdentifier(targetICCID)
	// The eUICC/profile switch may already have propagated without touching
	// the modem. Avoid any radio disruption when the target ICCID is live.
	if a.moduleProfileMatches(target) {
		message := "Profile 已切换，模块已识别新 SIM"
		if strings.TrimSpace(initialWarning) != "" {
			a.finishESIMOperation(operationID, message, &esimOperationResult{
				Warning:     initialWarning,
				WarningCode: "switch_profile_response_uncertain",
			}, 0)
		} else {
			a.finishESIMOperation(operationID, message, nil, 0)
		}
		return
	}
	if err := a.refreshModuleRadioAfterProfileSwitch(); err != nil {
		// A failed CFUN transition can briefly detach the USB AT interface.
		// Keep polling through the normal USB rediscovery path; do not reboot the
		// modem as a fallback, since users expect profile changes to stay local.
		log.Printf("eSIM profile switched; light radio refresh failed: %v", err)
	}
	for {
		if current := a.currentESIMOperation(); current == nil || current.ID != operationID {
			return
		}
		if a.moduleProfileMatches(target) {
			message := "Profile 已切换，模块已识别新 SIM"
			if strings.TrimSpace(initialWarning) != "" {
				a.finishESIMOperation(operationID, message, &esimOperationResult{
					Warning:     initialWarning,
					WarningCode: "switch_profile_response_uncertain",
				}, 0)
			} else {
				a.finishESIMOperation(operationID, message, nil, 0)
			}
			return
		}

		select {
		case <-ctx.Done():
			warning := "模块尚未确认新 Profile；应用未重启模块。请检查读卡器/模块连接后刷新状态"
			warningCode := "switch_profile_refresh_unconfirmed"
			if strings.TrimSpace(initialWarning) != "" {
				warning += "（" + initialWarning + "）"
			}
			a.finishESIMOperation(operationID, "Profile 切换指令已提交，但模块重连尚未确认", &esimOperationResult{
				Warning:     warning,
				WarningCode: warningCode,
			}, 0)
			return
		case <-ticker.C:
		}
	}
}
