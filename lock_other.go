//go:build !unix && !windows

package main

import (
	"sync"
)

type memCatalogLock struct {
	mu  sync.Mutex
	gen uint64
}

var catalogDirLocks sync.Map

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
	return lk, nil
}

func lockFileShared(lockPath string) (func(), uint64, bool, error) {
	lk, err := lockFileExclusive(lockPath)
	if err != nil {
		return nil, 0, false, err
	}
	return lk.unlock, lk.gen, true, nil
}
