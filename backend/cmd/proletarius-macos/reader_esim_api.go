package main

import (
	"context"
	"errors"
	"log"
	"net/http"
	"strings"
	"time"

	"proletarius.local/core/internal/esim"
	"proletarius.local/core/internal/readerapp"
)

// ---------------------------------------------------------------------------
// HTTP API — 读卡器 eSIM Profile 管理（CCID 读卡器通道，独立于模块 eSIM）
//
// 与 /api/esim/* 的关键区别：
//   - 使用 readerManager.esimManager（CCID 通道），而非 a.esim（模块 AT 通道）
//   - Profile 切换后不重启模块（AT+CFUN=1,1），不监控模块 ICCID 变化
//   - 读卡器是独立 USB CCID 设备，与模块蜂窝通道互不干扰
// ---------------------------------------------------------------------------

// GET /api/reader/esim — 读卡器 eSIM 概览（Profile 列表 + 芯片信息）
func (a *app) readerESIMOverview(w http.ResponseWriter, r *http.Request) {
	esimManager, _ := a.readerMgr.ESIMManager()
	if esimManager == nil {
		writeCodedError(w, http.StatusServiceUnavailable, "reader_esim_unavailable",
			"读卡器 eSIM 管理服务不可用（未连接读卡器或当前卡片非 eUICC）", true)
		return
	}

	if strings.EqualFold(strings.TrimSpace(r.URL.Query().Get("refresh")), "true") || r.URL.Query().Get("refresh") == "1" {
		if a.rejectIfESIMOperationActive(w) {
			return
		}
		readerName := a.readerMgr.Status().ReaderName
		err := (&readerCardService{manager: a.readerMgr}).RefreshOverview(
			r.Context(), readerapp.ReaderID(readerName))
		if err != nil {
			if isPhysicalSIMESIMProbeError(err) {
				writeJSON(w, http.StatusOK, physicalSIMOverviewResponse(
					"读卡器中的卡片为实体 SIM，不支持 eSIM Profile 管理", a.currentESIMOperation()))
				return
			}
			writeCodedError(w, http.StatusBadGateway, "reader_esim_refresh_failed",
				"刷新读卡器 eSIM 卡片信息失败，请稍后重试", true)
			return
		}
	}

	readerName := a.readerMgr.Status().ReaderName
	overview, err := (&readerCardService{manager: a.readerMgr}).Overview(
		r.Context(), readerapp.ReaderID(readerName))
	if err != nil {
		if isPhysicalSIMESIMProbeError(err) {
			writeJSON(w, http.StatusOK, physicalSIMOverviewResponse(
				"读卡器中的卡片为实体 SIM，不支持 eSIM Profile 管理", a.currentESIMOperation()))
			return
		}
		if errors.Is(err, esim.ErrOperationInProgress) {
			writeCodedError(w, http.StatusConflict, "esim_operation_busy",
				"eSIM 操作正在进行，当前显示内容暂不可刷新", true)
			return
		}
		writeCodedError(w, http.StatusBadGateway, "reader_esim_overview_failed",
			"读取读卡器 eSIM 卡片信息失败，请重新连接读卡器后重试", true)
		return
	}

	profiles := overview.Profiles
	if profiles == nil {
		profiles = []esim.EUICCProfiles{}
	}
	updatedAt := overview.UpdatedAt
	writeJSON(w, http.StatusOK, esimOverviewResponse{
		CardType:     esimCardTypeEUICC,
		ChipInfo:     overview.ChipInfo,
		Profiles:     profiles,
		Capabilities: a.readerESIMCapabilities(overview),
		UpdatedAt:    &updatedAt,
		Operation:    a.currentESIMOperation(),
	})
}

// readerESIMCapabilities 构造读卡器 eSIM 的能力集。
// 读卡器 eSIM 始终允许切换（不依赖模块 modem）。
func (a *app) readerESIMCapabilities(overview *esim.EsimOverview) esimCapabilities {
	available := overview != nil && overview.ChipInfo != nil && len(overview.ChipInfo.EIDs) > 0
	serialAvailable := available && strings.TrimSpace(overview.ChipInfo.SerialNumber) != ""
	return esimCapabilities{
		CanRefresh:            available,
		CanDownload:           available,
		CanSwitch:             available, // 读卡器 eSIM 始终可切换
		CanRename:             available,
		CanDelete:             available,
		CanProbePhonebook:     false, // 读卡器场景不支持电话簿探测
		VendorSerialAvailable: serialAvailable,
	}
}

// GET /api/reader/esim/notifications — list eUICC notifications which have
// not completed both ES9+ delivery and ES10b confirmation yet.
func (a *app) readerESIMNotifications(w http.ResponseWriter, r *http.Request) {
	esimManager, _ := a.readerMgr.ESIMManager()
	if esimManager == nil {
		writeCodedError(w, http.StatusServiceUnavailable, "reader_esim_unavailable",
			"读卡器 eSIM 管理服务不可用", true)
		return
	}
	if !beginReaderVoWiFiOperation() {
		writeCodedError(w, http.StatusConflict, "reader_operation_active",
			"读卡器操作正在进行，请稍后重试", true)
		return
	}
	defer endReaderVoWiFiOperation()

	releaseCard := a.readerMgr.beginCardOperation()
	items, err := esimManager.ListNotifications(strings.TrimSpace(r.URL.Query().Get("aid")))
	releaseCard()
	if err != nil {
		writeReaderNotificationError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"notifications": items})
}

// POST /api/reader/esim/notifications/retry — retry one pending operator
// notification and remove it from the eUICC only after remote delivery.
func (a *app) readerESIMRetryNotification(w http.ResponseWriter, r *http.Request) {
	esimManager, _ := a.readerMgr.ESIMManager()
	if esimManager == nil {
		writeCodedError(w, http.StatusServiceUnavailable, "reader_esim_unavailable",
			"读卡器 eSIM 管理服务不可用", true)
		return
	}
	var body struct {
		SequenceNumber int64  `json:"sequence_number"`
		AID            string `json:"aid"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	if !beginReaderVoWiFiOperation() {
		writeCodedError(w, http.StatusConflict, "reader_operation_active",
			"读卡器操作正在进行，请稍后重试", true)
		return
	}
	defer endReaderVoWiFiOperation()

	releaseCard := a.readerMgr.beginCardOperation()
	err := esimManager.RetryNotification(body.SequenceNumber, strings.TrimSpace(body.AID))
	releaseCard()
	if err != nil {
		writeReaderNotificationError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"message":         "运营商通知发送并确认完成",
		"sequence_number": body.SequenceNumber,
	})
}

func writeReaderNotificationError(w http.ResponseWriter, err error) {
	switch esim.ClassifyNotificationError(err) {
	case esim.NotificationErrorInvalidSequence, esim.NotificationErrorInvalidAIDHex:
		writeCodedError(w, http.StatusBadRequest, "invalid_notification", err.Error(), false)
	case esim.NotificationErrorNotFound:
		writeCodedError(w, http.StatusNotFound, "notification_not_found", err.Error(), false)
	case esim.NotificationErrorBusy:
		writeCodedError(w, http.StatusConflict, "reader_operation_active",
			"读卡器操作正在进行，请稍后重试", true)
	default:
		writeCodedError(w, http.StatusBadGateway, "notification_sync_failed", err.Error(), true)
	}
}

// POST /api/reader/esim/switch — 读卡器 eSIM Profile 切换
//
// 与模块 switchESIM 的关键区别：
//   - 不执行 AT+CFUN=1,1（不重启模块）
//   - 不调用 monitorESIMSwitch（不监控模块 ICCID 变化）
//   - Profile 切换在 eUICC APDU 层面完成即可
func (a *app) readerESIMSwitch(w http.ResponseWriter, r *http.Request) {
	esimManager, _ := a.readerMgr.ESIMManager()
	if esimManager == nil {
		writeCodedError(w, http.StatusServiceUnavailable, "reader_esim_unavailable",
			"读卡器 eSIM 管理服务不可用", true)
		return
	}

	var body struct {
		ICCID string `json:"iccid"`
		AID   string `json:"aid"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	if strings.TrimSpace(body.ICCID) == "" {
		writeCodedError(w, http.StatusBadRequest, "invalid_iccid",
			"请选择要启用的 Profile", false)
		return
	}
	if !beginReaderVoWiFiOperation() {
		writeCodedError(w, http.StatusConflict, "reader_operation_active",
			"读卡器 VoWiFi 操作正在进行，请稍后重试", true)
		return
	}
	defer endReaderVoWiFiOperation()
	readerName := a.readerMgr.Status().ReaderName

	operation, started := a.beginESIMOperation("switch_profile", "正在向读卡器 eUICC 提交 Profile 切换…", body.ICCID)
	if !started {
		writeCodedError(w, http.StatusConflict, "esim_operation_busy",
			"已有 eSIM 操作正在进行，请等待完成后重试", true)
		return
	}
	a.updateESIMOperation(operation.ID, esimOperationStateRun, "switch",
		"正在向读卡器 eUICC 提交 Profile 切换…", 25)
	stopVoWiFi()

	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Minute)
	defer cancel()

	result, err := (&readerCardService{manager: a.readerMgr}).SwitchProfile(
		ctx, readerapp.ReaderID(readerName), body.ICCID, body.AID)
	if err != nil {
		// 记录详细错误，便于定位切换失败的真正原因（APDU 超时、协议不匹配等）。
		// ICCID is a persistent subscriber identifier. Keep enough context to
		// correlate the physical reader and failure, but never write the target
		// profile identity (or an error string that repeats it) to local logs.
		log.Printf("eSIM profile 切换失败: reader=%s err=%s",
			readerName, redactText(err.Error(), body.ICCID))
		code, message := readerProfileSwitchFailure(err)
		recoverable := true
		status := http.StatusBadGateway
		if errors.Is(err, esim.ErrOperationInProgress) {
			code, message, status = "esim_operation_busy",
				"已有 eSIM 操作正在进行，请稍后重试", http.StatusConflict
		}
		a.failESIMOperation(operation.ID, code, message, recoverable)
		writeCodedError(w, status, code, message, recoverable)
		return
	}

	// Standalone readers use refresh=false: the eUICC can persist the selected
	// profile while macOS PC/SC still serves the old USIM application because
	// uicc-go closes with SCARD_LEAVE_CARD. After closing the LPA channel, use
	// the already-validated reader reset below and confirm the real identity.
	a.updateESIMOperation(operation.ID, esimOperationStateRun, "reader_reconnect",
		"Profile 已受理，正在等待 eUICC 复位稳定…", 60)
	select {
	case <-ctx.Done():
		code := "switch_reader_settle_timeout"
		message := "Profile 已受理，但等待 eUICC 复位稳定超时"
		a.failESIMOperation(operation.ID, code, message, true)
		writeCodedError(w, http.StatusGatewayTimeout, code, message, true)
		return
	// A short grace period covers card bookkeeping. The existing validated
	// reset follows for ESTKme readers; other readers only rebuild the session.
	case <-time.After(500 * time.Millisecond):
	}
	a.updateESIMOperation(operation.ID, esimOperationStateRun, "reader_reconnect",
		"正在重建卡片会话并读取当前身份…", 70)
	// Rebuild the session in place: the reader stays logically connected, so
	// the UI never shows "disconnected → reconnecting" during the switch.
	// A hardware reset is validated only for ESTKme readers. Other CCID
	// devices keep the conservative no-reset policy; the fresh USIM identity
	// below, not the reader's brand, determines whether activation completed.
	softwareResetSupported := isESTKmeReader(readerName)
	var powerCycleFn func() error
	if softwareResetSupported {
		powerCycleFn = func() error { return a.readerMgr.PowerCycleCard(readerName) }
	} else {
		log.Printf("reader %s: skipping unverified PC/SC power cycle; activation will be checked from refreshed identity", readerName)
	}
	if rebuildErr := a.readerMgr.RebuildSession(powerCycleFn); rebuildErr != nil {
		code := "switch_reader_reconnect_failed"
		message := "Profile 已受理，但重建读卡器会话失败: " + rebuildErr.Error()
		a.failESIMOperation(operation.ID, code, message, true)
		writeCodedError(w, http.StatusBadGateway, code, message, true)
		return
	}

	// Reopen the ISD-R channel after rebuilding to check pending enable/disable
	// notifications. ListNotifications synchronizes them first; delivery or
	// confirmation failure must remain visible, not be reported as completed.
	notificationWarning := ""
	notificationWarningCode := ""
	a.updateESIMOperation(operation.ID, esimOperationStateRun, "operator_notification",
		"正在向运营商确认 Profile 切换…", 78)
	if reconnectedManager, _ := a.readerMgr.ESIMManager(); reconnectedManager == nil {
		notificationWarning = "切卡命令已受理，但运营商通知服务尚未恢复，请保持读卡器连接后刷新重试"
		notificationWarningCode = "switch_notification_manager_unavailable"
	} else {
		releaseNotificationCard := a.readerMgr.beginCardOperation()
		pending, notificationErr := reconnectedManager.ListNotifications(body.AID)
		releaseNotificationCard()
		if notificationErr != nil {
			notificationWarning = "切卡命令已受理，但向运营商确认切卡状态失败: " + notificationErr.Error()
			notificationWarningCode = "switch_notification_sync_failed"
		} else {
			for _, item := range pending {
				if item.Event == "enable" || item.Event == "disable" {
					notificationWarning = "切卡命令已受理，但运营商切卡通知仍待发送；请保持网络和读卡器连接后刷新重试"
					notificationWarningCode = "switch_notification_pending"
					break
				}
			}
		}
	}

	// Profile 切换后必须等待卡实际呈现目标 ICCID；eSIM 概览只反映
	// Profile 列表，不能证明 USIM 应用已经完成复位。
	refreshWarnings := make([]string, 0, 2)
	refreshWarningCodes := make([]string, 0, 2)
	if notificationWarning != "" {
		refreshWarnings = append(refreshWarnings, notificationWarning)
		refreshWarningCodes = append(refreshWarningCodes, notificationWarningCode)
	}
	var refreshedInfo *usimInfo
	var identityErr error
	if softwareResetSupported {
		a.updateESIMOperation(operation.ID, esimOperationStateRun, "identity_refresh",
			"Profile 已受理，正在等待读卡器识别新身份…", 80)
		refreshCtx, refreshCancel := context.WithTimeout(ctx, 60*time.Second)
		refreshedInfo, identityErr = a.readerMgr.WaitForUSIMICCID(refreshCtx, result.TargetICCID, time.Second)
		refreshCancel()
	} else {
		// RebuildSession clears the old USIM cache and performs one physical
		// read. Use that result directly: no speculative reset or extra slow
		// APDU polling, and no unconditional claim that reinsertion is needed.
		refreshedInfo = a.readerMgr.USIMInfo()
	}
	identityWarning, identityWarningCode := readerSwitchIdentityWarning(
		result.TargetICCID, refreshedInfo, identityErr, softwareResetSupported)
	if identityWarning != "" {
		refreshWarnings = append(refreshWarnings, identityWarning)
		refreshWarningCodes = append(refreshWarningCodes, identityWarningCode)
	}
	refreshWarning := strings.Join(refreshWarnings, "；")
	refreshWarningCode := strings.Join(refreshWarningCodes, ",")
	// 不重启 DJI 模块；仅重建独立的 PC/SC/CCID 会话。
	completionMessage := "Profile 切换完成"
	if identityWarning != "" {
		completionMessage = "Profile 切换已受理，激活结果待确认"
	}
	a.finishESIMOperation(operation.ID, completionMessage, &esimOperationResult{
		Warning:     refreshWarning,
		WarningCode: refreshWarningCode,
	}, 0)

	operation = a.currentESIMOperation()
	writeJSON(w, http.StatusOK, map[string]any{
		"switch_accepted":             result.SwitchAccepted,
		"phase":                       result.Phase,
		"target_iccid":                result.TargetICCID,
		"recovery_pending":            false, // 读卡器场景无后续恢复
		"module_reboot_requested":     false, // 不重启模块
		"module_reboot_response":      "",
		"module_reboot_warning":       "",
		"reconnect_wait_seconds":      0,
		"reader_refresh_warning":      refreshWarning,
		"reader_refresh_warning_code": refreshWarningCode,
		"reader_info":                 a.buildReaderInfoResponse(),
		"operation":                   operation,
	})
}

// The transport's ability to reset is separate from the eUICC's actual active
// identity. This also covers removable 9eSIM cards in non-ESTK PC/SC readers.
func readerSwitchIdentityWarning(targetICCID string, info *usimInfo, readErr error, resetSupported bool) (string, string) {
	target := normalizeSIMIdentifier(targetICCID)
	if readErr == nil && target != "" && info != nil && normalizeSIMIdentifier(info.ICCID) == target {
		return "", ""
	}
	message := "Profile 切换已受理，但尚未确认读卡器呈现目标卡片身份；请刷新卡片状态，确认前不要重复切卡"
	if !resetSupported {
		message += "。若仍显示旧卡，请拔插读卡器后再次确认；应用没有对未经验证的读卡器强制复位"
	}
	return message, "switch_reader_identity_unconfirmed"
}

// PATCH /api/reader/esim/profile — 读卡器 eSIM Profile 重命名
func (a *app) readerESIMRename(w http.ResponseWriter, r *http.Request) {
	esimManager, _ := a.readerMgr.ESIMManager()
	if esimManager == nil {
		writeCodedError(w, http.StatusServiceUnavailable, "reader_esim_unavailable",
			"读卡器 eSIM 管理服务不可用", true)
		return
	}
	var body struct {
		ICCID string `json:"iccid"`
		AID   string `json:"aid"`
		Name  string `json:"name"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	body.ICCID = strings.TrimSpace(body.ICCID)
	body.Name = strings.TrimSpace(body.Name)
	if body.ICCID == "" || body.Name == "" {
		writeCodedError(w, http.StatusBadRequest, "invalid_profile_name",
			"Profile 名称不能为空", false)
		return
	}
	if a.rejectIfESIMOperationActive(w) {
		return
	}
	if !beginReaderVoWiFiOperation() {
		writeCodedError(w, http.StatusConflict, "reader_operation_active",
			"读卡器操作正在进行，请稍后重试", true)
		return
	}
	defer endReaderVoWiFiOperation()
	stopVoWiFi()
	releaseCard := a.readerMgr.beginCardOperation()
	err := esimManager.RenameProfile(body.ICCID, body.Name, body.AID)
	releaseCard()
	if err != nil {
		if errors.Is(err, esim.ErrOperationInProgress) {
			writeCodedError(w, http.StatusConflict, "esim_operation_busy",
				"已有 eSIM 操作正在进行，请稍后重试", true)
			return
		}
		writeCodedError(w, http.StatusBadGateway, "rename_profile_failed",
			"修改 Profile 名称失败，请稍后重试", true)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"message": "Profile 名称修改成功"})
}

// DELETE /api/reader/esim/profile — 读卡器 eSIM Profile 删除
func (a *app) readerESIMDelete(w http.ResponseWriter, r *http.Request) {
	esimManager, _ := a.readerMgr.ESIMManager()
	if esimManager == nil {
		writeCodedError(w, http.StatusServiceUnavailable, "reader_esim_unavailable",
			"读卡器 eSIM 管理服务不可用", true)
		return
	}
	var body struct {
		ICCID string `json:"iccid"`
		AID   string `json:"aid"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	body.ICCID = strings.TrimSpace(body.ICCID)
	if body.ICCID == "" {
		writeCodedError(w, http.StatusBadRequest, "invalid_iccid",
			"请选择要删除的 Profile", false)
		return
	}
	if a.rejectIfESIMOperationActive(w) {
		return
	}
	if !beginReaderVoWiFiOperation() {
		writeCodedError(w, http.StatusConflict, "reader_operation_active",
			"读卡器操作正在进行，请稍后重试", true)
		return
	}
	defer endReaderVoWiFiOperation()
	stopVoWiFi()
	releaseCard := a.readerMgr.beginCardOperation()
	result, err := esimManager.DeleteProfile(body.ICCID, body.AID)
	releaseCard()
	if err != nil {
		writeDeleteProfileError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, result)
}

// POST /api/reader/esim/download — 读卡器 eSIM Profile 下载
func (a *app) readerESIMDownload(w http.ResponseWriter, r *http.Request) {
	esimManager, _ := a.readerMgr.ESIMManager()
	if esimManager == nil {
		writeCodedError(w, http.StatusServiceUnavailable, "reader_esim_unavailable",
			"读卡器 eSIM 管理服务不可用", true)
		return
	}
	var body struct {
		SMDP             string `json:"smdp"`
		MatchingID       string `json:"matching_id"`
		ConfirmationCode string `json:"confirmation_code"`
		AID              string `json:"aid"`
		IMEI             string `json:"imei"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	body.SMDP = strings.TrimSpace(strings.TrimPrefix(strings.TrimPrefix(body.SMDP, "https://"), "http://"))
	if body.SMDP == "" {
		writeCodedError(w, http.StatusBadRequest, "invalid_smdp",
			"SM-DP+ 地址不能为空", false)
		return
	}
	if !beginReaderVoWiFiOperation() {
		writeCodedError(w, http.StatusConflict, "reader_operation_active",
			"读卡器操作正在进行，请稍后重试", true)
		return
	}
	operation, started := a.beginESIMOperation("download_profile", "正在准备下载 Profile…", "")
	if !started {
		endReaderVoWiFiOperation()
		writeCodedError(w, http.StatusConflict, "esim_operation_busy",
			"已有 eSIM 操作正在进行，请等待完成后重试", true)
		return
	}
	a.updateESIMOperation(operation.ID, esimOperationStateRun, "preflight",
		"正在准备下载 Profile…", 5)
	if a.installESIMDownloadConfirmation(operation.ID) == nil {
		endReaderVoWiFiOperation()
		a.failESIMOperation(operation.ID, "esim_operation_state_invalid", "无法创建下载确认会话，请重试", true)
		writeCodedError(w, http.StatusConflict, "esim_operation_state_invalid", "无法创建下载确认会话，请重试", true)
		return
	}
	// eUICC package installation changes card application state and must not
	// overlap an established IMS session or an in-flight AKA transaction.
	stopVoWiFi()
	writeJSON(w, http.StatusAccepted, a.currentESIMOperation())

	go func(operationID string) {
		defer endReaderVoWiFiOperation()
		defer a.clearESIMDownloadConfirmation(operationID)
		downloadState := readerapp.NewCardDownloadService()
		operationLog := readerapp.NewLogger(log.Writer())
		_ = operationLog.Write("card_download", "start", string(readerapp.Preparing), "pending", "")
		releaseCard := a.readerMgr.beginCardOperation()
		defer releaseCard()
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		defer cancel()
		var lastStep string
		result, err := esimManager.DownloadProfile(ctx, body.AID, body.SMDP,
			body.MatchingID, body.ConfirmationCode, body.IMEI,
			func(event esim.DownloadProgressEvent) {
				lastStep = event.Step
				var state readerapp.DownloadState
				switch event.Step {
				case "preflight":
					state = readerapp.Preparing
				case "init_auth", "auth_euicc", "auth_client":
					state = readerapp.Authenticating
				case "auth_server":
					state = readerapp.Downloading
				case "install":
					state = readerapp.Installing
				}
				if state != "" && downloadState.Advance(state, "") == nil {
					_ = operationLog.Write("card_download", "progress", string(state), "pending", "")
				}
				a.updateESIMOperation(operationID, esimOperationStateRun,
					event.Step, event.Msg, event.Pct)
			}, func(estimatedSizeBytes int64) bool {
				return a.awaitESIMDownloadConfirmation(ctx, operationID, estimatedSizeBytes)
			})
		if err != nil {
			if esim.IsDownloadCancelled(err) {
				_ = operationLog.Write("card_download", "finish", "cancelled", "cancelled", "")
				a.cancelESIMOperation(operationID, "已取消下载，Profile 未写入 eUICC")
				return
			}
			code, message, recoverable := classifyDownloadOperationError(err)
			// 详细错误会按 APDU/SM-DP+ 阶段分开标识，激活码仍会脱敏。
			log.Printf("eSIM profile 下载失败: %s",
				esimDownloadFailureDetail(lastStep, err, body.MatchingID, body.ConfirmationCode, body.IMEI))
			message = applyDownloadStageToMessage(code, message, lastStep)
			_ = downloadState.Advance(readerapp.Failed, code)
			_ = operationLog.Write("card_download", "finish", string(readerapp.Failed), "error", code)
			diagnostic := esimDownloadDiagnostic(lastStep, err, body.MatchingID, body.ConfirmationCode, body.IMEI)
			a.failESIMOperationDetailed(operationID, code, message, recoverable, lastStep, diagnostic)
			return
		}
		_ = downloadState.Advance(readerapp.Installing, "")
		_ = downloadState.Advance(readerapp.Completed, "")
		_ = operationLog.Write("card_download", "finish", string(readerapp.Completed), "ok", "")
		a.finishESIMOperation(operationID, "Profile 下载并安装完成", &esimOperationResult{
			Warning:     result.Warning,
			WarningCode: result.WarningCode,
			SpaceDelta:  result.SpaceDelta,
		}, 2)
	}(operation.ID)
}

// GET /api/reader/esim/health — 读卡器 eSIM 健康检查
func (a *app) readerESIMHealth(w http.ResponseWriter, r *http.Request) {
	esimManager, _ := a.readerMgr.ESIMManager()
	if esimManager == nil {
		writeCodedError(w, http.StatusServiceUnavailable, "reader_esim_unavailable",
			"读卡器 eSIM 管理服务不可用", true)
		return
	}
	readerName := a.readerMgr.Status().ReaderName
	overview, err := (&readerCardService{manager: a.readerMgr}).Overview(
		r.Context(), readerapp.ReaderID(readerName))
	if err != nil {
		if isPhysicalSIMESIMProbeError(err) {
			writeJSON(w, http.StatusOK, map[string]any{
				"card_type": esimCardTypePhysicalSIM,
				"state":     "physical_sim",
				"operation": a.currentESIMOperation(),
			})
			return
		}
		writeCodedError(w, http.StatusBadGateway, "reader_esim_health_failed",
			"读取读卡器 eSIM 运行状态失败", true)
		return
	}

	var active *esim.ProfileItem
	profileReadFailed := false
	for _, group := range overview.Profiles {
		if group.ReadState == "error" {
			profileReadFailed = true
		}
		for index := range group.Profiles {
			if group.Profiles[index].State == 1 {
				active = &group.Profiles[index]
				break
			}
		}
		if active != nil {
			break
		}
	}
	if active == nil {
		state := "no_active_profile"
		message := "读卡器 eSIM 卡片已识别，但没有已启用的 Profile"
		if profileReadFailed {
			state = "profile_read_error"
			message = "读卡器 eSIM 卡片已识别，但 Profile 状态读取失败"
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"card_type": esimCardTypeEUICC,
			"state":     state,
			"message":   message,
			"operation": a.currentESIMOperation(),
		})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"card_type":      esimCardTypeEUICC,
		"state":          "active",
		"active_profile": active,
		"operation":      a.currentESIMOperation(),
	})
}
