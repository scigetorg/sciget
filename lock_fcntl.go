//go:build unix && !(darwin || dragonfly || freebsd || linux || netbsd || openbsd)

package main

import (
	"errors"
	"os"
	"path/filepath"
	"sync"
	"syscall"

	"golang.org/x/sys/unix"
)

var catalogDirLocks sync.Map

// lockCatalogDir acquires both an in-process mutex and a POSIX fcntl write
// lock for dir on Unix platforms without BSD flock (such as Solaris/illumos).
func lockCatalogDir(dir string) (func(), error) {
	lockPath := catalogLockPath(dir)
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
	return func() {
		unlk := unix.Flock_t{Type: unix.F_UNLCK, Whence: 0, Start: 0, Len: 0}
		_ = unix.FcntlFlock(f.Fd(), unix.F_SETLK, &unlk)
		_ = f.Close()
		mu.Unlock()
	}, nil
}

// lockCatalogDirShared acquires both an in-process mutex and a shared POSIX
// fcntl read lock when a lock file exists. When the lock file does not exist
// yet, it returns ok=false.
func lockCatalogDirShared(dir string) (func(), bool, error) {
	lockPath := catalogLockPath(dir)
	v, _ := catalogDirLocks.LoadOrStore(lockPath, &sync.Mutex{})
	mu := v.(*sync.Mutex)
	mu.Lock()

	f, err := os.Open(lockPath)
	if errors.Is(err, os.ErrNotExist) {
		mu.Unlock()
		return func() {}, false, nil
	}
	if err != nil {
		mu.Unlock()
		return nil, false, err
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
		return nil, false, err
	}
	return func() {
		unlk := unix.Flock_t{Type: unix.F_UNLCK, Whence: 0, Start: 0, Len: 0}
		_ = unix.FcntlFlock(f.Fd(), unix.F_SETLK, &unlk)
		_ = f.Close()
		mu.Unlock()
	}, true, nil
}
