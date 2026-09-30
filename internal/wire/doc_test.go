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

// spec holds the value CLAUDE.md gives every constant (§3–§4, and §6, §6.5,
// §7, §11 and §12 where they fix a number), written by hand. It is never
// generated: a constant changed in wire and in PROTOCOL.md together still
// fails here, until CLAUDE.md and this table change too, which CLAUDE.md
// §1.4 asks the user to approve first.
var spec = map[string]string{
	"ADSize":                  `28`,
	"AcceptPromptTimeout":     `10m0s`,
	"AckDelivered":            `1`,
	"BusyRejectWindow":        `10m0s`,
	"ByeGracePeriod":          `2s`,
	"ByeLocalError":           `2`,
	"ByeReplaced":             `3`,
	"ByeResourceLimit":        `4`,
	"ByeShutdown":             `1`,
	"ByeUserQuit":             `0`,
	"CancelHashDiverged":      `1`,
	"CancelLocalIO":           `2`,
	"CancelShutdown":          `3`,
	"CancelUser":              `0`,
	"ChainKeySize":            `32`,
	"ChainsInfo":              `"burrow/1 chains"`,
	"ChatQueueCap":            `64`,
	"ChunkData":               `65524`,
	"ClassChat":               `2`,
	"ClassControl":            `1`,
	"ClassTransfer":           `3`,
	"ClassUnknown":            `0`,
	"ClosedStreamMemory":      `64`,
	"ControlQueueCap":         `8`,
	"ControlQueueStall":       `5s`,
	"ControllerTick":          `1s`,
	"DeadPeerTimeout":         `1m30s`,
	"DefaultInviteTTL":        `1h0m0s`,
	"DefaultListenPort":       `47337`,
	"DefaultMaxFile":          `104857600`,
	"DefaultMaxImage":         `26214400`,
	"DirInitiatorToResponder": `0`,
	"DirResponderToInitiator": `1`,
	"EpochMaxFrames":          `65536`,
	"FeatureAnimatedGIF":      `4`,
	"FeatureFiles":            `8`,
	"FeatureImages":           `1`,
	"FeatureMask":             `15`,
	"FeatureTyping":           `2`,
	"FormatGIF":               `4`,
	"FormatJPEG":              `2`,
	"FormatPNG":               `1`,
	"FormatWebP":              `3`,
	"FrameADPrefix":           `"burrow-frame-v1"`,
	"FreeSpaceMargin":         `67108864`,
	"GlareWindow":             `5s`,
	"HS1Len":                  `1232`,
	"HS1PayloadLen":           `1184`,
	"HS2Len":                  `1168`,
	"HS2PayloadLen":           `1120`,
	"HS3Len":                  `113`,
	"HS3PayloadLen":           `49`,
	"HSLenPrefix":             `2`,
	"HSRandomSize":            `32`,
	"HandshakeMsg1Wait":       `5s`,
	"HandshakeTimeout":        `10s`,
	"HashSize":                `32`,
	"InnerHeaderSize":         `8`,
	"InviteTokenSz":           `16`,
	"KEMCiphertextSize":       `1088`,
	"KEMEncapsulationKeySize": `1184`,
	"KEMSeedSize":             `64`,
	"KEMSharedSecretSize":     `32`,
	"LargeBucket":             `4096`,
	"MaxActiveTransfers":      `2`,
	"MaxBusyRejects":          `32`,
	"MaxCaptionBytes":         `1024`,
	"MaxCiphertext":           `65552`,
	"MaxConcurrentDecode":     `2`,
	"MaxContacts":             `1024`,
	"MaxContactsPerInvite":    `64`,
	"MaxDrainingStreams":      `16`,
	"MaxFileExt":              `8`,
	"MaxGIFFrames":            `200`,
	"MaxHelloName":            `32`,
	"MaxImageDecodeBytes":     `268435456`,
	"MaxImagePixels":          `40000000`,
	"MaxImageSide":            `16384`,
	"MaxInner":                `65536`,
	"MaxOffersPerWindow":      `16`,
	"MaxOuterFrame":           `65556`,
	"MaxPayload":              `65528`,
	"MaxPeers":                `32`,
	"MaxPendingHandshakes":    `64`,
	"MaxPendingOffers":        `4`,
	"MaxPingsPerWindow":       `4`,
	"MaxQueuedMessages":       `1000`,
	"MaxTextBytes":            `16384`,
	"MaxTransferSize":         `1099511627776`,
	"MaxTypingPerWindow":      `32`,
	"MinCiphertext":           `272`,
	"MinInner":                `256`,
	"Msg1ReplayLRU":           `4096`,
	"MsgIDDedupSet":           `4096`,
	"NonceSize":               `12`,
	"OfferWindow":             `1m0s`,
	"OuterLenSize":            `4`,
	"PingFloodWindow":         `10s`,
	"PingInterval":            `30s`,
	"Prologue":                `"burrow/1"`,
	"ProtocolVersion":         `1`,
	"RateLimitPenalty":        `10m0s`,
	"ReconnectMaxBackoff":     `1m0s`,
	"ReconnectMinBackoff":     `1s`,
	"RejectBusy":              `3`,
	"RejectDeclined":          `0`,
	"RejectTooLarge":          `1`,
	"RejectUnsupported":       `2`,
	"RekeyEpochFailSafe":      `25m0s`,
	"RekeyInFlightTimeout":    `30s`,
	"RekeyInitCounter":        `1024`,
	"RekeyInitInterval":       `15m0s`,
	"RekeyRequestCounter":     `2048`,
	"RekeyRequestInterval":    `20m0s`,
	"RekeyRequestTimeout":     `10s`,
	"ResultAborted":           `2`,
	"ResultHashMismatch":      `1`,
	"ResultOK":                `0`,
	"RootKeySize":             `32`,
	"SmallBucket":             `256`,
	"SmallBucketMax":          `4096`,
	"StreamChat":              `1`,
	"StreamControl":           `0`,
	"StreamMinTransfer":       `2`,
	"TCPFailuresPerMinute":    `10`,
	"TagSize":                 `16`,
	"TorFailuresPerMinute":    `60`,
	"TorLimitedSlots":         `16`,
	"TransferQueueCap":        `2`,
	"TypeAck":                 `0x11`,
	"TypeBye":                 `0x04`,
	"TypeFileOffer":           `0x27`,
	"TypeHello":               `0x01`,
	"TypeImgAccept":           `0x21`,
	"TypeImgCancel":           `0x26`,
	"TypeImgChunk":            `0x23`,
	"TypeImgDone":             `0x24`,
	"TypeImgOffer":            `0x20`,
	"TypeImgReject":           `0x22`,
	"TypeImgResult":           `0x25`,
	"TypePing":                `0x02`,
	"TypePong":                `0x03`,
	"TypeRekeyDone":           `0x32`,
	"TypeRekeyInit":           `0x30`,
	"TypeRekeyRequest":        `0x33`,
	"TypeRekeyResp":           `0x31`,
	"TypeText":                `0x10`,
	"TypeTyping":              `0x12`,
	"TypingStart":             `1`,
	"TypingStop":              `0`,
	"TypingWindow":            `1m0s`,
	"WriteDeadline":           `30s`,
	"X25519Size":              `32`,
}

// The code and the spec agree on every constant, and no constant exists
// that the spec does not list.
func TestConstantsMatchTheSpec(t *testing.T) {
	have := wireConstants(t)
	for n, v := range have {
		if sv, ok := spec[n]; !ok {
			t.Errorf("%s = %s is not in the spec table; add it by hand from CLAUDE.md", n, v)
		} else if sv != v {
			t.Errorf("%s = %s, the spec says %s", n, v, sv)
		}
	}
	for n := range spec {
		if _, ok := have[n]; !ok {
			t.Errorf("the spec lists %s, which internal/wire does not export", n)
		}
	}
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
