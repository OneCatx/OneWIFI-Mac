package imscore

import (
	"context"
	"net"
	"reflect"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"go.uber.org/zap/zapcore"

	"proletarius.local/core/internal/vowifi/ipsec3gpp"
	"proletarius.local/core/internal/vowifi/voiceclient"
)

type packetDiagnosticFixture struct {
	IMSNetwork // Socket methods must never be called by a diagnostic snapshot.
	count      atomic.Uint64
}

func (p *packetDiagnosticFixture) transportStats() ipsec3gpp.TransportStats {
	return ipsec3gpp.TransportStats{
		InboundPackets: p.count.Load() + 1, TransformErrors: 2,
		LastSourceIP: "private-source", LastDestinationIP: "private-destination",
		LastSourcePort: 1234, LastDestinationPort: 5678,
	}
}

func (p *packetDiagnosticFixture) IPSecDataplaneStats() ipsec3gpp.TransformingDataplaneStats {
	return ipsec3gpp.TransformingDataplaneStats{InboundPackets: p.count.Load() + 3, InboundDropped: 4}
}

func (p *packetDiagnosticFixture) SWUNetstackStats() voiceclient.SWUNetstackStats {
	return voiceclient.SWUNetstackStats{TCPValidReceived: p.count.Load() + 5, LastOutboundError: "private raw error"}
}

func packetDiagnosticMap(rt *transportRuntime) map[string]interface{} {
	encoder := zapcore.NewMapObjectEncoder()
	for _, field := range rt.packetDiagnosticFields() {
		field.AddTo(encoder)
	}
	return encoder.Fields
}

func TestPacketDiagnosticsAllowlistedCounterMapping(t *testing.T) {
	provider := &packetDiagnosticFixture{}
	provider.count.Store(10)
	sources := transportDiagnosticsForNetwork(nil, provider)
	sources.esp = provider.transportStats
	rt := &transportRuntime{diagnosticSources: sources}
	rt.incomingDiagnostics.listenerState(true)
	rt.incomingDiagnostics.accepted()
	want := map[string]interface{}{
		"esp_stats_available": true, "dataplane_stats_available": true, "netstack_stats_available": true,
		"port_s_listener_started": true, "port_s_accepts": uint64(1),
		"esp_inbound": uint64(11), "esp_transform_errors": uint64(2),
		"dataplane_inbound": uint64(13), "dataplane_inbound_dropped": uint64(4),
		"tcp_valid_received": uint64(15),
	}
	if got := packetDiagnosticMap(rt); !reflect.DeepEqual(got, want) {
		t.Fatalf("diagnostic fields = %#v, want only %#v", got, want)
	}
}

func TestPacketDiagnosticsMissingProvidersAreNotZero(t *testing.T) {
	for _, network := range []IMSNetwork{nil, &UserspaceIMSNetwork{}, (*UserspaceIMSNetwork)(nil)} {
		rt := &transportRuntime{diagnosticSources: transportDiagnosticsForNetwork(nil, network)}
		want := map[string]interface{}{
			"esp_stats_available": false, "dataplane_stats_available": false, "netstack_stats_available": false,
			"port_s_listener_started": false, "port_s_accepts": uint64(0),
		}
		if got := packetDiagnosticMap(rt); !reflect.DeepEqual(got, want) {
			t.Fatalf("unavailable diagnostic fields = %#v, want %#v", got, want)
		}
	}
	provider := &packetDiagnosticFixture{}
	rt := &transportRuntime{diagnosticSources: transportDiagnosticSources{dataplane: provider.IPSecDataplaneStats}}
	got := packetDiagnosticMap(rt)
	if got["dataplane_stats_available"] != true || got["esp_stats_available"] != false || got["netstack_stats_available"] != false {
		t.Fatal("one available layer incorrectly implied other providers exist")
	}
	if _, exists := got["tcp_valid_received"]; exists {
		t.Fatal("missing netstack counter was emitted")
	}
}

func TestPacketDiagnosticsConcurrentSnapshots(t *testing.T) {
	provider := &packetDiagnosticFixture{}
	sources := transportDiagnosticsForNetwork(nil, provider)
	sources.esp = provider.transportStats
	rt := &transportRuntime{diagnosticSources: sources}
	var group sync.WaitGroup
	for worker := 0; worker < 4; worker++ {
		group.Add(1)
		go func() {
			defer group.Done()
			for i := 0; i < 100; i++ {
				provider.count.Add(1)
				rt.incomingDiagnostics.accepted()
				_ = packetDiagnosticMap(rt)
			}
		}()
	}
	group.Wait()
	if got := packetDiagnosticMap(rt)["port_s_accepts"]; got != uint64(400) {
		t.Fatalf("accept counter = %v, want 400", got)
	}
}

func TestPacketDiagnosticsProductionLifecycleBindsProvider(t *testing.T) {
	svc := serviceStateTestService(0)
	provider := &packetDiagnosticFixture{}
	provider.count.Store(20)
	svc.network = provider
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	svc.lifecycleCtx = ctx
	svc.lifecycleCancel = cancel
	policy, transport := serviceStateTestTransport(t)
	client, peer := net.Pipe()
	defer peer.Close()
	listener := newServiceStateTrackingListener()
	if err := svc.activateRegisteredRuntime(ctx, 0, &serviceStateTestDialer{}, &registerResult{
		pcscfAddr: "192.0.2.20:5060", expiresSeconds: 600,
		secureConn: client, transport: transport, ipsecPolicy: policy, portSListener: listener,
	}, "192.0.2.20:5060"); err != nil {
		t.Fatal(err)
	}
	defer func() {
		closeCtx, closeCancel := context.WithTimeout(context.Background(), time.Second)
		defer closeCancel()
		if err := svc.Close(closeCtx); err != nil {
			t.Error(err)
		}
	}()
	got := packetDiagnosticMap(svc.runtimeSnapshot())
	if got["dataplane_stats_available"] != true || got["netstack_stats_available"] != true || got["esp_stats_available"] != true || got["dataplane_inbound"] != uint64(23) || got["tcp_valid_received"] != uint64(25) {
		t.Fatalf("production lifecycle did not bind diagnostic providers: %#v", got)
	}
	provider.count.Store(30)
	if got := packetDiagnosticMap(svc.runtimeSnapshot())["dataplane_inbound"]; got != uint64(33) {
		t.Fatalf("provider was copied as a stale initial value: %v", got)
	}
}
