//go:build e2e

package main

import (
	"os"
	"time"
)

// testChunkDelay reads BURROW_DEBUG_CHUNK_DELAY so a test can stop a transfer
// half way: on loopback it would otherwise finish before anything can happen.
func testChunkDelay() time.Duration {
	d, _ := time.ParseDuration(os.Getenv("BURROW_DEBUG_CHUNK_DELAY"))
	return max(d, 0)
}
