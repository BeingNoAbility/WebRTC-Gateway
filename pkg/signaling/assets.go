package signaling

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io/fs"
	"log"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

var (
	manifestPathRegex      = regexp.MustCompile(`^/__p2p_gw__/(?:assets/)?[A-Za-z0-9._-]+$`)
	manifestSubAssetPathRe = regexp.MustCompile(`^/__p2p_gw__/assets/[A-Za-z0-9._-]+$`)
	hashedAssetFilenameRe  = regexp.MustCompile(`\.[0-9a-fA-F]{8,}\.[A-Za-z0-9]+$`)
)

type assetManifest struct {
	PortalJS  string `json:"portal_js"`
	PortalCSS string `json:"portal_css"`
	BridgeJS  string `json:"bridge_js"`
	PatchJS   string `json:"patch_js"`
	SWJS      string `json:"sw_js"`
}

func defaultAssetManifest() assetManifest {
	return assetManifest{
		PortalJS:  "/__p2p_gw__/portal.js",
		PortalCSS: "/__p2p_gw__/portal.css",
		BridgeJS:  "/__p2p_gw__/assets/bridge.js",
		PatchJS:   "/__p2p_gw__/assets/patch.js",
		SWJS:      "/__p2p_gw__/sw.js",
	}
}

type preRenderedAsset struct {
	body        []byte
	etag        string // Strong quoted SHA-256 prefix ETag
	contentType string
	cacheCtrl   string
	csp         string
}

type assetStore struct {
	// Pre-rendered assets (ready for zero-allocation serving when origins are fixed at startup).
	indexHTML  *preRenderedAsset
	bridgeHTML *preRenderedAsset
	swJS       *preRenderedAsset
	portalJS   *preRenderedAsset
	portalCSS  *preRenderedAsset
	bridgeJS   *preRenderedAsset
	patchJS    *preRenderedAsset
	faviconSVG *preRenderedAsset

	// Raw templates retained ONLY for ephemeral-port LocalDev (:0) test mode.
	rawIndexHTML  string
	rawBridgeHTML string
	rawSWJS       []byte
}

func computeETag(body []byte) string {
	sum := sha256.Sum256(body)
	return `"` + hex.EncodeToString(sum[:8]) + `"`
}

func newPreRendered(body []byte, contentType, cacheCtrl, csp string) *preRenderedAsset {
	return &preRenderedAsset{
		body:        body,
		etag:        computeETag(body),
		contentType: contentType,
		cacheCtrl:   cacheCtrl,
		csp:         csp,
	}
}

func (s *SignalServer) readStaticFile(relPath string) ([]byte, error) {
	cleanRel := filepath.ToSlash(filepath.Clean(relPath))
	if strings.HasPrefix(cleanRel, "..") || strings.HasPrefix(cleanRel, "/") {
		return nil, os.ErrNotExist
	}
	if s.opts.StaticDir != "" {
		return os.ReadFile(filepath.Join(s.opts.StaticDir, filepath.FromSlash(cleanRel)))
	}
	if s.opts.StaticFS != nil {
		return fs.ReadFile(s.opts.StaticFS, "dist/"+cleanRel)
	}
	return nil, os.ErrNotExist
}

func (s *SignalServer) loadAndPreRenderAssets() error {
	if err := s.loadAssetManifest(); err != nil {
		return err
	}
	readOptional := func(rel string) ([]byte, error) {
		raw, err := s.readStaticFile(rel)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) || errors.Is(err, fs.ErrNotExist) {
				return nil, nil
			}
			return nil, fmt.Errorf("signaling: read static asset %q: %w", rel, err)
		}
		return raw, nil
	}

	if raw, err := readOptional(strings.TrimPrefix(s.manifest.PortalJS, "/")); err != nil {
		return err
	} else if raw != nil {
		s.assets.portalJS = newPreRendered(raw, "application/javascript; charset=utf-8", assetCacheControl(s.manifest.PortalJS), "")
	}
	if raw, err := readOptional(strings.TrimPrefix(s.manifest.PortalCSS, "/")); err != nil {
		return err
	} else if raw != nil {
		s.assets.portalCSS = newPreRendered(raw, "text/css; charset=utf-8", assetCacheControl(s.manifest.PortalCSS), "")
	}
	if raw, err := readOptional(strings.TrimPrefix(s.manifest.BridgeJS, "/")); err != nil {
		return err
	} else if raw != nil {
		s.assets.bridgeJS = newPreRendered(raw, "application/javascript; charset=utf-8", assetCacheControl(s.manifest.BridgeJS), "")
	}
	if raw, err := readOptional(strings.TrimPrefix(s.manifest.PatchJS, "/")); err != nil {
		return err
	} else if raw != nil {
		s.assets.patchJS = newPreRendered(raw, "application/javascript; charset=utf-8", assetCacheControl(s.manifest.PatchJS), "")
	}
	if raw, err := readOptional("favicon.svg"); err != nil {
		return err
	} else if raw != nil {
		s.assets.faviconSVG = newPreRendered(raw, "image/svg+xml", "public, max-age=3600", "default-src 'none'; sandbox")
	}

	ephemeralLocal := s.Policy.LocalDevMode && s.Policy.LocalPort == ""

	if raw, err := readOptional("index.html"); err != nil {
		return err
	} else if raw != nil {
		s.assets.rawIndexHTML = string(raw)
		if !ephemeralLocal {
			s.assets.indexHTML = s.renderPortalIndex("")
		}
	}
	if raw, err := readOptional("__p2p_gw__/bridge.html"); err != nil {
		return err
	} else if raw != nil {
		s.assets.rawBridgeHTML = string(raw)
		if !ephemeralLocal {
			s.assets.bridgeHTML = s.renderBridgeHTML("")
		}
	}
	if raw, err := readOptional(strings.TrimPrefix(s.manifest.SWJS, "/")); err != nil {
		return err
	} else if raw != nil {
		s.assets.rawSWJS = raw
		if !ephemeralLocal {
			s.assets.swJS = s.renderSWJS("")
		}
	}
	if s.opts.StaticDir != "" && s.assets.rawIndexHTML == "" {
		log.Printf("[Signal] WARNING: static dir %q does not contain index.html; Portal UI will return 404", s.opts.StaticDir)
	} else if s.opts.StaticDir == "" && s.opts.StaticFS != nil && s.assets.rawSWJS == nil {
		log.Printf("[Signal] WARNING: embedded web assets are missing (built without '-tags embedweb' and no -static dir); rebuild with 'make build' or pass -static")
	}
	return nil
}

func (s *SignalServer) loadAssetManifest() error {
	raw, err := s.readStaticFile("__p2p_gw__/manifest.json")
	if err != nil {
		if errors.Is(err, os.ErrNotExist) || errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("signaling: read asset manifest: %w", err)
	}
	var m assetManifest
	if err := json.Unmarshal(raw, &m); err != nil {
		return fmt.Errorf("signaling: invalid asset manifest JSON (Fail-Closed): %w", err)
	}
	apply := func(field, val string, re *regexp.Regexp, dst *string) error {
		if val == "" {
			return nil
		}
		if !re.MatchString(val) || strings.Contains(val, "..") {
			return fmt.Errorf("signaling: invalid manifest path for %s: %q", field, val)
		}
		*dst = val
		return nil
	}
	if err := apply("portal_js", m.PortalJS, manifestPathRegex, &s.manifest.PortalJS); err != nil {
		return err
	}
	if err := apply("portal_css", m.PortalCSS, manifestPathRegex, &s.manifest.PortalCSS); err != nil {
		return err
	}
	if err := apply("bridge_js", m.BridgeJS, manifestSubAssetPathRe, &s.manifest.BridgeJS); err != nil {
		return err
	}
	if err := apply("patch_js", m.PatchJS, manifestSubAssetPathRe, &s.manifest.PatchJS); err != nil {
		return err
	}
	if err := apply("sw_js", m.SWJS, manifestPathRegex, &s.manifest.SWJS); err != nil {
		return err
	}
	return nil
}

func (s *SignalServer) renderPortalIndex(portOverride string) *preRenderedAsset {
	if s.assets.rawIndexHTML == "" {
		return nil
	}
	cfg := map[string]any{
		"portalOrigin":      s.Policy.PortalOrigin,
		"portalDomain":      s.Policy.PortalDomain,
		"sandboxOrigin":     s.Policy.SandboxOrigin,
		"sandboxMode":       string(s.Policy.Mode),
		"sandboxDomain":     s.Policy.SandboxDomain,
		"restrictedSandbox": s.Policy.RestrictedSandbox,
		"sameSiteMode":      s.Policy.IsSameSite,
		"localDevMode":      s.Policy.LocalDevMode,
		"version":           s.opts.Version,
	}
	if s.Policy.Mode == SandboxModeFixed {
		cfg["sandboxHost"] = s.Policy.SandboxDomain
	}
	if s.Policy.LocalDevMode {
		p := s.Policy.LocalPort
		if p == "" {
			p = portOverride
		}
		cfg["localPort"] = p
	}
	rawJSON, _ := json.Marshal(cfg)
	metaTag := fmt.Sprintf(`<meta name="p2p-config" content="%s">`, html.EscapeString(string(rawJSON)))
	doc := injectHeadMeta(s.assets.rawIndexHTML, metaTag)
	csp := fmt.Sprintf(
		"default-src 'self'; script-src 'self'; style-src 'self'; img-src 'self' data: blob:; font-src 'self' data:; connect-src 'self'; frame-src %s; frame-ancestors 'none'; object-src 'none'; base-uri 'none'; form-action 'self'",
		s.Policy.SandboxFrameSrc(portOverride),
	)
	return newPreRendered([]byte(doc), "text/html; charset=utf-8", "no-store", csp)
}

func (s *SignalServer) renderBridgeHTML(portOverride string) *preRenderedAsset {
	if s.assets.rawBridgeHTML == "" {
		return nil
	}
	origins := s.Policy.AllowedPortalOriginsList(portOverride)
	rawJSON, _ := json.Marshal(origins)
	metaTag := fmt.Sprintf(`<meta name="p2p-portal-origins" content="%s">`, html.EscapeString(string(rawJSON)))
	doc := injectHeadMeta(s.assets.rawBridgeHTML, metaTag)
	csp := fmt.Sprintf(
		"default-src 'none'; script-src 'self'; connect-src 'self'; frame-ancestors %s; base-uri 'none'; object-src 'none'; form-action 'none'",
		s.Policy.PortalFrameAncestors(portOverride),
	)
	return newPreRendered([]byte(doc), "text/html; charset=utf-8", "no-store", csp)
}

func (s *SignalServer) renderSWJS(portOverride string) *preRenderedAsset {
	if s.assets.rawSWJS == nil {
		return nil
	}
	originsJSON, _ := json.Marshal(s.Policy.AllowedPortalOriginsList(portOverride))
	patchJSON, _ := json.Marshal(s.manifest.PatchJS)
	preamble := fmt.Sprintf(
		"self.__P2P_GW_PORTAL_ORIGINS__=%s;self.__P2P_GW_PATCH_URL__=%s;\n",
		originsJSON,
		patchJSON,
	)
	body := make([]byte, 0, len(preamble)+len(s.assets.rawSWJS))
	body = append(body, preamble...)
	body = append(body, s.assets.rawSWJS...)
	return newPreRendered(
		body,
		"application/javascript; charset=utf-8",
		"private, no-cache",
		"default-src 'none'; script-src 'self'; connect-src 'self'",
	)
}

func assetCacheControl(urlPath string) string {
	// Only content-hashed filenames (e.g. /__p2p_gw__/portal.0123456789.js) are safely immutable;
	// unhashed multi-dot names such as portal.min.js or app.v1.js must remain revalidated.
	base := filepath.Base(urlPath)
	if hashedAssetFilenameRe.MatchString(base) {
		return "public, max-age=31536000, immutable"
	}
	return "no-cache"
}

func isForbiddenWorkerScriptRequest(r *http.Request) bool {
	if strings.EqualFold(strings.TrimSpace(r.Header.Get("Service-Worker")), "script") {
		return true
	}
	switch strings.ToLower(strings.TrimSpace(r.Header.Get("Sec-Fetch-Dest"))) {
	case "serviceworker", "worker", "sharedworker":
		return true
	}
	return false
}

// isValidSWScriptRequest ensures /__p2p_gw__/sw.js is only served to legitimate browser
// Service Worker registration fetches (Service-Worker: script or Sec-Fetch-Dest: serviceworker),
// blocking untrusted sandbox <script> tags or fetch() reads.
func isValidSWScriptRequest(r *http.Request) bool {
	dest := strings.ToLower(strings.TrimSpace(r.Header.Get("Sec-Fetch-Dest")))
	if dest != "" && dest != "serviceworker" {
		return false
	}
	if strings.EqualFold(strings.TrimSpace(r.Header.Get("Service-Worker")), "script") {
		return true
	}
	return dest == "serviceworker"
}

func etagMatches(ifNoneMatch, etag string) bool {
	if ifNoneMatch == "" || etag == "" {
		return false
	}
	for _, part := range strings.Split(ifNoneMatch, ",") {
		tag := strings.TrimSpace(part)
		if tag == "*" || tag == etag || strings.TrimPrefix(tag, "W/") == etag {
			return true
		}
	}
	return false
}

func servePreRendered(w http.ResponseWriter, r *http.Request, a *preRenderedAsset) {
	if a == nil {
		http.NotFound(w, r)
		return
	}
	ct := a.contentType
	if ct == "" {
		ct = mime.TypeByExtension(filepath.Ext(r.URL.Path))
		if ct == "" {
			ct = "application/octet-stream"
		}
	}
	if a.csp != "" {
		w.Header().Set("Content-Security-Policy", a.csp)
	}
	w.Header().Set("Content-Type", ct)
	w.Header().Set("Cache-Control", a.cacheCtrl)
	if a.etag != "" {
		w.Header().Set("ETag", a.etag)
		if etagMatches(r.Header.Get("If-None-Match"), a.etag) {
			w.WriteHeader(http.StatusNotModified)
			return
		}
	}
	w.WriteHeader(http.StatusOK)
	if r.Method == http.MethodGet {
		_, _ = w.Write(a.body)
	}
}

func (s *SignalServer) ephemeralPortOverride(r *http.Request) string {
	_, port, _ := splitHostPortLower(r.Host)
	return port
}

func (s *SignalServer) servePortalAsset(w http.ResponseWriter, r *http.Request, canonPath string) {
	switch canonPath {
	case "/", "/index.html":
		asset := s.assets.indexHTML
		if asset == nil && s.Policy.LocalDevMode && s.Policy.LocalPort == "" {
			asset = s.renderPortalIndex(s.ephemeralPortOverride(r))
		}
		if asset == nil {
			http.Error(w, "Portal index.html not found", http.StatusNotFound)
			return
		}
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("Cross-Origin-Opener-Policy", "same-origin")
		servePreRendered(w, r, asset)
	case s.manifest.PortalJS:
		servePreRendered(w, r, s.assets.portalJS)
	case s.manifest.PortalCSS:
		servePreRendered(w, r, s.assets.portalCSS)
	case "/__p2p_gw__/favicon.svg", "/favicon.svg":
		w.Header().Set("X-Frame-Options", "DENY")
		servePreRendered(w, r, s.assets.faviconSVG)
	default:
		// Strict Zero-SW air-gap on Portal origin: /sw.js, /__p2p_gw__/sw.js, /__p2p_gw__/bridge.html etc. MUST 404.
		http.NotFound(w, r)
	}
}

func (s *SignalServer) serveSandboxAsset(w http.ResponseWriter, r *http.Request, canonPath string) {
	switch canonPath {
	case s.manifest.SWJS:
		w.Header().Set("Cross-Origin-Resource-Policy", "same-origin")
		w.Header().Set("Vary", "Service-Worker, Sec-Fetch-Dest")
		if !isValidSWScriptRequest(r) {
			http.Error(w, "Forbidden: sw.js requires a Service Worker registration request", http.StatusForbidden)
			return
		}
		asset := s.assets.swJS
		if asset == nil && s.Policy.LocalDevMode && s.Policy.LocalPort == "" {
			asset = s.renderSWJS(s.ephemeralPortOverride(r))
		}
		if asset == nil {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Service-Worker-Allowed", "/")
		servePreRendered(w, r, asset)
	case "/__p2p_gw__/bridge.html":
		asset := s.assets.bridgeHTML
		if asset == nil && s.Policy.LocalDevMode && s.Policy.LocalPort == "" {
			asset = s.renderBridgeHTML(s.ephemeralPortOverride(r))
		}
		if asset == nil {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Cross-Origin-Opener-Policy", "same-origin")
		servePreRendered(w, r, asset)
	case s.manifest.BridgeJS:
		w.Header().Set("Vary", "Service-Worker, Sec-Fetch-Dest")
		if isForbiddenWorkerScriptRequest(r) {
			http.Error(w, "Forbidden: bridge script cannot be registered as a Service Worker", http.StatusForbidden)
			return
		}
		servePreRendered(w, r, s.assets.bridgeJS)
	case s.manifest.PatchJS:
		w.Header().Set("Vary", "Service-Worker, Sec-Fetch-Dest")
		if isForbiddenWorkerScriptRequest(r) {
			http.Error(w, "Forbidden: patch script cannot be registered as a Service Worker", http.StatusForbidden)
			return
		}
		servePreRendered(w, r, s.assets.patchJS)
	default:
		http.NotFound(w, r)
	}
}

// indexASCIILower finds ASCII-only needle (lowercase) inside s without allocating or altering UTF-8 byte offsets.
func indexASCIILower(s, needle string) int {
	n := len(needle)
	if n == 0 || len(s) < n {
		return -1
	}
	for i := 0; i <= len(s)-n; i++ {
		match := true
		for j := 0; j < n; j++ {
			c := s[i+j]
			if c >= 'A' && c <= 'Z' {
				c += 32
			}
			if c != needle[j] {
				match = false
				break
			}
		}
		if match {
			return i
		}
	}
	return -1
}

func injectHeadMeta(htmlDoc, metaTag string) string {
	if idx := indexASCIILower(htmlDoc, "<head>"); idx >= 0 {
		insertAt := idx + len("<head>")
		return htmlDoc[:insertAt] + "\n  " + metaTag + htmlDoc[insertAt:]
	}
	return metaTag + "\n" + htmlDoc
}
