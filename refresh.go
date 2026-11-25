package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"sync/atomic"
	"time"

	"github.com/spf13/cobra"
)

// renameCatalogFileRoot commits a staged catalog file inside root and can be
// replaced in tests to simulate mid-commit rename failures.
var renameCatalogFileRoot = func(root *os.Root, dir, oldName, newName string) error {
	return root.Rename(oldName, newName)
}

var tempFileSeq atomic.Uint64

func newRefreshCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "refresh",
		Short: "Download the latest catalogs in use",
		Args:  noArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			results, err := refreshCatalogs()
			if err != nil {
				return err
			}
			var failures []error
			for _, r := range results {
				if r.err != nil {
					failures = append(failures, fmt.Errorf("catalog %q: %w", r.name, r.err))
					continue
				}
				result := "is up to date."
				if r.changed {
					result = "updated."
				}
				if r.empty {
					result += " It has no apps list yet."
				}
				if _, err := fmt.Fprintf(cmd.OutOrStdout(), "Catalog %q %s\n", r.name, result); err != nil {
					return err
				}
			}
			if len(failures) > 0 {
				return errors.Join(failures...)
			}
			cfg, err := readConfig()
			if err != nil {
				return err
			}
			outdated, err := outdatedModules(cfg)
			if err != nil {
				return err
			}
			for _, name := range outdated {
				if _, err := fmt.Fprintf(cmd.OutOrStdout(), "Module %s is from an older build; to update it, run: %s\n",
					printable(name), shellJoin([]string{"sciget", "install", name})); err != nil {
					return err
				}
			}
			return nil
		},
	}
}

// refreshResult is one catalog's refresh: whether its copy changed and
// whether it is an empty catalog, or why it failed.
type refreshResult struct {
	name    string
	changed bool
	empty   bool
	err     error
}

// refreshCatalogs downloads every catalog in use, in order, with a
// client of network_timeout_seconds. One catalog failing does not stop
// the others. sciget refresh and the TUI both use it.
func refreshCatalogs() ([]refreshResult, error) {
	cacheDir, used, err := usedCatalogs()
	if err != nil {
		return nil, err
	}
	cfg, err := readConfig()
	if err != nil {
		return nil, err
	}
	maxSize, err := cfg.getNumber("max_catalog_size")
	if err != nil {
		return nil, err
	}
	client, err := newHTTPClient(cfg)
	if err != nil {
		return nil, err
	}
	client.CheckRedirect = httpsOnly // as the catalog URL itself must be
	read := onceReader(client, maxSize)
	startGens := make([]catalogGen, len(used))
	startDirs := make([]string, len(used))
	startResolved := make([]bool, len(used))
	startErrs := make([]error, len(used))
	dirs := make([]string, len(used))
	slotPaths := make([]string, len(used))
	targetPaths := make([]string, len(used))
	for i, cat := range used {
		dirs[i] = cat.dir(cacheDir)
		slotPaths[i] = catalogLockPath(dirs[i])
		startGens[i], startDirs[i], startResolved[i], startErrs[i] = readCatalogLockState(dirs[i])
		if startResolved[i] && startDirs[i] != "" {
			targetPaths[i] = catalogTargetLockPathForClean(startDirs[i])
		} else {
			targetPaths[i] = catalogTargetLockPath(dirs[i])
		}
	}
	var results []refreshResult
	for i, cat := range used {
		if startErrs[i] != nil {
			results = append(results, refreshResult{name: cat.Name, err: fmt.Errorf("reading catalog lock: %w", startErrs[i])})
			continue
		}
		dir := dirs[i]
		changed, empty, newGen, bumped, err := refreshCatalogAtGen(read, cat, dir, startGens[i], startDirs[i], startResolved[i], true, func() (bool, error) {
			return catalogDirRegistered(dir)
		})
		if bumped {
			// Advance the expected slot/target generations for later catalogs
			// in this same refresh pass that share a bumped lock — even when
			// this catalog rolled back after bumping — so a single
			// 'sciget refresh' does not conflict with its own earlier bump.
			for j := i + 1; j < len(used); j++ {
				if sameLockFile(slotPaths[j], slotPaths[i]) && newGen.slot > startGens[j].slot {
					startGens[j].slot = newGen.slot
				}
				if sameLockFile(slotPaths[j], targetPaths[i]) && newGen.target > startGens[j].slot {
					startGens[j].slot = newGen.target
				}
				if sameLockFile(targetPaths[j], slotPaths[i]) && newGen.slot > startGens[j].target {
					startGens[j].target = newGen.slot
				}
				if sameLockFile(targetPaths[j], targetPaths[i]) && newGen.target > startGens[j].target {
					startGens[j].target = newGen.target
				}
			}
		}
		results = append(results, refreshResult{name: cat.Name, changed: changed, empty: empty, err: err})
	}
	return results, nil
}

// newHTTPClient returns a client whose requests time out after
// network_timeout_seconds in total.
func newHTTPClient(cfg config) (*http.Client, error) {
	seconds, err := cfg.getNumber("network_timeout_seconds")
	if err != nil {
		return nil, err
	}
	return &http.Client{Timeout: time.Duration(seconds) * time.Second}, nil
}

// refreshCatalog reads cat's catalog JSON with read (taking its domain
// from a domains file), then its apps list, and if either differs from the
// copy in dir, replaces it there: apps.json, then catalog.json. An empty
// catalog's apps list is {}. It reports whether it replaced either and
// whether the catalog is empty. Any failure leaves the copy as it was.
// Each success also writes the URL to dir's url file.
func refreshCatalog(read func(link string) ([]byte, string, error), cat catalog, dir string) (changed, empty bool, err error) {
	startGen, startDir, startResolved, err := readCatalogLockState(dir)
	if err != nil {
		return false, false, fmt.Errorf("reading catalog lock: %w", err)
	}
	changed, empty, _, _, err = refreshCatalogAtGen(read, cat, dir, startGen, startDir, startResolved, true, nil)
	return changed, empty, err
}

func refreshCatalogAt(read func(link string) ([]byte, string, error), cat catalog, dir string, startGen catalogGen, stillRegistered func() (bool, error)) (changed, empty bool, err error) {
	changed, empty, _, _, err = refreshCatalogAtGen(read, cat, dir, startGen, "", false, false, stillRegistered)
	return changed, empty, err
}

func refreshCatalogAtGen(read func(link string) ([]byte, string, error), cat catalog, dir string, startGen catalogGen, startDir string, startResolved bool, checkStartDir bool, stillRegistered func() (bool, error)) (changed, empty bool, newGen catalogGen, bumped bool, err error) {
	data, base, err := read(cat.URL)
	if err != nil {
		return false, false, catalogGen{}, false, err
	}
	if cat.Domain == "" && isDomainsFile(data) {
		return false, false, catalogGen{}, false, fmt.Errorf("%s is a domains file; run 'sciget catalog add' again for it", cat.URL)
	}
	if cat.Domain != "" {
		parts, err := splitDomains(data)
		if err != nil {
			return false, false, catalogGen{}, false, fmt.Errorf("%s is not a valid domains file: %w", cat.URL, err)
		}
		var ok bool
		if data, ok = parts[cat.Domain]; !ok {
			return false, false, catalogGen{}, false, fmt.Errorf("domain %q is no longer in %s", cat.Domain, cat.URL)
		}
	}
	_, appsLink, err := checkCatalogJSON(data, base)
	if err != nil {
		return false, false, catalogGen{}, false, fmt.Errorf("%s is not a valid catalog JSON: %w", cat.URL, err)
	}
	eessiMeta, err := parseCatalogEESSI(data)
	if err != nil {
		return false, false, catalogGen{}, false, fmt.Errorf("%s is not a valid catalog JSON: %w", cat.URL, err)
	}
	eessiLink, err := resolveEESSILink(eessiMeta, base)
	if err != nil {
		return false, false, catalogGen{}, false, fmt.Errorf("%s is not a valid catalog JSON: %w", cat.URL, err)
	}
	empty = appsLink == "" && eessiLink == ""
	apps := []byte("{}")
	needsEmptyAppsCheck := false
	if appsLink != "" {
		if apps, _, err = read(appsLink); err != nil {
			return false, false, catalogGen{}, false, err
		}
		containers, err := parseCatalog(apps)
		if err != nil {
			return false, false, catalogGen{}, false, fmt.Errorf("downloaded catalog is invalid: %w", err)
		}
		if len(containers) == 0 {
			if !isEmptyJSONObject(apps) {
				return false, false, catalogGen{}, false, errors.New("downloaded catalog is invalid: no containers")
			}
			if eessiLink == "" {
				needsEmptyAppsCheck = true
			}
		}
	}
	var eessiData []byte
	if eessiLink != "" {
		if eessiData, _, err = read(eessiLink); err != nil {
			return false, false, catalogGen{}, false, err
		}
		if _, err := parseEESSICatalog(eessiData, eessiMeta.EESSIStacks); err != nil {
			return false, false, catalogGen{}, false, fmt.Errorf("downloaded EESSI catalog is invalid: %w", err)
		}
	}
	if needsEmptyAppsCheck {
		// Only accept an empty {} apps.json with no EESSI catalog when dir
		// already caches an empty apps.json (e.g. a retired EESSI-only domain).
		// Check under the directory lock before writing url so a concurrent
		// container refresh cannot be overwritten.
		if _, err := os.Stat(dir); err != nil {
			return false, false, catalogGen{}, false, errors.New("downloaded catalog is invalid: no containers")
		}
	}
	lk, err := lockCatalogDir(dir)
	if err != nil {
		return false, false, catalogGen{}, false, fmt.Errorf("writing catalog: %w", err)
	}
	defer lk.unlock()
	ioDir := lk.dir
	if ioDir == "" {
		return false, false, catalogGen{}, false, errors.New("catalog cache changed during refresh; run 'sciget refresh' again")
	}
	if lk.gen != startGen || (checkStartDir && (!startResolved || ioDir != startDir)) {
		return false, false, catalogGen{}, false, errors.New("catalog cache changed during refresh; run 'sciget refresh' again")
	}
	if stillRegistered != nil {
		ok, err := stillRegistered()
		if err != nil {
			return false, false, catalogGen{}, false, fmt.Errorf("checking catalog registration: %w", err)
		}
		if !ok {
			return false, false, catalogGen{}, false, fmt.Errorf("catalog %q was removed during refresh", cat.Name)
		}
	}
	var dirRoot *os.Root
	var closeDirRoot bool
	if lk.targetRoot != nil {
		dirRoot = lk.targetRoot
	} else {
		if lk.targetParent == nil {
			return false, false, catalogGen{}, false, errors.New("catalog cache changed during refresh; run 'sciget refresh' again")
		}
		if !needsEmptyAppsCheck {
			if err := lk.targetParent.Mkdir(lk.targetBase, 0o755); err != nil && !errors.Is(err, os.ErrExist) {
				return false, false, catalogGen{}, false, fmt.Errorf("writing catalog: %w", err)
			}
		}
		sub, subOK, subErr := openPinnedSubdir(lk.targetParent, lk.targetBase)
		if errors.Is(subErr, errRootChanged) || (subErr == nil && !subOK) {
			return false, false, catalogGen{}, false, errors.New("catalog cache changed during refresh; run 'sciget refresh' again")
		}
		if subErr != nil {
			return false, false, catalogGen{}, false, fmt.Errorf("writing catalog: %w", subErr)
		}
		dirRoot = sub
		closeDirRoot = true
	}
	if closeDirRoot {
		defer dirRoot.Close()
	}
	if needsEmptyAppsCheck {
		if !cachedAppsIsEmptyRoot(dirRoot) && !cachedHasEESSIRoot(dirRoot) {
			return false, false, catalogGen{}, false, errors.New("downloaded catalog is invalid: no containers")
		}
		empty = true
	}
	if err := lk.bump(); err != nil {
		return false, false, catalogGen{}, false, fmt.Errorf("writing catalog: %w", err)
	}
	newGen = lk.gen
	bumped = true
	// 0o600, as catalogs.json: a query string may carry a token.
	if err := dirRoot.WriteFile("url", []byte(cat.URL+"\n"), 0o600); err != nil {
		return false, false, newGen, true, fmt.Errorf("writing catalog: %w", err)
	}
	// Stage every changed file and back up replaced files first so a mid-commit
	// rename failure rolls back earlier renames instead of leaving a mixed cache.
	type pendingFile struct {
		name string
		data []byte
		mode os.FileMode
	}
	files := []pendingFile{{"apps.json", apps, 0o644}}
	if eessiLink != "" {
		files = append(files, pendingFile{"eessi.json", eessiData, 0o644})
	}
	files = append(files, pendingFile{"catalog.json", data, 0o600})

	type stagedFile struct {
		tmp  string
		bak  string
		dest string
	}
	var staged []stagedFile
	cleanupStaged := func() {
		for _, s := range staged {
			if s.tmp != "" {
				_ = dirRoot.Remove(s.tmp)
			}
			if s.bak != "" {
				_ = dirRoot.Remove(s.bak)
			}
		}
	}
	for _, file := range files {
		old, oldErr := dirRoot.ReadFile(file.name)
		if oldErr == nil && bytes.Equal(old, file.data) {
			continue
		}
		if oldErr != nil && !errors.Is(oldErr, os.ErrNotExist) {
			cleanupStaged()
			return false, empty, newGen, true, fmt.Errorf("reading existing catalog %s: %w", file.name, oldErr)
		}
		tmpName, err := stageTempRoot(dirRoot, file.name+".", ".incomplete", file.data, file.mode)
		if err != nil {
			cleanupStaged()
			return false, empty, newGen, true, fmt.Errorf("writing catalog: %w", err)
		}
		var bakName string
		if oldErr == nil {
			bakName, err = stageTempRoot(dirRoot, file.name+".", ".bak", old, file.mode)
			if err != nil {
				_ = dirRoot.Remove(tmpName)
				cleanupStaged()
				return false, empty, newGen, true, fmt.Errorf("writing catalog: %w", err)
			}
		}
		staged = append(staged, stagedFile{tmp: tmpName, bak: bakName, dest: file.name})
	}

	var staleEESSIBak string
	if eessiLink == "" {
		if _, err := dirRoot.Lstat("eessi.json"); err == nil {
			f, name, err := createTempRoot(dirRoot, "eessi.json.", ".bak", 0o644)
			if err != nil {
				cleanupStaged()
				return false, empty, newGen, true, fmt.Errorf("removing stale EESSI catalog: %w", err)
			}
			_ = f.Close()
			staleEESSIBak = name
			if err := dirRoot.Rename("eessi.json", staleEESSIBak); err != nil {
				_ = dirRoot.Remove(staleEESSIBak)
				cleanupStaged()
				return false, empty, newGen, true, fmt.Errorf("removing stale EESSI catalog: %w", err)
			}
		} else if !errors.Is(err, os.ErrNotExist) {
			cleanupStaged()
			return false, empty, newGen, true, fmt.Errorf("removing stale EESSI catalog: %w", err)
		}
	}

	for i, s := range staged {
		if err := renameCatalogFileRoot(dirRoot, ioDir, s.tmp, s.dest); err != nil {
			// Roll back every file already renamed in this refresh; preserve
			// any backup whose restoration fails and report rollback errors.
			var rollbackErrs []error
			for j := i - 1; j >= 0; j-- {
				if staged[j].bak != "" {
					if rerr := dirRoot.Rename(staged[j].bak, staged[j].dest); rerr != nil {
						rollbackErrs = append(rollbackErrs, fmt.Errorf("restoring %s (backup kept at %s): %w", staged[j].dest, filepath.Join(ioDir, staged[j].bak), rerr))
					}
					staged[j].bak = ""
				} else {
					if rerr := dirRoot.Remove(staged[j].dest); rerr != nil && !errors.Is(rerr, os.ErrNotExist) {
						rollbackErrs = append(rollbackErrs, fmt.Errorf("removing %s: %w", staged[j].dest, rerr))
					}
				}
			}
			if staleEESSIBak != "" {
				if rerr := dirRoot.Rename(staleEESSIBak, "eessi.json"); rerr != nil {
					rollbackErrs = append(rollbackErrs, fmt.Errorf("restoring eessi.json (backup kept at %s): %w", filepath.Join(ioDir, staleEESSIBak), rerr))
				}
				staleEESSIBak = ""
			}
			if len(rollbackErrs) == 0 {
				for j := i; j < len(staged); j++ {
					_ = dirRoot.Remove(staged[j].tmp)
					staged[j].tmp = ""
				}
				cleanupStaged()
			}
			commitErr := fmt.Errorf("writing catalog: %w", err)
			if len(rollbackErrs) > 0 {
				return false, empty, newGen, true, errors.Join(commitErr, fmt.Errorf("rollback incomplete (run 'sciget refresh' to recover): %w", errors.Join(rollbackErrs...)))
			}
			return false, empty, newGen, true, commitErr
		}
		staged[i].tmp = ""
		changed = true
	}
	if staleEESSIBak != "" {
		_ = dirRoot.Remove(staleEESSIBak)
		changed = true
	}
	cleanupStaged()
	df, err := dirRoot.Open(".")
	if err != nil {
		return changed, empty, newGen, true, fmt.Errorf("cleaning catalog backups: %w", err)
	}
	names, err := df.Readdirnames(-1)
	_ = df.Close()
	if err != nil {
		return changed, empty, newGen, true, fmt.Errorf("cleaning catalog backups: %w", err)
	}
	for _, name := range names {
		if isRefreshTempFile(name) {
			if err := dirRoot.Remove(name); err != nil && !errors.Is(err, os.ErrNotExist) {
				return changed, empty, newGen, true, fmt.Errorf("removing stale catalog backup %s: %w", name, err)
			}
		}
	}
	return changed, empty, newGen, true, nil
}

// downloadCatalog returns the body of a GET of catalogURL, which must
// answer 200 with at most maxSize bytes, so a catalog URL that names some
// other, larger file fails before it fills memory. It also returns the URL
// the body came from, after any redirects.
func downloadCatalog(client *http.Client, catalogURL string, maxSize int64) ([]byte, string, error) {
	req, err := http.NewRequest(http.MethodGet, catalogURL, nil)
	if err != nil {
		return nil, "", fmt.Errorf("downloading catalog: %w", err)
	}
	req.Header.Set("User-Agent", "sciget/"+version)
	resp, err := client.Do(req)
	if err != nil {
		return nil, "", fmt.Errorf("downloading catalog: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, "", fmt.Errorf("downloading catalog: %s returned %s", catalogURL, resp.Status)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxSize+1))
	if err != nil {
		return nil, "", fmt.Errorf("downloading catalog: %w", err)
	}
	if int64(len(data)) > maxSize {
		return nil, "", fmt.Errorf("downloading catalog: %s is larger than max_catalog_size (%d bytes)",
			catalogURL, maxSize)
	}
	return data, resp.Request.URL.String(), nil
}

// isEmptyJSONObject reports whether data is an empty JSON object ({}),
// rejecting null, arrays, or non-empty objects whose tools had null/empty apps.
func isEmptyJSONObject(data []byte) bool {
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) == 0 || trimmed[0] != '{' {
		return false
	}
	var m map[string]json.RawMessage
	return json.Unmarshal(trimmed, &m) == nil && m != nil && len(m) == 0
}

// cachedAppsIsEmptyRoot reports whether root already caches a valid empty
// apps.json ({}, zero containers). Must be called while holding lockCatalogDir.
func cachedAppsIsEmptyRoot(root *os.Root) bool {
	appsData, err := root.ReadFile("apps.json")
	if err != nil || !isEmptyJSONObject(appsData) {
		return false
	}
	containers, err := parseCatalog(appsData)
	return err == nil && len(containers) == 0
}

// cachedHasEESSIRoot reports whether root currently caches an EESSI catalog
// (eessi.json on disk or eessiCatalogUrl in catalog.json), so retiring EESSI
// and the last container together removes stale eessi.json cleanly.
func cachedHasEESSIRoot(root *os.Root) bool {
	if _, err := root.Lstat("eessi.json"); err == nil {
		return true
	}
	catData, err := root.ReadFile("catalog.json")
	if err != nil {
		return false
	}
	meta, err := parseCatalogEESSI(catData)
	return err == nil && meta.EESSICatalogURL != ""
}

func createTempRoot(root *os.Root, prefix, suffix string, mode os.FileMode) (*os.File, string, error) {
	base := uint64(time.Now().UnixNano())
	for range 1000 {
		n := base + tempFileSeq.Add(1)
		name := prefix + strconv.FormatUint(n, 10) + suffix
		f, err := root.OpenFile(name, os.O_RDWR|os.O_CREATE|os.O_EXCL, mode)
		if errors.Is(err, os.ErrExist) {
			continue
		}
		if err != nil {
			return nil, "", err
		}
		return f, name, nil
	}
	return nil, "", errors.New("could not create temporary catalog file")
}

func stageTempRoot(root *os.Root, prefix, suffix string, data []byte, mode os.FileMode) (string, error) {
	f, name, err := createTempRoot(root, prefix, suffix, mode)
	if err != nil {
		return "", err
	}
	_, err = f.Write(data)
	if err == nil {
		err = f.Chmod(mode)
	}
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		_ = root.Remove(name)
		return "", err
	}
	return name, nil
}

// stageTemp writes data, with mode, to a new temporary file in dir matching
// pattern and returns its path.
func stageTemp(dir, pattern string, data []byte, mode os.FileMode) (string, error) {
	f, err := os.CreateTemp(dir, pattern)
	if err != nil {
		return "", err
	}
	_, err = f.Write(data)
	if err == nil {
		err = f.Chmod(mode)
	}
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		os.Remove(f.Name())
		return "", err
	}
	return f.Name(), nil
}

// stageCatalog writes data, with mode, to a new temporary file beside path
// and returns its path ready to be renamed to path.
func stageCatalog(path string, data []byte, mode os.FileMode) (string, error) {
	return stageTemp(filepath.Dir(path), filepath.Base(path)+".*.incomplete", data, mode)
}

// writeCatalog writes data, with mode, to a new temporary file beside path
// and renames it to path, so readers see the old file or the new one, never a
// partial write, and two refreshes at once never share a file. The
// directory must exist.
func writeCatalog(path string, data []byte, mode os.FileMode) error {
	tmp, err := stageCatalog(path, data, mode)
	if err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return err
	}
	return nil
}
