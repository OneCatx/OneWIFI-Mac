package main

import (
	"net"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	swucore "github.com/1239t/swu-go/pkg/swu"

	"proletarius.local/core/internal/vowifi"
	"proletarius.local/core/internal/vowifi/carrier"
)

func TestReaderCarrierOverrideMergeAndRequestEPDGPrecedence(t *testing.T) {
	set, err := carrier.LoadOverrides([]byte(`
carrier_overrides:
  "310/260":
    id: tmobile_custom
    custom_epdg: override.example.net
    epdg_port: 4500
    apn: custom-ims
    ip_stack: ipv4
    dns_servers: ["8.8.8.8"]
    nat_keepalive_seconds: 25
    dpd_interval_seconds: 600
    reauth_interval_seconds: 3600
    algorithm_policy: balanced
    enable_legacy_ciphers: false
    ike_proposals: [aes256-sha256-prfsha1-modp2048, aes128-sha256-modp2048]
    esp_proposals: [aes256-sha256]
    aka_challenge_mode: checkcode
    device_model: iphone15,4
    device_identity_enabled: true
    device_identity_imei: "351234567890124"
    ims_domain: ims.example.net
    ims_realm: realm.example.net
    ims_transport: tcp
    ims_identity_source: auto
    ims_local_port: 5070
    ims_tcp_keepalive_seconds: 35
    ims_options_ping_interval_seconds: 45
    ims_register_template:
      id: custom_register
      expires: 600
      contact_mode: android_default
      sec_agree_mode: required
      supported_header: path,sec-agree,test
      access_type: wlan1
      contact_param_order: [sip_instance, access_type]
      include_pani_authenticated: false
      strict_security_server_offer: false
      enable_initial_reject_fallback: true
      security_client_mechanisms:
        - {alg: hmac-sha-1-96, ealg: aes-cbc, prot: esp, mode: trans}
      register_policy:
        temporary_status_codes: [429]
        forbidden_status_codes: [403]
        initial_reject_fallback_status_codes: [400]
        temporary_retry_seconds: 12
`))
	if err != nil {
		t.Fatal(err)
	}
	override, matched := set.Match("310", "260")
	base := carrier.FromSIM("310", "260")
	simInfo := &vowifi.SIMInfo{
		IMSI: "310260123456789", MCC: "310", MNC: "260",
		PrivateIdentity: "card-private@example.net", PublicIdentity: "sip:card-public@example.net",
	}
	effective, err := resolveReaderVoWiFiEffectiveConfig(
		base, override, matched, simInfo, "app-choice.example.net", 500, "351234567890124",
	)
	if err != nil {
		t.Fatal(err)
	}
	if effective.EPDG != "app-choice.example.net" || effective.EPDGPort != 500 || effective.EPDGSource != "custom" {
		t.Fatalf("ePDG=%s:%d source=%s", effective.EPDG, effective.EPDGPort, effective.EPDGSource)
	}
	if effective.Profile.ID != "tmobile_custom" || effective.Profile.Source != readerProfileSourceOverride {
		t.Fatalf("profile id/source=%s/%s", effective.Profile.ID, effective.Profile.Source)
	}
	if effective.APN != "custom-ims" || effective.IPStack != "ipv4" || effective.NATTKeepaliveSeconds != 25 || effective.DPDIntervalSeconds != 600 {
		t.Fatalf("unexpected tunnel config: %#v", effective)
	}
	if len(effective.DNSServers) != 1 || effective.DNSServers[0] != "8.8.8.8:53" {
		t.Fatalf("DNS metadata=%v", effective.DNSServers)
	}
	if effective.IMSRegisterTemplate.ID != "custom_register" || effective.IMSRegisterTemplate.Expires != 600 ||
		effective.IMSRegisterTemplate.SecAgreeMode != "required" || effective.IMSRegisterTemplate.AccessType != "wlan1" {
		t.Fatalf("register template=%#v", effective.IMSRegisterTemplate)
	}
	if len(effective.IMSRegisterTemplate.SecurityClientMechanisms) != 1 ||
		effective.IMSRegisterTemplate.RegisterPolicy.TemporaryRetrySeconds != 12 {
		t.Fatalf("register mechanisms/policy=%#v", effective.IMSRegisterTemplate)
	}
	impi, impu, err := resolveReaderIMSIdentities(effective, simInfo)
	if err != nil || impi != simInfo.PrivateIdentity || impu != simInfo.PublicIdentity {
		t.Fatalf("auto identities=%q/%q err=%v", impi, impu, err)
	}
}

func TestSharedVoWiFiResolverUsesSameCarrierPresetAndOverrides(t *testing.T) {
	t.Setenv("PROLETARIUS_CARRIER_SOCKET", "")
	path := filepath.Join(t.TempDir(), "carrier-overrides.yaml")
	if err := os.WriteFile(path, []byte(`
carrier_overrides:
  "234/33":
    custom_epdg: shared-epdg.example.net
    nat_keepalive_seconds: 27
`), 0o600); err != nil {
		t.Fatal(err)
	}
	appInstance := &app{voWiFiOverrides: newVoWiFiOverrideStore(path)}
	identity := carrier.SIMIdentity{
		MCC: "234", MNC: "33", IMSI: "234330123456789", ICCID: "8944301234567890123",
	}
	simInfo := &vowifi.SIMInfo{
		MCC: identity.MCC, MNC: identity.MNC, IMSI: identity.IMSI, ICCID: identity.ICCID,
		HomeDomain: "ims.mnc033.mcc234.3gppnetwork.org",
	}
	effective, err := resolveSharedVoWiFiEffectiveConfig(
		t.Context(), appInstance, identity, simInfo, "", 0)
	if err != nil {
		t.Fatal(err)
	}
	if effective.Profile.ID != "apple_orange_uk" || !effective.OverrideMatched {
		t.Fatalf("profile=%q override=%t; module and reader must resolve the same carrier bundle and override", effective.Profile.ID, effective.OverrideMatched)
	}
	if effective.EPDG != "shared-epdg.example.net" || effective.EPDGSource != "carrier-override" || effective.NATTKeepaliveSeconds != 27 {
		t.Fatalf("shared carrier override was not applied: ePDG=%s source=%s keepalive=%d", effective.EPDG, effective.EPDGSource, effective.NATTKeepaliveSeconds)
	}
	if len(effective.IKEProposals) != 1 || effective.IKEProposals[0] != "aes128-sha256-prfsha256-modp2048" {
		t.Fatalf("carrier-specific Orange/CTExcel IKE proposal was not preserved: %v", effective.IKEProposals)
	}
}

func TestSharedUSIMCarrierIdentityIncludesAPDUSelectors(t *testing.T) {
	got := carrierIdentityFromUSIM(&usimInfo{
		MCC: "234", MNC: "10", IMSI: "234100123456789", ICCID: "8944101234567890123",
		GID1: "508FFFFF00", GID2: "A1B2", OperatorName: "giffgaff", OperatorSource: "ef_spn",
	})
	if got.MCC != "234" || got.MNC != "10" || got.GID1 != "508FFFFF00" || got.GID2 != "A1B2" || got.SPN != "giffgaff" {
		t.Fatalf("USIM selector APDU fields lost before carrier resolution: %+v", got)
	}
}

func TestReaderOverrideEPDGBeatsPresetButNotAppChoice(t *testing.T) {
	set, err := carrier.LoadOverrides([]byte("carrier_overrides:\n  \"310/260\": {custom_epdg: override.example.net, epdg_port: 4500}\n"))
	if err != nil {
		t.Fatal(err)
	}
	override, matched := set.Match("310", "260")
	effective, err := resolveReaderVoWiFiEffectiveConfig(carrier.FromSIM("310", "260"), override, matched,
		&vowifi.SIMInfo{IMSI: "310260123456789", MCC: "310", MNC: "260"}, "", 0, "351234567890124")
	if err != nil {
		t.Fatal(err)
	}
	if effective.EPDG != "override.example.net" || effective.EPDGPort != 4500 || effective.EPDGSource != "carrier-override" {
		t.Fatalf("ePDG=%s:%d source=%s", effective.EPDG, effective.EPDGPort, effective.EPDGSource)
	}
}

func TestCTExcelEffectiveConfigUsesHostIMSPhoneContext(t *testing.T) {
	base := carrier.Resolve(carrier.SIMIdentity{MCC: "234", MNC: "33", IMSI: "234330123456789"})
	effective, err := resolveReaderVoWiFiEffectiveConfig(
		base, nil, false,
		&vowifi.SIMInfo{
			IMSI: "234330123456789", MCC: "234", MNC: "33",
			HomeDomain: "ims.mnc033.mcc234.3gppnetwork.org",
		},
		"", 0, "351234567890124",
	)
	if err != nil {
		t.Fatal(err)
	}
	if effective.IMSDomain != "ims.mnc033.mcc234.3gppnetwork.org" {
		t.Fatalf("subscriber IMS domain=%q", effective.IMSDomain)
	}
	if effective.IMSPhoneContext != "ims.mnc033.mcc234.3gppnetwork.org" {
		t.Fatalf("call phone-context=%q", effective.IMSPhoneContext)
	}
}

func TestReaderExactCarrierTemplateProposalsForCTExcelAndGiffgaff(t *testing.T) {
	tests := []struct {
		name     string
		identity carrier.SIMIdentity
		wantIKE  []string
		wantESP  []string
	}{
		{
			name:     "ctexcel",
			identity: carrier.SIMIdentity{MCC: "234", MNC: "33", IMSI: "234330123456789"},
			wantIKE:  []string{"aes128-sha256-prfsha256-modp2048"},
			wantESP:  []string{"aes128-sha256"},
		},
		{
			name:     "giffgaff",
			identity: carrier.SIMIdentity{MCC: "234", MNC: "10", IMSI: "234100123456789", GID1: "508FFFFF00"},
			wantIKE:  []string{"aes256-sha512-prfsha512-modp2048"},
			wantESP:  []string{"aes256-sha512"},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			base := carrier.Resolve(test.identity)
			effective, err := resolveReaderVoWiFiEffectiveConfig(base, nil, false,
				&vowifi.SIMInfo{IMSI: test.identity.IMSI, MCC: test.identity.MCC, MNC: test.identity.MNC},
				"", 0, "351234567890124")
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(effective.IKEProposals, test.wantIKE) || !reflect.DeepEqual(effective.ESPProposals, test.wantESP) {
				t.Fatalf("proposals IKE=%v ESP=%v, want IKE=%v ESP=%v", effective.IKEProposals, effective.ESPProposals, test.wantIKE, test.wantESP)
			}
		})
	}
}

func TestReaderCarrierProposalOverrideAndPolicyBeatTemplate(t *testing.T) {
	base := carrier.Resolve(carrier.SIMIdentity{MCC: "234", MNC: "33", IMSI: "234330123456789"})
	simInfo := &vowifi.SIMInfo{IMSI: "234330123456789", MCC: "234", MNC: "33"}

	t.Run("explicit proposals", func(t *testing.T) {
		set, err := carrier.LoadOverrides([]byte(`
carrier_overrides:
  "234/33":
    ike_proposals: [aes256-sha256-prfsha256-modp2048]
    esp_proposals: [aes256-sha256]
`))
		if err != nil {
			t.Fatal(err)
		}
		override, matched := set.Match("234", "33")
		effective, err := resolveReaderVoWiFiEffectiveConfig(base, override, matched, simInfo, "", 0, "351234567890124")
		if err != nil {
			t.Fatal(err)
		}
		if got, want := effective.IKEProposals, []string{"aes256-sha256-prfsha256-modp2048"}; !reflect.DeepEqual(got, want) {
			t.Fatalf("IKE=%v, want %v", got, want)
		}
		if got, want := effective.ESPProposals, []string{"aes256-sha256"}; !reflect.DeepEqual(got, want) {
			t.Fatalf("ESP=%v, want %v", got, want)
		}
	})

	t.Run("explicit policy", func(t *testing.T) {
		set, err := carrier.LoadOverrides([]byte("carrier_overrides:\n  \"234/33\": {algorithm_policy: strict}\n"))
		if err != nil {
			t.Fatal(err)
		}
		override, matched := set.Match("234", "33")
		effective, err := resolveReaderVoWiFiEffectiveConfig(base, override, matched, simInfo, "", 0, "351234567890124")
		if err != nil {
			t.Fatal(err)
		}
		wantIKE, wantESP := readerAlgorithmProposals(effective)
		if !reflect.DeepEqual(effective.IKEProposals, wantIKE) || !reflect.DeepEqual(effective.ESPProposals, wantESP) {
			t.Fatalf("policy should retain generic defaults, got IKE=%v ESP=%v", effective.IKEProposals, effective.ESPProposals)
		}
	})
}

func TestReaderUnsupportedCarrierTemplateDoesNotMapDHTo14(t *testing.T) {
	base := carrier.Resolve(carrier.SIMIdentity{MCC: "262", MNC: "01", IMSI: "262010123456789"})
	if base.ID != "apple_tmobile_germany" || base.IKEDHGroup != 2 || !base.TemplateIKEProposalExact || !base.TemplateESPProposalExact {
		t.Fatalf("unexpected unsupported template: %+v", base)
	}
	if ike, esp, ok := carrierTemplateProposals(base); ok || ike != nil || esp != nil {
		t.Fatalf("unsupported DH template was mapped: IKE=%v ESP=%v", ike, esp)
	}
	effective, err := resolveReaderVoWiFiEffectiveConfig(base, nil, false,
		&vowifi.SIMInfo{IMSI: "262010123456789", MCC: "262", MNC: "01"}, "", 0, "351234567890124")
	if err != nil {
		t.Fatal(err)
	}
	if effective.IKEProposals != nil || effective.ESPProposals != nil {
		t.Fatalf("unsupported DH template changed configured proposals: IKE=%v ESP=%v", effective.IKEProposals, effective.ESPProposals)
	}
}

func TestReaderIMSIdentityModes(t *testing.T) {
	effective := &readerVoWiFiEffectiveConfig{IMSDomain: "ims.example.net", IMSRealm: "realm.example.net", IMSIdentitySource: "derived"}
	simInfo := &vowifi.SIMInfo{IMSI: "310260123456789"}
	impi, impu, err := resolveReaderIMSIdentities(effective, simInfo)
	if err != nil || impi != "310260123456789@realm.example.net" || impu != "sip:310260123456789@ims.example.net" {
		t.Fatalf("derived=%q/%q err=%v", impi, impu, err)
	}
	effective.IMSIdentitySource = "isim"
	if _, _, err := resolveReaderIMSIdentities(effective, simInfo); err == nil {
		t.Fatal("strict ISIM accepted missing card identities")
	}
	effective.IMSIdentitySource = "auto"
	simInfo.PrivateIdentity = "private@example.net"
	if _, _, err := resolveReaderIMSIdentities(effective, simInfo); err == nil || !strings.Contains(err.Error(), "不完整") {
		t.Fatalf("partial ISIM error=%v", err)
	}
}

func TestReaderConfiguredPCSCFNeverUsesHostDNS(t *testing.T) {
	if _, err := normalizeReaderPCSCF("pcscf.example.net:5060"); err == nil || !strings.Contains(err.Error(), "不能触发宿主机 DNS") {
		t.Fatalf("hostname P-CSCF error=%v", err)
	}
	got, err := normalizeReaderPCSCF("2001:db8::10")
	if err != nil || got != "[2001:db8::10]:5060" {
		t.Fatalf("IPv6 P-CSCF=%q err=%v", got, err)
	}
}

func TestSelectIMSPathRespectsConfiguredStackAndPCSCF(t *testing.T) {
	snapshot := swucore.SessionSnapshot{
		IPv4: net.ParseIP("10.0.0.2"), IPv6: net.ParseIP("2001:db8::2"),
		PCSCFv4: []net.IP{net.ParseIP("10.0.0.10")}, PCSCFv6: []net.IP{net.ParseIP("2001:db8::10")},
	}
	local, candidates, err := selectIMSPathForConfig(snapshot, "ipv4", "")
	if err != nil || local.To4() == nil || len(candidates) != 1 || candidates[0] != "10.0.0.10:5060" {
		t.Fatalf("IPv4 selection local=%v candidates=%v err=%v", local, candidates, err)
	}
	local, candidates, err = selectIMSPathForConfig(snapshot, "ipv4v6", "10.0.0.99:5070")
	if err != nil || local.To4() == nil || candidates[0] != "10.0.0.99:5070" {
		t.Fatalf("configured selection local=%v candidates=%v err=%v", local, candidates, err)
	}
}

func TestReaderUnsupportedCapabilitiesFailClosed(t *testing.T) {
	if err := validateReaderVoWiFiStartCapabilities(&readerVoWiFiEffectiveConfig{IMSTransport: "udp"}); err == nil {
		t.Fatal("UDP was reported as supported")
	}
	if err := validateReaderVoWiFiStartCapabilities(&readerVoWiFiEffectiveConfig{IMSTransport: "tcp", E911Enabled: true, E911Provider: "att_entitlement"}); err == nil {
		t.Fatal("unsupported E911 was reported as active")
	}
}
