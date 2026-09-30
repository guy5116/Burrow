package store

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestConfig(t *testing.T) {
	dir := t.TempDir()
	c, err := LoadConfig(dir)
	if err != nil || c != DefaultConfig() {
		t.Fatal(c, err)
	}
	// Defaults audit (§17).
	if c.Typing || !c.Timestamps || c.AutoAcceptFromVerified || c.MDNS || c.ParanoidImages || c.OpenLinks || c.DisplayName != "" || c.ListenPort != 47337 || c.History || c.Transport != "tcp" || c.MaxPeers != 32 || c.MaxImageMiB != 25 || c.MaxFileMiB != 100 {
		t.Fatalf("defaults %+v", c)
	}
	for _, k := range ConfigKeys() {
		if _, err := c.Get(k); err != nil {
			t.Fatal(k, err)
		}
	}
	if _, err := c.Get("nope"); !errors.Is(err, ErrConfigKey) {
		t.Fatal(err)
	}
	if err := c.Set("nope", "1"); !errors.Is(err, ErrConfigKey) {
		t.Fatal(err)
	}
	for _, kv := range [][2]string{{"listen_port", "0"}, {"listen_port", "x"}, {"typing", "maybe"}, {"max_image_mib", "0"}, {"max_file_mib", "-1"}, {"max_file_mib", "1048577"}, {"max_file_mib", "big"}, {"display_name", "\xff\xfe"}, {"image_dir", "/tmp/\xff"}, {"display_name", string(make([]byte, 33))}, {"transport", "udp"}, {"history", "sometimes"}, {"max_peers", "0"}, {"max_peers", "33"}} {
		if err := c.Set(kv[0], kv[1]); err == nil {
			t.Fatalf("Set(%s,%q) accepted", kv[0], kv[1])
		}
	}
	for _, kv := range [][2]string{{"listen_port", "1234"}, {"display_name", "Zoë"}, {"typing", "true"}, {"image_dir", "/tmp/x"}, {"max_image_mib", "5"}, {"max_file_mib", "0"}, {"max_file_mib", "2048"}, {"invite_host", "203.0.113.5"}, {"max_peers", "8"}} {
		if err := c.Set(kv[0], kv[1]); err != nil {
			t.Fatal(kv, err)
		}
		if v, _ := c.Get(kv[0]); v != kv[1] {
			t.Fatalf("%s = %q", kv[0], v)
		}
	}
	if err := SaveConfig(dir, c); err != nil {
		t.Fatal(err)
	}
	c2, err := LoadConfig(dir)
	if err != nil || c2 != c {
		t.Fatalf("%+v %v", c2, err)
	}
	// Corrupt file → error, defaults untouched.
	if err := writeFileAtomic(dir+"/"+ConfigFile, []byte("listen_port = \"x")); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadConfig(dir); err == nil {
		t.Fatal("corrupt config accepted")
	}
}

// The limits hold for a file edited by hand as well.
func TestLoadConfigValidates(t *testing.T) {
	for _, line := range []string{"max_peers = 100000", "max_peers = -1", "listen_port = 0", "max_image_mib = 0",
		"max_file_mib = 2000000", `transport = "udp"`, `display_name = "` + strings.Repeat("x", 33) + `"`} {
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, ConfigFile), []byte(line+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		if c, err := LoadConfig(dir); err == nil || c != DefaultConfig() {
			t.Errorf("%s: accepted (%v)", line, err)
		}
	}
	dir := t.TempDir()
	_ = os.WriteFile(filepath.Join(dir, ConfigFile), []byte("max_peers = 8\nmax_file_mib = 0\n"), 0o600)
	if c, err := LoadConfig(dir); err != nil || c.MaxPeers != 8 || c.MaxFileMiB != 0 || c.ListenPort != 47337 {
		t.Fatal(c, err)
	}
}
