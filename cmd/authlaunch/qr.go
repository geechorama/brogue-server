package main

import (
	"io"

	"github.com/mdp/qrterminal/v3"
)

// renderQRCode draws a scannable QR code of text into w using Unicode
// half-block glyphs (two QR rows per text row, so the code stays small enough
// to fit an 80x24 terminal).
//
// The half-block constants qrterminal ships with carry no color, so they
// inherit the terminal's theme — which inverts the code on a light-background
// terminal and can defeat a phone camera. We instead force each half to bright
// white (light module) or black (dark module) with explicit ANSI SGR codes, so
// the polarity (dark modules on a light field) is correct on any theme:
//
//	█ both halves light   (97: bright-white fg fills the cell)
//	(space) both dark     (40: black bg shows through)
//	▀ upper light, lower dark   (97 fg on 40 bg)
//	▄ upper dark,  lower light
//
// Output uses bare "\n"; callers in a raw-mode PTY must wrap w to translate to
// "\r\n" (see crlfWriter) or the rows will staircase.
func renderQRCode(w io.Writer, text string) {
	qrterminal.GenerateWithConfig(text, qrterminal.Config{
		Writer: w,
		// Low error correction keeps the module count (and thus the rendered
		// size) down; a terminal renders the code pixel-perfect, so the extra
		// recovery of higher levels buys nothing here.
		Level:          qrterminal.L,
		HalfBlocks:     true,
		WhiteChar:      "\033[97m█\033[0m",
		BlackChar:      "\033[40m \033[0m",
		WhiteBlackChar: "\033[97;40m▀\033[0m",
		BlackWhiteChar: "\033[97;40m▄\033[0m",
		// A 2-module light border is enough for an on-screen code (no print
		// bleed to guard against) and keeps the whole code inside 80x24 even
		// for a longer issuer URL.
		QuietZone: 2,
	})
}

// crlfWriter translates a "\n" that isn't already preceded by a "\r" into
// "\r\n", so output meant for a normal stream renders correctly on a PTY in raw
// mode (where the terminal does no line-discipline translation). It tracks the
// previous byte across Write calls — qrterminal emits the code in many small
// fragments — so an already-CRLF input is left untouched rather than doubled to
// "\r\r\n". Byte-at-a-time writes are fine for a one-shot QR render.
type crlfWriter struct {
	w      io.Writer
	prevCR bool
}

func (c *crlfWriter) Write(p []byte) (int, error) {
	for i, b := range p {
		var err error
		if b == '\n' && !c.prevCR {
			_, err = c.w.Write([]byte{'\r', '\n'})
		} else {
			_, err = c.w.Write([]byte{b})
		}
		if err != nil {
			return i, err
		}
		c.prevCR = b == '\r'
	}
	return len(p), nil
}
