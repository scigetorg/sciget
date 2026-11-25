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

// lockCatalogDir serializes in-process refreshes and cache removals on
// platforms without Unix or Windows file locking APIs.
func lockCatalogDir(dir string) (*catalogLock, error) {
	key := catalogLockPath(dir)
	v, _ := catalogDirLocks.LoadOrStore(key, &memCatalogLock{})
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

func lockCatalogDirShared(dir string) (func(), uint64, bool, error) {
	lk, err := lockCatalogDir(dir)
	if err != nil {
		return nil, 0, false, err
	}
	return lk.unlock, lk.gen, true, nil
}
