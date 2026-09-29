package media

import (
	"bytes"
	"crypto/rand"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/guy5116/burrow/internal/wire"
	"golang.org/x/crypto/blake2b"
)

func TestPrepareFileAndStream(t *testing.T) {
	data := make([]byte, 2*wire.ChunkData+123)
	_, _ = rand.Read(data)
	path := filepath.Join(t.TempDir(), "Backup.Tar.GZ")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	p, err := PrepareFile(path, 0)
	if err != nil {
		t.Fatal(err)
	}
	if p.Size != uint64(len(data)) || p.Hash != blake2b.Sum256(data) || p.Format != FormatFile || p.Ext != "gz" {
		t.Fatalf("%+v", p)
	}
	var got bytes.Buffer
	chunks := uint32(0)
	err = Stream(p, func(i uint32, c []byte) error {
		if i != chunks {
			t.Fatalf("chunk %d out of order", i)
		}
		chunks++
		got.Write(c)
		return nil
	})
	if err != nil || chunks != 3 || !bytes.Equal(got.Bytes(), data) {
		t.Fatal(err, chunks)
	}
	// The file changes between the passes.
	data[5] ^= 1
	_ = os.WriteFile(path, data, 0o600)
	if err := Stream(p, func(uint32, []byte) error { return nil }); !errors.Is(err, ErrDiverged) {
		t.Fatal(err)
	}
}

func TestPrepareFileRefusals(t *testing.T) {
	dir := t.TempDir()
	big := filepath.Join(dir, "big.zip")
	_ = os.WriteFile(big, make([]byte, 2048), 0o600)
	if _, err := PrepareFile(big, 1024); !errors.Is(err, ErrTooLarge) {
		t.Fatal(err)
	}
	empty := filepath.Join(dir, "empty.pdf")
	_ = os.WriteFile(empty, nil, 0o600)
	if _, err := PrepareFile(empty, 0); !errors.Is(err, ErrEmpty) {
		t.Fatal(err)
	}
	if _, err := PrepareFile(dir, 0); !errors.Is(err, ErrUnsupported) {
		t.Fatal(err)
	}
	if _, err := PrepareFile(filepath.Join(dir, "missing"), 0); !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
}

func TestFileExt(t *testing.T) {
	for path, want := range map[string]string{
		"a.zip": "zip", "a.RAR": "rar", "a.tar.gz": "gz", "a.7z": "7z", "noext": "", "dot.": "",
		"a.tönnchen": "", "a.waytoolongext": "", "a.b c": "", ".bashrc": "bashrc", "a.p/df": "",
	} {
		if got := FileExt(path); got != want {
			t.Errorf("%q: %q want %q", path, got, want)
		}
	}
}
