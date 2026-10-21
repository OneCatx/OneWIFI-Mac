package swu

import (
	"crypto/sha1"
	"testing"
	"time"

	"github.com/1239t/swu-go/pkg/eap"
	"github.com/1239t/swu-go/pkg/ikev2"
	"go.uber.org/zap"
)

type effectiveConfigSIM struct{}

func (*effectiveConfigSIM) GetIMSI() (string, error) { return "310260123456789", nil }
func (*effectiveConfigSIM) GetIMEI() (string, error) { return "351234567890124", nil }
func (*effectiveConfigSIM) CalculateAKA(rand, autn []byte) ([]byte, []byte, []byte, []byte, error) {
	return nil, nil, nil, nil, nil
}
func (*effectiveConfigSIM) Close() error { return nil }

func TestEffectiveConfigDefaultsAndValidation(t *testing.T) {
	cfg := &Config{}
	ipv4, ipv6, err := cfg.requestedAddressFamilies()
	if err != nil || !ipv4 || !ipv6 {
		t.Fatalf("default families=%t/%t err=%v", ipv4, ipv6, err)
	}
	if cfg.effectiveNATTKeepaliveInterval() != 20*time.Second || cfg.effectiveDPDInterval() != 120*time.Second {
		t.Fatalf("defaults natt=%v dpd=%v", cfg.effectiveNATTKeepaliveInterval(), cfg.effectiveDPDInterval())
	}
	cfg.IPStack = "bad"
	if err := cfg.validate(); err == nil {
		t.Fatal("invalid IP stack accepted")
	}
	cfg.IPStack = "ipv4"
	cfg.IKEProposals = []string{"unknown"}
	if err := cfg.validate(); err == nil {
		t.Fatal("invalid IKE proposal accepted")
	}
}

func TestDisableDPDDoesNotChangePublishedInterval(t *testing.T) {
	cfg := &Config{DisableDPD: true, DPDIntervalSeconds: 600}
	if got := cfg.effectiveDPDInterval(); got != 600*time.Second {
		t.Fatalf("effective interval = %v, want 600s", got)
	}
}

func TestBuildIKEAuthInitPayloadsUsesIPStackProposalsAndDeviceGate(t *testing.T) {
	disabled := false
	session := NewSession(&Config{
		SIM: &effectiveConfigSIM{}, APN: "ims", IPStack: "ipv4",
		ESPProposals: []string{"aes256-sha256"}, DeviceIdentityEnabled: &disabled,
	}, zap.NewNop())
	payloads, err := session.buildIKEAuthInitPayloads()
	if err != nil {
		t.Fatal(err)
	}
	var cp *ikev2.EncryptedPayloadCP
	var childSA *ikev2.EncryptedPayloadSA
	var selectors []*ikev2.EncryptedPayloadTS
	for _, payload := range payloads {
		switch value := payload.(type) {
		case *ikev2.EncryptedPayloadCP:
			cp = value
		case *ikev2.EncryptedPayloadSA:
			childSA = value
		case *ikev2.EncryptedPayloadTS:
			selectors = append(selectors, value)
		case *ikev2.EncryptedPayloadNotify:
			if value.NotifyType == ikev2.DEVICE_IDENTITY || value.NotifyType == ikev2.DEVICE_IDENTITY_3GPP {
				t.Fatal("device identity notify present while disabled")
			}
		}
	}
	if cp == nil || len(cp.Attributes) != 3 {
		t.Fatalf("IPv4 CP attributes=%#v", cp)
	}
	for _, attribute := range cp.Attributes {
		if attribute.Type == ikev2.INTERNAL_IP6_ADDRESS || attribute.Type == ikev2.P_CSCF_IP6_ADDRESS {
			t.Fatalf("IPv6 attribute leaked into IPv4 config: %d", attribute.Type)
		}
	}
	if childSA == nil || len(childSA.Proposals) != 1 {
		t.Fatalf("configured ESP list not authoritative: %#v", childSA)
	}
	if len(selectors) != 2 || len(selectors[0].TrafficSelectors) != 1 || selectors[0].TrafficSelectors[0].TSType != ikev2.TS_IPV4_ADDR_RANGE {
		t.Fatalf("traffic selectors=%#v", selectors)
	}
}

func TestAKACheckcodeModes(t *testing.T) {
	transcript := []byte("identity-request-and-response")
	digest := sha1.Sum(transcript)

	minimal := &Session{cfg: &Config{AKAChallengeMode: "minimal"}, eapIdentityCheckcode: digest, eapIdentityCheckcodeValid: true}
	if got, err := minimal.eapAKACheckcodeResponse(map[uint8]*eap.Attribute{}); err != nil || got != nil {
		t.Fatalf("minimal got=%#v err=%v", got, err)
	}

	check := &Session{cfg: &Config{AKAChallengeMode: "checkcode"}, eapIdentityCheckcode: digest, eapIdentityCheckcodeValid: true}
	attrs := map[uint8]*eap.Attribute{eap.AT_CHECKCODE: {Type: eap.AT_CHECKCODE, Value: digest[:]}}
	got, err := check.eapAKACheckcodeResponse(attrs)
	if err != nil || got == nil || len(got.Value) != sha1.Size {
		t.Fatalf("checkcode got=%#v err=%v", got, err)
	}
	attrs[eap.AT_CHECKCODE].Value[0] ^= 0xff
	if _, err := check.eapAKACheckcodeResponse(attrs); err == nil {
		t.Fatal("mismatched checkcode accepted")
	}

	recompute := &Session{cfg: &Config{AKAChallengeMode: "recompute"}, eapIdentityCheckcode: sha1.Sum(transcript), eapIdentityCheckcodeValid: true}
	if got, err := recompute.eapAKACheckcodeResponse(map[uint8]*eap.Attribute{}); err != nil || got == nil {
		t.Fatalf("recompute got=%#v err=%v", got, err)
	}
}
