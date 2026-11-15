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
		Short: "Download the latest catalogues in use",
		Args:  noArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			results, err := refreshCatalogues()
			if err != nil {
				return err
			}
			var failures []error
			for _, r := range results {
				if r.err != nil {
					failures = append(failures, fmt.Errorf("catalogue %q: %w", r.name, r.err))
					continue
				}
				result := "is up to date."
				if r.changed {
					result = "updated."
				}
				if _, err := fmt.Fprintf(cmd.OutOrStdout(), "Catalogue %q %s\n", r.name, result); err != nil {
					return err
				}
			}
			return errors.Join(failures...)
		},
	}
}

// refreshResult is one catalogue's refresh: whether its copy changed, or
// why it failed.
type refreshResult struct {
	name    string
	changed bool
	err     error
}

// refreshCatalogues downloads every catalogue in use, in order, with a
// client of network_timeout_seconds. One catalogue failing does not stop
// the others. sciget refresh and the TUI both use it.
func refreshCatalogues() ([]refreshResult, error) {
	cacheDir, used, err := usedCatalogues()
	if err != nil {
		return nil, err
	}
	cfg, err := readConfig()
	if err != nil {
		return nil, err
	}
	maxSize, err := cfg.getNumber("max_catalogue_size")
	if err != nil {
		return nil, err
	}
	client, err := newHTTPClient(cfg)
	if err != nil {
		return nil, err
	}
	client.CheckRedirect = httpsOnly // as the catalogue URL itself must be
	var results []refreshResult
	for _, cat := range used {
		changed, err := refreshCatalogue(client, cat.URL, cat.dir(cacheDir), maxSize)
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

// refreshCatalogue downloads the catalogue at catalogueURL and, if it differs
// from apps.json in dir, replaces that file. It reports whether it did. Each
// success also writes the URL to dir's url file. A catalogue larger than
// maxSize bytes is an error.
func refreshCatalogue(client *http.Client, catalogueURL, dir string, maxSize int64) (bool, error) {
	data, err := downloadCatalogue(client, catalogueURL, maxSize)
	if err != nil {
		return false, err
	}
	if _, err := parseCatalogue(data); err != nil {
		return false, fmt.Errorf("downloaded catalogue is invalid: %w", err)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return false, fmt.Errorf("writing catalogue: %w", err)
	}
	// 0o600, as catalogues.json: a query string may carry a token.
	if err := os.WriteFile(filepath.Join(dir, "url"), []byte(catalogueURL+"\n"), 0o600); err != nil {
		return false, fmt.Errorf("writing catalogue: %w", err)
	}
	path := filepath.Join(dir, "apps.json")
	if old, err := os.ReadFile(path); err == nil && bytes.Equal(old, data) {
		return false, nil
	}
	if err := writeCatalogue(path, data); err != nil {
		return false, fmt.Errorf("writing catalogue: %w", err)
	}
	return true, nil
}

// downloadCatalogue returns the body of a GET of catalogueURL, which must
// answer 200 with at most maxSize bytes, so a catalogue URL that names some
// other, larger file fails before it fills memory.
func downloadCatalogue(client *http.Client, catalogueURL string, maxSize int64) ([]byte, error) {
	req, err := http.NewRequest(http.MethodGet, catalogueURL, nil)
	if err != nil {
		return nil, fmt.Errorf("downloading catalogue: %w", err)
	}
	req.Header.Set("User-Agent", "sciget/"+version)
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("downloading catalogue: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("downloading catalogue: %s returned %s", catalogueURL, resp.Status)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxSize+1))
	if err != nil {
		return nil, fmt.Errorf("downloading catalogue: %w", err)
	}
	if int64(len(data)) > maxSize {
		return nil, fmt.Errorf("downloading catalogue: %s is larger than max_catalogue_size (%d bytes)",
			catalogueURL, maxSize)
	}
	return data, nil
}

// writeCatalogue writes data to a new temporary file beside path and
// renames it to path, so readers see the old file or the new one, never a
// partial write, and two refreshes at once never share a file. The
// directory must exist.
func writeCatalogue(path string, data []byte) error {
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
