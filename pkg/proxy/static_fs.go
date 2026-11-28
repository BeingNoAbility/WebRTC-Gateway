package proxy

import (
	"bytes"
	"errors"
	"fmt"
	"html"
	"io"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
)

const (
	maxStaticDirEntries   = 10000
	maxStaticDirScanned   = 20000
	maxDirListingSymlinks = 512
	dirReadBatchSize      = 512
	maxSymlinkHops        = 16

	// Applied to ALL served files and error/redirect responses when !inlineActive (blocks extensionless HTML/SVG XSS).
	staticFileSandboxCSP = "sandbox; default-src 'none'; style-src 'unsafe-inline'; img-src data: blob:; media-src data: blob:"
	// Applied to generated directory listings (includes allow-downloads so browsers permit file downloads).
	staticDirSandboxCSP = "sandbox allow-downloads; default-src 'none'; style-src 'unsafe-inline'"
)

// StaticFSOptions configures the minimal Go 1.24+ os.Root read-only static handler.
type StaticFSOptions struct {
	RootDir        string // Directory to serve (mutually exclusive with SingleFile)
	SingleFile     string // Optional single file path to serve at /
	HideDotfiles   bool   // Default true: blocks dotfiles (and symlinks targeting dotfiles)
	InlineActive   bool   // Default false: applies CSP: sandbox to ALL served files
	FollowSymlinks bool   // Default false (Fail-Closed O_NOFOLLOW): reject symlinks unless explicitly opted in
}

// StaticFSHandler is a minimal read-only http.Handler backed by Go 1.24+ os.Root
// plugged directly into HTTPProxy via NewInProcessTransport.
type StaticFSHandler struct {
	root           *os.Root
	singleName     string
	hideDotfiles   bool
	inlineActive   bool
	followSymlinks bool
	closeOnce      sync.Once
}

func NewStaticFSHandler(opts StaticFSOptions) (*StaticFSHandler, error) {
	dir := opts.RootDir
	singleName := ""
	if opts.SingleFile != "" {
		abs, err := filepath.Abs(opts.SingleFile)
		if err != nil {
			return nil, err
		}
		dir = filepath.Dir(abs)
		singleName = filepath.Base(abs)
	}
	if dir == "" {
		return nil, errors.New("proxy/static: RootDir or SingleFile is required")
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		return nil, fmt.Errorf("proxy/static: open os.Root %q: %w", dir, err)
	}
	h := &StaticFSHandler{
		root:           root,
		singleName:     singleName,
		hideDotfiles:   opts.HideDotfiles,
		inlineActive:   opts.InlineActive,
		followSymlinks: opts.FollowSymlinks,
	}
	// Fail-Closed at startup: verify SingleFile exists and is a regular file (L-9).
	if singleName != "" {
		f, _, _, oerr := h.openSafeRootPath(singleName, "", true)
		if oerr != nil {
			_ = root.Close()
			return nil, fmt.Errorf("proxy/static: invalid SingleFile %q: %w", opts.SingleFile, oerr)
		}
		fi, serr := f.Stat()
		_ = f.Close()
		if serr != nil || !fi.Mode().IsRegular() {
			_ = root.Close()
			return nil, fmt.Errorf("proxy/static: SingleFile %q is not a regular file", opts.SingleFile)
		}
	}
	return h, nil
}

func (h *StaticFSHandler) Close() error {
	var err error
	h.closeOnce.Do(func() {
		err = h.root.Close()
	})
	return err
}

func (h *StaticFSHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// Always apply Fail-Closed baseline headers before any error/redirect/content write (M-17).
	h.applyFileSecurityHeaders(w)

	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
		return
	}

	cleanReqPath := cleanPathPreserveSlash(r.URL.Path)

	if h.singleName != "" {
		if cleanReqPath != "/" && cleanReqPath != "/"+h.singleName {
			http.NotFound(w, r)
			return
		}
		h.serveSingleFile(w, r, h.singleName)
		return
	}

	rel := strings.TrimPrefix(cleanReqPath, "/")
	if rel == "" {
		rel = "."
	} else {
		rel = strings.TrimSuffix(rel, "/")
	}

	// Enforce CapabilityScope.PathPrefix confinement even across symlink hops (H-8).
	scopePrefix := strings.TrimPrefix(StaticPathPrefixFromContext(r.Context()), "/")

	f, resolvedRel, dirRoot, err := h.openSafeRootPath(rel, scopePrefix, false)
	if err != nil {
		if os.IsNotExist(err) || errors.Is(err, os.ErrPermission) {
			http.NotFound(w, r)
		} else {
			http.Error(w, "Forbidden", http.StatusForbidden)
		}
		return
	}
	defer f.Close()
	if dirRoot != nil && dirRoot != h.root {
		defer dirRoot.Close()
	}

	fi, err := f.Stat()
	if err != nil {
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		return
	}

	if fi.IsDir() {
		if !strings.HasSuffix(cleanReqPath, "/") {
			targetPath := "/"
			if cleanVirtual := strings.Trim(cleanReqPath, "/"); cleanVirtual != "" {
				targetPath = "/" + cleanVirtual + "/"
			}
			redir := (&url.URL{Path: targetPath}).EscapedPath()
			if !strings.HasPrefix(redir, "/") || strings.HasPrefix(redir, "//") {
				http.Error(w, "Bad Request", http.StatusBadRequest)
				return
			}
			if r.URL.RawQuery != "" {
				redir += "?" + r.URL.RawQuery
			}
			http.Redirect(w, r, redir, http.StatusMovedPermanently) // #nosec G710 -- root-relative path built from cleaned virtual request path with // prefix rejected
			return
		}
		idxRel := "index.html"
		if resolvedRel != "." {
			idxRel = resolvedRel + "/index.html"
		}
		if idxFile, _, idxDirRoot, ierr := h.openSafeRootPath(idxRel, scopePrefix, false); ierr == nil {
			if idxDirRoot != nil && idxDirRoot != h.root {
				_ = idxDirRoot.Close()
			}
			if idxInfo, serr := idxFile.Stat(); serr == nil && idxInfo.Mode().IsRegular() {
				defer idxFile.Close()
				http.ServeContent(w, r, "index.html", idxInfo.ModTime(), idxFile)
				return
			}
			_ = idxFile.Close()
		}
		h.serveDirListing(w, r, f, dirRoot, resolvedRel, cleanReqPath, scopePrefix)
		return
	}

	if strings.HasSuffix(cleanReqPath, "/") {
		http.NotFound(w, r)
		return
	}
	if !fi.Mode().IsRegular() {
		http.Error(w, "Forbidden", http.StatusForbidden)
		return
	}

	http.ServeContent(w, r, fi.Name(), fi.ModTime(), f)
}

func (h *StaticFSHandler) serveSingleFile(w http.ResponseWriter, r *http.Request, name string) {
	f, _, _, err := h.openSafeRootPath(name, "", true)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil || !fi.Mode().IsRegular() {
		http.Error(w, "Forbidden", http.StatusForbidden)
		return
	}
	http.ServeContent(w, r, fi.Name(), fi.ModTime(), f)
}

func isWithinScopePrefix(relPath, scopePrefix string) bool {
	if scopePrefix == "" || scopePrefix == "." {
		return true
	}
	clean := path.Clean(relPath)
	return clean == scopePrefix || strings.HasPrefix(clean, scopePrefix+"/")
}

func validateSymlinkTarget(target string) error {
	if target == "" {
		return os.ErrPermission
	}
	if strings.ContainsRune(target, '\x00') || strings.ContainsRune(target, '\\') {
		return os.ErrPermission
	}
	if path.IsAbs(target) || filepath.IsAbs(target) || filepath.VolumeName(target) != "" {
		return os.ErrPermission
	}
	if len(target) >= 2 && target[1] == ':' && ((target[0] >= 'a' && target[0] <= 'z') || (target[0] >= 'A' && target[0] <= 'Z')) {
		return os.ErrPermission
	}
	return nil
}

// openSafeRootPath descends stepwise via os.Root.OpenRoot(seg) with SameFile verification and
// O_NOFOLLOW|O_NONBLOCK. When followSymlinks is false (default Fail-Closed), ALL symlinks are
// rejected immediately. When followSymlinks is true, multi-hop symlinks are resolved strictly
// within BOTH os.Root AND scopePrefix (eliminating H-8 CapabilityScope symlink escapes).
// When the resolved entry is a directory, dirRoot is returned (caller closes if != h.root) so
// serveDirListing can Lstat DT_UNKNOWN entries directly in O(1) syscalls (eliminating H-12).
func (h *StaticFSHandler) openSafeRootPath(rel, scopePrefix string, allowSingleDotfile bool) (*os.File, string, *os.Root, error) {
	curPath := path.Clean(rel)
	if !isWithinScopePrefix(curPath, scopePrefix) {
		return nil, "", nil, os.ErrPermission
	}
	if curPath == "." || curPath == "" {
		f, err := openRootNoFollowNoBlock(h.root, ".")
		if err != nil {
			return nil, "", nil, err
		}
		fi, serr := f.Stat()
		if serr != nil || !fi.IsDir() {
			_ = f.Close()
			return nil, "", nil, os.ErrPermission
		}
		return f, ".", h.root, nil
	}
	if strings.HasPrefix(curPath, "/") || curPath == ".." || strings.HasPrefix(curPath, "../") {
		return nil, "", nil, os.ErrPermission
	}

	hops := 0
	for {
		parts := strings.Split(curPath, "/")
		curRoot := h.root
		closeCur := func() {
			if curRoot != h.root {
				_ = curRoot.Close()
				curRoot = h.root
			}
		}

		prefix := ""
		restarted := false

		for i, seg := range parts {
			if seg == "" || seg == "." {
				continue
			}
			if seg == ".." || strings.ContainsRune(seg, '\\') || strings.ContainsRune(seg, '\x00') {
				closeCur()
				return nil, "", nil, os.ErrPermission
			}
			allowDot := allowSingleDotfile && hops == 0 && len(parts) == 1 && seg == h.singleName
			if h.hideDotfiles && strings.HasPrefix(seg, ".") && !allowDot {
				closeCur()
				return nil, "", nil, os.ErrPermission
			}
			if prefix == "" {
				prefix = seg
			} else {
				prefix = prefix + "/" + seg
			}

			li, err := curRoot.Lstat(seg)
			if err != nil {
				closeCur()
				return nil, "", nil, err
			}
			if li.Mode()&os.ModeSymlink != 0 {
				if !h.followSymlinks {
					closeCur()
					return nil, "", nil, os.ErrPermission
				}
				hops++
				if hops > maxSymlinkHops {
					closeCur()
					return nil, "", nil, os.ErrPermission
				}
				target, rerr := curRoot.Readlink(seg)
				if rerr != nil || validateSymlinkTarget(target) != nil {
					closeCur()
					return nil, "", nil, os.ErrPermission
				}
				parent := path.Dir(prefix)
				resolved := path.Clean(target)
				if parent != "." {
					resolved = path.Clean(parent + "/" + target)
				}
				if resolved == ".." || strings.HasPrefix(resolved, "../") || strings.HasPrefix(resolved, "/") {
					closeCur()
					return nil, "", nil, os.ErrPermission
				}
				if h.hideDotfiles {
					for _, tSeg := range strings.Split(resolved, "/") {
						if tSeg != "" && tSeg != "." && strings.HasPrefix(tSeg, ".") {
							closeCur()
							return nil, "", nil, os.ErrPermission
						}
					}
				}
				rest := parts[i+1:]
				if len(rest) > 0 {
					if resolved == "." {
						curPath = path.Clean(strings.Join(rest, "/"))
					} else {
						curPath = path.Clean(resolved + "/" + strings.Join(rest, "/"))
					}
				} else {
					curPath = resolved
				}
				closeCur()
				if !isWithinScopePrefix(curPath, scopePrefix) {
					return nil, "", nil, os.ErrPermission
				}
				if curPath == "." {
					f, oerr := openRootNoFollowNoBlock(h.root, ".")
					if oerr != nil {
						return nil, "", nil, oerr
					}
					fi, serr := f.Stat()
					if serr != nil || !fi.IsDir() {
						_ = f.Close()
						return nil, "", nil, os.ErrPermission
					}
					return f, ".", h.root, nil
				}
				restarted = true
				break
			}

			if i < len(parts)-1 {
				if !li.IsDir() {
					closeCur()
					return nil, "", nil, os.ErrNotExist
				}
				nextRoot, oerr := curRoot.OpenRoot(seg)
				if oerr != nil {
					closeCur()
					return nil, "", nil, oerr
				}
				nextFi, serr := nextRoot.Stat(".")
				if serr != nil || !nextFi.IsDir() || !os.SameFile(li, nextFi) {
					_ = nextRoot.Close()
					closeCur()
					return nil, "", nil, os.ErrPermission
				}
				closeCur()
				curRoot = nextRoot
				continue
			}

			if !li.Mode().IsRegular() && !li.IsDir() {
				closeCur()
				return nil, "", nil, os.ErrPermission
			}
			f, oerr := openRootNoFollowNoBlock(curRoot, seg)
			if oerr != nil {
				closeCur()
				return nil, "", nil, oerr
			}
			fi, serr := f.Stat()
			if serr != nil || (!fi.Mode().IsRegular() && !fi.IsDir()) || !os.SameFile(li, fi) {
				_ = f.Close()
				closeCur()
				return nil, "", nil, os.ErrPermission
			}
			var targetDirRoot *os.Root
			if fi.IsDir() {
				if dr, derr := curRoot.OpenRoot(seg); derr == nil {
					targetDirRoot = dr
				}
			}
			closeCur()
			return f, prefix, targetDirRoot, nil
		}

		if !restarted {
			closeCur()
			return nil, "", nil, os.ErrPermission
		}
	}
}

func (h *StaticFSHandler) applyFileSecurityHeaders(w http.ResponseWriter) {
	w.Header().Set("X-Content-Type-Options", "nosniff")
	if !h.inlineActive {
		w.Header().Set("Content-Security-Policy", staticFileSandboxCSP)
	}
}

func (h *StaticFSHandler) serveDirListing(
	w http.ResponseWriter,
	r *http.Request,
	dir *os.File,
	dirRoot *os.Root,
	resolvedRel, displayPath, scopePrefix string,
) {
	type item struct {
		name  string
		isDir bool
	}
	var items []item
	scanned := 0
	symlinksResolved := 0
	truncated := false

	for len(items) < maxStaticDirEntries && scanned < maxStaticDirScanned {
		if r.Context().Err() != nil {
			return
		}
		batch, err := dir.ReadDir(dirReadBatchSize)
		for _, de := range batch {
			scanned++
			if scanned > maxStaticDirScanned {
				truncated = true
				break
			}
			name := de.Name()
			if h.hideDotfiles && strings.HasPrefix(name, ".") {
				continue
			}
			isDir := false
			typ := de.Type()
			// Fast O(1) Lstat via pinned dirRoot for DT_UNKNOWN (FUSE/NFS/XFS) instead of re-traversing from root (H-12).
			if typ == 0 && dirRoot != nil {
				if li, lerr := dirRoot.Lstat(name); lerr == nil {
					typ = li.Mode().Type()
				} else {
					continue
				}
			}
			if typ&os.ModeSymlink != 0 {
				if !h.followSymlinks || symlinksResolved >= maxDirListingSymlinks {
					continue
				}
				symlinksResolved++
				childRel := name
				if resolvedRel != "." {
					childRel = resolvedRel + "/" + name
				}
				cf, _, childDirRoot, cerr := h.openSafeRootPath(childRel, scopePrefix, false)
				if childDirRoot != nil && childDirRoot != h.root {
					_ = childDirRoot.Close()
				}
				if cerr != nil {
					continue
				}
				cfi, serr := cf.Stat()
				_ = cf.Close()
				if serr != nil || (!cfi.Mode().IsRegular() && !cfi.IsDir()) {
					continue
				}
				isDir = cfi.IsDir()
			} else if typ.IsDir() {
				isDir = true
			} else if !typ.IsRegular() {
				continue
			}
			items = append(items, item{name: name, isDir: isDir})
			if len(items) >= maxStaticDirEntries {
				truncated = true
				break
			}
		}
		if err == io.EOF || truncated {
			break
		}
		if err != nil {
			http.Error(w, "Internal Server Error", http.StatusInternalServerError)
			return
		}
	}

	sort.Slice(items, func(i, j int) bool {
		if items[i].isDir != items[j].isDir {
			return items[i].isDir
		}
		return strings.ToLower(items[i].name) < strings.ToLower(items[j].name)
	})

	var buf bytes.Buffer
	buf.WriteString("<!doctype html><html><head><meta charset=\"utf-8\"><title>Index of ")
	buf.WriteString(html.EscapeString(displayPath))
	buf.WriteString("</title></head><body><h1>Index of ")
	buf.WriteString(html.EscapeString(displayPath))
	buf.WriteString("</h1>")
	if truncated {
		buf.WriteString("<p><em>Directory listing truncated (limit reached).</em></p>")
	}
	buf.WriteString("<ul>")
	if displayPath != "/" {
		buf.WriteString(`<li><a href="../">../</a></li>`)
	}
	for _, it := range items {
		label := it.name
		if it.isDir {
			label += "/"
		}
		href := "./" + (&url.URL{Path: it.name}).EscapedPath()
		if it.isDir {
			href += "/"
		}
		buf.WriteString(`<li><a href="`)
		buf.WriteString(html.EscapeString(href))
		buf.WriteString(`">`)
		buf.WriteString(html.EscapeString(label))
		buf.WriteString("</a></li>")
	}
	buf.WriteString("</ul></body></html>\n")

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Content-Length", strconv.Itoa(buf.Len()))
	w.Header().Set("Content-Security-Policy", staticDirSandboxCSP)
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(http.StatusOK)
	if r.Method == http.MethodGet {
		_, _ = w.Write(buf.Bytes())
	}
}

// NewInProcessTransport adapts any standard http.Handler into a race-free http.RoundTripper
// with atomic header snapshots, HEAD body suppression, accurate ContentLength, cancellation, and panic recovery.
func NewInProcessTransport(handler http.Handler) http.RoundTripper {
	return roundTripperFunc(func(req *http.Request) (*http.Response, error) {
		pr, pw := io.Pipe()
		rw := newPipeResponseWriter(pw, req.Method == http.MethodHead)
		go func() {
			defer func() {
				if rec := recover(); rec != nil {
					if !rw.wroteHeader {
						rw.header.Set("Content-Type", "text/plain; charset=utf-8")
						rw.WriteHeader(http.StatusInternalServerError)
					}
					_ = pw.CloseWithError(fmt.Errorf("static handler panic: %v", rec))
					return
				}
				rw.finish()
			}()
			handler.ServeHTTP(rw, req)
		}()

		select {
		case <-rw.hdrReady:
		case <-req.Context().Done():
			_ = pr.Close()
			return nil, req.Context().Err()
		}

		hdr := rw.snapshotHeader
		var contentLen int64 = -1
		if cl := hdr.Get("Content-Length"); cl != "" {
			if n, err := strconv.ParseInt(strings.TrimSpace(cl), 10, 64); err == nil && n >= 0 {
				contentLen = n
			}
		}
		return &http.Response{
			StatusCode:    rw.status,
			Status:        fmt.Sprintf("%d %s", rw.status, http.StatusText(rw.status)),
			Proto:         "HTTP/1.1",
			ProtoMajor:    1,
			ProtoMinor:    1,
			Header:        hdr,
			Body:          pr,
			ContentLength: contentLen,
			Request:       req,
		}, nil
	})
}

type roundTripperFunc func(*http.Request) (*http.Response, error)

func (f roundTripperFunc) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }

type pipeResponseWriter struct {
	header         http.Header
	snapshotHeader http.Header
	pw             *io.PipeWriter
	isHead         bool
	status         int
	wroteHeader    bool
	hdrReady       chan struct{}
	once           sync.Once
}

func newPipeResponseWriter(pw *io.PipeWriter, isHead bool) *pipeResponseWriter {
	return &pipeResponseWriter{
		header:   make(http.Header),
		pw:       pw,
		isHead:   isHead,
		status:   http.StatusOK,
		hdrReady: make(chan struct{}),
	}
}

func (w *pipeResponseWriter) Header() http.Header { return w.header }

func (w *pipeResponseWriter) WriteHeader(code int) {
	if w.wroteHeader || (code >= 100 && code < 200) {
		return
	}
	w.wroteHeader = true
	w.status = code
	w.snapshotHeader = w.header.Clone()
	w.once.Do(func() { close(w.hdrReady) })
}

func (w *pipeResponseWriter) Write(p []byte) (int, error) {
	if !w.wroteHeader {
		w.WriteHeader(http.StatusOK)
	}
	if w.isHead {
		return len(p), nil
	}
	return w.pw.Write(p)
}

func (w *pipeResponseWriter) finish() {
	if !w.wroteHeader {
		w.WriteHeader(http.StatusOK)
	}
	_ = w.pw.Close()
}
