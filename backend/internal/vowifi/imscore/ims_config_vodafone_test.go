package imscore

import (
	"context"
	"net"
	"testing"
	"time"
)

func TestVodafoneSMSOnlyDisablesOnlyOptionalOptionsProbe(t *testing.T) {
	for _, test := range []struct {
		name, carrier                   string
		configSMSOnly, inputSMSOnly     bool
		optionsSeconds, expectedOptions int
	}{
		{"new-vodafone", "apple_vodafone_uk", false, true, 30, 0},
		{"config-sms-only", "apple_vodafone_uk", true, false, 45, 0},
		{"already-disabled", "apple_vodafone_uk", false, true, 0, 0},
		{"legacy-vodafone", "apple_vodafone_uk", false, false, 30, 30},
		{"legacy-custom-interval", "apple_vodafone_uk", false, false, 45, 45},
		{"other-sms-only", "apple_ee_uk", false, true, 30, 30},
		{"unknown-sms-only", "", false, true, 30, 30},
	} {
		t.Run(test.name, func(t *testing.T) {
			cfg := internalConfigFromIMS(IMSConfig{
				CarrierPresetID: test.carrier, SMSOnly: test.configSMSOnly,
				OptionsPingIntervalSeconds: test.optionsSeconds,
				TCPKeepaliveSeconds:        30,
			}, StartSessionInput{SMSOnly: test.inputSMSOnly, RegisterExpirySeconds: 600})
			if cfg.OptionsPingIntervalSeconds != test.expectedOptions {
				t.Fatalf("OPTIONS interval=%d, want %d", cfg.OptionsPingIntervalSeconds, test.expectedOptions)
			}
			if cfg.TCPKeepaliveSeconds != 30 || cfg.RegisterExpirySeconds != 600 {
				t.Fatalf("TCP keepalive or registration lease changed: keepalive=%d expiry=%d", cfg.TCPKeepaliveSeconds, cfg.RegisterExpirySeconds)
			}
		})
	}
}

type vodafoneKeepaliveDialer struct {
	runtimeTestDialer
	conn      net.Conn
	keepalive time.Duration
}

func (d *vodafoneKeepaliveDialer) DialContextTCPWithKeepalive(_ context.Context, _ net.IP, _ int, _ net.IP, _ int, keepalive time.Duration) (net.Conn, error) {
	d.keepalive = keepalive
	return d.conn, nil
}

func TestVodafoneSMSOnlyRetainsProtectedTCPKeepaliveAndFailureSignal(t *testing.T) {
	cfg := internalConfigFromIMS(IMSConfig{
		CarrierPresetID: "apple_vodafone_uk",
		PCSCF:           "192.0.2.20:5060", Domain: "ims.example.test", IMPU: "sip:test@ims.example.test",
		TCPKeepaliveSeconds: 30, OptionsPingIntervalSeconds: 30,
	}, StartSessionInput{SMSOnly: true, LocalIP: net.ParseIP("192.0.2.10")})
	if cfg.OptionsPingIntervalSeconds != 0 {
		t.Fatal("Vodafone SMS-only OPTIONS probe was not disabled")
	}
	client, peer := net.Pipe()
	defer peer.Close()
	defer client.Close()
	dialer := &vodafoneKeepaliveDialer{conn: client}
	dialer.listener = newBlockingTCPListener()
	policy, transport := runtimeTestPolicy(t)
	conn, err := dialSecureRegisterConn(context.Background(), cfg, dialer, registerState{
		transport: transport, ipsecPolicy: policy,
	})
	if err != nil {
		t.Fatal(err)
	}
	if dialer.keepalive != 30*time.Second {
		t.Fatalf("protected TCP keepalive=%s, want 30s", dialer.keepalive)
	}
	rt, err := startTransportRuntime(context.Background(), cfg, dialer, policy, transport, conn)
	if err != nil {
		t.Fatal(err)
	}
	defer rt.Close()
	service := &Service{registered: true, ipsecInstalled: true, transportRuntime: rt}
	if !service.MessagingReady() {
		t.Fatal("authenticated SMS transport was not ready")
	}
	if err := peer.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-service.TransportFailure():
		if err == nil {
			t.Fatal("EOF produced a nil failure")
		}
	case <-time.After(time.Second):
		t.Fatal("disabling OPTIONS suppressed the transport failure signal")
	}
	if service.MessagingReady() {
		t.Fatal("disconnected SMS-only transport remained ready")
	}
}
