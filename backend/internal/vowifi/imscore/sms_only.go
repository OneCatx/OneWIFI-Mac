package imscore

import (
	"strings"

	"proletarius.local/core/internal/vowifi/policy"
)

// Filter a copy: carrier templates are shared with the preserved voice path.
func smsOnlyRegisterTemplate(template policy.IMSRegisterTemplate) policy.IMSRegisterTemplate {
	order := template.ContactParamOrder
	if order == nil {
		order = policy.DefaultIMSRegisterTemplate().ContactParamOrder
	}
	filtered := make([]string, 0, len(order)+1)
	hasSMS := false
	for _, parameter := range order {
		switch strings.ToLower(strings.TrimSpace(parameter)) {
		case "audio", "icsi_ref", "mid_call", "srvcc_alerting", "ps2cs_srvcc_orig_pre_alerting":
			continue
		case "smsip":
			hasSMS = true
		}
		filtered = append(filtered, parameter)
	}
	if !hasSMS {
		filtered = append(filtered, "smsip")
	}
	template.ContactParamOrder = filtered
	return template
}
