package main

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log"
	"log/slog"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/damonto/uicc-go/apdu"
	"github.com/damonto/uicc-go/usim"
	usimcard "github.com/damonto/uicc-go/usim/card"
	usimcommand "github.com/damonto/uicc-go/usim/command"
	"github.com/damonto/uicc-go/usim/simfile"
)

// moduleVoWiFiSIM is the module-backed SIM/AKA boundary. The QDC507 is used
// only for card APDUs; IKE, ESP and IMS traffic use the Mac network stack.
type moduleVoWiFiSIM struct {
	app       *app
	info      *usimInfo
	atCommand func(string, time.Duration) (string, error)

	mu sync.Mutex
}

// moduleUSIMReader re-selects EF_DIR for each application record. Some modem
// AT bridges report 0x6986 for a later EF_DIR record when the upstream reader
// relies on the previous SELECT still being active. Only that exact status is
// skipped; at least one parseable AID is still required before continuing.
type moduleUSIMReader struct {
	*usim.Reader
	transmitter usimcard.Transmitter
}

func newModuleUSIMReader(transmitter usimcard.Transmitter) (*moduleUSIMReader, error) {
	if transmitter == nil {
		return nil, errors.New("DJI SIM APDU transmitter unavailable")
	}
	reader, err := usim.NewReader(transmitter)
	if err != nil {
		return nil, err
	}
	return &moduleUSIMReader{Reader: reader, transmitter: transmitter}, nil
}

func (r *moduleUSIMReader) ListApplications(ctx context.Context) ([]usimcard.Application, error) {
	if r == nil || r.transmitter == nil {
		return nil, errors.New("DJI SIM APDU transmitter unavailable")
	}
	probe, err := usim.NewReader(r.transmitter)
	if err != nil {
		return nil, err
	}
	file := usimcard.FileRef{Path: []byte{0x2F, 0x00}}
	attributes, err := probe.FileAttributes(ctx, file)
	if err != nil {
		return nil, fmt.Errorf("reading EF_DIR attributes: %w", err)
	}
	if attributes.FileStructure != simfile.StructureLinearFixed || attributes.RecordCount == 0 {
		return nil, errors.New("reading EF_DIR: unexpected file structure")
	}

	return listModuleEFDirApplications(ctx, attributes.RecordCount, attributes.RecordSize,
		func(ctx context.Context, recordID uint16) ([]byte, error) {
			// A fresh Reader has no cached selection and therefore selects MF and
			// EF_DIR immediately before every READ RECORD.
			recordReader, err := usim.NewReader(r.transmitter)
			if err != nil {
				return nil, err
			}
			return recordReader.ReadRecord(ctx, usimcard.RecordRead{
				File: file, Record: recordID, Length: attributes.RecordSize,
			})
		})
}

func listModuleEFDirApplications(
	ctx context.Context,
	recordCount, recordSize uint16,
	readRecord func(context.Context, uint16) ([]byte, error),
) ([]usimcard.Application, error) {
	if readRecord == nil || recordCount == 0 || recordSize == 0 {
		return nil, errors.New("reading EF_DIR: invalid record metadata")
	}
	applications := make([]usimcard.Application, 0, recordCount)
	for recordID := uint16(1); recordID <= recordCount; recordID++ {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		record, err := readRecord(ctx, recordID)
		if err != nil {
			if apdu.IsStatus(err, 0x6986) {
				continue
			}
			return nil, fmt.Errorf("reading EF_DIR record %d: %w", recordID, err)
		}
		var parsed simfile.EFDirRecord
		if err := parsed.UnmarshalBinary(record); err != nil {
			return nil, fmt.Errorf("parsing EF_DIR record %d: %w", recordID, err)
		}
		if len(parsed.AID) == 0 {
			continue
		}
		applications = append(applications, usimcard.Application{
			AID: append([]byte(nil), parsed.AID...), Label: parsed.Label,
		})
	}
	if len(applications) == 0 {
		return nil, errors.New("reading EF_DIR: no accessible application AID")
	}
	return applications, nil
}

type moduleRadioLease struct {
	app         *app
	restoreMode int

	mu         sync.Mutex
	active     bool
	lastMode   int
	lastModeAt time.Time
}

func (l *moduleRadioLease) enter(ctx context.Context) error {
	if l == nil || l.app == nil {
		return errors.New("DJI radio lease unavailable")
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.active {
		return nil
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	mode, err := readModuleCFUN(l.app)
	if err != nil {
		return err
	}
	if mode != 4 {
		if _, err := l.app.runATCommand("AT+CFUN=4", 12*time.Second); err != nil {
			// The command may have taken effect before the USB bridge timed out.
			if current, checkErr := readModuleCFUN(l.app); checkErr != nil || current != 4 {
				return fmt.Errorf("启用模块飞行模式: %w", err)
			}
		}
	}
	l.active = true
	l.lastMode = 4
	l.lastModeAt = time.Now()
	if err := waitModuleCFUN(l.app, 4, 8*time.Second); err != nil {
		return fmt.Errorf("模块未确认进入飞行模式: %w", err)
	}
	return nil
}

func (l *moduleRadioLease) restore() error {
	if l == nil || l.app == nil {
		return nil
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if !l.active {
		return nil
	}
	preferences, preferenceErr := l.app.readModuleFlightModePreferences()
	l.restoreMode = moduleRadioRestoreMode(l.restoreMode, preferences, preferenceErr)
	var lastErr error
	for attempt := 1; attempt <= 2; attempt++ {
		_, commandErr := l.app.runATCommand(fmt.Sprintf("AT+CFUN=%d", l.restoreMode), 12*time.Second)
		if commandErr != nil {
			if current, checkErr := readModuleCFUN(l.app); checkErr == nil && current == l.restoreMode {
				l.active = false
				l.lastMode = current
				l.lastModeAt = time.Now()
				return nil
			}
			lastErr = fmt.Errorf("恢复模块原 CFUN=%d: %w", l.restoreMode, commandErr)
		} else if err := waitModuleCFUN(l.app, l.restoreMode, 8*time.Second); err == nil {
			l.active = false
			l.lastMode = l.restoreMode
			l.lastModeAt = time.Now()
			return nil
		} else {
			lastErr = fmt.Errorf("模块未确认恢复原 CFUN=%d: %w", l.restoreMode, err)
		}
		if attempt < 2 {
			time.Sleep(300 * time.Millisecond)
		}
	}
	return lastErr
}

func moduleRadioRestoreMode(previous int, preferences moduleFlightModePreferences, preferenceErr error) int {
	// A stale lease must never enable cellular service over an explicit or
	// default airplane-mode policy. An unreadable policy also fails closed.
	if preferenceErr != nil || defaultModuleFlightModeShouldRun(preferences) || previous < 0 || previous > 4 {
		return 4
	}
	return previous
}

// observedMode throttles the UI's repeated CFUN checks and serializes the
// occasional physical check with the module SIM's multi-command AKA exchange.
func (l *moduleRadioLease) observedMode(sim *moduleVoWiFiSIM, maxAge time.Duration) (int, error) {
	if l == nil || l.app == nil {
		return -1, errors.New("DJI 无线状态租约不可用")
	}
	l.mu.Lock()
	if l.lastModeAt.IsZero() {
		l.mu.Unlock()
		return readModuleCFUNForSIM(l.app, sim)
	}
	mode, observedAt, active := l.lastMode, l.lastModeAt, l.active
	l.mu.Unlock()
	if active && mode == 4 && time.Since(observedAt) < maxAge {
		return mode, nil
	}
	mode, err := readModuleCFUNForSIM(l.app, sim)
	if err != nil {
		return -1, err
	}
	l.mu.Lock()
	l.lastMode = mode
	l.lastModeAt = time.Now()
	l.mu.Unlock()
	return mode, nil
}

func readModuleCFUN(a *app) (int, error) {
	if a == nil {
		return -1, errors.New("DJI 模块未连接")
	}
	response, err := a.runATCommand("AT+CFUN?", 4*time.Second)
	if err != nil {
		return -1, fmt.Errorf("读取模块 CFUN: %w", err)
	}
	match := moduleCFUNResponse.FindStringSubmatch(response)
	if len(match) != 2 {
		return -1, fmt.Errorf("无法解析模块 CFUN 状态: %q", response)
	}
	mode, err := strconv.Atoi(match[1])
	if err != nil || mode < 0 || mode > 4 {
		return -1, fmt.Errorf("模块返回无效 CFUN 模式 %q", match[1])
	}
	return mode, nil
}

func readModuleCFUNForSIM(a *app, sim *moduleVoWiFiSIM) (int, error) {
	if sim == nil {
		return readModuleCFUN(a)
	}
	sim.mu.Lock()
	defer sim.mu.Unlock()
	return readModuleCFUN(a)
}

func waitModuleCFUN(a *app, expected int, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		mode, err := readModuleCFUN(a)
		if err == nil && mode == expected {
			return nil
		}
		time.Sleep(250 * time.Millisecond)
	}
	mode, err := readModuleCFUN(a)
	if err != nil {
		return err
	}
	return fmt.Errorf("current CFUN=%d, expected %d", mode, expected)
}

func lockModuleSIMContext(ctx context.Context, mu *sync.Mutex) error {
	if mu == nil {
		return errors.New("模块 SIM 通道锁不可用")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	ticker := time.NewTicker(25 * time.Millisecond)
	defer ticker.Stop()
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		if mu.TryLock() {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

func openModuleVoWiFiSIM(ctx context.Context, a *app) (*moduleVoWiFiSIM, error) {
	if a == nil {
		return nil, errors.New("DJI 模块未连接")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := a.ensureUSBAT(); err != nil {
		return nil, fmt.Errorf("连接 DJI AT 通道: %w", err)
	}
	// Reserve the AT channel for the full card-discovery transaction. SIM
	// SELECT/GET RESPONSE APDUs cannot safely interleave with status, SMS or
	// call-mode probes on this single USB serial endpoint.
	if err := lockModuleSIMContext(ctx, &a.moduleSIMMu); err != nil {
		return nil, fmt.Errorf("等待 DJI SIM/APDU 通道: %w", err)
	}
	defer a.moduleSIMMu.Unlock()
	numberResponse, _ := a.runSIMATCommand("AT+CNUM", 3*time.Second)
	transmitter := &moduleATAPDUTransmitter{command: a.runSIMATCommand}
	reader, err := newModuleUSIMReader(transmitter)
	if err != nil {
		return nil, fmt.Errorf("创建 DJI SIM APDU 通道: %w", err)
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	card, err := usim.New(ctx, reader, logger)
	if err != nil {
		return nil, fmt.Errorf("读取 DJI USIM/ISIM 信息: %w", err)
	}
	defer card.Close()
	selectors := readUSIMCarrierSelectors(ctx, reader)
	serviceCenter := card.ServiceCenter()
	smscResponse, _ := a.runSIMATCommand("AT+CSCA?", 3*time.Second)
	smsc, smscTarget := resolveModuleServiceCenter(card.SMSC(), serviceCenter.Target(), smscResponse)
	info := &usimInfo{
		ICCID:           strings.TrimSpace(card.ICCID()),
		IMSI:            strings.TrimSpace(card.IMSI()),
		PhoneNumber:     strings.TrimSpace(parseUSBATCNUM(numberResponse)),
		OperatorName:    selectors.operatorName,
		OperatorSource:  selectors.operatorSource,
		MCC:             strings.TrimSpace(card.MCC()),
		MNC:             strings.TrimSpace(card.MNC()),
		GID1:            strings.TrimSpace(card.GID1()),
		GID2:            selectors.gid2,
		ApplicationAID:  hex.EncodeToString(selectors.applicationAID),
		SMSC:            smsc,
		SMSCTarget:      smscTarget,
		PrivateIdentity: strings.TrimSpace(card.PrivateIdentity()),
		PublicIdentity:  strings.TrimSpace(card.PublicIdentity()),
		HomeDomain:      strings.TrimSpace(card.HomeDomain()),
		LoadedAt:        time.Now(),
	}
	if isMissingPhoneNumber(info.PhoneNumber) {
		info.PhoneNumber = selectors.phoneNumber
	}
	if isMissingPhoneNumber(info.PhoneNumber) {
		info.PhoneNumber = a.profilePhoneForICCID(info.ICCID)
	}
	if info.IMSI == "" || info.MCC == "" || info.MNC == "" {
		return nil, errors.New("DJI 模块 SIM 未提供可用于运营商配置的 IMSI/MCC/MNC")
	}
	log.Printf("[Module VoWiFi] SIM SMS route metadata: SMSC present=%t, IMS SMS target present=%t",
		strings.TrimSpace(info.SMSC) != "", strings.TrimSpace(info.SMSCTarget) != "")
	return &moduleVoWiFiSIM{app: a, info: info, atCommand: a.runSIMATCommand}, nil
}

// resolveModuleServiceCenter prefers values stored in the active UICC
// application. AT+CSCA? is a standards-based fallback for the SMSC number;
// it is never treated as an IMS PSI or copied from a previously active SIM.
func resolveModuleServiceCenter(cardAddress, cardTarget, atResponse string) (address, target string) {
	address = normalizeModuleSMSC(cardAddress)
	if address == "" {
		address = parseModuleCSCA(atResponse)
	}
	target = strings.TrimSpace(cardTarget)
	if target == "" && address != "" {
		target = "tel:" + address
	}
	return address, target
}

var moduleCSCAResponse = regexp.MustCompile(`(?im)^\s*\+CSCA:\s*"([^"]*)"\s*,?\s*(?:\d+)?\s*$`)

func parseModuleCSCA(response string) string {
	match := moduleCSCAResponse.FindStringSubmatch(response)
	if len(match) != 2 {
		return ""
	}
	return normalizeModuleSMSC(match[1])
}

func normalizeModuleSMSC(value string) string {
	value = strings.TrimSpace(strings.Trim(value, `"`))
	var digits strings.Builder
	prefix := false
	for _, r := range value {
		switch {
		case r >= '0' && r <= '9':
			digits.WriteRune(r)
		case r == '+' && digits.Len() == 0 && !prefix:
			prefix = true
		case r == ' ' || r == '\t' || r == '-' || r == '(' || r == ')' || r == '.':
			// Ignore common phone-number formatting characters.
		default:
			return ""
		}
	}
	if digits.Len() < 5 || digits.Len() > 15 || strings.Trim(digits.String(), "0") == "" {
		return ""
	}
	if prefix {
		return "+" + digits.String()
	}
	return digits.String()
}

func (s *moduleVoWiFiSIM) authenticate(ctx context.Context, application string, rand, autn []byte) (*akaResult, error) {
	if s == nil || s.app == nil || s.info == nil {
		return nil, errors.New("DJI SIM AKA 通道未初始化")
	}
	if len(rand) != 16 || len(autn) != 16 {
		return nil, errors.New("RAND 和 AUTN 必须为 16 字节")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := lockModuleSIMContext(ctx, &s.app.moduleSIMMu); err != nil {
		return nil, fmt.Errorf("等待 DJI SIM/APDU 通道: %w", err)
	}
	defer s.app.moduleSIMMu.Unlock()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := s.validateProfileLocked(ctx); err != nil {
		return nil, err
	}

	transmitter := &moduleATAPDUTransmitter{command: s.runATCommand}
	reader, err := newModuleUSIMReader(transmitter)
	if err != nil {
		return nil, err
	}
	applications, err := reader.ListApplications(ctx)
	if err != nil {
		return nil, fmt.Errorf("读取 DJI SIM 应用目录: %w", err)
	}
	var prefix []byte
	switch application {
	case "usim":
		prefix = usimApplicationAIDPrefix
	case "isim":
		prefix = isimApplicationAIDPrefix
	default:
		return nil, fmt.Errorf("不支持的 SIM 鉴权应用 %q", application)
	}
	aid := findApplicationAID(applications, prefix)
	if len(aid) == 0 {
		if application == "isim" {
			return nil, errReaderISIMUnavailable
		}
		return nil, errors.New("DJI SIM 中未找到 USIM 应用")
	}
	payload, err := reader.Authenticate3G(ctx, usimcard.AuthenticateRequest{
		AID: aid, Rand: append([]byte(nil), rand...), AUTN: append([]byte(nil), autn...), EAPAKA: true,
	})
	if err != nil {
		return nil, fmt.Errorf("DJI %s AKA APDU: %w", strings.ToUpper(application), err)
	}
	var parsed usimcommand.Authenticate3GResult
	if err := parsed.UnmarshalBinary(payload); err != nil {
		return nil, fmt.Errorf("解析 DJI %s AKA 响应: %w", strings.ToUpper(application), err)
	}
	result := &akaResult{
		RES:  append([]byte(nil), parsed.RES...),
		CK:   append([]byte(nil), parsed.CK...),
		IK:   append([]byte(nil), parsed.IK...),
		AUTS: append([]byte(nil), parsed.AUTS...),
	}
	switch {
	case parsed.IsAuthenticationReject():
		return nil, errReaderAKARejected
	case parsed.IsSynchronizationFailure():
		if len(parsed.AUTS) != 14 {
			return nil, fmt.Errorf("DJI SIM AKA AUTS 长度无效: %d", len(parsed.AUTS))
		}
		return result, errReaderAKASyncFailure
	case !parsed.IsSuccess():
		return nil, errors.New("DJI SIM AKA 返回不完整")
	default:
		return result, nil
	}
}

func (s *moduleVoWiFiSIM) validateProfile(ctx context.Context) error {
	if s == nil {
		return errors.New("DJI SIM 会话不可用")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.app != nil {
		if err := lockModuleSIMContext(ctx, &s.app.moduleSIMMu); err != nil {
			return fmt.Errorf("等待 DJI SIM/APDU 通道: %w", err)
		}
		defer s.app.moduleSIMMu.Unlock()
	}
	return s.validateProfileLocked(ctx)
}

// validateProfileLocked must be called while holding s.mu. Profile checks
// share the module's single AT/APDU channel with AKA; an unlocked QCCID/CIMI
// pair can otherwise interleave with SELECT/GET RESPONSE/AUTHENTICATE APDUs.
func (s *moduleVoWiFiSIM) validateProfileLocked(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if s.atCommand == nil {
		if s.app == nil {
			return errors.New("DJI SIM AT 通道不可用")
		}
		if err := s.app.ensureUSBAT(); err != nil {
			return err
		}
	}
	iccidResponse, iccidErr := s.runATCommand("AT+QCCID", 4*time.Second)
	imsiResponse, imsiErr := s.runATCommand("AT+CIMI", 4*time.Second)
	if iccidErr != nil || imsiErr != nil {
		return fmt.Errorf("验证 DJI 当前 SIM Profile: ICCID=%v IMSI=%v", iccidErr, imsiErr)
	}
	iccid := normalizeSIMIdentifier(parseUSBATPrefixed(iccidResponse, "+QCCID:"))
	imsi := normalizeSIMIdentifier(parseUSBATIMSI(imsiResponse))
	if iccid != normalizeSIMIdentifier(s.info.ICCID) || imsi != normalizeSIMIdentifier(s.info.IMSI) {
		return errors.New("DJI eSIM Profile 已切换，已拒绝继续使用旧 VoWiFi 身份；请重新启动 Wi-Fi Calling")
	}
	return nil
}

func (s *moduleVoWiFiSIM) runATCommand(command string, timeout time.Duration) (string, error) {
	if s != nil && s.atCommand != nil {
		return s.atCommand(command, timeout)
	}
	if s == nil || s.app == nil {
		return "", errors.New("DJI SIM AT 通道不可用")
	}
	return s.app.runSIMATCommand(command, timeout)
}

// moduleATAPDUTransmitter sends SIM commands over the module's USB AT bridge.
// It remains usable in CFUN=4: the RF stack is disabled while the SIM interface
// stays available for ePDG EAP-AKA.
type moduleATAPDUTransmitter struct {
	command func(string, time.Duration) (string, error)
}

func (t *moduleATAPDUTransmitter) Transmit(ctx context.Context, apdu []byte) ([]byte, error) {
	if t == nil || t.command == nil {
		return nil, errors.New("DJI AT/APDU transport unavailable")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if len(apdu) == 0 || len(apdu) > 255 {
		return nil, fmt.Errorf("SIM APDU length out of range: %d", len(apdu))
	}
	encoded := strings.ToUpper(hex.EncodeToString(apdu))
	var lastErr error
	for _, length := range []int{len(encoded), len(apdu)} {
		response, err := t.command(fmt.Sprintf(`AT+CSIM=%d,"%s"`, length, encoded), 12*time.Second)
		if err != nil {
			lastErr = err
			continue
		}
		match := moduleCSIMResponse.FindStringSubmatch(response)
		if len(match) != 2 {
			lastErr = fmt.Errorf("invalid AT+CSIM response")
			continue
		}
		decoded, err := hex.DecodeString(match[1])
		if err != nil {
			lastErr = fmt.Errorf("decode AT+CSIM response: %w", err)
			continue
		}
		return decoded, nil
	}
	return nil, fmt.Errorf("AT+CSIM failed: %w", lastErr)
}

func (t *moduleATAPDUTransmitter) Close() error { return nil }

var moduleCSIMResponse = regexp.MustCompile(`(?i)\+CSIM:\s*\d+\s*,\s*"?([0-9a-f]+)"?`)
var moduleCFUNResponse = regexp.MustCompile(`(?m)\+CFUN:\s*(\d+)`)
