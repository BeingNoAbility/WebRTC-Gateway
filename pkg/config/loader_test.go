package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadYAMLFile_BOMAndKnownFields(t *testing.T) {
	type sample struct {
		Host string `yaml:"host"`
		Port int    `yaml:"port"`
	}
	dir := t.TempDir()

	bomPath := filepath.Join(dir, "bom.yaml")
	if err := os.WriteFile(bomPath, append([]byte{0xEF, 0xBB, 0xBF}, []byte("host: example.com\nport: 8443\n")...), 0600); err != nil {
		t.Fatal(err)
	}
	var got sample
	if err := LoadYAMLFile(bomPath, &got); err != nil {
		t.Fatalf("expected BOM YAML to parse, got %v", err)
	}
	if got.Host != "example.com" || got.Port != 8443 {
		t.Fatalf("unexpected decoded value: %+v", got)
	}

	jsonPath := filepath.Join(dir, "cfg.json")
	if err := os.WriteFile(jsonPath, []byte(`{"host":"127.0.0.1","port":8888}`), 0600); err != nil {
		t.Fatal(err)
	}
	got = sample{}
	if err := LoadYAMLFile(jsonPath, &got); err != nil {
		t.Fatalf("expected JSON to parse, got %v", err)
	}
	if got.Host != "127.0.0.1" || got.Port != 8888 {
		t.Fatalf("unexpected JSON decode: %+v", got)
	}

	badPath := filepath.Join(dir, "bad.yaml")
	if err := os.WriteFile(badPath, []byte("host: ok\ntypo_field: true\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := LoadYAMLFile(badPath, &got); err == nil {
		t.Fatal("expected unknown YAML field to fail, got nil")
	}

	multiPath := filepath.Join(dir, "multi.yaml")
	if err := os.WriteFile(multiPath, []byte("host: one\n---\nhost: two\n"), 0600); err == nil {
		if err := LoadYAMLFile(multiPath, &got); err == nil {
			t.Fatal("expected multi-document YAML to fail, got nil")
		}
	}
}

func TestResolveExplicitConfigPath_NeverAutoloadsCWD(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "agent.yaml"), []byte("allow_public: true\n"), 0600); err != nil {
		t.Fatal(err)
	}
	oldWD, _ := os.Getwd()
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(oldWD) })

	t.Setenv("CONFIG_FILE", "")
	if p := ResolveExplicitConfigPath(""); p != "" {
		t.Fatalf("must not autoload CWD config file, got %q", p)
	}
	t.Setenv("CONFIG_FILE", "/etc/p2p/agent.yaml")
	if p := ResolveExplicitConfigPath(""); p != "/etc/p2p/agent.yaml" {
		t.Fatalf("expected CONFIG_FILE path, got %q", p)
	}
	if p := ResolveExplicitConfigPath("/cli/override.yaml"); p != "/cli/override.yaml" {
		t.Fatalf("expected CLI override path, got %q", p)
	}
}

func TestResolveHelpers_Precedence(t *testing.T) {
	t.Setenv("TEST_STR", "from-env")
	if got := ResolveString("from-cli", true, "TEST_STR", "from-cfg", "default"); got != "from-cli" {
		t.Fatalf("expected CLI to win, got %q", got)
	}
	if got := ResolveString("", false, "TEST_STR", "from-cfg", "default"); got != "from-env" {
		t.Fatalf("expected Env to win, got %q", got)
	}
	t.Setenv("TEST_STR", "")
	if got := ResolveString("", false, "TEST_STR", "from-cfg", "default"); got != "from-cfg" {
		t.Fatalf("expected YAML to win, got %q", got)
	}
	if got := ResolveString("", false, "TEST_STR", "", "default"); got != "default" {
		t.Fatalf("expected default, got %q", got)
	}

	falsePtr := false
	t.Setenv("TEST_BOOL_P", "true")
	if v, set, err := ResolveBool(false, true, "TEST_BOOL_P", &falsePtr, true, true); err != nil || !set || v {
		t.Fatalf("expected CLI false override, got (%v, %v, %v)", v, set, err)
	}
	if v, set, err := ResolveBool(false, false, "TEST_BOOL_P", &falsePtr, false, false); err != nil || !set || !v {
		t.Fatalf("expected Env true override, got (%v, %v, %v)", v, set, err)
	}
	t.Setenv("TEST_BOOL_P", "")
	if v, set, err := ResolveBool(false, false, "TEST_BOOL_P", &falsePtr, false, true); err != nil || !set || v {
		t.Fatalf("expected YAML *bool=false to win over default true, got (%v, %v, %v)", v, set, err)
	}

	t.Setenv("TEST_INT_P", "")
	if _, err := ResolveInt(0, true, "TEST_INT_P", 10, 64, 1, 100); err == nil {
		t.Fatal("expected out-of-range CLI int to fail")
	}
	if n, err := ResolveInt(0, false, "TEST_INT_P", 0, 64, 1, 100); err != nil || n != 64 {
		t.Fatalf("expected default 64, got (%d, %v)", n, err)
	}
}

func TestResolveSecret_FileEnvPriority(t *testing.T) {
	dir := t.TempDir()
	secFile := filepath.Join(dir, "secret.txt")
	if err := os.WriteFile(secFile, []byte("super-secret-value\r\n"), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TEST_SECRET_FILE", secFile)
	t.Setenv("TEST_SECRET", "env-value")

	val, fromCLI, err := ResolveSecret("", false, "TEST_SECRET", "TEST_SECRET_FILE", "cfg-value")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if val != "super-secret-value" || fromCLI {
		t.Fatalf("expected file secret with fromCLI=false, got (%q, %v)", val, fromCLI)
	}

	val, fromCLI, err = ResolveSecret("cli-value", true, "TEST_SECRET", "TEST_SECRET_FILE", "cfg-value")
	if err != nil || val != "cli-value" || !fromCLI {
		t.Fatalf("expected CLI override with fromCLI=true, got (%q, %v, %v)", val, fromCLI, err)
	}

	emptyFile := filepath.Join(dir, "empty.txt")
	if err := os.WriteFile(emptyFile, []byte("\r\n"), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TEST_SECRET_FILE", emptyFile)
	if _, _, err := ResolveSecret("", false, "TEST_SECRET", "TEST_SECRET_FILE", "cfg-value"); err == nil {
		t.Fatal("expected empty secret file to fail closed")
	}
}

func TestParseEnvStrict(t *testing.T) {
	t.Setenv("TEST_BOOL", "ture")
	if _, _, err := ParseEnvBool("TEST_BOOL"); err == nil {
		t.Fatal("expected typo boolean `ture` to fail fast")
	}
	t.Setenv("TEST_BOOL", "yes")
	if b, set, err := ParseEnvBool("TEST_BOOL"); err != nil || !set || !b {
		t.Fatalf("expected (true, true, nil), got (%v, %v, %v)", b, set, err)
	}

	t.Setenv("TEST_INT", "0")
	if _, _, err := ParseEnvInt("TEST_INT", 1, 100); err == nil {
		t.Fatal("expected out-of-range integer to fail fast")
	}
	t.Setenv("TEST_INT", "42")
	if n, set, err := ParseEnvInt("TEST_INT", 1, 100); err != nil || !set || n != 42 {
		t.Fatalf("expected (42, true, nil), got (%v, %v, %v)", n, set, err)
	}

	got := SplitNonEmptyCSV(" stun:a:3478 , , stun:b:3478 ")
	if len(got) != 2 || got[0] != "stun:a:3478" || got[1] != "stun:b:3478" {
		t.Fatalf("unexpected CSV split: %#v", got)
	}
}
