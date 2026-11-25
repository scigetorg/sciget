//go:build !unix && !windows

package main

import (
	"sync"
)

var catalogDirLocks sync.Map

// lockCatalogDir serializes in-process refreshes and cache removals on
// platforms without Unix or Windows file locking APIs.
func lockCatalogDir(dir string) (func(), error) {
	key := catalogLockPath(dir)
	v, _ := catalogDirLocks.LoadOrStore(key, &sync.Mutex{})
	mu := v.(*sync.Mutex)
	mu.Lock()
	return mu.Unlock, nil
}

func lockCatalogDirShared(dir string) (func(), bool, error) {
	unlock, err := lockCatalogDir(dir)
	if err != nil {
		return nil, false, err
	}
	return unlock, true, nil
}
