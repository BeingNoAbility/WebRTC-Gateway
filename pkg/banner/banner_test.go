package banner

import (
	"bytes"
	"strings"
	"testing"
)

func TestFormatSecurityBanner(t *testing.T) {
	title := "[SECURITY WARNING] Running in Anonymous Mode"
	lines := []string{
		"No SIGNAL_SECRET configured!",
		"Cluster registration is completely unauthenticated.",
		"Configure -signal-secret or SIGNAL_SECRET for production.",
	}

	// 1. Non-TTY test: must NOT contain ANSI escape characters
	nonTTY := FormatSecurityBanner(false, title, lines)
	if strings.Contains(nonTTY, "\033") {
		t.Errorf("Expected non-TTY banner to contain NO ANSI codes, but found ESC: %q", nonTTY)
	}
	if !strings.Contains(nonTTY, title) {
		t.Errorf("Expected banner to contain title %q", title)
	}
	for _, l := range lines {
		if !strings.Contains(nonTTY, l) {
			t.Errorf("Expected banner to contain line %q", l)
		}
	}

	// 2. TTY test: must contain ANSI bold yellow prefix and reset suffix
	tty := FormatSecurityBanner(true, title, lines)
	if !strings.HasPrefix(tty, ansiBoldYellow) {
		t.Errorf("Expected TTY banner to start with ANSI yellow, got: %q", tty[:10])
	}
	if !strings.HasSuffix(tty, ansiReset) {
		t.Errorf("Expected TTY banner to end with ANSI reset, got: %q", tty[len(tty)-10:])
	}
	if !strings.Contains(tty, title) {
		t.Errorf("Expected TTY banner to contain title %q", title)
	}

	// 3. PrintSecurityBanner helper
	var buf bytes.Buffer
	PrintSecurityBanner(&buf, false, title, lines)
	if buf.Len() == 0 || strings.Contains(buf.String(), "\033") {
		t.Errorf("PrintSecurityBanner failed, got output: %q", buf.String())
	}
}
