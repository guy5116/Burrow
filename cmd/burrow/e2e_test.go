package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"image"
	"image/png"
	"io"
	"math/rand/v2"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// proc is one `burrow --plain --json` child process.
type proc struct {
	t      *testing.T
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	mu     sync.Mutex
	events []map[string]any
	cond   *sync.Cond
	home   string
}

var binPath string

func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "burrow-e2e")
	if err != nil {
		panic(err)
	}
	binPath = filepath.Join(dir, "burrow")
	if out, err := exec.CommandContext(context.Background(), "go", "build", "-o", binPath, ".").CombinedOutput(); err != nil {
		panic(string(out))
	}
	code := m.Run()
	_ = os.RemoveAll(dir)
	os.Exit(code)
}

func newProc(t *testing.T, home string, args ...string) *proc {
	t.Helper()
	if home == "" {
		home = t.TempDir()
		out, err := exec.CommandContext(context.Background(), binPath, "--config", home, "--data", home, "init", "--insecure-no-passphrase").CombinedOutput()
		if err != nil {
			t.Fatalf("init: %v\n%s", err, out)
		}
	}
	full := append([]string{"--plain", "--json", "--config", home, "--data", home}, args...)
	cmd := exec.CommandContext(context.Background(), binPath, full...)
	cmd.Stderr = os.Stderr
	cmd.Env = append(os.Environ(), "BURROW_DEBUG_CHUNK_DELAY=8ms")
	stdin, _ := cmd.StdinPipe()
	stdout, _ := cmd.StdoutPipe()
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	p := &proc{t: t, cmd: cmd, stdin: stdin, home: home}
	p.cond = sync.NewCond(&p.mu)
	go func() {
		sc := bufio.NewScanner(stdout)
		for sc.Scan() {
			var m map[string]any
			if json.Unmarshal(sc.Bytes(), &m) != nil {
				continue
			}
			p.mu.Lock()
			p.events = append(p.events, m)
			p.cond.Broadcast()
			p.mu.Unlock()
		}
	}()
	t.Cleanup(func() { p.kill() })
	return p
}

func (p *proc) kill() {
	if p.cmd.Process != nil {
		_ = p.cmd.Process.Kill()
		_, _ = p.cmd.Process.Wait()
	}
}

func (p *proc) send(line string) {
	if _, err := io.WriteString(p.stdin, line+"\n"); err != nil {
		p.t.Fatal(err)
	}
}

// wait blocks until an event matching pred appears (searching from the start).
func (p *proc) wait(what string, pred func(map[string]any) bool) map[string]any {
	p.t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	p.mu.Lock()
	defer p.mu.Unlock()
	for {
		for _, ev := range p.events {
			if pred(ev) {
				return ev
			}
		}
		if time.Now().After(deadline) {
			var names []string
			for _, ev := range p.events {
				names = append(names, ev["event"].(string))
			}
			p.t.Fatalf("timeout waiting for %s; events: %s", what, strings.Join(names, " "))
		}
		timer := time.AfterFunc(100*time.Millisecond, p.cond.Broadcast)
		p.cond.Wait()
		timer.Stop()
	}
}

func is(name string) func(map[string]any) bool {
	return func(m map[string]any) bool { return m["event"] == name }
}

func TestEndToEndText(t *testing.T) {
	if testing.Short() {
		t.Skip("spawns processes")
	}
	a := newProc(t, "", "listen", "--listen", "127.0.0.1:0", "--host", "127.0.0.1")
	ready := a.wait("Ready", is("Ready"))
	addr := ready["listen"].([]any)[0].(string)
	port := addr[strings.LastIndex(addr, ":")+1:]
	a.send("/invite 127.0.0.1")
	inv := a.wait("invite", func(m map[string]any) bool {
		s, _ := m["text"].(string)
		return m["event"] == "Output" && strings.HasPrefix(s, "burrow1:")
	})["text"].(string)
	if !strings.Contains(inv, "burrow1:") {
		t.Fatal(inv)
	}
	_ = port

	b := newProc(t, "", "listen", "--listen", "127.0.0.1:0")
	b.wait("Ready", is("Ready"))
	b.send("/connect " + inv)
	b.wait("PeerConnected", is("PeerConnected"))
	a.wait("PeerConnected", is("PeerConnected"))
	a.wait("NewPeerViaInvite", is("NewPeerViaInvite"))
	aFP := a.wait("Ready", is("Ready"))["fingerprint"].(string)
	bFP := b.wait("Ready", is("Ready"))["fingerprint"].(string)

	// Text both ways with delivery status.
	b.send("hello from b")
	got := a.wait("MessageReceived", is("MessageReceived"))
	if got["text"] != "hello from b" || got["peer"] != bFP {
		t.Fatalf("%v", got)
	}
	b.wait("delivered", func(m map[string]any) bool { return m["event"] == "MessageStatus" && m["status"] == "delivered" })
	a.send("/msg " + bFP[:12] + " hi b")
	if m := b.wait("MessageReceived", is("MessageReceived")); m["text"] != "hi b" || m["peer"] != aFP {
		t.Fatalf("%v", m)
	}

	// Kill A abruptly; B queues; A restarts on the same port; B reconnects and delivers in order.
	a.kill()
	b.wait("PeerDisconnected", is("PeerDisconnected"))
	b.send("queued one")
	b.send("queued two")
	b.wait("pending", func(m map[string]any) bool { return m["event"] == "MessageStatus" && m["status"] == "pending" })
	a2 := newProc(t, a.home, "listen", "--listen", "127.0.0.1:"+port, "--host", "127.0.0.1")
	a2.wait("Ready", is("Ready"))
	b.wait("Reconnecting", is("Reconnecting"))
	a2.wait("PeerConnected", is("PeerConnected"))
	m1 := a2.wait("queued one", func(m map[string]any) bool { return m["event"] == "MessageReceived" && m["text"] == "queued one" })
	m2 := a2.wait("queued two", func(m map[string]any) bool { return m["event"] == "MessageReceived" && m["text"] == "queued two" })
	a2.mu.Lock()
	i1, i2 := -1, -1
	for i, ev := range a2.events {
		if ev["id"] == m1["id"] {
			i1 = i
		}
		if ev["id"] == m2["id"] {
			i2 = i
		}
	}
	a2.mu.Unlock()
	if i1 < 0 || i2 < i1 {
		t.Fatal("order not preserved")
	}
	b.wait("delivered after reconnect", func(m map[string]any) bool {
		return m["event"] == "MessageStatus" && m["status"] == "delivered" && m["id"] == m2["id"]
	})
	// Contacts persisted across A's restart; safety numbers agree.
	a2.send("/contacts")
	a2.wait("contact listed", func(m map[string]any) bool {
		s, _ := m["text"].(string)
		return m["event"] == "Output" && strings.Contains(s, bFP[:8])
	})
	a2.send("/safety " + bFP[:12])
	b.send("/safety " + aFP[:12])
	sa := a2.wait("safety", func(m map[string]any) bool { s, _ := m["text"].(string); return len(s) == 23 && s[5] == ' ' })
	sb := b.wait("safety", func(m map[string]any) bool { s, _ := m["text"].(string); return len(s) == 23 && s[5] == ' ' })
	if sa["text"] != sb["text"] {
		t.Fatalf("safety numbers differ: %v %v", sa["text"], sb["text"])
	}
	b.send("/quit")
	a2.wait("PeerDisconnected", func(m map[string]any) bool { return m["event"] == "PeerDisconnected" && m["reason"] == "peer left" })
	a2.send("/quit")
	_ = a2.cmd.Wait()
}

// makePNG writes a noisy PNG of about the requested size to dir.
func makePNG(t *testing.T, dir string, side int) string {
	t.Helper()
	img := image.NewNRGBA(image.Rect(0, 0, side, side))
	r := rand.New(rand.NewPCG(7, 9))
	for i := range img.Pix {
		img.Pix[i] = byte(r.UintN(256))
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(dir, "shot.png")
	if err := os.WriteFile(p, buf.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestEndToEndImage(t *testing.T) {
	if testing.Short() {
		t.Skip("spawns processes")
	}
	a := newProc(t, "", "listen", "--listen", "127.0.0.1:0", "--host", "127.0.0.1")
	ready := a.wait("Ready", is("Ready"))
	addr := ready["listen"].([]any)[0].(string)
	port := addr[strings.LastIndex(addr, ":")+1:]
	a.send("/invite 127.0.0.1")
	inv := a.wait("invite", func(m map[string]any) bool {
		s, _ := m["text"].(string)
		return m["event"] == "Output" && strings.HasPrefix(s, "burrow1:")
	})["text"].(string)
	b := newProc(t, "", "listen", "--listen", "127.0.0.1:0")
	b.wait("Ready", is("Ready"))
	b.send("/connect " + inv)
	b.wait("PeerConnected", is("PeerConnected"))
	a.wait("PeerConnected", is("PeerConnected"))
	bFP := b.wait("Ready", is("Ready"))["fingerprint"].(string)

	// Small image: offer, accept, saved and decodable on B.
	small := makePNG(t, t.TempDir(), 64)
	a.send("/to " + bFP[:12])
	a.send("/image " + small + " a caption")
	off := b.wait("ImageOffered", is("ImageOffered"))
	if off["caption"] != "a caption" || off["format"] != "PNG" {
		t.Fatalf("%v", off)
	}
	b.send("/accept 1")
	done := b.wait("TransferDone", is("TransferDone"))
	path := done["path"].(string)
	if !strings.HasSuffix(path, ".png") || !strings.HasPrefix(path, b.home) {
		t.Fatal(path)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatal(err)
	}
	a.wait("sender done", func(m map[string]any) bool { return m["event"] == "TransferDone" && m["outgoing"] == true })

	// Large image: kill A mid-transfer, restart it, B reconnects, A re-offers, B resumes.
	big := makePNG(t, t.TempDir(), 2400) // ~23 MiB of incompressible pixels
	a.send("/image " + big)
	b.wait("big offer", func(m map[string]any) bool { return m["event"] == "ImageOffered" && m["number"] == float64(2) })
	b.send("/accept 2")
	b.wait("progress", func(m map[string]any) bool {
		return m["event"] == "TransferProgress" && m["done"].(float64) >= 2<<20
	})
	a.kill()
	b.wait("failed", func(m map[string]any) bool { return m["event"] == "TransferFailed" && m["reason"] == "connection lost" })
	parts, _ := filepath.Glob(filepath.Join(b.home, "partials", "*.part"))
	if len(parts) != 1 {
		t.Fatalf("partial not kept: %v", parts)
	}
	a2 := newProc(t, a.home, "listen", "--listen", "127.0.0.1:"+port, "--host", "127.0.0.1")
	a2.wait("Ready", is("Ready"))
	b.wait("reconnected", func(m map[string]any) bool {
		return m["event"] == "PeerConnected" && b.countEvents("PeerConnected") >= 2
	})
	a2.send("/to " + bFP[:12])
	a2.send("/image " + big)
	b.wait("re-offer", func(m map[string]any) bool { return m["event"] == "ImageOffered" && m["number"] == float64(3) })
	b.send("/accept 3")
	b.wait("TransferResumed", is("TransferResumed"))
	done2 := b.wait("big done", func(m map[string]any) bool { return m["event"] == "TransferDone" && m["path"] != path })
	if fi, err := os.Stat(done2["path"].(string)); err != nil || fi.Size() < 20<<20 {
		t.Fatal(err)
	}
	if parts, _ := filepath.Glob(filepath.Join(b.home, "partials", "*")); len(parts) != 0 {
		t.Fatal("partials left over")
	}
	b.send("/quit")
	a2.send("/quit")
	_ = a2.cmd.Wait()
}

func (p *proc) countEvents(name string) int {
	n := 0
	for _, ev := range p.events { // caller holds p.mu via wait's predicate
		if ev["event"] == name {
			n++
		}
	}
	return n
}
