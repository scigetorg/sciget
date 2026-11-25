//go:build darwin || dragonfly || freebsd || linux || netbsd || openbsd

package main

import (
	"errors"
	"os"
	"path/filepath"
	"syscall"
)

// lockFileExclusive acquires an exclusive advisory flock on lockPath so
// concurrent writers cannot interleave updates, commits, rollbacks, or deletes.
func lockFileExclusive(lockPath string) (*catalogLock, error) {
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
	lk.syncGen = func(minGen uint64) error {
		if minGen <= lk.gen {
			return nil
		}
		lk.gen = minGen
		return writeLockFileGen(f, lk.gen)
	}
	return lk, nil
}

// lockFileShared acquires a shared advisory flock on lockPath when it exists,
// so readers wait for any in-progress writer to finish. When lockPath does not
// exist yet, it returns ok=false.
func lockFileShared(lockPath string) (func(), uint64, bool, error) {
	f, err := os.Open(lockPath)
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

// lockOpenFileExclusive acquires an exclusive advisory flock directly on the
// open file f (following symlinks natively without creating a sibling file).
// It takes ownership of f: on error it closes f before returning; on success
// the returned function releases the lock and closes f.
func lockOpenFileExclusive(f *os.File) (func() error, error) {
	var err error
	for {
		err = syscall.Flock(int(f.Fd()), syscall.LOCK_EX)
		if !errors.Is(err, syscall.EINTR) {
			break
		}
	}
	if err != nil {
		_ = f.Close()
		return nil, err
	}
	return func() error {
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		return f.Close()
	}, nil
}

// lockOpenFileShared acquires a shared advisory flock directly on open file f.
// It takes ownership of f: on error it closes f before returning; on success
// the returned function releases the lock and closes f.
func lockOpenFileShared(f *os.File) (func() error, error) {
	var err error
	for {
		err = syscall.Flock(int(f.Fd()), syscall.LOCK_SH)
		if !errors.Is(err, syscall.EINTR) {
			break
		}
	}
	if err != nil {
		_ = f.Close()
		return nil, err
	}
	return func() error {
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		return f.Close()
	}, nil
}
