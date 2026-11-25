//go:build !unix

package main

import (
	"path/filepath"
	"sync"
)

var catalogDirLocks sync.Map

// lockCatalogDir serializes in-process refreshes on dir on platforms without
// POSIX flock.
func lockCatalogDir(dir string) (func(), error) {
	key := filepath.Clean(dir)
	v, _ := catalogDirLocks.LoadOrStore(key, &sync.Mutex{})
	mu := v.(*sync.Mutex)
	mu.Lock()
	return mu.Unlock, nil
}
