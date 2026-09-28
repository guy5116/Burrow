// Command burrow is the CLI/TUI for Burrow. Phase 0 ships the engine
// skeleton only; subcommands arrive in Phase 1 (CLAUDE.md §9).
package main

import (
	"fmt"
	"os"
)

func main() {
	fmt.Fprintln(os.Stderr, "burrow: not yet functional (Phase 0 skeleton); see docs/STATUS.md")
	os.Exit(2)
}
