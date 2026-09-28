package secret

import (
	"bytes"
	"testing"
)

func TestBuffer(t *testing.T) {
	src := []byte{1, 2, 3, 4}
	s := From(src)
	if !bytes.Equal(src, make([]byte, 4)) {
		t.Fatal("From must wipe its input")
	}
	if !bytes.Equal(s.Bytes(), []byte{1, 2, 3, 4}) || s.Len() != 4 {
		t.Fatal("copy mismatch")
	}
	s.Clear()
	if !bytes.Equal(s.Bytes(), make([]byte, 4)) {
		t.Fatal("Clear must zero")
	}
	s.Clear()
	var nilBuf *Buffer
	nilBuf.Clear()
	if nilBuf.Bytes() != nil || nilBuf.Len() != 0 {
		t.Fatal("nil Buffer must be inert")
	}
	r, err := Random(32)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(r.Bytes(), make([]byte, 32)) {
		t.Fatal("Random returned zeros")
	}
}
