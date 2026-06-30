package main

import (
	"bytes"
	"regexp"
	"strings"
	"testing"
)

var ansiSGR = regexp.MustCompile("\033\\[[0-9;]*m")

// renderQRCode must emit a non-trivial block of half-block glyphs and stay
// within an 80x24 terminal for a realistic device-verification URL.
func TestRenderQRCodeFits80x24(t *testing.T) {
	var buf bytes.Buffer
	renderQRCode(&buf, "https://auth.example.com/device?user_code=ABCD-EFGH")

	out := buf.String()
	if !strings.ContainsAny(out, "█▀▄") {
		t.Fatal("expected half-block QR glyphs in output")
	}

	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
	if len(lines) > 24 {
		t.Errorf("QR is %d rows tall; want <= 24", len(lines))
	}
	for _, l := range lines {
		// Width is measured against the visible glyphs, not the ANSI codes.
		if w := len([]rune(ansiSGR.ReplaceAllString(l, ""))); w > 80 {
			t.Errorf("QR row is %d cols wide; want <= 80", w)
		}
	}
}

// crlfWriter must turn a bare "\n" into "\r\n" (so PTY raw mode doesn't
// staircase) while leaving an already-CRLF "\r\n" untouched, and report the
// original input length as consumed.
func TestCRLFWriter(t *testing.T) {
	var buf bytes.Buffer
	w := &crlfWriter{w: &buf}
	in := []byte("a\nb\r\nc\n")
	n, err := w.Write(in)
	if err != nil {
		t.Fatalf("write: %v", err)
	}
	if n != len(in) {
		t.Errorf("reported %d bytes consumed; want %d", n, len(in))
	}
	// Bare \n's gain a \r; the existing \r\n is left as-is (not doubled).
	if got := buf.String(); got != "a\r\nb\r\nc\r\n" {
		t.Errorf("got %q", got)
	}
}

// The prevCR state must persist across separate Write calls, so a "\r" ending
// one write suppresses the "\r" insertion for a "\n" beginning the next.
func TestCRLFWriterSplitAcrossWrites(t *testing.T) {
	var buf bytes.Buffer
	w := &crlfWriter{w: &buf}
	_, _ = w.Write([]byte("x\r"))
	_, _ = w.Write([]byte("\ny"))
	if got := buf.String(); got != "x\r\ny" {
		t.Errorf("got %q", got)
	}
}
