package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

// ServerConfigFile represents the deserialized structure of a signaling server configuration file (YAML or JSON).
type ServerConfigFile struct {
	Addr           string `json:"addr" yaml:"addr"`
	SignalSecret   string `json:"signal_secret" yaml:"signal_secret"`
	StaticDir      string `json:"static" yaml:"static"`
	TLSCert        string `json:"tls_cert" yaml:"tls_cert"`
	TLSKey         string `json:"tls_key" yaml:"tls_key"`
	TLSAuto        *bool  `json:"tls_auto" yaml:"tls_auto"`
	MaxIPConns     int    `json:"max_ip_conns" yaml:"max_ip_conns"`
	TrustedProxies string `json:"trusted_proxies" yaml:"trusted_proxies"`
	MetricsAddr    string `json:"metrics_addr" yaml:"metrics_addr"`
}

// LoadServerConfigFile parses a signaling server configuration file (YAML or JSON).
func LoadServerConfigFile(path string) (*ServerConfigFile, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var cfg ServerConfigFile
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("parse server config %s: %w", path, err)
	}
	return &cfg, nil
}

// FindServerConfigFile checks if a config path was explicitly provided via CLI/ENV,
// or performs smart auto-detection for default candidate file names.
func FindServerConfigFile(args []string, defaultCandidates []string) (path string, explicit bool) {
	// 1. Check CLI args: -config <path>, --config <path>, -config=<path>, --config=<path>
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if arg == "-config" || arg == "--config" {
			if i+1 < len(args) {
				return args[i+1], true
			}
		} else if strings.HasPrefix(arg, "-config=") {
			return strings.TrimPrefix(arg, "-config="), true
		} else if strings.HasPrefix(arg, "--config=") {
			return strings.TrimPrefix(arg, "--config="), true
		}
	}

	// 2. Check ENV: CONFIG_FILE or CONFIG
	if env := os.Getenv("CONFIG_FILE"); env != "" {
		return env, true
	}
	if env := os.Getenv("CONFIG"); env != "" {
		return env, true
	}

	// 3. Auto-detect default candidates in cwd and executable dir
	dirs := []string{"."}
	if exe, err := os.Executable(); err == nil {
		exeDir := filepath.Dir(exe)
		if exeDir != "." && exeDir != "" {
			dirs = append(dirs, exeDir)
		}
	}

	for _, dir := range dirs {
		for _, cand := range defaultCandidates {
			target := filepath.Join(dir, cand)
			if fi, err := os.Stat(target); err == nil && !fi.IsDir() {
				return target, false
			}
		}
	}

	return "", false
}
