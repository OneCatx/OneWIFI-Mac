package main

import (
	"context"
	"net/http"
	"strings"
	"time"
)

// Developer diagnostics and the existing reader page share the same
// app-owned BluetoothReaderManager. These API methods never issue raw APDUs.
func (a *app) readerBLEStatus(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	devices, selected, state, errorCode, err := readerBLEClient().Status(ctx)
	if err != nil {
		writeCodedError(w, http.StatusServiceUnavailable, "ble_bridge_unavailable", err.Error(), true)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"devices": devices, "connected": selected,
		"state": state, "error_code": errorCode,
	})
}

func (a *app) readerBLEScan(w http.ResponseWriter, r *http.Request) {
	if !beginReaderVoWiFiOperation() {
		writeCodedError(w, http.StatusConflict, "reader_operation_active", "读卡器会话正在使用设备，请稍后重试", true)
		return
	}
	defer endReaderVoWiFiOperation()
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	if err := readerBLEClient().Scan(ctx); err != nil {
		writeCodedError(w, http.StatusServiceUnavailable, "ble_scan_failed", err.Error(), true)
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]any{"scanning": true})
}

func (a *app) readerBLEConnect(w http.ResponseWriter, r *http.Request) {
	var request struct {
		Reader string `json:"reader"`
	}
	if !decodeJSON(w, r, &request) {
		return
	}
	reader := strings.TrimSpace(request.Reader)
	if reader == "" {
		writeCodedError(w, http.StatusBadRequest, "ble_reader_required", "请选择蓝牙读卡器", false)
		return
	}
	if !beginReaderVoWiFiOperation() {
		writeCodedError(w, http.StatusConflict, "reader_operation_active", "读卡器会话正在使用设备，请稍后重试", true)
		return
	}
	defer endReaderVoWiFiOperation()
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	if err := readerBLEClient().Connect(ctx, reader); err != nil {
		writeCodedError(w, http.StatusBadGateway, "ble_connect_failed", err.Error(), true)
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]any{"connecting": true})
}

func (a *app) readerBLEDisconnect(w http.ResponseWriter, r *http.Request) {
	if !beginReaderVoWiFiOperation() {
		writeCodedError(w, http.StatusConflict, "reader_operation_active", "读卡器会话正在使用设备，请稍后重试", true)
		return
	}
	defer endReaderVoWiFiOperation()
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	if err := readerBLEClient().Disconnect(ctx); err != nil {
		writeCodedError(w, http.StatusBadGateway, "ble_disconnect_failed", err.Error(), true)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}
