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

// Kitty Unicode placeholders let an image flow with text: the image is sent
// once as a virtual placement, and the conversation holds rows of U+10EEEE
// cells whose foreground colour carries the image id. Text renderers treat
// the cells as ordinary characters, so the image scrolls with the viewport.

const kittyPlaceholder = '\U0010EEEE'

// rowDiacritics are the first entries of Kitty's row/column diacritic table.
var rowDiacritics = []rune{0x0305, 0x030D, 0x030E, 0x0310, 0x0312, 0x033D, 0x033E, 0x033F, 0x0346, 0x034A, 0x034B, 0x034C}

// MaxInlineRows is the tallest inline thumbnail, in terminal rows.
const MaxInlineRows = 12

// InlineImage is an image prepared for inline display.
type InlineImage struct {
	Transmit string   // escape sequence that uploads the image (send once, raw)
	Lines    []string // placeholder rows to put into the conversation
}

// KittyInline prepares img as a virtual placement of at most cols × MaxInlineRows
// cells with the given id (1–255).
func KittyInline(img image.Image, id uint8, cols int) (InlineImage, error) {
	if id == 0 || cols < 1 {
		return InlineImage{}, fmt.Errorf("bad inline image parameters")
	}
	b := img.Bounds()
	if b.Dx() < 1 || b.Dy() < 1 {
		return InlineImage{}, fmt.Errorf("empty image")
	}
	// Terminal cells are roughly twice as tall as wide.
	rows := (b.Dy()*cols/b.Dx() + 1) / 2
	if rows < 1 {
		rows = 1
	}
	if rows > MaxInlineRows {
		rows = MaxInlineRows
		cols = max(1, min(cols, rows*2*b.Dx()/b.Dy()))
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		return InlineImage{}, err
	}
	b64 := base64.StdEncoding.EncodeToString(buf.Bytes())
	var tx strings.Builder
	first := true
	for len(b64) > 0 {
		n := min(4096, len(b64))
		more := 0
		if n < len(b64) {
			more = 1
		}
		if first {
			fmt.Fprintf(&tx, "\x1b_Ga=T,U=1,f=100,q=2,i=%d,c=%d,r=%d,m=%d;%s\x1b\\", id, cols, rows, more, b64[:n])
			first = false
		} else {
			fmt.Fprintf(&tx, "\x1b_Gm=%d;%s\x1b\\", more, b64[:n])
		}
		b64 = b64[n:]
	}
	out := InlineImage{Transmit: tx.String()}
	for r := 0; r < rows; r++ {
		var sb strings.Builder
		fmt.Fprintf(&sb, "\x1b[38;5;%dm", id)
		sb.WriteRune(kittyPlaceholder)
		sb.WriteRune(rowDiacritics[r]) // following cells inherit the row and count columns up
		for c := 1; c < cols; c++ {
			sb.WriteRune(kittyPlaceholder)
		}
		sb.WriteString("\x1b[39m")
		out.Lines = append(out.Lines, sb.String())
	}
	return out, nil
}
