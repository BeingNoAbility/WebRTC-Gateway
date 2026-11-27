package config

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"go.yaml.in/yaml/v3"
)

var utf8BOM = []byte{0xEF, 0xBB, 0xBF}

// LoadYAMLFile reads a single-document YAML or JSON configuration file (stripping any
// leading UTF-8 BOM) and decodes it strictly with KnownFields(true) into dst.
// Multi-document YAML streams are rejected Fail-Closed.
func LoadYAMLFile(path string, dst any) error {
	raw, err := os.ReadFile(filepath.Clean(path)) // #nosec G304 -- explicit operator-configured CLI config file path
	if err != nil {
		return fmt.Errorf("read config %q: %w", path, err)
	}
	raw = bytes.TrimPrefix(raw, utf8BOM)
	if len(bytes.TrimSpace(raw)) == 0 {
		return nil
	}
	dec := yaml.NewDecoder(bytes.NewReader(raw))
	dec.KnownFields(true)
	if err := dec.Decode(dst); err != nil {
		return fmt.Errorf("parse config %q: %w", path, err)
	}
	// Reject trailing YAML documents instead of silently ignoring them.
	var extra any
	if err := dec.Decode(&extra); err != nil && !errors.Is(err, io.EOF) {
		return fmt.Errorf("parse config %q: unexpected trailing content: %w", path, err)
	} else if err == nil {
		return fmt.Errorf("parse config %q: multiple YAML documents are not allowed", path)
	}
	return nil
}

// ResolveExplicitConfigPath resolves a config file ONLY from an explicit parsed CLI flag
// or the CONFIG_FILE environment variable. It NEVER auto-loads from CWD (Fail-Closed).
func ResolveExplicitConfigPath(cliConfigVal string) string {
	if v := strings.TrimSpace(cliConfigVal); v != "" {
		return v
	}
	if envPath := strings.TrimSpace(os.Getenv("CONFIG_FILE")); envPath != "" {
		return envPath
	}
	return ""
}

// ResolveString merges configuration with strict priority: CLI flag > Env > YAML > defaultVal.
func ResolveString(cliVal string, cliSet bool, envName, cfgVal, defaultVal string) string {
	if cliSet {
		return cliVal
	}
	if envName != "" {
		if v, ok := os.LookupEnv(envName); ok && v != "" {
			return v
		}
	}
	if cfgVal != "" {
		return cfgVal
	}
	return defaultVal
}

// ResolveBool merges a boolean flag with strict priority: CLI flag > Env > YAML (*bool or bool) > defaultVal.
// Returns (value, explicitlySet, err).
func ResolveBool(cliVal, cliSet bool, envName string, cfgPtr *bool, cfgBool, defaultVal bool) (bool, bool, error) {
	if cliSet {
		return cliVal, true, nil
	}
	if envName != "" {
		b, set, err := ParseEnvBool(envName)
		if err != nil {
			return false, true, err
		}
		if set {
			return b, true, nil
		}
	}
	if cfgPtr != nil {
		return *cfgPtr, true, nil
	}
	if cfgBool {
		return true, true, nil
	}
	return defaultVal, false, nil
}

// ResolveInt merges an integer flag with strict priority: CLI flag > Env > YAML > defaultVal,
// validating minVal <= v <= maxVal (when maxVal > 0) whenever an explicit value is supplied.
func ResolveInt(cliVal int, cliSet bool, envName string, cfgVal, defaultVal, minVal, maxVal int) (int, error) {
	var val int
	var set bool
	switch {
	case cliSet:
		val = cliVal
		set = true
	case envName != "":
		n, envSet, err := ParseEnvInt(envName, minVal, maxVal)
		if err != nil {
			return 0, err
		}
		if envSet {
			return n, nil
		}
		if cfgVal != 0 {
			val = cfgVal
			set = true
		}
	case cfgVal != 0:
		val = cfgVal
		set = true
	}
	if !set {
		return defaultVal, nil
	}
	if val < minVal || (maxVal > 0 && val > maxVal) {
		return 0, fmt.Errorf("value out of range (%d): must be >= %d (max %d)", val, minVal, maxVal)
	}
	return val, nil
}

// ResolveSecret resolves a secret value with priority:
//  1. explicit CLI flag (when cliSet == true), returning fromCLI=true so callers can warn
//  2. <ENV>_FILE (trims trailing CR/LF only, preserving interior spaces; errors if empty)
//  3. <ENV> environment variable
//  4. config file fallback
func ResolveSecret(cliVal string, cliSet bool, envName, fileEnvName, configVal string) (val string, fromCLI bool, err error) {
	if cliSet {
		return cliVal, cliVal != "", nil
	}
	if fileEnvName != "" {
		if path := strings.TrimSpace(os.Getenv(fileEnvName)); path != "" {
			data, readErr := os.ReadFile(filepath.Clean(path)) // #nosec G304,G703 -- operator-configured *_FILE secret path (Docker/systemd secrets)
			if readErr != nil {
				return "", false, fmt.Errorf("read %s (%q): %w", fileEnvName, path, readErr)
			}
			trimmed := strings.TrimRight(string(data), "\r\n")
			if trimmed == "" {
				return "", false, fmt.Errorf("%s (%q) is empty", fileEnvName, path)
			}
			return trimmed, false, nil
		}
	}
	if envName != "" {
		if v, ok := os.LookupEnv(envName); ok && v != "" {
			return v, false, nil
		}
	}
	return configVal, false, nil
}

// ParseEnvBool strictly parses a boolean environment variable.
// Accepted true values: "1", "true", "yes", "on". Accepted false values: "0", "false", "no", "off".
// Any other non-empty string fails fast with an error instead of silently ignoring typos.
func ParseEnvBool(envName string) (val bool, set bool, err error) {
	raw, ok := os.LookupEnv(envName)
	if !ok || strings.TrimSpace(raw) == "" {
		return false, false, nil
	}
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "1", "true", "yes", "on":
		return true, true, nil
	case "0", "false", "no", "off":
		return false, true, nil
	default:
		return false, true, fmt.Errorf("invalid boolean for %s: %q (expected true/false/1/0/yes/no/on/off)", envName, raw)
	}
}

// ParseEnvInt strictly parses an integer environment variable and enforces minVal <= v <= maxVal (when maxVal > 0).
func ParseEnvInt(envName string, minVal, maxVal int) (val int, set bool, err error) {
	raw, ok := os.LookupEnv(envName)
	if !ok || strings.TrimSpace(raw) == "" {
		return 0, false, nil
	}
	n, convErr := strconv.Atoi(strings.TrimSpace(raw))
	if convErr != nil {
		return 0, true, fmt.Errorf("invalid integer for %s: %q: %w", envName, raw, convErr)
	}
	if n < minVal || (maxVal > 0 && n > maxVal) {
		return 0, true, fmt.Errorf("%s out of range (%d): must be >= %d (max %d)", envName, n, minVal, maxVal)
	}
	return n, true, nil
}

// SplitNonEmptyCSV splits a comma-separated list, trims whitespace, and drops empty entries.
func SplitNonEmptyCSV(raw string) []string {
	parts := strings.Split(raw, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if t := strings.TrimSpace(p); t != "" {
			out = append(out, t)
		}
	}
	return out
}
