package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
)

// catalogJSON is a catalog's own file: where its apps list is and which
// mirrors serve its containers. Without an apps list it is an empty
// catalog; without mirrors its containers come only from a share (--dir).
// Other keys are ignored.
type catalogJSON struct {
	SchemaVersion   int      `json:"schemaVersion"`
	Title           string   `json:"title"`
	Description     string   `json:"description"`
	AppJSONURL      string   `json:"appJsonUrl"`
	EESSICatalogURL string   `json:"eessiCatalogUrl"`
	EESSIStacks     []string `json:"eessi_stacks"`
	DownloadURLs    []string `json:"downloadUrls"`
}

// parseCatalogJSON returns the catalog JSON in data, checked for container
// catalog fields. EESSI-specific fields are validated on add/refresh via
// checkCatalogJSON so older caches never break ordinary container commands.
func parseCatalogJSON(data []byte) (catalogJSON, error) {
	var c catalogJSON
	if err := json.Unmarshal(data, &c); err != nil {
		return catalogJSON{}, err
	}
	switch {
	case c.SchemaVersion > 1:
		return catalogJSON{}, fmt.Errorf("schemaVersion %d needs a newer sciget", c.SchemaVersion)
	case c.SchemaVersion != 1:
		return catalogJSON{}, errors.New("schemaVersion must be 1")
	}
	for _, u := range c.DownloadURLs {
		if err := checkDownloadURL(u); err != nil {
			return catalogJSON{}, fmt.Errorf("download URL %q %w", u, err)
		}
	}
	return c, nil
}

func checkEESSIStacks(stacks []string) error {
	for _, stack := range stacks {
		if !eessiStackPattern.MatchString(stack) {
			return fmt.Errorf("invalid eessi_stacks entry %q", stack)
		}
	}
	return nil
}

// checkCatalogJSON returns the catalog JSON in data, read from base, and
// the link to its apps list, or "" for an empty catalog.
func checkCatalogJSON(data []byte, base string) (catalogJSON, string, error) {
	c, err := parseCatalogJSON(data)
	if err != nil {
		return catalogJSON{}, "", err
	}
	if err := checkEESSIStacks(c.EESSIStacks); err != nil {
		return catalogJSON{}, "", err
	}
	if c.EESSICatalogURL != "" {
		if _, err := resolveLink(base, c.EESSICatalogURL); err != nil {
			return catalogJSON{}, "", fmt.Errorf("eessiCatalogUrl: %w", err)
		}
	}
	if c.AppJSONURL == "" {
		return c, "", nil
	}
	apps, err := resolveLink(base, c.AppJSONURL)
	if err != nil {
		return catalogJSON{}, "", fmt.Errorf("appJsonUrl: %w", err)
	}
	return c, apps, nil
}

// resolveEESSILink returns the resolved link for c.EESSICatalogURL relative to
// base, or "" when no EESSI catalog is configured.
func resolveEESSILink(c catalogJSON, base string) (string, error) {
	if c.EESSICatalogURL == "" {
		return "", nil
	}
	link, err := resolveLink(base, c.EESSICatalogURL)
	if err != nil {
		return "", fmt.Errorf("eessiCatalogUrl: %w", err)
	}
	return link, nil
}

// isURL reports whether link is a URL rather than a local path.
func isURL(link string) bool { return strings.Contains(link, "://") }

// checkLink checks a catalog's link: a URL as checkCatalogURL, or an
// absolute path.
func checkLink(link string) error {
	if isURL(link) {
		return checkCatalogURL(link)
	}
	return checkAbsolute(link)
}

// resolveLink returns ref, a link found in the file read from base, as a
// link of its own. A relative ref is relative to base: to its URL, or to
// its folder when base is a local file. A ref from a URL is always a URL,
// never a local path.
func resolveLink(base, ref string) (string, error) {
	var err error
	switch {
	case isURL(base):
		var b, r *url.URL
		if b, err = url.Parse(base); err == nil {
			if r, err = url.Parse(ref); err == nil {
				ref = b.ResolveReference(r).String()
				err = checkCatalogURL(ref)
			}
		}
	case isURL(ref):
		err = checkCatalogURL(ref)
	default:
		if !filepath.IsAbs(ref) {
			ref = filepath.Join(filepath.Dir(base), ref)
		}
	}
	if err != nil {
		return "", fmt.Errorf("invalid link %q: %w", ref, err)
	}
	return ref, nil
}

// readLink returns the file at link, a URL or a local path, and what
// relative links in it resolve against: the URL it was finally served
// from, after redirects, or the path. A file larger than maxSize bytes is
// an error.
func readLink(client *http.Client, link string, maxSize int64) ([]byte, string, error) {
	if isURL(link) {
		return downloadCatalog(client, link, maxSize)
	}
	f, err := os.Open(link)
	if err != nil {
		return nil, "", fmt.Errorf("reading catalog: %w", err)
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, maxSize+1))
	if err != nil {
		return nil, "", fmt.Errorf("reading catalog: %w", err)
	}
	if int64(len(data)) > maxSize {
		return nil, "", fmt.Errorf("reading catalog: %s is larger than max_catalog_size (%d bytes)", link, maxSize)
	}
	return data, link, nil
}

// onceReader returns a readLink that reads each link at most once, so
// catalogs from one domains file fetch it once per refresh.
func onceReader(client *http.Client, maxSize int64) func(link string) ([]byte, string, error) {
	type result struct {
		data []byte
		base string
		err  error
	}
	seen := map[string]result{}
	return func(link string) ([]byte, string, error) {
		r, ok := seen[link]
		if !ok {
			r.data, r.base, r.err = readLink(client, link, maxSize)
			seen[link] = r
		}
		return r.data, r.base, r.err
	}
}
