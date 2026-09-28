//go:build gui

package main

import (
	"fmt"
	"os"
)

func main() {
	fmt.Fprintln(os.Stderr, "burrow-gui: not yet functional (Phase 3); see docs/STATUS.md")
	os.Exit(2)
}
