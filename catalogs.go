package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
)

// catalog is one entry of catalogs.json: a named apps.json and whether it
// is in use.
type catalog struct {
	Name   string `json:"name"`
	URL    string `json:"url"`
	Use    bool   `json:"use"`
	Dir    string `json:"dir,omitempty"`
	Layout string `json:"layout,omitempty"`
}

// catalogEntry is how loadCatalogList decodes an entry. Use is a pointer
// so that a missing or null "use" is told apart from false.
type catalogEntry struct {
	Name   string `json:"name"`
	URL    string `json:"url"`
	Use    *bool  `json:"use"`
	Dir    string `json:"dir"`
	Layout string `json:"layout"`
}

// dir returns the catalog's directory in cacheDir. Its name ends in a hash
// of the URL, so a catalog whose URL changes gets a new directory.
func (c catalog) dir(cacheDir string) string {
	sum := sha256.Sum256([]byte(c.URL))
	return filepath.Join(cacheDir, "catalogs", c.Name+"-"+hex.EncodeToString(sum[:])[:12])
}

// readCatalogs reads the config and catalogs.json. It returns cache_dir,
// the path of catalogs.json and every catalog, in order.
func readCatalogs() (cacheDir, path string, catalogs []catalog, err error) {
	c, err := readConfig()
	if err != nil {
		return "", "", nil, err
	}
	cacheDir, _, err = c.get("cache_dir")
	if err != nil {
		return "", "", nil, err
	}
	path, err = catalogListPath()
	if err != nil {
		return "", "", nil, err
	}
	catalogs, err = loadCatalogList(path)
	if err != nil {
		return "", "", nil, err
	}
	return cacheDir, path, catalogs, nil
}

// Errors from usedCatalogs, which the TUI tells apart.
var (
	errNoCatalogs = errors.New("no catalogs; run 'sciget catalog add <name> <url>'")
	errNoneInUse  = errors.New("no catalogs in use; run 'sciget catalog use <name>'")
)

// usedCatalogs returns cache_dir and the catalogs in use, in order. Having
// none is an error.
func usedCatalogs() (cacheDir string, used []catalog, err error) {
	cacheDir, _, catalogs, err := readCatalogs()
	if err != nil {
		return "", nil, err
	}
	if len(catalogs) == 0 {
		return "", nil, errNoCatalogs
	}
	for _, c := range catalogs {
		if c.Use {
			used = append(used, c)
		}
	}
	if len(used) == 0 {
		return "", nil, errNoneInUse
	}
	return cacheDir, used, nil
}

// catalogListPath returns the path of catalogs.json, beside the config
// file.
func catalogListPath() (string, error) {
	path, err := configPath()
	if err != nil {
		return "", err
	}
	return filepath.Join(filepath.Dir(path), "catalogs.json"), nil
}

// loadCatalogList reads catalogs.json at path. A missing file lists no
// catalogs. Anything but a JSON array of entries with a valid name, URL
// and use is an error, as is a name listed twice.
func loadCatalogList(path string) ([]catalog, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("reading catalogs: %w", err)
	}
	catalogs, err := parseCatalogList(data)
	if err != nil {
		return nil, fmt.Errorf("reading catalogs %s: %w", path, err)
	}
	return catalogs, nil
}

// parseCatalogList returns the catalogs in data, the JSON of
// catalogs.json.
func parseCatalogList(data []byte) ([]catalog, error) {
	var entries []catalogEntry
	d := json.NewDecoder(bytes.NewReader(data))
	d.DisallowUnknownFields()
	if err := d.Decode(&entries); err != nil {
		return nil, err
	}
	if _, err := d.Token(); err != io.EOF {
		return nil, errors.New("unexpected data after the list")
	}
	if entries == nil {
		return nil, errors.New("not a JSON array")
	}
	// The decoder matches field names without regard to case, so check them
	// exactly.
	var objects []map[string]json.RawMessage
	if err := json.Unmarshal(data, &objects); err != nil {
		return nil, err
	}
	for _, o := range objects {
		for _, key := range slices.Sorted(maps.Keys(o)) {
			if !slices.Contains([]string{"name", "url", "use", "dir", "layout"}, key) {
				return nil, fmt.Errorf("unknown field %q", key)
			}
		}
	}

	catalogs := make([]catalog, 0, len(entries))
	for _, e := range entries {
		if err := checkCatalogName(e.Name); err != nil {
			return nil, fmt.Errorf("invalid catalog name %q: %w", e.Name, err)
		}
		if err := checkCatalogURL(e.URL); err != nil {
			return nil, fmt.Errorf("catalog %q has an invalid URL: %w", e.Name, err)
		}
		if e.Use == nil {
			return nil, fmt.Errorf("catalog %q has no \"use\"", e.Name)
		}
		if findCatalog(catalogs, e.Name) >= 0 {
			return nil, fmt.Errorf("catalog %q is listed twice", e.Name)
		}
		if e.Dir != "" {
			if err := checkAbsolute(e.Dir); err != nil {
				return nil, fmt.Errorf("catalog %q has an invalid dir: %w", e.Name, err)
			}
		}
		if e.Layout != "" {
			if e.Dir == "" {
				return nil, fmt.Errorf("catalog %q has a layout without a dir", e.Name)
			}
			if err := checkLayout(e.Layout); err != nil {
				return nil, fmt.Errorf("catalog %q has an invalid layout: %w", e.Name, err)
			}
		}
		catalogs = append(catalogs, catalog{Name: e.Name, URL: e.URL, Use: *e.Use, Dir: e.Dir, Layout: e.Layout})
	}
	return catalogs, nil
}

// writeCatalogList writes catalogs to catalogs.json at path, in place.
func writeCatalogList(path string, catalogs []catalog) error {
	if catalogs == nil {
		catalogs = []catalog{} // written as [], not null
	}
	if err := writeJSONFile(path, catalogs); err != nil {
		return fmt.Errorf("writing catalogs: %w", err)
	}
	return nil
}

// findCatalog returns the index of the catalog with this name, or -1.
func findCatalog(catalogs []catalog, name string) int {
	for i, c := range catalogs {
		if c.Name == name {
			return i
		}
	}
	return -1
}

// placeCatalog returns catalogs with c added first, or last if last is
// set.
func placeCatalog(catalogs []catalog, c catalog, last bool) []catalog {
	if last {
		return append(catalogs, c)
	}
	return append([]catalog{c}, catalogs...)
}

// writeCatalogTable prints one line per catalog with its position among
// those in use, or "-". For a terminal it adds a header and aligns the
// columns.
func writeCatalogTable(w io.Writer, catalogs []catalog, terminal bool) error {
	var rows strings.Builder
	order := 0
	for _, c := range catalogs {
		position := "-"
		if c.Use {
			order++
			position = strconv.Itoa(order)
		}
		fmt.Fprintf(&rows, "%s\t%s\t%s\t%s\n", c.Name, position, c.URL, c.Dir)
	}
	return writeTable(w, terminal, "NAME\tORDER\tURL\tDIR\n", rows.String())
}

func checkCatalogName(name string) error {
	valid := name != ""
	for i, r := range name {
		letterOrDigit := 'a' <= r && r <= 'z' || '0' <= r && r <= '9'
		if !letterOrDigit && (i == 0 || r != '-' && r != '_') {
			valid = false
		}
	}
	if !valid {
		return errors.New(`must be lowercase letters, digits, "-" and "_", starting with a letter or digit`)
	}
	return nil
}

// checkCatalogURL checks a catalog's URL: https, with a host and no
// credentials. A catalog decides what runs, so plain http is allowed only
// to this machine, where it never crosses the network (a test server, or a
// port forwarded through an SSH tunnel).
func checkCatalogURL(value string) error {
	if err := checkURL(value); err != nil {
		return err
	}
	u, _ := url.Parse(value)
	if ip := net.ParseIP(u.Hostname()); u.Scheme == "http" && u.Hostname() != "localhost" && (ip == nil || !ip.IsLoopback()) {
		return errors.New("must use https; plain http is allowed only to this machine (localhost)")
	}
	if u.User != nil {
		return errors.New("must not contain credentials")
	}
	return nil
}

func checkURL(value string) error {
	u, err := url.Parse(value)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") {
		return errors.New("must start with https:// or http://")
	}
	if u.Hostname() == "" {
		return errors.New("must name a host")
	}
	return nil
}
