package text

import (
	"errors"
	"strings"
	"testing"
	"unicode"
	"unicode/utf8"
)

func TestSanitize(t *testing.T) {
	cases := []struct {
		in   string
		f    Field
		want string
	}{
		{"hello", Multiline, "hello"},
		{"a\nb\tc", Multiline, "a\nb\tc"},
		{"a\nb\tc", SingleLine, "a b c"},
		{"a\nb", Name, "a b"},
		{"a\rb", Multiline, "a�b"},
		{"a\x00b\x7fc\u0085d", Multiline, "a�b�c�d"},
		{"x\u202Ey\u2066z\u202A", Multiline, "x�y�z�"},
		{"\u2028\u2029\u200B\u200E\u200F\u2060\u2064\uFEFF\u061C\u180E", Multiline, strings.Repeat("�", 10)},
		{"\U000E0001\U000E007F", Multiline, "��"},
		{"\u200C\u200Cx", Multiline, "\u200C\u200Cx"},               // ZWNJ kept
		{"👩\u200D👩\u200D👧", SingleLine, "👩\u200D👩\u200D👧"},          // ZWJ family kept
		{"a\u200D\u200D\u200D\u200Db", Multiline, "a\u200D\u200Db"}, // run capped at 2
		{"a\u200Db", Name, "ab"},                                    // removed from names
		{"é́́́́́x", Multiline, "é́́́x"},                           // marks capped at 4
		{"é́́́́", Multiline, "é́́́"},
		{"́́́́́", Multiline, "́́́́"}, // no base
		{"é⃝⃝⃝⃝⃝a", Multiline, "é⃝⃝⃝⃝a"},
		{"日本語 مرحبا", SingleLine, "日本語 مرحبا"},
		{"", Multiline, ""},
	}
	for _, c := range cases {
		got, err := Sanitize([]byte(c.in), c.f)
		if err != nil || got != c.want {
			t.Errorf("Sanitize(%q,%d)=%q,%v want %q", c.in, c.f, got, err, c.want)
		}
	}
	if _, err := Sanitize([]byte{0xff}, Multiline); !errors.Is(err, ErrUTF8) {
		t.Fatal(err)
	}
	if _, err := Sanitize([]byte("\xc3"), Name); !errors.Is(err, ErrUTF8) {
		t.Fatal(err)
	}
}

// Joiners and dropped marks are invisible: neither may be used to get around
// the cap on the other.
func TestSanitizeCapsCannotBeInterleaved(t *testing.T) {
	count := func(s string, pred func(rune) bool) (n int) {
		for _, r := range s {
			if pred(r) {
				n++
			}
		}
		return n
	}
	mark := func(r rune) bool { return r == 0x0301 }
	joiner := func(r rune) bool { return r == 0x200D }
	stacked := "a" + strings.Repeat("\u0301\u0301\u0301\u0301\u200d", 50)
	for _, f := range []Field{Multiline, SingleLine, Name} {
		got, err := Sanitize([]byte(stacked), f)
		if err != nil || count(got, mark) > maxCombiningRun {
			t.Errorf("field %d: %d marks on one base", f, count(got, mark))
		}
	}
	got, _ := Sanitize([]byte("a\u0301\u0301\u0301\u0301"+strings.Repeat("\u200d\u200d\u0301", 50)), Multiline)
	if count(got, joiner) > maxJoinerRun {
		t.Errorf("%d joiners in a row", count(got, joiner))
	}
}

func TestNormalize(t *testing.T) {
	cases := [][2]string{
		{"Alice", "alice"},
		{"  Alice   Smith ", "alice smith"},
		{"ＡＬＩＣＥ", "alice"},       // fullwidth → NFKC
		{"Ａlice\u200D", "alice"}, // Cf removed
		{"al\u00ADice", "alice"}, // soft hyphen (Cf)
		{"ali️ce", "alice"},      // variation selector
		{"ﬁsh", "fish"},          // ligature
		{"Straße", "strasse"},    // full case fold
		{"a b", "a b"},           // nbsp collapses (NFKC → space)
		{"\u200B", ""},           // ZWSP is Cf → empty
		{"a\t\n b", "a b"},
	}
	for _, c := range cases {
		if got := Normalize(c[0]); got != c[1] {
			t.Errorf("Normalize(%q)=%q want %q", c[0], got, c[1])
		}
	}
	if Normalize("Alice") != Normalize("alice ") || Normalize("Alice") == Normalize("Alicia") {
		t.Fatal("collision semantics")
	}
}

func TestRedact(t *testing.T) {
	r := Redact("hello")
	if !strings.HasPrefix(r, "[len=5 h=") || len(r) != len("[len=5 h=")+8+1 || strings.Contains(r, "hello") {
		t.Fatalf("%q", r)
	}
	if Redact("hello") != r || Redact("hellp") == r {
		t.Fatal("determinism")
	}
	if Redact("") != "[len=0 h="+Redact("")[9:] {
		t.Fatal("empty")
	}
	if itoa(1234567) != "1234567" {
		t.Fatal("itoa")
	}
}

func checkClean(t *testing.T, s string, f Field) {
	t.Helper()
	if !utf8.ValidString(s) {
		t.Fatal("invalid utf8 out")
	}
	joiners, marks := 0, 0
	for _, r := range s {
		if forbidden(r) && r != '\n' && r != '\t' {
			t.Fatalf("forbidden rune %U survived", r)
		}
		if f != Multiline && (r == '\n' || r == '\t') {
			t.Fatal("newline in single-line field")
		}
		if r == 0x200C || r == 0x200D {
			if f == Name {
				t.Fatal("joiner in name")
			}
			joiners++
			if joiners > maxJoinerRun {
				t.Fatal("joiner run")
			}
			continue
		}
		if unicode.Is(unicode.Mn, r) || unicode.Is(unicode.Me, r) {
			marks++
			joiners = 0
			if marks > maxCombiningRun {
				t.Fatal("mark run")
			}
			continue
		}
		joiners, marks = 0, 0
	}
}

func FuzzSanitize(f *testing.F) {
	for _, s := range []string{"hello", "a\r\nb", "\u202E", "👩\u200D👩", "é́́́́", "\xff", ""} {
		f.Add([]byte(s), uint8(0))
		f.Add([]byte("a\u0301\u0301\u0301\u0301\u200d\u0301\u200d\u200d\u0301"+s), uint8(1))
		f.Add([]byte(s), uint8(2))
	}
	f.Fuzz(func(t *testing.T, b []byte, fld uint8) {
		field := Field(fld % 3)
		out, err := Sanitize(b, field)
		if err != nil {
			if utf8.Valid(b) {
				t.Fatal("rejected valid utf8")
			}
			return
		}
		if len(out) > len(b)*3 { // U+FFFD is 3 bytes; nothing else grows
			t.Fatal("growth")
		}
		checkClean(t, out, field)
		again, err := Sanitize([]byte(out), field)
		if err != nil || again != out {
			t.Fatalf("not idempotent: %q -> %q", out, again)
		}
	})
}

func FuzzNormalize(f *testing.F) {
	for _, s := range []string{"Alice", "ＡＬＩＣＥ", "Straße", "a\u00ADb", " x  y "} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		out := Normalize(s)
		if !utf8.ValidString(out) {
			t.Fatal("utf8")
		}
		if strings.HasPrefix(out, " ") || strings.HasSuffix(out, " ") || strings.Contains(out, "  ") {
			t.Fatalf("whitespace: %q", out)
		}
		for _, r := range out {
			if unicode.Is(unicode.Cf, r) || (unicode.IsSpace(r) && r != ' ') {
				t.Fatalf("rune %U survived", r)
			}
		}
	})
}
