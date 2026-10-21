package main

import (
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strings"

	"proletarius.local/core/internal/vowifi/carrier"
)

// ---------------------------------------------------------------------------
// HTTP API — 读卡器管理
// ---------------------------------------------------------------------------

// readerListResponse 读卡器列表响应
type readerListResponse struct {
	Readers  []string `json:"readers"`
	Selected string   `json:"selected,omitempty"`
}

// readerConnectRequest 连接读卡器请求
type readerConnectRequest struct {
	Reader string `json:"reader"`
}

type readerReconcileRequest struct {
	Readers []string `json:"readers"`
}

// readerInfoResponse 读卡器+SIM 卡信息
type readerInfoResponse struct {
	Status             readerStatus `json:"status"`
	USIMInfo           *usimInfo    `json:"usim_info,omitempty"`
	EPDGFQDN           string       `json:"epdg_fqdn,omitempty"`
	CarrierConfigError string       `json:"carrier_config_error,omitempty"`
}

// readerAuthRequest AKA 鉴权请求
type readerAuthRequest struct {
	RAND string `json:"rand"` // hex string, 32 chars (16 bytes)
	AUTN string `json:"autn"` // hex string, 32 chars (16 bytes)
	Mode string `json:"mode"` // "aka" | "eap-aka" (default: "aka")
}

// readerAuthResponse AKA 鉴权响应
type readerAuthResponse struct {
	RES  string `json:"res"`            // hex string
	CK   string `json:"ck"`             // hex string
	IK   string `json:"ik"`             // hex string
	AUTS string `json:"auts,omitempty"` // hex string (同步失败时)
}

// GET /api/reader/list — 列出所有 PC/SC 读卡器
func (a *app) readerList(w http.ResponseWriter, _ *http.Request) {
	a.readerListWithInventory(w, a.readerMgr.ListReaders)
}

// The inventory source is passed in so a missing scan can be verified without
// asking a physical reader to disappear during the regression test.
func (a *app) readerListWithInventory(w http.ResponseWriter, listReaders func() ([]string, error)) {
	readers, err := listReaders()
	if err != nil {
		writeCodedError(w, http.StatusBadGateway, "reader_list_failed",
			"枚举读卡器失败: "+err.Error(), false)
		return
	}
	// A list request is a single inventory sample, not proof that a selected
	// card has been removed. Only the confirmed hotplug monitor may retire it.
	selected := ""
	if status := a.readerMgr.Status(); status.Connected {
		selected = status.ReaderName
	}
	writeJSON(w, http.StatusOK, readerListResponse{
		Readers:  readers,
		Selected: selected,
	})
}

// readerReconcile accepts the inventory observed by Rust's read-only PC/SC
// scanner. It is intentionally observation-only: /api/v2/device-snapshot is
// polled frequently, so one incomplete scan must not disconnect the Go card
// session or stop a live VoWiFi call. The confirmed hotplug monitor owns
// physical-removal cleanup. This private route is available only on the
// authenticated Unix socket.
func (a *app) readerReconcile(w http.ResponseWriter, r *http.Request) {
	var req readerReconcileRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	readers, err := validateReaderInventory(req.Readers)
	if err != nil {
		writeCodedError(w, http.StatusBadRequest, "invalid_reader_inventory", err.Error(), false)
		return
	}
	observation := a.readerMgr.observePresence(r.Context(), readers)
	// observePresence already performs the authoritative bridge status query
	// for an established selected BLE reader. Do not enqueue a second bridge
	// request here: the Swift bridge serializes GATT/APDU work, so a duplicate
	// status probe can wait behind a long card operation without changing this
	// response. Reuse the confirmed/indeterminate selected identity for the
	// inventory; keep a single discovery query only when no BLE reader is the
	// selected source of this observation.
	if isBLEReader(observation.selected) {
		if observation.bleReader != "" {
			readers = appendReaderIfMissing(readers, observation.bleReader)
		} else if observation.presence != readerPresenceAbsent {
			readers = appendReaderIfMissing(readers, observation.selected)
		}
	} else {
		if ble := listConnectedBLEReader(r.Context()); ble != "" {
			readers = appendReaderIfMissing(readers, ble)
		}
	}
	// During the BLE handshake/APDU window, the bridge intentionally cannot
	// answer status. Preserve that selected identity in the read model until a
	// later observation is conclusive; otherwise the dashboard would flicker
	// offline even though the Go owner correctly kept the session alive.
	if observation.presence == readerPresenceIndeterminate {
		readers = appendReaderIfMissing(readers, observation.selected)
	}
	selected := ""
	if observation.presence != readerPresenceAbsent {
		selected = observation.selected
	}
	writeJSON(w, http.StatusOK, readerListResponse{Readers: readers, Selected: selected})
}

func appendReaderIfMissing(readers []string, candidate string) []string {
	for _, reader := range readers {
		if reader == candidate {
			return readers
		}
	}
	return append(readers, candidate)
}

func validateReaderInventory(input []string) ([]string, error) {
	if len(input) > 64 {
		return nil, fmt.Errorf("读卡器数量超出限制")
	}
	readers := make([]string, 0, len(input))
	seen := make(map[string]struct{}, len(input))
	for _, value := range input {
		name := strings.TrimSpace(value)
		if name == "" || len(name) > 512 {
			return nil, fmt.Errorf("读卡器名称无效")
		}
		if _, exists := seen[name]; exists {
			return nil, fmt.Errorf("读卡器名称重复")
		}
		seen[name] = struct{}{}
		readers = append(readers, name)
	}
	return readers, nil
}

// POST /api/reader/connect — 连接到指定读卡器
func (a *app) readerConnect(w http.ResponseWriter, r *http.Request) {
	var req readerConnectRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeCodedError(w, http.StatusBadRequest, "invalid_json", "请求格式错误", false)
		return
	}
	readerName := strings.TrimSpace(req.Reader)
	if readerName == "" {
		writeCodedError(w, http.StatusBadRequest, "reader_name_empty", "读卡器名称不能为空", false)
		return
	}
	if !beginReaderVoWiFiOperation() {
		writeCodedError(w, http.StatusConflict, "reader_operation_active",
			"读卡器 VoWiFi 操作正在进行，请稍后重试", true)
		return
	}
	defer endReaderVoWiFiOperation()
	if a.rejectReaderSelectionWhileBusy(w) {
		return
	}
	if err := a.readerMgr.Connect(readerName); err != nil {
		if errors.Is(err, errReaderCardOperationActive) {
			writeCodedError(w, http.StatusConflict, "reader_operation_active", err.Error(), true)
			return
		}
		writeCodedError(w, http.StatusBadGateway, "reader_connect_failed",
			"连接读卡器失败: "+err.Error(), false)
		return
	}
	// 返回连接后的信息
	resp := a.buildReaderInfoResponse()
	writeJSON(w, http.StatusOK, resp)
}

// POST /api/reader/disconnect — 断开读卡器连接
func (a *app) readerDisconnect(w http.ResponseWriter, _ *http.Request) {
	if !beginReaderVoWiFiOperation() {
		writeCodedError(w, http.StatusConflict, "reader_operation_active",
			"读卡器 VoWiFi 操作正在进行，请稍后重试", true)
		return
	}
	defer endReaderVoWiFiOperation()
	if a.rejectReaderSelectionWhileBusy(w) {
		return
	}
	if err := a.readerMgr.Disconnect(); err != nil {
		if errors.Is(err, errReaderCardOperationActive) {
			writeCodedError(w, http.StatusConflict, "reader_operation_active", err.Error(), true)
			return
		}
		writeCodedError(w, http.StatusBadGateway, "reader_disconnect_failed",
			"断开读卡器失败: "+err.Error(), false)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// The server owns both operation and IMS state. A stale UI snapshot must not
// disconnect a card or cancel a live session while selecting a reader.
func (a *app) rejectReaderSelectionWhileBusy(w http.ResponseWriter) bool {
	if a.rejectIfESIMOperationActive(w) {
		return true
	}
	vowifiSessionMu.Lock()
	busy := activeSession != nil || startingVoWiFi != nil || retiringSession != nil
	vowifiSessionMu.Unlock()
	if !busy {
		return false
	}
	writeCodedError(w, http.StatusConflict, "reader_operation_active",
		"VoWiFi 会话正在使用设备，请先关闭后重试", true)
	return true
}

// GET /api/reader/info — 获取读卡器状态和 SIM 卡信息
func (a *app) readerInfo(w http.ResponseWriter, _ *http.Request) {
	resp := a.buildReaderInfoResponse()
	writeJSON(w, http.StatusOK, resp)
}

// POST /api/reader/refresh — 刷新 SIM 卡信息
func (a *app) readerRefresh(w http.ResponseWriter, _ *http.Request) {
	if !beginReaderVoWiFiOperation() {
		writeCodedError(w, http.StatusConflict, "reader_operation_active", "读卡器正由 VoWiFi 或卡片会话使用", true)
		return
	}
	defer endReaderVoWiFiOperation()
	if err := a.readerMgr.RefreshUSIMInfo(); err != nil {
		writeCodedError(w, http.StatusBadGateway, "reader_refresh_failed",
			"刷新 SIM 卡信息失败: "+err.Error(), true)
		return
	}
	resp := a.buildReaderInfoResponse()
	writeJSON(w, http.StatusOK, resp)
}

// POST /api/reader/auth — 在 SIM 卡上执行 AKA/EAP-AKA 鉴权
func (a *app) readerAuth(w http.ResponseWriter, r *http.Request) {
	if !beginReaderVoWiFiOperation() {
		writeCodedError(w, http.StatusConflict, "reader_operation_active", "读卡器正由 VoWiFi 或卡片会话使用", true)
		return
	}
	defer endReaderVoWiFiOperation()
	var req readerAuthRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeCodedError(w, http.StatusBadRequest, "invalid_json", "请求格式错误", false)
		return
	}
	randBytes, err := hex.DecodeString(req.RAND)
	if err != nil || len(randBytes) != 16 {
		writeCodedError(w, http.StatusBadRequest, "invalid_rand", "RAND 必须为 16 字节 hex 字符串", false)
		return
	}
	autnBytes, err := hex.DecodeString(req.AUTN)
	if err != nil || len(autnBytes) != 16 {
		writeCodedError(w, http.StatusBadRequest, "invalid_autn", "AUTN 必须为 16 字节 hex 字符串", false)
		return
	}

	mode := strings.ToLower(strings.TrimSpace(req.Mode))
	if mode == "" {
		mode = "aka"
	}

	var result *akaResult
	var authErr error
	switch mode {
	case "aka":
		result, authErr = a.readerMgr.AuthenticateAKA(randBytes, autnBytes)
	case "eap-aka":
		result, authErr = a.readerMgr.AuthenticateEAPAKA(randBytes, autnBytes)
	default:
		writeCodedError(w, http.StatusBadRequest, "invalid_mode", "mode 必须为 aka 或 eap-aka", false)
		return
	}

	if authErr != nil {
		writeCodedError(w, http.StatusBadGateway, "auth_failed",
			"鉴权失败: "+authErr.Error(), false)
		return
	}

	resp := readerAuthResponse{
		RES: hex.EncodeToString(result.RES),
		CK:  hex.EncodeToString(result.CK),
		IK:  hex.EncodeToString(result.IK),
	}
	if len(result.AUTS) > 0 {
		resp.AUTS = hex.EncodeToString(result.AUTS)
	}
	writeJSON(w, http.StatusOK, resp)
}

// buildReaderInfoResponse 构造读卡器信息响应
func (a *app) buildReaderInfoResponse() readerInfoResponse {
	// Keep the reader status and SIM identity in the same generation.  A
	// profile switch may replace cachedUSIM immediately after a status read.
	status, info := a.readerMgr.statusAndUSIMInfo()
	resp := readerInfoResponse{Status: status}
	if info != nil {
		if isMissingPhoneNumber(info.PhoneNumber) {
			info.PhoneNumber = a.profilePhoneForICCID(info.ICCID)
		}
		if info.OperatorName == "" {
			if esimManager, _ := a.readerMgr.ESIMManager(); esimManager != nil {
				if name, err := esimManager.ActiveProfileDisplayName(); err == nil && strings.TrimSpace(name) != "" {
					info.OperatorName = strings.TrimSpace(name)
					info.OperatorSource = "esim_profile"
				}
			}
		}
		if info.OperatorName == "" {
			info.OperatorName = strings.Trim(strings.TrimSpace(info.MCC)+"/"+strings.TrimSpace(info.MNC), "/")
			info.OperatorSource = "mcc_mnc"
		}
		resp.USIMInfo = info
		profile, err := resolveRuntimeCarrier(carrier.SIMIdentity{
			MCC: info.MCC, MNC: info.MNC, IMSI: info.IMSI,
			ICCID: info.ICCID, GID1: info.GID1, GID2: info.GID2, SPN: info.selectorSPN(),
		})
		if err != nil {
			resp.CarrierConfigError = err.Error()
		} else {
			resp.EPDGFQDN = profile.EPDG()
		}
	}
	return resp
}

func (a *app) profilePhoneForICCID(iccid string) string {
	target := normalizeSIMIdentifier(iccid)
	if target == "" {
		return ""
	}
	a.profileNotesMu.Lock()
	defer a.profileNotesMu.Unlock()
	if err := a.loadProfileNotesLocked(); err != nil {
		log.Printf("load local SIM profile phone: %v", err)
		return ""
	}
	for key, note := range a.profileNotes {
		if normalizeSIMIdentifier(key) == target {
			phone := strings.TrimSpace(note.Phone)
			if isMissingPhoneNumber(phone) {
				return ""
			}
			return phone
		}
	}
	return ""
}

func isMissingPhoneNumber(phone string) bool {
	phone = strings.TrimSpace(phone)
	switch strings.ToLower(phone) {
	case "", "-", "unknown", "unknown number", "n/a", "号码未知", "未知号码":
		return true
	}
	digits := strings.Map(func(r rune) rune {
		if r >= '0' && r <= '9' {
			return r
		}
		return -1
	}, phone)
	return digits == "" || strings.Trim(digits, "0") == ""
}

func (a *app) rememberProfilePhone(iccid, phone string) {
	iccid = strings.TrimSpace(iccid)
	phone = strings.TrimSpace(phone)
	if normalizeSIMIdentifier(iccid) == "" || isMissingPhoneNumber(phone) {
		return
	}
	a.profileNotesMu.Lock()
	defer a.profileNotesMu.Unlock()
	if err := a.loadProfileNotesLocked(); err != nil {
		log.Printf("load local SIM profile phone before save: %v", err)
		return
	}
	note := a.profileNotes[iccid]
	if note.Phone == phone {
		return
	}
	note.Phone = phone
	a.profileNotes[iccid] = note
	if err := a.persistProfileNotesLocked(); err != nil {
		log.Printf("persist reader profile phone: %v", err)
	}
}
