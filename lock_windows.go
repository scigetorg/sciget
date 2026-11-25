//go:build windows

package main

import (
	"errors"
	"os"
	"path/filepath"

	"golang.org/x/sys/windows"
)

// lockCatalogDir acquires an exclusive Windows file lock for dir so concurrent
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
	var ol windows.Overlapped
	if err := windows.LockFileEx(windows.Handle(f.Fd()), windows.LOCKFILE_EXCLUSIVE_LOCK, 0, 1, 0, &ol); err != nil {
		f.Close()
		return nil, err
	}
	return func() {
		var uol windows.Overlapped
		_ = windows.UnlockFileEx(windows.Handle(f.Fd()), 0, 1, 0, &uol)
		_ = f.Close()
	}, nil
}

// lockCatalogDirShared acquires a shared Windows file lock when a lock file
// exists, so readers wait for any in-progress commit, rollback, or removal.
// When the lock file does not exist yet, it returns ok=false.
func lockCatalogDirShared(dir string) (func(), bool, error) {
	f, err := os.Open(catalogLockPath(dir))
	if errors.Is(err, os.ErrNotExist) {
		return func() {}, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	var ol windows.Overlapped
	if err := windows.LockFileEx(windows.Handle(f.Fd()), 0, 0, 1, 0, &ol); err != nil {
		f.Close()
		return nil, false, err
	}
	return func() {
		var uol windows.Overlapped
		_ = windows.UnlockFileEx(windows.Handle(f.Fd()), 0, 1, 0, &uol)
		_ = f.Close()
	}, true, nil
}
