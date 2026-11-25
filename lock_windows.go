//go:build windows

package main

import (
	"errors"
	"os"
	"path/filepath"

	"golang.org/x/sys/windows"
)

func isReadOnlyOrPermError(err error) bool {
	return errors.Is(err, os.ErrPermission) || errors.Is(err, windows.ERROR_WRITE_PROTECT)
}

func pathFileID(path string, isDir bool) (dev, ino, nlink uint64, ok bool) {
	p16, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return 0, 0, 0, false
	}
	flags := uint32(windows.FILE_ATTRIBUTE_NORMAL)
	if isDir {
		flags = windows.FILE_FLAG_BACKUP_SEMANTICS
	}
	h, err := windows.CreateFile(p16, 0, windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE, nil, windows.OPEN_EXISTING, flags, 0)
	if err != nil {
		return 0, 0, 0, false
	}
	defer windows.CloseHandle(h)
	var info windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(h, &info); err != nil {
		return 0, 0, 0, false
	}
	return uint64(info.VolumeSerialNumber), (uint64(info.FileIndexHigh) << 32) | uint64(info.FileIndexLow), uint64(info.NumberOfLinks), true
}

// lockFileExclusive acquires an exclusive Windows file lock on lockPath so
// concurrent writers cannot interleave updates, commits, rollbacks, or deletes.
func lockFileExclusive(lockPath string) (*catalogLock, error) {
	return lockFileExclusiveMode(lockPath, false)
}

func tryLockFileExclusive(lockPath string) (*catalogLock, error) {
	return lockFileExclusiveMode(lockPath, true)
}

func lockFileExclusiveMode(lockPath string, nonBlocking bool) (*catalogLock, error) {
	if err := os.MkdirAll(filepath.Dir(lockPath), 0o755); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	flags := uint32(windows.LOCKFILE_EXCLUSIVE_LOCK)
	if nonBlocking {
		flags |= windows.LOCKFILE_FAIL_IMMEDIATELY
	}
	var ol windows.Overlapped
	if err := windows.LockFileEx(windows.Handle(f.Fd()), flags, 0, 1, 0, &ol); err != nil {
		f.Close()
		if nonBlocking && errors.Is(err, windows.ERROR_LOCK_VIOLATION) {
			return nil, errLockBusy
		}
		return nil, err
	}
	gen, err := readLockFileGen(f)
	if err != nil {
		var uol windows.Overlapped
		_ = windows.UnlockFileEx(windows.Handle(f.Fd()), 0, 1, 0, &uol)
		_ = f.Close()
		return nil, err
	}
	lk := &catalogLock{
		gen: gen,
		unlock: func() {
			var uol windows.Overlapped
			_ = windows.UnlockFileEx(windows.Handle(f.Fd()), 0, 1, 0, &uol)
			_ = f.Close()
		},
	}
	lk.bump = func() error {
		lk.gen++
		return writeLockFileGen(f, lk.gen)
	}
	lk.syncGen = func(minGen uint64) error {
		if minGen <= lk.gen {
			return nil
		}
		lk.gen = minGen
		return writeLockFileGen(f, lk.gen)
	}
	return lk, nil
}

// lockFileShared acquires a shared Windows file lock when lockPath exists, so
// readers wait for any in-progress writer to finish. When lockPath does not
// exist yet, it returns ok=false.
func lockFileShared(lockPath string) (func(), uint64, bool, error) {
	return lockFileSharedMode(lockPath, false)
}

func tryLockFileShared(lockPath string) (func(), uint64, bool, error) {
	return lockFileSharedMode(lockPath, true)
}

func lockFileSharedMode(lockPath string, nonBlocking bool) (func(), uint64, bool, error) {
	f, err := os.Open(lockPath)
	if errors.Is(err, os.ErrNotExist) {
		return func() {}, 0, false, nil
	}
	if err != nil {
		return nil, 0, false, err
	}
	var flags uint32
	if nonBlocking {
		flags = windows.LOCKFILE_FAIL_IMMEDIATELY
	}
	var ol windows.Overlapped
	if err := windows.LockFileEx(windows.Handle(f.Fd()), flags, 0, 1, 0, &ol); err != nil {
		f.Close()
		if nonBlocking && errors.Is(err, windows.ERROR_LOCK_VIOLATION) {
			return nil, 0, false, errLockBusy
		}
		return nil, 0, false, err
	}
	gen, err := readLockFileGen(f)
	if err != nil {
		var uol windows.Overlapped
		_ = windows.UnlockFileEx(windows.Handle(f.Fd()), 0, 1, 0, &uol)
		_ = f.Close()
		return nil, 0, false, err
	}
	return func() {
		var uol windows.Overlapped
		_ = windows.UnlockFileEx(windows.Handle(f.Fd()), 0, 1, 0, &uol)
		_ = f.Close()
	}, gen, true, nil
}

// lockOpenFileExclusive acquires an exclusive Windows file lock directly on
// open file f (following symlinks, junctions, and case-insensitive paths
// natively in the OS without creating a sibling lock file). It takes ownership
// of f: on error it closes f before returning; on success the returned
// function releases the lock and closes f.
func lockOpenFileExclusive(f *os.File) (func() error, error) {
	var ol windows.Overlapped
	if err := windows.LockFileEx(windows.Handle(f.Fd()), windows.LOCKFILE_EXCLUSIVE_LOCK, 0, 1, 0, &ol); err != nil {
		_ = f.Close()
		return nil, err
	}
	return func() error {
		var uol windows.Overlapped
		_ = windows.UnlockFileEx(windows.Handle(f.Fd()), 0, 1, 0, &uol)
		return f.Close()
	}, nil
}

// lockOpenFileShared acquires a shared Windows file lock directly on open file f.
// It takes ownership of f: on error it closes f before returning; on success
// the returned function releases the lock and closes f.
func lockOpenFileShared(f *os.File) (func() error, error) {
	var ol windows.Overlapped
	if err := windows.LockFileEx(windows.Handle(f.Fd()), 0, 0, 1, 0, &ol); err != nil {
		_ = f.Close()
		return nil, err
	}
	return func() error {
		var uol windows.Overlapped
		_ = windows.UnlockFileEx(windows.Handle(f.Fd()), 0, 1, 0, &uol)
		return f.Close()
	}, nil
}
