package imscore

import (
	"context"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/emiago/sipgo/sip"
	"proletarius.local/core/internal/vowifi/policy"
)

func TestSMSOnlyRegistrationDoesNotAdvertiseVoice(t *testing.T) {
	for _, template := range []policy.IMSRegisterTemplate{policy.DefaultIMSRegisterTemplate(), policy.DefaultGiffgaffTemplate(), policy.OrangeUKTemplate()} {
		cfg := Config{LocalIP: net.ParseIP("192.0.2.10"), HomeDomain: "ims.example.test", PublicURI: "sip:test@ims.example.test", PrivateID: "test@ims.example.test", Template: template}
		for _, smsOnly := range []bool{false, true} {
			cfg.SMSOnly = smsOnly
			req, err := buildRegisterRequest(cfg, registerState{sipInstance: "urn:uuid:test"}, 5060, false, initialRegisterVariant{})
			if err != nil {
				t.Fatal(err)
			}
			wire := req.String()
			if !strings.Contains(wire, "+g.3gpp.smsip") {
				t.Fatal("SMS capability lost")
			}
			if smsOnly {
				for _, forbidden := range []string{";audio", "mmtel", "mid-call", "srvcc", "INVITE"} {
					if strings.Contains(wire, forbidden) {
						t.Fatalf("SMS-only request advertises %q", forbidden)
					}
				}
			} else if !strings.Contains(wire, "mmtel") {
				t.Fatal("legacy voice capability lost")
			}
		}
	}
}

func TestSMSOnlyRejectsInviteBeforeCustomHandler(t *testing.T) {
	rt := &transportRuntime{cfg: Config{SMSOnly: true}, incomingSIP: func(*sip.Request, SIPResponder) { t.Fatal("SMS-only invoked voice callback") }}
	conn := &payloadAndEOFConn{}
	req := sip.NewRequest(sip.INVITE, sip.Uri{Host: "ims.example.test"})
	rt.dispatchIncomingRequest(conn, req, false, nil)
	if !strings.Contains(conn.writes.String(), "488 Not Acceptable Here") {
		t.Fatalf("response=%s", conn.writes.String())
	}
	conn.writes.Reset()
	rt.dispatchIncomingRequest(conn, sip.NewRequest(sip.OPTIONS, sip.Uri{Host: "ims.example.test"}), false, nil)
	if !strings.Contains(conn.writes.String(), "200 OK") {
		t.Fatal("SMS-only lost OPTIONS liveness")
	}
}

func TestSMSOnlyRuntimeHasNoVoiceAndFencesRawSMSAfterClose(t *testing.T) {
	svc := serviceStateTestService(0)
	svc.cfg.SMSOnly = true
	if newCallRuntime(svc) != nil {
		t.Fatal("SMS-only allocated voice runtime")
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	svc.lifecycleCtx, svc.lifecycleCancel = ctx, cancel
	persisted := 0
	svc.cfg.IncomingRawSMS = func(IncomingSMSEvent) error { persisted++; return nil }
	ipsecPolicy, transport := serviceStateTestTransport(t)
	client, peer := net.Pipe()
	defer peer.Close()
	if err := svc.activateRegisteredRuntime(ctx, 0, &serviceStateTestDialer{}, &registerResult{
		pcscfAddr: "192.0.2.20:5060", secureConn: client, transport: transport,
		ipsecPolicy: ipsecPolicy, portSListener: newServiceStateTrackingListener(),
	}, "192.0.2.20:5060"); err != nil {
		t.Fatal(err)
	}
	if svc.calls != nil || svc.CallReady() {
		t.Fatal("SMS-only became voice ready")
	}
	raw := svc.transportRuntime.cfg.IncomingRawSMS
	if err := raw(IncomingSMSEvent{}); err != nil {
		t.Fatal(err)
	}
	closeCtx, closeCancel := context.WithTimeout(context.Background(), time.Second)
	defer closeCancel()
	if err := svc.Close(closeCtx); err != nil {
		t.Fatal(err)
	}
	if err := raw(IncomingSMSEvent{}); err == nil {
		t.Fatal("stale runtime accepted SMS after Close")
	}
	if persisted != 1 {
		t.Fatalf("persistence calls=%d", persisted)
	}
}

func TestSMSOnlyConfigPropagation(t *testing.T) {
	if !internalConfigFromIMS(IMSConfig{SMSOnly: true}, StartSessionInput{}).SMSOnly ||
		!internalConfigFromIMS(IMSConfig{}, StartSessionInput{SMSOnly: true}).SMSOnly {
		t.Fatal("SMS-only configuration dropped")
	}
	if internalConfigFromIMS(IMSConfig{}, StartSessionInput{}).SMSOnly {
		t.Fatal("legacy default changed")
	}
}
