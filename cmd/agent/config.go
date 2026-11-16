package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

// StringOrSlice allows a configuration field to be specified either as a comma-separated
// string ("stun1,stun2") or as a YAML/JSON list of strings (["stun1", "stun2"]).
type StringOrSlice []string

func (s *StringOrSlice) UnmarshalYAML(value *yaml.Node) error {
	if value.Kind == yaml.ScalarNode {
		parts := strings.Split(value.Value, ",")
		var res []string
		for _, p := range parts {
			if trimmed := strings.TrimSpace(p); trimmed != "" {
				res = append(res, trimmed)
			}
		}
		*s = res
		return nil
	}
	var slice []string
	if err := value.Decode(&slice); err != nil {
		return err
	}
	*s = slice
	return nil
}

func (s *StringOrSlice) UnmarshalJSON(data []byte) error {
	var str string
	if err := json.Unmarshal(data, &str); err == nil {
		parts := strings.Split(str, ",")
		var res []string
		for _, p := range parts {
			if trimmed := strings.TrimSpace(p); trimmed != "" {
				res = append(res, trimmed)
			}
		}
		*s = res
		return nil
	}
	var slice []string
	if err := json.Unmarshal(data, &slice); err != nil {
		return err
	}
	*s = slice
	return nil
}

func (s StringOrSlice) String() string {
	return strings.Join(s, ",")
}

// AgentConfigFile represents the deserialized structure of an agent configuration file (YAML or JSON).
type AgentConfigFile struct {
	NodeID       string        `json:"node_id" yaml:"node_id"`
	SignalURL    string        `json:"signal" yaml:"signal"`
	SignalSecret string        `json:"signal_secret" yaml:"signal_secret"`
	AccessSecret string        `json:"access_secret" yaml:"access_secret"`
	TargetURL    string        `json:"target" yaml:"target"`
	Dir          string        `json:"dir" yaml:"dir"`
	File         string        `json:"file" yaml:"file"`
	ProxyURL     string        `json:"proxy" yaml:"proxy"`
	SNI          string        `json:"sni" yaml:"sni"`
	Insecure     *bool         `json:"insecure" yaml:"insecure"`
	TurnURL      string        `json:"turn_url" yaml:"turn_url"`
	TurnUser     string        `json:"turn_user" yaml:"turn_user"`
	TurnPass     string        `json:"turn_pass" yaml:"turn_pass"`
	StunServers  StringOrSlice `json:"stun_servers" yaml:"stun_servers"`
	MaxStreams   int           `json:"max_streams" yaml:"max_streams"`
	MetricsAddr  string        `json:"metrics_addr" yaml:"metrics_addr"`
	PortalURL           string        `json:"portal_url" yaml:"portal_url"`
	AllowedProxyDomains StringOrSlice `json:"allowed_proxy_domains" yaml:"allowed_proxy_domains"`
}

// LoadAgentConfigFile parses an agent configuration file (YAML or JSON).
func LoadAgentConfigFile(path string) (*AgentConfigFile, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var cfg AgentConfigFile
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("parse config %s: %w", path, err)
	}
	return &cfg, nil
}

// FindConfigFile checks if a config path was explicitly provided via CLI/ENV,
// or performs smart auto-detection for default candidate file names.
func FindConfigFile(args []string, defaultCandidates []string) (path string, explicit bool) {
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
