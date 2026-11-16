//go:build linux

package main

import (
	"fmt"
	"syscall"
)

// freeSpace returns the bytes an unprivileged user may still write to the
// filesystem holding dir. Quotas are not counted.
func freeSpace(dir string) (int64, error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(dir, &st); err != nil {
		return 0, fmt.Errorf("checking free space in %s: %w", dir, err)
	}
	return int64(st.Bavail) * int64(st.Bsize), nil
}
