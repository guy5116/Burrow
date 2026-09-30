//go:build windows

package core

import "os"

// markDownloaded tells Windows that a received file came from the internet
// (the Mark of the Web), so that it warns before running it and opens
// documents in protected view.
func markDownloaded(path string) {
	_ = os.WriteFile(path+":Zone.Identifier", []byte("[ZoneTransfer]\r\nZoneId=3\r\n"), 0o600) // #nosec G306 -- not secret
}
