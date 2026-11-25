//go:build unix

package main

import (
	"fmt"
	"os"
	"syscall"
	"testing"
)

func TestIsReadOnlyOrPermErrorUnix(t *testing.T) {
	if !isReadOnlyOrPermError(os.ErrPermission) {
		t.Fatal("expected os.ErrPermission to be treated as read-only/perm error")
	}
	if !isReadOnlyOrPermError(syscall.EROFS) {
		t.Fatal("expected syscall.EROFS to be treated as read-only/perm error")
	}
	if !isReadOnlyOrPermError(fmt.Errorf("wrapped: %w", syscall.EROFS)) {
		t.Fatal("expected wrapped syscall.EROFS to be treated as read-only/perm error")
	}
	if isReadOnlyOrPermError(os.ErrNotExist) {
		t.Fatal("os.ErrNotExist must not be treated as read-only/perm error")
	}
}
