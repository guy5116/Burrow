//go:build !e2e

package main

import "time"

// testChunkDelay slows the file-writer in binaries built for the end-to-end
// tests (-tags e2e). A release build has no such knob.
func testChunkDelay() time.Duration { return 0 }
