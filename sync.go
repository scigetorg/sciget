package main

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"
)

func newSyncCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "sync",
		Short: "Download the latest catalogue",
		Args:  noArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := readConfig()
			if err != nil {
				return err
			}
			catalogueURL, _, err := c.get("catalogue_url")
			if err != nil {
				return err
			}
			dir, _, err := c.get("cache_dir")
			if err != nil {
				return err
			}
			changed, err := syncCatalogue(&http.Client{}, catalogueURL, dir)
			if err != nil {
				return err
			}
			message := "Already up-to-date."
			if changed {
				message = "Updated catalogue."
			}
			_, err = fmt.Fprintln(cmd.OutOrStdout(), message)
			return err
		},
	}
}

// syncCatalogue downloads the catalogue at catalogueURL and, if it differs
// from apps.json in dir, replaces that file. It reports whether it did.
func syncCatalogue(client *http.Client, catalogueURL, dir string) (bool, error) {
	data, err := downloadCatalogue(client, catalogueURL)
	if err != nil {
		return false, err
	}
	if _, err := parseCatalogue(data); err != nil {
		return false, fmt.Errorf("downloaded catalogue is invalid: %w", err)
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
// answer 200.
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
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("downloading catalogue: %w", err)
	}
	return data, nil
}

// writeCatalogue writes data to path.incomplete and renames it to path, so
// the file at path is always a whole catalogue.
func writeCatalogue(path string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
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
