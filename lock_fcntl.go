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

// lockFileExclusive acquires both an in-process mutex and a POSIX fcntl write
// lock on lockPath for Unix platforms without BSD flock (such as Solaris/illumos).
func lockFileExclusive(lockPath string) (*catalogLock, error) {
	v, _ := catalogDirLocks.LoadOrStore(lockPath, &sync.Mutex{})
	mu := v.(*sync.Mutex)
	mu.Lock()

	if err := os.MkdirAll(filepath.Dir(lockPath), 0o755); err != nil {
		mu.Unlock()
		return nil, err
	}
	f, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		mu.Unlock()
		return nil, err
	}
	lk := unix.Flock_t{
		Type:   unix.F_WRLCK,
		Whence: 0,
		Start:  0,
		Len:    0,
	}
	for {
		err = unix.FcntlFlock(f.Fd(), unix.F_SETLKW, &lk)
		if !errors.Is(err, syscall.EINTR) {
			break
		}
	}
	if err != nil {
		f.Close()
		mu.Unlock()
		return nil, err
	}
	gen, err := readLockFileGen(f)
	if err != nil {
		unlk := unix.Flock_t{Type: unix.F_UNLCK, Whence: 0, Start: 0, Len: 0}
		_ = unix.FcntlFlock(f.Fd(), unix.F_SETLK, &unlk)
		_ = f.Close()
		mu.Unlock()
		return nil, err
	}
	res := &catalogLock{
		gen: gen,
		unlock: func() {
			unlk := unix.Flock_t{Type: unix.F_UNLCK, Whence: 0, Start: 0, Len: 0}
			_ = unix.FcntlFlock(f.Fd(), unix.F_SETLK, &unlk)
			_ = f.Close()
			mu.Unlock()
		},
	}
	res.bump = func() error {
		res.gen++
		return writeLockFileGen(f, res.gen)
	}
	return res, nil
}

// lockFileShared acquires both an in-process mutex and a shared POSIX fcntl
// read lock when lockPath exists. When it does not exist yet, returns ok=false.
func lockFileShared(lockPath string) (func(), uint64, bool, error) {
	v, _ := catalogDirLocks.LoadOrStore(lockPath, &sync.Mutex{})
	mu := v.(*sync.Mutex)
	mu.Lock()

	f, err := os.Open(lockPath)
	if errors.Is(err, os.ErrNotExist) {
		mu.Unlock()
		return func() {}, 0, false, nil
	}
	if err != nil {
		mu.Unlock()
		return nil, 0, false, err
	}
	lk := unix.Flock_t{Type: unix.F_RDLCK, Whence: 0, Start: 0, Len: 0}
	for {
		err = unix.FcntlFlock(f.Fd(), unix.F_SETLKW, &lk)
		if !errors.Is(err, syscall.EINTR) {
			break
		}
	}
	if err != nil {
		f.Close()
		mu.Unlock()
		return nil, 0, false, err
	}
	gen, err := readLockFileGen(f)
	if err != nil {
		unlk := unix.Flock_t{Type: unix.F_UNLCK, Whence: 0, Start: 0, Len: 0}
		_ = unix.FcntlFlock(f.Fd(), unix.F_SETLK, &unlk)
		_ = f.Close()
		mu.Unlock()
		return nil, 0, false, err
	}
	return func() {
		unlk := unix.Flock_t{Type: unix.F_UNLCK, Whence: 0, Start: 0, Len: 0}
		_ = unix.FcntlFlock(f.Fd(), unix.F_SETLK, &unlk)
		_ = f.Close()
		mu.Unlock()
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
// write lock directly on open file f.
func lockOpenFileExclusive(f *os.File) (func(), error) {
	key, err := openFileInodeKey(f)
	if err != nil {
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
		mu.Unlock()
		return nil, err
	}
	return func() {
		unlk := unix.Flock_t{Type: unix.F_UNLCK, Whence: 0, Start: 0, Len: 0}
		_ = unix.FcntlFlock(f.Fd(), unix.F_SETLK, &unlk)
		mu.Unlock()
	}, nil
}

// lockOpenFileShared acquires an in-process inode mutex and POSIX fcntl read
// lock directly on open file f.
func lockOpenFileShared(f *os.File) (func(), error) {
	key, err := openFileInodeKey(f)
	if err != nil {
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
		mu.Unlock()
		return nil, err
	}
	return func() {
		unlk := unix.Flock_t{Type: unix.F_UNLCK, Whence: 0, Start: 0, Len: 0}
		_ = unix.FcntlFlock(f.Fd(), unix.F_SETLK, &unlk)
		mu.Unlock()
	}, nil
}
