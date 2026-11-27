package signaling

import (
	"fmt"
	"strings"
)

// ExportServerMetrics renders instance-scoped Prometheus text metrics (zero package globals).
func (s *SignalServer) ExportMetrics() string {
	var b strings.Builder
	b.WriteString("# HELP webrtc_gateway_registered_nodes Number of currently registered agent nodes\n")
	b.WriteString("# TYPE webrtc_gateway_registered_nodes gauge\n")
	b.WriteString(fmt.Sprintf("webrtc_gateway_registered_nodes %d\n\n", s.Broker.AgentCount()))

	b.WriteString("# HELP webrtc_gateway_active_clients Number of currently connected visitor clients\n")
	b.WriteString("# TYPE webrtc_gateway_active_clients gauge\n")
	b.WriteString(fmt.Sprintf("webrtc_gateway_active_clients %d\n\n", s.Broker.ClientCount()))

	b.WriteString("# HELP webrtc_gateway_signaling_messages_total Total accepted signaling messages\n")
	b.WriteString("# TYPE webrtc_gateway_signaling_messages_total counter\n")
	b.WriteString(fmt.Sprintf("webrtc_gateway_signaling_messages_total %d\n\n", s.messagesTotal.Load()))

	b.WriteString("# HELP webrtc_gateway_ratelimit_rejected_total Total requests rejected by rate limiting\n")
	b.WriteString("# TYPE webrtc_gateway_ratelimit_rejected_total counter\n")
	b.WriteString(fmt.Sprintf("webrtc_gateway_ratelimit_rejected_total %d\n\n", s.Limiter.Rejections()))
	return b.String()
}
