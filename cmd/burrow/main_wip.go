//go:build cli_wip

// Command burrow is the CLI/TUI for Burrow (CLAUDE.md §9).
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	"github.com/guy5116/burrow/internal/store"
)

const usage = `usage: burrow [global flags] <command> [args]

commands:
  init [--insecure-no-passphrase]      create an identity (refuses if a store exists)
  id [--qr]                            print your fingerprint
  invite [--ttl 1h] [--multi-use] [--qr] [--host H]
  listen                               listen for peers and open the chat
  connect [<contact>|<invite>|-]       connect (and keep listening); no argument prompts for an invite
  contacts list|verify|rename|remove|block|unblock <contact> [name]
  config get|set <key> [value]
  passphrase [--insecure-no-passphrase]   change the passphrase or mode
  burn                                 wipe optional history and partial transfers

global flags:
  --plain        line mode: no alternate screen, stdin commands, stdout events
  --json         with --plain: newline-delimited JSON events on stdout
  --log-level    debug|info|warn|error (default warn), on stderr
  --log-file     append logs to a file instead of stderr (never contains content)
  --config DIR   config directory
  --data DIR     data directory
`

type app struct {
	plain, json bool
	paths       store.Paths
	cfg         store.Config
	log         *slog.Logger
	logClose    io.Closer
}

func main() {
	harden()
	os.Exit(run(os.Args[1:], os.Stdin, os.Stdout, os.Stderr))
}

func run(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("burrow", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() { fmt.Fprint(stderr, usage) }
	a := &app{}
	var level, logFile, cfgDir, dataDir string
	fs.BoolVar(&a.plain, "plain", false, "")
	fs.BoolVar(&a.json, "json", false, "")
	fs.StringVar(&level, "log-level", "warn", "")
	fs.StringVar(&logFile, "log-file", "", "")
	fs.StringVar(&cfgDir, "config", "", "")
	fs.StringVar(&dataDir, "data", "", "")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() == 0 {
		fs.Usage()
		return 2
	}
	if a.json {
		a.plain = true
	}
	var err error
	a.paths, err = store.DefaultPaths()
	if err != nil && (cfgDir == "" || dataDir == "") {
		fmt.Fprintln(stderr, "burrow: cannot determine home directory; pass --config and --data")
		return 1
	}
	if cfgDir != "" {
		a.paths.Config = cfgDir
	}
	if dataDir != "" {
		a.paths.Data = dataDir
	}
	if a.log, a.logClose, err = newLogger(level, logFile, stderr); err != nil {
		fmt.Fprintln(stderr, "burrow:", err)
		return 1
	}
	defer func() {
		if a.logClose != nil {
			_ = a.logClose.Close()
		}
	}()
	if a.cfg, err = store.LoadConfig(a.paths.Config); err != nil {
		fmt.Fprintln(stderr, "burrow: config:", err)
		return 1
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	cmd, rest := fs.Arg(0), fs.Args()[1:]
	var code int
	switch cmd {
	case "init":
		code = a.cmdInit(rest, stdin, stdout, stderr)
	case "id":
		code = a.cmdID(rest, stdin, stdout, stderr)
	case "invite":
		code = a.cmdInvite(ctx, rest, stdin, stdout, stderr)
	case "listen", "connect":
		code = a.cmdRun(ctx, cmd, rest, stdin, stdout, stderr)
	case "contacts":
		code = a.cmdContacts(rest, stdin, stdout, stderr)
	case "config":
		code = a.cmdConfig(rest, stdout, stderr)
	case "passphrase":
		code = a.cmdPassphrase(rest, stdin, stdout, stderr)
	case "burn":
		code = a.cmdBurn(stdin, stdout, stderr)
	case "help", "-h", "--help":
		fmt.Fprint(stdout, usage)
	default:
		fmt.Fprintf(stderr, "burrow: unknown command %q\n", cmd)
		fs.Usage()
		code = 2
	}
	return code
}

func newLogger(level, file string, stderr io.Writer) (*slog.Logger, io.Closer, error) {
	var lv slog.Level
	if err := lv.UnmarshalText([]byte(level)); err != nil {
		return nil, nil, fmt.Errorf("bad --log-level %q", level)
	}
	w := stderr
	var closer io.Closer
	if file != "" {
		f, err := os.OpenFile(filepath.Clean(file), os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600)
		if err != nil {
			return nil, nil, err
		}
		w, closer = f, f
	}
	return slog.New(slog.NewTextHandler(w, &slog.HandlerOptions{Level: lv})), closer, nil
}

// exitErr prints a user-facing error and returns 1.
func exitErr(stderr io.Writer, err error) int {
	if errors.Is(err, store.ErrNoStore) {
		fmt.Fprintln(stderr, "burrow: no identity yet; run `burrow init` first")
		return 1
	}
	fmt.Fprintln(stderr, "burrow:", err)
	return 1
}
