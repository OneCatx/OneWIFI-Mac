package main

import (
	"net/http"
	"regexp"
	"strings"
)

type smsDeviceStatus struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Kind      string `json:"kind"`
	Phone     string `json:"phone,omitempty"`
	Available bool   `json:"available"`
	Ready     bool   `json:"ready"`
	Detail    string `json:"detail,omitempty"`
}

const (
	smsDeviceDJI    = "dji"
	smsDeviceReader = "reader"
)

type smsDeviceSelectionError struct {
	Status      int
	Code        string
	Message     string
	Recoverable bool
}

func (e *smsDeviceSelectionError) Error() string { return e.Message }

func (a *app) currentSMSDevices() []smsDeviceStatus {
	moduleSession := activeVoWiFiSession("module")
	djiReady := a.modem != nil || a.usbAT != nil
	djiAvailable := djiReady || a.currentUSBDevice() != nil
	djiPhone := ""
	djiSMSRouteMissing := false
	if a.modem != nil {
		djiPhone = strings.TrimSpace(a.modem.GetFullStatus().PhoneNumber)
	} else {
		a.smsIdentityMu.RLock()
		djiPhone = strings.TrimSpace(a.djiPhoneNumber)
		a.smsIdentityMu.RUnlock()
	}
	if moduleSession != nil {
		moduleSession.mu.Lock()
		service, info := moduleSession.imsService, moduleSession.simInfo
		imsReady := service != nil && service.MessagingReady() && moduleSession.imsState == imsRegistered
		routeReady := info != nil && moduleSMSRouteReady(info.SMSC, info.SMSCTarget)
		djiReady = imsReady && routeReady
		if info != nil {
			djiPhone = strings.TrimSpace(info.MSISDN)
		}
		moduleSession.mu.Unlock()
		djiSMSRouteMissing = !djiReady && imsReady && !routeReady
	}
	dji := smsDeviceStatus{
		ID: smsDeviceDJI, Kind: smsDeviceDJI, Phone: djiPhone,
		Name:      smsDeviceDisplayName(djiPhone, "DJI"),
		Available: djiAvailable, Ready: djiReady,
	}
	if djiSMSRouteMissing {
		dji.Detail = "IMS 已注册，但当前 SIM 未提供可用 SMSC/IMS 短信目标"
	} else if !djiAvailable {
		dji.Detail = "未检测到 DJI 模块"
	} else if moduleSession != nil && !djiReady {
		dji.Detail = "DJI 模块 VoWiFi 短信通道尚未就绪"
	} else if !djiReady {
		dji.Detail = "DJI 模块短信通道正在连接"
	}

	readerConnected := false
	if a.readerMgr != nil {
		readerConnected = a.readerMgr.Status().Connected
	} else {
		// 仅供不创建 readerManager 的隔离测试使用。
		readerConnected = a.readerMode
	}
	readerStatus := getVoWiFiStatus()
	readerReady := readerConnected && readerStatus.IMSState == imsRegistered && readerStatus.SMSReady
	readerPhone := strings.TrimSpace(readerStatus.RegisteredMSISDN)
	if isMissingPhoneNumber(readerPhone) {
		readerPhone = ""
	}
	if readerPhone == "" && a.readerMgr != nil {
		if info := a.readerMgr.USIMInfo(); info != nil {
			readerPhone = strings.TrimSpace(info.PhoneNumber)
			if isMissingPhoneNumber(readerPhone) {
				readerPhone = ""
				readerPhone = a.profilePhoneForICCID(info.ICCID)
			}
		}
	}
	reader := smsDeviceStatus{
		ID: smsDeviceReader, Kind: smsDeviceReader, Phone: readerPhone,
		Name:      smsDeviceDisplayName(readerPhone, "Reader"),
		Available: readerConnected, Ready: readerReady,
	}
	if !readerConnected {
		reader.Detail = "读卡器未连接"
	} else if readerStatus.IMSState != imsRegistered {
		reader.Detail = "读卡器 IMS 尚未注册"
	} else if !readerStatus.SMSReady {
		reader.Detail = "读卡器 IMS 短信通道尚未就绪"
	}
	return []smsDeviceStatus{dji, reader}
}

func smsDeviceDisplayName(phone, suffix string) string {
	phone = strings.TrimSpace(phone)
	suffix = strings.TrimSpace(suffix)
	if phone == "" {
		return "未知号码_" + suffix
	}
	return phone + "_" + suffix
}

func moduleSMSRouteReady(smsc, target string) bool {
	return strings.TrimSpace(smsc) != "" && strings.TrimSpace(target) != ""
}

func parseUSBATCNUM(response string) string {
	matches := regexp.MustCompile(`(?i)\+CNUM:\s*"[^"]*"\s*,\s*"([^"]*)"`).FindAllStringSubmatch(response, -1)
	for _, match := range matches {
		if len(match) != 2 {
			continue
		}
		phone := strings.TrimSpace(match[1])
		if strings.HasPrefix(strings.ToLower(phone), "tel:") {
			phone = strings.TrimSpace(phone[4:])
		}
		phone = strings.NewReplacer(" ", "", "-", "", "(", "", ")", "").Replace(phone)
		if phone == "" || strings.EqualFold(phone, "ffffffff") {
			continue
		}
		digits := strings.TrimPrefix(phone, "+")
		if len(digits) < 5 || len(digits) > 15 {
			continue
		}
		valid := true
		for _, digit := range digits {
			if digit < '0' || digit > '9' {
				valid = false
				break
			}
		}
		if valid {
			// Some module firmware reports a zero-filled CNUM when the SIM has
			// no MSISDN record. Do not let it mask the ICCID-keyed saved number.
			if strings.Trim(digits, "0") == "" {
				continue
			}
			return phone
		}
	}
	return ""
}

func (a *app) smsDevices(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"devices": a.currentSMSDevices()})
}

func selectSMSDevice(devices []smsDeviceStatus, requested string) (string, error) {
	requested = strings.ToLower(strings.TrimSpace(requested))
	known := make(map[string]smsDeviceStatus, len(devices))
	ready := make([]string, 0, len(devices))
	for _, device := range devices {
		known[device.ID] = device
		if device.Ready {
			ready = append(ready, device.ID)
		}
	}
	if requested != "" {
		device, ok := known[requested]
		if !ok {
			return "", &smsDeviceSelectionError{
				Status: http.StatusBadRequest, Code: "invalid_sms_device",
				Message: "未知短信发送设备：" + requested,
			}
		}
		if !device.Ready {
			message := device.Name + "短信通道尚未就绪"
			if device.Detail != "" {
				message = device.Name + "：" + device.Detail
			}
			return "", &smsDeviceSelectionError{
				Status: http.StatusServiceUnavailable, Code: "sms_device_not_ready",
				Message: message, Recoverable: true,
			}
		}
		return requested, nil
	}
	if len(ready) == 0 {
		return "", &smsDeviceSelectionError{
			Status: http.StatusServiceUnavailable, Code: "sms_device_unavailable",
			Message: "当前没有可用的短信发送设备", Recoverable: true,
		}
	}
	if len(ready) > 1 {
		return "", &smsDeviceSelectionError{
			Status: http.StatusConflict, Code: "sms_device_required",
			Message: "DJI 模块和读卡器 VoWiFi 均可发送，请先选择发送设备",
		}
	}
	return ready[0], nil
}
