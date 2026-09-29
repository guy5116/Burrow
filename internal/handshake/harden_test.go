package handshake

import (
	"context"
	"errors"
	"net"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/guy5116/burrow/internal/wire"
)

func TestErrorText(t *testing.T) {
	e := &Error{Stage: "msg2", Err: ErrLength}
	if !strings.Contains(e.Error(), "msg2") || !errors.Is(e, ErrLength) {
		t.Fatal(e)
	}
	var ne net.Error = timeoutErr{}
	if got := fail("msg1", ne); !errors.Is(got, ErrTimeout) {
		t.Fatal(got)
	}
	if HSLen := wire.HS1Len; HSLen != 1232 {
		t.Fatal(HSLen)
	}
}

type timeoutErr struct{}

func (timeoutErr) Error() string   { return "t" }
func (timeoutErr) Timeout() bool   { return true }
func (timeoutErr) Temporary() bool { return true }

func TestInitiateOnBrokenConn(t *testing.T) {
	i, r := newID(t), newID(t)
	c1, c2 := net.Pipe()
	_ = c2.Close()
	if _, err := Initiate(context.Background(), c1, i, r.Public(), nil); err == nil {
		t.Fatal("handshake over a closed pipe succeeded")
	}
	// The responder closes right after msg1: the initiator fails at msg2.
	c3, c4 := net.Pipe()
	go func() {
		buf := make([]byte, wire.HSLenPrefix+wire.HS1Len)
		_, _ = readFull(c4, buf)
		_ = c4.Close()
	}()
	_, err := Initiate(context.Background(), c3, i, r.Public(), nil)
	var he *Error
	if !errors.As(err, &he) || he.Stage != "msg2" {
		t.Fatal(err)
	}
	if err := writeHS(c3, make([]byte, 10), 100); !errors.Is(err, ErrLength) {
		t.Fatal(err)
	}
}

func readFull(c net.Conn, b []byte) (int, error) {
	n := 0
	for n < len(b) {
		m, err := c.Read(b[n:])
		n += m
		if err != nil {
			return n, err
		}
	}
	return n, nil
}

func TestLimiterEviction(t *testing.T) {
	f := NewFailureLimiter(3, time.Minute, 10*time.Minute)
	base := time.Unix(10_000, 0)
	// Fill with stale entries, then one fresh failure triggers eviction of the stale ones.
	for i := 0; i < maxLimiterKeys; i++ {
		f.Fail("k"+strconv.Itoa(i), base)
	}
	f.Fail("fresh", base.Add(time.Hour))
	if len(f.entries) > maxLimiterKeys/2+1 {
		t.Fatalf("stale entries not evicted: %d", len(f.entries))
	}
	// All entries fresh and banned: eviction falls back to dropping arbitrary keys but stays bounded.
	g := NewFailureLimiter(1, time.Minute, time.Hour)
	for i := 0; i <= maxLimiterKeys; i++ {
		g.Fail("b"+strconv.Itoa(i), base)
	}
	if len(g.entries) > maxLimiterKeys {
		t.Fatal(len(g.entries))
	}
}
