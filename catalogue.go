package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

// container is one row of the catalogue: a single container file, which may
// back several catalogue entries (a command-line entry and its graphical
// ones).
type container struct {
	tool       string // "fsl"
	release    string // "6.0.3"
	image      string // "fsl_6.0.3"
	buildDate  string // "20200905"
	categories []string
	graphical  bool // some entry has a graphical command
	hidden     bool // every entry is hidden
}

// name returns the name users type: "fsl/6.0.3".
func (c container) name() string { return c.tool + "/" + c.release }

// fileName returns the container's file name without extension:
// "fsl_6.0.3_20200905".
func (c container) fileName() string { return c.image + "_" + c.buildDate }

// catalogueTool and catalogueApp mirror the JSON of apps.json.
type catalogueTool struct {
	Categories []string                `json:"categories"`
	Apps       map[string]catalogueApp `json:"apps"`
}

type catalogueApp struct {
	BuildDate     string `json:"version"`
	Exec          string `json:"exec"`
	Image         string `json:"image"`
	ShowInApplist *bool  `json:"show_in_applist"`
}

// cataloguePath returns where the downloaded catalogue is kept: apps.json in
// the config's cache_dir.
func cataloguePath() (string, error) {
	c, err := readConfig()
	if err != nil {
		return "", err
	}
	dir, _, err := c.get("cache_dir")
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "apps.json"), nil
}

// readCatalogue loads the catalogue from cataloguePath.
func readCatalogue() ([]container, error) {
	path, err := cataloguePath()
	if err != nil {
		return nil, err
	}
	return loadCatalogue(path)
}

// loadCatalogue reads apps.json at path and returns its containers, sorted by
// tool and then by release.
func loadCatalogue(path string) ([]container, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, fmt.Errorf("no catalogue at %s", path)
	}
	if err != nil {
		return nil, fmt.Errorf("reading catalogue: %w", err)
	}
	var tools map[string]catalogueTool
	if err := json.Unmarshal(data, &tools); err != nil {
		return nil, fmt.Errorf("reading catalogue %s: %w", path, err)
	}

	byName := map[string]*container{}
	for tool, t := range tools {
		for key, app := range t.Apps {
			// Keys are "<label> <release>", except that entries with an
			// image may end in another word ("amico 2.1.0 arm64").
			image := app.Image
			if image == "" {
				i := strings.LastIndex(key, " ")
				if i < 0 {
					return nil, fmt.Errorf("catalogue entry %q has no release", key)
				}
				image = tool + "_" + key[i+1:]
			}
			release := strings.TrimPrefix(image, tool+"_")

			c := byName[tool+"/"+release]
			if c == nil {
				c = &container{tool: tool, release: release, image: image,
					buildDate: app.BuildDate, categories: t.Categories, hidden: true}
				byName[c.name()] = c
			}
			if app.BuildDate != c.buildDate {
				return nil, fmt.Errorf("catalogue lists %s with build dates %s and %s",
					c.name(), c.buildDate, app.BuildDate)
			}
			if app.Exec != "" {
				c.graphical = true
			}
			if app.ShowInApplist == nil || *app.ShowInApplist {
				c.hidden = false
			}
		}
	}
	if len(byName) == 0 {
		return nil, fmt.Errorf("catalogue %s has no containers", path)
	}

	containers := make([]container, 0, len(byName))
	for _, c := range byName {
		containers = append(containers, *c)
	}
	slices.SortFunc(containers, func(a, b container) int {
		if n := strings.Compare(a.tool, b.tool); n != 0 {
			return n
		}
		return compareVersions(a.release, b.release)
	})
	return containers, nil
}

// compareVersions compares two releases, treating runs of digits as numbers so
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
