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

// ResolveExplicitConfigPath resolves a config file ONLY from explicit CLI flags
// (-config / --config) or the CONFIG_FILE environment variable.
// It intentionally NEVER auto-loads from the current working directory (Fail-Closed).
func ResolveExplicitConfigPath(args []string, valueFlags map[string]bool) string {
	if v, ok := FindFlagValue(args, "config", valueFlags); ok && strings.TrimSpace(v) != "" {
		return strings.TrimSpace(v)
	}
	if envPath := strings.TrimSpace(os.Getenv("CONFIG_FILE")); envPath != "" {
		return envPath
	}
	return ""
}

func parseFlagPrefix(arg string) (string, bool) {
	if strings.HasPrefix(arg, "---") {
		return "", false
	}
	if strings.HasPrefix(arg, "--") {
		if len(arg) == 2 {
			return "", false
		}
		return arg[2:], true
	}
	if strings.HasPrefix(arg, "-") && len(arg) > 1 {
		return arg[1:], true
	}
	return "", false
}

// FindFlagValue scans args for -name / --name (matching Go flag.FlagSet semantics: stops at
// "--" or the first positional argument, rejects "---flag", and returns the last occurrence)
// while skipping values belonging to other value-taking flags.
func FindFlagValue(args []string, target string, valueFlags map[string]bool) (string, bool) {
	var foundVal string
	found := false
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if arg == "--" {
			break
		}
		if !strings.HasPrefix(arg, "-") || arg == "-" {
			break // Go flag.FlagSet stops parsing at the first non-flag positional argument.
		}
		trimmed, ok := parseFlagPrefix(arg)
		if !ok {
			continue
		}
		name, val, hasEq := strings.Cut(trimmed, "=")
		if name == target {
			if hasEq {
				foundVal = val
				found = true
				continue
			}
			if i+1 < len(args) {
				foundVal = args[i+1]
				found = true
				i++
				continue
			}
			foundVal = ""
			found = true
			continue
		}
		if !hasEq && valueFlags[name] && i+1 < len(args) {
			i++ // skip the flag's value token
		}
	}
	return foundVal, found
}

// HasBoolFlag returns true if any of the listed boolean flag names (e.g. "v", "version", "h", "help")
// appears as an actual flag token evaluating to true in args, ignoring tokens that are values of
// valueFlags, tokens after "--" or the first positional arg, or explicit false forms like `-version=false`.
func HasBoolFlag(args []string, names []string, valueFlags map[string]bool) bool {
	want := make(map[string]bool, len(names))
	for _, n := range names {
		want[n] = true
	}
	result := false
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if arg == "--" {
			break
		}
		if !strings.HasPrefix(arg, "-") || arg == "-" {
			break
		}
		trimmed, ok := parseFlagPrefix(arg)
		if !ok {
			continue
		}
		name, val, hasEq := strings.Cut(trimmed, "=")
		if want[name] {
			if !hasEq {
				result = true
				continue
			}
			switch strings.ToLower(strings.TrimSpace(val)) {
			case "1", "true", "t", "yes", "on":
				result = true
			default:
				result = false
			}
			continue
		}
		if !hasEq && valueFlags[name] && i+1 < len(args) {
			i++
		}
	}
	return result
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
