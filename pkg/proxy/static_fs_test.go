package proxy

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestStaticFSStepwiseRootAndWindowsSymlink(t *testing.T) {
	dir := t.TempDir()
	outside := t.TempDir()

	if err := os.MkdirAll(filepath.Join(dir, "sub", "nested"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "sub", "nested", "ok.txt"), []byte("hello-stepwise"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".secret"), []byte("top-secret"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(outside, "escape.txt"), []byte("escaped"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Create symlinks covering: valid dir symlink, escaping symlink, dotfile symlink, Windows backslash & drive targets.
	if err := os.Symlink("sub", filepath.Join(dir, "linkdir")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(outside, "escape.txt"), filepath.Join(dir, "escape_link")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(".secret", filepath.Join(dir, "dot_link")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(`..\secret`, filepath.Join(dir, "win_backslash")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(`C:secret.txt`, filepath.Join(dir, "win_drive")); err != nil {
		t.Fatal(err)
	}

	// Default Fail-Closed mode (FollowSymlinks: false) MUST reject ALL symlinks (including in-root linkdir).
	hDefault, err := NewStaticFSHandler(StaticFSOptions{
		RootDir:      dir,
		HideDotfiles: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer hDefault.Close()
	recDef := httptest.NewRecorder()
	hDefault.ServeHTTP(recDef, httptest.NewRequest("GET", "/linkdir/nested/ok.txt", nil))
	if recDef.Code == http.StatusOK {
		t.Fatalf("default Fail-Closed O_NOFOLLOW mode must reject symlinks, got 200")
	}

	// Opt-in FollowSymlinks: true tests stepwise symlink validation + escaping/dotfile/Windows rejection.
	h, err := NewStaticFSHandler(StaticFSOptions{
		RootDir:        dir,
		HideDotfiles:   true,
		FollowSymlinks: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()

	// 1. Deep stepwise OpenRoot path succeeds.
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/sub/nested/ok.txt", nil))
	if rec.Code != http.StatusOK || rec.Body.String() != "hello-stepwise" {
		t.Fatalf("expected 200 hello-stepwise, got %d %q", rec.Code, rec.Body.String())
	}

	// 2. Windows backslash / drive-letter / escaping / dotfile symlinks MUST all fail closed!
	for _, p := range []string{"/win_backslash", "/win_drive", "/escape_link", "/dot_link", "/.secret"} {
		rec = httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest("GET", p, nil))
		if rec.Code == http.StatusOK {
			t.Fatalf("expected forbidden/404 for %s, got 200", p)
		}
	}

	// 3. Directory listing MUST hide escaping, dotfile, and Windows symlinks, and append '/' to dir symlink!
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 on dir listing, got %d", rec.Code)
	}
	body := rec.Body.String()
	for _, forbidden := range []string{"escape_link", "dot_link", "win_backslash", "win_drive", ".secret"} {
		if strings.Contains(body, forbidden) {
			t.Fatalf("dir listing leaked disallowed entry %q: %s", forbidden, body)
		}
	}
	if !strings.Contains(body, `href="./linkdir/"`) {
		t.Fatalf("dir listing did not mark valid directory symlink with trailing slash: %s", body)
	}

	// 4. 405 Method Not Allowed MUST include Allow: GET, HEAD.
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/", nil))
	if rec.Code != http.StatusMethodNotAllowed || rec.Header().Get("Allow") != "GET, HEAD" {
		t.Fatalf("expected 405 with Allow: GET, HEAD, got %d Allow=%q", rec.Code, rec.Header().Get("Allow"))
	}

	// 5. Single-file mode: allows explicit dotfile AND serves ONLY '/' or '/<basename>'.
	dotPath := filepath.Join(dir, ".explicit.txt")
	if err := os.WriteFile(dotPath, []byte("explicit-dot"), 0o644); err != nil {
		t.Fatal(err)
	}
	sf, err := NewStaticFSHandler(StaticFSOptions{
		SingleFile:   dotPath,
		HideDotfiles: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer sf.Close()

	for _, p := range []string{"/", "/.explicit.txt"} {
		rec = httptest.NewRecorder()
		sf.ServeHTTP(rec, httptest.NewRequest("GET", p, nil))
		if rec.Code != http.StatusOK || rec.Body.String() != "explicit-dot" {
			t.Fatalf("single-file %s failed: %d %q", p, rec.Code, rec.Body.String())
		}
	}
	rec = httptest.NewRecorder()
	sf.ServeHTTP(rec, httptest.NewRequest("GET", "/other.txt", nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("single-file unmatched path /other.txt must return 404, got %d", rec.Code)
	}

	// 6. P2-20: Directory symlink redirect uses virtual request path (never leaks resolved target),
	//    and trailing slash on regular file returns 404 (not 200).
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/linkdir?q=1", nil))
	if rec.Code != http.StatusMovedPermanently || rec.Header().Get("Location") != "/linkdir/?q=1" {
		t.Fatalf("P2-20 regression: symlink dir redirect leaked target or wrong location: code=%d loc=%q", rec.Code, rec.Header().Get("Location"))
	}
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/sub/nested/ok.txt/", nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("P2-20 regression: trailing slash on regular file must return 404, got %d", rec.Code)
	}
}
