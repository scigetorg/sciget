package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"unicode"
)

// container is one row of the catalog: a single container file, which may
// back several catalog entries (a command-line entry and its graphical
// ones).
type container struct {
	tool          string // "fsl"
	version       string // "6.0.3", the software's own version
	image         string // "fsl_6.0.3"
	buildDate     string // "20200905"
	licences      []string
	dois          []string
	apptainerArgs []string // added to every run; all entries agree
	categories    []string
	graphical     bool     // some entry has a graphical command
	hidden        bool     // every entry is hidden
	catalog       string   // the name of the catalog it comes from
	mirrors       []string // the catalog's download URLs, with {file}
	shadowed      bool     // an earlier catalog in use has the same name
	entries       []entry  // visible entries with a command, in key order
	published     download // the image's size and SHA-256, if the catalog gives them
}

// entry is a visible catalog entry that runs a command in its container,
// such as a graphical program.
type entry struct {
	key   string // "fsleyesGUI-fsl 6.0.3"
	label string // "fsleyesGUI"
	exec  string // "fsleyes"
}

// name returns the name users type: "fsl/6.0.3".
func (c container) name() string { return c.tool + "/" + c.version }

// fileName returns the container's file name without extension:
// "fsl_6.0.3_20200905".
func (c container) fileName() string { return c.image + "_" + c.buildDate }

// catalogTool is one tool in apps.json, holding its entries by key.
type catalogTool struct {
	Categories []string              `json:"categories"`
	Apps       map[string]catalogApp `json:"apps"`
}

// catalogApp is one entry of apps.json. Its "version" field holds the
// container's build date, not the software version.
type catalogApp struct {
	ApptainerArgs []string `json:"apptainer_args"`
	Licences      []string `json:"license"`
	DOI           string   `json:"doi"`
	BuildDate     string   `json:"version"`
	Exec          string   `json:"exec"`
	Image         string   `json:"image"`
	ShowInApplist *bool    `json:"show_in_applist"`
	SHA256        string   `json:"sha256"`
	Size          int64    `json:"size"`
}

// readContainers returns the containers of every catalog in use, sorted by
// tool, then version, then catalog order. A container whose name an
// earlier catalog also has is marked shadowed.
func readContainers() ([]container, error) {
	cacheDir, used, err := usedCatalogs()
	if err != nil {
		return nil, err
	}
	var all []container
	seen := map[string]bool{}
	for _, cat := range used {
		containers, mirrors, err := readCatalogSnapshot(cat.dir(cacheDir))
		if err != nil {
			return nil, &catalogError{name: cat.Name, err: err}
		}
		for _, c := range containers {
			c.catalog = cat.Name
			c.mirrors = mirrors
			c.shadowed = seen[c.name()]
			seen[c.name()] = true
			all = append(all, c)
		}
	}
	// A stable sort keeps copies of a name in catalog order.
	slices.SortStableFunc(all, compareContainers)
	return all, nil
}

// checkCatalogClean returns an error if dir has leftover refresh backup or
// staged files from an incomplete rollback.
func checkCatalogClean(dir string) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		name := entry.Name()
		if strings.HasSuffix(name, ".bak") || strings.HasSuffix(name, ".incomplete") {
			return errors.New("catalog cache is incomplete after a failed refresh; run 'sciget refresh' to recover")
		}
	}
	return nil
}

func readCatalogSnapshotLocked(dir string) ([]container, []string, error) {
	unlock, locked, err := lockCatalogDirShared(dir)
	if err != nil {
		return nil, nil, err
	}
	if !locked {
		return nil, nil, &notDownloadedError{path: filepath.Join(dir, "apps.json")}
	}
	defer unlock()
	if err := checkCatalogClean(dir); err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, nil, err
	}
	containers, err := loadCatalog(filepath.Join(dir, "apps.json"))
	if err != nil {
		return nil, nil, err
	}
	mirrors, err := loadMirrors(filepath.Join(dir, "catalog.json"))
	if err != nil {
		return nil, nil, err
	}
	return containers, mirrors, nil
}

// readCatalogSnapshot reads apps.json and catalog.json consistently. When dir
// has a .refresh.lock, it holds the shared lock across both reads so an
// in-progress refresh commit or rollback is never observed. Older caches
// without a .refresh.lock are still read unlocked, then re-checked for a
// concurrently created lock so a first refresh cannot slip uncommitted files
// into the read.
func readCatalogSnapshot(dir string) ([]container, []string, error) {
	unlock, locked, err := lockCatalogDirShared(dir)
	if err != nil {
		return nil, nil, err
	}
	if locked {
		defer unlock()
		if err := checkCatalogClean(dir); err != nil && !errors.Is(err, os.ErrNotExist) {
			return nil, nil, err
		}
		containers, err := loadCatalog(filepath.Join(dir, "apps.json"))
		if err != nil {
			return nil, nil, err
		}
		mirrors, err := loadMirrors(filepath.Join(dir, "catalog.json"))
		if err != nil {
			return nil, nil, err
		}
		return containers, mirrors, nil
	}
	if err := checkCatalogClean(dir); err != nil && !errors.Is(err, os.ErrNotExist) {
		if _, statErr := os.Stat(filepath.Join(dir, ".refresh.lock")); statErr == nil {
			return readCatalogSnapshotLocked(dir)
		}
		return nil, nil, err
	}
	containers, err := loadCatalog(filepath.Join(dir, "apps.json"))
	if err != nil {
		if _, statErr := os.Stat(filepath.Join(dir, ".refresh.lock")); statErr == nil {
			return readCatalogSnapshotLocked(dir)
		}
		return nil, nil, err
	}
	mirrors, err := loadMirrors(filepath.Join(dir, "catalog.json"))
	if err != nil {
		if _, statErr := os.Stat(filepath.Join(dir, ".refresh.lock")); statErr == nil {
			return readCatalogSnapshotLocked(dir)
		}
		return nil, nil, err
	}
	if _, statErr := os.Stat(filepath.Join(dir, ".refresh.lock")); statErr == nil {
		return readCatalogSnapshotLocked(dir)
	} else if !errors.Is(statErr, os.ErrNotExist) {
		return nil, nil, statErr
	}
	return containers, mirrors, nil
}

// loadMirrors returns the download URLs in the catalog JSON at path, or
// none when there is no such file, so a catalog last refreshed by an older
// sciget still lists its containers.
func loadMirrors(path string) ([]string, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("reading catalog: %w", err)
	}
	c, err := parseCatalogJSON(data)
	if err != nil {
		return nil, fmt.Errorf("reading catalog %s: %w", path, err)
	}
	return c.DownloadURLs, nil
}

// catalogError is why the catalog in use called name could not be read.
// Its text is err's alone; the TUI uses the name.
type catalogError struct {
	name string
	err  error
}

func (e *catalogError) Error() string { return e.err.Error() }
func (e *catalogError) Unwrap() error { return e.err }

// notDownloadedError is a catalog in use whose apps.json, at path, does
// not exist yet.
type notDownloadedError struct{ path string }

func (e *notDownloadedError) Error() string {
	return fmt.Sprintf("no catalog at %s; run 'sciget refresh'", e.path)
}

// loadCatalog reads apps.json at path and returns its containers, sorted by
// tool and then by version.
func loadCatalog(path string) ([]container, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, &notDownloadedError{path: path}
	}
	if err != nil {
		return nil, fmt.Errorf("reading catalog: %w", err)
	}
	containers, err := parseCatalog(data)
	if err != nil {
		return nil, fmt.Errorf("reading catalog %s: %w", path, err)
	}
	return containers, nil
}

// valueOptions are the apptainer_args options whose next word is their
// value. Add one when a catalog needs it.
var valueOptions = []string{"--bind", "-B"}

// parseCatalog returns the containers in data, the JSON of apps.json,
// sorted by tool and then by version.
func parseCatalog(data []byte) ([]container, error) {
	var tools map[string]catalogTool
	if err := json.Unmarshal(data, &tools); err != nil {
		return nil, err
	}

	byName := map[string]*container{}
	for tool, t := range tools {
		for key, app := range t.Apps {
			// Every string kept is checked here, once, so no output path
			// can print a terminal escape from a catalog.
			texts := slices.Concat([]string{tool, key, app.Image, app.BuildDate, app.DOI, app.Exec},
				t.Categories, app.Licences, app.ApptainerArgs)
			if slices.ContainsFunc(texts, func(s string) bool { return printable(s) != s }) {
				return nil, fmt.Errorf("catalog entry %q of %q has a control character", key, tool)
			}
			if strings.TrimSpace(app.Exec) == "" {
				app.Exec = ""
			}
			// Keys are "<label> <version>", except that entries with an
			// image may end in another word ("amico 2.1.0 arm64").
			image := app.Image
			if image == "" {
				i := strings.LastIndex(key, " ")
				if i < 0 {
					return nil, fmt.Errorf("catalog entry %q has no version", key)
				}
				image = tool + "_" + key[i+1:]
			}
			toolVersion, ok := strings.CutPrefix(image, tool+"_")
			switch {
			case !ok:
				return nil, fmt.Errorf("catalog entry %q has image %q, which does not start with %q",
					key, image, tool+"_")
			case toolVersion == "":
				return nil, fmt.Errorf("catalog entry %q has no version", key)
			case app.BuildDate == "":
				return nil, fmt.Errorf("catalog entry %q has no build date", key)
			case unsafeLayoutValue(tool) || unsafeLayoutValue(toolVersion) || unsafeLayoutValue(app.BuildDate):
				return nil, fmt.Errorf("catalog entry %q has a tool, version or build date with \"/\" or \"..\", or that is \".\"", key)
			case app.SHA256 != "" && !sha256Hex.MatchString(app.SHA256):
				return nil, fmt.Errorf("catalog entry %q has a sha256 that is not 64 lowercase hex digits", key)
			case app.Size < 0:
				return nil, fmt.Errorf("catalog entry %q has a negative size", key)
			}
			published := download{size: app.Size, sha256: app.SHA256}

			// The arguments go before the image, so a stray word would be
			// read as the image: each is an option, and an option in
			// valueOptions is followed by exactly one value.
			args := app.ApptainerArgs
			for i := 0; i < len(args); i++ {
				if len(args[i]) < 2 || args[i][0] != '-' || args[i] == "--" {
					return nil, fmt.Errorf("catalog entry %q has apptainer_args element %q, which is not an option or its value", key, args[i])
				}
				if slices.Contains(valueOptions, args[i]) {
					i++
					if i == len(args) || args[i] == "" || args[i][0] == '-' {
						return nil, fmt.Errorf("catalog entry %q has apptainer_args option %q without a value", key, args[i-1])
					}
				}
			}

			c := byName[tool+"/"+toolVersion]
			if c == nil {
				c = &container{tool: tool, version: toolVersion, image: image,
					buildDate: app.BuildDate, categories: t.Categories, hidden: true,
					apptainerArgs: app.ApptainerArgs, published: published}
				byName[c.name()] = c
			}
			if app.BuildDate != c.buildDate {
				return nil, fmt.Errorf("catalog lists %s with build dates %s and %s",
					c.name(), c.buildDate, app.BuildDate)
			}
			if !slices.Equal(app.ApptainerArgs, c.apptainerArgs) {
				return nil, fmt.Errorf("catalog lists %s with apptainer_args %q and %q",
					c.name(), c.apptainerArgs, app.ApptainerArgs)
			}
			if published != c.published {
				return nil, fmt.Errorf("catalog lists %s with different sizes or SHA-256s", c.name())
			}
			c.licences = append(c.licences, app.Licences...)
			if app.DOI != "" {
				c.dois = append(c.dois, app.DOI)
			}
			if app.Exec != "" {
				c.graphical = true
			}
			if app.ShowInApplist == nil || *app.ShowInApplist {
				c.hidden = false
				if app.Exec != "" {
					label, _, _ := strings.Cut(key, "-")
					c.entries = append(c.entries, entry{key: key, label: label, exec: app.Exec})
				}
			}
		}
	}
	containers := make([]container, 0, len(byName))
	for _, c := range byName {
		c.licences = slices.Compact(slices.Sorted(slices.Values(c.licences)))
		c.dois = slices.Compact(slices.Sorted(slices.Values(c.dois)))
		slices.SortFunc(c.entries, func(a, b entry) int { return strings.Compare(a.key, b.key) })
		containers = append(containers, *c)
	}
	slices.SortFunc(containers, compareContainers)
	return containers, nil
}

// sha256Hex matches a SHA-256 written as lowercase hex.
var sha256Hex = regexp.MustCompile(`^[0-9a-f]{64}$`)

// compareContainers orders containers by tool, then by version.
func compareContainers(a, b container) int {
	if n := strings.Compare(a.tool, b.tool); n != 0 {
		return n
	}
	return compareVersions(a.version, b.version)
}

// compareVersions compares two versions, treating runs of digits as numbers so
// that "6.0.9" sorts before "6.0.10". It returns -1, 0 or +1.
func compareVersions(a, b string) int {
	for a != "" && b != "" {
		var x, y string
		x, a = versionChunk(a)
		y, b = versionChunk(b)
		if n := compareChunks(x, y); n != 0 {
			return n
		}
	}
	switch {
	case a == b:
		return 0
	case a == "":
		return -1
	default:
		return 1
	}
}

// versionChunk splits s after its leading run of digits or of non-digits.
func versionChunk(s string) (chunk, rest string) {
	digit := isDigit(s[0])
	i := 1
	for i < len(s) && isDigit(s[i]) == digit {
		i++
	}
	return s[:i], s[i:]
}

// compareChunks compares two chunks, as numbers if both are digits.
func compareChunks(x, y string) int {
	if isDigit(x[0]) && isDigit(y[0]) {
		x, y = strings.TrimLeft(x, "0"), strings.TrimLeft(y, "0")
		if len(x) != len(y) {
			if len(x) < len(y) {
				return -1
			}
			return 1
		}
	}
	return strings.Compare(x, y)
}

func isDigit(b byte) bool { return '0' <= b && b <= '9' }

// lookupContainer returns the winning container with this exact name.
func lookupContainer(containers []container, name string) (container, error) {
	for _, c := range containers {
		if c.name() == name && !c.shadowed {
			return c, nil
		}
	}
	tool, _, _ := strings.Cut(name, "/")
	return container{}, fmt.Errorf("no container named %q; run 'sciget find %s' to list versions, or 'sciget refresh' to update the catalogs", name, tool)
}

// printable returns s fit to draw: whole terminal escape sequences, control
// characters (C0, DEL, C1) and Unicode format characters (such as
// bidirectional overrides and zero-width characters) are removed, so text
// from outside sciget can neither inject codes nor reorder or hide text.
// Use it only for display; launching and lookups use the raw value.
func printable(s string) string {
	s = escapeSequence.ReplaceAllString(s, "")
	return strings.Map(func(r rune) rune {
		if r < 0x20 || r >= 0x7f && r < 0xa0 || unicode.Is(unicode.Cf, r) {
			return -1
		}
		return r
	}, s)
}

// escapeSequence matches ANSI escape sequences: CSI ("ESC [ ... final"),
// OSC ("ESC ] ... BEL or ESC \\") and two-character escapes.
var escapeSequence = regexp.MustCompile(`\x1b(\[[0-?]*[ -/]*[@-~]|\][^\x07\x1b]*(\x07|\x1b\\)|[@-Z\\-_])`)
