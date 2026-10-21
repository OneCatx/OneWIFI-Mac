package main

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"proletarius.local/core/internal/vowifi"
	"proletarius.local/core/internal/vowifi/carrier"
	"proletarius.local/core/internal/vowifi/messaging"
)

// cryptoRand is an alias to avoid naming collision with encoding/binary.
// (crypto/rand is imported as "crypto/rand" and its Read function is used directly.)

// proxyCheckRequest 代理检测请求
type proxyCheckRequest struct {
	Proxy string `json:"proxy"`
	EPDG  string `json:"epdg,omitempty"`
}

// proxyCheckResult 代理检测结果
type proxyCheckResult struct {
	OK           bool   `json:"ok"`
	ProbeMessage string `json:"probe_message,omitempty"`
	CountryCode  string `json:"country_code,omitempty"`
	Country      string `json:"country,omitempty"`
	ExitIP       string `json:"exit_ip,omitempty"`
	Latency      int64  `json:"latency_ms,omitempty"`
	UDPReady     bool   `json:"udp_ready"`
	Transport    string `json:"transport,omitempty"`
	UDPMessage   string `json:"udp_message,omitempty"`
	Error        string `json:"error,omitempty"`
}

// ---------------------------------------------------------------------------
// HTTP API — 读卡器 VoWiFi
//
// 完整实现：IKEv2 + EAP-AKA + ePDG 隧道 + IMS SIP REGISTER + SMS over IMS
// 用户态网络栈（gVisor netstack），不使用 tun/xfrm。
// ---------------------------------------------------------------------------

// tunnelState 隧道状态
type tunnelState string

const (
	tunnelDisconnected tunnelState = "disconnected"
	tunnelConnecting   tunnelState = "connecting"
	tunnelConnected    tunnelState = "connected"
	tunnelError        tunnelState = "error"
)

// imsState IMS 注册状态
type imsState string

const (
	imsIdle        imsState = "idle"
	imsRegistering imsState = "registering"
	imsRegistered  imsState = "registered"
	imsFailed      imsState = "failed"
)

// progressStep VoWiFi 启动进度阶段
// 1=SIM  2=Access(EAP-AKA)  3=Tunnel(IKE/ESP)  4=IMS  5=SMS
type progressStep int

const (
	progressIdle   progressStep = 0
	progressSIM    progressStep = 1
	progressAccess progressStep = 2
	progressTunnel progressStep = 3
	progressIMS    progressStep = 4
	progressSMS    progressStep = 5
)

// progressStepName 返回阶段名称
func progressStepName(step progressStep) string {
	switch step {
	case progressSIM:
		return "SIM"
	case progressAccess:
		return "Access"
	case progressTunnel:
		return "Tunnel"
	case progressIMS:
		return "IMS"
	case progressSMS:
		return "SMS"
	default:
		return ""
	}
}

// readerVoWiFiStatus 读卡器模式 VoWiFi 状态
type readerVoWiFiStatus struct {
	ReaderID           string      `json:"reader_id,omitempty"`
	SIMID              string      `json:"sim_id,omitempty"`
	ReaderGeneration   uint64      `json:"reader_generation,omitempty"`
	Available          bool        `json:"available"`
	MCC                string      `json:"mcc,omitempty"`
	MNC                string      `json:"mnc,omitempty"`
	IMSI               string      `json:"imsi,omitempty"`
	EPDGFQDN           string      `json:"epdg_fqdn,omitempty"`
	EPDGSource         string      `json:"epdg_source,omitempty"`
	ProfileID          string      `json:"profile_id,omitempty"`
	ProfileName        string      `json:"profile_name,omitempty"`
	ProfileSource      string      `json:"profile_source,omitempty"`
	OverrideMatched    bool        `json:"override_matched,omitempty"`
	OverrideID         string      `json:"override_id,omitempty"`
	CarrierConfigPath  string      `json:"carrier_config_path,omitempty"`
	CarrierConfigError string      `json:"carrier_config_error,omitempty"`
	IMEI               string      `json:"imei,omitempty"`
	TunnelState        tunnelState `json:"tunnel_state"`
	IMSState           imsState    `json:"ims_state"`
	Message            string      `json:"message,omitempty"`
	LocalIPv4          string      `json:"local_ipv4,omitempty"`
	LocalIPv6          string      `json:"local_ipv6,omitempty"`
	PCSCF              []string    `json:"pcscf,omitempty"`
	IKEEncryption      string      `json:"ike_encryption,omitempty"`
	ESPEncryption      string      `json:"esp_encryption,omitempty"`
	ResponderAUTH      string      `json:"responder_auth,omitempty"`
	EffectiveAPN       string      `json:"effective_apn,omitempty"`
	IPStack            string      `json:"ip_stack,omitempty"`
	AlgorithmPolicy    string      `json:"algorithm_policy,omitempty"`
	AKAChallengeMode   string      `json:"aka_challenge_mode,omitempty"`
	DeviceModel        string      `json:"device_model,omitempty"`
	IMSIdentitySource  string      `json:"ims_identity_source,omitempty"`
	E911Enabled        bool        `json:"e911_enabled,omitempty"`
	E911Status         string      `json:"e911_status,omitempty"`
	// IMS fields
	RegisteredMSISDN string `json:"registered_msisdn,omitempty"`
	IMSTransport     string `json:"ims_transport,omitempty"`
	IMSSecurityMode  string `json:"ims_security_mode,omitempty"`
	SMSReady         bool   `json:"sms_ready,omitempty"`
	SMSState         string `json:"sms_state,omitempty"`
	CallReady        bool   `json:"call_ready,omitempty"`
	CallState        string `json:"call_state,omitempty"`
	// Port-S diagnostics are transport observations only. They deliberately do
	// not include caller, URI, endpoint address, or raw SIP content.
	IncomingListenerStarted        bool   `json:"incoming_listener_started"`
	IncomingAcceptCount            uint64 `json:"incoming_accept_count"`
	IncomingLastSIPAt              string `json:"incoming_last_sip_at,omitempty"`
	IncomingLastResponseWriteError string `json:"incoming_last_response_write_error,omitempty"`
	IncomingReachability           string `json:"incoming_reachability"`
	// 进度信息
	ProgressStep progressStep `json:"progress_step"`           // 0=未开始 1~5 对应五个阶段
	ProgressDesc string       `json:"progress_desc,omitempty"` // 当前阶段的描述文本
}

// readerVoWiFiRequest 读卡器模式 Wi-Fi Calling 请求
type readerVoWiFiRequest struct {
	Enable   bool   `json:"enable"`
	Proxy    string `json:"proxy,omitempty"`     // 可选：填写时优先使用，留空时尝试网络直连
	EPDG     string `json:"epdg,omitempty"`      // 自定义 ePDG（可选，优先于模板）
	EPDGPort int    `json:"epdg_port,omitempty"` // 自定义起始端口，0=UDP 500
}

func (a *app) sendReaderVoWiFiSMS(phone, text string) (int, error) {
	return a.sendVoWiFiSessionSMS("reader", phone, text)
}

func (a *app) sendModuleVoWiFiSMS(phone, text string) (int, error) {
	return a.sendVoWiFiSessionSMS("module", phone, text)
}

func (a *app) sendVoWiFiSessionSMS(source, phone, text string) (int, error) {
	vowifiSessionMu.Lock()
	session := activeSession
	vowifiSessionMu.Unlock()
	if session == nil {
		return 0, fmt.Errorf("%s VoWiFi 未连接", voWiFiSourceLabel(source))
	}
	if session.sourceKind != source {
		return 0, fmt.Errorf("当前活动 VoWiFi 会话属于 %s", voWiFiSourceLabel(session.sourceKind))
	}
	return a.sendBoundVoWiFiSMS(session, source, phone, text)
}

func (a *app) sendBoundVoWiFiSMS(session *vowifiSession, source, phone, text string) (int, error) {
	session.imsOpMu.Lock()
	defer session.imsOpMu.Unlock()
	if source == "module" {
		if session.moduleSIM == nil {
			return 0, fmt.Errorf("DJI 模块 SIM 会话不可用")
		}
		if err := session.moduleSIM.validateProfile(session.ctx); err != nil {
			var secrets []string
			if session.simInfo != nil {
				secrets = append(secrets, session.simInfo.ICCID, session.simInfo.IMSI)
			}
			log.Printf("[Module VoWiFi SMS] rejected at profile validation: %s", redactSMSFailureDetails(err.Error(), append(secrets, phone, text)...))
			return 0, fmt.Errorf("DJI 模块 eSIM Profile 已变化，短信发送已拒绝: %w", err)
		}
		mode, err := readModuleCFUN(a)
		if err != nil || mode != 4 {
			log.Printf("[Module VoWiFi SMS] rejected: module radio is not confirmed in CFUN=4")
			return 0, fmt.Errorf("DJI 模块未处于 VoWiFi 飞行模式，短信发送已拒绝")
		}
	}
	session.mu.Lock()
	service, info := session.imsService, session.simInfo
	ready := service != nil && service.MessagingReady()
	session.mu.Unlock()
	if !ready {
		if source == "module" {
			log.Printf("[Module VoWiFi SMS] rejected: IMS messaging transport is not ready")
		}
		return 0, fmt.Errorf("IMS 短信安全通道未就绪")
	}
	if info == nil {
		return 0, fmt.Errorf("SIM 短信服务中心资料不可用")
	}
	if strings.TrimSpace(info.SMSC) == "" {
		if source == "module" {
			log.Printf("[Module VoWiFi SMS] rejected: no SMSC address from UICC or AT+CSCA")
		}
		return 0, fmt.Errorf("SIM 未提供 SMSC 地址；已尝试读取当前 Profile 的 AT+CSCA，拒绝猜测运营商号码")
	}
	if strings.TrimSpace(info.SMSCTarget) == "" {
		if source == "module" {
			log.Printf("[Module VoWiFi SMS] rejected: no IMS SMSC PSI or valid tel:SMSC target")
		}
		return 0, fmt.Errorf("SIM 未提供可用的 IMS 短信目标（PSI/SMSC URI）")
	}
	baseCtx := session.ctx
	if baseCtx == nil {
		baseCtx = context.Background()
	}
	seed := []byte{0}
	if _, err := rand.Read(seed); err != nil {
		return 0, fmt.Errorf("生成短信引用号: %w", err)
	}
	tpdus, err := prepareRuntimeTPDUs(baseCtx, phone, text, seed[0])
	if err != nil {
		if source == "module" {
			log.Printf("[Module VoWiFi SMS] encode failed: %s", redactSMSFailureDetails(err.Error(), phone, text, info.SMSC, info.SMSCTarget, info.ICCID, info.IMSI))
		}
		return 0, err
	}
	parts := make([]messaging.SMSPart, 0, len(tpdus))
	for i, prepared := range tpdus {
		ref := seed[0] + byte(i)
		tpdu := append([]byte(nil), prepared...)
		if len(tpdu) < 2 {
			return 0, fmt.Errorf("短信 TPDU 过短")
		}
		tpdu[1] = ref
		body, err := encodeRuntimeRPDU(baseCtx, ref, info.SMSC, tpdu)
		if err != nil {
			if source == "module" {
				log.Printf("[Module VoWiFi SMS] RPDU construction failed: %s", redactSMSFailureDetails(err.Error(), phone, text, info.SMSC, info.SMSCTarget, info.ICCID, info.IMSI))
			}
			return 0, err
		}
		parts = append(parts, messaging.SMSPart{RPMR: ref, Body: body})
	}
	if session.multiAuth != nil && !multiSessionReaderCurrent(session) {
		return 0, fmt.Errorf("读卡器已断开或更换，已拒绝短信发送")
	}
	ctx, cancel := context.WithTimeout(baseCtx, time.Duration(len(parts))*20*time.Second)
	defer cancel()
	if _, err := service.SendSMS(ctx, info.SMSCTarget, text, parts); err != nil {
		if source == "module" {
			log.Printf("[Module VoWiFi SMS] SIP MESSAGE failed: parts=%d detail=%s", len(parts), redactSMSFailureDetails(err.Error(), phone, text, info.SMSC, info.SMSCTarget, info.ICCID, info.IMSI))
		}
		return 0, err
	}
	if source == "module" {
		log.Printf("[Module VoWiFi SMS] SIP MESSAGE accepted: parts=%d", len(parts))
	}
	return len(parts), nil
}

func redactSMSFailureDetails(value string, secrets ...string) string {
	for _, secret := range secrets {
		secret = strings.TrimSpace(secret)
		if secret != "" {
			value = strings.ReplaceAll(value, secret, "[redacted]")
		}
	}
	return value
}

func voWiFiSourceLabel(source string) string {
	if source == "module" {
		return "DJI 模块"
	}
	return "读卡器"
}

// --- 隧道 + IMS 管理由 vowifiSession 负责（见 reader_vowifi_session.go） ---

var (
	// readerIMEI 是用户通过 API 配置的 IMEI，持久化到配置文件
	readerIMEI   string
	readerIMEIMu sync.RWMutex
)

// readerVoWiFi 读写卡器模式下 VoWiFi 状态
func (a *app) readerVoWiFi(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet {
		a.getReaderVoWiFi(w, r)
		return
	}
	if r.Method == http.MethodPost {
		a.setReaderVoWiFi(w, r)
		return
	}
	w.Header().Set("Allow", "GET, POST")
	http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
}

// getReaderVoWiFi 返回读卡器模式 VoWiFi 状态
func (a *app) getReaderVoWiFi(w http.ResponseWriter, r *http.Request) {
	status := getVoWiFiStatus()
	readerStatus, info := a.readerMgr.statusAndUSIMInfo()
	if !readerVoWiFiStatusMatchesCurrentIdentity(status, readerStatus, info) {
		// Do not publish a healthy-looking IMS/SMS state from a session that
		// belongs to a previous reader or eSIM profile.  GET is intentionally
		// read-only: teardown remains owned by the switch/disconnect lifecycle.
		writeJSON(w, http.StatusOK, staleReaderVoWiFiStatus(readerStatus, info))
		return
	}
	if info == nil {
		status.Message = "读卡器未连接或无 SIM 卡"
		writeJSON(w, http.StatusOK, status)
		return
	}

	status.Available = true
	status.MCC = info.MCC
	status.MNC = info.MNC
	status.IMSI = info.IMSI
	profile, profileErr := resolveRuntimeCarrierContext(r.Context(), carrier.SIMIdentity{
		MCC: info.MCC, MNC: info.MNC, IMSI: info.IMSI,
		ICCID: info.ICCID, GID1: info.GID1, GID2: info.GID2, SPN: info.selectorSPN(),
	})
	generatedIMEI := a.getDeviceIMEI()
	override, overrideMatched, configErr := a.voWiFiOverrides.match(info.MCC, info.MNC)
	status.CarrierConfigPath, status.CarrierConfigError = a.voWiFiOverrides.status()
	if configErr != nil {
		status.CarrierConfigError = configErr.Error()
	}
	if profileErr != nil {
		status.CarrierConfigError = profileErr.Error()
	}
	if status.ProfileID == "" && configErr == nil && profileErr == nil {
		preview, err := resolveReaderVoWiFiEffectiveConfig(profile, override, overrideMatched, &vowifi.SIMInfo{
			IMSI: info.IMSI, MCC: info.MCC, MNC: info.MNC, ICCID: info.ICCID,
			PrivateIdentity: info.PrivateIdentity, PublicIdentity: info.PublicIdentity, HomeDomain: info.HomeDomain,
		}, "", 0, generatedIMEI)
		if err != nil {
			status.CarrierConfigError = err.Error()
		} else {
			status.EPDGFQDN = preview.EPDG
			status.EPDGSource = preview.EPDGSource
			status.ProfileID = preview.Profile.ID
			status.ProfileName = preview.Profile.Name
			status.ProfileSource = preview.Profile.Source
			status.OverrideMatched = preview.OverrideMatched
			status.OverrideID = preview.OverrideID
			status.IMEI = preview.DeviceIdentityIMEI
			status.EffectiveAPN = preview.APN
			status.IPStack = preview.IPStack
			status.AlgorithmPolicy = preview.AlgorithmPolicy
			status.AKAChallengeMode = preview.AKAChallengeMode
			status.DeviceModel = preview.DeviceModel
			status.IMSTransport = preview.IMSTransport
			status.IMSIdentitySource = preview.IMSIdentitySource
			status.E911Enabled = preview.E911Enabled
			if preview.E911Enabled {
				status.E911Status = "unsupported"
			}
		}
	}
	if status.ProfileID == "" && profile != nil {
		status.EPDGFQDN, status.EPDGSource = profile.ResolveEPDG("")
		status.ProfileID = profile.ID
		status.ProfileName = profile.Name
		status.ProfileSource = profile.Source
	}
	if status.IMEI == "" {
		status.IMEI = generatedIMEI
	}

	if status.TunnelState == tunnelConnected && status.IMSState == imsRegistered {
		if status.SMSReady {
			status.Message = "VoWiFi 已连接，SMS 就绪"
		} else {
			status.Message = "IMS 已注册，SMS 通道尚未就绪"
		}
	} else if status.TunnelState == tunnelConnecting {
		// 根据进度阶段返回详细消息
		switch status.ProgressStep {
		case progressSIM:
			status.Message = "SIM 卡已就绪，正在初始化..."
		case progressAccess:
			status.Message = "正在通过 EAP-AKA 认证..."
		case progressTunnel:
			status.Message = "隧道已建立，正在配置 ESP..."
		case progressIMS:
			status.Message = "正在注册 IMS..."
		default:
			status.Message = "正在建立 VoWiFi 隧道..."
		}
	} else if status.TunnelState == tunnelError {
		if status.Message != "" {
			status.Message = "VoWiFi 连接失败: " + status.Message
		} else {
			status.Message = "VoWiFi 连接失败"
		}
	} else if status.TunnelState == tunnelDisconnected {
		if status.CarrierConfigError != "" {
			status.Message = "VoWiFi 运营商配置不可用: " + status.CarrierConfigError
		} else {
			status.Message = "VoWiFi 未启动"
		}
	}

	writeJSON(w, http.StatusOK, status)
}

// readerVoWiFiStatusHasSessionIdentity differentiates the ordinary idle
// status (which deliberately has no session key) from a live or retiring
// session.  Any keyed session has to match the current reader generation
// before it can be exposed through the public status endpoint.
func readerVoWiFiStatusHasSessionIdentity(status *readerVoWiFiStatus) bool {
	if status == nil {
		return false
	}
	return strings.TrimSpace(status.ReaderID) != "" ||
		strings.TrimSpace(status.SIMID) != "" ||
		status.ReaderGeneration != 0 ||
		status.TunnelState != tunnelDisconnected ||
		status.IMSState != imsIdle ||
		status.SMSReady || status.CallReady
}

func currentReaderSIMID(info *usimInfo) string {
	if info == nil {
		return ""
	}
	if value := strings.TrimSpace(info.ICCID); value != "" {
		return value
	}
	return strings.TrimSpace(info.IMSI)
}

func readerVoWiFiStatusMatchesCurrentIdentity(status *readerVoWiFiStatus, reader readerStatus, info *usimInfo) bool {
	if !readerVoWiFiStatusHasSessionIdentity(status) {
		return true
	}
	expectedSIMID := currentReaderSIMID(info)
	return reader.Connected &&
		strings.TrimSpace(reader.ReaderName) != "" &&
		reader.Generation != 0 &&
		expectedSIMID != "" &&
		strings.TrimSpace(status.ReaderID) == strings.TrimSpace(reader.ReaderName) &&
		status.ReaderGeneration == reader.Generation &&
		strings.TrimSpace(status.SIMID) == expectedSIMID
}

func staleReaderVoWiFiStatus(reader readerStatus, info *usimInfo) *readerVoWiFiStatus {
	return &readerVoWiFiStatus{
		ReaderID:             reader.ReaderName,
		SIMID:                currentReaderSIMID(info),
		ReaderGeneration:     reader.Generation,
		Available:            false,
		TunnelState:          tunnelDisconnected,
		IMSState:             imsIdle,
		Message:              "读卡器或 SIM 身份已变更，已忽略过期 VoWiFi 会话状态",
		SMSState:             "unavailable",
		CallState:            "unavailable",
		IncomingReachability: "unverified",
	}
}

// readerVoWiFiOperation 读写卡器 VoWiFi 操作进行中标志
var readerVoWiFiMu sync.Mutex
var readerVoWiFiOperationActive bool
var readerNativeSessionLeaseActive bool
var readerMultiStandbyActive bool

func beginReaderVoWiFiOperation() bool {
	readerVoWiFiMu.Lock()
	defer readerVoWiFiMu.Unlock()
	if readerVoWiFiOperationActive || readerNativeSessionLeaseActive || readerMultiStandbyActive {
		return false
	}
	readerVoWiFiOperationActive = true
	return true
}

// beginNativeReaderSessionLeaseWindow prevents a new Go reader VoWiFi start,
// card mutation, connect or disconnect while Rust owns a generation-bound
// reader session. Existing live Go sessions reject the lease rather than
// being disturbed by a competing native owner.
func beginNativeReaderSessionLeaseWindow() bool {
	readerVoWiFiMu.Lock()
	defer readerVoWiFiMu.Unlock()
	if readerVoWiFiOperationActive || readerNativeSessionLeaseActive || readerMultiStandbyActive {
		return false
	}
	for _, status := range getAllVoWiFiStatuses() {
		if status != nil && status.TunnelState != tunnelDisconnected {
			return false
		}
	}
	readerNativeSessionLeaseActive = true
	return true
}

func endNativeReaderSessionLeaseWindow() {
	readerVoWiFiMu.Lock()
	readerNativeSessionLeaseActive = false
	readerVoWiFiMu.Unlock()
}

func nativeReaderLeaseWindowActive() bool {
	readerVoWiFiMu.Lock()
	defer readerVoWiFiMu.Unlock()
	return readerNativeSessionLeaseActive
}

func endReaderVoWiFiOperation() {
	readerVoWiFiMu.Lock()
	readerVoWiFiOperationActive = false
	readerVoWiFiMu.Unlock()
}

// setReaderVoWiFi 读写卡器模式 Wi-Fi Calling
func (a *app) setReaderVoWiFi(w http.ResponseWriter, r *http.Request) {
	var req readerVoWiFiRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "请求格式错误")
		return
	}
	if req.Enable {
		proxyAddr := strings.TrimPrefix(strings.TrimPrefix(strings.TrimSpace(req.Proxy), "socks5://"), "socks5h://")
		if err := validateReaderVoWiFiProxy(proxyAddr); err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
	}
	if !req.Enable {
		// Cancellation bypasses the reader-operation admission gate. If a start
		// is still selecting its transport, stopVoWiFi waits on the session lock
		// and cancels the newly published session immediately afterwards. This
		// makes Stop idempotent and prevents a transient 409 from stranding IKE.
		stopVoWiFi()
		writeJSON(w, http.StatusOK, map[string]any{
			"accepted":     true,
			"tunnel_state": "disconnected",
			"message":      "Wi-Fi Calling 已关闭",
		})
		return
	}
	if !beginReaderVoWiFiOperation() {
		writeError(w, http.StatusConflict, "读卡器 VoWiFi 操作正在进行，请稍后重试")
		return
	}
	defer endReaderVoWiFiOperation()

	info := a.readerMgr.USIMInfo()
	if info == nil {
		writeError(w, http.StatusServiceUnavailable, "读卡器未连接或无 SIM 卡")
		return
	}

	// 异步启动 VoWiFi
	if err := startVoWiFi(a, req.Proxy, req.EPDG, req.EPDGPort); err != nil {
		writeError(w, http.StatusInternalServerError, fmt.Sprintf("启动 VoWiFi 失败: %v", err))
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"accepted":     true,
		"tunnel_state": "connecting",
		"message":      "VoWiFi 正在启动...",
	})
}

// --- IMEI 配置 ---

// imeiConfigPath 返回 IMEI 配置文件路径
func imeiConfigPath() string {
	return filepath.Join(switchDataDirectory(), "imei.json")
}

// imeiConfigFile IMEI 配置文件结构
type imeiConfigFile struct {
	IMEI string `json:"imei"`
}

// loadIMEI 从配置文件加载已保存的 IMEI
func loadIMEI() {
	path := imeiConfigPath()
	data, err := os.ReadFile(path)
	if err != nil {
		return
	}
	var cfg imeiConfigFile
	if err := json.Unmarshal(data, &cfg); err != nil {
		return
	}
	if cfg.IMEI != "" {
		readerIMEIMu.Lock()
		readerIMEI = cfg.IMEI
		readerIMEIMu.Unlock()
		log.Printf("已加载读卡器 VoWiFi 设备身份")
	}
}

// saveIMEI 保存 IMEI 到配置文件
func saveIMEI(imei string) error {
	path := imeiConfigPath()
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(imeiConfigFile{IMEI: imei}, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o600)
}

// getDeviceIMEI 返回稳定的设备身份。首次使用时生成并持久化，避免每次
// 状态刷新或 IMS 重连都改变身份而触发运营商侧的异常注册策略。
func (a *app) getDeviceIMEI() string {
	readerIMEIMu.Lock()
	defer readerIMEIMu.Unlock()
	if readerIMEI != "" {
		return readerIMEI
	}
	readerIMEI = generateRandomIMEI()
	if err := saveIMEI(readerIMEI); err != nil {
		log.Printf("读卡器 VoWiFi 设备身份保存失败: %v", err)
	}
	return readerIMEI
}

// generateRandomIMEI 生成随机 15 位 IMEI
// 格式：TAC(8位, 前缀353936) + SNR(6位随机) + Luhn校验位(1位)
// A-M1 fix: use crypto/rand instead of math/rand with predictable time seed
func generateRandomIMEI() string {
	tac := "353936" + fmt.Sprintf("%02d", cryptoRandIntn(100))
	snr := fmt.Sprintf("%06d", cryptoRandIntn(1000000))
	imei14 := tac + snr
	checksum := luhnChecksum(imei14)
	return imei14 + string(rune('0'+checksum))
}

// cryptoRandIntn returns a uniform random int in [0, n) using crypto/rand.
func cryptoRandIntn(n int) int {
	if n <= 0 {
		return 0
	}
	b := make([]byte, 4)
	if _, err := rand.Read(b); err != nil {
		// Fallback should never happen, but return 0 rather than panic
		return 0
	}
	v := int(binary.BigEndian.Uint32(b)) % n
	if v < 0 {
		v += n
	}
	return v
}

// luhnChecksum 计算 IMEI 的 Luhn 校验位
func luhnChecksum(imei14 string) int {
	sum := 0
	for i := 0; i < len(imei14); i++ {
		d := int(imei14[i] - '0')
		if i%2 == 1 {
			d *= 2
			if d > 9 {
				d -= 9
			}
		}
		sum += d
	}
	return (10 - sum%10) % 10
}

// readerIMEIHandler 处理 IMEI 配置 API
// GET /api/reader/imei — 获取当前 IMEI
// PUT /api/reader/imei — 设置 IMEI {"imei": "123456789012345"}
// DELETE /api/reader/imei — 清除 IMEI
func (a *app) readerIMEIHandler(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		readerIMEIMu.RLock()
		imei := readerIMEI
		readerIMEIMu.RUnlock()
		writeJSON(w, http.StatusOK, map[string]any{
			"imei":   imei,
			"config": imei != "",
		})

	case http.MethodPut:
		var req struct {
			IMEI string `json:"imei"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeError(w, http.StatusBadRequest, "请求格式错误")
			return
		}
		imei := strings.TrimSpace(req.IMEI)
		if imei != "" {
			// 校验 IMEI 格式：15 位数字
			if len(imei) != 15 || !strings.ContainsFunc(imei, func(c rune) bool {
				return c < '0' || c > '9'
			}) == false {
				// 更精确的检查
				allDigits := true
				for _, c := range imei {
					if c < '0' || c > '9' {
						allDigits = false
						break
					}
				}
				if !allDigits || len(imei) != 15 {
					writeError(w, http.StatusBadRequest, "IMEI 必须为 15 位数字")
					return
				}
			}
		}
		readerIMEIMu.Lock()
		readerIMEI = imei
		readerIMEIMu.Unlock()
		if err := saveIMEI(imei); err != nil {
			log.Printf("IMEI 保存失败: %v", err)
		}
		log.Printf("读卡器 VoWiFi 设备身份已更新")
		writeJSON(w, http.StatusOK, map[string]any{
			"imei":    imei,
			"message": "IMEI 已保存",
		})

	case http.MethodDelete:
		readerIMEIMu.Lock()
		readerIMEI = ""
		readerIMEIMu.Unlock()
		_ = saveIMEI("")
		writeJSON(w, http.StatusOK, map[string]any{
			"message": "IMEI 已清除",
		})

	default:
		w.Header().Set("Allow", "GET, PUT, DELETE")
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
	}
}

// ConstructEPDGFQDN 从 SIM 卡的 MCC/MNC 构造 ePDG FQDN
func ConstructEPDGFQDN(mcc, mnc string) string {
	return carrier.ConstructEPDGFQDN(mcc, mnc)
}

// checkProxyHandler first verifies the local SOCKS5 endpoint without relying
// on any public website. Exit-IP geolocation is optional and bounded: a Surge
// rule that blocks ip-api.com must never turn a healthy local proxy check into
// a UI timeout.
// POST /api/reader/vowifi/proxy-check
// 请求体中的 proxy 由用户手动填写，不提供内置代理地址。
// 响应体: {"ok": true, "country_code": "GB", "country": "United Kingdom", "exit_ip": "1.2.3.4", "latency_ms": 120}
func (a *app) checkProxyHandler(w http.ResponseWriter, r *http.Request) {
	var req proxyCheckRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "请求格式错误")
		return
	}

	proxyAddr := strings.TrimSpace(req.Proxy)
	if proxyAddr == "" {
		writeJSON(w, http.StatusOK, proxyCheckResult{Error: "代理地址为空"})
		return
	}

	// 去除 socks5:// / socks5h:// 前缀
	proxyAddr = strings.TrimPrefix(strings.TrimPrefix(proxyAddr, "socks5://"), "socks5h://")

	probe, native, err := probeRuntimeProxy(r.Context(), proxyAddr)
	var latency int64
	if native {
		latency = probe.Latency
	} else {
		latency, err = probeSOCKS5(proxyAddr, 3*time.Second)
	}
	if err != nil {
		writeJSON(w, http.StatusOK, proxyCheckResult{Error: fmt.Sprintf("代理连接失败: %v", err)})
		return
	}

	result := proxyCheckResult{
		OK:           true,
		ProbeMessage: "SOCKS5 本地握手正常",
		Latency:      latency,
	}
	if native && !probe.UDPReady {
		result.ProbeMessage += "；UDP ASSOCIATE 不可用"
	}
	epdg := strings.TrimSpace(req.EPDG)
	var (
		mode         string
		transportErr error
		geo          proxyGeoInfo
		geoErr       error
		checks       sync.WaitGroup
	)
	// DNS/TUN readiness and optional geolocation are independent. Running them
	// concurrently caps the normal check near four seconds instead of adding
	// their timeouts together.
	checks.Add(1)
	go func() {
		defer checks.Done()
		geo, geoErr = queryProxyGeo(proxyAddr, 4*time.Second)
	}()
	if epdg != "" {
		checks.Add(1)
		go func() {
			defer checks.Done()
			_, mode, transportErr = selectReaderSWUTransport(r.Context(), proxyAddr, epdg)
		}()
	}
	checks.Wait()

	if epdg == "" {
		result.UDPMessage = "尚无 ePDG，连接读卡器后可检测 IKE UDP/TUN 路径"
	} else if transportErr != nil {
		result.UDPMessage = transportErr.Error()
	} else {
		result.UDPReady = true
		result.Transport = mode
		result.UDPMessage = "IKE UDP 500/4500 传输路径已就绪"
	}
	if geoErr == nil {
		result.CountryCode = geo.CountryCode
		result.Country = geo.Country
		result.ExitIP = geo.Query
	} else {
		result.ProbeMessage += "；出口归属查询不可用（不影响本地代理检测）"
	}
	writeJSON(w, http.StatusOK, result)
}

func probeSOCKS5(proxyAddr string, timeout time.Duration) (int64, error) {
	startedAt := time.Now()
	conn, err := net.DialTimeout("tcp", proxyAddr, timeout)
	if err != nil {
		return 0, err
	}
	defer conn.Close()
	if err := conn.SetDeadline(time.Now().Add(timeout)); err != nil {
		return 0, err
	}
	if _, err := conn.Write([]byte{0x05, 0x01, 0x00}); err != nil {
		return 0, fmt.Errorf("发送 SOCKS5 握手: %w", err)
	}
	resp := make([]byte, 2)
	if _, err := io.ReadFull(conn, resp); err != nil {
		return 0, fmt.Errorf("读取 SOCKS5 握手: %w", err)
	}
	if resp[0] != 0x05 || resp[1] != 0x00 {
		return 0, fmt.Errorf("SOCKS5 无认证模式不可用 (method=%d)", resp[1])
	}
	return time.Since(startedAt).Milliseconds(), nil
}

type proxyGeoInfo struct {
	Query       string `json:"query"`
	Country     string `json:"country"`
	CountryCode string `json:"countryCode"`
	Status      string `json:"status"`
	Message     string `json:"message"`
}

func queryProxyGeo(proxyAddr string, timeout time.Duration) (proxyGeoInfo, error) {
	conn, err := socks5TCPDial(proxyAddr, "ip-api.com", 80, timeout)
	if err != nil {
		return proxyGeoInfo{}, err
	}
	defer conn.Close()
	httpReq := "GET /json/ HTTP/1.1\r\nHost: ip-api.com\r\nConnection: close\r\nUser-Agent: OneWIFI/1.0\r\n\r\n"
	if _, err := conn.Write([]byte(httpReq)); err != nil {
		return proxyGeoInfo{}, err
	}
	const maxProxyGeoResponse = 64 * 1024
	respData, err := io.ReadAll(io.LimitReader(conn, maxProxyGeoResponse+1))
	if err != nil {
		return proxyGeoInfo{}, err
	}
	if len(respData) > maxProxyGeoResponse {
		return proxyGeoInfo{}, fmt.Errorf("出口接口响应过大")
	}
	body := extractHTTPBody(respData)
	if body == nil {
		return proxyGeoInfo{}, fmt.Errorf("出口接口响应无效")
	}
	var info proxyGeoInfo
	if err := json.Unmarshal(body, &info); err != nil {
		return proxyGeoInfo{}, err
	}
	if info.Status != "success" {
		return proxyGeoInfo{}, fmt.Errorf("出口接口: %s", info.Message)
	}
	return info, nil
}

// socks5TCPDial 通过 SOCKS5 代理建立 TCP 连接到目标地址
func socks5TCPDial(proxyAddr, dstHost string, dstPort int, timeout time.Duration) (net.Conn, error) {
	// 解析目标主机名为 IP（SOCKS5 需要直接用 IP 或域名）
	// 使用 SOCKS5 TCP CONNECT
	// 先建立到代理的 TCP 连接
	conn, err := net.DialTimeout("tcp", proxyAddr, timeout)
	if err != nil {
		return nil, fmt.Errorf("连接代理失败: %w", err)
	}
	if err := conn.SetDeadline(time.Now().Add(timeout)); err != nil {
		conn.Close()
		return nil, fmt.Errorf("设置代理检测期限失败: %w", err)
	}

	// SOCKS5 握手 — 无认证
	if _, err := conn.Write([]byte{0x05, 0x01, 0x00}); err != nil {
		conn.Close()
		return nil, fmt.Errorf("SOCKS5 握手失败: %w", err)
	}
	resp := make([]byte, 2)
	if _, err := io.ReadFull(conn, resp); err != nil {
		conn.Close()
		return nil, fmt.Errorf("读取握手响应失败: %w", err)
	}
	if resp[0] != 0x05 || resp[1] != 0x00 {
		conn.Close()
		return nil, fmt.Errorf("SOCKS5 握手被拒绝: method=%d", resp[1])
	}

	// SOCKS5 CONNECT — 使用域名模式 (ATYP=0x03)
	hostBytes := []byte(dstHost)
	req := make([]byte, 0, 4+1+len(hostBytes)+2)
	req = append(req, 0x05) // VER
	req = append(req, 0x01) // CMD=CONNECT
	req = append(req, 0x00) // RSV
	req = append(req, 0x03) // ATYP=Domain
	req = append(req, byte(len(hostBytes)))
	req = append(req, hostBytes...)
	req = append(req, byte(dstPort>>8), byte(dstPort&0xff))

	if _, err := conn.Write(req); err != nil {
		conn.Close()
		return nil, fmt.Errorf("发送 CONNECT 请求失败: %w", err)
	}

	// 读取响应头
	rep := make([]byte, 4)
	if _, err := io.ReadFull(conn, rep); err != nil {
		conn.Close()
		return nil, fmt.Errorf("读取 CONNECT 响应失败: %w", err)
	}
	if rep[1] != 0x00 {
		conn.Close()
		return nil, fmt.Errorf("SOCKS5 CONNECT 失败: rep=%d", rep[1])
	}

	// 读取并丢弃 BND.ADDR + BND.PORT
	// A-H fix: don't ignore io.ReadFull errors — a truncated response means
	// the SOCKS5 tunnel is broken and the connection must not be returned.
	switch rep[3] {
	case 0x01: // IPv4
		if _, err := io.ReadFull(conn, make([]byte, 4+2)); err != nil {
			conn.Close()
			return nil, fmt.Errorf("读取 BND.ADDR (IPv4) 失败: %w", err)
		}
	case 0x03: // Domain
		lb := make([]byte, 1)
		if _, err := io.ReadFull(conn, lb); err != nil {
			conn.Close()
			return nil, fmt.Errorf("读取 BND.ADDR 长度字节失败: %w", err)
		}
		if _, err := io.ReadFull(conn, make([]byte, int(lb[0])+2)); err != nil {
			conn.Close()
			return nil, fmt.Errorf("读取 BND.ADDR (Domain) 失败: %w", err)
		}
	case 0x04: // IPv6
		if _, err := io.ReadFull(conn, make([]byte, 16+2)); err != nil {
			conn.Close()
			return nil, fmt.Errorf("读取 BND.ADDR (IPv6) 失败: %w", err)
		}
	}

	return conn, nil
}

// extractHTTPBody 从 HTTP 响应中提取 body
func extractHTTPBody(data []byte) []byte {
	// 找到 \r\n\r\n 分隔符
	idx := -1
	for i := 0; i < len(data)-3; i++ {
		if data[i] == '\r' && data[i+1] == '\n' && data[i+2] == '\r' && data[i+3] == '\n' {
			idx = i + 4
			break
		}
	}
	if idx < 0 || idx >= len(data) {
		return nil
	}
	return data[idx:]
}
