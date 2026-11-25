package main

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"
)

// renameCatalogFile commits a staged catalog file and can be replaced in tests
// to simulate mid-commit rename failures.
var renameCatalogFile = os.Rename

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
	startGens := make([]uint64, len(used))
	startErrs := make([]error, len(used))
	for i, cat := range used {
		startGens[i], startErrs[i] = readCatalogLockGen(cat.dir(cacheDir))
	}
	var results []refreshResult
	for i, cat := range used {
		if startErrs[i] != nil {
			results = append(results, refreshResult{name: cat.Name, err: fmt.Errorf("reading catalog lock: %w", startErrs[i])})
			continue
		}
		dir := cat.dir(cacheDir)
		changed, empty, err := refreshCatalogAt(read, cat, dir, startGens[i], func() (bool, error) {
			return catalogDirRegistered(dir)
		})
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
	startGen, err := readCatalogLockGen(dir)
	if err != nil {
		return false, false, fmt.Errorf("reading catalog lock: %w", err)
	}
	return refreshCatalogAt(read, cat, dir, startGen, nil)
}

func refreshCatalogAt(read func(link string) ([]byte, string, error), cat catalog, dir string, startGen uint64, stillRegistered func() (bool, error)) (changed, empty bool, err error) {
	data, base, err := read(cat.URL)
	if err != nil {
		return false, false, err
	}
	if cat.Domain == "" && isDomainsFile(data) {
		return false, false, fmt.Errorf("%s is a domains file; run 'sciget catalog add' again for it", cat.URL)
	}
	if cat.Domain != "" {
		parts, err := splitDomains(data)
		if err != nil {
			return false, false, fmt.Errorf("%s is not a valid domains file: %w", cat.URL, err)
		}
		var ok bool
		if data, ok = parts[cat.Domain]; !ok {
			return false, false, fmt.Errorf("domain %q is no longer in %s", cat.Domain, cat.URL)
		}
	}
	_, appsLink, err := checkCatalogJSON(data, base)
	if err != nil {
		return false, false, fmt.Errorf("%s is not a valid catalog JSON: %w", cat.URL, err)
	}
	eessiMeta, err := parseCatalogEESSI(data)
	if err != nil {
		return false, false, fmt.Errorf("%s is not a valid catalog JSON: %w", cat.URL, err)
	}
	eessiLink, err := resolveEESSILink(eessiMeta, base)
	if err != nil {
		return false, false, fmt.Errorf("%s is not a valid catalog JSON: %w", cat.URL, err)
	}
	empty = appsLink == "" && eessiLink == ""
	apps := []byte("{}")
	needsEmptyAppsCheck := false
	if appsLink != "" {
		if apps, _, err = read(appsLink); err != nil {
			return false, false, err
		}
		containers, err := parseCatalog(apps)
		if err != nil {
			return false, false, fmt.Errorf("downloaded catalog is invalid: %w", err)
		}
		if len(containers) == 0 {
			if !isJSONObject(apps) {
				return false, false, errors.New("downloaded catalog is invalid: expected a JSON object")
			}
			if eessiLink == "" {
				needsEmptyAppsCheck = true
			}
		}
	}
	var eessiData []byte
	if eessiLink != "" {
		if eessiData, _, err = read(eessiLink); err != nil {
			return false, false, err
		}
		if _, err := parseEESSICatalog(eessiData, eessiMeta.EESSIStacks); err != nil {
			return false, false, fmt.Errorf("downloaded EESSI catalog is invalid: %w", err)
		}
	}
	if needsEmptyAppsCheck {
		// Only accept an empty {} apps.json with no EESSI catalog when dir
		// already caches an empty apps.json (e.g. a retired EESSI-only domain).
		// Check under the directory lock before writing url so a concurrent
		// container refresh cannot be overwritten.
		if _, err := os.Stat(dir); err != nil {
			return false, false, errors.New("downloaded catalog is invalid: no containers")
		}
	}
	lk, err := lockCatalogDir(dir)
	if err != nil {
		return false, false, fmt.Errorf("writing catalog: %w", err)
	}
	defer lk.unlock()
	if lk.gen != startGen {
		return false, false, errors.New("catalog cache changed during refresh; run 'sciget refresh' again")
	}
	if stillRegistered != nil {
		ok, err := stillRegistered()
		if err != nil {
			return false, false, fmt.Errorf("checking catalog registration: %w", err)
		}
		if !ok {
			return false, false, fmt.Errorf("catalog %q was removed during refresh", cat.Name)
		}
	}
	if needsEmptyAppsCheck {
		if !cachedAppsIsEmpty(dir) {
			return false, false, errors.New("downloaded catalog is invalid: no containers")
		}
		empty = true
	} else {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return false, false, fmt.Errorf("writing catalog: %w", err)
		}
	}
	if err := lk.bump(); err != nil {
		return false, false, fmt.Errorf("writing catalog: %w", err)
	}
	// 0o600, as catalogs.json: a query string may carry a token.
	if err := os.WriteFile(filepath.Join(dir, "url"), []byte(cat.URL+"\n"), 0o600); err != nil {
		return false, false, fmt.Errorf("writing catalog: %w", err)
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
				os.Remove(s.tmp)
			}
			if s.bak != "" {
				os.Remove(s.bak)
			}
		}
	}
	for _, file := range files {
		path := filepath.Join(dir, file.name)
		old, oldErr := os.ReadFile(path)
		if oldErr == nil && bytes.Equal(old, file.data) {
			continue
		}
		if oldErr != nil && !errors.Is(oldErr, os.ErrNotExist) {
			cleanupStaged()
			return false, empty, fmt.Errorf("reading existing catalog %s: %w", file.name, oldErr)
		}
		tmpPath, err := stageCatalog(path, file.data, file.mode)
		if err != nil {
			cleanupStaged()
			return false, empty, fmt.Errorf("writing catalog: %w", err)
		}
		var bakPath string
		if oldErr == nil {
			bakPath, err = stageTemp(dir, file.name+".*.bak", old, file.mode)
			if err != nil {
				os.Remove(tmpPath)
				cleanupStaged()
				return false, empty, fmt.Errorf("writing catalog: %w", err)
			}
		}
		staged = append(staged, stagedFile{tmp: tmpPath, bak: bakPath, dest: path})
	}

	var staleEESSIBak string
	eessiPath := filepath.Join(dir, "eessi.json")
	if eessiLink == "" {
		if _, err := os.Lstat(eessiPath); err == nil {
			f, err := os.CreateTemp(dir, "eessi.json.*.bak")
			if err != nil {
				cleanupStaged()
				return false, empty, fmt.Errorf("removing stale EESSI catalog: %w", err)
			}
			staleEESSIBak = f.Name()
			f.Close()
			if err := os.Rename(eessiPath, staleEESSIBak); err != nil {
				os.Remove(staleEESSIBak)
				cleanupStaged()
				return false, empty, fmt.Errorf("removing stale EESSI catalog: %w", err)
			}
		} else if !errors.Is(err, os.ErrNotExist) {
			cleanupStaged()
			return false, empty, fmt.Errorf("removing stale EESSI catalog: %w", err)
		}
	}

	for i, s := range staged {
		if err := renameCatalogFile(s.tmp, s.dest); err != nil {
			// Roll back every file already renamed in this refresh; preserve
			// any backup whose restoration fails and report rollback errors.
			var rollbackErrs []error
			for j := i - 1; j >= 0; j-- {
				if staged[j].bak != "" {
					if rerr := os.Rename(staged[j].bak, staged[j].dest); rerr != nil {
						rollbackErrs = append(rollbackErrs, fmt.Errorf("restoring %s (backup kept at %s): %w", filepath.Base(staged[j].dest), staged[j].bak, rerr))
					}
					staged[j].bak = ""
				} else {
					if rerr := os.Remove(staged[j].dest); rerr != nil && !errors.Is(rerr, os.ErrNotExist) {
						rollbackErrs = append(rollbackErrs, fmt.Errorf("removing %s: %w", filepath.Base(staged[j].dest), rerr))
					}
				}
			}
			if staleEESSIBak != "" {
				if rerr := os.Rename(staleEESSIBak, eessiPath); rerr != nil {
					rollbackErrs = append(rollbackErrs, fmt.Errorf("restoring eessi.json (backup kept at %s): %w", staleEESSIBak, rerr))
				}
				staleEESSIBak = ""
			}
			if len(rollbackErrs) == 0 {
				for j := i; j < len(staged); j++ {
					os.Remove(staged[j].tmp)
					staged[j].tmp = ""
				}
				cleanupStaged()
			}
			commitErr := fmt.Errorf("writing catalog: %w", err)
			if len(rollbackErrs) > 0 {
				return false, empty, errors.Join(commitErr, fmt.Errorf("rollback incomplete (run 'sciget refresh' to recover): %w", errors.Join(rollbackErrs...)))
			}
			return false, empty, commitErr
		}
		staged[i].tmp = ""
		changed = true
	}
	if staleEESSIBak != "" {
		os.Remove(staleEESSIBak)
		changed = true
	}
	cleanupStaged()
	entries, err := os.ReadDir(dir)
	if err != nil {
		return changed, empty, fmt.Errorf("cleaning catalog backups: %w", err)
	}
	for _, entry := range entries {
		name := entry.Name()
		if strings.HasSuffix(name, ".bak") || strings.HasSuffix(name, ".incomplete") {
			if err := os.Remove(filepath.Join(dir, name)); err != nil && !errors.Is(err, os.ErrNotExist) {
				return changed, empty, fmt.Errorf("removing stale catalog backup %s: %w", name, err)
			}
		}
	}
	return changed, empty, nil
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

func isJSONObject(data []byte) bool {
	trimmed := bytes.TrimSpace(data)
	return len(trimmed) > 0 && trimmed[0] == '{'
}

// cachedAppsIsEmpty reports whether dir already caches a valid empty apps.json
// (zero containers). Must be called while holding lockCatalogDir(dir).
func cachedAppsIsEmpty(dir string) bool {
	appsData, err := os.ReadFile(filepath.Join(dir, "apps.json"))
	if err != nil || !isJSONObject(appsData) {
		return false
	}
	containers, err := parseCatalog(appsData)
	return err == nil && len(containers) == 0
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
