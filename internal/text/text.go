// Package text sanitizes, normalizes and redacts peer-provided strings
// (CLAUDE.md §9.5). Every peer string passes through Sanitize before it can
// exist as a Go string; Redact is the only way a peer string reaches a log.
package text

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strings"
	"unicode"
	"unicode/utf8"

	"golang.org/x/text/cases"
	"golang.org/x/text/unicode/norm"
)

// Field selects the sanitization profile.
type Field uint8

// Field profiles.
const (
	Multiline  Field = iota // TEXT: keeps \n and \t, keeps ZWJ/ZWNJ runs ≤ 2
	SingleLine              // captions: \n and \t become a space, keeps ZWJ/ZWNJ runs ≤ 2
	Name                    // display names: single line, ZWJ/ZWNJ removed
)

// ErrUTF8 is returned for invalid UTF-8 input.
var ErrUTF8 = errors.New("text: invalid UTF-8")

const (
	maxJoinerRun    = 2
	maxCombiningRun = 4
)

// Sanitize converts peer bytes into a safe string. Length limits are the
// caller's job (enforced on the byte slice in wire). Invalid UTF-8 is an error.
func Sanitize(b []byte, f Field) (string, error) {
	if !utf8.Valid(b) {
		return "", ErrUTF8
	}
	var sb strings.Builder
	sb.Grow(len(b))
	joiners, marks := 0, 0
	for i := 0; i < len(b); {
		r, n := utf8.DecodeRune(b[i:])
		i += n
		switch {
		case r == '\n' || r == '\t':
			joiners, marks = 0, 0
			if f == Multiline {
				sb.WriteRune(r)
			} else {
				sb.WriteByte(' ')
			}
			continue
		case r == 0x200C || r == 0x200D:
			// A joiner is not a base character: it leaves the mark count alone,
			// or marks could be stacked without limit between joiners.
			if f == Name {
				continue
			}
			joiners++
			if joiners > maxJoinerRun {
				continue
			}
			sb.WriteRune(r)
			continue
		case unicode.Is(unicode.Mn, r) || unicode.Is(unicode.Me, r):
			marks++
			if marks > maxCombiningRun {
				continue // dropped: it must not end a joiner run either
			}
			joiners = 0
			sb.WriteRune(r)
			continue
		case forbidden(r):
			r = utf8.RuneError
		}
		joiners, marks = 0, 0
		sb.WriteRune(r)
	}
	return sb.String(), nil
}

// forbidden reports whether r must be replaced with U+FFFD.
func forbidden(r rune) bool {
	switch {
	case unicode.Is(unicode.Cc, r): // C0, DEL, C1 (\n and \t handled by caller)
		return true
	case r >= 0x202A && r <= 0x202E, r >= 0x2066 && r <= 0x2069: // bidi overrides / isolates
		return true
	case r == 0x2028, r == 0x2029: // line / paragraph separator
		return true
	case r == 0x200B, r == 0x200E, r == 0x200F, r >= 0x2060 && r <= 0x2064:
		return true
	case r == 0xFEFF, r == 0x061C, r == 0x180E:
		return true
	case r >= 0xE0000 && r <= 0xE007F: // tag characters
		return true
	}
	return false
}

var fold = cases.Fold()

// Normalize maps a display name to its collision-check form: NFKC → case fold →
// drop Cf ∪ Other_Default_Ignorable_Code_Point ∪ Variation_Selector → collapse
// whitespace runs to one space → trim, repeated until nothing changes.
// Compare results byte-equal.
//
// One pass is not enough: an invisible character between a letter and a
// combining mark keeps NFKC from composing them, and removing it afterwards
// leaves "e" + U+0301 where "é" was meant. Two names that look the same must
// normalize the same, and Normalize(Normalize(x)) must equal Normalize(x).
func Normalize(name string) string {
	s := name
	for range 4 { // the second pass settles every input seen; the bound keeps it cheap
		next := normalizeOnce(s)
		if next == s {
			break
		}
		s = next
	}
	return s
}

func normalizeOnce(name string) string {
	s := fold.String(norm.NFKC.String(name))
	var sb strings.Builder
	sb.Grow(len(s))
	space := false
	for _, r := range s {
		if unicode.Is(unicode.Cf, r) || unicode.Is(unicode.Other_Default_Ignorable_Code_Point, r) || unicode.Is(unicode.Variation_Selector, r) {
			continue
		}
		if unicode.IsSpace(r) {
			space = true
			continue
		}
		if space && sb.Len() > 0 {
			sb.WriteByte(' ')
		}
		space = false
		sb.WriteRune(r)
	}
	return sb.String()
}

// redactKey is a per-process random HMAC key so a log reader cannot reverse a
// short redaction hash by guessing.
var redactKey = func() []byte {
	k := make([]byte, 32)
	if _, err := rand.Read(k); err != nil {
		panic("text: crypto/rand unavailable: " + err.Error())
	}
	return k
}()

// Redact replaces a peer-provided string with its length and a short keyed
// hash, e.g. "[len=12 h=1a2b3c4d]". It is the only form in which peer strings
// may appear in logs.
func Redact(s string) string {
	m := hmac.New(sha256.New, redactKey)
	m.Write([]byte(s))
	return "[len=" + itoa(len(s)) + " h=" + hex.EncodeToString(m.Sum(nil)[:4]) + "]"
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}
