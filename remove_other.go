//go:build !unix

package main

import (
	"errors"
	"os"
)

func removePinnedEmptyDir(parent *os.Root, base string) error {
	if err := parent.Remove(base); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

func removePinnedNonDir(parent *os.Root, base string) error {
	if err := parent.Remove(base); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}
