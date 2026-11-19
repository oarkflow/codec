//go:build linux || darwin

package store

import (
	"errors"
	"os"
	"syscall"
)

func openSegmentFile(path string, create, syncWrites bool) (*os.File, bool, error) {
	flags := syscall.O_RDWR | syscall.O_CLOEXEC
	if create {
		flags |= syscall.O_CREAT | syscall.O_EXCL
	}
	dsync := syncWrites
	if dsync {
		flags |= syscall.O_DSYNC
	}
	fd, err := syscall.Open(path, flags, 0o644)
	if err != nil && dsync && (errors.Is(err, syscall.EINVAL) || errors.Is(err, syscall.EOPNOTSUPP)) {
		flags &^= syscall.O_DSYNC
		fd, err = syscall.Open(path, flags, 0o644)
		dsync = false
	}
	if err != nil {
		return nil, false, err
	}
	return os.NewFile(uintptr(fd), path), dsync, nil
}
