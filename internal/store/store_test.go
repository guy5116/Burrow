package store

import (
	"bytes"
	"encoding/hex"
	"errors"
	"fmt"
	"github.com/guy5116/burrow/internal/secret"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"testing"

	"golang.org/x/crypto/argon2"
)

// Small Argon2 parameters keep the tests fast; DefaultArgon2 is exercised once in TestDefaultArgon2.
var fast = Options{Argon2: Argon2{Time: 1, MemKiB: MinMemKiB, Threads: 1}}

func pass(s string) []byte { return []byte(s) }

func mustInit(t *testing.T, dir string, pw []byte) *Store {
	t.Helper()
	s, err := Init(dir, pw, fast)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestInitOpenRoundTrip(t *testing.T) {
	dir := t.TempDir()
	s := mustInit(t, dir, pass("hunter2"))
	id, err := s.LoadIdentity()
	if err != nil {
		t.Fatal(err)
	}
	pub := id.Public()
	id.Clear()
	if err := s.WriteBlob("contacts", []byte("c1")); err != nil {
		t.Fatal(err)
	}
	if err := s.WriteBlob("partials/abcd.meta", []byte("m1")); err != nil {
		t.Fatal(err)
	}
	if _, err := Init(dir, pass("x"), fast); !errors.Is(err, ErrExists) {
		t.Fatal(err)
	}
	// second opener while locked
	if _, err := Open(dir, pass("hunter2")); !errors.Is(err, ErrInUse) {
		t.Fatalf("lock not enforced: %v", err)
	}
	s.Close()
	s.Close() // idempotent

	if m, err := Mode(dir); err != nil || m != ModePassphrase {
		t.Fatal(m, err)
	}
	if _, err := Open(dir, nil); !errors.Is(err, ErrPassphraseRequired) {
		t.Fatal(err)
	}
	if _, err := Open(dir, pass("wrong")); !errors.Is(err, ErrBlob) {
		t.Fatalf("wrong passphrase: %v", err)
	}
	s, err = Open(dir, pass("hunter2"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	id2, _ := s.LoadIdentity()
	if id2.Public() != pub {
		t.Fatal("identity changed")
	}
	if b, err := s.ReadBlob("contacts"); err != nil || string(b) != "c1" {
		t.Fatal(b, err)
	}
	if b, err := s.ReadBlob("partials/abcd.meta"); err != nil || string(b) != "m1" {
		t.Fatal(b, err)
	}
	if l, _ := s.ListBlobs("partials"); len(l) != 1 || l[0] != "partials/abcd.meta" {
		t.Fatal(l)
	}
	if l, _ := s.ListBlobs(""); len(l) != 2 {
		t.Fatal(l)
	}
	if l, err := s.ListBlobs("nope"); err != nil || l != nil {
		t.Fatal(l, err)
	}
	if err := s.DeleteBlob("partials/abcd.meta"); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteBlob("partials/abcd.meta"); err != nil {
		t.Fatal("delete missing must be nil")
	}
	if _, err := s.ReadBlob("partials/abcd.meta"); !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
	for _, bad := range []string{"", "../x", "/abs", "a//b", "a/", "lock", "master.hdr", "sub/master.key", "a b", "..", string(make([]byte, 200))} {
		if _, err := s.ReadBlob(bad); !errors.Is(err, ErrRelPath) {
			t.Errorf("ReadBlob(%q)=%v", bad, err)
		}
		if err := s.WriteBlob(bad, nil); !errors.Is(err, ErrRelPath) {
			t.Errorf("WriteBlob(%q)=%v", bad, err)
		}
		if err := s.DeleteBlob(bad); !errors.Is(err, ErrRelPath) {
			t.Errorf("DeleteBlob(%q)=%v", bad, err)
		}
	}
	if _, err := s.ListBlobs("../x"); !errors.Is(err, ErrRelPath) {
		t.Fatal(err)
	}
	if runtime.GOOS == "windows" {
		return // no permission bits: the per-user profile ACL protects the store (SECURITY.md)
	}
	if fi, _ := os.Stat(filepath.Join(dir, StoreDir, "contacts")); fi.Mode().Perm() != filePerm {
		t.Fatalf("perm %v", fi.Mode())
	}
	if fi, _ := os.Stat(filepath.Join(dir, StoreDir)); fi.Mode().Perm() != dirPerm {
		t.Fatalf("dir perm %v", fi.Mode())
	}
}

func TestInitErrors(t *testing.T) {
	if _, err := Init(t.TempDir(), pass(""), fast); !errors.Is(err, ErrPassphraseEmpty) {
		t.Fatal(err)
	}
	dir := t.TempDir()
	if _, err := Open(dir, pass("x")); !errors.Is(err, ErrNoStore) {
		t.Fatal(err)
	}
	if _, err := Mode(dir); !errors.Is(err, ErrNoStore) {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, oldDir), dirPerm); err != nil {
		t.Fatal(err)
	}
	if _, err := Init(dir, pass("x"), fast); !errors.Is(err, ErrExists) {
		t.Fatal(err)
	}
}

func TestModeNone(t *testing.T) {
	dir := t.TempDir()
	s := mustInit(t, dir, nil)
	if s.Mode() != ModeNone {
		t.Fatal("mode")
	}
	if err := s.WriteBlob("contacts", []byte("z")); err != nil {
		t.Fatal(err)
	}
	s.Close()
	if m, _ := Mode(dir); m != ModeNone {
		t.Fatal("mode")
	}
	s, err := Open(dir, pass("ignored"))
	if err != nil {
		t.Fatal(err)
	}
	if b, _ := s.ReadBlob("contacts"); string(b) != "z" {
		t.Fatal(b)
	}
	// corrupt master.key
	s.Close()
	if err := os.WriteFile(filepath.Join(dir, StoreDir, keyFile), []byte("short"), filePerm); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(dir, nil); !errors.Is(err, ErrBlob) {
		t.Fatal(err)
	}
}

func TestTamperAndRelpathSwap(t *testing.T) {
	dir := t.TempDir()
	s := mustInit(t, dir, pass("pw"))
	defer s.Close()
	if err := s.WriteBlob("contacts", []byte("data")); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(dir, StoreDir, "contacts")
	b, _ := os.ReadFile(p)
	// flip a ciphertext bit
	b[len(b)-1] ^= 1
	if err := os.WriteFile(p, b, filePerm); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ReadBlob("contacts"); !errors.Is(err, ErrBlob) {
		t.Fatal(err)
	}
	b[len(b)-1] ^= 1
	// copy under another name: AD binds the relpath
	if err := os.WriteFile(filepath.Join(dir, StoreDir, "invites"), b, filePerm); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ReadBlob("invites"); !errors.Is(err, ErrBlob) {
		t.Fatal(err)
	}
	// truncated / bad magic / bad version
	for _, bad := range [][]byte{b[:10], append([]byte("XXXX"), b[4:]...), append(append([]byte{}, b[:4]...), append([]byte{9}, b[5:]...)...)} {
		if _, err := openBlob(s.master, "contacts", bad); !errors.Is(err, ErrBlob) {
			t.Fatal(err)
		}
	}
	// wrong master
	m2 := mustInit(t, t.TempDir(), pass("pw"))
	defer m2.Close()
	if _, err := openBlob(m2.master, "contacts", b); !errors.Is(err, ErrBlob) {
		t.Fatal(err)
	}
	// identity blob shape
	if err := s.WriteBlob(IdentityKey, []byte{2, 3}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.LoadIdentity(); !errors.Is(err, ErrBlob) {
		t.Fatal(err)
	}
	if _, err := s.LoadOnionKey(); !errors.Is(err, ErrBlob) {
		t.Fatal(err)
	}
}

func TestHeader(t *testing.T) {
	h := Header{Mode: ModePassphrase, KDF: DefaultArgon2}
	h.Salt[0] = 1
	b, err := EncodeHeader(h)
	if err != nil || len(b) != HeaderLen {
		t.Fatal(err)
	}
	got, err := ParseHeader(b)
	if err != nil || got != h {
		t.Fatal(got, err)
	}
	bad := []Header{
		{Mode: 0, KDF: DefaultArgon2}, {Mode: 3, KDF: DefaultArgon2},
		{Mode: 1, KDF: Argon2{0, 1 << 16, 4}}, {Mode: 1, KDF: Argon2{17, 1 << 16, 4}},
		{Mode: 1, KDF: Argon2{3, MinMemKiB - 1, 4}}, {Mode: 1, KDF: Argon2{3, MaxMemKiB + 1, 4}},
		{Mode: 1, KDF: Argon2{3, 1 << 16, 0}}, {Mode: 1, KDF: Argon2{3, 1 << 16, 17}},
	}
	for _, bh := range bad {
		if _, err := EncodeHeader(bh); !errors.Is(err, ErrHeader) {
			t.Errorf("encode %+v accepted", bh)
		}
		raw := make([]byte, 0, HeaderLen)
		raw = append(raw, headerMagic...)
		raw = append(raw, headerVersion, bh.Mode, byte(bh.KDF.Time>>24), byte(bh.KDF.Time>>16), byte(bh.KDF.Time>>8), byte(bh.KDF.Time),
			byte(bh.KDF.MemKiB>>24), byte(bh.KDF.MemKiB>>16), byte(bh.KDF.MemKiB>>8), byte(bh.KDF.MemKiB), bh.KDF.Threads)
		raw = append(raw, make([]byte, SaltLen)...)
		if _, err := ParseHeader(raw); !errors.Is(err, ErrHeader) {
			t.Errorf("parse %+v accepted", bh)
		}
	}
	for _, raw := range [][]byte{nil, b[:HeaderLen-1], append(b, 0), append([]byte("BRWX"), b[4:]...), append(append([]byte{}, b[:4]...), append([]byte{2}, b[5:]...)...)} {
		if _, err := ParseHeader(raw); !errors.Is(err, ErrHeader) {
			t.Errorf("accepted %x", raw)
		}
	}
	// A store with an out-of-bounds header refuses to unlock.
	dir := t.TempDir()
	s := mustInit(t, dir, pass("pw"))
	s.Close()
	hb, _ := os.ReadFile(filepath.Join(dir, StoreDir, headerFile))
	hb[14] = 0 // threads = 0
	if err := os.WriteFile(filepath.Join(dir, StoreDir, headerFile), hb, filePerm); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(dir, pass("pw")); !errors.Is(err, ErrHeader) {
		t.Fatal(err)
	}
}

// Argon2id known answers from x/crypto/argon2's test vectors (password
// "password", salt "somesalt", no secret/AD, 24-byte output).
func TestArgon2idVectors(t *testing.T) {
	cases := []struct {
		time, mem uint32
		par       uint8
		hash      string
	}{
		{1, 64, 1, "655ad15eac652dc59f7170a7332bf49b8469be1fdb9c28bb"},
		{2, 64, 1, "068d62b26455936aa6ebe60060b0a65870dbfa3ddf8d41f7"},
		{2, 64, 2, "350ac37222f436ccb5c0972f1ebd3bf6b958bf2071841362"},
		{3, 256, 2, "4668d30ac4187e6878eedeacf0fd83c5a0a30db2cc16ef0b"},
		{4, 4096, 4, "145db9733a9f4ee43edf33c509be96b934d505a4efb33c5a"},
		{4, 1024, 8, "8dafa8e004f8ea96bf7c0f93eecf67a6047476143d15577f"},
	}
	for _, c := range cases {
		want, _ := hex.DecodeString(c.hash)
		got := argon2.IDKey([]byte("password"), []byte("somesalt"), c.time, c.mem, c.par, uint32(len(want)))
		if !bytes.Equal(got, want) {
			t.Errorf("t=%d m=%d p=%d: %x", c.time, c.mem, c.par, got)
		}
	}
}

func TestDefaultArgon2(t *testing.T) {
	if testing.Short() {
		t.Skip("64 MiB Argon2")
	}
	dir := t.TempDir()
	s, err := Init(dir, pass("pw"), Options{})
	if err != nil {
		t.Fatal(err)
	}
	if s.hdr.KDF != DefaultArgon2 {
		t.Fatal(s.hdr.KDF)
	}
	s.Close()
	s, err = Open(dir, pass("pw"))
	if err != nil {
		t.Fatal(err)
	}
	s.Close()
}

func TestChangePassphraseAndModes(t *testing.T) {
	dir := t.TempDir()
	s := mustInit(t, dir, pass("one"))
	id, _ := s.LoadIdentity()
	pub := id.Public()
	id.Clear()
	if err := s.WriteBlob("contacts", []byte("c")); err != nil {
		t.Fatal(err)
	}
	if err := s.WriteBlob("partials/x.meta", []byte("m")); err != nil {
		t.Fatal(err)
	}
	if err := s.ChangePassphrase(pass(""), fast); !errors.Is(err, ErrPassphraseEmpty) {
		t.Fatal(err)
	}
	// 1 → 1 (new passphrase), still open and usable
	if err := s.ChangePassphrase(pass("two"), fast); err != nil {
		t.Fatal(err)
	}
	if b, err := s.ReadBlob("contacts"); err != nil || string(b) != "c" {
		t.Fatal(b, err)
	}
	if err := s.WriteBlob("invites", []byte("i")); err != nil {
		t.Fatal(err)
	}
	// 1 → 2
	if err := s.ChangePassphrase(nil, fast); err != nil {
		t.Fatal(err)
	}
	if s.Mode() != ModeNone {
		t.Fatal("mode")
	}
	// 2 → 1
	if err := s.ChangePassphrase(pass("three"), fast); err != nil {
		t.Fatal(err)
	}
	// lock still held by s
	if _, err := Open(dir, pass("three")); !errors.Is(err, ErrInUse) {
		t.Fatal(err)
	}
	s.Close()
	for _, d := range []string{newDir, oldDir} {
		if exists(filepath.Join(dir, d)) {
			t.Fatalf("%s left behind", d)
		}
	}
	if exists(filepath.Join(dir, StoreDir, keyFile)) {
		t.Fatal("master.key left behind after mode 2 → 1")
	}
	if _, err := Open(dir, pass("one")); !errors.Is(err, ErrBlob) {
		t.Fatal("old passphrase still works")
	}
	s, err := Open(dir, pass("three"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	id, _ = s.LoadIdentity()
	if id.Public() != pub {
		t.Fatal("identity lost")
	}
	for rel, want := range map[string]string{"contacts": "c", "partials/x.meta": "m", "invites": "i"} {
		if b, err := s.ReadBlob(rel); err != nil || string(b) != want {
			t.Fatalf("%s: %q %v", rel, b, err)
		}
	}
}

var errCrash = errors.New("simulated crash")

// Crash between every step of the directory swap: afterwards exactly one of
// the old/new passphrases opens a complete store with the same identity.
func TestSwapCrashRecovery(t *testing.T) {
	defer func() { swapStep = func(int) error { return nil } }()
	for step := 0; step <= 3; step++ {
		t.Run(string(rune('0'+step)), func(t *testing.T) {
			dir := t.TempDir()
			s := mustInit(t, dir, pass("old"))
			id, _ := s.LoadIdentity()
			pub := id.Public()
			id.Clear()
			if err := s.WriteBlob("contacts", []byte("c")); err != nil {
				t.Fatal(err)
			}
			swapStep = func(i int) error {
				if i == step {
					return errCrash
				}
				return nil
			}
			if err := s.ChangePassphrase(pass("new"), fast); !errors.Is(err, errCrash) {
				t.Fatal(err)
			}
			swapStep = func(int) error { return nil }
			s.Close() // process dies; lock released

			if err := Recover(dir); err != nil {
				t.Fatal(err)
			}
			for _, d := range []string{newDir, oldDir, "store.init"} {
				if exists(filepath.Join(dir, d)) {
					t.Fatalf("%s left after recovery", d)
				}
			}
			var okPass []string
			for _, pw := range []string{"old", "new"} {
				st, err := Open(dir, pass(pw))
				if err == nil {
					okPass = append(okPass, pw)
					st.Close()
				} else if !errors.Is(err, ErrBlob) {
					t.Fatalf("%s: %v", pw, err)
				}
			}
			if len(okPass) != 1 {
				t.Fatalf("passphrases that open the store: %v", okPass)
			}
			opened, err := Open(dir, pass(okPass[0]))
			if err != nil {
				t.Fatal(err)
			}
			defer opened.Close()
			// Once store.old exists the new store is authoritative.
			if want := map[bool]string{true: "new", false: "old"}[step >= 2]; okPass[0] != want {
				t.Fatalf("step %d: opened with %s, want %s", step, okPass[0], want)
			}
			id, err = opened.LoadIdentity()
			if err != nil || id.Public() != pub {
				t.Fatal("identity", err)
			}
			if b, err := opened.ReadBlob("contacts"); err != nil || string(b) != "c" {
				t.Fatal(b, err)
			}
		})
	}
}

func TestRecoverStates(t *testing.T) {
	mk := func(t *testing.T, dirs ...string) string {
		dir := t.TempDir()
		for _, d := range dirs {
			if err := os.MkdirAll(filepath.Join(dir, d), dirPerm); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, d, "tag"), []byte(d), filePerm); err != nil {
				t.Fatal(err)
			}
		}
		return dir
	}
	tag := func(dir string) string { b, _ := os.ReadFile(filepath.Join(dir, StoreDir, "tag")); return string(b) }
	cases := []struct {
		dirs []string
		want string // tag of the resulting store/, "" for none
	}{
		{[]string{StoreDir}, StoreDir},
		{[]string{newDir, oldDir}, newDir},
		{[]string{StoreDir, newDir}, StoreDir},
		{[]string{StoreDir, oldDir}, StoreDir},
		{[]string{oldDir}, oldDir},
		{[]string{StoreDir, newDir, oldDir}, StoreDir},
		{nil, ""},
	}
	for _, c := range cases {
		dir := mk(t, c.dirs...)
		if err := Recover(dir); err != nil {
			t.Fatal(err)
		}
		if got := tag(dir); got != c.want {
			t.Errorf("%v: store tag %q want %q", c.dirs, got, c.want)
		}
		if exists(filepath.Join(dir, newDir)) || exists(filepath.Join(dir, oldDir)) {
			t.Errorf("%v: leftovers", c.dirs)
		}
	}
}

// A store.new alone is no state the swap produces, and may be the only copy
// there is: a whole one becomes the store, an incomplete one is left alone.
func TestRecoverLoneNewStore(t *testing.T) {
	whole := t.TempDir()
	mustInit(t, whole, pass("pw")).Close()
	if err := os.Rename(filepath.Join(whole, StoreDir), filepath.Join(whole, newDir)); err != nil {
		t.Fatal(err)
	}
	if err := Recover(whole); err != nil {
		t.Fatal(err)
	}
	s, err := Open(whole, pass("pw"))
	if err != nil {
		t.Fatal("a whole store.new was not kept", err)
	}
	s.Close()

	partial := t.TempDir()
	if err := os.MkdirAll(filepath.Join(partial, newDir), dirPerm); err != nil {
		t.Fatal(err)
	}
	if err := Recover(partial); err == nil || !exists(filepath.Join(partial, newDir)) {
		t.Fatal("an incomplete store.new was deleted", err)
	}
}

func TestDefaultPaths(t *testing.T) {
	p, err := DefaultPaths()
	if err != nil || p.Config == "" || p.Data == "" {
		t.Fatal(p, err)
	}
	t.Setenv("XDG_CONFIG_HOME", "/tmp/c")
	t.Setenv("XDG_DATA_HOME", "/tmp/d")
	p, _ = DefaultPaths()
	if p.Config != filepath.Join("/tmp/c", "burrow") && p.Config != filepath.Join(os.Getenv("APPDATA"), "burrow") && !bytes.Contains([]byte(p.Config), []byte("Library")) {
		t.Fatal(p)
	}
}

func TestRandomID(t *testing.T) {
	a, err := RandomID(16)
	b, _ := RandomID(16)
	if err != nil || len(a) != 32 || a == b {
		t.Fatal(a, b, err)
	}
}

func FuzzStoreBlob(f *testing.F) {
	master, err := secret.Random(MasterKeyLen)
	if err != nil {
		f.Fatal(err)
	}
	s := &Store{master: master}
	good, _ := sealBlob(s.master, "contacts", []byte("hello"))
	f.Add(good, "contacts")
	f.Add(good, "invites")
	f.Add([]byte("BRWS"), "contacts")
	f.Fuzz(func(t *testing.T, blob []byte, rel string) {
		pt, err := openBlob(s.master, rel, blob)
		if err != nil {
			return
		}
		// Only our own sealed blob for the same relpath may open.
		if rel != "contacts" || !bytes.Equal(pt, []byte("hello")) {
			t.Fatalf("forged blob opened: rel=%q pt=%q", rel, pt)
		}
	})
}

func FuzzMasterHeader(f *testing.F) {
	b, _ := EncodeHeader(Header{Mode: ModePassphrase, KDF: DefaultArgon2})
	f.Add(b)
	f.Add([]byte("BRWM"))
	f.Fuzz(func(t *testing.T, b []byte) {
		h, err := ParseHeader(b)
		if err != nil {
			return
		}
		re, err := EncodeHeader(h)
		if err != nil || !bytes.Equal(re, b) {
			t.Fatal("re-encode")
		}
		if !h.KDF.valid() {
			t.Fatal("bounds")
		}
	})
}

func TestErrorPaths(t *testing.T) {
	if os.Getuid() == 0 || runtime.GOOS == "windows" {
		t.Skip("needs directory permissions that bind the current user")
	}
	dir := t.TempDir()
	s := mustInit(t, dir, pass("pw"))
	defer s.Close()
	if s.Dir() != filepath.Join(dir, StoreDir) {
		t.Fatal(s.Dir())
	}
	// Unwritable store directory → atomic write fails and leaves no temp file.
	if err := os.Chmod(s.Dir(), 0o500); err != nil {
		t.Fatal(err)
	}
	if err := s.WriteBlob("contacts", []byte("x")); err == nil {
		t.Fatal("write into read-only dir succeeded")
	}
	// The swap itself only needs the data dir; deleting the unwritable store.old fails
	// after the change is durable.
	if err := s.ChangePassphrase(pass("q"), fast); !errors.Is(err, ErrOldStoreRemains) {
		t.Fatal(err)
	}
	if !exists(filepath.Join(dir, oldDir)) {
		t.Fatal("store.old should remain")
	}
	_ = os.Chmod(filepath.Join(dir, oldDir), dirPerm)
	if b, err := s.ReadBlob(IdentityKey); err != nil || len(b) != 65 {
		t.Fatal("store unusable after swap", err)
	}
	// Recovery never touches the directories of a store that is in use.
	if err := Recover(dir); !errors.Is(err, ErrInUse) || !exists(filepath.Join(dir, oldDir)) {
		t.Fatal("Recover ran underneath an open store", err)
	}
	s.Close()
	if err := Recover(dir); err != nil || exists(filepath.Join(dir, oldDir)) {
		t.Fatal("Recover did not remove store.old", err)
	}
	s, err := Open(dir, pass("q"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ents, _ := os.ReadDir(s.Dir())
	for _, e := range ents {
		if len(e.Name()) > 4 && e.Name()[:5] == ".tmp-" {
			t.Fatal("temp file left behind")
		}
	}
	// Data dir is a file → Init cannot create directories.
	f := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(f, nil, filePerm); err != nil {
		t.Fatal(err)
	}
	if _, err := Init(f, pass("pw"), fast); err == nil {
		t.Fatal("Init on a file succeeded")
	}
	if _, err := Mode(f); err == nil {
		t.Fatal("Mode on a file succeeded")
	}
	// Corrupt header → Mode and Open refuse.
	d2 := t.TempDir()
	s2 := mustInit(t, d2, pass("pw"))
	s2.Close()
	hp := filepath.Join(d2, StoreDir, headerFile)
	if err := os.WriteFile(hp, []byte("junk"), filePerm); err != nil {
		t.Fatal(err)
	}
	if _, err := Mode(d2); !errors.Is(err, ErrHeader) {
		t.Fatal(err)
	}
	if _, err := Open(d2, pass("pw")); !errors.Is(err, ErrHeader) {
		t.Fatal(err)
	}
	// Header is a directory → read error surfaces.
	_ = os.Remove(hp)
	if err := os.Mkdir(hp, dirPerm); err != nil {
		t.Fatal(err)
	}
	if _, err := Mode(d2); err == nil || errors.Is(err, ErrNoStore) {
		t.Fatal(err)
	}
	if _, err := Open(d2, pass("pw")); err == nil || errors.Is(err, ErrNoStore) {
		t.Fatal(err)
	}
	// Recover cannot rename over an unwritable data dir.
	d3 := t.TempDir()
	if err := os.Mkdir(filepath.Join(d3, oldDir), dirPerm); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(d3, 0o500); err != nil {
		t.Fatal(err)
	}
	if err := Recover(d3); err == nil {
		t.Fatal("Recover in read-only dir succeeded")
	}
	_ = os.Chmod(d3, dirPerm)
}

func TestDefaultPathsEnv(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", "")
	t.Setenv("XDG_DATA_HOME", "")
	if p, err := DefaultPaths(); err != nil || p.Config == "" {
		t.Fatal(p, err)
	}
	t.Setenv("HOME", "")
	t.Setenv("USERPROFILE", "")
	t.Setenv("APPDATA", "")
	if _, err := DefaultPaths(); err == nil {
		t.Fatal("expected error without a home directory")
	}
}

func TestOnionKeyAndV1Upgrade(t *testing.T) {
	dir := t.TempDir()
	s := mustInit(t, dir, pass("pw"))
	defer s.Close()
	k1, err := s.LoadOnionKey()
	if err != nil || len(k1) != 64 {
		t.Fatal(err)
	}
	k2, _ := s.LoadOnionKey()
	if string(k1) != string(k2) {
		t.Fatal("onion key not stable")
	}
	id, _ := s.LoadIdentity()
	pub := id.Public()
	id.Clear()
	// Downgrade to a v1 blob (no onion seed): identity still loads, onion key is generated and persisted.
	b, _ := s.ReadBlob(IdentityKey)
	v1 := append([]byte{identityBlobV1}, b[1:33]...)
	if err := s.WriteBlob(IdentityKey, v1); err != nil {
		t.Fatal(err)
	}
	id2, err := s.LoadIdentity()
	if err != nil || id2.Public() != pub {
		t.Fatal("v1 identity", err)
	}
	k3, err := s.LoadOnionKey()
	if err != nil || string(k3) == string(k1) {
		t.Fatal("v1 upgrade", err)
	}
	if b2, _ := s.ReadBlob(IdentityKey); len(b2) != 65 || b2[0] != identityBlobV2 {
		t.Fatal("blob not upgraded")
	}
	if k4, _ := s.LoadOnionKey(); string(k4) != string(k3) {
		t.Fatal("upgraded key not persisted")
	}
}

// Blob operations and a passphrase change from different goroutines: every
// blob written before, during or after the change opens with the new key.
func TestPassphraseChangeUnderLoad(t *testing.T) {
	dir := t.TempDir()
	s := mustInit(t, dir, pass("old"))
	stop, done := make(chan struct{}), make(chan error, 1)
	go func() {
		for n := 0; ; n++ {
			select {
			case <-stop:
				done <- nil
				return
			default:
			}
			name := "partials/" + strconv.Itoa(n%8) + ".meta"
			if err := s.WriteBlob(name, []byte(name)); err != nil {
				done <- err
				return
			}
			if b, err := s.ReadBlob(name); err != nil || string(b) != name {
				done <- fmt.Errorf("%s: %q: %w", name, b, err)
				return
			}
		}
	}()
	for n := 0; n < 3; n++ {
		if err := s.ChangePassphrase(pass("new"), fast); err != nil {
			t.Fatal(err)
		}
	}
	close(stop)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	s.Close()
	s, err := Open(dir, pass("new"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	blobs, _ := s.ListBlobs("partials")
	for _, rel := range blobs {
		if b, err := s.ReadBlob(rel); err != nil || string(b) != rel {
			t.Fatalf("%s: %q %v", rel, b, err)
		}
	}
	if len(blobs) == 0 {
		t.Fatal("nothing was written")
	}
}

// A step of a passphrase change that fails while the process lives on. The
// old store is put back when it can be; when it cannot, the store closes
// itself and the error says which passphrase the next start will want. Never
// a fresh, empty store/, and never both passphrases or neither.
func TestSwapFailureInLivingProcess(t *testing.T) {
	defer func() { rename, syncDataDir, removeAll = os.Rename, syncDir, os.RemoveAll }()
	injected := errors.New("injected")
	type move struct{ from, to string }
	for _, c := range []struct {
		name       string
		failRename []move
		failSync   int  // the n-th sync of the data directory fails (0: none)
		keepNew    bool // removing store.new fails
		closed     bool // the store closed itself
		want       error
		newApplies bool
	}{
		{name: "step aside fails", failRename: []move{{StoreDir, oldDir}}, want: injected},
		{name: "step aside not durable", failSync: 1, want: injected},
		{name: "step aside not durable, no way back", failSync: 1, failRename: []move{{oldDir, StoreDir}},
			closed: true, want: ErrChangeFailedAtRestart},
		{name: "new store cannot move in", failRename: []move{{newDir, StoreDir}}, want: injected},
		{name: "new store cannot move in, no way back", failRename: []move{{newDir, StoreDir}, {oldDir, StoreDir}},
			closed: true, want: ErrChangeFailedAtRestart},
		{name: "no way back, and the new store cannot be removed", failRename: []move{{newDir, StoreDir}, {oldDir, StoreDir}},
			keepNew: true, closed: true, want: ErrChangeCompletesAtRestart, newApplies: true},
		{name: "moved in but not durable", failSync: 2, newApplies: true},
	} {
		t.Run(c.name, func(t *testing.T) {
			dir := t.TempDir()
			s := mustInit(t, dir, pass("old"))
			if err := s.WriteBlob("contacts", []byte("c")); err != nil {
				t.Fatal(err)
			}
			rename = func(from, to string) error {
				for _, m := range c.failRename {
					if filepath.Base(from) == m.from && filepath.Base(to) == m.to {
						return injected
					}
				}
				return os.Rename(from, to)
			}
			removeAll = func(d string) error {
				if c.keepNew && filepath.Base(d) == newDir {
					return injected
				}
				return os.RemoveAll(d)
			}
			syncs := 0
			syncDataDir = func(d string) error {
				if syncs++; syncs == c.failSync {
					return injected
				}
				return syncDir(d)
			}
			err := s.ChangePassphrase(pass("new"), fast)
			rename, syncDataDir, removeAll = os.Rename, syncDir, os.RemoveAll
			if !errors.Is(err, c.want) || (c.want == nil) != (err == nil) {
				t.Fatalf("got %v, want %v", err, c.want)
			}
			werr := s.WriteBlob("invites", []byte("i"))
			b, rerr := s.ReadBlob("contacts")
			if c.closed != errors.Is(werr, ErrClosed) || (!c.closed && (rerr != nil || string(b) != "c")) {
				t.Fatalf("closed=%v: write %v, read %q %v", c.closed, werr, b, rerr)
			}
			s.Close()
			sOld, errOld := Open(dir, pass("old"))
			sOld.Close()
			sNew, errNew := Open(dir, pass("new"))
			sNew.Close()
			if c.newApplies != (errNew == nil) || c.newApplies == (errOld == nil) {
				t.Fatalf("old passphrase: %v, new passphrase: %v", errOld, errNew)
			}
		})
	}
}

// A store.old left by an earlier change in the same process does not stand
// in the way of the next change.
func TestChangeAfterLeftover(t *testing.T) {
	dir := t.TempDir()
	s := mustInit(t, dir, pass("a"))
	defer s.Close()
	if err := os.MkdirAll(filepath.Join(dir, oldDir, "sub"), dirPerm); err != nil {
		t.Fatal(err)
	}
	if err := s.ChangePassphrase(pass("b"), fast); err != nil {
		t.Fatal(err)
	}
	if exists(filepath.Join(dir, oldDir)) {
		t.Fatal("store.old remains")
	}
}

// A failed init leaves no store, so that the next init can run.
func TestInitNotDurable(t *testing.T) {
	defer func() { syncDataDir = syncDir }()
	syncDataDir = func(string) error { return errors.New("injected") }
	dir := t.TempDir()
	if _, err := Init(dir, pass("pw"), fast); err == nil {
		t.Fatal("init reported success")
	}
	syncDataDir = syncDir
	s, err := Init(dir, pass("pw"), fast)
	if err != nil {
		t.Fatal(err)
	}
	s.Close()
}

func TestDeleteBlobInMissingDirectory(t *testing.T) {
	s := mustInit(t, t.TempDir(), pass("pw"))
	defer s.Close()
	if err := s.DeleteBlob("partials/none.meta"); err != nil {
		t.Fatal(err)
	}
}

// Nothing is touched by a second process while the first holds the store,
// whatever state the directories are in.
func TestSecondProcessLeavesSwapAlone(t *testing.T) {
	dir := t.TempDir()
	s := mustInit(t, dir, pass("pw"))
	defer s.Close()
	for _, d := range []string{newDir, oldDir, initDir} {
		if err := os.Mkdir(filepath.Join(dir, d), dirPerm); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := Mode(dir); !errors.Is(err, ErrInUse) {
		t.Fatal(err)
	}
	if _, err := Open(dir, pass("pw")); !errors.Is(err, ErrInUse) {
		t.Fatal(err)
	}
	if err := Recover(dir); !errors.Is(err, ErrInUse) {
		t.Fatal(err)
	}
	for _, d := range []string{newDir, oldDir, initDir} {
		if !exists(filepath.Join(dir, d)) {
			t.Fatalf("%s was removed underneath the running process", d)
		}
	}
}

// A process of a version that locked store/lock is still noticed.
func TestLegacyLockIsHonoured(t *testing.T) {
	dir := t.TempDir()
	mustInit(t, dir, pass("pw")).Close()
	old, err := acquireLock(filepath.Join(dir, StoreDir, lockFile))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Open(dir, pass("pw")); !errors.Is(err, ErrInUse) {
		t.Fatal(err)
	}
	_ = releaseLock(old)
	s, err := Open(dir, pass("pw"))
	if err != nil {
		t.Fatal(err)
	}
	s.Close()
}

// What an interrupted write left behind is no blob, and is cleaned up.
func TestLeftoverTempFiles(t *testing.T) {
	dir := t.TempDir()
	s := mustInit(t, dir, pass("pw"))
	tmp := filepath.Join(s.Dir(), tempPrefix+"0011223344556677")
	if err := os.WriteFile(tmp, []byte("half a blob"), filePerm); err != nil {
		t.Fatal(err)
	}
	if blobs, _ := s.ListBlobs(""); len(blobs) != 1 || blobs[0] != IdentityKey {
		t.Fatal(blobs)
	}
	if err := s.ChangePassphrase(pass("new"), fast); err != nil {
		t.Fatal(err)
	}
	s.Close()
	_ = os.WriteFile(tmp, []byte("half a blob"), filePerm)
	s, err := Open(dir, pass("new"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if exists(tmp) {
		t.Fatal("temporary file survived Open")
	}
}

func TestClosedStore(t *testing.T) {
	s := mustInit(t, t.TempDir(), pass("pw"))
	s.Close()
	if err := s.WriteBlob("contacts", []byte("x")); !errors.Is(err, ErrClosed) {
		t.Fatal(err)
	}
	if _, err := s.ReadBlob(IdentityKey); !errors.Is(err, ErrClosed) {
		t.Fatal(err)
	}
	if err := s.DeleteBlob("contacts"); !errors.Is(err, ErrClosed) {
		t.Fatal(err)
	}
	if _, err := s.ListBlobs(""); !errors.Is(err, ErrClosed) {
		t.Fatal(err)
	}
	if err := s.ChangePassphrase(pass("n"), fast); !errors.Is(err, ErrClosed) {
		t.Fatal(err)
	}
	if exists(filepath.Join(s.Dir(), "contacts")) {
		t.Fatal("a closed store wrote a blob")
	}
}

func TestBlobNames(t *testing.T) {
	for _, ok := range []string{"identity", "contacts", "partials/ab12.meta", "history/seg-3", "a.b_c-d"} {
		if !validRelPath(ok) {
			t.Errorf("%q refused", ok)
		}
	}
	for _, bad := range []string{"", ".", "..", "a/..", "/a", "a/", "a//b", "a/b/c", ".hidden", "partials/.tmp-00",
		"lock", "LOCK", "Lock", "partials/lock", "master.hdr", "MASTER.KEY", "Master.anything", "a b", "a\\b", "é"} {
		if validRelPath(bad) {
			t.Errorf("%q accepted", bad)
		}
	}
}
