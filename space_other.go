//go:build !linux

package main

// freeSpace returns -1, meaning unknown; containers run only on Linux.
func freeSpace(dir string) (int64, error) { return -1, nil }
