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

	// UTF-8 BOM YAML must load cleanly.
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

	// JSON is valid YAML 1.2 and must decode through the same strict path.
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

	// Unknown field must fail closed.
	badPath := filepath.Join(dir, "bad.yaml")
	if err := os.WriteFile(badPath, []byte("host: ok\ntypo_field: true\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := LoadYAMLFile(badPath, &got); err == nil {
		t.Fatal("expected unknown YAML field to fail, got nil")
	}

	// Multi-document YAML must fail closed.
	multiPath := filepath.Join(dir, "multi.yaml")
	if err := os.WriteFile(multiPath, []byte("host: one\n---\nhost: two\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := LoadYAMLFile(multiPath, &got); err == nil {
		t.Fatal("expected multi-document YAML to fail, got nil")
	}
}

func TestFlagScanning_IgnoresFlagValues(t *testing.T) {
	valueFlags := map[string]bool{
		"access-secret": true,
		"signal-secret": true,
		"config":        true,
	}

	// Negative test: `-access-secret -v` must not treat the value `-v` as a boolean flag.
	if HasBoolFlag([]string{"-access-secret", "-v", "-signal-secret", "-h"}, []string{"v", "version", "h", "help"}, valueFlags) {
		t.Fatal("flag values `-v` / `-h` were falsely treated as boolean flags")
	}
	if !HasBoolFlag([]string{"-access-secret", "secret", "-v"}, []string{"v", "version"}, valueFlags) {
		t.Fatal("expected real `-v` flag to be detected")
	}
	if HasBoolFlag([]string{"-version=false"}, []string{"v", "version"}, valueFlags) {
		t.Fatal("expected `-version=false` to evaluate to false")
	}
	if !HasBoolFlag([]string{"-version=true"}, []string{"v", "version"}, valueFlags) {
		t.Fatal("expected `-version=true` to evaluate to true")
	}
	if HasBoolFlag([]string{"--", "-v"}, []string{"v"}, valueFlags) {
		t.Fatal("expected `--` terminator to stop flag scanning")
	}

	// `-access-secret -config` must not treat `-config` as the config flag.
	if _, ok := FindFlagValue([]string{"-access-secret", "-config"}, "config", valueFlags); ok {
		t.Fatal("flag value `-config` was falsely matched as config flag")
	}
	if v, ok := FindFlagValue([]string{"-access-secret", "pw", "-config", "/etc/p2p/agent.yaml"}, "config", valueFlags); !ok || v != "/etc/p2p/agent.yaml" {
		t.Fatalf("expected /etc/p2p/agent.yaml, got (%q, %v)", v, ok)
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
	if p := ResolveExplicitConfigPath(nil, map[string]bool{"config": true}); p != "" {
		t.Fatalf("must not autoload CWD config file, got %q", p)
	}
	t.Setenv("CONFIG_FILE", "/etc/p2p/agent.yaml")
	if p := ResolveExplicitConfigPath(nil, map[string]bool{"config": true}); p != "/etc/p2p/agent.yaml" {
		t.Fatalf("expected CONFIG_FILE path, got %q", p)
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

func TestFlagScannerBoundaries(t *testing.T) {
	vflags := map[string]bool{"config": true, "id": true}

	// Must stop at "--" and not consume trailing arguments as flags.
	if val, ok := FindFlagValue([]string{"-id", "nas", "--", "-config", "/etc/evil.yaml"}, "config", vflags); ok || val != "" {
		t.Fatalf("FindFlagValue crossed -- terminator: got (%q, %v)", val, ok)
	}
	if HasBoolFlag([]string{"--", "-version"}, []string{"v", "version"}, vflags) {
		t.Fatal("HasBoolFlag crossed -- terminator")
	}

	// Must stop at first positional argument (matching Go flag.FlagSet).
	if val, ok := FindFlagValue([]string{"positional", "-config", "/tmp/a.yaml"}, "config", vflags); ok || val != "" {
		t.Fatalf("FindFlagValue crossed positional arg: got (%q, %v)", val, ok)
	}

	// Must reject triple-dash ---config.
	if val, ok := FindFlagValue([]string{"---config=/tmp/a.yaml"}, "config", vflags); ok || val != "" {
		t.Fatalf("FindFlagValue accepted ---config: got (%q, %v)", val, ok)
	}

	// Must use the last occurrence of -config (matching Go flag.FlagSet).
	if val, ok := FindFlagValue([]string{"-config", "/tmp/first.yaml", "-config=/tmp/second.yaml"}, "config", vflags); !ok || val != "/tmp/second.yaml" {
		t.Fatalf("expected last -config to win, got (%q, %v)", val, ok)
	}
}
