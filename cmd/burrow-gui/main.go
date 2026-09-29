//go:build gui

package main

import (
	"flag"
	"fmt"
	"log/slog"
	"os"

	"github.com/guy5116/burrow/internal/store"
	"github.com/guy5116/burrow/internal/ui/gui"
)

func main() {
	store.HardenProcess() // no core dumps, not dumpable (CLAUDE.md §3.6)
	cfgDir := flag.String("config", "", "config directory")
	dataDir := flag.String("data", "", "data directory")
	level := flag.String("log-level", "warn", "debug|info|warn|error (stderr)")
	flag.Parse()
	paths, err := store.DefaultPaths()
	if err != nil && (*cfgDir == "" || *dataDir == "") {
		fmt.Fprintln(os.Stderr, "burrow-gui: cannot determine home directory; pass --config and --data")
		os.Exit(1)
	}
	if *cfgDir != "" {
		paths.Config = *cfgDir
	}
	if *dataDir != "" {
		paths.Data = *dataDir
	}
	cfg, err := store.LoadConfig(paths.Config)
	if err != nil {
		fmt.Fprintln(os.Stderr, "burrow-gui: config:", err)
		os.Exit(1)
	}
	var lv slog.Level
	_ = lv.UnmarshalText([]byte(*level))
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: lv}))
	os.Exit(gui.Run(paths, cfg, logger))
}
