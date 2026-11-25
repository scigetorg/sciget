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
// lock on dir for Unix platforms without BSD flock (such as Solaris/illumos).
func lockCatalogDir(dir string) (func(), error) {
	key := filepath.Clean(dir)
	v, _ := catalogDirLocks.LoadOrStore(key, &sync.Mutex{})
	mu := v.(*sync.Mutex)
	mu.Lock()

	f, err := os.OpenFile(filepath.Join(dir, ".refresh.lock"), os.O_CREATE|os.O_RDWR, 0o600)
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
// fcntl read lock when a refresh lock file exists.
func lockCatalogDirShared(dir string) (func(), error) {
	key := filepath.Clean(dir)
	v, _ := catalogDirLocks.LoadOrStore(key, &sync.Mutex{})
	mu := v.(*sync.Mutex)
	mu.Lock()

	f, err := os.Open(filepath.Join(dir, ".refresh.lock"))
	if errors.Is(err, os.ErrNotExist) {
		return mu.Unlock, nil
	}
	if err != nil {
		mu.Unlock()
		return nil, err
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
		return nil, err
	}
	return func() {
		unlk := unix.Flock_t{Type: unix.F_UNLCK, Whence: 0, Start: 0, Len: 0}
		_ = unix.FcntlFlock(f.Fd(), unix.F_SETLK, &unlk)
		_ = f.Close()
		mu.Unlock()
	}, nil
}
