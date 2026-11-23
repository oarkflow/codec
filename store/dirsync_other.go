//go:build !unix

package store

// Go does not provide a portable directory flush on non-Unix targets. File
// contents are still synced where requested; the rename remains atomic.
func syncDir(string) error { return nil }
