package tcp

import (
	"net"
	"testing"

	"go.uber.org/goleak"

	"github.com/guy5116/burrow/internal/transport"
	"github.com/guy5116/burrow/internal/transport/conformance"
)

func TestMain(m *testing.M) { goleak.VerifyTestMain(m) }

func TestConformance(t *testing.T) {
	tr := New("127.0.0.1:0")
	conformance.Run(t, tr, func(a net.Addr) transport.Address {
		ta := a.(*net.TCPAddr)
		return transport.Address{Kind: transport.KindTCP, Host: ta.IP.String(), Port: uint16(ta.Port)}
	})
}

func TestRateKey(t *testing.T) {
	tr := New(":0")
	cases := map[string]string{
		"192.0.2.77:1234":      "192.0.2.0/24",
		"[2001:db8:1:2::5]:1":  "2001:db8:1::/48",
		"[::ffff:192.0.2.9]:5": "192.0.2.0/24",
		"garbage":              "garbage",
	}
	for in, want := range cases {
		var a net.Addr = stringAddr(in)
		if got := tr.RateKey(a); got != want {
			t.Errorf("RateKey(%s)=%s want %s", in, got, want)
		}
	}
	if tr.RateKey(nil) != "" {
		t.Fatal("nil")
	}
}

type stringAddr string

func (s stringAddr) Network() string { return "tcp" }
func (s stringAddr) String() string  { return string(s) }
