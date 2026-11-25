//go:build windows

package main

import (
	"errors"
	"os"
	"path/filepath"

	"golang.org/x/sys/windows"
)

// lockFileExclusive acquires an exclusive Windows file lock on lockPath so
// concurrent writers cannot interleave updates, commits, rollbacks, or deletes.
func lockFileExclusive(lockPath string) (*catalogLock, error) {
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
	gen, err := readLockFileGen(f)
	if err != nil {
		var uol windows.Overlapped
		_ = windows.UnlockFileEx(windows.Handle(f.Fd()), 0, 1, 0, &uol)
		_ = f.Close()
		return nil, err
	}
	lk := &catalogLock{
		gen: gen,
		unlock: func() {
			var uol windows.Overlapped
			_ = windows.UnlockFileEx(windows.Handle(f.Fd()), 0, 1, 0, &uol)
			_ = f.Close()
		},
	}
	lk.bump = func() error {
		lk.gen++
		return writeLockFileGen(f, lk.gen)
	}
	return lk, nil
}

// lockFileShared acquires a shared Windows file lock when lockPath exists, so
// readers wait for any in-progress writer to finish. When lockPath does not
// exist yet, it returns ok=false.
func lockFileShared(lockPath string) (func(), uint64, bool, error) {
	f, err := os.Open(lockPath)
	if errors.Is(err, os.ErrNotExist) {
		return func() {}, 0, false, nil
	}
	if err != nil {
		return nil, 0, false, err
	}
	var ol windows.Overlapped
	if err := windows.LockFileEx(windows.Handle(f.Fd()), 0, 0, 1, 0, &ol); err != nil {
		f.Close()
		return nil, 0, false, err
	}
	gen, err := readLockFileGen(f)
	if err != nil {
		var uol windows.Overlapped
		_ = windows.UnlockFileEx(windows.Handle(f.Fd()), 0, 1, 0, &uol)
		_ = f.Close()
		return nil, 0, false, err
	}
	return func() {
		var uol windows.Overlapped
		_ = windows.UnlockFileEx(windows.Handle(f.Fd()), 0, 1, 0, &uol)
		_ = f.Close()
	}, gen, true, nil
}

// lockOpenFileExclusive acquires an exclusive Windows file lock directly on
// open file f (following symlinks, junctions, and case-insensitive paths
// natively in the OS without creating a sibling lock file). The returned
// function releases the lock and closes f.
func lockOpenFileExclusive(f *os.File) (func() error, error) {
	var ol windows.Overlapped
	if err := windows.LockFileEx(windows.Handle(f.Fd()), windows.LOCKFILE_EXCLUSIVE_LOCK, 0, 1, 0, &ol); err != nil {
		return nil, err
	}
	return func() error {
		var uol windows.Overlapped
		_ = windows.UnlockFileEx(windows.Handle(f.Fd()), 0, 1, 0, &uol)
		return f.Close()
	}, nil
}

// lockOpenFileShared acquires a shared Windows file lock directly on open file f.
// The returned function releases the lock and closes f.
func lockOpenFileShared(f *os.File) (func() error, error) {
	var ol windows.Overlapped
	if err := windows.LockFileEx(windows.Handle(f.Fd()), 0, 0, 1, 0, &ol); err != nil {
		return nil, err
	}
	return func() error {
		var uol windows.Overlapped
		_ = windows.UnlockFileEx(windows.Handle(f.Fd()), 0, 1, 0, &uol)
		return f.Close()
	}, nil
}
