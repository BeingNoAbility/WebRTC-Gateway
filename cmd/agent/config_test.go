package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestAgentConfig_FileParsingAndStringOrSlice(t *testing.T) {
	// Test YAML with list of STUN servers and comments
	yamlContent := `
# Agent configuration for NAS
node_id: "test-nas"
signal: "wss://signal.example.com/ws"
signal_secret: "cluster-token-123"
access_secret: "guest-password-456"
target: "http://127.0.0.1:5000"
max_streams: 1024
stun_servers:
  - "stun.qq.com:3478"
  - "stun.miwifi.com:3478"
`
	tmpYAML, err := os.CreateTemp("", "agent-*.yaml")
	if err != nil {
		t.Fatalf("Failed to create temp yaml: %v", err)
	}
	defer os.Remove(tmpYAML.Name())
	_, _ = tmpYAML.WriteString(yamlContent)
	_ = tmpYAML.Close()

	cfg, err := LoadAgentConfigFile(tmpYAML.Name())
	if err != nil {
		t.Fatalf("Failed to load agent config YAML: %v", err)
	}

	if cfg.NodeID != "test-nas" {
		t.Errorf("Expected node_id test-nas, got %q", cfg.NodeID)
	}
	if cfg.SignalSecret != "cluster-token-123" {
		t.Errorf("Expected signal_secret cluster-token-123, got %q", cfg.SignalSecret)
	}
	if cfg.AccessSecret != "guest-password-456" {
		t.Errorf("Expected access_secret guest-password-456, got %q", cfg.AccessSecret)
	}
	if cfg.MaxStreams != 1024 {
		t.Errorf("Expected max_streams 1024, got %d", cfg.MaxStreams)
	}
	if len(cfg.StunServers) != 2 || cfg.StunServers[0] != "stun.qq.com:3478" {
		t.Errorf("Expected 2 stun servers parsed, got: %v", cfg.StunServers)
	}

	// Test JSON with comma-separated string STUN servers
	jsonContent := `{
		"node_id": "json-nas",
		"signal_secret": "json-sig-secret",
		"access_secret": "",
		"stun_servers": "stun1.example.com:3478, stun2.example.com:3478"
	}`
	tmpJSON, err := os.CreateTemp("", "agent-*.json")
	if err != nil {
		t.Fatalf("Failed to create temp json: %v", err)
	}
	defer os.Remove(tmpJSON.Name())
	_, _ = tmpJSON.WriteString(jsonContent)
	_ = tmpJSON.Close()

	cfgJSON, err := LoadAgentConfigFile(tmpJSON.Name())
	if err != nil {
		t.Fatalf("Failed to load agent config JSON: %v", err)
	}
	if cfgJSON.NodeID != "json-nas" {
		t.Errorf("Expected node_id json-nas, got %q", cfgJSON.NodeID)
	}
	if len(cfgJSON.StunServers) != 2 || cfgJSON.StunServers[1] != "stun2.example.com:3478" {
		t.Errorf("Expected 2 stun servers from comma-separated string, got: %v", cfgJSON.StunServers)
	}
}

func TestAgent_FindConfigFileDetection(t *testing.T) {
	// 1. Explicit CLI arg: -config <path>
	path, explicit := FindConfigFile([]string{"-node-id", "abc", "-config", "/etc/test.yaml"}, []string{"agent.yaml"})
	if path != "/etc/test.yaml" || !explicit {
		t.Errorf("Expected explicit path /etc/test.yaml, got %q (explicit=%v)", path, explicit)
	}

	// 2. Explicit CLI arg: --config=<path>
	path, explicit = FindConfigFile([]string{"--config=/etc/test2.yaml"}, []string{"agent.yaml"})
	if path != "/etc/test2.yaml" || !explicit {
		t.Errorf("Expected explicit path /etc/test2.yaml, got %q (explicit=%v)", path, explicit)
	}

	// 3. Fallback to candidate file in temp dir
	tmpDir, err := os.MkdirTemp("", "cand-test-*")
	if err != nil {
		t.Fatalf("MkdirTemp failed: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	candPath := filepath.Join(tmpDir, "agent.yaml")
	_ = os.WriteFile(candPath, []byte("node_id: cand-nas"), 0644)

	origWd, _ := os.Getwd()
	_ = os.Chdir(tmpDir)
	defer os.Chdir(origWd)

	path, explicit = FindConfigFile([]string{}, []string{"agent.yaml", "agent.json"})
	if !strings.Contains(path, "agent.yaml") || explicit {
		t.Errorf("Expected implicit candidate path, got %q (explicit=%v)", path, explicit)
	}
}

func TestAgent_FailFastDeprecatedSecret(t *testing.T) {
	// Build or run binary with -secret and verify it exits with error and deprecation message
	cmd := exec.Command("go", "run", ".", "-secret", "old-pass")
	cmd.Dir = "."
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Errorf("Expected command to fail on -secret, but it succeeded: %s", string(out))
	}
	if !strings.Contains(string(out), "flag -secret has been deprecated and removed") {
		t.Errorf("Expected deprecation message in output, got:\n%s", string(out))
	}
}

func TestAgentConfig_DirAndFileParsing(t *testing.T) {
	yamlContent := `
node_id: "file-server-node"
signal: "wss://signal.example.com/ws"
dir: "/data/www"
file: "/data/archive.zip"
`
	tmpYAML, err := os.CreateTemp("", "agent-file-*.yaml")
	if err != nil {
		t.Fatalf("Failed to create temp yaml: %v", err)
	}
	defer os.Remove(tmpYAML.Name())
	_, _ = tmpYAML.WriteString(yamlContent)
	_ = tmpYAML.Close()

	cfg, err := LoadAgentConfigFile(tmpYAML.Name())
	if err != nil {
		t.Fatalf("Failed to load agent config YAML: %v", err)
	}

	if cfg.Dir != "/data/www" {
		t.Errorf("Expected dir /data/www, got %q", cfg.Dir)
	}
	if cfg.File != "/data/archive.zip" {
		t.Errorf("Expected file /data/archive.zip, got %q", cfg.File)
	}
}

func TestAgent_ConflictingModesFailFast(t *testing.T) {
	cmd := exec.Command("go", "run", ".", "-target", "http://127.0.0.1:8080", "-file", "/some/file.zip")
	cmd.Dir = "."
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Errorf("Expected command to fail on conflicting -target and -file, but it succeeded: %s", string(out))
	}
	if !strings.Contains(string(out), "Conflicting modes: cannot specify more than one") {
		t.Errorf("Expected conflicting modes error message, got:\n%s", string(out))
	}
}

func TestAgent_HelpAndVersionFlags(t *testing.T) {
	// 1. Test -h
	cmd := exec.Command("go", "run", ".", "-h")
	cmd.Dir = "."
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Errorf("Expected agent -h to exit 0, got err: %v\nOutput: %s", err, string(out))
	}
	output := string(out)
	if !strings.Contains(output, "WebRTC Gateway Agent") || !strings.Contains(output, "Operational Modes") || !strings.Contains(output, "-allowed-proxy-domains") {
		t.Errorf("Unexpected help output for agent -h:\n%s", output)
	}

	// 2. Test help subcommand
	cmd = exec.Command("go", "run", ".", "help")
	cmd.Dir = "."
	out, err = cmd.CombinedOutput()
	if err != nil {
		t.Errorf("Expected agent help to exit 0, got err: %v\nOutput: %s", err, string(out))
	}
	if !strings.Contains(string(out), "Operational Modes") {
		t.Errorf("Unexpected help output for agent help:\n%s", string(out))
	}

	// 3. Test -v and --version
	for _, flagArg := range []string{"-v", "--version"} {
		cmd = exec.Command("go", "run", ".", flagArg)
		cmd.Dir = "."
		out, err = cmd.CombinedOutput()
		if err != nil {
			t.Errorf("Expected agent %s to exit 0, got err: %v\nOutput: %s", flagArg, err, string(out))
		}
		if !strings.Contains(string(out), "WebRTC Gateway Agent v") {
			t.Errorf("Expected version string for %s, got:\n%s", flagArg, string(out))
		}
	}
}
