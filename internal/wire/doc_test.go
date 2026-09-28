package wire

import (
	"bytes"
	"flag"
	"fmt"
	"go/ast"
	"go/constant"
	"go/importer"
	"go/parser"
	"go/token"
	"go/types"
	"os"
	"regexp"
	"sort"
	"strings"
	"testing"
	"time"
)

var updateDoc = flag.Bool("update", false, "regenerate the constants table in docs/PROTOCOL.md")

const (
	docPath  = "../../docs/PROTOCOL.md"
	docBegin = "<!-- constants:begin -->"
	docEnd   = "<!-- constants:end -->"
)

// wireConstants type-checks this package's non-test sources and returns every
// exported constant rendered the way PROTOCOL.md shows it.
func wireConstants(t *testing.T) map[string]string {
	t.Helper()
	fset := token.NewFileSet()
	ents, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	var files []*ast.File
	for _, e := range ents {
		if !strings.HasSuffix(e.Name(), ".go") || strings.HasSuffix(e.Name(), "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, e.Name(), nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		files = append(files, f)
	}
	conf := types.Config{Importer: importer.ForCompiler(fset, "source", nil)}
	pkg, err := conf.Check("wire", fset, files, nil)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]string{}
	for _, name := range pkg.Scope().Names() {
		c, ok := pkg.Scope().Lookup(name).(*types.Const)
		if !ok || !c.Exported() {
			continue
		}
		out[name] = render(c)
	}
	return out
}

func render(c *types.Const) string {
	v := c.Val()
	if v.Kind() == constant.String {
		return fmt.Sprintf("%q", constant.StringVal(v))
	}
	if named, ok := c.Type().(*types.Named); ok {
		switch named.Obj().Name() {
		case "Duration":
			n, _ := constant.Int64Val(v)
			return time.Duration(n).String()
		case "FrameType":
			n, _ := constant.Int64Val(v)
			return fmt.Sprintf("0x%02X", n)
		}
	}
	if v.Kind() == constant.Int {
		if n, exact := constant.Int64Val(v); exact {
			return fmt.Sprintf("%d", n)
		}
		if n, exact := constant.Uint64Val(v); exact {
			return fmt.Sprintf("%d", n)
		}
	}
	return v.ExactString()
}

var rowRE = regexp.MustCompile("^\\| `([A-Za-z0-9_]+)` \\| ([^|]*) \\|")

func TestProtocolDocConstants(t *testing.T) {
	want := wireConstants(t)
	doc, err := os.ReadFile(docPath)
	if err != nil {
		t.Fatal(err)
	}
	b, e := bytes.Index(doc, []byte(docBegin)), bytes.Index(doc, []byte(docEnd))
	if b < 0 || e < b {
		t.Fatalf("%s lacks %s/%s markers", docPath, docBegin, docEnd)
	}
	if *updateDoc {
		names := make([]string, 0, len(want))
		for n := range want {
			names = append(names, n)
		}
		sort.Strings(names)
		var sb strings.Builder
		sb.WriteString(docBegin + "\n| Constant | Value |\n|---|---|\n")
		for _, n := range names {
			fmt.Fprintf(&sb, "| `%s` | %s |\n", n, want[n])
		}
		sb.WriteString(docEnd)
		out := append(append(append([]byte{}, doc[:b]...), sb.String()...), doc[e+len(docEnd):]...)
		if err := os.WriteFile(docPath, out, 0o644); err != nil {
			t.Fatal(err)
		}
		doc = out
	}
	got := map[string]string{}
	for _, line := range strings.Split(string(doc[b:e]), "\n") {
		if m := rowRE.FindStringSubmatch(line); m != nil {
			got[m[1]] = strings.TrimSpace(m[2])
		}
	}
	for n, v := range want {
		if dv, ok := got[n]; !ok {
			t.Errorf("PROTOCOL.md is missing constant %s (run: go test ./internal/wire -run TestProtocolDocConstants -update)", n)
		} else if dv != v {
			t.Errorf("PROTOCOL.md %s = %s, wire has %s", n, dv, v)
		}
	}
	for n := range got {
		if _, ok := want[n]; !ok {
			t.Errorf("PROTOCOL.md lists %s, which internal/wire does not export", n)
		}
	}
}
