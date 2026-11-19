//go:build !unix

package store

import "os"

func mapReadOnly(*os.File, int64) ([]byte, error) { return nil, nil }
func unmapReadOnly([]byte) error                  { return nil }
