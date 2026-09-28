package store

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"runtime/debug"

	"golang.org/x/crypto/argon2"

	"github.com/guy5116/burrow/internal/identity"
	"github.com/guy5116/burrow/internal/secret"
)

// Directory and file names under the data directory.
const (
	StoreDir    = "store"
	newDir      = "store.new"
	oldDir      = "store.old"
	headerFile  = "master.hdr"
	keyFile     = "master.key"
	lockFile    = "lock"
	IdentityKey = "identity" // blob relpath
)

// Errors reported to the user.
var (
	ErrInUse              = errors.New("store: store in use by another process")
	ErrExists             = errors.New("store: a store already exists")
	ErrNoStore            = errors.New("store: no store found; run `burrow init` first")
	ErrPassphraseRequired = errors.New("store: passphrase required")
	ErrPassphraseEmpty    = errors.New("store: passphrase must not be empty")
	// ErrOldStoreRemains means the passphrase change is complete and durable,
	// but store.old (encrypted under the previous key) could not be deleted;
	// Recover removes it at the next start.
	ErrOldStoreRemains = errors.New("store: passphrase changed, but the old store copy could not be removed")
)

// Store is an unlocked store. One process holds it at a time.
type Store struct {
	dir    string // <data>/store
	hdr    Header
	master *secret.Buffer
	lock   *os.File
}

// Options for Init and ChangePassphrase.
type Options struct {
	// Argon2 overrides DefaultArgon2 (tests use smaller parameters).
	Argon2 Argon2
}

func (o Options) kdf() Argon2 {
	if o.Argon2 == (Argon2{}) {
		return DefaultArgon2
	}
	return o.Argon2
}

// Mode reads the header and reports which unlock mode the store uses, so a UI
// knows whether to prompt for a passphrase. Runs recovery first.
func Mode(dataDir string) (uint8, error) {
	if err := Recover(dataDir); err != nil {
		return 0, err
	}
	b, err := os.ReadFile(filepath.Join(dataDir, StoreDir, headerFile))
	if errors.Is(err, os.ErrNotExist) {
		return 0, ErrNoStore
	}
	if err != nil {
		return 0, err
	}
	h, err := ParseHeader(b)
	if err != nil {
		return 0, err
	}
	return h.Mode, nil
}

// Init creates a new store with a fresh identity. passphrase == nil selects
// ModeNone (--insecure-no-passphrase); empty but non-nil is an error. It
// refuses to run while store/, store.new/ or store.old/ exists.
func Init(dataDir string, passphrase []byte, opts Options) (*Store, error) {
	defer secret.Wipe(passphrase)
	for _, d := range []string{StoreDir, newDir, oldDir} {
		if exists(filepath.Join(dataDir, d)) {
			return nil, ErrExists
		}
	}
	if passphrase != nil && len(passphrase) == 0 {
		return nil, ErrPassphraseEmpty
	}
	if err := os.MkdirAll(dataDir, dirPerm); err != nil {
		return nil, err
	}
	id, err := identity.Generate()
	if err != nil {
		return nil, err
	}
	defer id.Clear()
	idBlob := encodeIdentity(id)
	defer secret.Wipe(idBlob)

	tmp := filepath.Join(dataDir, "store.init")
	_ = os.RemoveAll(tmp)
	master, hdr, err := buildStore(tmp, passphrase, opts.kdf(), map[string][]byte{IdentityKey: idBlob})
	if err != nil {
		_ = os.RemoveAll(tmp)
		return nil, err
	}
	dir := filepath.Join(dataDir, StoreDir)
	if err := os.Rename(tmp, dir); err != nil {
		master.Clear()
		_ = os.RemoveAll(tmp)
		return nil, err
	}
	if err := syncDir(dataDir); err != nil {
		master.Clear()
		return nil, err
	}
	lock, err := acquireLock(filepath.Join(dir, lockFile))
	if err != nil {
		master.Clear()
		return nil, err
	}
	return &Store{dir: dir, hdr: hdr, master: master, lock: lock}, nil
}

// buildStore writes a complete store directory at dir: header, master.key for
// ModeNone, every blob, and a lock file; then fsyncs the tree. Returns the master key.
func buildStore(dir string, passphrase []byte, kdf Argon2, blobs map[string][]byte) (*secret.Buffer, Header, error) {
	if err := os.MkdirAll(dir, dirPerm); err != nil {
		return nil, Header{}, err
	}
	hdr := Header{Mode: ModePassphrase, KDF: kdf}
	if passphrase == nil {
		hdr.Mode = ModeNone
	}
	if _, err := rand.Read(hdr.Salt[:]); err != nil {
		return nil, Header{}, err
	}
	hb, err := EncodeHeader(hdr)
	if err != nil {
		return nil, Header{}, err
	}
	var master *secret.Buffer
	if hdr.Mode == ModeNone {
		if master, err = secret.Random(MasterKeyLen); err != nil {
			return nil, Header{}, err
		}
		if err := writeFileAtomic(filepath.Join(dir, keyFile), master.Bytes()); err != nil {
			master.Clear()
			return nil, Header{}, err
		}
	} else {
		master = deriveMaster(passphrase, hdr)
	}
	if err := writeFileAtomic(filepath.Join(dir, headerFile), hb); err != nil {
		master.Clear()
		return nil, Header{}, err
	}
	for rel, pt := range blobs {
		if err := writeBlob(dir, master, rel, pt); err != nil {
			master.Clear()
			return nil, Header{}, err
		}
	}
	if err := writeFileAtomic(filepath.Join(dir, lockFile), nil); err != nil {
		master.Clear()
		return nil, Header{}, err
	}
	if err := syncTree(dir); err != nil {
		master.Clear()
		return nil, Header{}, err
	}
	return master, hdr, nil
}

func deriveMaster(passphrase []byte, hdr Header) *secret.Buffer {
	k := argon2.IDKey(passphrase, hdr.Salt[:], hdr.KDF.Time, hdr.KDF.MemKiB, hdr.KDF.Threads, MasterKeyLen)
	m := secret.From(k)
	debug.FreeOSMemory() // return Argon2's working memory (§7)
	return m
}

func writeBlob(dir string, master *secret.Buffer, rel string, pt []byte) error {
	if !validRelPath(rel) {
		return ErrRelPath
	}
	blob, err := sealBlob(master, rel, pt)
	if err != nil {
		return err
	}
	p := filepath.Join(dir, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(p), dirPerm); err != nil {
		return err
	}
	return writeFileAtomic(p, blob)
}

// Open runs recovery, parses the header, derives or loads the master key,
// takes the exclusive lock and verifies the key by opening the identity blob.
// passphrase is wiped. For a ModePassphrase store an empty passphrase yields
// ErrPassphraseRequired; for ModeNone it is ignored.
func Open(dataDir string, passphrase []byte) (*Store, error) {
	defer secret.Wipe(passphrase)
	if err := Recover(dataDir); err != nil {
		return nil, err
	}
	dir := filepath.Join(dataDir, StoreDir)
	hb, err := os.ReadFile(filepath.Join(dir, headerFile))
	if errors.Is(err, os.ErrNotExist) {
		return nil, ErrNoStore
	}
	if err != nil {
		return nil, err
	}
	hdr, err := ParseHeader(hb)
	if err != nil {
		return nil, err
	}
	lock, err := acquireLock(filepath.Join(dir, lockFile))
	if err != nil {
		return nil, err
	}
	var master *secret.Buffer
	switch hdr.Mode {
	case ModeNone:
		kb, err := os.ReadFile(filepath.Join(dir, keyFile))
		if err != nil || len(kb) != MasterKeyLen {
			_ = releaseLock(lock)
			return nil, ErrBlob
		}
		master = secret.From(kb)
	default:
		if len(passphrase) == 0 {
			_ = releaseLock(lock)
			return nil, ErrPassphraseRequired
		}
		master = deriveMaster(passphrase, hdr)
	}
	s := &Store{dir: dir, hdr: hdr, master: master, lock: lock}
	idb, err := s.ReadBlob(IdentityKey)
	if err != nil {
		s.Close()
		return nil, err
	}
	secret.Wipe(idb)
	return s, nil
}

// Close wipes the master key and releases the lock.
func (s *Store) Close() {
	if s == nil {
		return
	}
	s.master.Clear()
	_ = releaseLock(s.lock)
	s.lock = nil
}

// Mode returns the unlock mode of the open store.
func (s *Store) Mode() uint8 { return s.hdr.Mode }

// Dir returns the store directory.
func (s *Store) Dir() string { return s.dir }

// ReadBlob decrypts the blob at relpath. Callers wipe the result when it holds secrets.
func (s *Store) ReadBlob(relpath string) ([]byte, error) {
	if !validRelPath(relpath) {
		return nil, ErrRelPath
	}
	b, err := os.ReadFile(filepath.Join(s.dir, filepath.FromSlash(relpath)))
	if err != nil {
		return nil, err
	}
	return openBlob(s.master, relpath, b)
}

// WriteBlob atomically writes an encrypted blob at relpath.
func (s *Store) WriteBlob(relpath string, plaintext []byte) error {
	return writeBlob(s.dir, s.master, relpath, plaintext)
}

// DeleteBlob removes a blob; a missing blob is not an error.
func (s *Store) DeleteBlob(relpath string) error {
	if !validRelPath(relpath) {
		return ErrRelPath
	}
	err := os.Remove(filepath.Join(s.dir, filepath.FromSlash(relpath)))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return syncDir(filepath.Dir(filepath.Join(s.dir, filepath.FromSlash(relpath))))
}

// ListBlobs returns the relpaths of blobs directly under subdir (e.g. "partials").
func (s *Store) ListBlobs(subdir string) ([]string, error) {
	if subdir != "" && !validRelPath(subdir) {
		return nil, ErrRelPath
	}
	ents, err := os.ReadDir(filepath.Join(s.dir, filepath.FromSlash(subdir)))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []string
	for _, e := range ents {
		if e.IsDir() {
			continue
		}
		rel := e.Name()
		if subdir != "" {
			rel = subdir + "/" + rel
		}
		if validRelPath(rel) && (subdir != "" || rel != headerFile && rel != keyFile && rel != lockFile) {
			out = append(out, rel)
		}
	}
	return out, nil
}

// Identity blob: version u8 = 1 || scalar[32]. Phase 4 adds the onion key.
const identityBlobVersion = 1

func encodeIdentity(id *identity.Identity) []byte {
	return append([]byte{identityBlobVersion}, id.Scalar()...)
}

// LoadIdentity decrypts and returns the long-term identity.
func (s *Store) LoadIdentity() (*identity.Identity, error) {
	b, err := s.ReadBlob(IdentityKey)
	if err != nil {
		return nil, err
	}
	defer secret.Wipe(b)
	if len(b) != 33 || b[0] != identityBlobVersion {
		return nil, ErrBlob
	}
	return identity.FromScalar(append([]byte(nil), b[1:]...))
}

// swapStep is a test hook invoked before each step of the directory swap;
// returning an error simulates a crash at that point.
var swapStep = func(int) error { return nil }

// ChangePassphrase re-encrypts every blob under a new master key (new
// passphrase, or ModeNone when newPassphrase is nil) using the atomic
// directory swap of §7. The store stays open and usable afterwards.
func (s *Store) ChangePassphrase(newPassphrase []byte, opts Options) error {
	defer secret.Wipe(newPassphrase)
	if newPassphrase != nil && len(newPassphrase) == 0 {
		return ErrPassphraseEmpty
	}
	dataDir := filepath.Dir(s.dir)
	nd, od := filepath.Join(dataDir, newDir), filepath.Join(dataDir, oldDir)
	_ = os.RemoveAll(nd)

	blobs := map[string][]byte{}
	defer func() {
		for _, b := range blobs {
			secret.Wipe(b)
		}
	}()
	if err := filepath.WalkDir(s.dir, func(p string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		rel, _ := filepath.Rel(s.dir, p)
		rel = filepath.ToSlash(rel)
		if rel == headerFile || rel == keyFile || rel == lockFile || !validRelPath(rel) {
			return nil
		}
		pt, err := s.ReadBlob(rel)
		if err != nil {
			return err
		}
		blobs[rel] = pt
		return nil
	}); err != nil {
		return err
	}

	if err := swapStep(0); err != nil {
		return err
	}
	master, hdr, err := buildStore(nd, newPassphrase, opts.kdf(), blobs)
	if err != nil {
		_ = os.RemoveAll(nd)
		return err
	}
	if err := swapStep(1); err != nil { // store.new complete, nothing renamed yet
		master.Clear()
		return err
	}
	if err := os.Rename(s.dir, od); err != nil {
		master.Clear()
		_ = os.RemoveAll(nd)
		return err
	}
	_ = syncDir(dataDir)
	if err := swapStep(2); err != nil { // store.old + store.new, no store
		master.Clear()
		return err
	}
	if err := os.Rename(nd, s.dir); err != nil {
		master.Clear()
		return err
	}
	_ = syncDir(dataDir)
	if err := swapStep(3); err != nil { // store + store.old
		master.Clear()
		return err
	}
	// Take the new lock before releasing the old one (Windows cannot delete an open file).
	lock, err := acquireLock(filepath.Join(s.dir, lockFile))
	if err != nil {
		master.Clear()
		return err
	}
	_ = releaseLock(s.lock)
	s.lock = lock
	s.master.Clear()
	s.master, s.hdr = master, hdr
	if err := os.RemoveAll(od); err != nil {
		return ErrOldStoreRemains
	}
	return syncDir(dataDir)
}

// Recover repairs an interrupted directory swap (§7). It is safe to call at
// every startup and requires no key.
func Recover(dataDir string) error {
	st, nd, od := filepath.Join(dataDir, StoreDir), filepath.Join(dataDir, newDir), filepath.Join(dataDir, oldDir)
	_ = os.RemoveAll(filepath.Join(dataDir, "store.init"))
	hasStore, hasNew, hasOld := exists(st), exists(nd), exists(od)
	if !hasStore {
		switch {
		case hasNew && hasOld:
			// The new store was complete before the first rename: roll forward.
			if err := os.Rename(nd, st); err != nil {
				return err
			}
		case hasOld:
			if err := os.Rename(od, st); err != nil {
				return err
			}
		case hasNew:
			// Unreachable from the swap sequence; store.new alone is incomplete by construction.
			if err := os.RemoveAll(nd); err != nil {
				return err
			}
		}
	}
	// With store/ in place, any leftover is stale.
	if err := os.RemoveAll(nd); err != nil {
		return err
	}
	if err := os.RemoveAll(od); err != nil {
		return err
	}
	if hasNew || hasOld {
		return syncDir(dataDir)
	}
	return nil
}

// RandomID returns a hex string of n random bytes for opaque file names.
func RandomID(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}
