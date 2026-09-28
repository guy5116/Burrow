package store

import (
	"crypto/hkdf"
	"crypto/rand"
	"crypto/sha256"
	"errors"

	"golang.org/x/crypto/chacha20poly1305"

	"github.com/guy5116/burrow/internal/secret"
)

// Blob layout: magic "BRWS" || version u8 || nonce[24] || XChaCha20-Poly1305(file_key, nonce, ad, plaintext)
// ad = "BRWS" || version || nonce || relpath ; file_key = HKDF-Expand(master, "burrow/1 store " || relpath, 32).
const (
	blobMagic     = "BRWS"
	blobVersion   = 1
	blobNonceLen  = chacha20poly1305.NonceSizeX
	blobHeaderLen = 4 + 1 + blobNonceLen
	blobKeyInfo   = "burrow/1 store "
	// MaxBlobLen bounds a blob read from disk (contacts are capped at 1,024; images never live in blobs).
	MaxBlobLen = 16 << 20
)

// ErrBlob is returned when a blob cannot be authenticated: wrong key
// (passphrase), tampering, or a copied/renamed file.
var ErrBlob = errors.New("store: cannot open blob (wrong passphrase or corrupted store)")

func fileKey(master *secret.Buffer, relpath string) (*secret.Buffer, error) {
	k, err := hkdf.Expand(sha256.New, master.Bytes(), blobKeyInfo+relpath, chacha20poly1305.KeySize)
	if err != nil {
		return nil, err
	}
	return secret.From(k), nil
}

// sealBlob encrypts plaintext for relpath under master.
func sealBlob(master *secret.Buffer, relpath string, plaintext []byte) ([]byte, error) {
	fk, err := fileKey(master, relpath)
	if err != nil {
		return nil, err
	}
	defer fk.Clear()
	aead, err := chacha20poly1305.NewX(fk.Bytes())
	if err != nil {
		return nil, err
	}
	out := make([]byte, blobHeaderLen, blobHeaderLen+len(plaintext)+aead.Overhead())
	copy(out, blobMagic)
	out[4] = blobVersion
	if _, err := rand.Read(out[5:blobHeaderLen]); err != nil {
		return nil, err
	}
	ad := append(append([]byte{}, out[:blobHeaderLen]...), relpath...)
	return aead.Seal(out, out[5:blobHeaderLen], plaintext, ad), nil
}

// openBlob authenticates and decrypts a blob read from relpath.
func openBlob(master *secret.Buffer, relpath string, blob []byte) ([]byte, error) {
	if len(blob) < blobHeaderLen+chacha20poly1305.Overhead || len(blob) > MaxBlobLen ||
		string(blob[:4]) != blobMagic || blob[4] != blobVersion {
		return nil, ErrBlob
	}
	fk, err := fileKey(master, relpath)
	if err != nil {
		return nil, err
	}
	defer fk.Clear()
	aead, err := chacha20poly1305.NewX(fk.Bytes())
	if err != nil {
		return nil, err
	}
	ad := append(append([]byte{}, blob[:blobHeaderLen]...), relpath...)
	pt, err := aead.Open(nil, blob[5:blobHeaderLen], blob[blobHeaderLen:], ad)
	if err != nil {
		return nil, ErrBlob
	}
	return pt, nil
}
