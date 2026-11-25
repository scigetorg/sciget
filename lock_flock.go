//go:build darwin || dragonfly || freebsd || linux || netbsd || openbsd

package main

import (
	"errors"
	"os"
	"path/filepath"
	"syscall"
)

// lockCatalogDir acquires an exclusive advisory flock on dir so concurrent
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

// lockCatalogDirShared acquires a shared advisory flock on dir when a refresh
// lock file exists, so readers wait for any in-progress refresh commit or
// rollback to finish. When the lock file does not exist yet, it returns ok=false
// so the caller skips the uncommitted catalog directory instead of racing a
// first refresh.
func lockCatalogDirShared(dir string) (func(), bool, error) {
	f, err := os.Open(filepath.Join(dir, ".refresh.lock"))
	if errors.Is(err, os.ErrNotExist) {
		return func() {}, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	for {
		err = syscall.Flock(int(f.Fd()), syscall.LOCK_SH)
		if !errors.Is(err, syscall.EINTR) {
			break
		}
	}
	if err != nil {
		f.Close()
		return nil, false, err
	}
	return func() {
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		_ = f.Close()
	}, true, nil
}
