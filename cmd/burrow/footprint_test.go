//go:build linux

package main

import (
	"os"
	"strconv"
	"strings"
	"testing"
	"time"
)

// procStat reads resident memory (KiB) and consumed CPU time (clock ticks) of pid.
func procStat(t *testing.T, pid int) (rssKiB int, ticks int) {
	t.Helper()
	status, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/status")
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range strings.Split(string(status), "\n") {
		if strings.HasPrefix(line, "VmRSS:") {
			rssKiB, _ = strconv.Atoi(strings.Fields(line)[1])
		}
	}
	stat, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		t.Fatal(err)
	}
	f := strings.Fields(string(stat)[strings.LastIndex(string(stat), ")")+2:])
	u, _ := strconv.Atoi(f[11]) // utime
	s, _ := strconv.Atoi(f[12]) // stime
	return rssKiB, u + s
}

// §11 budgets for the CLI: startup to ready < 300 ms after unlock, idle RSS
// with one peer < 30 MiB, idle CPU < 0.5 %.
func TestFootprint(t *testing.T) {
	if testing.Short() {
		t.Skip("spawns processes and idles for several seconds")
	}
	a := newProc(t, "", "listen", "--listen", "127.0.0.1:0", "--host", "127.0.0.1")
	a.wait("Ready", is("Ready"))
	// newProc runs `init` first; measure a second start on the existing store.
	a.kill()
	start := time.Now()
	a = newProc(t, a.home, "listen", "--listen", "127.0.0.1:0", "--host", "127.0.0.1")
	a.wait("Ready", is("Ready"))
	startup := time.Since(start)
	t.Logf("startup to ready: %v", startup.Round(time.Millisecond))
	if startup > 300*time.Millisecond {
		t.Errorf("startup %v exceeds the 300 ms budget", startup)
	}

	a.send("/invite 127.0.0.1")
	inv := a.wait("invite", func(m map[string]any) bool {
		s, _ := m["text"].(string)
		return m["event"] == "Output" && strings.HasPrefix(s, "burrow1:")
	})["text"].(string)
	b := newProc(t, "", "listen", "--listen", "127.0.0.1:0")
	b.wait("Ready", is("Ready"))
	b.send("/connect " + inv)
	a.wait("PeerConnected", is("PeerConnected"))
	b.send("hello")
	a.wait("MessageReceived", is("MessageReceived"))

	time.Sleep(2 * time.Second) // settle (GC, post-handshake)
	_, t0 := procStat(t, a.cmd.Process.Pid)
	const idle = 6 * time.Second
	time.Sleep(idle)
	rss, t1 := procStat(t, a.cmd.Process.Pid)
	cpu := float64(t1-t0) / 100 / idle.Seconds() * 100 // USER_HZ is 100 on Linux
	t.Logf("idle with one peer: RSS %.1f MiB, CPU %.2f %%", float64(rss)/1024, cpu)
	if rss > 30*1024 {
		t.Errorf("idle RSS %.1f MiB exceeds the 30 MiB budget", float64(rss)/1024)
	}
	if cpu > 0.5 {
		t.Errorf("idle CPU %.2f %% exceeds the 0.5 %% budget", cpu)
	}
}
