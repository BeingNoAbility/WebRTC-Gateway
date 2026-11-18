package proxy

import (
	"context"
	"encoding/json"
	"fmt"
	"html"
	"log"
	"mime"
	"net/http"
	"net/url"
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

	realDir, err := filepath.EvalSymlinks(cleanPath)
	if err != nil {
		return nil, fmt.Errorf("eval symlinks for directory %s: %w", cleanPath, err)
	}

	fi, err := os.Stat(realDir)
	if err != nil {
		return nil, fmt.Errorf("stat directory %s: %w", realDir, err)
	}
	if !fi.IsDir() {
		return nil, fmt.Errorf("path %s is a file, not a directory (use -file instead)", realDir)
	}

	return &FileProxy{
		mode:       FileModeDirectory,
		targetPath: realDir,
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
	ctx, cancel := context.WithTimeout(stream.Context(), 10*time.Second)
	headersPayload, err := stream.ReadHeaders(ctx)
	cancel()

	if err != nil {
		log.Printf("[FileProxy] Stream %d failed to read headers: %v", stream.ID(), err)
		_ = stream.Close()
		return
	}

	var reqMeta RequestMetadata
	if err := json.Unmarshal(headersPayload, &reqMeta); err != nil {
		log.Printf("[FileProxy] Stream %d failed to parse request headers: %v", stream.ID(), err)
		fp.sendError(stream, http.StatusBadRequest, "Invalid request metadata")
		_ = stream.Close()
		return
	}

	fp.HandleStreamWithMeta(sessionID, stream, &reqMeta)
}

// HandleStreamWithMeta serves files/directories in memory using pre-parsed request metadata.
func (fp *FileProxy) HandleStreamWithMeta(sessionID string, stream *mux.Stream, reqMeta *RequestMetadata) {
	defer stream.Close()

	if reqMeta == nil {
		fp.sendError(stream, http.StatusBadRequest, "Missing request metadata")
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

	// 1. Safe URL normalization and unescaping
	// Strip query parameters for filesystem mapping
	rawPath := reqPath
	if idx := strings.Index(rawPath, "?"); idx != -1 {
		rawPath = rawPath[:idx]
	}

	// Unescape BEFORE path.Clean to defeat %2e%2e traversal bypasses while properly
	// resolving special characters (such as Asian characters, brackets, spaces, etc.)
	unescapedPath, err := url.PathUnescape(rawPath)
	if err != nil {
		unescapedPath = rawPath
	}

	// Reject embedded null bytes immediately
	if strings.ContainsRune(unescapedPath, 0) {
		http.Error(w, "Bad Request: Null byte detected in path", http.StatusBadRequest)
		return
	}

	// Normalize Windows backslashes to forward slashes before path.Clean
	unescapedPath = strings.ReplaceAll(unescapedPath, "\\", "/")
	cleanReqPath := path.Clean("/" + unescapedPath)

	req, err := http.NewRequestWithContext(stream.Context(), method, cleanReqPath, nil)
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
		w.Header().Set("X-P2P-Mode", "file")
		w.Header().Set("X-P2P-File-Name", fp.fileName)
		w.Header().Set("X-P2P-File-Size", fmt.Sprintf("%d", fp.fileSize))
		if mimeType := detectMimeType(fp.targetPath); mimeType != "" {
			w.Header().Set("Content-Type", mimeType)
		}
		if fp.isInline {
			w.Header().Set("Content-Disposition", fmt.Sprintf("inline; filename=%q", fp.fileName))
		} else {
			w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", fp.fileName))
		}
		w.Header().Set("Accept-Ranges", "bytes")

		f, err := os.Open(fp.targetPath)
		if err != nil {
			http.NotFound(w, req)
			return
		}
		defer f.Close()

		fi, err := f.Stat()
		var modTime time.Time
		if err == nil {
			modTime = fi.ModTime()
		}
		http.ServeContent(w, req, fp.fileName, modTime, f)
		return
	}

	// 3. Handle Directory Mode with Path Traversal & Symlink Defense
	relSubPath := filepath.FromSlash(strings.TrimPrefix(cleanReqPath, "/"))
	localPath := filepath.Join(fp.targetPath, relSubPath)

	// Lexical boundary check: ensure path does not escape root
	rel, err := filepath.Rel(fp.targetPath, localPath)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		http.Error(w, "Forbidden: Path traversal blocked", http.StatusForbidden)
		return
	}

	// Check existence
	fi, err := os.Stat(localPath)
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
	if err != nil || realRel == ".." || strings.HasPrefix(realRel, ".."+string(filepath.Separator)) {
		http.Error(w, "Forbidden: Symlink target escapes root directory", http.StatusForbidden)
		return
	}

	w.Header().Set("X-P2P-Mode", "dir")
	if fi.IsDir() {
		indexPath := filepath.Join(realPath, "index.html")
		if idxFi, err := os.Stat(indexPath); err == nil && !idxFi.IsDir() {
			idxRealPath, err := filepath.EvalSymlinks(indexPath)
			if err != nil {
				http.NotFound(w, req)
				return
			}
			idxRel, err := filepath.Rel(fp.targetPath, idxRealPath)
			if err != nil || idxRel == ".." || strings.HasPrefix(idxRel, ".."+string(filepath.Separator)) {
				http.Error(w, "Forbidden: Symlink target escapes root directory", http.StatusForbidden)
				return
			}

			f, err := os.Open(idxRealPath)
			if err != nil {
				http.NotFound(w, req)
				return
			}
			defer f.Close()

			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			w.Header().Set("Accept-Ranges", "bytes")
			http.ServeContent(w, req, "index.html", idxFi.ModTime(), f)
			return
		}

		// index.html does not exist in directory
		rawPath := reqMeta.Path
		hasSlash := strings.HasSuffix(rawPath, "/") || rawPath == "" || cleanReqPath == "/"
		if !hasSlash {
			redirectTarget := cleanReqPath + "/"
			http.Redirect(w, req, redirectTarget, http.StatusMovedPermanently)
			return
		}

		fp.renderDirectoryListing(w, req, realPath, cleanReqPath)
		return
	}

	w.Header().Set("X-P2P-File-Name", fi.Name())
	w.Header().Set("X-P2P-File-Size", fmt.Sprintf("%d", fi.Size()))
	if mimeType := detectMimeType(realPath); mimeType != "" {
		w.Header().Set("Content-Type", mimeType)
	}
	if isInlineExtension(filepath.Ext(realPath)) {
		w.Header().Set("Content-Disposition", fmt.Sprintf("inline; filename=%q", fi.Name()))
	} else {
		w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", fi.Name()))
	}
	w.Header().Set("Accept-Ranges", "bytes")

	f, err := os.Open(realPath)
	if err != nil {
		http.NotFound(w, req)
		return
	}
	defer f.Close()

	http.ServeContent(w, req, fi.Name(), fi.ModTime(), f)
}

func (fp *FileProxy) renderDirectoryListing(w http.ResponseWriter, req *http.Request, dirPath, reqPath string) {
	entries, err := os.ReadDir(dirPath)
	if err != nil {
		http.Error(w, "Failed to read directory", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusOK)

	escapedPath := html.EscapeString(reqPath)
	fmt.Fprintf(w, "<!DOCTYPE html>\n<html><head><meta charset=\"utf-8\"><title>Index of %s</title></head><body>\n", escapedPath)
	fmt.Fprintf(w, "<h2>Index of %s</h2><hr>\n<pre>\n", escapedPath)

	if reqPath != "/" && reqPath != "" {
		fmt.Fprintf(w, "<a href=\"../\">../</a>\n")
	}

	for _, entry := range entries {
		name := entry.Name()
		displayName := html.EscapeString(name)
		link := url.PathEscape(name)
		if entry.IsDir() {
			displayName += "/"
			link += "/"
		}
		info, err := entry.Info()
		sizeStr := "-"
		modTimeStr := ""
		if err == nil {
			modTimeStr = info.ModTime().Format("2006-01-02 15:04:05")
			if !entry.IsDir() {
				sizeStr = formatByteSize(info.Size())
			}
		}
		fmt.Fprintf(w, "<a href=\"%s\">%-30s</a> %20s %10s\n", link, displayName, modTimeStr, sizeStr)
	}

	fmt.Fprintf(w, "</pre><hr></body></html>\n")
}

func formatByteSize(b int64) string {
	const unit = 1024
	if b < unit {
		return fmt.Sprintf("%d B", b)
	}
	div, exp := int64(unit), 0
	for n := b / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(b)/float64(div), "KMGTPE"[exp])
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
	case ".mp4", ".m4v", ".webm", ".ogg", ".ogv", ".mp3", ".wav", ".flac", ".aac", ".m4a", ".m4s", ".ts",
		".png", ".jpg", ".jpeg", ".gif", ".webp", ".svg", ".ico", ".bmp",
		".pdf", ".txt", ".md", ".json", ".csv", ".log", ".html", ".htm", ".css", ".js", ".xml":
		return true
	default:
		return false
	}
}

// Built-in comprehensive MIME map to ensure correct Content-Type across all minimal OS environments
// (including Alpine Linux, minimal Docker images, and Windows without specific file registrations).
var builtinMimeTypes = map[string]string{
	// Video
	".mp4":  "video/mp4",
	".m4v":  "video/mp4",
	".webm": "video/webm",
	".mkv":  "video/x-matroska",
	".avi":  "video/x-msvideo",
	".mov":  "video/quicktime",
	".ogv":  "video/ogg",
	".ts":   "video/mp2t",
	// Audio
	".mp3":  "audio/mpeg",
	".wav":  "audio/wav",
	".flac": "audio/flac",
	".aac":  "audio/aac",
	".m4a":  "audio/mp4",
	".oga":  "audio/ogg",
	".ogg":  "audio/ogg",
	".weba": "audio/webm",
	// Documents & Text
	".pdf":  "application/pdf",
	".txt":  "text/plain; charset=utf-8",
	".md":   "text/markdown; charset=utf-8",
	".html": "text/html; charset=utf-8",
	".htm":  "text/html; charset=utf-8",
	".css":  "text/css; charset=utf-8",
	".js":   "application/javascript; charset=utf-8",
	".mjs":  "application/javascript; charset=utf-8",
	".json": "application/json; charset=utf-8",
	".xml":  "application/xml; charset=utf-8",
	".csv":  "text/csv; charset=utf-8",
	".log":  "text/plain; charset=utf-8",
	// Images
	".png":  "image/png",
	".jpg":  "image/jpeg",
	".jpeg": "image/jpeg",
	".gif":  "image/gif",
	".webp": "image/webp",
	".svg":  "image/svg+xml",
	".ico":  "image/x-icon",
	".bmp":  "image/bmp",
	// Archives & Binaries
	".zip": "application/zip",
	".tar": "application/x-tar",
	".gz":  "application/gzip",
	".tgz": "application/gzip",
	".rar": "application/vnd.rar",
	".7z":  "application/x-7z-compressed",
	".iso": "application/x-iso9660-image",
}

// detectMimeType returns the MIME Content-Type for a given file path based on its extension.
func detectMimeType(filePath string) string {
	ext := strings.ToLower(filepath.Ext(filePath))
	if mt, ok := builtinMimeTypes[ext]; ok {
		return mt
	}
	if mt := mime.TypeByExtension(ext); mt != "" {
		return mt
	}
	return ""
}
