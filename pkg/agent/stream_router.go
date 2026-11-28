package agent

import (
	"log"
	"time"

	"webrtc-gateway/pkg/mux"
	"webrtc-gateway/pkg/protocol"
)

// serveMuxStreams accepts authenticated streams from one visitor's Mux
// and dispatches each stream to HTTPProxy or WSProxy.
func (a *Agent) serveMuxStreams(sess *VisitorSession, m *mux.Mux) {
	for {
		stream, err := m.AcceptStream(a.rootCtx)
		if err != nil {
			return
		}
		a.metrics.ActiveStreams.Add(1)
		a.metrics.TotalStreamsServed.Add(1)

		go func(st *mux.Stream) {
			defer a.metrics.ActiveStreams.Add(-1)
			defer func() {
				if rec := recover(); rec != nil {
					log.Printf("[Agent] Recovered panic in stream handler: %v", rec)
					st.Reset(protocol.ErrCodeInternalError)
				}
			}()
			a.dispatchStream(sess, st)
		}(stream)
	}
}

func (a *Agent) dispatchStream(sess *VisitorSession, stream *mux.Stream) {
	if stream.Context().Err() != nil {
		_ = stream.Close()
		return
	}
	reqHdr, err := protocol.DecodeRequestHeaders(stream.HeadersRaw())
	if err != nil {
		stream.Reset(protocol.ErrCodeProtocolError)
		return
	}

	sess.mu.Lock()
	if sess.state != StateAuthenticated || !sess.authDone.Load() {
		sess.mu.Unlock()
		stream.Reset(protocol.ErrCodeAuthRequired)
		return
	}
	clientIP := sess.clientIP
	jar := sess.cookieJar
	capScope := sess.capScope
	capJTI := sess.capJTI
	capExp := sess.capExp
	sess.mu.Unlock()

	// Continuously enforce BOTH capability token expiry AND live revocation on every stream!
	if (capExp > 0 && time.Now().Unix() >= capExp) || (capJTI != "" && a.capStore.IsRevoked(capJTI)) {
		stream.Reset(protocol.ErrCodeAuthRequired)
		go a.closeSession(sess)
		return
	}

	switch reqHdr.Kind {
	case protocol.StreamKindWS:
		if a.cfg.WSProxy == nil {
			stream.Reset(protocol.ErrCodeForbidden)
			return
		}
		a.cfg.WSProxy.HandleStream(stream, reqHdr, clientIP, jar, capScope)
	case protocol.StreamKindHTTP:
		a.cfg.HTTPProxy.HandleStream(stream, reqHdr, clientIP, jar, capScope)
	default:
		stream.Reset(protocol.ErrCodeProtocolError)
	}
}
