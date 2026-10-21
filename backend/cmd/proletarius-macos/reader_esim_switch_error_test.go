package main

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	sgp22 "github.com/damonto/euicc-go/v2"
)

func TestReaderProfileSwitchFailurePreservesCardResultWithoutIdentity(t *testing.T) {
	const privateID = "8986001234567890123"
	for _, tc := range []struct {
		result sgp22.ProfileOperationResult
		code   string
	}{
		{sgp22.ProfileOperationResultUndefinedError, "profile_switch_card_rejected"},
		{sgp22.ProfileOperationResultCATBusy, "profile_switch_card_busy"},
		{sgp22.ProfileOperationResultICCIDOrAIDNotFound, "profile_switch_target_missing"},
		{sgp22.ProfileOperationResultProfileNotInDisabledState, "profile_switch_state_unconfirmed"},
		{sgp22.ProfileOperationResultDisallowedByPolicy, "profile_switch_policy_denied"},
		{sgp22.ProfileOperationResultWrongProfileReenabling, "profile_switch_rejected"},
		{sgp22.ProfileOperationResult(6), "profile_switch_rejected"},
	} {
		t.Run(tc.code, func(t *testing.T) {
			cardErr := &sgp22.ProfileOperationError{Operation: sgp22.EnableProfile, Result: tc.result}
			err := fmt.Errorf("启用 profile %s 失败: %w", privateID, cardErr)
			code, message := readerProfileSwitchFailure(err)
			if code != tc.code || !strings.Contains(message, cardErr.ResultName()) || !strings.Contains(message, fmt.Sprintf("结果码 %d", tc.result)) {
				t.Fatalf("missing structured error: %s %s", code, message)
			}
			if strings.Contains(message, privateID) || strings.Contains(message, "切换完成") || strings.Contains(message, "已受理") {
				t.Fatalf("identity leak or false activation: %s", message)
			}
		})
	}
}

func TestReaderProfileSwitchFailureDoesNotExposeRawTransportError(t *testing.T) {
	for _, err := range []error{errors.New("transport error for 8986001234567890123"),
		&sgp22.ProfileOperationError{Operation: sgp22.DeleteProfile, Result: sgp22.ProfileOperationResultUndefinedError}} {
		code, message := readerProfileSwitchFailure(err)
		if code != "profile_switch_failed" || strings.Contains(message, "898600") || strings.Contains(message, "deleteProfile") || !strings.Contains(message, "不要连续重复") {
			t.Fatalf("unsafe fallback: %s %s", code, message)
		}
	}
}
