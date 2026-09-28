package common

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"image"
	"image/png"
	"io"
	"os"
	"strings"
)

// TermImage is the inline-image protocol a terminal supports.
type TermImage uint8

// Protocols.
const (
	TermNone TermImage = iota
	TermKitty
	TermITerm2
)

// DetectTerminal sniffs the environment for Kitty or iTerm2 inline images.
func DetectTerminal() TermImage {
	switch {
	case os.Getenv("KITTY_WINDOW_ID") != "", strings.Contains(os.Getenv("TERM"), "kitty"):
		return TermKitty
	case os.Getenv("TERM_PROGRAM") == "iTerm.app", os.Getenv("TERM_PROGRAM") == "WezTerm":
		return TermITerm2
	}
	return TermNone
}

// WriteImage renders a decoded image inline using the given protocol. Only
// PNG bytes we encode ourselves ever reach the terminal.
func WriteImage(w io.Writer, img image.Image, kind TermImage) error {
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		return err
	}
	b64 := base64.StdEncoding.EncodeToString(buf.Bytes())
	switch kind {
	case TermKitty:
		const chunk = 4096
		first := true
		for len(b64) > 0 {
			n := min(chunk, len(b64))
			more := 0
			if n < len(b64) {
				more = 1
			}
			if first {
				fmt.Fprintf(w, "\x1b_Gf=100,a=T,t=d,m=%d;%s\x1b\\", more, b64[:n])
				first = false
			} else {
				fmt.Fprintf(w, "\x1b_Gm=%d;%s\x1b\\", more, b64[:n])
			}
			b64 = b64[n:]
		}
		_, err := fmt.Fprintln(w)
		return err
	case TermITerm2:
		_, err := fmt.Fprintf(w, "\x1b]1337;File=inline=1;size=%d:%s\x07\n", buf.Len(), b64)
		return err
	}
	return fmt.Errorf("terminal does not support inline images")
}
