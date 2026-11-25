//go:build !unix && !windows

package main

import (
	"errors"
	"os"
	"sync"
)

func isReadOnlyOrPermError(err error) bool {
	return errors.Is(err, os.ErrPermission)
}

func pathFileID(_ string, _ bool) (dev, ino, nlink uint64, ok bool) {
	return 0, 0, 0, false
}

type memCatalogLock struct {
	mu  sync.Mutex
	gen uint64
}

var (
	catalogDirLocks sync.Map
	openFileMu      sync.Mutex
)

// lockFileExclusive serializes in-process writers on platforms without Unix or
// Windows file locking APIs.
func lockFileExclusive(lockPath string) (*catalogLock, error) {
	v, _ := catalogDirLocks.LoadOrStore(lockPath, &memCatalogLock{})
	st := v.(*memCatalogLock)
	st.mu.Lock()
	lk := &catalogLock{
		gen:    st.gen,
		unlock: st.mu.Unlock,
	}
	lk.bump = func() error {
		st.gen++
		lk.gen = st.gen
		return nil
	}
	lk.syncGen = func(minGen uint64) error {
		if minGen > st.gen {
			st.gen = minGen
			lk.gen = minGen
		}
		return nil
	}
	return lk, nil
}

func lockFileShared(lockPath string) (func(), uint64, bool, error) {
	lk, err := lockFileExclusive(lockPath)
	if err != nil {
		return nil, 0, false, err
	}
	return lk.unlock, lk.gen, true, nil
}

func lockOpenFileExclusive(f *os.File) (func() error, error) {
	openFileMu.Lock()
	return func() error {
		err := f.Close()
		openFileMu.Unlock()
		return err
	}, nil
}

func lockOpenFileShared(f *os.File) (func() error, error) {
	openFileMu.Lock()
	return func() error {
		err := f.Close()
		openFileMu.Unlock()
		return err
	}, nil
}
