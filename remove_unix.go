//go:build unix

package main

import (
	"errors"
	"os"
	"syscall"

	"golang.org/x/sys/unix"
)

// removePinnedEmptyDir removes an empty directory entry inside parent using
// unlinkat(..., AT_REMOVEDIR) so it never unlinks a replacement symlink or
// regular file and never recursively removes a non-empty directory.
func removePinnedEmptyDir(parent *os.Root, base string) error {
	df, err := parent.Open(".")
	if err != nil {
		return err
	}
	defer df.Close()
	err = unix.Unlinkat(int(df.Fd()), base, unix.AT_REMOVEDIR)
	if errors.Is(err, syscall.ENOENT) {
		return nil
	}
	return err
}

// removePinnedNonDir unlinks a non-directory entry (such as a symlink) inside
// parent using unlinkat(..., 0) so it never falls back to AT_REMOVEDIR and
// never removes a replacement empty directory.
func removePinnedNonDir(parent *os.Root, base string) error {
	df, err := parent.Open(".")
	if err != nil {
		return err
	}
	defer df.Close()
	err = unix.Unlinkat(int(df.Fd()), base, 0)
	if errors.Is(err, syscall.ENOENT) {
		return nil
	}
	return err
}
