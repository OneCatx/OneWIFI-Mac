package main

import (
	"context"
	"fmt"
	"net"
	"strconv"
	"strings"

	"github.com/1239t/swu-go/pkg/ikev2"

	"proletarius.local/core/internal/vowifi"
	"proletarius.local/core/internal/vowifi/carrier"
	"proletarius.local/core/internal/vowifi/policy"
)

const readerProfileSourceOverride = "carrier-override"

func carrierIdentityFromUSIM(info *usimInfo) carrier.SIMIdentity {
	if info == nil {
		return carrier.SIMIdentity{}
	}
	return carrier.SIMIdentity{
		MCC: info.MCC, MNC: info.MNC, IMSI: info.IMSI, ICCID: info.ICCID,
		GID1: info.GID1, GID2: info.GID2, SPN: info.selectorSPN(),
	}
}

// readerVoWiFiEffectiveConfig is a per-session immutable snapshot shared by
// module and PC/SC VoWiFi. Editing the YAML while connected affects only the
// next session.
type readerVoWiFiEffectiveConfig struct {
	Profile         *carrier.Profile
	OverrideMatched bool
	OverrideID      string

	EPDG       string
	EPDGPort   int
	EPDGSource string
	APN        string
	IPStack    string
	DNSServers []string // diagnostic only; never used to pre-resolve the ePDG

	NATTKeepaliveSeconds  int
	DPDIntervalSeconds    int
	DisableDPD            bool
	ReauthIntervalSeconds int
	AlgorithmPolicy       string
	EnableLegacyCiphers   bool
	IKEProposals          []string
	ESPProposals          []string
	AKAChallengeMode      string
	DeviceModel           string
	DeviceIdentityEnabled bool
	DeviceIdentityIMEI    string

	IMSDomain                     string
	IMSRealm                      string
	IMSPhoneContext               string
	IMSPCSCF                      string
	IMSTransport                  string
	IMSIdentitySource             string
	IMSLocalPort                  int
	IMSTCPKeepaliveSeconds        int
	IMSOptionsPingIntervalSeconds int
	IMSRegisterTemplate           policy.IMSRegisterTemplate

	E911Enabled            bool
	E911Provider           string
	E911EntitlementURL     string
	E911WebsheetHostPolicy string
}

// resolveSharedVoWiFiEffectiveConfig keeps carrier template selection and
// user overrides identical across the module and PC/SC reader transports.
// Hardware-specific SIM/APDU transport is resolved before this boundary; all
// ePDG, IKE/ESP, IMS, AKA policy and carrier overrides are shared here.
func resolveSharedVoWiFiEffectiveConfig(
	ctx context.Context,
	a *app,
	identity carrier.SIMIdentity,
	simInfo *vowifi.SIMInfo,
	requestEPDG string,
	requestEPDGPort int,
) (*readerVoWiFiEffectiveConfig, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if a == nil {
		return nil, fmt.Errorf("VoWiFi 配置上下文不可用")
	}
	profile, err := resolveRuntimeCarrierContext(ctx, identity)
	if err != nil {
		return nil, err
	}
	override, matched, err := a.voWiFiOverrides.match(identity.MCC, identity.MNC)
	if err != nil {
		return nil, err
	}
	return resolveReaderVoWiFiEffectiveConfig(
		profile, override, matched, simInfo, requestEPDG, requestEPDGPort, a.getDeviceIMEI())
}

func resolveReaderVoWiFiEffectiveConfig(
	base *carrier.Profile,
	override *carrier.CarrierOverride,
	overrideMatched bool,
	simInfo *vowifi.SIMInfo,
	requestEPDG string,
	requestEPDGPort int,
	generatedIMEI string,
) (*readerVoWiFiEffectiveConfig, error) {
	if base == nil || simInfo == nil {
		return nil, fmt.Errorf("VoWiFi 运营商或 SIM 配置不可用")
	}
	profile := cloneReaderCarrierProfile(base)
	domain := strings.TrimSpace(simInfo.HomeDomain)
	if domain == "" {
		domain = profile.IMSDomain()
	}
	apn := strings.TrimSpace(profile.APN)
	if apn == "" {
		apn = "ims"
	}
	ipStack := strings.ToLower(strings.TrimSpace(profile.IPStack))
	if ipStack == "" {
		ipStack = "ipv4v6"
	}
	natt := profile.NATTKeepalive
	if natt <= 0 {
		natt = 20
	}
	dpd := profile.DPDInterval
	if dpd <= 0 {
		dpd = 120
	}
	phoneContext := strings.TrimSpace(profile.CallPhoneContext)
	if phoneContext == "" {
		phoneContext = domain
	}

	effective := &readerVoWiFiEffectiveConfig{
		Profile:                profile,
		APN:                    apn,
		IPStack:                ipStack,
		NATTKeepaliveSeconds:   natt,
		DPDIntervalSeconds:     dpd,
		DisableDPD:             profile.DisableDPD,
		AlgorithmPolicy:        "balanced",
		AKAChallengeMode:       "minimal",
		DeviceIdentityEnabled:  true,
		DeviceIdentityIMEI:     strings.TrimSpace(generatedIMEI),
		IMSDomain:              domain,
		IMSRealm:               domain,
		IMSPhoneContext:        phoneContext,
		IMSTransport:           "auto",
		IMSIdentitySource:      "derived",
		IMSLocalPort:           5060,
		IMSTCPKeepaliveSeconds: 30,
		// A SIP response validates the complete IKE/ESP/IPsec/IMS path rather
		// than merely proving that a local timer is still running. Together
		// with the 12-second acknowledgement deadline this detects a stale
		// session within 42 seconds in the worst phase alignment.
		IMSOptionsPingIntervalSeconds: 30,
		IMSRegisterTemplate:           policy.TemplateForProfile(profile),
	}

	if overrideMatched && override != nil {
		effective.OverrideMatched = true
		applyReaderCarrierOverride(effective, override)
	}

	// Preserve the existing in-app custom ePDG controls as the highest-priority
	// source. They override YAML without changing the rest of the matched preset.
	requestEPDG = strings.TrimSpace(requestEPDG)
	switch {
	case requestEPDG != "":
		effective.EPDG = trimAddressBrackets(requestEPDG)
		effective.EPDGSource = "custom"
	case overrideMatched && override != nil && override.CustomEPDG != nil && strings.TrimSpace(*override.CustomEPDG) != "":
		effective.EPDG = trimAddressBrackets(*override.CustomEPDG)
		effective.EPDGSource = "carrier-override"
	default:
		effective.EPDG, effective.EPDGSource = profile.ResolveEPDG("")
		effective.EPDG = trimAddressBrackets(effective.EPDG)
	}
	effective.EPDGPort = 500
	if overrideMatched && override != nil && override.EPDGPort != nil && *override.EPDGPort > 0 {
		effective.EPDGPort = *override.EPDGPort
	}
	if requestEPDGPort > 0 {
		effective.EPDGPort = requestEPDGPort
	}

	if effective.EPDG == "" {
		return nil, fmt.Errorf("ePDG 地址为空")
	}
	if effective.EPDGPort < 1 || effective.EPDGPort > 65535 {
		return nil, fmt.Errorf("ePDG 端口无效: %d", effective.EPDGPort)
	}
	if effective.IKEProposals != nil && len(effective.IKEProposals) == 0 {
		return nil, fmt.Errorf("ike_proposals 已显式配置但列表为空")
	}
	if effective.ESPProposals != nil && len(effective.ESPProposals) == 0 {
		return nil, fmt.Errorf("esp_proposals 已显式配置但列表为空")
	}
	if overrideRequestsAlgorithmDefaults(override) {
		defaultIKE, defaultESP := readerAlgorithmProposals(effective)
		if effective.IKEProposals == nil {
			effective.IKEProposals = defaultIKE
		}
		if effective.ESPProposals == nil {
			effective.ESPProposals = defaultESP
		}
	} else if effective.IKEProposals == nil && effective.ESPProposals == nil {
		// A user-supplied proposal list is authoritative. When no proposal,
		// algorithm policy, or legacy-default request exists, prefer an exact
		// carrier template only if the embedded source proves that our runtime
		// can express its complete initial IKE/ESP transform set.
		if templateIKE, templateESP, ok := carrierTemplateProposals(profile); ok {
			effective.IKEProposals = templateIKE
			effective.ESPProposals = templateESP
		}
	}
	if _, err := ikev2.BuildIKEProposals(effective.IKEProposals, nil); err != nil {
		return nil, fmt.Errorf("IKE proposal 配置无效: %w", err)
	}
	if _, err := ikev2.BuildESPProposals(effective.ESPProposals, []byte{0, 0, 0, 1}); err != nil {
		return nil, fmt.Errorf("ESP proposal 配置无效: %w", err)
	}
	if effective.IMSPCSCF != "" {
		normalized, err := normalizeReaderPCSCF(effective.IMSPCSCF)
		if err != nil {
			return nil, err
		}
		effective.IMSPCSCF = normalized
	}
	if effective.DeviceIdentityEnabled {
		if err := validateReaderIMEI(effective.DeviceIdentityIMEI); err != nil {
			return nil, err
		}
	} else {
		effective.DeviceIdentityIMEI = ""
	}
	return effective, nil
}

func applyReaderCarrierOverride(effective *readerVoWiFiEffectiveConfig, override *carrier.CarrierOverride) {
	if override.ID != nil {
		effective.OverrideID = strings.TrimSpace(*override.ID)
		if effective.OverrideID != "" {
			effective.Profile.ID = effective.OverrideID
		}
	}
	effective.Profile.Source = readerProfileSourceOverride
	if override.APN != nil && strings.TrimSpace(*override.APN) != "" {
		effective.APN = strings.TrimSpace(*override.APN)
	}
	if override.IPStack != nil {
		effective.IPStack = *override.IPStack
	}
	if override.DNSServers != nil {
		effective.DNSServers = append([]string(nil), (*override.DNSServers)...)
	}
	if override.NATKeepaliveSeconds != nil {
		effective.NATTKeepaliveSeconds = *override.NATKeepaliveSeconds
	}
	if override.DPDIntervalSeconds != nil {
		effective.DPDIntervalSeconds = *override.DPDIntervalSeconds
		// An explicit custom interval is an explicit request to run DPD and
		// therefore overrides a built-in carrier's disabled flag.
		effective.DisableDPD = false
	}
	if override.ReauthIntervalSeconds != nil && *override.ReauthIntervalSeconds > 0 {
		effective.ReauthIntervalSeconds = *override.ReauthIntervalSeconds
	}
	if override.AlgorithmPolicy != nil {
		effective.AlgorithmPolicy = *override.AlgorithmPolicy
	}
	if override.EnableLegacyCiphers != nil {
		effective.EnableLegacyCiphers = *override.EnableLegacyCiphers
	}
	if override.IKEProposals != nil {
		effective.IKEProposals = append([]string(nil), (*override.IKEProposals)...)
		if len(*override.IKEProposals) == 0 {
			effective.IKEProposals = make([]string, 0)
		}
	}
	if override.ESPProposals != nil {
		effective.ESPProposals = append([]string(nil), (*override.ESPProposals)...)
		if len(*override.ESPProposals) == 0 {
			effective.ESPProposals = make([]string, 0)
		}
	}
	if override.AKAChallengeMode != nil {
		effective.AKAChallengeMode = *override.AKAChallengeMode
	}
	if override.DeviceModel != nil {
		effective.DeviceModel = strings.TrimSpace(*override.DeviceModel)
	}
	if override.DeviceIdentityEnabled != nil {
		effective.DeviceIdentityEnabled = *override.DeviceIdentityEnabled
	}
	if override.DeviceIdentityIMEI != nil && strings.TrimSpace(*override.DeviceIdentityIMEI) != "" {
		effective.DeviceIdentityIMEI = strings.TrimSpace(*override.DeviceIdentityIMEI)
	}
	if override.IMSDomain != nil && strings.TrimSpace(*override.IMSDomain) != "" {
		effective.IMSDomain = strings.TrimSpace(*override.IMSDomain)
	}
	if override.IMSRealm != nil && strings.TrimSpace(*override.IMSRealm) != "" {
		effective.IMSRealm = strings.TrimSpace(*override.IMSRealm)
	} else if override.IMSDomain != nil && strings.TrimSpace(*override.IMSDomain) != "" {
		effective.IMSRealm = effective.IMSDomain
	}
	if override.IMSPhoneContext != nil && strings.TrimSpace(*override.IMSPhoneContext) != "" {
		effective.IMSPhoneContext = strings.TrimSpace(*override.IMSPhoneContext)
	}
	if override.IMSPCSCF != nil && strings.TrimSpace(*override.IMSPCSCF) != "" {
		effective.IMSPCSCF = strings.TrimSpace(*override.IMSPCSCF)
	}
	if override.IMSTransport != nil {
		effective.IMSTransport = *override.IMSTransport
	}
	if override.IMSIdentitySource != nil {
		effective.IMSIdentitySource = *override.IMSIdentitySource
	}
	if override.IMSLocalPort != nil {
		effective.IMSLocalPort = *override.IMSLocalPort
	}
	if override.IMSTCPKeepaliveSeconds != nil {
		effective.IMSTCPKeepaliveSeconds = *override.IMSTCPKeepaliveSeconds
	}
	if override.IMSOptionsPingIntervalSeconds != nil {
		effective.IMSOptionsPingIntervalSeconds = *override.IMSOptionsPingIntervalSeconds
	}
	mergeReaderRegisterTemplate(&effective.IMSRegisterTemplate, override.IMSRegisterTemplate)
	if override.E911 != nil {
		if override.E911.Enabled != nil {
			effective.E911Enabled = *override.E911.Enabled
		}
		if override.E911.Provider != nil {
			effective.E911Provider = strings.TrimSpace(*override.E911.Provider)
		}
		if override.E911.EntitlementURL != nil {
			effective.E911EntitlementURL = strings.TrimSpace(*override.E911.EntitlementURL)
		}
		if override.E911.WebsheetHostPolicy != nil {
			effective.E911WebsheetHostPolicy = strings.TrimSpace(*override.E911.WebsheetHostPolicy)
		}
	}
}

func mergeReaderRegisterTemplate(target *policy.IMSRegisterTemplate, source *carrier.IMSRegisterTemplateOverride) {
	if target == nil || source == nil {
		return
	}
	if source.ID != nil {
		target.ID = *source.ID
	}
	if source.Expires != nil && *source.Expires > 0 {
		target.Expires = *source.Expires
	}
	if source.ContactMode != nil {
		target.ContactMode = *source.ContactMode
	}
	if source.SecAgreeMode != nil {
		target.SecAgreeMode = *source.SecAgreeMode
	}
	if source.SupportedHeader != nil {
		target.SupportedHeader = *source.SupportedHeader
	}
	if source.AccessType != nil {
		target.AccessType = *source.AccessType
	}
	if source.ContactParamOrder != nil {
		target.ContactParamOrder = append([]string(nil), (*source.ContactParamOrder)...)
		if len(*source.ContactParamOrder) == 0 {
			target.ContactParamOrder = make([]string, 0)
		}
	}
	if source.IncludePANIAuthenticated != nil {
		target.IncludePANIAuthenticated = *source.IncludePANIAuthenticated
	}
	if source.StrictSecurityServerOffer != nil {
		target.StrictSecurityServerOffer = *source.StrictSecurityServerOffer
	}
	if source.EnableInitialRejectFallback != nil {
		target.EnableInitialRejectFallback = *source.EnableInitialRejectFallback
	}
	if source.SecurityClientMechanisms != nil {
		target.SecurityClientMechanisms = make([]policy.IPSec3GPPSecurityMechanism, 0, len(*source.SecurityClientMechanisms))
		for _, mechanism := range *source.SecurityClientMechanisms {
			target.SecurityClientMechanisms = append(target.SecurityClientMechanisms, policy.IPSec3GPPSecurityMechanism{
				Alg: readerStringValue(mechanism.Alg), EAlg: readerStringValue(mechanism.EAlg),
				Prot: readerStringValue(mechanism.Prot), Mode: readerStringValue(mechanism.Mode),
			})
		}
	}
	if source.RegisterPolicy != nil {
		registerPolicy := source.RegisterPolicy
		if registerPolicy.TemporaryStatusCodes != nil {
			target.RegisterPolicy.TemporaryStatusCodes = cloneReaderStatusCodes(*registerPolicy.TemporaryStatusCodes)
		}
		if registerPolicy.ForbiddenStatusCodes != nil {
			target.RegisterPolicy.ForbiddenStatusCodes = cloneReaderStatusCodes(*registerPolicy.ForbiddenStatusCodes)
		}
		if registerPolicy.InitialRejectFallbackStatusCodes != nil {
			target.RegisterPolicy.InitialRejectFallbackStatusCodes = cloneReaderStatusCodes(*registerPolicy.InitialRejectFallbackStatusCodes)
		}
		if registerPolicy.TemporaryRetrySeconds != nil {
			target.RegisterPolicy.TemporaryRetrySeconds = *registerPolicy.TemporaryRetrySeconds
		}
	}
}

func resolveReaderIMSIdentities(effective *readerVoWiFiEffectiveConfig, simInfo *vowifi.SIMInfo) (impi, impu string, err error) {
	if effective == nil || simInfo == nil {
		return "", "", fmt.Errorf("IMS 身份配置不可用")
	}
	privateID := strings.TrimSpace(simInfo.PrivateIdentity)
	publicID := strings.TrimSpace(simInfo.PublicIdentity)
	switch effective.IMSIdentitySource {
	case "isim":
		if privateID == "" || publicID == "" {
			return "", "", fmt.Errorf("ims_identity_source=isim，但卡片没有完整 IMPI/IMPU")
		}
		return privateID, publicID, nil
	case "auto":
		if privateID != "" && publicID != "" {
			return privateID, publicID, nil
		}
		if privateID != "" || publicID != "" {
			return "", "", fmt.Errorf("ISIM 身份不完整：IMPI/IMPU 必须同时存在")
		}
	case "derived":
	default:
		return "", "", fmt.Errorf("不支持的 IMS 身份来源 %q", effective.IMSIdentitySource)
	}
	if strings.TrimSpace(simInfo.IMSI) == "" || effective.IMSDomain == "" || effective.IMSRealm == "" {
		return "", "", fmt.Errorf("无法从 IMSI/IMS 域推导 IMS 身份")
	}
	return fmt.Sprintf("%s@%s", simInfo.IMSI, effective.IMSRealm),
		fmt.Sprintf("sip:%s@%s", simInfo.IMSI, effective.IMSDomain), nil
}

func validateReaderVoWiFiStartCapabilities(effective *readerVoWiFiEffectiveConfig) error {
	if effective == nil {
		return fmt.Errorf("VoWiFi 配置不可用")
	}
	if strings.EqualFold(effective.IMSTransport, "udp") {
		return fmt.Errorf("ims_transport=udp 尚不受读卡器用户态 IMS 栈支持；请使用 auto 或 tcp")
	}
	if effective.E911Enabled {
		return fmt.Errorf("E911 provider %q 尚未实现，已拒绝伪装成启用状态", effective.E911Provider)
	}
	return nil
}

func normalizeReaderPCSCF(value string) (string, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return "", nil
	}
	if ip := net.ParseIP(strings.Trim(value, "[]")); ip != nil {
		return net.JoinHostPort(ip.String(), "5060"), nil
	}
	host, portText, err := net.SplitHostPort(value)
	if err != nil {
		return "", fmt.Errorf("ims_pcscf %q 必须是隧道内数字 IP，可选端口", value)
	}
	ip := net.ParseIP(strings.Trim(host, "[]"))
	if ip == nil {
		return "", fmt.Errorf("ims_pcscf %q 不能触发宿主机 DNS；请填写隧道内数字 IP 或留空采用 ePDG 下发值", value)
	}
	port, err := strconv.Atoi(portText)
	if err != nil || port < 1 || port > 65535 {
		return "", fmt.Errorf("ims_pcscf 端口无效: %q", portText)
	}
	return net.JoinHostPort(ip.String(), strconv.Itoa(port)), nil
}

func validateReaderIMEI(imei string) error {
	imei = strings.TrimSpace(imei)
	if len(imei) != 15 {
		return fmt.Errorf("device_identity_imei 必须是 15 位有效 IMEI")
	}
	for _, digit := range imei {
		if digit < '0' || digit > '9' {
			return fmt.Errorf("device_identity_imei 必须是 15 位有效 IMEI")
		}
	}
	if luhnChecksum(imei[:14]) != int(imei[14]-'0') {
		return fmt.Errorf("device_identity_imei 的 Luhn 校验位无效")
	}
	return nil
}

func readerAlgorithmProposals(effective *readerVoWiFiEffectiveConfig) (ike, esp []string) {
	modernIKE := []string{
		"aes256-sha512-prfsha512-modp2048",
		"aes256-sha256-prfsha256-modp2048",
		"aes128-sha256-prfsha256-modp2048",
		"aes256gcm16-prfsha384-modp2048",
		"aes128gcm16-prfsha256-modp2048",
	}
	modernESP := []string{"aes256-sha512", "aes256-sha256", "aes128-sha256", "aes256gcm16", "aes128gcm16"}
	legacyIKE := []string{"aes128-sha1-prfsha1-modp2048"}
	legacyESP := []string{"aes128-sha1"}
	if !effective.EnableLegacyCiphers {
		return modernIKE, modernESP
	}
	if effective.AlgorithmPolicy == "legacy_prefer" {
		return append(legacyIKE, modernIKE...), append(legacyESP, modernESP...)
	}
	return append(modernIKE, legacyIKE...), append(modernESP, legacyESP...)
}

// carrierTemplateProposals converts only complete, single-transform carrier
// templates that the current Go IKE runtime can represent without changing a
// DH group. It intentionally returns false for multi-transform templates,
// non-DH14 groups, AEAD suites, and hashes outside the implemented SHA-2 set;
// those profiles retain the pre-existing generic proposal behavior.
func carrierTemplateProposals(profile *carrier.Profile) (ike, esp []string, ok bool) {
	if profile == nil || !profile.TemplateIKEProposalExact || !profile.TemplateESPProposalExact {
		return nil, nil, false
	}
	if profile.IKEDHGroup != 14 {
		return nil, nil, false
	}

	ikeEncryption := strings.ToLower(strings.TrimSpace(profile.IKEEncryption))
	ikeIntegrity := strings.ToLower(strings.TrimSpace(profile.IKEIntegrity))
	ikePRF := strings.ToLower(strings.TrimSpace(profile.IKEPRF))
	espEncryption := strings.ToLower(strings.TrimSpace(profile.ChildEncryption))
	espIntegrity := strings.ToLower(strings.TrimSpace(profile.ChildIntegrity))
	if !isCarrierTemplateAESCBC(ikeEncryption) || !isCarrierTemplateAESCBC(espEncryption) ||
		!isCarrierTemplateSHA2(ikeIntegrity) || !isCarrierTemplateSHA2(espIntegrity) ||
		!isCarrierTemplateSHA2(ikePRF) {
		return nil, nil, false
	}

	return []string{fmt.Sprintf("%s-%s-prf%s-modp2048", ikeEncryption, ikeIntegrity, ikePRF)},
		[]string{fmt.Sprintf("%s-%s", espEncryption, espIntegrity)}, true
}

func isCarrierTemplateAESCBC(value string) bool {
	return value == "aes128" || value == "aes256"
}

func isCarrierTemplateSHA2(value string) bool {
	return value == "sha256" || value == "sha512"
}

func overrideRequestsAlgorithmDefaults(override *carrier.CarrierOverride) bool {
	return override != nil && (override.AlgorithmPolicy != nil || override.EnableLegacyCiphers != nil)
}

func cloneReaderCarrierProfile(source *carrier.Profile) *carrier.Profile {
	cloned := *source
	cloned.EPDGCandidates = append([]string(nil), source.EPDGCandidates...)
	cloned.PCSCF = append([]string(nil), source.PCSCF...)
	cloned.RegisterContactParams = append([]string(nil), source.RegisterContactParams...)
	return &cloned
}

func trimAddressBrackets(value string) string {
	value = strings.TrimSpace(value)
	if strings.HasPrefix(value, "[") && strings.HasSuffix(value, "]") {
		return strings.TrimSuffix(strings.TrimPrefix(value, "["), "]")
	}
	return value
}

func readerStringValue(value *string) string {
	if value == nil {
		return ""
	}
	return strings.TrimSpace(*value)
}

func cloneReaderStatusCodes(source []int) []int {
	if len(source) == 0 {
		return make([]int, 0)
	}
	return append([]int(nil), source...)
}
