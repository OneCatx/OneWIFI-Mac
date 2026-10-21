package main

import (
	"errors"
	"fmt"

	sgp22 "github.com/damonto/euicc-go/v2"
)

// Present the card's structured rejection without exposing the wrapping
// error, which may contain ICCID, AID or other persistent identifiers.
// None of these results proves activation or authorizes an automatic replay.
func readerProfileSwitchFailure(err error) (code, message string) {
	var result *sgp22.ProfileOperationError
	if !errors.As(err, &result) || result.Operation != sgp22.EnableProfile {
		return "profile_switch_failed", "切换 Profile 未完成；请刷新卡片状态确认当前启用的号卡，不要连续重复切卡。详细原因可在诊断日志查看"
	}
	detail := "卡片返回未识别的启用结果，请保留错误码用于诊断"
	code = "profile_switch_rejected"
	switch result.Result {
	case sgp22.ProfileOperationResultICCIDOrAIDNotFound:
		code, detail = "profile_switch_target_missing", "卡片未找到目标 Profile，请刷新列表后重新选择"
	case sgp22.ProfileOperationResultProfileNotInDisabledState:
		code, detail = "profile_switch_state_unconfirmed", "目标 Profile 不处于可启用状态，且尚未确认激活结果；请刷新卡片身份，不要重复切卡"
	case sgp22.ProfileOperationResultDisallowedByPolicy:
		code, detail = "profile_switch_policy_denied", "卡片策略不允许启用此 Profile，请核对号卡服务商的限制"
	case sgp22.ProfileOperationResultWrongProfileReenabling:
		detail = "卡片拒绝重新启用此 Profile，请刷新卡片状态并核对号卡限制"
	case sgp22.ProfileOperationResultCATBusy:
		code, detail = "profile_switch_card_busy", "卡片仍在处理上一项操作，请等待完成后刷新状态"
	case sgp22.ProfileOperationResultUndefinedError:
		code, detail = "profile_switch_card_rejected", "卡片拒绝启用 Profile，但未提供具体原因；请勿连续重复切卡，可结合诊断日志核对兼容性"
	}
	return code, fmt.Sprintf("%s（%s / %s，结果码 %d）", detail, result.Operation, result.ResultName(), result.Result)
}
