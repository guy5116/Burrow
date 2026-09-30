package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"

	"golang.org/x/term"

	"github.com/guy5116/burrow/internal/secret"
	"github.com/guy5116/burrow/internal/store"
)

// readSecret prompts on the terminal without echo. It never reads flags or
// the environment. When no terminal is available it fails, unless allowStdin
// is set (used by `connect -`). When ctx ends first (Ctrl+C), the prompt is
// abandoned and the terminal gets its echo back.
func readSecret(ctx context.Context, prompt string, stdin io.Reader, stderr io.Writer, allowStdin bool) ([]byte, error) {
	if f, ok := stdin.(*os.File); ok && term.IsTerminal(int(f.Fd())) {
		fmt.Fprint(stderr, prompt)
		return readNoEcho(ctx, int(f.Fd()), stderr)
	}
	if !allowStdin {
		tty, err := os.OpenFile(ttyPath, os.O_RDWR, 0)
		if err != nil {
			return nil, errors.New("no terminal available to prompt securely")
		}
		defer func() { _ = tty.Close() }()
		fmt.Fprint(tty, prompt)
		return readNoEcho(ctx, int(tty.Fd()), tty)
	}
	// `connect -` with stdin that is no terminal (a pipe, a file). Read one
	// line byte by byte: a buffered reader would swallow the chat commands
	// that follow the invite on the same stream. The line is an invite, a
	// secret: it is never copied into a string, and its buffer never grows
	// (a grown slice would leave a copy behind).
	line := make([]byte, 0, maxSecretLine)
	var b [1]byte
	for {
		n, err := stdin.Read(b[:])
		if n == 1 {
			if b[0] == '\n' {
				break
			}
			if len(line) == maxSecretLine {
				secret.Wipe(line)
				return nil, errors.New("the first line of standard input is too long for an invite")
			}
			line = append(line, b[0])
		}
		if err != nil {
			if len(line) == 0 {
				return nil, errors.New("no invite on standard input")
			}
			break
		}
	}
	return bytes.TrimSuffix(line, []byte("\r")), nil
}

// maxSecretLine bounds a line read from a pipe: far more than any invite.
const maxSecretLine = 1024

// readNoEcho reads a line from a terminal without echo, unless ctx ends first.
func readNoEcho(ctx context.Context, fd int, out io.Writer) ([]byte, error) {
	state, err := term.GetState(fd)
	if err != nil {
		return nil, err
	}
	type result struct {
		b   []byte
		err error
	}
	done := make(chan result, 1)
	go func() {
		b, err := term.ReadPassword(fd)
		done <- result{b, err}
	}()
	select {
	case r := <-done:
		fmt.Fprintln(out)
		return r.b, r.err
	case <-ctx.Done():
		_ = term.Restore(fd, state) // the read is abandoned; the process is ending
		fmt.Fprintln(out)
		return nil, errors.New("cancelled")
	}
}

// unlock opens the store, prompting for the passphrase when the store needs one.
func (a *app) unlock(ctx context.Context, stdin io.Reader, stderr io.Writer) (*store.Store, error) {
	mode, err := store.Mode(a.paths.Data)
	if err != nil {
		return nil, err
	}
	var pw []byte
	if mode == store.ModePassphrase {
		if pw, err = readSecret(ctx, "Passphrase: ", stdin, stderr, false); err != nil {
			return nil, err
		}
		if len(pw) == 0 {
			secret.Wipe(pw)
			return nil, errors.New("empty passphrase")
		}
	} else {
		fmt.Fprintln(stderr, "WARNING: this store has no passphrase (--insecure-no-passphrase); anyone who can read your data directory can read your identity and contacts.")
	}
	return store.Open(a.paths.Data, pw)
}
