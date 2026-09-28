package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"

	qrcode "github.com/skip2/go-qrcode"

	"github.com/guy5116/burrow/internal/core"
	"github.com/guy5116/burrow/internal/secret"
	"github.com/guy5116/burrow/internal/store"
	"github.com/guy5116/burrow/internal/ui/common"
)

const insecureWarning = "WARNING: creating a store WITHOUT a passphrase. Anyone who can read your data directory can read your identity, contacts and invites. Use `burrow passphrase` later to add one."

func (a *app) cmdInit(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("init", flag.ContinueOnError)
	fs.SetOutput(stderr)
	insecure := fs.Bool("insecure-no-passphrase", false, "store the master key unencrypted on disk")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	var pw []byte
	if *insecure {
		fmt.Fprintln(stderr, insecureWarning)
	} else {
		var err error
		if pw, err = readSecret("Choose a passphrase: ", stdin, stderr, false); err != nil {
			return exitErr(stderr, err)
		}
		if len(pw) == 0 {
			return exitErr(stderr, errors.New("passphrase must not be empty"))
		}
		again, err := readSecret("Repeat the passphrase: ", stdin, stderr, false)
		if err != nil {
			secret.Wipe(pw)
			return exitErr(stderr, err)
		}
		same := string(pw) == string(again)
		secret.Wipe(again)
		if !same {
			secret.Wipe(pw)
			return exitErr(stderr, errors.New("passphrases do not match"))
		}
	}
	st, err := store.Init(a.paths.Data, pw, store.Options{})
	if err != nil {
		return exitErr(stderr, err)
	}
	defer st.Close()
	id, err := st.LoadIdentity()
	if err != nil {
		return exitErr(stderr, err)
	}
	defer id.Clear()
	if err := store.SaveConfig(a.paths.Config, a.cfg); err != nil {
		return exitErr(stderr, err)
	}
	fmt.Fprintf(stdout, "identity created\nfingerprint: %s\nstore: %s\nconfig: %s\n", id.Public().Display(),
		filepath.Join(a.paths.Data, store.StoreDir), filepath.Join(a.paths.Config, store.ConfigFile))
	return 0
}

func (a *app) cmdID(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("id", flag.ContinueOnError)
	fs.SetOutput(stderr)
	qr := fs.Bool("qr", false, "also print a QR code")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	st, err := a.unlock(stdin, stderr)
	if err != nil {
		return exitErr(stderr, err)
	}
	defer st.Close()
	id, err := st.LoadIdentity()
	if err != nil {
		return exitErr(stderr, err)
	}
	defer id.Clear()
	fmt.Fprintln(stdout, id.Public().Display())
	if *qr {
		printQR(stdout, id.Public().Fingerprint())
	}
	return 0
}

func printQR(w io.Writer, content string) {
	q, err := qrcode.New(content, qrcode.Medium)
	if err != nil {
		fmt.Fprintln(w, "(QR code unavailable: content too long)")
		return
	}
	fmt.Fprint(w, q.ToSmallString(false))
}

// inviteHost picks the address to embed: --host, then config invite_host,
// then the first non-loopback IPv4 of a local interface (LAN use).
func (a *app) inviteHost(flagHost string) (string, error) {
	if flagHost != "" {
		return flagHost, nil
	}
	if a.cfg.InviteHost != "" {
		return a.cfg.InviteHost, nil
	}
	ifaces, err := net.Interfaces()
	if err != nil {
		return "", err
	}
	for _, ifc := range ifaces {
		if ifc.Flags&net.FlagUp == 0 || ifc.Flags&net.FlagLoopback != 0 {
			continue
		}
		addrs, _ := ifc.Addrs()
		for _, ad := range addrs {
			if ipn, ok := ad.(*net.IPNet); ok && ipn.IP.To4() != nil && !ipn.IP.IsLoopback() {
				return ipn.IP.String(), nil
			}
		}
	}
	return "", errors.New("cannot guess your address; pass --host <ip-or-hostname> or set `burrow config set invite_host <addr>`")
}

func (a *app) cmdInvite(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("invite", flag.ContinueOnError)
	fs.SetOutput(stderr)
	ttl := fs.Duration("ttl", time.Hour, "validity")
	multi := fs.Bool("multi-use", false, "allow up to 64 peers")
	qr := fs.Bool("qr", false, "also print a QR code")
	host := fs.String("host", "", "address peers should dial (IP literal, hostname, .onion)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	h, err := a.inviteHost(*host)
	if err != nil {
		return exitErr(stderr, err)
	}
	st, err := a.unlock(stdin, stderr)
	if err != nil {
		return exitErr(stderr, err)
	}
	defer st.Close()
	e, err := core.New(a.engineConfig(), st, nil, a.log)
	if err != nil {
		return exitErr(stderr, err)
	}
	inv, err := e.CreateInvite(core.InviteOptions{Host: h, TTL: *ttl, MultiUse: *multi})
	if err != nil {
		return exitErr(stderr, err)
	}
	if net.ParseIP(h) == nil && !strings.HasSuffix(h, ".onion") {
		fmt.Fprintln(stderr, "note: a hostname makes your peer's DNS resolver learn it; an IP literal is more private")
	}
	fmt.Fprintf(stderr, "invite id %s, expires %s; share it over a channel you trust and treat it like a password\n", inv.ID, inv.Expiry.Local().Format(time.RFC822))
	fmt.Fprintln(stdout, inv.String)
	if *qr {
		printQR(stdout, inv.String)
	}
	_ = ctx
	return 0
}

func (a *app) engineConfig() core.Config {
	return core.Config{ListenPort: a.cfg.ListenPort, DisplayName: a.cfg.DisplayName, Typing: a.cfg.Typing,
		NoTimestamp: !a.cfg.Timestamps, AutoReconnect: a.cfg.AutoReconnect,
		DataDir: a.paths.Data, ImageDir: a.cfg.ImageDir, MaxImage: uint64(a.cfg.MaxImageMiB) << 20,
		Paranoid: a.cfg.ParanoidImages, AutoAcceptFromVerified: a.cfg.AutoAcceptFromVerified}
}

func (a *app) cmdContacts(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		args = []string{"list"}
	}
	st, err := a.unlock(stdin, stderr)
	if err != nil {
		return exitErr(stderr, err)
	}
	defer st.Close()
	e, err := core.New(a.engineConfig(), st, nil, a.log)
	if err != nil {
		return exitErr(stderr, err)
	}
	c := common.NewController(e, "")
	var line string
	switch args[0] {
	case "list":
		line = "/contacts"
	case "verify", "remove", "block", "unblock":
		if len(args) != 2 {
			return exitErr(stderr, fmt.Errorf("usage: burrow contacts %s <contact>", args[0]))
		}
		line = "/" + args[0] + " " + args[1]
	case "rename":
		if len(args) != 3 {
			return exitErr(stderr, errors.New("usage: burrow contacts rename <contact> <name>"))
		}
		line = "/rename " + args[1] + " " + args[2]
	case "safety":
		if len(args) != 2 {
			return exitErr(stderr, errors.New("usage: burrow contacts safety <contact>"))
		}
		line = "/safety " + args[1]
	default:
		return exitErr(stderr, fmt.Errorf("unknown contacts subcommand %q", args[0]))
	}
	out, _ := c.Exec(context.Background(), line)
	code := 0
	for _, l := range out {
		if strings.HasPrefix(l, "! ") {
			code = 1
		}
		fmt.Fprintln(stdout, l)
	}
	return code
}

func (a *app) cmdConfig(args []string, stdout, stderr io.Writer) int {
	switch {
	case len(args) == 0 || (args[0] == "get" && len(args) == 1):
		for _, k := range store.ConfigKeys() {
			v, _ := a.cfg.Get(k)
			fmt.Fprintf(stdout, "%s = %s\n", k, v)
		}
		return 0
	case args[0] == "get" && len(args) == 2:
		v, err := a.cfg.Get(args[1])
		if err != nil {
			return exitErr(stderr, err)
		}
		fmt.Fprintln(stdout, v)
		return 0
	case args[0] == "set" && len(args) == 3:
		if err := a.cfg.Set(args[1], args[2]); err != nil {
			return exitErr(stderr, err)
		}
		if err := store.SaveConfig(a.paths.Config, a.cfg); err != nil {
			return exitErr(stderr, err)
		}
		return 0
	}
	return exitErr(stderr, errors.New("usage: burrow config get [key] | set <key> <value>"))
}

func (a *app) cmdPassphrase(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("passphrase", flag.ContinueOnError)
	fs.SetOutput(stderr)
	insecure := fs.Bool("insecure-no-passphrase", false, "switch to an unencrypted master key")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	st, err := a.unlock(stdin, stderr)
	if err != nil {
		return exitErr(stderr, err)
	}
	defer st.Close()
	var pw []byte
	if *insecure {
		fmt.Fprintln(stderr, insecureWarning)
	} else {
		if pw, err = readSecret("New passphrase: ", stdin, stderr, false); err != nil {
			return exitErr(stderr, err)
		}
		again, err := readSecret("Repeat the new passphrase: ", stdin, stderr, false)
		if err != nil {
			secret.Wipe(pw)
			return exitErr(stderr, err)
		}
		same := string(pw) == string(again)
		secret.Wipe(again)
		if !same || len(pw) == 0 {
			secret.Wipe(pw)
			return exitErr(stderr, errors.New("passphrases do not match or are empty"))
		}
	}
	if err := st.ChangePassphrase(pw, store.Options{}); err != nil {
		if errors.Is(err, store.ErrOldStoreRemains) {
			fmt.Fprintln(stderr, "burrow:", err)
			return 0
		}
		return exitErr(stderr, err)
	}
	fmt.Fprintln(stdout, "store re-encrypted")
	return 0
}

func (a *app) cmdBurn(stdin io.Reader, stdout, stderr io.Writer) int {
	st, err := a.unlock(stdin, stderr)
	if err != nil {
		return exitErr(stderr, err)
	}
	defer st.Close()
	n := 0
	for _, sub := range []string{"partials", "history"} {
		blobs, err := st.ListBlobs(sub)
		if err != nil {
			return exitErr(stderr, err)
		}
		for _, b := range blobs {
			if err := st.DeleteBlob(b); err != nil {
				return exitErr(stderr, err)
			}
			n++
		}
	}
	parts, _ := filepath.Glob(filepath.Join(a.paths.Data, "partials", "*.part"))
	for _, p := range parts {
		if err := os.Remove(p); err == nil {
			n++
		}
	}
	fmt.Fprintf(stdout, "burned %d files\n", n)
	return 0
}
