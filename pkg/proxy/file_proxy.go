package proxy

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	"webrtc-gateway/pkg/mux"
)

// FileProxyMode defines the operating mode of the FileProxy.
type FileProxyMode int

const (
	FileModeSingleFile FileProxyMode = iota
	FileModeDirectory
)

// FileProxy provides zero-listener in-memory HTTP serving for local files or directories
// directly over multiplexed WebRTC DataChannel streams (mux.Stream).
type FileProxy struct {
	mode       FileProxyMode
	targetPath string // Absolute, cleaned path to file or directory
	fileName   string // Base name of the file (for single file mode)
	fileSize   int64  // File size in bytes
	isInline   bool   // Whether to set Content-Disposition inline vs attachment
}

// NewSingleFileProxy creates a FileProxy that serves a single specific file for all requests.
func NewSingleFileProxy(filePath string) (*FileProxy, error) {
	cleanPath, err := filepath.Abs(filepath.Clean(filePath))
	if err != nil {
		return nil, fmt.Errorf("resolve file path: %w", err)
	}

	fi, err := os.Stat(cleanPath)
	if err != nil {
		return nil, fmt.Errorf("stat file %s: %w", cleanPath, err)
	}
	if fi.IsDir() {
		return nil, fmt.Errorf("path %s is a directory, not a file (use -dir instead)", cleanPath)
	}

	ext := filepath.Ext(cleanPath)
	inline := isInlineExtension(ext)

	return &FileProxy{
		mode:       FileModeSingleFile,
		targetPath: cleanPath,
		fileName:   fi.Name(),
		fileSize:   fi.Size(),
		isInline:   inline,
	}, nil
}

// NewDirProxy creates a FileProxy that serves a directory tree with auto index and directory listing.
func NewDirProxy(dirPath string) (*FileProxy, error) {
	cleanPath, err := filepath.Abs(filepath.Clean(dirPath))
	if err != nil {
		return nil, fmt.Errorf("resolve directory path: %w", err)
	}

	fi, err := os.Stat(cleanPath)
	if err != nil {
		return nil, fmt.Errorf("stat directory %s: %w", cleanPath, err)
	}
	if !fi.IsDir() {
		return nil, fmt.Errorf("path %s is a file, not a directory (use -file instead)", cleanPath)
	}

	return &FileProxy{
		mode:       FileModeDirectory,
		targetPath: cleanPath,
	}, nil
}

// Mode returns whether this proxy is serving a single file or a directory.
func (fp *FileProxy) Mode() FileProxyMode {
	return fp.mode
}

// TargetPath returns the absolute path being served.
func (fp *FileProxy) TargetPath() string {
	return fp.targetPath
}

// FileName returns the base file name (in single file mode).
func (fp *FileProxy) FileName() string {
	return fp.fileName
}

// FileSize returns the size of the target file in bytes (in single file mode).
func (fp *FileProxy) FileSize() int64 {
	return fp.fileSize
}

// HandleStream handles an incoming multiplexed stream and serves files/directories in memory.
func (fp *FileProxy) HandleStream(sessionID string, stream *mux.Stream) {
	defer stream.Close()

	// 1. Read request headers with timeout
	ctx, cancel := context.WithTimeout(stream.Context(), 10*time.Second)
	headersPayload, err := stream.ReadHeaders(ctx)
	cancel()

	if err != nil {
		log.Printf("[FileProxy] Stream %d failed to read headers: %v", stream.ID(), err)
		return
	}

	var reqMeta RequestMetadata
	if err := json.Unmarshal(headersPayload, &reqMeta); err != nil {
		log.Printf("[FileProxy] Stream %d failed to parse request headers: %v", stream.ID(), err)
		fp.sendError(stream, http.StatusBadRequest, "Invalid request metadata")
		return
	}

	w := newStreamResponseWriter(stream)
	method := strings.ToUpper(reqMeta.Method)

	// Strict Read-Only policy: Only GET and HEAD methods are permitted
	if method != http.MethodGet && method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		http.Error(w, "Method Not Allowed: file server is strictly read-only", http.StatusMethodNotAllowed)
		return
	}

	// Construct http.Request for net/http compatibility
	reqPath := reqMeta.Path
	if reqPath == "" {
		reqPath = "/"
	}
	req, err := http.NewRequestWithContext(stream.Context(), method, reqPath, nil)
	if err != nil {
		fp.sendError(stream, http.StatusInternalServerError, "Failed to create request")
		return
	}

	// Copy incoming request headers (Range, If-Modified-Since, etc.)
	for k, v := range reqMeta.Headers {
		req.Header.Set(k, v)
	}

	// 2. Handle Single File Mode
	if fp.mode == FileModeSingleFile {
		if fp.isInline {
			w.Header().Set("Content-Disposition", fmt.Sprintf("inline; filename=%q", fp.fileName))
		} else {
			w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", fp.fileName))
		}
		http.ServeFile(w, req, fp.targetPath)
		return
	}

	// 3. Handle Directory Mode with Path Traversal & Symlink Defense
	cleanReqPath := path.Clean("/" + reqPath)
	relSubPath := filepath.FromSlash(strings.TrimPrefix(cleanReqPath, "/"))
	localPath := filepath.Join(fp.targetPath, relSubPath)

	// Lexical boundary check: ensure path does not escape root
	rel, err := filepath.Rel(fp.targetPath, localPath)
	if err != nil || strings.HasPrefix(rel, "..") || strings.HasPrefix(rel, "/..") || strings.HasPrefix(rel, `\..`) {
		http.Error(w, "Forbidden: Path traversal blocked", http.StatusForbidden)
		return
	}

	// Check existence
	_, err = os.Stat(localPath)
	if err != nil {
		if os.IsNotExist(err) {
			http.NotFound(w, req)
		} else {
			http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		}
		return
	}

	// Symlink escape defense: resolve symlink target and verify it remains within targetPath
	realPath, err := filepath.EvalSymlinks(localPath)
	if err != nil {
		http.NotFound(w, req)
		return
	}
	realRel, err := filepath.Rel(fp.targetPath, realPath)
	if err != nil || strings.HasPrefix(realRel, "..") {
		http.Error(w, "Forbidden: Symlink target escapes root directory", http.StatusForbidden)
		return
	}

	// Serve the verified file or directory (handles index.html or directory listing automatically)
	http.ServeFile(w, req, realPath)
}

func (fp *FileProxy) sendError(stream *mux.Stream, statusCode int, message string) {
	respMeta := ResponseMetadata{
		Status: statusCode,
		Msg:    message,
		Headers: map[string]string{
			"content-type": "text/plain; charset=utf-8",
		},
	}
	metaJSON, _ := json.Marshal(respMeta)
	_ = stream.WriteHeaders(metaJSON)
	_, _ = stream.Write([]byte(message))
}

// streamResponseWriter adapts mux.Stream to standard http.ResponseWriter in memory,
// avoiding any local TCP socket listeners and preserving the pure outbound architecture.
type streamResponseWriter struct {
	stream     *mux.Stream
	header     http.Header
	statusCode int
	headerSent bool
}

func newStreamResponseWriter(stream *mux.Stream) *streamResponseWriter {
	return &streamResponseWriter{
		stream: stream,
		header: make(http.Header),
	}
}

func (w *streamResponseWriter) Header() http.Header {
	return w.header
}

func (w *streamResponseWriter) WriteHeader(statusCode int) {
	if w.headerSent {
		return
	}
	w.statusCode = statusCode
	w.headerSent = true

	respHeaders := make(map[string]string)
	for k, v := range w.header {
		lk := strings.ToLower(k)
		// Strip iframe-blocking headers for seamless sandbox virtualization
		if lk == "x-frame-options" || lk == "content-security-policy" || lk == "content-security-policy-report-only" {
			continue
		}
		respHeaders[lk] = strings.Join(v, ", ")
	}

	respHeaders["referrer-policy"] = "no-referrer"

	respMeta := ResponseMetadata{
		Status:  statusCode,
		Msg:     http.StatusText(statusCode),
		Headers: respHeaders,
	}

	metaJSON, _ := json.Marshal(respMeta)
	_ = w.stream.WriteHeaders(metaJSON)
}

func (w *streamResponseWriter) Write(b []byte) (int, error) {
	if !w.headerSent {
		w.WriteHeader(http.StatusOK)
	}
	return w.stream.Write(b)
}

// isInlineExtension determines whether a file extension should be displayed inline
// (in-browser media playback / view) or as a file attachment download.
func isInlineExtension(ext string) bool {
	switch strings.ToLower(ext) {
	case ".mp4", ".webm", ".ogg", ".ogv", ".mp3", ".wav", ".flac", ".aac", ".m4a", ".m4s", ".ts",
		".png", ".jpg", ".jpeg", ".gif", ".webp", ".svg", ".ico", ".bmp",
		".pdf", ".txt", ".md", ".json", ".csv", ".log", ".html", ".htm", ".css", ".js", ".xml":
		return true
	default:
		return false
	}
}
