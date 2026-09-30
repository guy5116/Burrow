package main

import (
	"context"
	"os"
	"strconv"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// Ctrl+C at a passphrase prompt abandons it and gives the terminal its echo
// back: without that, the shell the user returns to shows nothing they type.
func TestPromptGivesUpOnCancel(t *testing.T) {
	ptmx, err := os.OpenFile("/dev/ptmx", os.O_RDWR|unix.O_NOCTTY, 0)
	if err != nil {
		t.Skip("no pseudo-terminals here:", err)
	}
	defer func() { _ = ptmx.Close() }()
	if err := unix.IoctlSetPointerInt(int(ptmx.Fd()), unix.TIOCSPTLCK, 0); err != nil {
		t.Fatal(err)
	}
	n, err := unix.IoctlGetInt(int(ptmx.Fd()), unix.TIOCGPTN)
	if err != nil {
		t.Fatal(err)
	}
	pts, err := os.OpenFile("/dev/pts/"+strconv.Itoa(n), os.O_RDWR|unix.O_NOCTTY, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = pts.Close() }()
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if _, err := readNoEcho(ctx, int(pts.Fd()), os.Stderr); err == nil || err.Error() != "cancelled" {
		t.Fatal(err)
	}
	tio, err := unix.IoctlGetTermios(int(pts.Fd()), unix.TCGETS)
	if err != nil {
		t.Fatal(err)
	}
	if tio.Lflag&unix.ECHO == 0 {
		t.Fatal("echo left off")
	}
}
