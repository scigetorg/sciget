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

// catalog is one entry of catalogs.json: a named catalog JSON and whether
// it is in use. URL is a URL or an absolute path; Domain, when set, is the
// key the catalog takes from the domains file there. In the user's file,
// an entry with no URL only switches the system catalog of that name on or
// off.
type catalog struct {
	Name   string `json:"name"`
	URL    string `json:"url,omitempty"`
	Domain string `json:"domain,omitempty"`
	Use    bool   `json:"use"`
	Dir    string `json:"dir,omitempty"`
	Layout string `json:"layout,omitempty"`
	System bool   `json:"-"` // from the system file
}

// catalogEntry is how loadCatalogList decodes an entry. Use is a pointer
// so that a missing or null "use" is told apart from false.
type catalogEntry struct {
	Name   string `json:"name"`
	URL    string `json:"url"`
	Domain string `json:"domain"`
	Use    *bool  `json:"use"`
	Dir    string `json:"dir"`
	Layout string `json:"layout"`
}

// dir returns the catalog's directory in cacheDir. Its name ends in a hash
// of the URL and domain, so a catalog whose source changes gets a new
// directory.
func (c catalog) dir(cacheDir string) string {
	sum := sha256.Sum256([]byte(c.URL + "\n" + c.Domain))
	return filepath.Join(cacheDir, "catalogs", c.Name+"-"+hex.EncodeToString(sum[:])[:12])
}

// catalogLayers is the user's and the system's catalogs.json.
type catalogLayers struct {
	cacheDir   string
	userPath   string
	user       []catalog // as the user's file lists them
	systemPath string    // "" when there is no system layer
	system     []catalog
}

// readCatalogLayers reads the config and both catalogs.json files.
func readCatalogLayers() (catalogLayers, error) {
	c, err := readConfig()
	if err != nil {
		return catalogLayers{}, err
	}
	var l catalogLayers
	if l.cacheDir, _, err = c.get("cache_dir"); err != nil {
		return catalogLayers{}, err
	}
	if l.userPath, err = catalogListPath(); err != nil {
		return catalogLayers{}, err
	}
	if l.user, err = loadCatalogList(l.userPath); err != nil {
		return catalogLayers{}, err
	}
	if l.systemPath, l.system, err = readSystemCatalogs(); err != nil {
		return catalogLayers{}, err
	}
	return l, nil
}

// readSystemCatalogs returns the path of the system catalogs.json and its
// catalogs, or "" and none when there is no system layer. Every entry
// needs a URL.
func readSystemCatalogs() (string, []catalog, error) {
	path := systemPath("catalogs.json")
	if path == "" {
		return "", nil, nil
	}
	catalogs, err := loadCatalogList(path)
	if err != nil {
		return "", nil, err
	}
	for i := range catalogs {
		if catalogs[i].URL == "" {
			return "", nil, fmt.Errorf("reading catalogs %s: catalog %q has no url", path, catalogs[i].Name)
		}
		catalogs[i].System = true
	}
	return path, catalogs, nil
}

// combined returns every catalog in order: the user's, then the system's.
// A user entry with no URL sets only whether the system catalog of that
// name is in use; a full one hides it.
func (l catalogLayers) combined() []catalog {
	var all []catalog
	for _, c := range l.user {
		if c.URL != "" {
			all = append(all, c)
		}
	}
	for _, c := range l.system {
		if i := findCatalog(l.user, c.Name); i >= 0 {
			if l.user[i].URL != "" {
				continue
			}
			c.Use = l.user[i].Use
		}
		all = append(all, c)
	}
	return all
}

// readCatalogs reads the config and both catalogs.json files. It returns
// cache_dir and every catalog, in order.
func readCatalogs() (cacheDir string, catalogs []catalog, err error) {
	l, err := readCatalogLayers()
	if err != nil {
		return "", nil, err
	}
	return l.cacheDir, l.combined(), nil
}

// Errors from usedCatalogs, which the TUI tells apart.
var (
	errNoCatalogs = errors.New("no catalogs; run 'sciget catalog add <name> <url>'")
	errNoneInUse  = errors.New("no catalogs in use; run 'sciget catalog use <name>'")
)

// usedCatalogs returns cache_dir and the catalogs in use, in order. Having
// none is an error.
func usedCatalogs() (cacheDir string, used []catalog, err error) {
	cacheDir, catalogs, err := readCatalogs()
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

// loadCatalogList reads catalogs.json at path while holding a shared lock
// directly on the open file so concurrent in-place updates cannot expose a
// truncated file, regardless of symlinks, junctions, or path casing. A missing
// file lists no catalogs. Anything but a JSON array of entries with a valid
// name, URL and use is an error, as is a name listed twice.
func loadCatalogList(path string) ([]catalog, error) {
	f, err := os.Open(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("reading catalogs: %w", err)
	}
	closeAndUnlock, err := lockOpenFileShared(f)
	if err != nil {
		return nil, fmt.Errorf("reading catalogs: %w", err)
	}
	data, readErr := io.ReadAll(f)
	_ = closeAndUnlock()
	if readErr != nil {
		return nil, fmt.Errorf("reading catalogs: %w", readErr)
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
			if !slices.Contains([]string{"name", "url", "domain", "use", "dir", "layout"}, key) {
				return nil, fmt.Errorf("unknown field %q", key)
			}
		}
	}

	catalogs := make([]catalog, 0, len(entries))
	for _, e := range entries {
		if err := checkCatalogName(e.Name); err != nil {
			return nil, fmt.Errorf("invalid catalog name %q: %w", e.Name, err)
		}
		if e.URL == "" && (e.Domain != "" || e.Dir != "" || e.Layout != "") {
			return nil, fmt.Errorf("catalog %q has a domain, dir or layout but no url", e.Name)
		}
		if e.URL != "" {
			if err := checkLink(e.URL); err != nil {
				return nil, fmt.Errorf("catalog %q has an invalid URL: %w", e.Name, err)
			}
		}
		if e.Domain != "" {
			if err := checkCatalogName(e.Domain); err != nil {
				return nil, fmt.Errorf("catalog %q has an invalid domain: %w", e.Name, err)
			}
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
		catalogs = append(catalogs, catalog{Name: e.Name, URL: e.URL, Domain: e.Domain, Use: *e.Use, Dir: e.Dir, Layout: e.Layout})
	}
	return catalogs, nil
}

// writeCatalogList writes catalogs to catalogs.json at path, in place, or
// with system set as writeSystemFile does. User writes lock the open file
// itself before truncating and writing so concurrent readers (including
// through symlinks, junctions, or mixed case) never observe O_TRUNC and
// writable files inside read-only directories stay updatable.
func writeCatalogList(path string, catalogs []catalog, system bool) error {
	if catalogs == nil {
		catalogs = []catalog{} // written as [], not null
	}
	if system {
		if err := writeSystemFile(path, catalogs); err != nil {
			return fmt.Errorf("writing catalogs: %w", err)
		}
		return nil
	}
	data, err := json.MarshalIndent(catalogs, "", "  ")
	if err != nil {
		return fmt.Errorf("writing catalogs: %w", err)
	}
	data = append(data, '\n')
	if _, statErr := os.Stat(path); errors.Is(statErr, fs.ErrNotExist) {
		// Resolve any dangling symlink chain so first creation atomically
		// publishes the target file via stage+link (using a short temp name
		// that fits even when the target basename is 255 bytes) instead of
		// exposing a zero-byte file before the exclusive lock is acquired.
		// If resolution fails (e.g. a broken symlink like missing/../target)
		// or the target directory does not exist, skip staging so os.OpenFile
		// reports the genuine OS error.
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			return fmt.Errorf("writing catalogs: %w", err)
		}
		if target, ok := resolveCanonicalPath(path, true); ok {
			if tmp, err := stageTemp(filepath.Dir(target), ".catalogs.*.tmp", data, 0o600); err == nil {
				linkErr := os.Link(tmp, target)
				_ = os.Remove(tmp)
				if linkErr == nil {
					return nil
				}
			}
		}
	}
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return fmt.Errorf("writing catalogs: %w", err)
	}
	closeAndUnlock, err := lockOpenFileExclusive(f)
	if err != nil {
		return fmt.Errorf("writing catalogs: %w", err)
	}
	if _, err = f.Seek(0, io.SeekStart); err == nil {
		if err = f.Truncate(0); err == nil {
			_, err = f.Write(data)
		}
	}
	if closeErr := closeAndUnlock(); err == nil {
		err = closeErr
	}
	if err != nil {
		return fmt.Errorf("writing catalogs: %w", err)
	}
	return nil
}

// sameSettings reports whether a and b read the same file and store
// containers in the same place.
func sameSettings(a, b catalog) bool {
	return a.URL == b.URL && a.Domain == b.Domain && a.Dir == b.Dir && a.Layout == b.Layout
}

// link returns where the catalog comes from, as catalog list shows it.
func (c catalog) link() string {
	if c.Domain != "" {
		return c.URL + "#" + c.Domain
	}
	return c.URL
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
// those in use, or "-", and its source, user or system. For a terminal it
// adds a header and aligns the columns.
func writeCatalogTable(w io.Writer, catalogs []catalog, terminal bool) error {
	var rows strings.Builder
	order := 0
	for _, c := range catalogs {
		position := "-"
		if c.Use {
			order++
			position = strconv.Itoa(order)
		}
		source := "user"
		if c.System {
			source = "system"
		}
		fmt.Fprintf(&rows, "%s\t%s\t%s\t%s\t%s\n", c.Name, position, source, c.link(), c.Dir)
	}
	return writeTable(w, terminal, "NAME\tORDER\tSOURCE\tURL\tDIR\n", rows.String())
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
