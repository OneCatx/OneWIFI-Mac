package main

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"
)

type nativeReaderSessionLeaseRequest struct {
	Reader string `json:"reader"`
}

type nativeReaderSessionLeaseReleaseRequest struct {
	LeaseID string `json:"lease_id"`
}

type nativeReaderSessionLeaseRenewRequest struct {
	LeaseID string `json:"lease_id"`
}

type nativeReaderSessionLeaseAKARequest struct {
	LeaseID          string `json:"lease_id"`
	Reader           string `json:"reader"`
	ReaderGeneration uint64 `json:"reader_generation"`
	Application      string `json:"application"`
	RAND             []int  `json:"rand"`
	AUTN             []int  `json:"autn"`
}

type nativeReaderSessionLeaseAKAResponse struct {
	Outcome string `json:"outcome"`
	RES     []int  `json:"res,omitempty"`
	CK      []int  `json:"ck,omitempty"`
	IK      []int  `json:"ik,omitempty"`
	AUTS    []int  `json:"auts,omitempty"`
}

// These routes are reachable only over the private authenticated Go socket.
// Rust uses the lease to freeze reader ownership, read a generation-bound
// identity snapshot, and request only serialized AKA APDUs for that lease.
func (a *app) acquireNativeReaderSessionLease(w http.ResponseWriter, r *http.Request) {
	var request nativeReaderSessionLeaseRequest
	if !decodeJSON(w, r, &request) {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), nativeReaderLeaseAcquireTimeout+time.Second)
	defer cancel()
	lease, err := a.readerMgr.acquireNativeReaderSessionLease(ctx, request.Reader)
	if err != nil {
		switch {
		case errors.Is(err, errNativeReaderLeaseBusy):
			writeCodedError(w, http.StatusConflict, "reader_session_lease_busy", "读卡器正由另一 VoWiFi 会话或卡片操作使用", true)
		case errors.Is(err, errNativeReaderLeaseStale):
			writeCodedError(w, http.StatusConflict, "reader_session_identity_changed", "读卡器连接或卡片身份已变化，请刷新后重试", true)
		case errors.Is(err, errNativeReaderLeaseNoIdentity):
			writeCodedError(w, http.StatusServiceUnavailable, "reader_identity_unavailable", "读卡器尚未取得稳定的 SIM 身份信息", true)
		case errors.Is(err, context.DeadlineExceeded):
			writeCodedError(w, http.StatusGatewayTimeout, "reader_session_lease_timeout", "等待读卡器当前 APDU 操作完成超时", true)
		case errors.Is(err, context.Canceled):
			writeCodedError(w, http.StatusRequestTimeout, "reader_session_lease_cancelled", "读卡器租约请求已取消", true)
		default:
			writeCodedError(w, http.StatusInternalServerError, "reader_session_lease_failed", "无法建立读卡器会话租约", true)
		}
		return
	}
	writeJSON(w, http.StatusOK, lease)
}

func (a *app) releaseNativeReaderSessionLease(w http.ResponseWriter, r *http.Request) {
	var request nativeReaderSessionLeaseReleaseRequest
	if !decodeJSON(w, r, &request) {
		return
	}
	if !a.readerMgr.releaseNativeReaderSessionLease(strings.TrimSpace(request.LeaseID)) {
		writeCodedError(w, http.StatusConflict, "reader_session_lease_invalid", "读卡器会话租约无效或已过期", false)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"released": true})
}

// renewNativeReaderSessionLease is private to the authenticated Unix socket.
// A Rust session actor renews well before the two-minute watchdog deadline;
// callers that lose ownership cannot extend or resurrect an expired lease.
func (a *app) renewNativeReaderSessionLease(w http.ResponseWriter, r *http.Request) {
	var request nativeReaderSessionLeaseRenewRequest
	if !decodeJSON(w, r, &request) {
		return
	}
	if !a.readerMgr.renewNativeReaderSessionLease(request.LeaseID) {
		writeCodedError(w, http.StatusConflict, "reader_session_lease_invalid", "读卡器会话租约无效或已过期", true)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"renewed": true})
}

// authenticateNativeReaderSessionLease is the only APDU path admitted while
// Rust owns the native reader lease. The authenticated socket plus the opaque
// lease token bind it to the exact reader generation; ordinary card operations
// remain excluded until release/expiry.
func (a *app) authenticateNativeReaderSessionLease(w http.ResponseWriter, r *http.Request) {
	var request nativeReaderSessionLeaseAKARequest
	if !decodeJSON(w, r, &request) {
		return
	}
	rand, err := checkedOctets(request.RAND, 16, 16)
	if err != nil {
		writeCodedError(w, http.StatusBadRequest, "invalid_rand", "RAND 必须为 16 字节", false)
		return
	}
	defer clear(rand)
	autn, err := checkedOctets(request.AUTN, 16, 16)
	if err != nil {
		writeCodedError(w, http.StatusBadRequest, "invalid_autn", "AUTN 必须为 16 字节", false)
		return
	}
	defer clear(autn)

	result, authErr := a.readerMgr.authenticateNativeReaderSessionLease(
		request.LeaseID,
		request.Reader,
		request.ReaderGeneration,
		request.Application,
		rand,
		autn,
	)
	if result != nil {
		defer clear(result.RES)
		defer clear(result.CK)
		defer clear(result.IK)
		defer clear(result.AUTS)
	}
	if errors.Is(authErr, errReaderAKASyncFailure) && result != nil && len(result.AUTS) == 14 {
		response := nativeReaderSessionLeaseAKAResponse{
			Outcome: "synchronization_failure",
			AUTS:    byteInts(result.AUTS),
		}
		writeJSON(w, http.StatusOK, response)
		clear(response.AUTS)
		return
	}
	if errors.Is(authErr, errReaderAKARejected) {
		writeJSON(w, http.StatusOK, nativeReaderSessionLeaseAKAResponse{Outcome: "authentication_reject"})
		return
	}
	if authErr != nil || result == nil {
		code := "reader_authentication_failed"
		if errors.Is(authErr, errNativeReaderLeaseStale) {
			code = "reader_session_lease_stale"
		}
		writeCodedError(w, http.StatusBadGateway, code, "租约绑定的 SIM AKA 失败", true)
		return
	}
	if len(result.RES) < 4 || len(result.RES) > 16 || len(result.CK) != 16 || len(result.IK) != 16 {
		writeCodedError(w, http.StatusBadGateway, "reader_authentication_invalid", "SIM AKA 返回长度无效", false)
		return
	}
	response := nativeReaderSessionLeaseAKAResponse{
		Outcome: "success",
		RES:     byteInts(result.RES),
		CK:      byteInts(result.CK),
		IK:      byteInts(result.IK),
	}
	writeJSON(w, http.StatusOK, response)
	clear(response.RES)
	clear(response.CK)
	clear(response.IK)
}
