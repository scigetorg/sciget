//go:build unix && !(darwin || dragonfly || freebsd || linux || netbsd || openbsd)

package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"syscall"

	"golang.org/x/sys/unix"
)

var catalogDirLocks sync.Map

func isReadOnlyOrPermError(err error) bool {
	return errors.Is(err, os.ErrPermission) || errors.Is(err, syscall.EROFS)
}

// lockFileExclusive acquires both an in-process inode mutex and a POSIX fcntl
// write lock on lockPath for Unix platforms without BSD flock (such as
// Solaris/illumos).
func lockFileExclusive(lockPath string) (*catalogLock, error) {
	if err := os.MkdirAll(filepath.Dir(lockPath), 0o755); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	closeAndUnlock, err := lockOpenFileExclusive(f)
	if err != nil {
		return nil, err
	}
	gen, err := readLockFileGen(f)
	if err != nil {
		_ = closeAndUnlock()
		return nil, err
	}
	res := &catalogLock{
		gen: gen,
		unlock: func() {
			_ = closeAndUnlock()
		},
	}
	res.bump = func() error {
		res.gen++
		return writeLockFileGen(f, res.gen)
	}
	res.syncGen = func(minGen uint64) error {
		if minGen <= res.gen {
			return nil
		}
		res.gen = minGen
		return writeLockFileGen(f, res.gen)
	}
	return res, nil
}

// lockFileShared acquires both an in-process inode mutex and a shared POSIX
// fcntl read lock when lockPath exists. When it does not exist yet, returns
// ok=false.
func lockFileShared(lockPath string) (func(), uint64, bool, error) {
	f, err := os.Open(lockPath)
	if errors.Is(err, os.ErrNotExist) {
		return func() {}, 0, false, nil
	}
	if err != nil {
		return nil, 0, false, err
	}
	closeAndUnlock, err := lockOpenFileShared(f)
	if err != nil {
		return nil, 0, false, err
	}
	gen, err := readLockFileGen(f)
	if err != nil {
		_ = closeAndUnlock()
		return nil, 0, false, err
	}
	return func() {
		_ = closeAndUnlock()
	}, gen, true, nil
}

func openFileInodeKey(f *os.File) (string, error) {
	var st unix.Stat_t
	if err := unix.Fstat(int(f.Fd()), &st); err != nil {
		return "", err
	}
	return fmt.Sprintf("inode:%d:%d", st.Dev, st.Ino), nil
}

// lockOpenFileExclusive acquires an in-process inode mutex and POSIX fcntl
// write lock directly on open file f. It takes ownership of f and always
// closes f BEFORE releasing the in-process mutex (on both success and error)
// so close(2) cannot drop another goroutine's process-owned fcntl lock.
func lockOpenFileExclusive(f *os.File) (func() error, error) {
	key, err := openFileInodeKey(f)
	if err != nil {
		_ = f.Close()
		return nil, err
	}
	v, _ := catalogDirLocks.LoadOrStore(key, &sync.Mutex{})
	mu := v.(*sync.Mutex)
	mu.Lock()
	lk := unix.Flock_t{Type: unix.F_WRLCK, Whence: 0, Start: 0, Len: 0}
	for {
		err = unix.FcntlFlock(f.Fd(), unix.F_SETLKW, &lk)
		if !errors.Is(err, syscall.EINTR) {
			break
		}
	}
	if err != nil {
		_ = f.Close()
		mu.Unlock()
		return nil, err
	}
	return func() error {
		unlk := unix.Flock_t{Type: unix.F_UNLCK, Whence: 0, Start: 0, Len: 0}
		_ = unix.FcntlFlock(f.Fd(), unix.F_SETLK, &unlk)
		err := f.Close()
		mu.Unlock()
		return err
	}, nil
}

// lockOpenFileShared acquires an in-process inode mutex and POSIX fcntl read
// lock directly on open file f. It takes ownership of f and always closes f
// BEFORE releasing the in-process mutex (on both success and error).
func lockOpenFileShared(f *os.File) (func() error, error) {
	key, err := openFileInodeKey(f)
	if err != nil {
		_ = f.Close()
		return nil, err
	}
	v, _ := catalogDirLocks.LoadOrStore(key, &sync.Mutex{})
	mu := v.(*sync.Mutex)
	mu.Lock()
	lk := unix.Flock_t{Type: unix.F_RDLCK, Whence: 0, Start: 0, Len: 0}
	for {
		err = unix.FcntlFlock(f.Fd(), unix.F_SETLKW, &lk)
		if !errors.Is(err, syscall.EINTR) {
			break
		}
	}
	if err != nil {
		_ = f.Close()
		mu.Unlock()
		return nil, err
	}
	return func() error {
		unlk := unix.Flock_t{Type: unix.F_UNLCK, Whence: 0, Start: 0, Len: 0}
		_ = unix.FcntlFlock(f.Fd(), unix.F_SETLK, &unlk)
		err := f.Close()
		mu.Unlock()
		return err
	}, nil
}
