//go:build unix

package main

import (
	"errors"
	"os"
	"path/filepath"
	"syscall"
)

// lockCatalogDir acquires an exclusive advisory file lock on dir so concurrent
// refreshes cannot interleave commits or rollbacks.
func lockCatalogDir(dir string) (func(), error) {
	f, err := os.OpenFile(filepath.Join(dir, ".refresh.lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	for {
		err = syscall.Flock(int(f.Fd()), syscall.LOCK_EX)
		if !errors.Is(err, syscall.EINTR) {
			break
		}
	}
	if err != nil {
		f.Close()
		return nil, err
	}
	return func() {
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		_ = f.Close()
	}, nil
}
