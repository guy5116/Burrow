package store

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"runtime/debug"
	"strings"
	"sync"

	"golang.org/x/crypto/argon2"

	"github.com/guy5116/burrow/internal/identity"
	"github.com/guy5116/burrow/internal/secret"
)

// Directory and file names under the data directory.
const (
	StoreDir    = "store"
	initDir     = "store.init" // a store being created; removed by recovery
	newDir      = "store.new"
	oldDir      = "store.old"
	headerFile  = "master.hdr"
	keyFile     = "master.key"
	lockFile    = "lock"       // inside store/: the lock of versions before the store lock moved; still honoured
	dataLock    = "store.lock" // in the data directory: the lock
	IdentityKey = "identity"   // blob relpath
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
	// ErrClosed is returned by every operation on a store that was closed, or
	// that closed itself because a passphrase change left it unusable.
	ErrClosed = errors.New("store: closed")
)

// Store is an unlocked store. One process holds it at a time: the lock is
// <data>/store.lock, beside the directories that a passphrase change renames,
// so it is held across the whole change and across recovery.
type Store struct {
	mu     sync.RWMutex // blob operations share it; passphrase change and Close take it alone
	dir    string       // <data>/store
	hdr    Header
	master *secret.Buffer // nil once closed
	lock   *os.File
}

// lockData takes the store lock for dataDir and repairs an interrupted
// passphrase change. Nothing under dataDir is touched before the lock is
// held, so a second process can never disturb one that is running.
func lockData(dataDir string) (*os.File, error) {
	if !exists(dataDir) {
		return nil, ErrNoStore
	}
	lock, err := acquireLock(filepath.Join(dataDir, dataLock))
	if err != nil {
		return nil, err
	}
	// A process of an older version locks store/lock (or the copy that a
	// passphrase change has moved aside) and does not know this lock.
	for _, d := range []string{StoreDir, oldDir} {
		legacy := filepath.Join(dataDir, d, lockFile)
		if !exists(legacy) {
			continue
		}
		l, err := acquireLock(legacy)
		if err != nil {
			_ = releaseLock(lock)
			return nil, err
		}
		_ = releaseLock(l)
	}
	if err := recoverSwap(dataDir); err != nil {
		_ = releaseLock(lock)
		return nil, err
	}
	return lock, nil
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
// knows whether to prompt for a passphrase. It fails with ErrInUse when
// another process holds the store.
func Mode(dataDir string) (uint8, error) {
	lock, err := lockData(dataDir)
	if err != nil {
		return 0, err
	}
	defer func() { _ = releaseLock(lock) }()
	h, err := readHeader(filepath.Join(dataDir, StoreDir))
	return h.Mode, err
}

func readHeader(dir string) (Header, error) {
	b, err := os.ReadFile(filepath.Join(dir, headerFile)) // #nosec G304 -- our store directory
	if errors.Is(err, os.ErrNotExist) {
		return Header{}, ErrNoStore
	}
	if err != nil {
		return Header{}, err
	}
	return ParseHeader(b)
}

// Init creates a new store with a fresh identity. passphrase == nil selects
// ModeNone (--insecure-no-passphrase); empty but non-nil is an error. It
// refuses to run while store/, store.new/ or store.old/ exists.
func Init(dataDir string, passphrase []byte, opts Options) (*Store, error) {
	defer secret.Wipe(passphrase)
	if passphrase != nil && len(passphrase) == 0 {
		return nil, ErrPassphraseEmpty
	}
	if exists(filepath.Join(dataDir, StoreDir)) {
		return nil, ErrExists // said before the lock is tried: "exists" is the better answer than "in use"
	}
	if err := os.MkdirAll(dataDir, dirPerm); err != nil {
		return nil, err
	}
	lock, err := acquireLock(filepath.Join(dataDir, dataLock))
	if err != nil {
		return nil, err
	}
	s, err := initLocked(dataDir, passphrase, opts)
	if err != nil {
		_ = releaseLock(lock)
		return nil, err
	}
	s.lock = lock
	return s, nil
}

func initLocked(dataDir string, passphrase []byte, opts Options) (*Store, error) {
	for _, d := range []string{StoreDir, newDir, oldDir} {
		if exists(filepath.Join(dataDir, d)) {
			return nil, ErrExists
		}
	}
	id, err := identity.Generate()
	if err != nil {
		return nil, err
	}
	defer id.Clear()
	onionSeed, err := secret.Random(onionSeedLen)
	if err != nil {
		return nil, err
	}
	defer onionSeed.Clear()
	idBlob := encodeIdentity(id, onionSeed.Bytes())
	defer secret.Wipe(idBlob)

	tmp := filepath.Join(dataDir, initDir)
	_ = os.RemoveAll(tmp)
	master, hdr, err := buildStore(tmp, passphrase, opts.kdf(), map[string][]byte{IdentityKey: idBlob})
	if err != nil {
		_ = os.RemoveAll(tmp)
		return nil, err
	}
	dir := filepath.Join(dataDir, StoreDir)
	if err = rename(tmp, dir); err == nil {
		err = syncDir(dataDir)
	}
	if err != nil {
		master.Clear()
		_ = os.RemoveAll(tmp)
		return nil, err
	}
	return &Store{dir: dir, hdr: hdr, master: master}, nil
}

// rename is os.Rename; tests replace it to make a step of the swap fail.
var rename = os.Rename

// buildStore writes a complete store directory at dir: header, master.key for
// ModeNone and every blob; then fsyncs the tree. Returns the master key.
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
	// Only the sub-directory is created, never the store itself: if the store
	// directory is gone, writing must fail rather than start an empty one.
	if sub := filepath.Dir(p); sub != dir {
		if err := os.Mkdir(sub, dirPerm); err != nil && !errors.Is(err, os.ErrExist) {
			return err
		}
	}
	return writeFileAtomic(p, blob)
}

// Open takes the lock, runs recovery, parses the header, derives or loads the
// master key and verifies it by opening the identity blob. passphrase is
// wiped. For a ModePassphrase store an empty passphrase yields
// ErrPassphraseRequired; for ModeNone it is ignored.
func Open(dataDir string, passphrase []byte) (*Store, error) {
	defer secret.Wipe(passphrase)
	lock, err := lockData(dataDir)
	if err != nil {
		return nil, err
	}
	s, err := openLocked(filepath.Join(dataDir, StoreDir), passphrase)
	if err != nil {
		_ = releaseLock(lock)
		return nil, err
	}
	s.lock = lock
	return s, nil
}

func openLocked(dir string, passphrase []byte) (*Store, error) {
	hdr, err := readHeader(dir)
	if err != nil {
		return nil, err
	}
	removeTemps(dir)
	var master *secret.Buffer
	switch hdr.Mode {
	case ModeNone:
		kb, err := os.ReadFile(filepath.Join(dir, keyFile)) // #nosec G304 -- our store directory
		if err != nil || len(kb) != MasterKeyLen {
			return nil, ErrBlob
		}
		master = secret.From(kb)
	default:
		if len(passphrase) == 0 {
			return nil, ErrPassphraseRequired
		}
		master = deriveMaster(passphrase, hdr)
	}
	s := &Store{dir: dir, hdr: hdr, master: master}
	idb, err := s.ReadBlob(IdentityKey)
	if err != nil {
		master.Clear()
		return nil, err
	}
	secret.Wipe(idb)
	return s, nil
}

// removeTemps deletes what an interrupted atomic write left behind in the
// store directory and its sub-directories (blobs are at most one level down).
func removeTemps(dir string) {
	ents, _ := os.ReadDir(dir)
	for _, e := range ents {
		switch {
		case e.IsDir():
			sub, _ := os.ReadDir(filepath.Join(dir, e.Name()))
			for _, f := range sub {
				if !f.IsDir() && strings.HasPrefix(f.Name(), tempPrefix) {
					_ = os.Remove(filepath.Join(dir, e.Name(), f.Name()))
				}
			}
		case strings.HasPrefix(e.Name(), tempPrefix):
			_ = os.Remove(filepath.Join(dir, e.Name()))
		}
	}
}

// Close wipes the master key and releases the lock. Operations on a closed
// store fail with ErrClosed.
func (s *Store) Close() {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closeLocked()
}

func (s *Store) closeLocked() {
	if s.master != nil {
		s.master.Clear()
		s.master = nil
	}
	_ = releaseLock(s.lock)
	s.lock = nil
}

// Mode returns the unlock mode of the open store.
func (s *Store) Mode() uint8 {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.hdr.Mode
}

// Dir returns the store directory.
func (s *Store) Dir() string { return s.dir }

// ReadBlob decrypts the blob at relpath. Callers wipe the result when it holds secrets.
func (s *Store) ReadBlob(relpath string) ([]byte, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.readBlob(relpath)
}

func (s *Store) readBlob(relpath string) ([]byte, error) {
	if s.master == nil {
		return nil, ErrClosed
	}
	if !validRelPath(relpath) {
		return nil, ErrRelPath
	}
	b, err := os.ReadFile(filepath.Join(s.dir, filepath.FromSlash(relpath))) // #nosec G304 -- relpath is validated
	if err != nil {
		return nil, err
	}
	return openBlob(s.master, relpath, b)
}

// WriteBlob atomically writes an encrypted blob at relpath.
func (s *Store) WriteBlob(relpath string, plaintext []byte) error {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.master == nil {
		return ErrClosed
	}
	return writeBlob(s.dir, s.master, relpath, plaintext)
}

// DeleteBlob removes a blob; a missing blob is not an error.
func (s *Store) DeleteBlob(relpath string) error {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.master == nil {
		return ErrClosed
	}
	if !validRelPath(relpath) {
		return ErrRelPath
	}
	p := filepath.Join(s.dir, filepath.FromSlash(relpath))
	if err := os.Remove(p); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return syncDir(filepath.Dir(p))
}

// ListBlobs returns the relpaths of blobs directly under subdir (e.g. "partials").
func (s *Store) ListBlobs(subdir string) ([]string, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.master == nil {
		return nil, ErrClosed
	}
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
		if validRelPath(rel) { // the header, key and lock files are not valid blob names
			out = append(out, rel)
		}
	}
	return out, nil
}

// Identity blob: version u8 = 2 || scalar[32] || onion seed[32]. Version 1
// (Phase 0–3 stores) lacks the onion seed and is upgraded on first open.
const (
	identityBlobV1 = 1
	identityBlobV2 = 2
	onionSeedLen   = 32
)

func encodeIdentity(id *identity.Identity, onionSeed []byte) []byte {
	b := append([]byte{identityBlobV2}, id.Scalar()...)
	return append(b, onionSeed...)
}

// LoadIdentity decrypts and returns the long-term identity.
func (s *Store) LoadIdentity() (*identity.Identity, error) {
	b, err := s.ReadBlob(IdentityKey)
	if err != nil {
		return nil, err
	}
	defer secret.Wipe(b)
	switch {
	case len(b) == 33 && b[0] == identityBlobV1, len(b) == 65 && b[0] == identityBlobV2:
		return identity.FromScalar(append([]byte(nil), b[1:33]...))
	}
	return nil, ErrBlob
}

// LoadOnionKey returns the ed25519 private key of the onion service, generating
// and persisting one (upgrading a v1 identity blob) when absent.
func (s *Store) LoadOnionKey() (ed25519.PrivateKey, error) {
	b, err := s.ReadBlob(IdentityKey)
	if err != nil {
		return nil, err
	}
	defer secret.Wipe(b)
	switch {
	case len(b) == 65 && b[0] == identityBlobV2:
		return ed25519.NewKeyFromSeed(b[33:]), nil
	case len(b) == 33 && b[0] == identityBlobV1:
		seed, err := secret.Random(onionSeedLen)
		if err != nil {
			return nil, err
		}
		defer seed.Clear()
		nb := append([]byte{identityBlobV2}, b[1:33]...)
		nb = append(nb, seed.Bytes()...)
		defer secret.Wipe(nb)
		if err := s.WriteBlob(IdentityKey, nb); err != nil {
			return nil, err
		}
		return ed25519.NewKeyFromSeed(seed.Bytes()), nil
	}
	return nil, ErrBlob
}

// swapStep is a test hook invoked before each step of the directory swap;
// returning an error simulates a crash at that point.
var swapStep = func(int) error { return nil }

// ChangePassphrase re-encrypts every blob under a new master key (new
// passphrase, or ModeNone when newPassphrase is nil) using the atomic
// directory swap of §7. Nothing else reads or writes the store meanwhile. The
// store stays open and usable afterwards; if the change fails half way and
// cannot be undone, the store closes itself, and the next Open repairs it.
func (s *Store) ChangePassphrase(newPassphrase []byte, opts Options) error {
	defer secret.Wipe(newPassphrase)
	if newPassphrase != nil && len(newPassphrase) == 0 {
		return ErrPassphraseEmpty
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.master == nil {
		return ErrClosed
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
		if !validRelPath(rel) {
			return nil // header, key, lock, temporary files
		}
		pt, err := s.readBlob(rel)
		if err != nil {
			return err
		}
		blobs[rel] = pt
		return nil
	}); err != nil {
		return err
	}

	// A failing swapStep stands for the process dying at that point: no
	// cleanup happens, and the store is unusable until recovery has run.
	crash := func(step int, master *secret.Buffer) error {
		err := swapStep(step)
		if err != nil {
			if master != nil {
				master.Clear()
			}
			s.closeLocked()
		}
		return err
	}
	if err := crash(0, nil); err != nil {
		return err
	}
	master, hdr, err := buildStore(nd, newPassphrase, opts.kdf(), blobs)
	if err != nil {
		_ = os.RemoveAll(nd)
		return err
	}
	if err := crash(1, master); err != nil { // store.new complete, nothing renamed yet
		return err
	}
	if err = rename(s.dir, od); err == nil {
		err = syncDir(dataDir)
	}
	if err != nil {
		master.Clear()
		_ = rename(od, s.dir) // if the rename itself failed there is nothing to undo
		_ = os.RemoveAll(nd)
		return err
	}
	if err := crash(2, master); err != nil { // store.old + store.new, no store
		return err
	}
	if err = rename(nd, s.dir); err == nil {
		err = syncDir(dataDir)
	}
	if err != nil {
		master.Clear()
		// Back to the old store. If that fails too, this process must not
		// write another byte: recovery at the next start sorts it out.
		_ = rename(s.dir, nd)
		if rename(od, s.dir) != nil {
			s.closeLocked()
		} else {
			_ = os.RemoveAll(nd)
		}
		return err
	}
	if err := crash(3, master); err != nil { // store + store.old
		return err
	}
	s.master.Clear()
	s.master, s.hdr = master, hdr
	if err := os.RemoveAll(od); err != nil {
		return ErrOldStoreRemains
	}
	return syncDir(dataDir)
}

// Recover repairs an interrupted directory swap (§7). It requires no key. It
// fails with ErrInUse while another process holds the store, whose
// directories it must not touch.
func Recover(dataDir string) error {
	lock, err := lockData(dataDir)
	if errors.Is(err, ErrNoStore) {
		return nil // no data directory: nothing to repair
	}
	if err != nil {
		return err
	}
	return releaseLock(lock)
}

// recoverSwap is Recover for a caller that holds the lock.
func recoverSwap(dataDir string) error {
	st, nd, od := filepath.Join(dataDir, StoreDir), filepath.Join(dataDir, newDir), filepath.Join(dataDir, oldDir)
	_ = os.RemoveAll(filepath.Join(dataDir, initDir))
	hasStore, hasNew, hasOld := exists(st), exists(nd), exists(od)
	if !hasStore {
		switch {
		case hasNew && hasOld:
			// The new store was complete before the first rename: roll forward.
			if err := rename(nd, st); err != nil {
				return err
			}
		case hasOld:
			if err := rename(od, st); err != nil {
				return err
			}
		}
	}
	// With store/ in place (or nothing to save), any leftover is stale.
	// store.new alone is incomplete by construction.
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
