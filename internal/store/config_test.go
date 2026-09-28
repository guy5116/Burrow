package store

import (
	"errors"
	"testing"
)

func TestConfig(t *testing.T) {
	dir := t.TempDir()
	c, err := LoadConfig(dir)
	if err != nil || c != DefaultConfig() {
		t.Fatal(c, err)
	}
	// Defaults audit (§17).
	if c.Typing || !c.Timestamps || c.AutoAcceptFromVerified || c.MDNS || c.ParanoidImages || c.OpenLinks || c.DisplayName != "" || c.ListenPort != 47337 {
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
	for _, kv := range [][2]string{{"listen_port", "0"}, {"listen_port", "x"}, {"typing", "maybe"}, {"max_image_mib", "0"}, {"display_name", string(make([]byte, 33))}} {
		if err := c.Set(kv[0], kv[1]); err == nil {
			t.Fatalf("Set(%s,%q) accepted", kv[0], kv[1])
		}
	}
	for _, kv := range [][2]string{{"listen_port", "1234"}, {"display_name", "Zoë"}, {"typing", "true"}, {"image_dir", "/tmp/x"}, {"max_image_mib", "5"}, {"invite_host", "203.0.113.5"}} {
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
