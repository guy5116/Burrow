//go:build !cli_wip

// Command burrow is the CLI/TUI for Burrow. The real entry point is being
// assembled in main_wip.go (build tag cli_wip) during Phase 1.
package main

import (
	"fmt"
	"os"
)

func main() {
	harden()
	fmt.Fprintln(os.Stderr, "burrow: CLI not yet wired (Phase 1 in progress); see docs/STATUS.md")
	os.Exit(2)
}
