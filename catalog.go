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

	"golang.org/x/text/cases"
	"golang.org/x/text/unicode/norm"
)

var (
	errLockBusy    = errors.New("lock busy")
	errRootChanged = errors.New("catalog root directory changed")
)

// container is one row of the catalog: a single container file, which may
// back several catalog entries (a command-line entry and its graphical
// ones).
type container struct {
	tool          string // "fsl"
	version       string // "6.0.3", the software's own version
	image         string // "fsl_6.0.3"
	buildDate     string // "20200905"
	arch          string // normalized CPU architecture (e.g. "x86_64", "aarch64"), or "" if unspecified
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

// hostArch returns the normalized CPU architecture of this machine.
// Tests replace it to exercise filtering across architectures.
var hostArch = func() string { return normalizeArch(runtime.GOARCH) }

var (
	arm64Token = regexp.MustCompile(`(?i)(?:^|[._-])(arm64|aarch64)(?:$|[._-])`)
	x86Token   = regexp.MustCompile(`(?i)(?:^|[._-])(x86[_-]64|amd64)(?:$|[._-])`)
)

// normalizeArch maps common CPU architecture names to canonical Linux names
// ("x86_64", "aarch64", etc.) so catalog entries and runtime.GOARCH match.
// Architecture-independent values ("all", "any", "noarch") normalize to "any".
func normalizeArch(s string) string {
	switch a := strings.ToLower(strings.TrimSpace(s)); a {
	case "":
		return ""
	case "all", "any", "noarch":
		return "any"
	case "amd64", "x86_64", "x86-64", "x64":
		return "x86_64"
	case "arm64", "aarch64":
		return "aarch64"
	default:
		return a
	}
}

// inferArch extracts a normalized architecture from a tool or version name
// when it carries a delimited architecture token (such as "fsl_arm64" or
// "2.1.0_arm64"). Ambiguous or untagged names return "".
func inferArch(tool, version string) string {
	hasArm := arm64Token.MatchString(tool) || arm64Token.MatchString(version)
	hasX86 := x86Token.MatchString(tool) || x86Token.MatchString(version)
	switch {
	case hasArm && !hasX86:
		return "aarch64"
	case hasX86 && !hasArm:
		return "x86_64"
	default:
		return ""
	}
}

// containerArch returns a container's normalized architecture, preferring an
// explicit catalog field and falling back to tool/version naming conventions.
func containerArch(declared, tool, version string) string {
	if strings.TrimSpace(declared) != "" {
		return normalizeArch(declared)
	}
	return inferArch(tool, version)
}

// effectiveArch returns c's normalized architecture, inferring it from tool
// or version if c.arch was not set explicitly.
func (c container) effectiveArch() string {
	if c.arch != "" {
		return normalizeArch(c.arch)
	}
	return inferArch(c.tool, c.version)
}

// compatibleWithHost reports whether c can run on this machine's CPU
// architecture. Containers without a known architecture match every host.
func (c container) compatibleWithHost() bool {
	arch := c.effectiveArch()
	return arch == "" || arch == "any" || arch == hostArch()
}

// catalogTool is one tool in apps.json, holding its entries by key.
type catalogTool struct {
	Categories []string              `json:"categories"`
	Apps       map[string]catalogApp `json:"apps"`
}

// catalogApp is one entry of apps.json. Its "version" field holds the
// container's build date, not the software version.
type catalogApp struct {
	ApptainerArgs []string `json:"apptainer_args"`
	Architecture  string   `json:"architecture"`
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
	gen      uint64
	fileInfo os.FileInfo
	bump     func() error
	syncGen  func(uint64) error
	unlock   func()
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

// catalogGen captures the independent monotonic generations of a catalog's
// slot lock (in dir's resolved parent) and target lock (in the canonical
// target's parent, or its in-dir .lock fallback). Comparing both counters
// ensures an update through either an alias or the direct target is always
// detected even when one counter is already higher than the other.
type catalogGen struct {
	slot   uint64
	target uint64
}

// catalogDirLock is an exclusive lock held across a catalog's slot and target
// lock files. dir is the canonical target directory verified while holding the
// locks, slotDir is the canonical parent joined with the slot leaf name, and
// linkLeaf records whether slotDir itself was a symlink or NTFS junction.
// slotParent, targetParent, and targetRoot are *os.Root directory handles
// verified against the locked lock-file inodes so post-lock I/O and removals
// never re-traverse parent paths.
type catalogDirLock struct {
	dir          string
	slotDir      string
	linkLeaf     bool
	slotParent   *os.Root
	slotBase     string
	targetParent *os.Root
	targetBase   string
	targetRoot   *os.Root
	gen          catalogGen
	bump         func() error
	unlock       func()
}

type sharedCatalogLock struct {
	unlock   func()
	gen      catalogGen
	locked   bool
	missing  []string
	dir      string
	resolved bool
	root     *os.Root
}

func rootEntryIsLink(root *os.Root, name string, info os.FileInfo) bool {
	if info != nil && info.Mode()&os.ModeSymlink != 0 {
		return true
	}
	_, err := root.Readlink(name)
	return err == nil
}

func openVerifiedRoot(dir, lockBase string, lockInfo os.FileInfo) (*os.Root, bool, error) {
	r, err := os.OpenRoot(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	if lockInfo != nil {
		st, statErr := r.Lstat(lockBase)
		if statErr != nil || !os.SameFile(st, lockInfo) {
			_ = r.Close()
			return nil, false, errRootChanged
		}
	}
	return r, true, nil
}

func openPinnedSubdirWithInfo(parent *os.Root, base string) (*os.Root, os.FileInfo, bool, error) {
	info, err := parent.Lstat(base)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil, false, nil
	}
	if err != nil {
		return nil, nil, false, err
	}
	if rootEntryIsLink(parent, base, info) {
		return nil, nil, false, errRootChanged
	}
	if !info.IsDir() {
		return nil, nil, false, nil
	}
	sub, err := parent.OpenRoot(base)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil, false, nil
	}
	if err != nil {
		return nil, nil, false, err
	}
	subInfo, err := sub.Stat(".")
	if err != nil {
		_ = sub.Close()
		return nil, nil, false, err
	}
	if !os.SameFile(subInfo, info) {
		_ = sub.Close()
		return nil, nil, false, errRootChanged
	}
	return sub, subInfo, true, nil
}

func openPinnedSubdir(parent *os.Root, base string) (*os.Root, bool, error) {
	sub, _, ok, err := openPinnedSubdirWithInfo(parent, base)
	return sub, ok, err
}

func openPinnedDirDirect(dir string) (*os.Root, bool, error) {
	info, err := os.Lstat(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	if isLinkLeaf(dir) {
		return nil, false, errRootChanged
	}
	if !info.IsDir() {
		return nil, false, nil
	}
	r, err := os.OpenRoot(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	rInfo, err := r.Stat(".")
	if err != nil {
		_ = r.Close()
		return nil, false, err
	}
	if !os.SameFile(rInfo, info) {
		_ = r.Close()
		return nil, false, errRootChanged
	}
	return r, true, nil
}

func lockPathMatchesInfo(lockPath string, lockInfo os.FileInfo) bool {
	if lockInfo == nil {
		return true
	}
	st, err := os.Lstat(lockPath)
	return err == nil && os.SameFile(st, lockInfo)
}

func removeRetiredInDirLockRoot(root *os.Root, name string) error {
	var err error
	for i := range 50 {
		err = root.Remove(name)
		if err == nil || errors.Is(err, os.ErrNotExist) {
			return nil
		}
		if i < 49 {
			time.Sleep(10 * time.Millisecond)
		}
	}
	return err
}

// isLinkLeaf reports whether path itself is a symlink or (on Windows) an NTFS
// directory junction, matching os.Readlink so junctions follow the same
// link-leaf removal path as symlinks.
func isLinkLeaf(path string) bool {
	cleaned := filepath.Clean(path)
	if info, err := os.Lstat(cleaned); err == nil && info.Mode()&os.ModeSymlink != 0 {
		return true
	}
	_, err := os.Readlink(cleaned)
	return err == nil
}

type lockIdentityKey struct {
	kind uint8 // 0 = hard-linked file inode, 1 = parent dir inode + base, 2 = existing ancestor inode + rel tail, 3 = lexical fallback
	dev  uint64
	ino  uint64
	name string
}

func lockOrderKey(p string) lockIdentityKey {
	if resolved, err := filepath.EvalSymlinks(p); err == nil {
		p = resolved
	}
	if dev, ino, nlink, ok := pathFileID(p, false); ok && nlink > 1 {
		return lockIdentityKey{kind: 0, dev: dev, ino: ino}
	}
	dir := filepath.Dir(p)
	if dev, ino, _, ok := pathFileID(dir, true); ok {
		return lockIdentityKey{kind: 1, dev: dev, ino: ino, name: filepath.Base(p)}
	}
	// If dir does not exist yet (e.g. a dangling catalogs/ symlink whose
	// parent exists under NFC/NFD or mixed-case spellings), anchor ordering on
	// the nearest existing ancestor's filesystem identity plus the relative tail.
	cur := dir
	for {
		parent := filepath.Dir(cur)
		if parent == cur {
			break
		}
		if dev, ino, _, ok := pathFileID(parent, true); ok {
			if rel, err := filepath.Rel(parent, p); err == nil {
				return lockIdentityKey{kind: 2, dev: dev, ino: ino, name: strings.ToLower(filepath.ToSlash(rel))}
			}
		}
		cur = parent
	}
	return lockIdentityKey{kind: 3, name: strings.ToLower(p)}
}

func lockPathLess(a, b string) bool {
	ka, kb := lockOrderKey(a), lockOrderKey(b)
	if ka.kind != kb.kind {
		return ka.kind < kb.kind
	}
	if ka.dev != kb.dev {
		return ka.dev < kb.dev
	}
	if ka.ino != kb.ino {
		return ka.ino < kb.ino
	}
	if ka.name != kb.name {
		return ka.name < kb.name
	}
	la, lb := strings.ToLower(a), strings.ToLower(b)
	if la != lb {
		return la < lb
	}
	return a < b
}

// sameLockFile reports whether a and b refer to the same lock file, matching
// equal paths, hard-linked files, and identical hashed lock basenames inside
// the same directory inode (e.g. Cache/ vs cache/ on case-insensitive volumes).
func sameLockFile(a, b string) bool {
	if a == b {
		return true
	}
	if ia, errA := os.Stat(a); errA == nil {
		if ib, errB := os.Stat(b); errB == nil && os.SameFile(ia, ib) {
			return true
		}
	}
	if filepath.Base(a) != filepath.Base(b) {
		return false
	}
	if pa, errA := os.Stat(filepath.Dir(a)); errA == nil {
		if pb, errB := os.Stat(filepath.Dir(b)); errB == nil && os.SameFile(pa, pb) {
			return true
		}
	}
	return false
}

func sameDirPath(a, b string) bool {
	ca, cb := filepath.Clean(a), filepath.Clean(b)
	if ca == cb {
		return true
	}
	pa, okA := resolveCanonicalPath(ca, false)
	if !okA {
		pa = ca
	}
	pb, okB := resolveCanonicalPath(cb, false)
	if !okB {
		pb = cb
	}
	if pa == pb {
		return true
	}
	if ia, errA := os.Stat(pa); errA == nil {
		if ib, errB := os.Stat(pb); errB == nil && os.SameFile(ia, ib) {
			return true
		}
	}
	return false
}

func sameCatalogSlot(a, b string) bool {
	ca, cb := filepath.Clean(a), filepath.Clean(b)
	if ca == cb {
		return true
	}
	pa, okA := resolveCanonicalPath(filepath.Dir(ca), false)
	if !okA {
		pa = filepath.Dir(ca)
	}
	pb, okB := resolveCanonicalPath(filepath.Dir(cb), false)
	if !okB {
		pb = filepath.Dir(cb)
	}
	sameParent := sameDirPath(pa, pb)
	if sameParent && filepath.Base(ca) == filepath.Base(cb) {
		return true
	}
	la, errA := os.Lstat(ca)
	lb, errB := os.Lstat(cb)
	if errA != nil || errB != nil {
		return false
	}
	if !os.SameFile(la, lb) {
		return false
	}
	if la.IsDir() {
		return true
	}
	if !sameParent {
		return false
	}
	// Both are non-directory leaves (such as symlinks) in the same parent with
	// the same inode: distinguish a single entry spelled differently (case- or
	// Unicode-insensitively) from two hard-linked symlinks in the same parent.
	entries, err := os.ReadDir(pa)
	if err != nil {
		return false
	}
	matches := 0
	for _, entry := range entries {
		if info, err := os.Lstat(filepath.Join(pa, entry.Name())); err == nil && os.SameFile(info, la) {
			matches++
			if matches > 1 {
				return false
			}
		}
	}
	return matches == 1
}

func sameCatalogTarget(a, b string) bool {
	if sameCatalogSlot(a, b) {
		return true
	}
	ca, cb := canonicalCatalogDir(a), canonicalCatalogDir(b)
	if ca == cb {
		return true
	}
	if ia, errA := os.Stat(ca); errA == nil {
		if ib, errB := os.Stat(cb); errB == nil && os.SameFile(ia, ib) {
			return true
		}
	}
	return false
}

func isSameOrUnderDir(candidate, dir string) bool {
	if sameCatalogTarget(candidate, dir) {
		return true
	}
	canonCand := canonicalCatalogDir(candidate)
	canonDir := canonicalCatalogDir(dir)
	if rel, err := filepath.Rel(canonDir, canonCand); err == nil && (rel == "." || filepath.IsLocal(rel)) {
		return true
	}
	// Also walk ancestors of canonCand with os.SameFile in case case-insensitive
	// or aliased mount spellings differ from filepath.Rel's lexical check.
	if dirInfo, err := os.Stat(canonDir); err == nil {
		cur := canonCand
		for {
			if curInfo, err := os.Stat(cur); err == nil && os.SameFile(curInfo, dirInfo) {
				return true
			}
			parent := filepath.Dir(cur)
			if parent == cur {
				break
			}
			cur = parent
		}
	}
	return false
}

// pathTraversesSlot reports whether path equals slot or traverses slot while
// following symlinks or Windows NTFS junctions (e.g. outer -> middle -> direct
// traverses middle). When includeSubdirs is true, any path at or beneath slot
// (such as alias -> direct/subdir) also matches so recursive deletion of slot
// cannot destroy a registered target beneath it.
func pathTraversesSlot(path, slot string) bool {
	return pathTraverses(path, slot, false)
}

func pathTraversesDir(path, dir string) bool {
	return pathTraverses(path, dir, true)
}

func pathTraverses(path, slot string, includeSubdirs bool) bool {
	match := func(p string) bool {
		if includeSubdirs {
			return isSameOrUnderDir(p, slot)
		}
		return sameCatalogSlot(p, slot)
	}
	if match(path) {
		return true
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
			continue
		}
		if comp == ".." {
			info, err := os.Stat(resolved)
			if err != nil || !info.IsDir() {
				return false
			}
			resolved = filepath.Dir(resolved)
			continue
		}
		next := filepath.Join(resolved, comp)
		if match(next) {
			return true
		}
		if target, err := os.Readlink(next); err == nil {
			symlinksFollowed++
			if symlinksFollowed > 40 {
				return false
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
		if eval, err := filepath.EvalSymlinks(next); err == nil {
			resolved = eval
			continue
		}
		resolved = next
	}
	return match(resolved)
}

// catalogDirRegistered reports whether any catalog currently registered in the
// user or system catalog lists maps to cache slot dir (used by refresh).
func catalogDirRegistered(dir string) (bool, error) {
	layers, err := readCatalogLayers()
	if err != nil {
		return false, err
	}
	for _, c := range layers.combined() {
		if sameCatalogSlot(c.dir(layers.cacheDir), dir) {
			return true, nil
		}
	}
	return false, nil
}

// catalogSlotReferenced reports whether any registered catalog maps to dir or
// traverses dir through a symlink/junction chain (e.g. outer -> middle -> direct),
// so removing an intermediate link never leaves a registered alias dangling.
func catalogSlotReferenced(dir string) (bool, error) {
	return catalogSlotInfoReferenced(dir, nil, "", nil)
}

func catalogSlotInfoReferenced(dir string, pinnedParent *os.Root, slotBase string, pinnedInfo os.FileInfo) (bool, error) {
	layers, err := readCatalogLayers()
	if err != nil {
		return false, err
	}
	var parentInfo os.FileInfo
	singleLinkInParent := false
	if pinnedParent != nil && pinnedInfo != nil {
		if pi, err := pinnedParent.Stat("."); err == nil {
			parentInfo = pi
		}
		if f, err := pinnedParent.Open("."); err == nil {
			names, _ := f.Readdirnames(-1)
			_ = f.Close()
			matches := 0
			for _, name := range names {
				if info, err := pinnedParent.Lstat(name); err == nil && os.SameFile(info, pinnedInfo) {
					matches++
					if matches > 1 {
						break
					}
				}
			}
			singleLinkInParent = matches == 1
		}
	}
	matchPinnedSlot := func(p string) bool {
		if parentInfo == nil || pinnedInfo == nil {
			return false
		}
		info, err := os.Lstat(p)
		if err != nil || !os.SameFile(info, pinnedInfo) {
			return false
		}
		pInfo, err := os.Stat(filepath.Dir(p))
		if err != nil || !os.SameFile(pInfo, parentInfo) {
			return false
		}
		return filepath.Base(p) == slotBase || singleLinkInParent
	}
	for _, c := range layers.combined() {
		cand := c.dir(layers.cacheDir)
		if pathTraversesSlot(cand, dir) {
			return true, nil
		}
		if parentInfo != nil && pinnedInfo != nil && pathTraversesMatch(cand, matchPinnedSlot) {
			return true, nil
		}
	}
	return false, nil
}

// catalogTargetRegistered reports whether any registered catalog maps to dir,
// resolves to dir or a subdirectory beneath dir (e.g. alias -> direct/subdir),
// or traverses dir through a symlink/junction chain, so recursively deleting
// dir never destroys or breaks another registered catalog's cache.
func catalogTargetRegistered(dir string) (bool, error) {
	return catalogTargetInfoRegistered(dir, nil)
}

func catalogTargetInfoRegistered(dir string, pinnedInfo os.FileInfo) (bool, error) {
	layers, err := readCatalogLayers()
	if err != nil {
		return false, err
	}
	matchPinnedDir := func(p string) bool {
		if pinnedInfo == nil {
			return false
		}
		if info, err := os.Lstat(p); err == nil && os.SameFile(info, pinnedInfo) {
			return true
		}
		if info, err := os.Stat(p); err == nil && os.SameFile(info, pinnedInfo) {
			return true
		}
		return false
	}
	for _, c := range layers.combined() {
		cand := c.dir(layers.cacheDir)
		if pathTraversesDir(cand, dir) {
			return true, nil
		}
		if pinnedInfo != nil {
			if pathTraversesMatch(cand, matchPinnedDir) {
				return true, nil
			}
			cur := canonicalCatalogDir(cand)
			for {
				if info, err := os.Stat(cur); err == nil && os.SameFile(info, pinnedInfo) {
					return true, nil
				}
				parent := filepath.Dir(cur)
				if parent == cur {
					break
				}
				cur = parent
			}
		}
	}
	return false, nil
}

func pathTraversesMatch(path string, match func(string) bool) bool {
	if match(path) {
		return true
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
			continue
		}
		if comp == ".." {
			info, err := os.Stat(resolved)
			if err != nil || !info.IsDir() {
				return false
			}
			resolved = filepath.Dir(resolved)
			continue
		}
		next := filepath.Join(resolved, comp)
		if match(next) {
			return true
		}
		if target, err := os.Readlink(next); err == nil {
			symlinksFollowed++
			if symlinksFollowed > 40 {
				return false
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
		if eval, err := filepath.EvalSymlinks(next); err == nil {
			resolved = eval
			continue
		}
		resolved = next
	}
	return match(resolved)
}

func readCatalogLockState(dir string) (catalogGen, string, bool, error) {
	slk, err := lockCatalogDirSharedWithMissing(dir)
	if err != nil {
		return catalogGen{}, "", false, err
	}
	slk.unlock()
	return slk.gen, slk.dir, slk.resolved, nil
}

func readCatalogLockGen(dir string) (catalogGen, error) {
	gen, _, _, err := readCatalogLockState(dir)
	return gen, err
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

// canonicalOnDiskBase returns the single on-disk directory entry name inside
// parent matching base by os.SameFile when the entry is an existing directory.
// Directories cannot be hard-linked, so restricting this to IsDir() preserves
// distinct identities for hard-linked symlinks while unifying Windows 8.3
// short names and mixed-case/Unicode spellings of existing directories.
func canonicalOnDiskBase(parent, base string) string {
	full := filepath.Join(parent, base)
	targetInfo, err := os.Lstat(full)
	if err != nil || !targetInfo.IsDir() {
		return base
	}
	entries, err := os.ReadDir(parent)
	if err != nil {
		return base
	}
	for _, e := range entries {
		if info, err := os.Lstat(filepath.Join(parent, e.Name())); err == nil && info.IsDir() && os.SameFile(info, targetInfo) {
			return e.Name()
		}
	}
	return base
}

func hashLockPath(parent, base string) string {
	base = canonicalOnDiskBase(parent, base)
	normalized := norm.NFD.String(cases.Fold().String(norm.NFD.String(base)))
	sum := sha256.Sum256([]byte(normalized))
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
func canonicalSlotPath(dir string) string {
	cleaned := filepath.Clean(dir)
	parent, ok := resolveCanonicalPath(filepath.Dir(cleaned), false)
	if !ok {
		parent = filepath.Dir(cleaned)
	}
	return filepath.Join(parent, filepath.Base(cleaned))
}

func catalogLockPath(dir string) string {
	slotDir := canonicalSlotPath(dir)
	return hashLockPath(filepath.Dir(slotDir), filepath.Base(slotDir))
}

func catalogTargetLockPathForClean(clean string) string {
	return hashLockPath(filepath.Dir(clean), filepath.Base(clean))
}

// catalogTargetLockPath returns the external lock file path for dir's
// canonical target directory.
func catalogTargetLockPath(dir string) string {
	return catalogTargetLockPathForClean(canonicalCatalogDir(dir))
}

func catalogExternalLockPathsFor(slot, target string) []string {
	if sameLockFile(slot, target) {
		return []string{slot}
	}
	if lockPathLess(target, slot) {
		return []string{target, slot}
	}
	return []string{slot, target}
}

// catalogExternalLockPaths returns the unique external lock file paths for dir
// in a case-insensitive canonical order (with exact spelling as tie-breaker),
// deduplicating paths that resolve to the same lock file/directory inode so
// mixed-case spellings on case-insensitive macOS/Windows volumes never invert
// lock order or self-deadlock.
func catalogExternalLockPaths(dir string) []string {
	return catalogExternalLockPathsFor(catalogLockPath(dir), catalogTargetLockPath(dir))
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

func lockCatalogDirWithMode(dir string, allowInDirFallback bool) (*catalogDirLock, error) {
	return lockCatalogDirWithRetry(dir, allowInDirFallback, 8)
}

// afterInDirFallbackLock runs immediately after the fallback in-dir lock is
// acquired and before checking whether a skipped external lock appeared; tests
// may set it to reproduce multi-step permission transitions.
var afterInDirFallbackLock = func(dir string) {}

func lockCatalogDirWithRetry(dir string, allowInDirFallback bool, retriesLeft int) (*catalogDirLock, error) {
	slotDir := canonicalSlotPath(dir)
	slotPath := hashLockPath(filepath.Dir(slotDir), filepath.Base(slotDir))
	linkLeaf := isLinkLeaf(slotDir)
	clean, targetResolved := resolveCanonicalPath(slotDir, false)
	if !targetResolved {
		if allowInDirFallback || !linkLeaf {
			return nil, fmt.Errorf("cannot resolve catalog directory %s", dir)
		}
		clean = ""
	}
	var targetPath, inDirPath string
	if clean != "" {
		targetPath = catalogTargetLockPathForClean(clean)
		inDirPath = filepath.Join(clean, ".lock")
	}
	if allowInDirFallback {
		// Ensure both external lock parent directories exist before computing
		// OS filesystem identity ordering keys so concurrent first refreshes
		// through a dangling symlink across filesystems never observe a
		// missing-parent vs existing-parent key transition and invert order.
		_ = os.MkdirAll(filepath.Dir(slotPath), 0o755)
		_ = os.MkdirAll(filepath.Dir(targetPath), 0o755)
	}
	slotSameAsTarget := targetPath != "" && sameLockFile(slotPath, targetPath)
	var extPaths []string
	if targetPath == "" {
		extPaths = []string{slotPath}
	} else if !allowInDirFallback && linkLeaf && !slotSameAsTarget {
		// When removing a dangling leaf link whose target parent does not even
		// exist, lock only slotPath without creating the missing target parent
		// (a single lock can never deadlock with concurrent refreshes).
		if _, err := os.Stat(filepath.Dir(targetPath)); errors.Is(err, os.ErrNotExist) {
			extPaths = []string{slotPath}
		}
	}
	if extPaths == nil {
		extPaths = catalogExternalLockPathsFor(slotPath, targetPath)
	}
	var extLocks []*catalogLock
	var slotLk, targetLk *catalogLock
	var skippedExt []string
	unlockAll := func() {
		for i := len(extLocks) - 1; i >= 0; i-- {
			extLocks[i].unlock()
		}
		extLocks = nil
		slotLk = nil
		targetLk = nil
	}
	targetChanged := func() bool {
		slotAfter := canonicalSlotPath(dir)
		if slotAfter != slotDir || !sameLockFile(hashLockPath(filepath.Dir(slotAfter), filepath.Base(slotAfter)), slotPath) {
			return true
		}
		if isLinkLeaf(slotAfter) != linkLeaf {
			return true
		}
		cleanAfter, okAfter := resolveCanonicalPath(slotAfter, false)
		if okAfter != targetResolved {
			return true
		}
		if !targetResolved {
			return false
		}
		return cleanAfter != clean || !sameLockFile(catalogTargetLockPathForClean(cleanAfter), targetPath)
	}
	needInDirFallback := false
	var waitFirst string
	for busyAttempt := range 64 {
		if busyAttempt > 0 {
			if targetChanged() {
				if retriesLeft > 0 {
					return lockCatalogDirWithRetry(dir, allowInDirFallback, retriesLeft-1)
				}
				return nil, errors.New("catalog locks changed during refresh; run 'sciget refresh' again")
			}
			if targetPath == "" {
				extPaths = []string{slotPath}
			} else {
				extPaths = catalogExternalLockPathsFor(slotPath, targetPath)
				if !allowInDirFallback && linkLeaf && !slotSameAsTarget {
					if _, err := os.Stat(filepath.Dir(targetPath)); errors.Is(err, os.ErrNotExist) {
						extPaths = []string{slotPath}
					}
				}
			}
			if waitFirst != "" && len(extPaths) == 2 && sameLockFile(extPaths[1], waitFirst) {
				extPaths = []string{extPaths[1], extPaths[0]}
			}
			waitFirst = ""
			if busyAttempt > 4 {
				time.Sleep(2 * time.Millisecond)
			}
		}
		skippedExt = nil
		needInDirFallback = false
		busyRetry := false
		for _, p := range extPaths {
			var (
				lk  *catalogLock
				err error
			)
			if len(extLocks) == 0 {
				lk, err = lockFileExclusive(p)
			} else {
				lk, err = tryLockFileExclusive(p)
			}
			if errors.Is(err, errLockBusy) {
				unlockAll()
				waitFirst = p
				busyRetry = true
				break
			}
			if err != nil {
				if isReadOnlyOrPermError(err) {
					// Removing a leaf symlink or junction only unlinks the link in
					// slot's parent; if the target's parent is read-only (0555 or
					// EROFS) we still hold slotPath and never touch the target dir.
					if !allowInDirFallback && linkLeaf && !sameLockFile(p, slotPath) {
						skippedExt = append(skippedExt, p)
						continue
					}
					if allowInDirFallback {
						if _, extStatErr := os.Stat(p); errors.Is(extStatErr, os.ErrNotExist) {
							if info, dirStatErr := os.Stat(clean); dirStatErr == nil && info.IsDir() {
								// Only fall back to target/.lock when targetPath
								// itself cannot be created. When an alias's own
								// slot parent is read-only but targetPath is
								// writable, targetPath alone serializes every
								// reader and writer on the target directory.
								if sameLockFile(p, targetPath) {
									needInDirFallback = true
								}
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
			if sameLockFile(p, slotPath) {
				slotLk = lk
			}
			if targetPath != "" && sameLockFile(p, targetPath) {
				targetLk = lk
			}
		}
		if !busyRetry {
			break
		}
		if busyAttempt == 63 {
			return nil, errors.New("catalog locks busy during refresh; run 'sciget refresh' again")
		}
	}

	if needInDirFallback {
		inLk, inErr := lockFileExclusive(inDirPath)
		if inErr != nil {
			unlockAll()
			return nil, inErr
		}
		targetRoot, ok, rootErr := openVerifiedRoot(clean, ".lock", inLk.fileInfo)
		if errors.Is(rootErr, errRootChanged) || (rootErr == nil && !ok) {
			inLk.unlock()
			unlockAll()
			if retriesLeft > 0 {
				return lockCatalogDirWithRetry(dir, allowInDirFallback, retriesLeft-1)
			}
			return nil, errors.New("catalog locks changed during refresh; run 'sciget refresh' again")
		}
		if rootErr != nil {
			inLk.unlock()
			unlockAll()
			return nil, rootErr
		}
		var slotParent *os.Root
		if slotLk != nil {
			sp, spOK, spErr := openVerifiedRoot(filepath.Dir(slotDir), filepath.Base(slotPath), slotLk.fileInfo)
			if errors.Is(spErr, errRootChanged) || (spErr == nil && !spOK) {
				_ = targetRoot.Close()
				inLk.unlock()
				unlockAll()
				if retriesLeft > 0 {
					return lockCatalogDirWithRetry(dir, allowInDirFallback, retriesLeft-1)
				}
				return nil, errors.New("catalog locks changed during refresh; run 'sciget refresh' again")
			}
			if spErr != nil {
				if isReadOnlyOrPermError(spErr) && lockPathMatchesInfo(slotPath, slotLk.fileInfo) {
					// Search-only (0111) slot parent: refresh writes only to
					// targetRoot, not to slotParent.
				} else {
					_ = targetRoot.Close()
					inLk.unlock()
					unlockAll()
					return nil, spErr
				}
			} else {
				slotParent = sp
			}
		}
		closeFallbackRoots := func() {
			if targetRoot != nil {
				_ = targetRoot.Close()
			}
			if slotParent != nil {
				_ = slotParent.Close()
			}
		}
		// Always check whether any skipped external lock path appeared or the
		// catalog alias was retargeted concurrently (even on the final attempt)
		// so we never return holding the wrong target's lock.
		afterInDirFallbackLock(dir)
		if exists, statErr := anyPathExists(skippedExt); statErr != nil {
			closeFallbackRoots()
			inLk.unlock()
			unlockAll()
			return nil, statErr
		} else if exists || targetChanged() {
			closeFallbackRoots()
			inLk.unlock()
			unlockAll()
			if retriesLeft > 0 {
				return lockCatalogDirWithRetry(dir, allowInDirFallback, retriesLeft-1)
			}
			return nil, errors.New("catalog locks changed during refresh; run 'sciget refresh' again")
		}
		computeGen := func() catalogGen {
			if slotSameAsTarget {
				return catalogGen{slot: inLk.gen, target: inLk.gen}
			}
			var slotG uint64
			if slotLk != nil {
				slotG = slotLk.gen
			}
			return catalogGen{slot: slotG, target: inLk.gen}
		}
		combined := &catalogDirLock{
			dir:        clean,
			slotDir:    slotDir,
			linkLeaf:   linkLeaf,
			slotParent: slotParent,
			slotBase:   filepath.Base(slotDir),
			targetBase: filepath.Base(clean),
			targetRoot: targetRoot,
			gen:        computeGen(),
			unlock: func() {
				closeFallbackRoots()
				inLk.unlock()
				unlockAll()
			},
		}
		combined.bump = func() error {
			for _, lk := range extLocks {
				if err := lk.bump(); err != nil {
					return err
				}
			}
			if err := inLk.bump(); err != nil {
				return err
			}
			combined.gen = computeGen()
			return nil
		}
		return combined, nil
	}

	// Verify dir still resolves to the target whose locks we acquired before
	// pinning directory handles or retiring any in-dir lock.
	if targetChanged() {
		unlockAll()
		if retriesLeft > 0 {
			return lockCatalogDirWithRetry(dir, allowInDirFallback, retriesLeft-1)
		}
		return nil, errors.New("catalog locks changed during refresh; run 'sciget refresh' again")
	}

	var slotParent, targetParent, targetRoot *os.Root
	closeRoots := func() {
		if targetRoot != nil {
			_ = targetRoot.Close()
		}
		if targetParent != nil {
			_ = targetParent.Close()
		}
		if slotParent != nil {
			_ = slotParent.Close()
		}
	}
	if slotLk != nil {
		sp, spOK, spErr := openVerifiedRoot(filepath.Dir(slotDir), filepath.Base(slotPath), slotLk.fileInfo)
		if errors.Is(spErr, errRootChanged) || (spErr == nil && !spOK) {
			unlockAll()
			if retriesLeft > 0 {
				return lockCatalogDirWithRetry(dir, allowInDirFallback, retriesLeft-1)
			}
			return nil, errors.New("catalog locks changed during refresh; run 'sciget refresh' again")
		}
		if spErr != nil {
			if allowInDirFallback && isReadOnlyOrPermError(spErr) && lockPathMatchesInfo(slotPath, slotLk.fileInfo) {
				// Search-only (0111) slot parent: refresh writes only to the
				// target directory, not to slotParent.
			} else {
				unlockAll()
				return nil, spErr
			}
		} else {
			slotParent = sp
		}
	}
	if targetLk != nil && clean != "" {
		// Always open and verify targetParent independently from slotParent so
		// hard-linked lock files across different parent directories never
		// redirect target writes into the slot parent.
		tp, tpOK, tpErr := openVerifiedRoot(filepath.Dir(clean), filepath.Base(targetPath), targetLk.fileInfo)
		if errors.Is(tpErr, errRootChanged) || (tpErr == nil && !tpOK) {
			closeRoots()
			unlockAll()
			if retriesLeft > 0 {
				return lockCatalogDirWithRetry(dir, allowInDirFallback, retriesLeft-1)
			}
			return nil, errors.New("catalog locks changed during refresh; run 'sciget refresh' again")
		}
		if tpErr != nil {
			if !allowInDirFallback && linkLeaf && isReadOnlyOrPermError(tpErr) && lockPathMatchesInfo(targetPath, targetLk.fileInfo) {
				// Search-only (0111) target parent during alias removal: we
				// verified the locked target lock file and removal only unlinks
				// the alias inside slotParent.
			} else if allowInDirFallback && isReadOnlyOrPermError(tpErr) && lockPathMatchesInfo(targetPath, targetLk.fileInfo) {
				tr, trOK, trErr := openPinnedDirDirect(clean)
				if errors.Is(trErr, errRootChanged) || (trErr == nil && !trOK) {
					closeRoots()
					unlockAll()
					if retriesLeft > 0 {
						return lockCatalogDirWithRetry(dir, allowInDirFallback, retriesLeft-1)
					}
					return nil, errors.New("catalog locks changed during refresh; run 'sciget refresh' again")
				}
				if trErr != nil {
					closeRoots()
					unlockAll()
					return nil, trErr
				}
				if !lockPathMatchesInfo(targetPath, targetLk.fileInfo) {
					_ = tr.Close()
					closeRoots()
					unlockAll()
					if retriesLeft > 0 {
						return lockCatalogDirWithRetry(dir, allowInDirFallback, retriesLeft-1)
					}
					return nil, errors.New("catalog locks changed during refresh; run 'sciget refresh' again")
				}
				targetRoot = tr
			} else {
				closeRoots()
				unlockAll()
				return nil, tpErr
			}
		} else {
			targetParent = tp
		}
	}
	if targetChanged() {
		closeRoots()
		unlockAll()
		if retriesLeft > 0 {
			return lockCatalogDirWithRetry(dir, allowInDirFallback, retriesLeft-1)
		}
		return nil, errors.New("catalog locks changed during refresh; run 'sciget refresh' again")
	}

	var retiredInLk *catalogLock
	var retiredSubRoot *os.Root
	targetBase := ""
	if clean != "" {
		targetBase = filepath.Base(clean)
	}
	// If an earlier refresh fell back to the in-dir lock while the target's
	// parent directory was read-only, drain and retire it once targetPath is
	// held so concurrent holders finish and target's monotonic generation is
	// preserved. Never retire the target's in-dir lock when removing a leaf
	// link (which leaves the target intact) or when targetPath was skipped.
	if targetLk != nil && clean != "" && !(linkLeaf && !allowInDirFallback) && (targetParent != nil || targetRoot != nil) {
		var sub *os.Root
		var subOK bool
		var subErr error
		closeSub := false
		if targetParent != nil {
			sub, subOK, subErr = openPinnedSubdir(targetParent, targetBase)
			closeSub = subOK
		} else {
			sub = targetRoot
			subOK = true
		}
		if errors.Is(subErr, errRootChanged) {
			closeRoots()
			unlockAll()
			if retriesLeft > 0 {
				return lockCatalogDirWithRetry(dir, allowInDirFallback, retriesLeft-1)
			}
			return nil, errors.New("catalog locks changed during refresh; run 'sciget refresh' again")
		}
		if subErr != nil {
			closeRoots()
			unlockAll()
			return nil, subErr
		}
		if subOK {
			if _, inStatErr := sub.Lstat(".lock"); inStatErr == nil {
				inLk, inErr := lockFileExclusive(inDirPath)
				if inErr != nil {
					if closeSub {
						_ = sub.Close()
					}
					closeRoots()
					unlockAll()
					return nil, inErr
				}
				if inLk.fileInfo != nil {
					if st, stErr := sub.Lstat(".lock"); stErr != nil || !os.SameFile(st, inLk.fileInfo) {
						inLk.unlock()
						if closeSub {
							_ = sub.Close()
						}
						closeRoots()
						unlockAll()
						if retriesLeft > 0 {
							return lockCatalogDirWithRetry(dir, allowInDirFallback, retriesLeft-1)
						}
						return nil, errors.New("catalog locks changed during refresh; run 'sciget refresh' again")
					}
				}
				if syncErr := targetLk.syncGen(inLk.gen); syncErr != nil {
					inLk.unlock()
					if closeSub {
						_ = sub.Close()
					}
					closeRoots()
					unlockAll()
					return nil, syncErr
				}
				if runtime.GOOS != "windows" {
					if rmErr := sub.Remove(".lock"); rmErr != nil && !errors.Is(rmErr, os.ErrNotExist) {
						inLk.unlock()
						if closeSub {
							_ = sub.Close()
						}
						closeRoots()
						unlockAll()
						return nil, rmErr
					}
					if closeSub {
						_ = sub.Close()
					}
					retiredInLk = inLk
				} else if !allowInDirFallback {
					inLk.unlock()
					rmErr := removeRetiredInDirLockRoot(sub, ".lock")
					if closeSub {
						_ = sub.Close()
					}
					if rmErr != nil {
						closeRoots()
						unlockAll()
						return nil, rmErr
					}
				} else {
					retiredInLk = inLk
					if closeSub {
						retiredSubRoot = sub
					}
				}
			} else if closeSub {
				_ = sub.Close()
			}
		}
	}
	var readOnlyTargetGen uint64
	if linkLeaf && !allowInDirFallback && targetLk == nil && clean != "" {
		if info, statErr := os.Stat(clean); statErr == nil && info.IsDir() {
			if inUnlock, inGen, _, inLocked, inErr := lockFileShared(inDirPath); inErr == nil && inLocked {
				inUnlock()
				readOnlyTargetGen = inGen
			}
		}
	}
	computeGen := func() catalogGen {
		var slotG, targetG uint64
		if slotLk != nil {
			slotG = slotLk.gen
		}
		if targetLk != nil {
			targetG = targetLk.gen
		} else {
			targetG = readOnlyTargetGen
		}
		return catalogGen{slot: slotG, target: targetG}
	}
	combined := &catalogDirLock{
		dir:          clean,
		slotDir:      slotDir,
		linkLeaf:     linkLeaf,
		slotParent:   slotParent,
		slotBase:     filepath.Base(slotDir),
		targetParent: targetParent,
		targetBase:   targetBase,
		targetRoot:   targetRoot,
		gen:          computeGen(),
		unlock: func() {
			if retiredInLk != nil {
				retiredInLk.unlock()
				if runtime.GOOS == "windows" {
					if retiredSubRoot != nil {
						_ = removeRetiredInDirLockRoot(retiredSubRoot, ".lock")
					} else if targetRoot != nil {
						_ = removeRetiredInDirLockRoot(targetRoot, ".lock")
					}
				}
			}
			if retiredSubRoot != nil {
				_ = retiredSubRoot.Close()
			}
			closeRoots()
			unlockAll()
		},
	}
	combined.bump = func() error {
		for _, lk := range extLocks {
			if err := lk.bump(); err != nil {
				return err
			}
		}
		combined.gen = computeGen()
		return nil
	}
	return combined, nil
}

func lockCatalogDir(dir string) (*catalogDirLock, error) {
	return lockCatalogDirWithMode(dir, true)
}

func lockCatalogDirForRemove(dir string) (*catalogDirLock, error) {
	return lockCatalogDirWithMode(dir, false)
}

// beforeInDirSharedLock runs after lockCatalogDirShared finds no external lock
// and before it opens the fallback in-dir lock; tests may set it to reproduce
// migration races.
var beforeInDirSharedLock = func(dir string) {}

func lockCatalogDirShared(dir string) (func(), catalogGen, bool, error) {
	slk, err := lockCatalogDirSharedWithMissing(dir)
	if err != nil {
		return nil, catalogGen{}, false, err
	}
	if !slk.locked {
		slk.unlock()
		return func() {}, slk.gen, false, nil
	}
	return slk.unlock, slk.gen, true, nil
}

// beforeSecondSharedLock runs after lockCatalogDirSharedWithMissing acquires
// the first external shared lock and before attempting the second; tests may
// set it to reproduce reader-vs-writer lock-order transitions.
var beforeSecondSharedLock = func(dir string) {}

// maxSharedLockAttempts bounds lockCatalogDirSharedWithMissing retries; tests
// may reduce it to verify contention exhaustion returns an error rather than
// bypassing the target lock.
var maxSharedLockAttempts = 64

func lockCatalogDirSharedWithMissing(dir string) (sharedCatalogLock, error) {
	var waitFirst string
	for attempt := range maxSharedLockAttempts {
		slotDir := canonicalSlotPath(dir)
		slotPath := hashLockPath(filepath.Dir(slotDir), filepath.Base(slotDir))
		clean, ok := resolveCanonicalPath(slotDir, false)
		if !ok {
			// Broken symlink containing ".." through a missing directory or a
			// symlink loop: never fall back to using the unresolved symlink as
			// lockedDir (which could later resolve to an unlocked target).
			return sharedCatalogLock{unlock: func() {}}, nil
		}
		targetPath := catalogTargetLockPathForClean(clean)
		slotSameAsTarget := sameLockFile(slotPath, targetPath)
		extPaths := catalogExternalLockPathsFor(slotPath, targetPath)
		if waitFirst != "" && len(extPaths) == 2 && sameLockFile(extPaths[1], waitFirst) {
			extPaths = []string{extPaths[1], extPaths[0]}
		}
		waitFirst = ""
		if attempt > 4 {
			time.Sleep(2 * time.Millisecond)
		}
		var unlocks []func()
		var missing []string
		var slotGen, targetGen uint64
		var targetLockInfo os.FileInfo
		targetLocked := false
		busyRetry := false
		release := func() {
			for i := len(unlocks) - 1; i >= 0; i-- {
				unlocks[i]()
			}
		}
		targetChanged := func() bool {
			slotAfter := canonicalSlotPath(dir)
			if slotAfter != slotDir || !sameLockFile(hashLockPath(filepath.Dir(slotAfter), filepath.Base(slotAfter)), slotPath) {
				return true
			}
			cleanAfter, okAfter := resolveCanonicalPath(slotAfter, false)
			return !okAfter || cleanAfter != clean || !sameLockFile(catalogTargetLockPathForClean(cleanAfter), targetPath)
		}
		for _, p := range extPaths {
			var (
				u    func()
				g    uint64
				info os.FileInfo
				ok   bool
				err  error
			)
			if len(unlocks) == 0 {
				u, g, info, ok, err = lockFileShared(p)
			} else {
				beforeSecondSharedLock(dir)
				u, g, info, ok, err = tryLockFileShared(p)
			}
			if errors.Is(err, errLockBusy) {
				release()
				waitFirst = p
				busyRetry = true
				break
			}
			if err != nil {
				release()
				return sharedCatalogLock{}, err
			}
			if ok {
				unlocks = append(unlocks, u)
				if sameLockFile(p, slotPath) {
					slotGen = g
				}
				if sameLockFile(p, targetPath) {
					targetGen = g
					targetLockInfo = info
					targetLocked = true
				}
			} else {
				missing = append(missing, p)
			}
		}
		if busyRetry {
			continue
		}
		if len(unlocks) > 0 {
			// If a fallback in-dir lock still exists (e.g. an external lock
			// was just created before a migrating writer acquired it while an
			// earlier writer still holds .lock, or target's parent is read-only
			// while slot has an external lock), also lock .lock in ext -> inDir
			// order so we wait for any active .lock writer and read target's gen.
			inDirLocked := false
			var inDirLockInfo os.FileInfo
			inDirPath := filepath.Join(clean, ".lock")
			if info, statErr := os.Stat(clean); statErr == nil && info.IsDir() {
				inUnlock, inGen, inInfo, inLocked, inErr := lockFileShared(inDirPath)
				if inErr != nil {
					release()
					return sharedCatalogLock{}, inErr
				}
				if inLocked {
					inDirLocked = true
					inDirLockInfo = inInfo
					unlocks = append(unlocks, inUnlock)
					if inGen > targetGen {
						targetGen = inGen
						if slotSameAsTarget {
							slotGen = inGen
						}
					}
				}
			}
			var dirRoot *os.Root
			if inDirLocked {
				r, rOK, rErr := openVerifiedRoot(clean, ".lock", inDirLockInfo)
				if errors.Is(rErr, errRootChanged) || (rErr == nil && !rOK) {
					release()
					continue
				}
				if rErr != nil {
					release()
					return sharedCatalogLock{}, rErr
				}
				dirRoot = r
			} else if targetLocked {
				tp, tpOK, tpErr := openVerifiedRoot(filepath.Dir(clean), filepath.Base(targetPath), targetLockInfo)
				if errors.Is(tpErr, errRootChanged) || (tpErr == nil && !tpOK) {
					release()
					continue
				}
				if tpErr != nil {
					if isReadOnlyOrPermError(tpErr) && lockPathMatchesInfo(targetPath, targetLockInfo) {
						// Search-only (0111) parent directory: pin clean
						// directly and re-verify the locked lock-file inode.
						sub, subOK, subErr := openPinnedDirDirect(clean)
						if errors.Is(subErr, errRootChanged) {
							release()
							continue
						}
						if subErr != nil {
							release()
							return sharedCatalogLock{}, subErr
						}
						if !lockPathMatchesInfo(targetPath, targetLockInfo) {
							if sub != nil {
								_ = sub.Close()
							}
							release()
							continue
						}
						if subOK {
							dirRoot = sub
						}
					} else {
						release()
						return sharedCatalogLock{}, tpErr
					}
				} else {
					sub, subOK, subErr := openPinnedSubdir(tp, filepath.Base(clean))
					_ = tp.Close()
					if errors.Is(subErr, errRootChanged) {
						release()
						continue
					}
					if subErr != nil {
						release()
						return sharedCatalogLock{}, subErr
					}
					if subOK {
						dirRoot = sub
					}
				}
			} else {
				if tp, tpErr := os.OpenRoot(filepath.Dir(clean)); tpErr == nil {
					sub, subOK, subErr := openPinnedSubdir(tp, filepath.Base(clean))
					_ = tp.Close()
					if errors.Is(subErr, errRootChanged) {
						release()
						continue
					}
					if subErr != nil {
						release()
						return sharedCatalogLock{}, subErr
					}
					if subOK {
						dirRoot = sub
					}
				} else if isReadOnlyOrPermError(tpErr) {
					sub, subOK, subErr := openPinnedDirDirect(clean)
					if errors.Is(subErr, errRootChanged) {
						release()
						continue
					}
					if subErr != nil {
						release()
						return sharedCatalogLock{}, subErr
					}
					if subOK {
						dirRoot = sub
					}
				}
			}
			closeAndRelease := func() {
				if dirRoot != nil {
					_ = dirRoot.Close()
				}
				release()
			}
			if len(missing) > 0 {
				if !targetLocked && !inDirLocked {
					missing = append(missing, inDirPath)
				}
				if appeared, statErr := anyPathExists(missing); statErr != nil {
					closeAndRelease()
					return sharedCatalogLock{}, statErr
				} else if appeared {
					closeAndRelease()
					continue
				}
			}
			if targetChanged() {
				closeAndRelease()
				continue
			}
			return sharedCatalogLock{
				unlock:   closeAndRelease,
				gen:      catalogGen{slot: slotGen, target: targetGen},
				locked:   true,
				missing:  missing,
				dir:      clean,
				resolved: true,
				root:     dirRoot,
			}, nil
		}
		beforeInDirSharedLock(dir)
		info, statErr := os.Stat(clean)
		if statErr != nil || !info.IsDir() {
			if targetChanged() {
				continue
			}
			return sharedCatalogLock{
				unlock:   func() {},
				dir:      clean,
				resolved: true,
			}, nil
		}
		inUnlock, inGen, inInfo, inLocked, inErr := lockFileShared(filepath.Join(clean, ".lock"))
		if inErr != nil {
			return sharedCatalogLock{}, inErr
		}
		if !inLocked {
			var dirRoot *os.Root
			if tp, tpErr := os.OpenRoot(filepath.Dir(clean)); tpErr == nil {
				sub, subOK, subErr := openPinnedSubdir(tp, filepath.Base(clean))
				_ = tp.Close()
				if errors.Is(subErr, errRootChanged) {
					continue
				}
				if subErr != nil {
					return sharedCatalogLock{}, subErr
				}
				if subOK {
					dirRoot = sub
				}
			} else if r, rOK, rErr := openPinnedDirDirect(clean); errors.Is(rErr, errRootChanged) {
				continue
			} else if rErr != nil {
				return sharedCatalogLock{}, rErr
			} else if rOK {
				dirRoot = r
			}
			if exists, extErr := anyPathExists(extPaths); extErr != nil {
				if dirRoot != nil {
					_ = dirRoot.Close()
				}
				return sharedCatalogLock{}, extErr
			} else if exists || targetChanged() {
				if dirRoot != nil {
					_ = dirRoot.Close()
				}
				continue
			}
			return sharedCatalogLock{
				unlock: func() {
					if dirRoot != nil {
						_ = dirRoot.Close()
					}
				},
				dir:      clean,
				resolved: true,
				root:     dirRoot,
			}, nil
		}
		dirRoot, rOK, rErr := openVerifiedRoot(clean, ".lock", inInfo)
		if errors.Is(rErr, errRootChanged) || (rErr == nil && !rOK) {
			inUnlock()
			continue
		}
		if rErr != nil {
			inUnlock()
			return sharedCatalogLock{}, rErr
		}
		closeIn := func() {
			_ = dirRoot.Close()
			inUnlock()
		}
		// Verify no external lock appeared and dir was not retargeted before
		// we acquired the in-dir lock.
		if exists, extErr := anyPathExists(extPaths); extErr != nil {
			closeIn()
			return sharedCatalogLock{}, extErr
		} else if exists || targetChanged() {
			closeIn()
			continue
		}
		gen := catalogGen{target: inGen}
		if slotSameAsTarget {
			gen.slot = inGen
		}
		return sharedCatalogLock{
			unlock:   closeIn,
			gen:      gen,
			locked:   true,
			dir:      clean,
			resolved: true,
			root:     dirRoot,
		}, nil
	}
	return sharedCatalogLock{}, errors.New("catalog locks busy during read; run again")
}

// beforeCatalogSharedLock runs immediately before readCatalogSnapshot acquires
// the shared lock; tests may set it to synchronize concurrent readers with an
// in-progress refresh.
var beforeCatalogSharedLock = func(dir string) {}

// checkCatalogCleanRoot returns an error if root has leftover refresh backup or
// staged files from an incomplete rollback.
func checkCatalogCleanRoot(root *os.Root) error {
	f, err := root.Open(".")
	if err != nil {
		return err
	}
	names, err := f.Readdirnames(-1)
	_ = f.Close()
	if err != nil {
		return err
	}
	for _, name := range names {
		if isRefreshTempFile(name) {
			return errors.New("catalog cache is incomplete after a failed refresh; run 'sciget refresh' to recover")
		}
	}
	return nil
}

func readCatalogSnapshotLocked(dir string) ([]container, []string, error) {
	for range 4 {
		slk, err := lockCatalogDirSharedWithMissing(dir)
		if err != nil {
			return nil, nil, err
		}
		if !slk.locked || !slk.resolved {
			slk.unlock()
			return nil, nil, &notDownloadedError{path: filepath.Join(dir, "apps.json")}
		}
		if slk.root == nil {
			slk.unlock()
			if len(slk.missing) > 0 {
				if appeared, statErr := anyPathExists(slk.missing); statErr == nil && appeared {
					continue
				}
			}
			return nil, nil, &notDownloadedError{path: filepath.Join(slk.dir, "apps.json")}
		}
		if err := checkCatalogCleanRoot(slk.root); err != nil && !errors.Is(err, os.ErrNotExist) {
			slk.unlock()
			if len(slk.missing) > 0 {
				if appeared, statErr := anyPathExists(slk.missing); statErr == nil && appeared {
					continue
				}
			}
			return nil, nil, err
		}
		containers, err := loadCatalogRoot(slk.root, filepath.Join(slk.dir, "apps.json"))
		if err != nil {
			slk.unlock()
			if len(slk.missing) > 0 {
				if appeared, statErr := anyPathExists(slk.missing); statErr == nil && appeared {
					continue
				}
			}
			return nil, nil, err
		}
		mirrors, err := loadMirrorsRoot(slk.root, filepath.Join(slk.dir, "catalog.json"))
		if err != nil {
			slk.unlock()
			if len(slk.missing) > 0 {
				if appeared, statErr := anyPathExists(slk.missing); statErr == nil && appeared {
					continue
				}
			}
			return nil, nil, err
		}
		slk.unlock()
		if len(slk.missing) > 0 {
			if appeared, statErr := anyPathExists(slk.missing); statErr != nil {
				return nil, nil, statErr
			} else if appeared {
				continue
			}
		}
		return containers, mirrors, nil
	}
	return nil, nil, errors.New("catalog locks changed during read; run again")
}

// beforeCatalogSnapshotRead runs after readCatalogSnapshot acquires its shared
// lock and before reading files; tests may set it to synchronize partial-lock
// races.
var beforeCatalogSnapshotRead = func(dir string) {}

// readCatalogSnapshot reads apps.json and catalog.json consistently. When dir
// has a catalog lock file, it holds the shared lock across both reads so an
// in-progress refresh commit, rollback, or removal is never observed. Older
// caches without a lock file are still read unlocked, then re-checked for a
// concurrently created lock so a first refresh cannot slip uncommitted files
// into the read.
func readCatalogSnapshot(dir string) ([]container, []string, error) {
	beforeCatalogSharedLock(dir)
	slk, err := lockCatalogDirSharedWithMissing(dir)
	if err != nil {
		return nil, nil, err
	}
	if slk.locked {
		beforeCatalogSnapshotRead(dir)
		if slk.root == nil {
			slk.unlock()
			if len(slk.missing) > 0 {
				if appeared, statErr := anyPathExists(slk.missing); statErr == nil && appeared {
					return readCatalogSnapshotLocked(dir)
				}
			}
			return nil, nil, &notDownloadedError{path: filepath.Join(slk.dir, "apps.json")}
		}
		if err := checkCatalogCleanRoot(slk.root); err != nil && !errors.Is(err, os.ErrNotExist) {
			slk.unlock()
			if len(slk.missing) > 0 {
				if appeared, statErr := anyPathExists(slk.missing); statErr == nil && appeared {
					return readCatalogSnapshotLocked(dir)
				}
			}
			return nil, nil, err
		}
		containers, err := loadCatalogRoot(slk.root, filepath.Join(slk.dir, "apps.json"))
		if err != nil {
			slk.unlock()
			if len(slk.missing) > 0 {
				if appeared, statErr := anyPathExists(slk.missing); statErr == nil && appeared {
					return readCatalogSnapshotLocked(dir)
				}
			}
			return nil, nil, err
		}
		mirrors, err := loadMirrorsRoot(slk.root, filepath.Join(slk.dir, "catalog.json"))
		if err != nil {
			slk.unlock()
			if len(slk.missing) > 0 {
				if appeared, statErr := anyPathExists(slk.missing); statErr == nil && appeared {
					return readCatalogSnapshotLocked(dir)
				}
			}
			return nil, nil, err
		}
		slk.unlock()
		if len(slk.missing) > 0 {
			if appeared, statErr := anyPathExists(slk.missing); statErr != nil {
				return nil, nil, statErr
			} else if appeared {
				return readCatalogSnapshotLocked(dir)
			}
		}
		return containers, mirrors, nil
	}
	if !slk.resolved || slk.root == nil {
		slk.unlock()
		return nil, nil, &notDownloadedError{path: filepath.Join(dir, "apps.json")}
	}
	unlockedChanged := func() (bool, error) {
		if exists, statErr := catalogLockExists(slk.dir); statErr != nil || exists {
			return exists, statErr
		}
		return catalogLockExists(dir)
	}
	if err := checkCatalogCleanRoot(slk.root); err != nil && !errors.Is(err, os.ErrNotExist) {
		slk.unlock()
		if exists, statErr := unlockedChanged(); statErr == nil && exists {
			return readCatalogSnapshotLocked(dir)
		}
		return nil, nil, err
	}
	containers, err := loadCatalogRoot(slk.root, filepath.Join(slk.dir, "apps.json"))
	if err != nil {
		slk.unlock()
		if exists, statErr := unlockedChanged(); statErr == nil && exists {
			return readCatalogSnapshotLocked(dir)
		}
		return nil, nil, err
	}
	mirrors, err := loadMirrorsRoot(slk.root, filepath.Join(slk.dir, "catalog.json"))
	if err != nil {
		slk.unlock()
		if exists, statErr := unlockedChanged(); statErr == nil && exists {
			return readCatalogSnapshotLocked(dir)
		}
		return nil, nil, err
	}
	slk.unlock()
	if exists, statErr := unlockedChanged(); statErr != nil {
		return nil, nil, statErr
	} else if exists {
		return readCatalogSnapshotLocked(dir)
	}
	return containers, mirrors, nil
}

func loadMirrorsRoot(root *os.Root, displayPath string) ([]string, error) {
	data, err := root.ReadFile("catalog.json")
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("reading catalog: %w", err)
	}
	c, err := parseCatalogJSON(data)
	if err != nil {
		return nil, fmt.Errorf("reading catalog %s: %w", displayPath, err)
	}
	return c.DownloadURLs, nil
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

func loadCatalogRoot(root *os.Root, displayPath string) ([]container, error) {
	data, err := root.ReadFile("apps.json")
	if errors.Is(err, fs.ErrNotExist) {
		return nil, &notDownloadedError{path: displayPath}
	}
	if err != nil {
		return nil, fmt.Errorf("reading catalog: %w", err)
	}
	containers, err := parseCatalog(data)
	if err != nil {
		return nil, fmt.Errorf("reading catalog %s: %w", displayPath, err)
	}
	return containers, nil
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
			texts := slices.Concat([]string{tool, key, app.Image, app.BuildDate, app.Architecture, app.DOI, app.Exec},
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

			arch := containerArch(app.Architecture, tool, toolVersion)
			c := byName[tool+"/"+toolVersion]
			if c == nil {
				c = &container{tool: tool, version: toolVersion, image: image,
					buildDate: app.BuildDate, arch: arch, categories: t.Categories, hidden: true,
					apptainerArgs: app.ApptainerArgs, published: published}
				byName[c.name()] = c
			}
			if arch != "" {
				if c.arch == "" {
					c.arch = arch
				} else if arch != c.arch {
					a, b := c.arch, arch
					if a > b {
						a, b = b, a
					}
					return nil, fmt.Errorf("catalog lists %s with architectures %q and %q",
						c.name(), a, b)
				}
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
