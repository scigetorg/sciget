//go:build !unix && !windows

package main

import (
	"path/filepath"
	"sync"
)

var catalogDirLocks sync.Map

// lockCatalogDir serializes in-process refreshes on platforms without Unix or
// Windows file locking APIs.
func lockCatalogDir(dir string) (func(), error) {
	key := filepath.Clean(dir)
	v, _ := catalogDirLocks.LoadOrStore(key, &sync.Mutex{})
	mu := v.(*sync.Mutex)
	mu.Lock()
	return mu.Unlock, nil
}

func lockCatalogDirShared(dir string) (func(), error) {
	return lockCatalogDir(dir)
}
