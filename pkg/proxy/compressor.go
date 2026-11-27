package proxy

import (
	"compress/gzip"
	"io"
	"strings"
	"sync"
)

var gzipWriterPool = sync.Pool{
	New: func() any {
		w, _ := gzip.NewWriterLevel(io.Discard, gzip.BestSpeed)
		return w
	},
}

// ShouldCompressContentType determines whether an uncompressed response MIME type benefits from
// streaming gzip paired with native browser DecompressionStream('gzip').
func ShouldCompressContentType(ct string, contentLen int64) bool {
	if contentLen >= 0 && contentLen < 1024 {
		return false
	}
	mime := strings.ToLower(strings.TrimSpace(strings.Split(ct, ";")[0]))
	if mime == "" {
		return false
	}
	if mime == "text/event-stream" {
		return false // Never buffer SSE streams!
	}
	if strings.HasPrefix(mime, "text/") {
		return true
	}
	switch mime {
	case "application/json", "application/javascript", "application/ecmascript",
		"application/xml", "application/xhtml+xml", "application/wasm",
		"image/svg+xml", "application/manifest+json":
		return true
	default:
		return strings.HasSuffix(mime, "+json") || strings.HasSuffix(mime, "+xml")
	}
}

// StreamingGzipWriter wraps a destination io.Writer with pooled gzip.BestSpeed compression
// and flushes after every Write so NDJSON, streaming logs, and chunked APIs never stall!
type StreamingGzipWriter struct {
	gz     *gzip.Writer
	closed bool
}

func NewStreamingGzipWriter(dst io.Writer) *StreamingGzipWriter {
	gz := gzipWriterPool.Get().(*gzip.Writer)
	gz.Reset(dst)
	return &StreamingGzipWriter{gz: gz}
}

func (w *StreamingGzipWriter) Write(p []byte) (int, error) {
	if w.closed {
		return 0, io.ErrClosedPipe
	}
	n, err := w.gz.Write(p)
	if err != nil {
		return n, err
	}
	if ferr := w.gz.Flush(); ferr != nil {
		return n, ferr
	}
	return n, nil
}

func (w *StreamingGzipWriter) Close() error {
	if w.closed {
		return nil
	}
	w.closed = true
	err := w.gz.Close()
	w.gz.Reset(io.Discard)
	gzipWriterPool.Put(w.gz)
	w.gz = nil
	return err
}

func (w *StreamingGzipWriter) Abort() {
	if w.closed {
		return
	}
	w.closed = true
	w.gz.Reset(io.Discard)
	gzipWriterPool.Put(w.gz)
	w.gz = nil
}
