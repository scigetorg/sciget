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
func lockCatalogDir(dir string) (*catalogLock, error) {
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
	gen, err := readLockFileGen(f)
	if err != nil {
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		_ = f.Close()
		return nil, err
	}
	lk := &catalogLock{
		gen: gen,
		unlock: func() {
			_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
			_ = f.Close()
		},
	}
	lk.bump = func() error {
		lk.gen++
		return writeLockFileGen(f, lk.gen)
	}
	return lk, nil
}

// lockCatalogDirShared acquires a shared advisory flock for dir when a lock
// file exists, so readers wait for any in-progress refresh commit, rollback,
// or cache removal to finish. When the lock file does not exist yet, it
// returns ok=false.
func lockCatalogDirShared(dir string) (func(), uint64, bool, error) {
	f, err := os.Open(catalogLockPath(dir))
	if errors.Is(err, os.ErrNotExist) {
		return func() {}, 0, false, nil
	}
	if err != nil {
		return nil, 0, false, err
	}
	for {
		err = syscall.Flock(int(f.Fd()), syscall.LOCK_SH)
		if !errors.Is(err, syscall.EINTR) {
			break
		}
	}
	if err != nil {
		f.Close()
		return nil, 0, false, err
	}
	gen, err := readLockFileGen(f)
	if err != nil {
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		_ = f.Close()
		return nil, 0, false, err
	}
	return func() {
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		_ = f.Close()
	}, gen, true, nil
}
