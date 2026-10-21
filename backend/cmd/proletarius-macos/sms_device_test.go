package main

import (
	"errors"
	"net/http"
	"testing"
)

func TestSelectSMSDeviceRequiresChoiceWhenBothReady(t *testing.T) {
	devices := []smsDeviceStatus{
		{ID: "dji", Name: "DJI 模块", Available: true, Ready: true},
		{ID: "reader", Name: "读卡器 VoWiFi", Available: true, Ready: true},
	}
	if _, err := selectSMSDevice(devices, ""); err == nil {
		t.Fatal("dual ready devices must require an explicit selection")
	} else {
		var selectionError *smsDeviceSelectionError
		if !errors.As(err, &selectionError) || selectionError.Status != http.StatusConflict || selectionError.Code != "sms_device_required" {
			t.Fatalf("dual-device error = %#v, want typed conflict", err)
		}
	}
	for _, id := range []string{"dji", "reader"} {
		got, err := selectSMSDevice(devices, id)
		if err != nil || got != id {
			t.Fatalf("select %q = %q, %v", id, got, err)
		}
	}
}

func TestSelectSMSDeviceReportsUnavailableDevice(t *testing.T) {
	devices := []smsDeviceStatus{
		{ID: "dji", Name: "DJI 模块", Available: true, Ready: false, Detail: "DJI 模块短信通道正在连接"},
		{ID: "reader", Name: "读卡器 VoWiFi", Available: false, Ready: false, Detail: "读卡器未连接"},
	}
	_, err := selectSMSDevice(devices, "dji")
	var selectionError *smsDeviceSelectionError
	if !errors.As(err, &selectionError) {
		t.Fatalf("error = %#v, want smsDeviceSelectionError", err)
	}
	if selectionError.Status != http.StatusServiceUnavailable || selectionError.Code != "sms_device_not_ready" || !selectionError.Recoverable {
		t.Fatalf("selection error = %#v", selectionError)
	}
}

func TestSelectSMSDeviceAutoSelectsOnlyReadyDevice(t *testing.T) {
	devices := []smsDeviceStatus{
		{ID: "dji", Name: "DJI 模块", Available: true, Ready: true},
		{ID: "reader", Name: "读卡器 VoWiFi", Available: true, Ready: false, Detail: "IMS 尚未注册"},
	}
	got, err := selectSMSDevice(devices, "")
	if err != nil || got != "dji" {
		t.Fatalf("auto selection = %q, %v; want dji", got, err)
	}
	if _, err := selectSMSDevice(devices, "reader"); err == nil {
		t.Fatal("an explicitly selected unready reader must fail closed")
	}
}

func TestSMSCacheKeySeparatesDevices(t *testing.T) {
	base := receivedSMS{Sender: "+447700900123", Content: "same"}
	dji := base
	dji.Device = "dji"
	reader := base
	reader.Device = "reader"
	if smsCacheKey(dji) == smsCacheKey(reader) {
		t.Fatal("messages from different devices must not deduplicate")
	}
}

func TestSMSDeviceDisplayName(t *testing.T) {
	if got := smsDeviceDisplayName("+447882950731", "Reader"); got != "+447882950731_Reader" {
		t.Fatalf("reader display name = %q", got)
	}
	if got := smsDeviceDisplayName("", "DJI"); got != "未知号码_DJI" {
		t.Fatalf("fallback display name = %q", got)
	}
}

func TestModuleSMSRouteReadinessRequiresBothSMSCAndTarget(t *testing.T) {
	for _, test := range []struct {
		name, smsc, target string
		want               bool
	}{
		{name: "complete route", smsc: "+441234567890", target: "tel:+441234567890", want: true},
		{name: "missing SMSC", target: "sip:smsc@example.test"},
		{name: "missing target", smsc: "+441234567890"},
		{name: "blank", smsc: " ", target: "\t"},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := moduleSMSRouteReady(test.smsc, test.target); got != test.want {
				t.Fatalf("moduleSMSRouteReady()=%t, want %t", got, test.want)
			}
		})
	}
}

func TestParseUSBATCNUM(t *testing.T) {
	tests := []struct {
		name     string
		response string
		want     string
	}{
		{name: "international", response: `AT+CNUM\r\n+CNUM: "","+447882950731",145\r\nOK`, want: "+447882950731"},
		{name: "tel prefix", response: "+CNUM: \"Line 1\",\"tel:+372 5555 1234\",145", want: "+37255551234"},
		{name: "placeholder", response: `+CNUM: "","FFFFFFFF",129`, want: ""},
		{name: "zero placeholder", response: `+CNUM: "","0000000000",129`, want: ""},
		{name: "invalid", response: `+CNUM: "","unknown",129`, want: ""},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := parseUSBATCNUM(test.response); got != test.want {
				t.Fatalf("parseUSBATCNUM() = %q, want %q", got, test.want)
			}
		})
	}
}
