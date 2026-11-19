//go:build unix

package store

import (
	"os"
	"syscall"
)

func mapReadOnly(f *os.File, size int64) ([]byte, error) {
	if size == 0 {
		return nil, nil
	}
	if size < 0 || uint64(size) > uint64(^uint(0)>>1) {
		return nil, ErrTooLarge
	}
	return syscall.Mmap(int(f.Fd()), 0, int(size), syscall.PROT_READ, syscall.MAP_SHARED)
}

func unmapReadOnly(b []byte) error {
	if len(b) == 0 {
		return nil
	}
	return syscall.Munmap(b)
}
