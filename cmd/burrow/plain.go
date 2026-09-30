package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"sync"

	"github.com/guy5116/burrow/internal/core"
	"github.com/guy5116/burrow/internal/ui/common"
)

// runPlain is line mode (§9.3): stdin commands, one event per stdout line.
func runPlain(ctx context.Context, ctl *common.Controller, target core.Target, asJSON bool, stdin io.Reader, stdout, stderr io.Writer) int {
	var wmu sync.Mutex
	enc := json.NewEncoder(stdout)
	emit := func(m map[string]any, line string) {
		wmu.Lock()
		defer wmu.Unlock()
		if asJSON {
			_ = enc.Encode(m)
		} else if line != "" {
			fmt.Fprintln(stdout, line)
		}
	}
	var addrs []string
	for _, a := range ctl.E.ListenAddrs() {
		addrs = append(addrs, a.String())
	}
	emit(map[string]any{"event": "Ready", "fingerprint": ctl.E.Identity().Fingerprint, "listen": addrs},
		"* ready on "+strings.Join(addrs, " ")+"; fingerprint "+ctl.E.Identity().Display+" (type /help)")

	for _, l := range ctl.StartupNotes(false) {
		emit(map[string]any{"event": "Output", "text": l}, l)
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	// Events on a dedicated goroutine (never the input thread).
	go func() {
		for {
			select {
			case ev := <-ctl.E.Events():
				ctl.Observe(ev)
				m := ctl.Names.JSON(ev)
				if o, ok := ev.(core.ImageOffered); ok {
					m["number"] = ctl.Number(o.ID)
				}
				emit(m, ctl.Names.Line(ev))
			case <-ctx.Done():
				return
			}
		}
	}()

	exec := func(line string) bool {
		out, quit := ctl.Exec(ctx, line)
		for _, l := range out {
			m := map[string]any{"event": "Output", "text": l}
			if strings.HasPrefix(l, "! ") {
				m["event"] = "Error"
			}
			emit(m, l)
		}
		return quit
	}
	if target.InviteBytes != nil {
		for _, l := range ctl.ConnectInvite(ctx, target.InviteBytes) {
			m := map[string]any{"event": "Output", "text": l}
			if strings.HasPrefix(l, "! ") {
				m["event"] = "Error"
			}
			emit(m, l)
		}
	} else if target.Invite != "" {
		exec("/connect " + target.Invite)
	} else if target.Name != "" {
		exec("/connect " + target.Name)
	}

	// Plain mode ends when stdin ends. A line longer than maxLine is an error,
	// not an end of input.
	lines := make(chan string)
	readErr := make(chan error, 1)
	go func() {
		sc := bufio.NewScanner(stdin)
		sc.Buffer(make([]byte, maxLine), maxLine)
		for sc.Scan() {
			select {
			case lines <- sc.Text():
			case <-ctx.Done():
				return
			}
		}
		readErr <- sc.Err()
		close(lines)
	}()
	for {
		select {
		case <-ctx.Done():
			return 0
		case line, ok := <-lines:
			if !ok { // input has ended
				if err := <-readErr; err != nil {
					fmt.Fprintln(stderr, "burrow: reading input:", err)
					return 1
				}
				return 0
			}
			if exec(line) { // /quit
				return 0
			}
		}
	}
}

// maxLine bounds one line of input: the longest message plus a command.
const maxLine = 64 << 10
