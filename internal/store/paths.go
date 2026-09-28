package store

import (
	"os"
	"path/filepath"
	"runtime"
)

// Paths are the config and data directories (§7).
type Paths struct{ Config, Data string }

// DefaultPaths follows XDG on Unix, %APPDATA% on Windows and
// ~/Library/Application Support on macOS.
func DefaultPaths() (Paths, error) {
	switch runtime.GOOS {
	case "windows":
		base := os.Getenv("APPDATA")
		if base == "" {
			return Paths{}, os.ErrNotExist
		}
		d := filepath.Join(base, "burrow")
		return Paths{Config: d, Data: d}, nil
	case "darwin":
		home, err := os.UserHomeDir()
		if err != nil {
			return Paths{}, err
		}
		d := filepath.Join(home, "Library", "Application Support", "burrow")
		return Paths{Config: d, Data: d}, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return Paths{}, err
	}
	cfg := os.Getenv("XDG_CONFIG_HOME")
	if cfg == "" {
		cfg = filepath.Join(home, ".config")
	}
	data := os.Getenv("XDG_DATA_HOME")
	if data == "" {
		data = filepath.Join(home, ".local", "share")
	}
	return Paths{Config: filepath.Join(cfg, "burrow"), Data: filepath.Join(data, "burrow")}, nil
}
