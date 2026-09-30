package store

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/BurntSushi/toml"
)

// Config is config.toml: nothing secret lives here. Both UIs share it and
// its defaults (CLAUDE.md §17 defaults audit).
type Config struct {
	ListenPort             uint16 `toml:"listen_port"`
	DisplayName            string `toml:"display_name"`
	Typing                 bool   `toml:"typing"`
	Timestamps             bool   `toml:"timestamps"`
	AutoReconnect          bool   `toml:"auto_reconnect"`
	AutoAcceptFromVerified bool   `toml:"auto_accept_from_verified"`
	ParanoidImages         bool   `toml:"paranoid_images"`
	MDNS                   bool   `toml:"mdns"`
	OpenLinks              bool   `toml:"open_links"`
	ImageDir               string `toml:"image_dir"`
	MaxImageMiB            uint32 `toml:"max_image_mib"`
	MaxFileMiB             uint32 `toml:"max_file_mib"` // 0 refuses files that are not images
	InviteHost             string `toml:"invite_host"`
	Transport              string `toml:"transport"` // tcp | tor | both
	TorExe                 string `toml:"tor_exe"`
	History                bool   `toml:"history"`
	MaxPeers               int    `toml:"max_peers"`
}

// MaxFileMiBLimit is the largest max_file_mib: 1 TiB, the protocol's own cap.
const MaxFileMiBLimit = 1 << 20

// DefaultConfig returns the shipped defaults.
func DefaultConfig() Config {
	return Config{ListenPort: 47337, Timestamps: true, AutoReconnect: true, MaxImageMiB: 25, MaxFileMiB: 100, Transport: "tcp", MaxPeers: 32}
}

// ConfigFile is the file name under the config directory.
const ConfigFile = "config.toml"

// LoadConfig reads config.toml, applying defaults for missing keys. A missing
// file yields the defaults.
func LoadConfig(configDir string) (Config, error) {
	c := DefaultConfig()
	b, err := os.ReadFile(filepath.Join(configDir, ConfigFile))
	if errors.Is(err, os.ErrNotExist) {
		return c, nil
	}
	if err != nil {
		return c, err
	}
	if _, err := toml.Decode(string(b), &c); err != nil {
		return DefaultConfig(), err
	}
	if err := c.validate(); err != nil {
		return DefaultConfig(), fmt.Errorf("%s: %w", ConfigFile, err)
	}
	return c, nil
}

// validate applies the limits of Set to a whole configuration, so a file
// edited by hand cannot hold what `burrow config set` would refuse.
func (c Config) validate() error {
	var probe Config
	for _, key := range ConfigKeys() {
		v, err := c.Get(key)
		if err == nil {
			err = probe.Set(key, v)
		}
		if err != nil {
			return err
		}
	}
	return nil
}

// SaveConfig writes config.toml atomically.
func SaveConfig(configDir string, c Config) error {
	if err := os.MkdirAll(configDir, dirPerm); err != nil {
		return err
	}
	var sb strings.Builder
	if err := toml.NewEncoder(&sb).Encode(c); err != nil {
		return err
	}
	return writeFileAtomic(filepath.Join(configDir, ConfigFile), []byte(sb.String()))
}

// ErrConfigKey is returned for an unknown key in Get/Set.
var ErrConfigKey = errors.New("store: unknown config key")

// Get returns a key's value as a string ("burrow config get").
func (c *Config) Get(key string) (string, error) {
	switch key {
	case "listen_port":
		return strconv.Itoa(int(c.ListenPort)), nil
	case "display_name":
		return c.DisplayName, nil
	case "typing":
		return strconv.FormatBool(c.Typing), nil
	case "timestamps":
		return strconv.FormatBool(c.Timestamps), nil
	case "auto_reconnect":
		return strconv.FormatBool(c.AutoReconnect), nil
	case "auto_accept_from_verified":
		return strconv.FormatBool(c.AutoAcceptFromVerified), nil
	case "paranoid_images":
		return strconv.FormatBool(c.ParanoidImages), nil
	case "mdns":
		return strconv.FormatBool(c.MDNS), nil
	case "open_links":
		return strconv.FormatBool(c.OpenLinks), nil
	case "image_dir":
		return c.ImageDir, nil
	case "max_image_mib":
		return strconv.Itoa(int(c.MaxImageMiB)), nil
	case "max_file_mib":
		return strconv.Itoa(int(c.MaxFileMiB)), nil
	case "invite_host":
		return c.InviteHost, nil
	case "transport":
		return c.Transport, nil
	case "tor_exe":
		return c.TorExe, nil
	case "history":
		return strconv.FormatBool(c.History), nil
	case "max_peers":
		return strconv.Itoa(c.MaxPeers), nil
	}
	return "", ErrConfigKey
}

// Set assigns a key from a string ("burrow config set").
func (c *Config) Set(key, value string) error {
	parseBool := func(dst *bool) error {
		v, err := strconv.ParseBool(value)
		if err != nil {
			return err
		}
		*dst = v
		return nil
	}
	switch key {
	case "listen_port":
		n, err := strconv.ParseUint(value, 10, 16)
		if err != nil || n == 0 {
			return errors.New("store: listen_port must be 1–65535")
		}
		c.ListenPort = uint16(n)
	case "display_name":
		if len(value) > 32 {
			return errors.New("store: display_name must be ≤ 32 bytes")
		}
		c.DisplayName = value
	case "typing":
		return parseBool(&c.Typing)
	case "timestamps":
		return parseBool(&c.Timestamps)
	case "auto_reconnect":
		return parseBool(&c.AutoReconnect)
	case "auto_accept_from_verified":
		return parseBool(&c.AutoAcceptFromVerified)
	case "paranoid_images":
		return parseBool(&c.ParanoidImages)
	case "mdns":
		return parseBool(&c.MDNS)
	case "open_links":
		return parseBool(&c.OpenLinks)
	case "image_dir":
		c.ImageDir = value
	case "max_image_mib":
		n, err := strconv.ParseUint(value, 10, 32)
		if err != nil || n == 0 {
			return errors.New("store: max_image_mib must be ≥ 1")
		}
		c.MaxImageMiB = uint32(n)
	case "max_file_mib":
		n, err := strconv.ParseUint(value, 10, 32)
		if err != nil || n > MaxFileMiBLimit {
			return errors.New("store: max_file_mib must be 0 (refuse files) to 1048576")
		}
		c.MaxFileMiB = uint32(n)
	case "invite_host":
		c.InviteHost = value
	case "transport":
		if value != "tcp" && value != "tor" && value != "both" {
			return errors.New("store: transport must be tcp, tor or both")
		}
		c.Transport = value
	case "tor_exe":
		c.TorExe = value
	case "history":
		return parseBool(&c.History)
	case "max_peers":
		n, err := strconv.Atoi(value)
		if err != nil || n < 1 || n > 32 {
			return errors.New("store: max_peers must be 1–32")
		}
		c.MaxPeers = n
	default:
		return ErrConfigKey
	}
	return nil
}

// Keys lists the config keys in display order.
func ConfigKeys() []string {
	return []string{"listen_port", "display_name", "typing", "timestamps", "auto_reconnect",
		"auto_accept_from_verified", "paranoid_images", "mdns", "open_links", "image_dir", "max_image_mib", "max_file_mib", "invite_host",
		"transport", "tor_exe", "history", "max_peers"}
}
