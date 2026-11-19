package banner

import (
	"fmt"
	"io"
	"os"
	"strings"
)

const (
	ansiBoldYellow = "\033[1;33m"
	ansiReset      = "\033[0m"
)

// IsTerminal returns true if the provided file is a character device (TTY/terminal).
func IsTerminal(f *os.File) bool {
	if f == nil {
		return false
	}
	fi, err := f.Stat()
	if err != nil {
		return false
	}
	return (fi.Mode() & os.ModeCharDevice) != 0
}

// FormatSecurityBanner formats a high-contrast warning box.
// If isTTY is true, ANSI bold yellow color escape sequences are included.
// If isTTY is false (e.g. redirected to file or CI/Docker log aggregator), ANSI codes are stripped for clean plaintext.
func FormatSecurityBanner(isTTY bool, title string, lines []string) string {
	maxWidth := 70
	if len(title) > maxWidth-4 {
		maxWidth = len(title) + 4
	}
	for _, l := range lines {
		if len(l) > maxWidth-4 {
			maxWidth = len(l) + 4
		}
	}

	border := "+" + strings.Repeat("-", maxWidth-2) + "+"
	var b strings.Builder

	if isTTY {
		b.WriteString(ansiBoldYellow)
	}

	b.WriteString(border)
	b.WriteString("\n")

	// Title line
	titlePad := maxWidth - 4 - len(title)
	if titlePad < 0 {
		titlePad = 0
	}
	b.WriteString(fmt.Sprintf("| %s%s |\n", title, strings.Repeat(" ", titlePad)))

	// Blank separator
	b.WriteString(fmt.Sprintf("|%s|\n", strings.Repeat(" ", maxWidth-2)))

	// Content lines
	for _, l := range lines {
		pad := maxWidth - 4 - len(l)
		if pad < 0 {
			pad = 0
		}
		b.WriteString(fmt.Sprintf("| %s%s |\n", l, strings.Repeat(" ", pad)))
	}

	b.WriteString(border)

	if isTTY {
		b.WriteString(ansiReset)
	}

	return b.String()
}

// PrintSecurityBanner writes the formatted security banner to the given writer.
func PrintSecurityBanner(w io.Writer, isTTY bool, title string, lines []string) {
	banner := FormatSecurityBanner(isTTY, title, lines)
	_, _ = fmt.Fprintln(w, banner)
}
