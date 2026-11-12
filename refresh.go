package main

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"
)

func newRefreshCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "refresh",
		Short: "Download the latest catalogues in use",
		Args:  noArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			cacheDir, used, err := usedCatalogues()
			if err != nil {
				return err
			}
			client := &http.Client{}
			var failures []error
			for _, cat := range used {
				changed, err := refreshCatalogue(client, cat.URL, cat.dir(cacheDir))
				if err != nil {
					failures = append(failures, fmt.Errorf("catalogue %q: %w", cat.Name, err))
					continue
				}
				result := "is up to date."
				if changed {
					result = "updated."
				}
				if _, err := fmt.Fprintf(cmd.OutOrStdout(), "Catalogue %q %s\n", cat.Name, result); err != nil {
					return err
				}
			}
			return errors.Join(failures...)
		},
	}
}

// refreshCatalogue downloads the catalogue at catalogueURL and, if it differs
// from apps.json in dir, replaces that file. It reports whether it did. Each
// success also writes the URL to dir's url file.
func refreshCatalogue(client *http.Client, catalogueURL, dir string) (bool, error) {
	data, err := downloadCatalogue(client, catalogueURL)
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

// maxCatalogueSize bounds a downloaded catalogue, so a catalogue URL that
// names some other, larger file fails before it fills memory.
const maxCatalogueSize = 64 << 20

// downloadCatalogue returns the body of a GET of catalogueURL, which must
// answer 200 with at most maxCatalogueSize bytes.
func downloadCatalogue(client *http.Client, catalogueURL string) ([]byte, error) {
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
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxCatalogueSize+1))
	if err != nil {
		return nil, fmt.Errorf("downloading catalogue: %w", err)
	}
	if len(data) > maxCatalogueSize {
		return nil, fmt.Errorf("downloading catalogue: %s is larger than %d MB",
			catalogueURL, maxCatalogueSize>>20)
	}
	return data, nil
}

// writeCatalogue writes data to path.incomplete and renames it to path, so
// readers see the old file or the new one, never a partial write in progress.
// The directory must exist.
func writeCatalogue(path string, data []byte) error {
	incomplete := path + ".incomplete"
	f, err := os.OpenFile(incomplete, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	_, err = f.Write(data)
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	return os.Rename(incomplete, path)
}
