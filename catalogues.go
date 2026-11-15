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

// catalogue is one entry of catalogues.json: a named apps.json and whether it
// is in use.
type catalogue struct {
	Name   string `json:"name"`
	URL    string `json:"url"`
	Use    bool   `json:"use"`
	Dir    string `json:"dir,omitempty"`
	Layout string `json:"layout,omitempty"`
}

// catalogueEntry is how loadCatalogueList decodes an entry. Use is a pointer
// so that a missing or null "use" is told apart from false.
type catalogueEntry struct {
	Name   string `json:"name"`
	URL    string `json:"url"`
	Use    *bool  `json:"use"`
	Dir    string `json:"dir"`
	Layout string `json:"layout"`
}

// dir returns the catalogue's directory in cacheDir. Its name ends in a hash
// of the URL, so a catalogue whose URL changes gets a new directory.
func (c catalogue) dir(cacheDir string) string {
	sum := sha256.Sum256([]byte(c.URL))
	return filepath.Join(cacheDir, "catalogues", c.Name+"-"+hex.EncodeToString(sum[:])[:12])
}

// readCatalogues reads the config and catalogues.json. It returns cache_dir,
// the path of catalogues.json and every catalogue, in order.
func readCatalogues() (cacheDir, path string, catalogues []catalogue, err error) {
	c, err := readConfig()
	if err != nil {
		return "", "", nil, err
	}
	cacheDir, _, err = c.get("cache_dir")
	if err != nil {
		return "", "", nil, err
	}
	path, err = catalogueListPath()
	if err != nil {
		return "", "", nil, err
	}
	catalogues, err = loadCatalogueList(path)
	if err != nil {
		return "", "", nil, err
	}
	return cacheDir, path, catalogues, nil
}

// Errors from usedCatalogues, which the TUI tells apart.
var (
	errNoCatalogues = errors.New("no catalogues; run 'sciget catalogue add <name> <url>'")
	errNoneInUse    = errors.New("no catalogues in use; run 'sciget catalogue use <name>'")
)

// usedCatalogues returns cache_dir and the catalogues in use, in order. Having
// none is an error.
func usedCatalogues() (cacheDir string, used []catalogue, err error) {
	cacheDir, _, catalogues, err := readCatalogues()
	if err != nil {
		return "", nil, err
	}
	if len(catalogues) == 0 {
		return "", nil, errNoCatalogues
	}
	for _, c := range catalogues {
		if c.Use {
			used = append(used, c)
		}
	}
	if len(used) == 0 {
		return "", nil, errNoneInUse
	}
	return cacheDir, used, nil
}

// catalogueListPath returns the path of catalogues.json, beside the config
// file.
func catalogueListPath() (string, error) {
	path, err := configPath()
	if err != nil {
		return "", err
	}
	return filepath.Join(filepath.Dir(path), "catalogues.json"), nil
}

// loadCatalogueList reads catalogues.json at path. A missing file lists no
// catalogues. Anything but a JSON array of entries with a valid name, URL
// and use is an error, as is a name listed twice.
func loadCatalogueList(path string) ([]catalogue, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("reading catalogues: %w", err)
	}
	catalogues, err := parseCatalogueList(data)
	if err != nil {
		return nil, fmt.Errorf("reading catalogues %s: %w", path, err)
	}
	return catalogues, nil
}

// parseCatalogueList returns the catalogues in data, the JSON of
// catalogues.json.
func parseCatalogueList(data []byte) ([]catalogue, error) {
	var entries []catalogueEntry
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

	catalogues := make([]catalogue, 0, len(entries))
	for _, e := range entries {
		if err := checkCatalogueName(e.Name); err != nil {
			return nil, fmt.Errorf("invalid catalogue name %q: %w", e.Name, err)
		}
		if err := checkCatalogueURL(e.URL); err != nil {
			return nil, fmt.Errorf("catalogue %q has an invalid URL: %w", e.Name, err)
		}
		if e.Use == nil {
			return nil, fmt.Errorf("catalogue %q has no \"use\"", e.Name)
		}
		if findCatalogue(catalogues, e.Name) >= 0 {
			return nil, fmt.Errorf("catalogue %q is listed twice", e.Name)
		}
		if e.Dir != "" {
			if err := checkAbsolute(e.Dir); err != nil {
				return nil, fmt.Errorf("catalogue %q has an invalid dir: %w", e.Name, err)
			}
		}
		if e.Layout != "" {
			if e.Dir == "" {
				return nil, fmt.Errorf("catalogue %q has a layout without a dir", e.Name)
			}
			if err := checkLayout(e.Layout); err != nil {
				return nil, fmt.Errorf("catalogue %q has an invalid layout: %w", e.Name, err)
			}
		}
		catalogues = append(catalogues, catalogue{Name: e.Name, URL: e.URL, Use: *e.Use, Dir: e.Dir, Layout: e.Layout})
	}
	return catalogues, nil
}

// writeCatalogueList writes catalogues to catalogues.json at path, in place.
func writeCatalogueList(path string, catalogues []catalogue) error {
	if catalogues == nil {
		catalogues = []catalogue{} // written as [], not null
	}
	if err := writeJSONFile(path, catalogues); err != nil {
		return fmt.Errorf("writing catalogues: %w", err)
	}
	return nil
}

// findCatalogue returns the index of the catalogue with this name, or -1.
func findCatalogue(catalogues []catalogue, name string) int {
	for i, c := range catalogues {
		if c.Name == name {
			return i
		}
	}
	return -1
}

// placeCatalogue returns catalogues with c added first, or last if last is
// set.
func placeCatalogue(catalogues []catalogue, c catalogue, last bool) []catalogue {
	if last {
		return append(catalogues, c)
	}
	return append([]catalogue{c}, catalogues...)
}

// writeCatalogueTable prints one line per catalogue with its position among
// those in use, or "-". For a terminal it adds a header and aligns the
// columns.
func writeCatalogueTable(w io.Writer, catalogues []catalogue, terminal bool) error {
	var rows strings.Builder
	order := 0
	for _, c := range catalogues {
		position := "-"
		if c.Use {
			order++
			position = strconv.Itoa(order)
		}
		fmt.Fprintf(&rows, "%s\t%s\t%s\t%s\n", c.Name, position, c.URL, c.Dir)
	}
	return writeTable(w, terminal, "NAME\tORDER\tURL\tDIR\n", rows.String())
}

func checkCatalogueName(name string) error {
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

// checkCatalogueURL checks a catalogue's URL: https, with a host and no
// credentials. A catalogue decides what runs, so plain http is allowed only
// to this machine, where it never crosses the network (a test server, or a
// port forwarded through an SSH tunnel).
func checkCatalogueURL(value string) error {
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
