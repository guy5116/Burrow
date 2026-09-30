//go:build gui && linux

package main

import (
	"bufio"
	"context"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// §11 budget for the desktop app: idle RSS with one peer < 200 MiB.
// It opens a real window, so it only runs when asked:
//
//	BURROW_GUI_FOOTPRINT=1 go test -tags gui -run TestGUIFootprint ./cmd/burrow-gui/
func TestGUIFootprint(t *testing.T) {
	if os.Getenv("BURROW_GUI_FOOTPRINT") != "1" {
		t.Skip("opens a window; set BURROW_GUI_FOOTPRINT=1 to run")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	bin := t.TempDir()
	for out, pkg := range map[string]string{"burrow": "../burrow", "burrow-gui": "."} {
		if b, err := exec.CommandContext(ctx, "go", "build", "-tags", "gui", "-o", filepath.Join(bin, out), pkg).CombinedOutput(); err != nil {
			t.Fatalf("build %s: %v\n%s", out, err, b)
		}
	}
	cli := func(home string, args ...string) string {
		t.Helper()
		full := append([]string{"--config", home, "--data", home}, args...)
		out, err := exec.CommandContext(ctx, filepath.Join(bin, "burrow"), full...).Output()
		if err != nil {
			t.Fatalf("burrow %v: %v", args, err)
		}
		return strings.TrimSpace(string(out))
	}
	ln, err := (&net.ListenConfig{}).Listen(ctx, "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := strconv.Itoa(ln.Addr().(*net.TCPAddr).Port)
	_ = ln.Close()

	alice, bob := t.TempDir(), t.TempDir()
	cli(alice, "init", "--insecure-no-passphrase")
	cli(alice, "config", "set", "listen_port", port)
	inv := cli(alice, "invite", "--host", "127.0.0.1")
	cli(bob, "init", "--insecure-no-passphrase")

	gui := exec.CommandContext(ctx, filepath.Join(bin, "burrow-gui"), "--config", alice, "--data", alice)
	gui.Stderr = os.Stderr
	if err := gui.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = gui.Process.Kill(); _ = gui.Wait() }()
	deadline := time.Now().Add(20 * time.Second)
	for {
		c, err := (&net.Dialer{Timeout: time.Second}).DialContext(ctx, "tcp", "127.0.0.1:"+port)
		if err == nil {
			_ = c.Close()
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the desktop app never started listening")
		}
		time.Sleep(100 * time.Millisecond)
	}

	peer := exec.CommandContext(ctx, filepath.Join(bin, "burrow"), "--plain", "--json", "--config", bob, "--data", bob,
		"connect", "--listen", "127.0.0.1:0", "-")
	stdin, _ := peer.StdinPipe()
	stdout, _ := peer.StdoutPipe()
	if err := peer.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = peer.Process.Kill(); _ = peer.Wait() }()
	if _, err := stdin.Write([]byte(inv + "\n")); err != nil {
		t.Fatal(err)
	}
	connected := make(chan bool, 1)
	go func() {
		sc := bufio.NewScanner(stdout)
		for sc.Scan() {
			if strings.Contains(sc.Text(), `"PeerConnected"`) {
				connected <- true
				return
			}
		}
		connected <- false
	}()
	select {
	case ok := <-connected:
		if !ok {
			t.Fatal("peer exited before connecting")
		}
	case <-time.After(20 * time.Second):
		t.Fatal("peer never connected to the desktop app")
	}

	time.Sleep(5 * time.Second) // settle
	status, err := os.ReadFile("/proc/" + strconv.Itoa(gui.Process.Pid) + "/status")
	if err != nil {
		t.Fatal(err)
	}
	rss := 0
	for _, line := range strings.Split(string(status), "\n") {
		if strings.HasPrefix(line, "VmRSS:") {
			rss, _ = strconv.Atoi(strings.Fields(line)[1])
		}
		if strings.HasPrefix(line, "Rss") { // anonymous, file-backed (libraries, GPU driver) and shared parts
			t.Log(strings.Join(strings.Fields(line), " "))
		}
	}
	t.Logf("desktop app idle with one peer: RSS %.1f MiB", float64(rss)/1024)
	if rss == 0 || rss > 200*1024 {
		t.Errorf("idle RSS %.1f MiB exceeds the 200 MiB budget", float64(rss)/1024)
	}
}
