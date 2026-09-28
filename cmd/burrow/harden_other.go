//go:build !linux && !darwin

package main

// harden has nothing portable to do here; Windows has no core-dump limit API
// usable without WER configuration (documented in docs/SECURITY.md).
func harden() {}
