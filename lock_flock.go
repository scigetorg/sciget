//go:build darwin || dragonfly || freebsd || linux || netbsd || openbsd

package main

import (
	"errors"
	"os"
	"path/filepath"
	"syscall"
)

// lockCatalogDir acquires an exclusive advisory flock for dir so concurrent
// refreshes and cache removals cannot interleave commits, rollbacks, or deletes.
func lockCatalogDir(dir string) (func(), error) {
	lockPath := catalogLockPath(dir)
	if err := os.MkdirAll(filepath.Dir(lockPath), 0o755); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o600)
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

// lockCatalogDirShared acquires a shared advisory flock for dir when a lock
// file exists, so readers wait for any in-progress refresh commit, rollback,
// or cache removal to finish. When the lock file does not exist yet, it
// returns ok=false.
func lockCatalogDirShared(dir string) (func(), bool, error) {
	f, err := os.Open(catalogLockPath(dir))
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
