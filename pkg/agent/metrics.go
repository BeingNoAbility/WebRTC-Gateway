package agent

import (
	"fmt"
	"strings"
	"sync/atomic"
)

// Metrics holds instance-scoped operational counters exported via /metrics.
type Metrics struct {
	ActiveSessions     atomic.Int64
	ActiveStreams      atomic.Int64
	AuthSuccessTotal   atomic.Uint64
	AuthFailureTotal   atomic.Uint64
	TotalStreamsServed atomic.Uint64
}

func (m *Metrics) ExportPrometheus(globalMemUsed int64) string {
	var b strings.Builder
	b.WriteString("# HELP webrtc_agent_active_sessions Currently active visitor WebRTC sessions\n")
	b.WriteString("# TYPE webrtc_agent_active_sessions gauge\n")
	b.WriteString(fmt.Sprintf("webrtc_agent_active_sessions %d\n\n", m.ActiveSessions.Load()))

	b.WriteString("# HELP webrtc_agent_active_streams Currently active proxied streams\n")
	b.WriteString("# TYPE webrtc_agent_active_streams gauge\n")
	b.WriteString(fmt.Sprintf("webrtc_agent_active_streams %d\n\n", m.ActiveStreams.Load()))

	b.WriteString("# HELP webrtc_agent_auth_success_total Total successful in-DTLS authentications\n")
	b.WriteString("# TYPE webrtc_agent_auth_success_total counter\n")
	b.WriteString(fmt.Sprintf("webrtc_agent_auth_success_total %d\n\n", m.AuthSuccessTotal.Load()))

	b.WriteString("# HELP webrtc_agent_auth_failure_total Total rejected authentications\n")
	b.WriteString("# TYPE webrtc_agent_auth_failure_total counter\n")
	b.WriteString(fmt.Sprintf("webrtc_agent_auth_failure_total %d\n\n", m.AuthFailureTotal.Load()))

	b.WriteString("# HELP webrtc_agent_streams_served_total Total proxied HTTP/WebSocket streams served\n")
	b.WriteString("# TYPE webrtc_agent_streams_served_total counter\n")
	b.WriteString(fmt.Sprintf("webrtc_agent_streams_served_total %d\n\n", m.TotalStreamsServed.Load()))

	b.WriteString("# HELP webrtc_agent_mux_inbound_bytes Process-global buffered inbound mux bytes\n")
	b.WriteString("# TYPE webrtc_agent_mux_inbound_bytes gauge\n")
	b.WriteString(fmt.Sprintf("webrtc_agent_mux_inbound_bytes %d\n\n", globalMemUsed))
	return b.String()
}
