package main

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"strconv"
	"strings"

	"github.com/guy5116/burrow/internal/core"
	"github.com/guy5116/burrow/internal/store"
	"github.com/guy5116/burrow/internal/ui/common"
	"github.com/guy5116/burrow/internal/ui/tui"
)

// cmdRun implements `listen` and `connect`: unlock, start the engine, then
// hand over to plain mode or the TUI.
func (a *app) cmdRun(ctx context.Context, cmd string, args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet(cmd, flag.ContinueOnError)
	fs.SetOutput(stderr)
	host := fs.String("host", "", "address to embed in invites created from the chat")
	listen := fs.String("listen", "", "listen address override (default :<listen_port>)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	most := 0
	if cmd == "connect" {
		most = 1
	}
	if tooMany(fs, most, stderr) {
		return 2
	}
	var target core.Target
	if cmd == "connect" {
		switch {
		case fs.NArg() == 0:
			s, err := readSecret("Invite (or contact name): ", stdin, stderr, false)
			if err != nil {
				return exitErr(stderr, err)
			}
			target = targetOfBytes(s)
		case fs.Arg(0) == "-":
			s, err := readSecret("", stdin, stderr, true)
			if err != nil {
				return exitErr(stderr, err)
			}
			target = targetOfBytes(s)
		default:
			arg := fs.Arg(0)
			if strings.HasPrefix(arg, "burrow1:") {
				fmt.Fprintln(stderr, "warning: an invite on the command line is visible in your shell history and to other users via ps; prefer `burrow connect` (prompt) or `burrow connect -`")
			}
			target = targetOf(arg)
		}
	}
	st, err := a.unlock(stdin, stderr)
	if err != nil {
		return exitErr(stderr, err)
	}
	defer st.Close()
	ecfg := a.engineConfig()
	if *listen != "" {
		ecfg.ListenAddr = *listen
	}
	ecfg.ChunkDelay = testChunkDelay()
	addr := ":" + strconv.Itoa(int(a.cfg.ListenPort))
	if *listen != "" {
		addr = *listen
	}
	trs, err := a.transports(st, addr)
	if err != nil {
		return exitErr(stderr, err)
	}
	e, err := core.New(ecfg, st, trs, a.log)
	if err != nil {
		return exitErr(stderr, err)
	}
	defer e.Close() // Start closes the engine too; this covers a failed Start
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	errc := make(chan error, 1)
	go func() { errc <- e.Start(ctx) }()
	select {
	case <-e.Ready(): // the listeners are up (with Tor this can take minutes)
	case err := <-errc:
		if err == nil {
			err = errors.New("stopped before it was ready")
		}
		return exitErr(stderr, err)
	}
	inviteHost, _ := a.inviteHost(*host)
	if a.cfg.Transport == "tor" {
		inviteHost = e.OnionAddress()
	}
	ctl := common.NewController(e, inviteHost)
	ctl.Onion = e.OnionAddress()
	ctl.QR = func(text string) string {
		var sb strings.Builder
		printQR(&sb, text)
		return sb.String()
	}
	var code int
	if a.plain {
		code = runPlain(ctx, ctl, target, a.json, stdin, stdout, stderr)
	} else {
		code = tui.Run(ctx, ctl, target, e.ListenAddrs())
	}
	cancel()
	<-errc
	return code
}

// tooMany reports, with a usage message, arguments beyond the most a command
// takes. The flag package stops at the first argument that is no flag, so
// `connect Alice --listen :5000` would otherwise run with the flag ignored.
func tooMany(fs *flag.FlagSet, most int, stderr io.Writer) bool {
	if fs.NArg() <= most {
		return false
	}
	fmt.Fprintf(stderr, "burrow %s: unexpected argument %q (flags go before other arguments)\n", fs.Name(), fs.Arg(most))
	return true
}

// targetOfBytes is targetOf for input read from a prompt or a pipe: an invite
// stays in bytes (wiped by the engine); a contact name becomes a string.
func targetOfBytes(b []byte) core.Target {
	t := bytes.TrimSpace(b)
	if bytes.HasPrefix(t, []byte("burrow1:")) {
		return core.Target{InviteBytes: b}
	}
	name := string(t)
	clear(b)
	return core.Target{Name: name}
}

// targetOf turns an argument into a Target; contact names are resolved later
// by the controller (it needs the engine).
func targetOf(s string) core.Target {
	s = strings.TrimSpace(s)
	if strings.HasPrefix(s, "burrow1:") {
		return core.Target{Invite: s}
	}
	return core.Target{Name: s}
}

// transports builds the transport set from config.toml: tcp (default), tor, or both.
func (a *app) transports(st *store.Store, listenAddr string) ([]core.Transport, error) {
	var torOpts *core.TorOptions
	if a.cfg.Transport == "tor" || a.cfg.Transport == "both" {
		key, err := st.LoadOnionKey()
		if err != nil {
			return nil, err
		}
		torOpts = &core.TorOptions{Key: key, Port: a.cfg.ListenPort, Exe: a.cfg.TorExe, DataDir: a.paths.Data}
	}
	if a.cfg.Transport == "tor" {
		listenAddr = ""
	}
	return core.Transports(listenAddr, torOpts), nil
}
