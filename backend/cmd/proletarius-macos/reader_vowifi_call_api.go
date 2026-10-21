package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"proletarius.local/core/internal/vowifi/imscore"
)

type readerCallRequest struct {
	Peer       string `json:"peer,omitempty"`
	Digit      string `json:"digit,omitempty"`
	Generation uint64 `json:"generation,omitempty"`
}

type readerCallResponse struct {
	Generation uint64            `json:"generation"`
	Source     string            `json:"source,omitempty"`
	Call       imscore.CallState `json:"call"`
}

// activeReaderCallSession snapshots the exact VoWiFi session that owns a call
// operation.  The public call-status endpoint uses the session identity as
// well as the service pointer so a previous eSIM profile cannot be exposed
// after the reader manager has observed a newer card generation.
func activeReaderCallSession(generation uint64) (*vowifiSession, *imscore.Service, uint64, error) {
	vowifiSessionMu.Lock()
	session := activeSession
	vowifiSessionMu.Unlock()
	if session == nil {
		return nil, nil, 0, fmt.Errorf("读卡器 VoWiFi 未连接")
	}
	session.mu.Lock()
	service, current := session.imsService, session.readerGeneration
	session.mu.Unlock()
	if generation != 0 && generation != current {
		return nil, nil, current, fmt.Errorf("eSIM Profile 已切换，请刷新后重试")
	}
	return session, service, current, nil
}

// activeReaderCallService is the single resolver for every reader call
// operation.  Unlike a caller-provided generation alone, it also verifies
// that the active session still belongs to the reader/SIM observation that is
// current at the API boundary.
func (a *app) activeReaderCallService(ctx context.Context, generation uint64, verifyModule bool) (*imscore.Service, uint64, string, error) {
	session, service, current, err := activeReaderCallSession(generation)
	if err != nil {
		return nil, current, "", err
	}
	source := session.sourceKind
	if source == "module" {
		if session.moduleSIM == nil || session.radioLease == nil {
			return nil, current, source, fmt.Errorf("DJI 模块 VoWiFi 身份状态不可用")
		}
		session.radioLease.mu.Lock()
		radioLeaseActive := session.radioLease.active
		session.radioLease.mu.Unlock()
		if !radioLeaseActive {
			return nil, current, source, fmt.Errorf("DJI 模块未处于 VoWiFi 飞行模式，通话已拒绝")
		}
		if verifyModule {
			if ctx == nil {
				ctx = context.Background()
			}
			if err := session.moduleSIM.validateProfile(ctx); err != nil {
				return nil, current, source, fmt.Errorf("DJI 模块 eSIM Profile 已变化，通话已拒绝: %w", err)
			}
			mode, modeErr := readModuleCFUN(a)
			if modeErr != nil || mode != 4 {
				return nil, current, source, fmt.Errorf("DJI 模块当前不在飞行模式，VoWiFi 通话已拒绝")
			}
		}
	} else {
		if a == nil || a.readerMgr == nil {
			return nil, current, source, fmt.Errorf("读卡器或 eSIM Profile 已变更，请刷新后重试")
		}
		reader, info := a.readerMgr.statusAndUSIMInfo()
		if !readerCallSessionMatchesCurrentIdentity(session, reader, info) {
			return nil, current, source, fmt.Errorf("读卡器或 eSIM Profile 已变更，请刷新后重试")
		}
	}
	if service == nil || !service.CallReady() {
		return nil, current, source, fmt.Errorf("IMS 通话通道或受保护的入站 SIP 监听尚未就绪")
	}
	return service, current, source, err
}

// readerCallSessionMatchesCurrentIdentity deliberately reuses the public
// VoWiFi identity guard.  Call state is sensitive: unlike an ordinary idle
// status, it must fail closed if a session did not retain all of its reader,
// SIM and generation identity fields.
func readerCallSessionMatchesCurrentIdentity(session *vowifiSession, reader readerStatus, info *usimInfo) bool {
	if session == nil {
		return false
	}
	session.mu.Lock()
	identity := readerVoWiFiStatus{
		ReaderID:         string(session.sessionKey.ReaderID),
		SIMID:            session.sessionKey.SIMID,
		ReaderGeneration: session.readerGeneration,
	}
	session.mu.Unlock()
	if strings.TrimSpace(identity.ReaderID) == "" ||
		strings.TrimSpace(identity.SIMID) == "" ||
		identity.ReaderGeneration == 0 {
		return false
	}
	return readerVoWiFiStatusMatchesCurrentIdentity(&identity, reader, info)
}

func decodeReaderCallRequest(r *http.Request) (readerCallRequest, error) {
	var request readerCallRequest
	if r.Body == nil {
		return request, nil
	}
	err := json.NewDecoder(r.Body).Decode(&request)
	return request, err
}

func (a *app) readerCallStatus(w http.ResponseWriter, r *http.Request) {
	service, generation, source, err := a.activeReaderCallService(r.Context(), 0, false)
	if err != nil {
		writeJSON(w, http.StatusOK, readerCallResponse{Call: imscore.CallState{State: "unavailable"}})
		return
	}
	state := service.CallStatus()
	a.trackVoWiFiCallState(source, state)
	writeJSON(w, http.StatusOK, readerCallResponse{Generation: generation, Source: source, Call: state})
}

func (a *app) readerCallDial(w http.ResponseWriter, r *http.Request) {
	request, err := decodeReaderCallRequest(r)
	if err != nil || strings.TrimSpace(request.Peer) == "" {
		writeError(w, http.StatusBadRequest, "请输入有效的被叫号码")
		return
	}
	service, generation, source, err := a.activeReaderCallService(r.Context(), request.Generation, true)
	if err != nil {
		writeError(w, http.StatusConflict, err.Error())
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	if err := service.DialCall(ctx, request.Peer); err != nil {
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	state := service.CallStatus()
	a.trackVoWiFiCallState(source, state)
	writeJSON(w, http.StatusAccepted, readerCallResponse{Generation: generation, Source: source, Call: state})
}

func (a *app) readerCallAnswer(w http.ResponseWriter, r *http.Request) {
	request, _ := decodeReaderCallRequest(r)
	service, generation, source, err := a.activeReaderCallService(r.Context(), request.Generation, true)
	if err != nil {
		writeError(w, http.StatusConflict, err.Error())
		return
	}
	if err := service.AnswerCall(r.Context()); err != nil {
		writeError(w, http.StatusConflict, err.Error())
		return
	}
	state := service.CallStatus()
	a.trackVoWiFiCallState(source, state)
	writeJSON(w, http.StatusOK, readerCallResponse{Generation: generation, Source: source, Call: state})
}

func (a *app) readerCallReject(w http.ResponseWriter, r *http.Request) {
	request, _ := decodeReaderCallRequest(r)
	service, generation, source, err := a.activeReaderCallService(r.Context(), request.Generation, true)
	if err != nil {
		writeError(w, http.StatusConflict, err.Error())
		return
	}
	if err := service.RejectCall(); err != nil {
		writeError(w, http.StatusConflict, err.Error())
		return
	}
	state := service.CallStatus()
	a.trackVoWiFiCallState(source, state)
	writeJSON(w, http.StatusOK, readerCallResponse{Generation: generation, Source: source, Call: state})
}

func (a *app) readerCallHangup(w http.ResponseWriter, r *http.Request) {
	request, _ := decodeReaderCallRequest(r)
	service, generation, source, err := a.activeReaderCallService(r.Context(), request.Generation, true)
	if err != nil {
		writeError(w, http.StatusConflict, err.Error())
		return
	}
	if err := service.HangupCall(r.Context()); err != nil {
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	state := service.CallStatus()
	a.trackVoWiFiCallState(source, state)
	writeJSON(w, http.StatusOK, readerCallResponse{Generation: generation, Source: source, Call: state})
}

func (a *app) readerCallDTMF(w http.ResponseWriter, r *http.Request) {
	request, err := decodeReaderCallRequest(r)
	if err != nil || !validDTMFDigit(request.Digit) {
		writeError(w, http.StatusBadRequest, "按键只能是 0-9、* 或 #")
		return
	}
	service, generation, source, err := a.activeReaderCallService(r.Context(), request.Generation, true)
	if err != nil {
		writeError(w, http.StatusConflict, err.Error())
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
	defer cancel()
	if err := service.SendCallDTMF(ctx, request.Digit); err != nil {
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, readerCallResponse{Generation: generation, Source: source, Call: service.CallStatus()})
}

// trackReaderCallState 把读卡器 IMS 呼叫并入与模块一致的持久化通话记录。
// 使用独立设备会话，避免 DJI 与读卡器同时接入时彼此覆盖。
func (a *app) trackReaderCallState(state imscore.CallState) {
	a.trackVoWiFiCallState("reader", state)
}

func (a *app) trackVoWiFiCallState(source string, state imscore.CallState) {
	device := callDeviceReader
	if source == "module" {
		device = callDeviceModule
	}
	direction := "out"
	if state.Direction == "incoming" {
		direction = "in"
	}
	switch state.State {
	case "incoming", "outgoing", "early", "connecting":
		a.beginDeviceCall(device, direction, state.Peer)
		a.updateDeviceCallNumber(device, state.Peer)
	case "active":
		a.beginDeviceCall(device, direction, state.Peer)
		a.updateDeviceCallNumber(device, state.Peer)
		a.markDeviceCallAnswered(device)
	case "idle", "ended", "failed", "unavailable":
		a.endDeviceCall(device)
	}
}
