package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"time"

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
	var target core.Target
	if cmd == "connect" {
		switch {
		case fs.NArg() == 0:
			s, err := readSecret("Invite (or contact name): ", stdin, stderr, false)
			if err != nil {
				return exitErr(stderr, err)
			}
			target = targetOf(string(s))
		case fs.Arg(0) == "-":
			s, err := readSecret("", stdin, stderr, true)
			if err != nil {
				return exitErr(stderr, err)
			}
			target = targetOf(string(s))
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
	if d, err := time.ParseDuration(os.Getenv("BURROW_DEBUG_CHUNK_DELAY")); err == nil && d > 0 {
		ecfg.ChunkDelay = d // debugging aid for transfer tests; not a config key
	}
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
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	errc := make(chan error, 1)
	go func() { errc <- e.Start(ctx) }()
	// Wait for the listeners.
	for len(e.ListenAddrs()) == 0 {
		select {
		case err := <-errc:
			if err == nil {
				err = errors.New("engine stopped")
			}
			return exitErr(stderr, err)
		default:
		}
	}
	inviteHost, _ := a.inviteHost(*host)
	if a.cfg.Transport == "tor" {
		inviteHost = e.OnionAddress()
	}
	ctl := common.NewController(e, inviteHost)
	ctl.Onion = e.OnionAddress()
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
		torOpts = &core.TorOptions{Key: key, Port: a.cfg.ListenPort, Exe: a.cfg.TorExe}
	}
	if a.cfg.Transport == "tor" {
		listenAddr = ""
	}
	return core.Transports(listenAddr, torOpts), nil
}
