package main

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/spf13/cobra"
)

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

// refreshResult is one catalog's refresh: whether its copy changed, or
// why it failed.
type refreshResult struct {
	name    string
	changed bool
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
	var results []refreshResult
	for _, cat := range used {
		changed, err := refreshCatalog(client, cat.URL, cat.dir(cacheDir), maxSize)
		results = append(results, refreshResult{name: cat.Name, changed: changed, err: err})
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

// refreshCatalog downloads the catalog at catalogURL and, if it differs
// from apps.json in dir, replaces that file. It reports whether it did. Each
// success also writes the URL to dir's url file. A catalog larger than
// maxSize bytes is an error.
func refreshCatalog(client *http.Client, catalogURL, dir string, maxSize int64) (bool, error) {
	data, err := downloadCatalog(client, catalogURL, maxSize)
	if err != nil {
		return false, err
	}
	if _, err := parseCatalog(data); err != nil {
		return false, fmt.Errorf("downloaded catalog is invalid: %w", err)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return false, fmt.Errorf("writing catalog: %w", err)
	}
	// 0o600, as catalogs.json: a query string may carry a token.
	if err := os.WriteFile(filepath.Join(dir, "url"), []byte(catalogURL+"\n"), 0o600); err != nil {
		return false, fmt.Errorf("writing catalog: %w", err)
	}
	path := filepath.Join(dir, "apps.json")
	if old, err := os.ReadFile(path); err == nil && bytes.Equal(old, data) {
		return false, nil
	}
	if err := writeCatalog(path, data); err != nil {
		return false, fmt.Errorf("writing catalog: %w", err)
	}
	return true, nil
}

// downloadCatalog returns the body of a GET of catalogURL, which must
// answer 200 with at most maxSize bytes, so a catalog URL that names some
// other, larger file fails before it fills memory.
func downloadCatalog(client *http.Client, catalogURL string, maxSize int64) ([]byte, error) {
	req, err := http.NewRequest(http.MethodGet, catalogURL, nil)
	if err != nil {
		return nil, fmt.Errorf("downloading catalog: %w", err)
	}
	req.Header.Set("User-Agent", "sciget/"+version)
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("downloading catalog: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("downloading catalog: %s returned %s", catalogURL, resp.Status)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxSize+1))
	if err != nil {
		return nil, fmt.Errorf("downloading catalog: %w", err)
	}
	if int64(len(data)) > maxSize {
		return nil, fmt.Errorf("downloading catalog: %s is larger than max_catalog_size (%d bytes)",
			catalogURL, maxSize)
	}
	return data, nil
}

// writeCatalog writes data to a new temporary file beside path and
// renames it to path, so readers see the old file or the new one, never a
// partial write, and two refreshes at once never share a file. The
// directory must exist.
func writeCatalog(path string, data []byte) error {
	f, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".*.incomplete")
	if err != nil {
		return err
	}
	_, err = f.Write(data)
	if err == nil {
		err = f.Chmod(0o644)
	}
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	if err == nil {
		err = os.Rename(f.Name(), path)
	}
	if err != nil {
		os.Remove(f.Name())
	}
	return err
}
