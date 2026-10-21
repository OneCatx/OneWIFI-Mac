package imscore

import (
	"go.uber.org/zap"

	"proletarius.local/core/internal/vowifi/ipsec3gpp"
	"proletarius.local/core/internal/vowifi/voiceclient"
)

// These read-only sources are bound before runtime goroutines start and never
// replaced. Snapshots contain aggregate counters, not packet or peer metadata.
// ESP here is the inner IMS ipsec-3gpp transport, not the outer SWu tunnel.
// Layers are sampled independently and include both Port-C and Port-S traffic;
// their differences are not packet-loss rates or proof of INVITE delivery.
type transportDiagnosticSources struct {
	esp       func() ipsec3gpp.TransportStats
	dataplane func() ipsec3gpp.TransformingDataplaneStats
	netstack  func() voiceclient.SWUNetstackStats
}

func transportDiagnosticsForNetwork(transport *ipsec3gpp.Transport, network IMSNetwork) transportDiagnosticSources {
	var sources transportDiagnosticSources
	if transport != nil {
		sources.esp = transport.Stats
	}
	// UserspaceIMSNetwork's legacy getters return zero for absent internals.
	// Preserve that distinction here instead of advertising missing counters.
	if userspace, ok := network.(*UserspaceIMSNetwork); ok {
		if userspace != nil {
			if userspace.packetTransforms != nil {
				sources.dataplane = userspace.packetTransforms.Stats
			}
			if provider, ok := userspace.swu.(voiceclient.SWUNetstackStatsProvider); ok {
				sources.netstack = provider.SWUNetstackStats
			}
		}
		return sources
	}
	if provider, ok := network.(interface {
		IPSecDataplaneStats() ipsec3gpp.TransformingDataplaneStats
	}); ok {
		sources.dataplane = provider.IPSecDataplaneStats
	}
	if provider, ok := network.(voiceclient.SWUNetstackStatsProvider); ok {
		sources.netstack = provider.SWUNetstackStats
	}
	return sources
}

func (rt *transportRuntime) packetDiagnosticFields() []zap.Field {
	sources := rt.diagnosticSources
	incoming := rt.incomingPortSDiagnostics()
	fields := []zap.Field{
		zap.Bool("esp_stats_available", sources.esp != nil),
		zap.Bool("dataplane_stats_available", sources.dataplane != nil),
		zap.Bool("netstack_stats_available", sources.netstack != nil),
		zap.Bool("port_s_listener_started", incoming.listenerStarted),
		zap.Uint64("port_s_accepts", incoming.acceptCount),
	}
	if sources.esp != nil {
		stats := sources.esp()
		fields = append(fields,
			zap.Uint64("esp_inbound", stats.InboundPackets),
			// This existing counter includes inbound AND outbound errors.
			zap.Uint64("esp_transform_errors", stats.TransformErrors))
	}
	if sources.dataplane != nil {
		stats := sources.dataplane()
		fields = append(fields,
			zap.Uint64("dataplane_inbound", stats.InboundPackets),
			zap.Uint64("dataplane_inbound_dropped", stats.InboundDropped))
	}
	if sources.netstack != nil {
		stats := sources.netstack()
		fields = append(fields, zap.Uint64("tcp_valid_received", stats.TCPValidReceived))
	}
	return fields
}
