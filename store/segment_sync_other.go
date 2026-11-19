//go:build !linux && !darwin

package store

import "os"

func openSegmentFile(path string, create, _ bool) (*os.File, bool, error) {
	flags := os.O_RDWR
	if create {
		flags |= os.O_CREATE | os.O_EXCL
	}
	f, err := os.OpenFile(path, flags, 0o644)
	return f, false, err
}
