package main

import (
	"os"
	"path/filepath"
	"testing"

	"webrtc-gateway/pkg/config"
)

func TestServerFileConfigStrictYAML(t *testing.T) {
	dir := t.TempDir()
	validPath := filepath.Join(dir, "server.yaml")
	validYAML := "addr: 127.0.0.1:8888\ndev: true\nallow_anonymous_agents: true\nallow_force_rekey: false\n"
	if err := os.WriteFile(validPath, []byte(validYAML), 0600); err != nil {
		t.Fatal(err)
	}
	var cfg serverFileConfig
	if err := config.LoadYAMLFile(validPath, &cfg); err != nil {
		t.Fatalf("valid server YAML failed: %v", err)
	}
	if cfg.Addr != "127.0.0.1:8888" || !cfg.DevMode || !cfg.AllowAnonymousAgents || cfg.AllowForceRekey == nil || *cfg.AllowForceRekey {
		t.Fatalf("unexpected parsed serverFileConfig: %+v", cfg)
	}

	badPath := filepath.Join(dir, "bad.yaml")
	if err := os.WriteFile(badPath, []byte("unknown_field: 123\n"), 0600); err != nil {
		t.Fatal(err)
	}
	var badCfg serverFileConfig
	if err := config.LoadYAMLFile(badPath, &badCfg); err == nil {
		t.Fatal("expected strict YAML loader to reject unknown field")
	}
}
