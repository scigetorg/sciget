package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode"
)

// container is one row of the catalog: a single container file, which may
// back several catalog entries (a command-line entry and its graphical
// ones).
type container struct {
	tool          string // "fsl"
	version       string // "6.0.3", the software's own version
	image         string // "fsl_6.0.3"
	buildDate     string // "20200905"
	licences      []string
	dois          []string
	apptainerArgs []string // added to every run; all entries agree
	categories    []string
	graphical     bool     // some entry has a graphical command
	hidden        bool     // every entry is hidden
	catalog       string   // the name of the catalog it comes from
	mirrors       []string // the catalog's download URLs, with {file}
	shadowed      bool     // an earlier catalog in use has the same name
	entries       []entry  // visible entries with a command, in key order
	published     download // the image's size and SHA-256, if the catalog gives them
}

// entry is a visible catalog entry that runs a command in its container,
// such as a graphical program.
type entry struct {
	key   string // "fsleyesGUI-fsl 6.0.3"
	label string // "fsleyesGUI"
	exec  string // "fsleyes"
}

// name returns the name users type: "fsl/6.0.3".
func (c container) name() string { return c.tool + "/" + c.version }

// fileName returns the container's file name without extension:
// "fsl_6.0.3_20200905".
func (c container) fileName() string { return c.image + "_" + c.buildDate }

// catalogTool is one tool in apps.json, holding its entries by key.
type catalogTool struct {
	Categories []string              `json:"categories"`
	Apps       map[string]catalogApp `json:"apps"`
}

// catalogApp is one entry of apps.json. Its "version" field holds the
// container's build date, not the software version.
type catalogApp struct {
	ApptainerArgs []string `json:"apptainer_args"`
	Licences      []string `json:"license"`
	DOI           string   `json:"doi"`
	BuildDate     string   `json:"version"`
	Exec          string   `json:"exec"`
	Image         string   `json:"image"`
	ShowInApplist *bool    `json:"show_in_applist"`
	SHA256        string   `json:"sha256"`
	Size          int64    `json:"size"`
}

// readContainers returns the containers of every catalog in use, sorted by
// tool, then version, then catalog order. A container whose name an
// earlier catalog also has is marked shadowed.
func readContainers() ([]container, error) {
	cacheDir, used, err := usedCatalogs()
	if err != nil {
		return nil, err
	}
	var all []container
	seen := map[string]bool{}
	for _, cat := range used {
		containers, mirrors, err := readCatalogSnapshot(cat.dir(cacheDir))
		if err != nil {
			return nil, &catalogError{name: cat.Name, err: err}
		}
		for _, c := range containers {
			c.catalog = cat.Name
			c.mirrors = mirrors
			c.shadowed = seen[c.name()]
			seen[c.name()] = true
			all = append(all, c)
		}
	}
	// A stable sort keeps copies of a name in catalog order.
	slices.SortStableFunc(all, compareContainers)
	return all, nil
}

// catalogLock is an exclusive lock held on a catalog's external lock file.
// gen is the monotonic generation stored in the lock file when acquired;
// bump increments it in place before the lock is released so any delayed
// refresh that started against an older generation detects the change.
type catalogLock struct {
	gen     uint64
	bump    func() error
	syncGen func(uint64) error
	unlock  func()
}

func readLockFileGen(f *os.File) (uint64, error) {
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return 0, err
	}
	data, err := io.ReadAll(f)
	if err != nil {
		return 0, err
	}
	s := strings.TrimSpace(string(data))
	if s == "" {
		return 0, nil
	}
	gen, err := strconv.ParseUint(s, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("invalid catalog lock generation: %w", err)
	}
	return gen, nil
}

// writeLockFileGen overwrites the lock file's fixed-width generation record
// in place at offset 0 without truncating the file to zero, so an interrupted
// write can never reset the stored generation back to 0.
func writeLockFileGen(f *os.File, gen uint64) error {
	buf := fmt.Sprintf("%020d\n", gen)
	n, err := f.WriteAt([]byte(buf), 0)
	if err != nil {
		return err
	}
	if n != len(buf) {
		return io.ErrShortWrite
	}
	return nil
}

// catalogDirRegistered reports whether any catalog currently registered in the
// user or system catalog lists maps to cache directory dir.
func catalogDirRegistered(dir string) (bool, error) {
	layers, err := readCatalogLayers()
	if err != nil {
		return false, err
	}
	clean := filepath.Clean(dir)
	for _, c := range layers.combined() {
		if filepath.Clean(c.dir(layers.cacheDir)) == clean {
			return true, nil
		}
	}
	return false, nil
}

func readCatalogLockGen(dir string) (uint64, error) {
	unlock, gen, locked, err := lockCatalogDirShared(dir)
	if err != nil {
		return 0, err
	}
	if locked {
		unlock()
	}
	return gen, nil
}

func endsWithDirSyntax(p string) bool {
	s := filepath.ToSlash(p)
	return s == "." || s == ".." || strings.HasSuffix(s, "/") || strings.HasSuffix(s, "/.") || strings.HasSuffix(s, "/..")
}

// resolveCanonicalPath resolves symlinks and Windows NTFS junctions component
// by component, including dangling leaves and missing tail components. It
// refuses to collapse ".." through a non-existent directory (returning
// ok=false), matching OS pathname lookup. When forFile is true, it also
// rejects paths or leaf symlink targets with directory-only syntax (trailing
// "/", "/.", or "/..") so callers never create a regular file for a target
// that OS open() requires to be a directory.
func resolveCanonicalPath(path string, forFile bool) (string, bool) {
	if forFile && endsWithDirSyntax(path) {
		return "", false
	}
	cur := path
	if !filepath.IsAbs(cur) {
		if abs, err := filepath.Abs(cur); err == nil {
			cur = abs
		}
	}
	vol := filepath.VolumeName(cur)
	rest := cur[len(vol):]
	resolved := vol + string(filepath.Separator)
	var queue []string
	for _, part := range strings.Split(filepath.ToSlash(rest), "/") {
		if part != "" {
			queue = append(queue, part)
		}
	}
	symlinksFollowed := 0
	for len(queue) > 0 {
		comp := queue[0]
		queue = queue[1:]
		if comp == "." {
			if forFile && len(queue) == 0 {
				return "", false
			}
			continue
		}
		if comp == ".." {
			if forFile && len(queue) == 0 {
				return "", false
			}
			info, err := os.Stat(resolved)
			if err != nil || !info.IsDir() {
				return "", false
			}
			resolved = filepath.Dir(resolved)
			continue
		}
		next := filepath.Join(resolved, comp)
		// Check os.Readlink BEFORE filepath.EvalSymlinks because Go 1.23+
		// intentionally leaves Windows NTFS directory junctions unresolved in
		// filepath.EvalSymlinks while os.Readlink returns their target path.
		if target, err := os.Readlink(next); err == nil {
			if forFile && len(queue) == 0 && endsWithDirSyntax(target) {
				return "", false
			}
			symlinksFollowed++
			if symlinksFollowed > 40 {
				return "", false
			}
			if filepath.IsAbs(target) {
				tVol := filepath.VolumeName(target)
				resolved = tVol + string(filepath.Separator)
				target = target[len(tVol):]
			} else if runtime.GOOS == "windows" && (strings.HasPrefix(target, `\`) || strings.HasPrefix(target, "/")) {
				resolved = filepath.VolumeName(resolved) + string(filepath.Separator)
			}
			var targetParts []string
			for _, part := range strings.Split(filepath.ToSlash(target), "/") {
				if part != "" {
					targetParts = append(targetParts, part)
				}
			}
			queue = append(targetParts, queue...)
			continue
		}
		// For existing non-link components, filepath.EvalSymlinks normalizes
		// on-disk casing (e.g. on Windows).
		if eval, err := filepath.EvalSymlinks(next); err == nil {
			resolved = eval
			continue
		}
		resolved = next
	}
	return resolved, true
}

func canonicalCatalogDir(dir string) string {
	clean, ok := resolveCanonicalPath(dir, false)
	if !ok {
		clean = filepath.Clean(dir)
	}
	return clean
}

func hashLockPath(parent, base string) string {
	sum := sha256.Sum256([]byte(strings.ToLower(base)))
	return filepath.Join(parent, ".sciget-lock-"+hex.EncodeToString(sum[:16])+".lock")
}

// catalogLockPath returns the advisory slot lock file path for cache directory
// dir. It resolves symlinks and Windows junctions along dir's parent so two
// cache roots sharing a catalogs/ subtree serialize on the same lock file while
// keeping the catalog slot basename stable even if dir itself is a symlink
// that gets removed and recreated. Placing the lock beside dir (outside dir,
// inside its resolved parent) preserves lock identity across removals and
// avoids needing write access outside the resolved catalogs tree, and hashing
// the lowercased basename keeps the filename well under 255-byte limits.
func catalogLockPath(dir string) string {
	cleaned := filepath.Clean(dir)
	parent, ok := resolveCanonicalPath(filepath.Dir(cleaned), false)
	if !ok {
		parent = filepath.Dir(cleaned)
	}
	return hashLockPath(parent, filepath.Base(cleaned))
}

// catalogExternalLockPaths returns the unique external lock file paths for dir
// in deterministic lexicographical order. Normally this is a single path; when
// dir itself is a symlink to a different directory, it includes both the slot
// lock in dir's resolved parent and the target lock in the canonical target's
// parent so accesses via either path serialize and removing the symlink keeps
// the slot's generation.
func catalogExternalLockPaths(dir string) []string {
	slot := catalogLockPath(dir)
	clean := canonicalCatalogDir(dir)
	target := hashLockPath(filepath.Dir(clean), filepath.Base(clean))
	if target == slot {
		return []string{slot}
	}
	if target < slot {
		return []string{target, slot}
	}
	return []string{slot, target}
}

// isRefreshTempFile reports whether name is a backup or staged temporary file
// created by refreshCatalogAt (<catalog-file>.<digits>.{bak,incomplete}).
func isRefreshTempFile(name string) bool {
	for _, prefix := range []string{"apps.json.", "eessi.json.", "catalog.json."} {
		rest, ok := strings.CutPrefix(name, prefix)
		if !ok {
			continue
		}
		var mid string
		if m, ok := strings.CutSuffix(rest, ".bak"); ok {
			mid = m
		} else if m, ok := strings.CutSuffix(rest, ".incomplete"); ok {
			mid = m
		} else {
			continue
		}
		if mid == "" {
			return false
		}
		for _, r := range mid {
			if r < '0' || r > '9' {
				return false
			}
		}
		return true
	}
	return false
}

// removeRetiredInDirLock removes a retired in-dir .lock file on Windows,
// retrying briefly so any reader that opened .lock just before extPath
// appeared finishes closing its handle before os.RemoveAll(dir) runs.
func removeRetiredInDirLock(path string) error {
	var err error
	for i := range 50 {
		err = os.Remove(path)
		if err == nil || errors.Is(err, os.ErrNotExist) {
			return nil
		}
		if i < 49 {
			time.Sleep(10 * time.Millisecond)
		}
	}
	return err
}

func catalogLockExists(dir string) (bool, error) {
	for _, extPath := range catalogExternalLockPaths(dir) {
		if _, err := os.Stat(extPath); err == nil {
			return true, nil
		} else if !errors.Is(err, os.ErrNotExist) {
			return false, err
		}
	}
	clean := canonicalCatalogDir(dir)
	if info, err := os.Stat(clean); err == nil && info.IsDir() {
		if _, err := os.Stat(filepath.Join(clean, ".lock")); err == nil {
			return true, nil
		} else if !errors.Is(err, os.ErrNotExist) {
			return false, err
		}
	}
	return false, nil
}

func anyPathExists(paths []string) (bool, error) {
	for _, p := range paths {
		if _, err := os.Stat(p); err == nil {
			return true, nil
		} else if !errors.Is(err, os.ErrNotExist) {
			return false, err
		}
	}
	return false, nil
}

func lockCatalogDirWithMode(dir string, allowInDirFallback bool) (*catalogLock, error) {
	clean := canonicalCatalogDir(dir)
	slotPath := catalogLockPath(dir)
	extPaths := catalogExternalLockPaths(dir)
	inDirPath := filepath.Join(clean, ".lock")
	isSymlinkLeaf := false
	if info, err := os.Lstat(filepath.Clean(dir)); err == nil && info.Mode()&os.ModeSymlink != 0 {
		isSymlinkLeaf = true
	}

	var extLocks []*catalogLock
	var skippedExt []string
	unlockAll := func() {
		for i := len(extLocks) - 1; i >= 0; i-- {
			extLocks[i].unlock()
		}
	}
	needInDirFallback := false
	for _, p := range extPaths {
		lk, err := lockFileExclusive(p)
		if err != nil {
			if errors.Is(err, os.ErrPermission) {
				// Removing a leaf symlink only unlinks the symlink in slot's
				// parent; if the target's parent is read-only we still hold
				// slotPath and never touch the target directory.
				if !allowInDirFallback && isSymlinkLeaf && p != slotPath {
					continue
				}
				if allowInDirFallback {
					if _, extStatErr := os.Stat(p); errors.Is(extStatErr, os.ErrNotExist) {
						if info, dirStatErr := os.Stat(clean); dirStatErr == nil && info.IsDir() {
							needInDirFallback = true
							skippedExt = append(skippedExt, p)
							continue
						}
					}
				}
			}
			unlockAll()
			return nil, err
		}
		extLocks = append(extLocks, lk)
	}

	if needInDirFallback {
		inLk, inErr := lockFileExclusive(inDirPath)
		if inErr != nil {
			unlockAll()
			return nil, inErr
		}
		// If any skipped external lock path appeared concurrently, release
		// inLk and re-acquire cleanly under the external locks.
		if exists, statErr := anyPathExists(skippedExt); statErr != nil {
			inLk.unlock()
			unlockAll()
			return nil, statErr
		} else if exists {
			inLk.unlock()
			unlockAll()
			return lockCatalogDirWithMode(dir, false)
		}
		maxGen := inLk.gen
		for _, lk := range extLocks {
			if lk.gen > maxGen {
				maxGen = lk.gen
			}
		}
		for _, lk := range extLocks {
			if err := lk.syncGen(maxGen); err != nil {
				inLk.unlock()
				unlockAll()
				return nil, err
			}
		}
		if err := inLk.syncGen(maxGen); err != nil {
			inLk.unlock()
			unlockAll()
			return nil, err
		}
		combined := &catalogLock{
			gen: maxGen,
			unlock: func() {
				inLk.unlock()
				unlockAll()
			},
		}
		combined.bump = func() error {
			combined.gen++
			for _, lk := range extLocks {
				if err := lk.syncGen(combined.gen); err != nil {
					return err
				}
			}
			return inLk.syncGen(combined.gen)
		}
		combined.syncGen = func(minGen uint64) error {
			if minGen <= combined.gen {
				return nil
			}
			combined.gen = minGen
			for _, lk := range extLocks {
				if err := lk.syncGen(minGen); err != nil {
					return err
				}
			}
			return inLk.syncGen(minGen)
		}
		return combined, nil
	}

	var maxGen uint64
	for _, lk := range extLocks {
		if lk.gen > maxGen {
			maxGen = lk.gen
		}
	}
	var retiredInLk *catalogLock
	// If an earlier refresh fell back to the in-dir lock while the parent
	// directory was read-only, drain and retire it under the external locks so
	// any concurrent holders finish and monotonic generation is preserved.
	// Keep inLk held until extLocks are released so any reader that opened
	// .lock before it was unlinked still blocks until this operation completes.
	if info, statErr := os.Stat(clean); statErr == nil && info.IsDir() {
		if _, inStatErr := os.Stat(inDirPath); inStatErr == nil {
			inLk, inErr := lockFileExclusive(inDirPath)
			if inErr != nil {
				unlockAll()
				return nil, inErr
			}
			if inLk.gen > maxGen {
				maxGen = inLk.gen
			}
			for _, lk := range extLocks {
				if syncErr := lk.syncGen(maxGen); syncErr != nil {
					inLk.unlock()
					unlockAll()
					return nil, syncErr
				}
			}
			if runtime.GOOS != "windows" {
				if rmErr := os.Remove(inDirPath); rmErr != nil && !errors.Is(rmErr, os.ErrNotExist) {
					inLk.unlock()
					unlockAll()
					return nil, rmErr
				}
				retiredInLk = inLk
			} else if !allowInDirFallback {
				inLk.unlock()
				if rmErr := removeRetiredInDirLock(inDirPath); rmErr != nil {
					unlockAll()
					return nil, rmErr
				}
			} else {
				retiredInLk = inLk
			}
		}
	}
	for _, lk := range extLocks {
		if syncErr := lk.syncGen(maxGen); syncErr != nil {
			if retiredInLk != nil {
				retiredInLk.unlock()
			}
			unlockAll()
			return nil, syncErr
		}
	}
	combined := &catalogLock{
		gen: maxGen,
		unlock: func() {
			if retiredInLk != nil {
				retiredInLk.unlock()
				if runtime.GOOS == "windows" {
					_ = removeRetiredInDirLock(inDirPath)
				}
			}
			unlockAll()
		},
	}
	combined.bump = func() error {
		combined.gen++
		for _, lk := range extLocks {
			if err := lk.syncGen(combined.gen); err != nil {
				return err
			}
		}
		return nil
	}
	combined.syncGen = func(minGen uint64) error {
		if minGen <= combined.gen {
			return nil
		}
		combined.gen = minGen
		for _, lk := range extLocks {
			if err := lk.syncGen(minGen); err != nil {
				return err
			}
		}
		return nil
	}
	return combined, nil
}

func lockCatalogDir(dir string) (*catalogLock, error) {
	return lockCatalogDirWithMode(dir, true)
}

func lockCatalogDirForRemove(dir string) (*catalogLock, error) {
	return lockCatalogDirWithMode(dir, false)
}

// beforeInDirSharedLock runs after lockCatalogDirShared finds no external lock
// and before it opens the fallback in-dir lock; tests may set it to reproduce
// migration races.
var beforeInDirSharedLock = func(dir string) {}

func lockCatalogDirShared(dir string) (func(), uint64, bool, error) {
	extPaths := catalogExternalLockPaths(dir)
	clean := canonicalCatalogDir(dir)
	for range 4 {
		var unlocks []func()
		var missing []string
		var maxGen uint64
		release := func() {
			for i := len(unlocks) - 1; i >= 0; i-- {
				unlocks[i]()
			}
		}
		for _, p := range extPaths {
			u, g, ok, err := lockFileShared(p)
			if err != nil {
				release()
				return nil, 0, false, err
			}
			if ok {
				unlocks = append(unlocks, u)
				if g > maxGen {
					maxGen = g
				}
			} else {
				missing = append(missing, p)
			}
		}
		if len(unlocks) > 0 {
			// If a fallback in-dir lock still exists (e.g. an external lock
			// was just created before a migrating writer acquired it while an
			// earlier writer still holds .lock), also lock .lock in ext -> inDir
			// order so we wait for any active .lock writer.
			if info, statErr := os.Stat(clean); statErr == nil && info.IsDir() {
				inUnlock, inGen, inLocked, inErr := lockFileShared(filepath.Join(clean, ".lock"))
				if inErr != nil {
					release()
					return nil, 0, false, inErr
				}
				if inLocked {
					unlocks = append(unlocks, inUnlock)
					if inGen > maxGen {
						maxGen = inGen
					}
				}
			}
			if len(missing) > 0 {
				if appeared, statErr := anyPathExists(missing); statErr != nil {
					release()
					return nil, 0, false, statErr
				} else if appeared {
					release()
					continue
				}
			}
			return release, maxGen, true, nil
		}
		beforeInDirSharedLock(dir)
		info, statErr := os.Stat(clean)
		if statErr != nil || !info.IsDir() {
			return func() {}, 0, false, nil
		}
		inUnlock, inGen, inLocked, inErr := lockFileShared(filepath.Join(clean, ".lock"))
		if inErr != nil {
			return nil, 0, false, inErr
		}
		if !inLocked {
			if exists, extErr := anyPathExists(extPaths); extErr != nil {
				return nil, 0, false, extErr
			} else if exists {
				continue
			}
			return func() {}, 0, false, nil
		}
		// Verify no external lock appeared before we acquired the in-dir lock
		// (e.g. if our open of .lock raced with a migration that unlinked it).
		if exists, extErr := anyPathExists(extPaths); extErr != nil {
			inUnlock()
			return nil, 0, false, extErr
		} else if exists {
			inUnlock()
			continue
		}
		return inUnlock, inGen, true, nil
	}
	return lockFileShared(catalogLockPath(dir))
}

// beforeCatalogSharedLock runs immediately before readCatalogSnapshot acquires
// the shared lock; tests may set it to synchronize concurrent readers with an
// in-progress refresh.
var beforeCatalogSharedLock = func(dir string) {}

// checkCatalogClean returns an error if dir has leftover refresh backup or
// staged files from an incomplete rollback.
func checkCatalogClean(dir string) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if isRefreshTempFile(entry.Name()) {
			return errors.New("catalog cache is incomplete after a failed refresh; run 'sciget refresh' to recover")
		}
	}
	return nil
}

func readCatalogSnapshotLocked(dir string) ([]container, []string, error) {
	unlock, _, locked, err := lockCatalogDirShared(dir)
	if err != nil {
		return nil, nil, err
	}
	if !locked {
		return nil, nil, &notDownloadedError{path: filepath.Join(dir, "apps.json")}
	}
	defer unlock()
	if err := checkCatalogClean(dir); err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, nil, err
	}
	containers, err := loadCatalog(filepath.Join(dir, "apps.json"))
	if err != nil {
		return nil, nil, err
	}
	mirrors, err := loadMirrors(filepath.Join(dir, "catalog.json"))
	if err != nil {
		return nil, nil, err
	}
	return containers, mirrors, nil
}

// readCatalogSnapshot reads apps.json and catalog.json consistently. When dir
// has a catalog lock file, it holds the shared lock across both reads so an
// in-progress refresh commit, rollback, or removal is never observed. Older
// caches without a lock file are still read unlocked, then re-checked for a
// concurrently created lock so a first refresh cannot slip uncommitted files
// into the read.
func readCatalogSnapshot(dir string) ([]container, []string, error) {
	beforeCatalogSharedLock(dir)
	unlock, _, locked, err := lockCatalogDirShared(dir)
	if err != nil {
		return nil, nil, err
	}
	if locked {
		defer unlock()
		if err := checkCatalogClean(dir); err != nil && !errors.Is(err, os.ErrNotExist) {
			return nil, nil, err
		}
		containers, err := loadCatalog(filepath.Join(dir, "apps.json"))
		if err != nil {
			return nil, nil, err
		}
		mirrors, err := loadMirrors(filepath.Join(dir, "catalog.json"))
		if err != nil {
			return nil, nil, err
		}
		return containers, mirrors, nil
	}
	if err := checkCatalogClean(dir); err != nil && !errors.Is(err, os.ErrNotExist) {
		if exists, statErr := catalogLockExists(dir); statErr == nil && exists {
			return readCatalogSnapshotLocked(dir)
		}
		return nil, nil, err
	}
	containers, err := loadCatalog(filepath.Join(dir, "apps.json"))
	if err != nil {
		if exists, statErr := catalogLockExists(dir); statErr == nil && exists {
			return readCatalogSnapshotLocked(dir)
		}
		return nil, nil, err
	}
	mirrors, err := loadMirrors(filepath.Join(dir, "catalog.json"))
	if err != nil {
		if exists, statErr := catalogLockExists(dir); statErr == nil && exists {
			return readCatalogSnapshotLocked(dir)
		}
		return nil, nil, err
	}
	if exists, statErr := catalogLockExists(dir); statErr != nil {
		return nil, nil, statErr
	} else if exists {
		return readCatalogSnapshotLocked(dir)
	}
	return containers, mirrors, nil
}

// loadMirrors returns the download URLs in the catalog JSON at path, or
// none when there is no such file, so a catalog last refreshed by an older
// sciget still lists its containers.
func loadMirrors(path string) ([]string, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("reading catalog: %w", err)
	}
	c, err := parseCatalogJSON(data)
	if err != nil {
		return nil, fmt.Errorf("reading catalog %s: %w", path, err)
	}
	return c.DownloadURLs, nil
}

// catalogError is why the catalog in use called name could not be read.
// Its text is err's alone; the TUI uses the name.
type catalogError struct {
	name string
	err  error
}

func (e *catalogError) Error() string { return e.err.Error() }
func (e *catalogError) Unwrap() error { return e.err }

// notDownloadedError is a catalog in use whose apps.json, at path, does
// not exist yet.
type notDownloadedError struct{ path string }

func (e *notDownloadedError) Error() string {
	return fmt.Sprintf("no catalog at %s; run 'sciget refresh'", e.path)
}

// loadCatalog reads apps.json at path and returns its containers, sorted by
// tool and then by version.
func loadCatalog(path string) ([]container, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, &notDownloadedError{path: path}
	}
	if err != nil {
		return nil, fmt.Errorf("reading catalog: %w", err)
	}
	containers, err := parseCatalog(data)
	if err != nil {
		return nil, fmt.Errorf("reading catalog %s: %w", path, err)
	}
	return containers, nil
}

// valueOptions are the apptainer_args options whose next word is their
// value. Add one when a catalog needs it.
var valueOptions = []string{"--bind", "-B"}

// parseCatalog returns the containers in data, the JSON of apps.json,
// sorted by tool and then by version.
func parseCatalog(data []byte) ([]container, error) {
	var tools map[string]catalogTool
	if err := json.Unmarshal(data, &tools); err != nil {
		return nil, err
	}

	byName := map[string]*container{}
	for tool, t := range tools {
		for key, app := range t.Apps {
			// Every string kept is checked here, once, so no output path
			// can print a terminal escape from a catalog.
			texts := slices.Concat([]string{tool, key, app.Image, app.BuildDate, app.DOI, app.Exec},
				t.Categories, app.Licences, app.ApptainerArgs)
			if slices.ContainsFunc(texts, func(s string) bool { return printable(s) != s }) {
				return nil, fmt.Errorf("catalog entry %q of %q has a control character", key, tool)
			}
			if strings.TrimSpace(app.Exec) == "" {
				app.Exec = ""
			}
			// Keys are "<label> <version>", except that entries with an
			// image may end in another word ("amico 2.1.0 arm64").
			image := app.Image
			if image == "" {
				i := strings.LastIndex(key, " ")
				if i < 0 {
					return nil, fmt.Errorf("catalog entry %q has no version", key)
				}
				image = tool + "_" + key[i+1:]
			}
			toolVersion, ok := strings.CutPrefix(image, tool+"_")
			switch {
			case !ok:
				return nil, fmt.Errorf("catalog entry %q has image %q, which does not start with %q",
					key, image, tool+"_")
			case toolVersion == "":
				return nil, fmt.Errorf("catalog entry %q has no version", key)
			case app.BuildDate == "":
				return nil, fmt.Errorf("catalog entry %q has no build date", key)
			case unsafeLayoutValue(tool) || unsafeLayoutValue(toolVersion) || unsafeLayoutValue(app.BuildDate):
				return nil, fmt.Errorf("catalog entry %q has a tool, version or build date with \"/\" or \"..\", or that is \".\"", key)
			case app.SHA256 != "" && !sha256Hex.MatchString(app.SHA256):
				return nil, fmt.Errorf("catalog entry %q has a sha256 that is not 64 lowercase hex digits", key)
			case app.Size < 0:
				return nil, fmt.Errorf("catalog entry %q has a negative size", key)
			}
			published := download{size: app.Size, sha256: app.SHA256}

			// The arguments go before the image, so a stray word would be
			// read as the image: each is an option, and an option in
			// valueOptions is followed by exactly one value.
			args := app.ApptainerArgs
			for i := 0; i < len(args); i++ {
				if len(args[i]) < 2 || args[i][0] != '-' || args[i] == "--" {
					return nil, fmt.Errorf("catalog entry %q has apptainer_args element %q, which is not an option or its value", key, args[i])
				}
				if slices.Contains(valueOptions, args[i]) {
					i++
					if i == len(args) || args[i] == "" || args[i][0] == '-' {
						return nil, fmt.Errorf("catalog entry %q has apptainer_args option %q without a value", key, args[i-1])
					}
				}
			}

			c := byName[tool+"/"+toolVersion]
			if c == nil {
				c = &container{tool: tool, version: toolVersion, image: image,
					buildDate: app.BuildDate, categories: t.Categories, hidden: true,
					apptainerArgs: app.ApptainerArgs, published: published}
				byName[c.name()] = c
			}
			if app.BuildDate != c.buildDate {
				return nil, fmt.Errorf("catalog lists %s with build dates %s and %s",
					c.name(), c.buildDate, app.BuildDate)
			}
			if !slices.Equal(app.ApptainerArgs, c.apptainerArgs) {
				return nil, fmt.Errorf("catalog lists %s with apptainer_args %q and %q",
					c.name(), c.apptainerArgs, app.ApptainerArgs)
			}
			if published != c.published {
				return nil, fmt.Errorf("catalog lists %s with different sizes or SHA-256s", c.name())
			}
			c.licences = append(c.licences, app.Licences...)
			if app.DOI != "" {
				c.dois = append(c.dois, app.DOI)
			}
			if app.Exec != "" {
				c.graphical = true
			}
			if app.ShowInApplist == nil || *app.ShowInApplist {
				c.hidden = false
				if app.Exec != "" {
					label, _, _ := strings.Cut(key, "-")
					c.entries = append(c.entries, entry{key: key, label: label, exec: app.Exec})
				}
			}
		}
	}
	containers := make([]container, 0, len(byName))
	for _, c := range byName {
		c.licences = slices.Compact(slices.Sorted(slices.Values(c.licences)))
		c.dois = slices.Compact(slices.Sorted(slices.Values(c.dois)))
		slices.SortFunc(c.entries, func(a, b entry) int { return strings.Compare(a.key, b.key) })
		containers = append(containers, *c)
	}
	slices.SortFunc(containers, compareContainers)
	return containers, nil
}

// sha256Hex matches a SHA-256 written as lowercase hex.
var sha256Hex = regexp.MustCompile(`^[0-9a-f]{64}$`)

// compareContainers orders containers by tool, then by version.
func compareContainers(a, b container) int {
	if n := strings.Compare(a.tool, b.tool); n != 0 {
		return n
	}
	return compareVersions(a.version, b.version)
}

// compareVersions compares two versions, treating runs of digits as numbers so
// that "6.0.9" sorts before "6.0.10". It returns -1, 0 or +1.
func compareVersions(a, b string) int {
	for a != "" && b != "" {
		var x, y string
		x, a = versionChunk(a)
		y, b = versionChunk(b)
		if n := compareChunks(x, y); n != 0 {
			return n
		}
	}
	switch {
	case a == b:
		return 0
	case a == "":
		return -1
	default:
		return 1
	}
}

// versionChunk splits s after its leading run of digits or of non-digits.
func versionChunk(s string) (chunk, rest string) {
	digit := isDigit(s[0])
	i := 1
	for i < len(s) && isDigit(s[i]) == digit {
		i++
	}
	return s[:i], s[i:]
}

// compareChunks compares two chunks, as numbers if both are digits.
func compareChunks(x, y string) int {
	if isDigit(x[0]) && isDigit(y[0]) {
		x, y = strings.TrimLeft(x, "0"), strings.TrimLeft(y, "0")
		if len(x) != len(y) {
			if len(x) < len(y) {
				return -1
			}
			return 1
		}
	}
	return strings.Compare(x, y)
}

func isDigit(b byte) bool { return '0' <= b && b <= '9' }

// lookupContainer returns the winning container with this exact name.
func lookupContainer(containers []container, name string) (container, error) {
	for _, c := range containers {
		if c.name() == name && !c.shadowed {
			return c, nil
		}
	}
	tool, _, _ := strings.Cut(name, "/")
	return container{}, fmt.Errorf("no container named %q; run 'sciget find %s' to list versions, or 'sciget refresh' to update the catalogs", name, tool)
}

// printable returns s fit to draw: whole terminal escape sequences, control
// characters (C0, DEL, C1) and Unicode format characters (such as
// bidirectional overrides and zero-width characters) are removed, so text
// from outside sciget can neither inject codes nor reorder or hide text.
// Use it only for display; launching and lookups use the raw value.
func printable(s string) string {
	s = escapeSequence.ReplaceAllString(s, "")
	return strings.Map(func(r rune) rune {
		if r < 0x20 || r >= 0x7f && r < 0xa0 || unicode.Is(unicode.Cf, r) {
			return -1
		}
		return r
	}, s)
}

// escapeSequence matches ANSI escape sequences: CSI ("ESC [ ... final"),
// OSC ("ESC ] ... BEL or ESC \\") and two-character escapes.
var escapeSequence = regexp.MustCompile(`\x1b(\[[0-?]*[ -/]*[@-~]|\][^\x07\x1b]*(\x07|\x1b\\)|[@-Z\\-_])`)
