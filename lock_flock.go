//go:build darwin || dragonfly || freebsd || linux || netbsd || openbsd

package main

import (
	"errors"
	"os"
	"path/filepath"
	"syscall"
)

func isReadOnlyOrPermError(err error) bool {
	return errors.Is(err, os.ErrPermission) || errors.Is(err, syscall.EROFS)
}

func pathFileID(path string, _ bool) (dev, ino, nlink uint64, ok bool) {
	info, err := os.Stat(path)
	if err != nil {
		return 0, 0, 0, false
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, 0, 0, false
	}
	return uint64(st.Dev), uint64(st.Ino), uint64(st.Nlink), true
}

// lockFileExclusive acquires an exclusive advisory flock on lockPath so
// concurrent writers cannot interleave updates, commits, rollbacks, or deletes.
func lockFileExclusive(lockPath string) (*catalogLock, error) {
	return lockFileExclusiveMode(lockPath, false)
}

func tryLockFileExclusive(lockPath string) (*catalogLock, error) {
	return lockFileExclusiveMode(lockPath, true)
}

func lockFileExclusiveMode(lockPath string, nonBlocking bool) (*catalogLock, error) {
	if err := os.MkdirAll(filepath.Dir(lockPath), 0o755); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	how := syscall.LOCK_EX
	if nonBlocking {
		how |= syscall.LOCK_NB
	}
	for {
		err = syscall.Flock(int(f.Fd()), how)
		if !errors.Is(err, syscall.EINTR) {
			break
		}
	}
	if err != nil {
		f.Close()
		if nonBlocking && (errors.Is(err, syscall.EWOULDBLOCK) || errors.Is(err, syscall.EAGAIN)) {
			return nil, errLockBusy
		}
		return nil, err
	}
	fileInfo, err := f.Stat()
	if err != nil {
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		_ = f.Close()
		return nil, err
	}
	gen, err := readLockFileGen(f)
	if err != nil {
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		_ = f.Close()
		return nil, err
	}
	lk := &catalogLock{
		gen:      gen,
		fileInfo: fileInfo,
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
func lockFileShared(lockPath string) (func(), uint64, os.FileInfo, bool, error) {
	return lockFileSharedMode(lockPath, false)
}

func tryLockFileShared(lockPath string) (func(), uint64, os.FileInfo, bool, error) {
	return lockFileSharedMode(lockPath, true)
}

func lockFileSharedMode(lockPath string, nonBlocking bool) (func(), uint64, os.FileInfo, bool, error) {
	f, err := os.Open(lockPath)
	if errors.Is(err, os.ErrNotExist) {
		return func() {}, 0, nil, false, nil
	}
	if err != nil {
		return nil, 0, nil, false, err
	}
	how := syscall.LOCK_SH
	if nonBlocking {
		how |= syscall.LOCK_NB
	}
	for {
		err = syscall.Flock(int(f.Fd()), how)
		if !errors.Is(err, syscall.EINTR) {
			break
		}
	}
	if err != nil {
		f.Close()
		if nonBlocking && (errors.Is(err, syscall.EWOULDBLOCK) || errors.Is(err, syscall.EAGAIN)) {
			return nil, 0, nil, false, errLockBusy
		}
		return nil, 0, nil, false, err
	}
	fileInfo, err := f.Stat()
	if err != nil {
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		_ = f.Close()
		return nil, 0, nil, false, err
	}
	gen, err := readLockFileGen(f)
	if err != nil {
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		_ = f.Close()
		return nil, 0, nil, false, err
	}
	return func() {
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		_ = f.Close()
	}, gen, fileInfo, true, nil
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
