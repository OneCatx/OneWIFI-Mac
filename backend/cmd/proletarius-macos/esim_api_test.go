package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/damonto/euicc-go/driver"
	"proletarius.local/core/internal/esim"
)

func TestESIMOperationLifecycle(t *testing.T) {
	a := &app{}
	operation, started := a.beginESIMOperation("download_profile", "准备下载", "")
	if !started || operation == nil || operation.State != esimOperationStateQueued {
		t.Fatalf("begin operation = %#v started=%v", operation, started)
	}
	if _, secondStarted := a.beginESIMOperation("switch_profile", "切换", "8986"); secondStarted {
		t.Fatal("second operation started while first is active")
	}

	a.updateESIMOperation(operation.ID, esimOperationStateRun, "install", "正在写入", 80)
	running := a.currentESIMOperation()
	if running == nil || running.State != esimOperationStateRun || running.Step != "install" || running.Progress != 80 {
		t.Fatalf("running operation = %#v", running)
	}

	a.finishESIMOperation(operation.ID, "完成", &esimOperationResult{Warning: "通知待确认", WarningCode: "notify_pending"}, 2)
	finished := a.currentESIMOperation()
	if finished == nil || finished.State != esimOperationStateWarn || finished.FinishedAt == nil || finished.RefreshAfterSeconds != 2 {
		t.Fatalf("finished operation = %#v", finished)
	}
	if _, nextStarted := a.beginESIMOperation("switch_profile", "切换", "8986"); !nextStarted {
		t.Fatal("new operation did not start after terminal state")
	}
}

func TestCurrentESIMOperationReturnsDeepCopy(t *testing.T) {
	a := &app{}
	operation, _ := a.beginESIMOperation("download_profile", "准备下载", "")
	a.finishESIMOperation(operation.ID, "完成", &esimOperationResult{
		SpaceDelta: &esim.SpaceDelta{Direction: esim.SpaceDeltaDirectionConsumed, Bytes: 42},
	}, 0)

	copy1 := a.currentESIMOperation()
	copy1.Message = "mutated"
	copy1.Result.SpaceDelta.Bytes = 99
	copy2 := a.currentESIMOperation()
	if copy2.Message == "mutated" || copy2.Result.SpaceDelta.Bytes != 42 {
		t.Fatalf("operation store leaked mutable pointer: %#v", copy2)
	}
}

func TestModuleProfileICCIDMatchesNormalizesPadding(t *testing.T) {
	if !moduleProfileICCIDMatches("  8931440012345678901F\r\n", "8931440012345678901") {
		t.Fatal("matching eUICC profile ICCID with trailing F padding was rejected")
	}
	if moduleProfileICCIDMatches("8931440012345678902", "8931440012345678901") {
		t.Fatal("different profile ICCID was accepted")
	}
	if moduleProfileICCIDMatches("", "8931440012345678901") {
		t.Fatal("empty module ICCID was accepted")
	}
}

func TestESIMOverviewHasActiveProfileUsesExactNormalizedICCID(t *testing.T) {
	overview := &esim.EsimOverview{Profiles: []esim.EUICCProfiles{{
		Profiles: []esim.ProfileItem{
			{ICCID: "8986001234567890123", State: 0},
			{ICCID: "8931440012345678901F", State: 1},
		},
	}}}
	if !esimOverviewHasActiveProfile(overview, "8931440012345678901") {
		t.Fatal("active profile with trailing F padding was not recognized")
	}
	if esimOverviewHasActiveProfile(overview, "8986001234567890123") {
		t.Fatal("disabled profile was treated as active")
	}
	if esimOverviewHasActiveProfile(overview, "") || esimOverviewHasActiveProfile(nil, "8931440012345678901") {
		t.Fatal("empty target or overview was treated as active")
	}
}

func TestWriteCodedError(t *testing.T) {
	recorder := httptest.NewRecorder()
	writeCodedError(recorder, http.StatusConflict, "esim_operation_busy", "正在进行", true)
	if recorder.Code != http.StatusConflict {
		t.Fatalf("status=%d", recorder.Code)
	}
	var payload codedErrorPayload
	if err := json.Unmarshal(recorder.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	if payload.Code != "esim_operation_busy" || payload.Error != "正在进行" || !payload.Recoverable {
		t.Fatalf("payload=%#v", payload)
	}
}

func TestRedactTextRemovesICCIDFromReaderDiagnostics(t *testing.T) {
	const iccid = "8931440012345678901"
	got := redactText("switch failed for profile "+iccid, iccid)
	if strings.Contains(got, iccid) || !strings.Contains(got, "[redacted]") {
		t.Fatalf("sensitive profile identity was not redacted: %q", got)
	}
}

func TestClassifyDownloadOperationErrorBusy(t *testing.T) {
	code, message, recoverable := classifyDownloadOperationError(errors.Join(errors.New("wrapped"), esim.ErrOperationInProgress))
	if code != "esim_operation_busy" || message == "" || !recoverable {
		t.Fatalf("classification=%q %q %v", code, message, recoverable)
	}
}

func TestClassifyDownloadOperationErrorDoesNotExposeRawActivationData(t *testing.T) {
	const secret = "MATCHING-ID-SECRET"
	err := esim.NewDownloadProfileError(errors.New("request failed for " + secret))
	code, message, recoverable := classifyDownloadOperationError(err)
	if code != esim.DownloadErrorGeneric || !recoverable {
		t.Fatalf("classification=%q %q %v", code, message, recoverable)
	}
	if strings.Contains(message, secret) {
		t.Fatalf("message exposed activation data: %q", message)
	}
}

func TestApplyDownloadStageToMessageAppendsStageForGenericFailures(t *testing.T) {
	code, message, _ := classifyDownloadOperationError(esim.NewDownloadProfileError(errors.New("server refused")))
	composed := applyDownloadStageToMessage(code, message, "auth_client")
	if !strings.Contains(composed, "身份认证") {
		t.Fatalf("composed=%q", composed)
	}
	if !strings.Contains(composed, message) {
		t.Fatalf("composed lost original message: %q", composed)
	}
}

func TestApplyDownloadStageToMessageKeepsSpecificErrorsUntouched(t *testing.T) {
	composed := applyDownloadStageToMessage(esim.DownloadErrorEUICCInsufficientMemory, "eUICC 安装 profile 时空间不足，请删除未使用的 profile 后重试", "install")
	if composed != "eUICC 安装 profile 时空间不足，请删除未使用的 profile 后重试" {
		t.Fatalf("specific message was modified: %q", composed)
	}
}

func TestApplyDownloadStageToMessageUnknownStep(t *testing.T) {
	composed := applyDownloadStageToMessage("download_failed", "下载 Profile 失败，请检查网络和激活码后重试", "")
	if composed != "下载 Profile 失败，请检查网络和激活码后重试" {
		t.Fatalf("composed=%q", composed)
	}
}

func TestEsimDownloadFailureDetailRedactsActivationData(t *testing.T) {
	const matchingID = "RQP4I-J4FEF-X7GMW-LMXYX"
	const confirmationCode = "1234-CONF"
	err := esim.NewDownloadProfileError(errors.New("server refused " + matchingID + " with code " + confirmationCode))
	detail := esimDownloadFailureDetail("auth_client", err, matchingID, confirmationCode)
	if strings.Contains(detail, matchingID) || strings.Contains(detail, confirmationCode) {
		t.Fatalf("detail leaked activation data: %q", detail)
	}
	if !strings.Contains(detail, "stage=auth_client") || !strings.Contains(detail, esim.DownloadErrorGeneric) {
		t.Fatalf("detail missing stage or code: %q", detail)
	}
}

func TestESIMDownloadDiagnosticIncludesFailureReasonAndRedactsSecrets(t *testing.T) {
	const matchingID = "RQP4I-J4FEF-X7GMW-LMXYX"
	const confirmationCode = "1234-CONF"
	err := esim.NewDownloadProfileError(errors.New("SM-DP+ rejected EID as disabled; matching=" + matchingID + "; confirm=" + confirmationCode))
	diagnostic := esimDownloadDiagnostic("auth_client", err, matchingID, confirmationCode)
	if !strings.Contains(diagnostic, "SM-DP+ 客户端身份认证阶段失败") {
		t.Fatalf("diagnostic missing stage: %q", diagnostic)
	}
	if !strings.Contains(diagnostic, "EID as disabled") {
		t.Fatalf("diagnostic lost provider reason: %q", diagnostic)
	}
	if strings.Contains(diagnostic, matchingID) || strings.Contains(diagnostic, confirmationCode) {
		t.Fatalf("diagnostic leaked activation secret: %q", diagnostic)
	}
}

func TestESIMDownloadDiagnosticIdentifiesEUICCAuthenticateServerAPDU(t *testing.T) {
	diagnostic := esimDownloadDiagnostic("auth_euicc", errors.New("returned an unexpected response with status 6982"))
	if !strings.Contains(diagnostic, "eUICC 验证 SM-DP+ 服务器签名阶段失败") || !strings.Contains(diagnostic, "6982") {
		t.Fatalf("diagnostic=%q", diagnostic)
	}
}

func TestFailedESIMOperationRetainsDownloadStageAndDiagnostic(t *testing.T) {
	a := &app{}
	operation, started := a.beginESIMOperation("download_profile", "正在准备下载", "")
	if !started {
		t.Fatal("download operation did not start")
	}
	diagnostic := "阶段：eUICC 验证 SM-DP+ 服务器签名阶段失败；底层返回：6982"
	a.failESIMOperationDetailed(operation.ID, "download_failed", "下载失败", true, "auth_euicc", diagnostic)
	failed := a.currentESIMOperation()
	if failed == nil || failed.State != esimOperationStateFailed {
		t.Fatalf("failed operation=%#v", failed)
	}
	if failed.Step != "auth_euicc" || failed.Diagnostic != diagnostic {
		t.Fatalf("failed operation lost context: step=%q diagnostic=%q", failed.Step, failed.Diagnostic)
	}
}

func TestESIMDownloadConfirmationPublishesEstimateAndResumes(t *testing.T) {
	a := &app{}
	operation, started := a.beginESIMOperation("download_profile", "正在准备下载", "")
	if !started {
		t.Fatal("download operation did not start")
	}
	if a.installESIMDownloadConfirmation(operation.ID) == nil {
		t.Fatal("confirmation channel was not installed")
	}

	result := make(chan bool, 1)
	go func() {
		result <- a.awaitESIMDownloadConfirmation(context.Background(), operation.ID, 39008)
	}()
	deadline := time.Now().Add(time.Second)
	for {
		current := a.currentESIMOperation()
		if current != nil && current.State == esimOperationStateAwaitingConfirmation {
			if current.EstimatedProfileSizeBytes == nil || *current.EstimatedProfileSizeBytes != 39008 {
				t.Fatalf("size estimate=%v want 39008", current.EstimatedProfileSizeBytes)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("operation did not enter confirmation state: %#v", current)
		}
		time.Sleep(time.Millisecond)
	}

	updated, accepted := a.respondESIMDownloadConfirmation(operation.ID, true)
	if !accepted || updated.State != esimOperationStateRun {
		t.Fatalf("confirmation result=%#v accepted=%v", updated, accepted)
	}
	select {
	case confirmed := <-result:
		if !confirmed {
			t.Fatal("download callback received cancel")
		}
	case <-time.After(time.Second):
		t.Fatal("download callback did not resume")
	}
	a.clearESIMDownloadConfirmation(operation.ID)
}

func TestESIMDownloadConfirmationCanBeCancelled(t *testing.T) {
	a := &app{}
	operation, started := a.beginESIMOperation("download_profile", "正在准备下载", "")
	if !started || a.installESIMDownloadConfirmation(operation.ID) == nil {
		t.Fatal("download confirmation could not start")
	}
	result := make(chan bool, 1)
	go func() { result <- a.awaitESIMDownloadConfirmation(context.Background(), operation.ID, 0) }()
	deadline := time.Now().Add(time.Second)
	for {
		current := a.currentESIMOperation()
		if current != nil && current.State == esimOperationStateAwaitingConfirmation {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("operation did not enter confirmation state: %#v", current)
		}
		time.Sleep(time.Millisecond)
	}
	if _, accepted := a.respondESIMDownloadConfirmation(operation.ID, false); !accepted {
		t.Fatal("cancel confirmation was rejected")
	}
	select {
	case confirmed := <-result:
		if confirmed {
			t.Fatal("download callback received confirm")
		}
	case <-time.After(time.Second):
		t.Fatal("download callback did not receive cancel")
	}
	a.cancelESIMOperation(operation.ID, "已取消下载")
	if current := a.currentESIMOperation(); current == nil || current.State != esimOperationStateCancelled {
		t.Fatalf("cancelled operation=%#v", current)
	}
}

func TestEsimDownloadFailureDetailPlainError(t *testing.T) {
	detail := esimDownloadFailureDetail("preflight", errors.New("无效的 SM-DP+ 地址"), "RQP4I-J4FEF-X7GMW-LMXYX")
	if !strings.Contains(detail, "stage=preflight") || !strings.Contains(detail, "无效的 SM-DP+ 地址") {
		t.Fatalf("detail=%q", detail)
	}
}

func TestEsimDownloadFailureDetailNilError(t *testing.T) {
	if detail := esimDownloadFailureDetail("auth_client", nil); detail != "" {
		t.Fatalf("nil error should produce empty detail, got %q", detail)
	}
}

func TestPhysicalSIMClassificationRequiresATRejection(t *testing.T) {
	if !isPhysicalSIMESIMProbeError(fmt.Errorf("%w: AT+CCHO=... ERROR", esim.ErrEUICCNotFound)) {
		t.Fatal("AT AID rejection should be classified as a physical SIM")
	}
	if isPhysicalSIMESIMProbeError(esim.ErrEUICCNotFound) {
		t.Fatal("bare discovery failure must not be classified as a physical SIM")
	}
	if isPhysicalSIMESIMProbeError(fmt.Errorf("%w: QMI transport timeout", esim.ErrEUICCNotFound)) {
		t.Fatal("transport failure must not be classified as a physical SIM")
	}
}

func TestPhonebookStorageSupported(t *testing.T) {
	response := `+CPBS: ("SM","ME","ON")`
	if !phonebookStorageSupported(response, "ME") {
		t.Fatal("ME storage was not detected")
	}
	if phonebookStorageSupported(response, "FD") {
		t.Fatal("unsupported FD storage was detected")
	}
}

func TestEncodeModuleProfileNoteCountsUnicodeCharacters(t *testing.T) {
	note := moduleProfileNote{ICCID: "8901000000000000000", Label: strings.Repeat("卡", 48)}
	if _, err := encodeModuleProfileNote(note); err != nil {
		t.Fatalf("48 Unicode characters should be accepted: %v", err)
	}
	note.Label += "卡"
	if _, err := encodeModuleProfileNote(note); err == nil {
		t.Fatal("49 Unicode characters should be rejected")
	}
}

func TestSaveESIMNoteCountsUnicodeCharacters(t *testing.T) {
	a := &app{profileNotesPath: t.TempDir() + "/notes.json"}

	save := func(label string) *httptest.ResponseRecorder {
		body, err := json.Marshal(map[string]string{
			"iccid": "8901000000000000000",
			"label": label,
			"phone": "",
			"tags":  "",
		})
		if err != nil {
			t.Fatal(err)
		}
		recorder := httptest.NewRecorder()
		a.saveESIMNote(recorder, httptest.NewRequest(http.MethodPut, "/api/esim/notes", strings.NewReader(string(body))))
		return recorder
	}

	if recorder := save(strings.Repeat("卡", 80)); recorder.Code != http.StatusOK {
		t.Fatalf("80 Unicode characters status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	if recorder := save(strings.Repeat("卡", 81)); recorder.Code != http.StatusBadRequest {
		t.Fatalf("81 Unicode characters status=%d body=%s", recorder.Code, recorder.Body.String())
	}
}

func TestModuleESIMOverviewDoesNotProbeSIMWhileVoWiFiOwnsChannel(t *testing.T) {
	channelFactoryCalled := false
	manager, err := esim.NewManager(esim.ManagerOptions{
		DeviceID: "module-test", Transport: "custom",
		SmartCardChannelFactory: func() (driver.SmartCardChannel, error) {
			channelFactoryCalled = true
			return newUSBATESIMChannel(func(string, time.Duration) (string, error) { return "", nil }), nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	a := &app{esim: manager, esimSwitchAllowed: true}
	previousActive, previousStarting, previousRetiring := activeSession, startingVoWiFi, retiringSession
	vowifiSessionMu.Lock()
	activeSession, retiringSession = nil, nil
	startingVoWiFi = &vowifiStartup{sourceKind: "module"}
	vowifiSessionMu.Unlock()
	t.Cleanup(func() {
		vowifiSessionMu.Lock()
		activeSession, startingVoWiFi, retiringSession = previousActive, previousStarting, previousRetiring
		vowifiSessionMu.Unlock()
	})

	response := httptest.NewRecorder()
	a.esimOverview(response, httptest.NewRequest(http.MethodGet, "/api/esim?refresh=true", nil))
	if response.Code != http.StatusConflict || !strings.Contains(response.Body.String(), "module_vowifi_owns_sim_channel") {
		t.Fatalf("overview during module VoWiFi status=%d body=%s", response.Code, response.Body.String())
	}
	if channelFactoryCalled {
		t.Fatal("eSIM endpoint touched the SIM/APDU channel while VoWiFi owned it")
	}
}
