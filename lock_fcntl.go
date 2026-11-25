//go:build unix && !(darwin || dragonfly || freebsd || linux || netbsd || openbsd)

package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"syscall"

	"golang.org/x/sys/unix"
)

var (
	catalogDirLocks sync.Map
	fcntlCreateMu   sync.RWMutex
	fcntlActiveMu   sync.Mutex
	fcntlActiveCond = sync.NewCond(&fcntlActiveMu)
	fcntlHeldLocks  int
	fcntlCloseSeq   uint64
)

func leaveFcntlHeld() {
	fcntlActiveMu.Lock()
	fcntlHeldLocks--
	if fcntlHeldLocks == 0 {
		fcntlActiveCond.Broadcast()
	}
	fcntlActiveMu.Unlock()
}

// closeUnknownInode closes an open fd whose inode could not be determined via
// fstat. It waits only for currently held fcntl locks (never for goroutines
// blocked in F_SETLKW on unrelated inodes), closes synchronously when none are
// held, and increments fcntlCloseSeq so any concurrent F_SETLKW that completes
// across the close automatically re-acquires its lock.
func closeUnknownInode(f *os.File) {
	fcntlActiveMu.Lock()
	if fcntlHeldLocks == 0 {
		_ = f.Close()
		fcntlCloseSeq++
		fcntlActiveMu.Unlock()
		return
	}
	fcntlActiveMu.Unlock()
	go func() {
		fcntlActiveMu.Lock()
		for fcntlHeldLocks > 0 {
			fcntlActiveCond.Wait()
		}
		_ = f.Close()
		fcntlCloseSeq++
		fcntlActiveMu.Unlock()
	}()
}

func fstatRetryEINTR(fd int, st *unix.Stat_t) error {
	for {
		err := unix.Fstat(fd, st)
		if !errors.Is(err, syscall.EINTR) {
			return err
		}
	}
}

func fcntlAcquireFlock(fd uintptr, cmd int, lk *unix.Flock_t) error {
	for {
		fcntlActiveMu.Lock()
		seqBefore := fcntlCloseSeq
		fcntlActiveMu.Unlock()

		var err error
		for {
			err = unix.FcntlFlock(fd, cmd, lk)
			if !errors.Is(err, syscall.EINTR) {
				break
			}
		}
		if err != nil {
			return err
		}

		fcntlActiveMu.Lock()
		if fcntlCloseSeq == seqBefore {
			fcntlHeldLocks++
			fcntlActiveMu.Unlock()
			return nil
		}
		fcntlActiveMu.Unlock()
	}
}

func isReadOnlyOrPermError(err error) bool {
	return errors.Is(err, os.ErrPermission) || errors.Is(err, syscall.EROFS)
}

func pathFileID(path string, _ bool) (dev, ino, nlink uint64, ok bool) {
	var st unix.Stat_t
	if err := unix.Stat(path, &st); err != nil {
		return 0, 0, 0, false
	}
	return uint64(st.Dev), uint64(st.Ino), uint64(st.Nlink), true
}

func inodeMutex(dev, ino uint64) *sync.Mutex {
	key := fmt.Sprintf("inode:%d:%d", dev, ino)
	v, _ := catalogDirLocks.LoadOrStore(key, &sync.Mutex{})
	return v.(*sync.Mutex)
}

func acquireInodeMutex(mu *sync.Mutex, nonBlocking bool) bool {
	if nonBlocking {
		return mu.TryLock()
	}
	mu.Lock()
	return true
}

func closeUnderInodeMutexAsync(mu *sync.Mutex, f *os.File) {
	go func() {
		mu.Lock()
		_ = f.Close()
		mu.Unlock()
	}()
}

// openAndLockPathExclusive acquires the in-process inode mutex BEFORE opening
// lockPath so a failed non-blocking try-lock never opens and closes an fd on
// an inode locked by another goroutine (which POSIX fcntl would treat as
// releasing the process's existing lock on that inode).
func openAndLockPathExclusive(lockPath string, nonBlocking bool) (*os.File, func() error, error) {
	if err := os.MkdirAll(filepath.Dir(lockPath), 0o755); err != nil {
		return nil, nil, err
	}
	var mu *sync.Mutex
	var f *os.File
	for range 16 {
		var st unix.Stat_t
		fcntlCreateMu.RLock()
		statErr := unix.Stat(lockPath, &st)
		fcntlCreateMu.RUnlock()
		if statErr == nil {
			candMu := inodeMutex(uint64(st.Dev), uint64(st.Ino))
			if !acquireInodeMutex(candMu, nonBlocking) {
				return nil, nil, errLockBusy
			}
			candFile, openErr := os.OpenFile(lockPath, os.O_RDWR, 0o600)
			if errors.Is(openErr, os.ErrNotExist) {
				candMu.Unlock()
				continue
			}
			if openErr != nil {
				candMu.Unlock()
				return nil, nil, openErr
			}
			var fst unix.Stat_t
			if fstatErr := fstatRetryEINTR(int(candFile.Fd()), &fst); fstatErr != nil {
				candMu.Unlock()
				closeUnknownInode(candFile)
				return nil, nil, fstatErr
			}
			if uint64(fst.Dev) != uint64(st.Dev) || uint64(fst.Ino) != uint64(st.Ino) {
				candMu.Unlock()
				actualMu := inodeMutex(uint64(fst.Dev), uint64(fst.Ino))
				if !acquireInodeMutex(actualMu, nonBlocking) {
					closeUnderInodeMutexAsync(actualMu, candFile)
					return nil, nil, errLockBusy
				}
				candMu = actualMu
			}
			mu = candMu
			f = candFile
			break
		}
		if !errors.Is(statErr, os.ErrNotExist) {
			return nil, nil, statErr
		}
		fcntlCreateMu.Lock()
		candFile, createErr := os.OpenFile(lockPath, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0o600)
		if errors.Is(createErr, os.ErrExist) {
			fcntlCreateMu.Unlock()
			continue
		}
		if createErr != nil {
			fcntlCreateMu.Unlock()
			return nil, nil, createErr
		}
		var fst unix.Stat_t
		if fstatErr := fstatRetryEINTR(int(candFile.Fd()), &fst); fstatErr != nil {
			fcntlCreateMu.Unlock()
			closeUnknownInode(candFile)
			return nil, nil, fstatErr
		}
		candMu := inodeMutex(uint64(fst.Dev), uint64(fst.Ino))
		candMu.Lock()
		fcntlCreateMu.Unlock()
		mu = candMu
		f = candFile
		break
	}
	if f == nil {
		return nil, nil, errors.New("lock file changed repeatedly during open")
	}
	lk := unix.Flock_t{Type: unix.F_WRLCK, Whence: 0, Start: 0, Len: 0}
	cmd := unix.F_SETLKW
	if nonBlocking {
		cmd = unix.F_SETLK
	}
	if err := fcntlAcquireFlock(f.Fd(), cmd, &lk); err != nil {
		_ = f.Close()
		mu.Unlock()
		if nonBlocking && (errors.Is(err, syscall.EACCES) || errors.Is(err, syscall.EAGAIN)) {
			return nil, nil, errLockBusy
		}
		return nil, nil, err
	}
	return f, func() error {
		unlk := unix.Flock_t{Type: unix.F_UNLCK, Whence: 0, Start: 0, Len: 0}
		_ = unix.FcntlFlock(f.Fd(), unix.F_SETLK, &unlk)
		closeErr := f.Close()
		leaveFcntlHeld()
		mu.Unlock()
		return closeErr
	}, nil
}

// lockFileExclusive acquires both an in-process inode mutex and a POSIX fcntl
// write lock on lockPath for Unix platforms without BSD flock (such as
// Solaris/illumos).
func lockFileExclusive(lockPath string) (*catalogLock, error) {
	return lockFileExclusiveMode(lockPath, false)
}

func tryLockFileExclusive(lockPath string) (*catalogLock, error) {
	return lockFileExclusiveMode(lockPath, true)
}

func lockFileExclusiveMode(lockPath string, nonBlocking bool) (*catalogLock, error) {
	f, closeAndUnlock, err := openAndLockPathExclusive(lockPath, nonBlocking)
	if err != nil {
		return nil, err
	}
	fileInfo, err := f.Stat()
	if err != nil {
		_ = closeAndUnlock()
		return nil, err
	}
	gen, err := readLockFileGen(f)
	if err != nil {
		_ = closeAndUnlock()
		return nil, err
	}
	res := &catalogLock{
		gen:      gen,
		fileInfo: fileInfo,
		unlock: func() {
			_ = closeAndUnlock()
		},
	}
	res.bump = func() error {
		res.gen++
		return writeLockFileGen(f, res.gen)
	}
	res.syncGen = func(minGen uint64) error {
		if minGen <= res.gen {
			return nil
		}
		res.gen = minGen
		return writeLockFileGen(f, res.gen)
	}
	return res, nil
}

// lockFileShared acquires both an in-process inode mutex and a shared POSIX
// fcntl read lock when lockPath exists. When it does not exist yet, returns
// ok=false. Acquires the inode mutex BEFORE opening lockPath so a failed
// non-blocking try-lock never closes an fd on another goroutine's locked inode.
func lockFileShared(lockPath string) (func(), uint64, os.FileInfo, bool, error) {
	return lockFileSharedMode(lockPath, false)
}

func tryLockFileShared(lockPath string) (func(), uint64, os.FileInfo, bool, error) {
	return lockFileSharedMode(lockPath, true)
}

func lockFileSharedMode(lockPath string, nonBlocking bool) (func(), uint64, os.FileInfo, bool, error) {
	var st unix.Stat_t
	fcntlCreateMu.RLock()
	statErr := unix.Stat(lockPath, &st)
	fcntlCreateMu.RUnlock()
	if statErr != nil {
		if errors.Is(statErr, os.ErrNotExist) {
			return func() {}, 0, nil, false, nil
		}
		return nil, 0, nil, false, statErr
	}
	mu := inodeMutex(uint64(st.Dev), uint64(st.Ino))
	if !acquireInodeMutex(mu, nonBlocking) {
		return nil, 0, nil, false, errLockBusy
	}
	f, err := os.Open(lockPath)
	if errors.Is(err, os.ErrNotExist) {
		mu.Unlock()
		return func() {}, 0, nil, false, nil
	}
	if err != nil {
		mu.Unlock()
		return nil, 0, nil, false, err
	}
	var fst unix.Stat_t
	if fstatErr := fstatRetryEINTR(int(f.Fd()), &fst); fstatErr != nil {
		mu.Unlock()
		closeUnknownInode(f)
		return nil, 0, nil, false, fstatErr
	}
	if uint64(fst.Dev) != uint64(st.Dev) || uint64(fst.Ino) != uint64(st.Ino) {
		mu.Unlock()
		actualMu := inodeMutex(uint64(fst.Dev), uint64(fst.Ino))
		if !acquireInodeMutex(actualMu, nonBlocking) {
			closeUnderInodeMutexAsync(actualMu, f)
			return nil, 0, nil, false, errLockBusy
		}
		mu = actualMu
	}
	lk := unix.Flock_t{Type: unix.F_RDLCK, Whence: 0, Start: 0, Len: 0}
	cmd := unix.F_SETLKW
	if nonBlocking {
		cmd = unix.F_SETLK
	}
	if err := fcntlAcquireFlock(f.Fd(), cmd, &lk); err != nil {
		_ = f.Close()
		mu.Unlock()
		if nonBlocking && (errors.Is(err, syscall.EACCES) || errors.Is(err, syscall.EAGAIN)) {
			return nil, 0, nil, false, errLockBusy
		}
		return nil, 0, nil, false, err
	}
	closeAndUnlock := func() error {
		unlk := unix.Flock_t{Type: unix.F_UNLCK, Whence: 0, Start: 0, Len: 0}
		_ = unix.FcntlFlock(f.Fd(), unix.F_SETLK, &unlk)
		closeErr := f.Close()
		leaveFcntlHeld()
		mu.Unlock()
		return closeErr
	}
	fileInfo, err := f.Stat()
	if err != nil {
		_ = closeAndUnlock()
		return nil, 0, nil, false, err
	}
	gen, err := readLockFileGen(f)
	if err != nil {
		_ = closeAndUnlock()
		return nil, 0, nil, false, err
	}
	return func() {
		_ = closeAndUnlock()
	}, gen, fileInfo, true, nil
}

func openFileInodeMutex(f *os.File) (*sync.Mutex, error) {
	var st unix.Stat_t
	if err := fstatRetryEINTR(int(f.Fd()), &st); err != nil {
		return nil, err
	}
	return inodeMutex(uint64(st.Dev), uint64(st.Ino)), nil
}

// lockOpenFileExclusive acquires an in-process inode mutex and POSIX fcntl
// write lock directly on open file f. It takes ownership of f and always
// closes f BEFORE releasing the in-process mutex (on both success and error)
// so close(2) cannot drop another goroutine's process-owned fcntl lock.
func lockOpenFileExclusive(f *os.File) (func() error, error) {
	mu, err := openFileInodeMutex(f)
	if err != nil {
		closeUnknownInode(f)
		return nil, err
	}
	mu.Lock()
	lk := unix.Flock_t{Type: unix.F_WRLCK, Whence: 0, Start: 0, Len: 0}
	if err := fcntlAcquireFlock(f.Fd(), unix.F_SETLKW, &lk); err != nil {
		_ = f.Close()
		mu.Unlock()
		return nil, err
	}
	return func() error {
		unlk := unix.Flock_t{Type: unix.F_UNLCK, Whence: 0, Start: 0, Len: 0}
		_ = unix.FcntlFlock(f.Fd(), unix.F_SETLK, &unlk)
		err := f.Close()
		leaveFcntlHeld()
		mu.Unlock()
		return err
	}, nil
}

// lockOpenFileShared acquires an in-process inode mutex and POSIX fcntl read
// lock directly on open file f. It takes ownership of f and always closes f
// BEFORE releasing the in-process mutex (on both success and error).
func lockOpenFileShared(f *os.File) (func() error, error) {
	mu, err := openFileInodeMutex(f)
	if err != nil {
		closeUnknownInode(f)
		return nil, err
	}
	mu.Lock()
	lk := unix.Flock_t{Type: unix.F_RDLCK, Whence: 0, Start: 0, Len: 0}
	if err := fcntlAcquireFlock(f.Fd(), unix.F_SETLKW, &lk); err != nil {
		_ = f.Close()
		mu.Unlock()
		return nil, err
	}
	return func() error {
		unlk := unix.Flock_t{Type: unix.F_UNLCK, Whence: 0, Start: 0, Len: 0}
		_ = unix.FcntlFlock(f.Fd(), unix.F_SETLK, &unlk)
		err := f.Close()
		leaveFcntlHeld()
		mu.Unlock()
		return err
	}, nil
}
