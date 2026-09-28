//go:build cli_wip

package main

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"golang.org/x/term"

	"github.com/guy5116/burrow/internal/secret"
	"github.com/guy5116/burrow/internal/store"
)

// readSecret prompts on the terminal without echo. It never reads flags or
// the environment. When no terminal is available it fails, unless allowStdin
// is set (used by `connect -`).
func readSecret(prompt string, stdin io.Reader, stderr io.Writer, allowStdin bool) ([]byte, error) {
	if f, ok := stdin.(*os.File); ok && term.IsTerminal(int(f.Fd())) {
		fmt.Fprint(stderr, prompt)
		b, err := term.ReadPassword(int(f.Fd()))
		fmt.Fprintln(stderr)
		return b, err
	}
	if tty, err := os.OpenFile(ttyPath, os.O_RDWR, 0); err == nil {
		defer tty.Close()
		fmt.Fprint(tty, prompt)
		b, err := term.ReadPassword(int(tty.Fd()))
		fmt.Fprintln(tty)
		return b, err
	}
	if !allowStdin {
		return nil, errors.New("no terminal available to prompt securely")
	}
	line, err := bufio.NewReader(stdin).ReadString('\n')
	if err != nil && line == "" {
		return nil, err
	}
	return []byte(strings.TrimRight(line, "\r\n")), nil
}

// unlock opens the store, prompting for the passphrase when the store needs one.
func (a *app) unlock(stdin io.Reader, stderr io.Writer) (*store.Store, error) {
	mode, err := store.Mode(a.paths.Data)
	if err != nil {
		return nil, err
	}
	var pw []byte
	if mode == store.ModePassphrase {
		if pw, err = readSecret("Passphrase: ", stdin, stderr, false); err != nil {
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
