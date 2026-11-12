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
	version    string // "6.0.3", the software's own version
	image      string // "fsl_6.0.3"
	buildDate  string // "20200905"
	categories []string
	graphical  bool   // some entry has a graphical command
	hidden     bool   // every entry is hidden
	catalogue  string // the name of the catalogue it comes from
	shadowed   bool   // an earlier catalogue in use has the same name
}

// name returns the name users type: "fsl/6.0.3".
func (c container) name() string { return c.tool + "/" + c.version }

// fileName returns the container's file name without extension:
// "fsl_6.0.3_20200905".
func (c container) fileName() string { return c.image + "_" + c.buildDate }

// catalogueTool is one tool in apps.json, holding its entries by key.
type catalogueTool struct {
	Categories []string                `json:"categories"`
	Apps       map[string]catalogueApp `json:"apps"`
}

// catalogueApp is one entry of apps.json. Its "version" field holds the
// container's build date, not the software version.
type catalogueApp struct {
	BuildDate     string `json:"version"`
	Exec          string `json:"exec"`
	Image         string `json:"image"`
	ShowInApplist *bool  `json:"show_in_applist"`
}

// readContainers returns the containers of every catalogue in use, sorted by
// tool, then version, then catalogue order. A container whose name an
// earlier catalogue also has is marked shadowed.
func readContainers() ([]container, error) {
	cacheDir, used, err := usedCatalogues()
	if err != nil {
		return nil, err
	}
	var all []container
	seen := map[string]bool{}
	for _, cat := range used {
		containers, err := loadCatalogue(filepath.Join(cat.dir(cacheDir), "apps.json"))
		if err != nil {
			return nil, err
		}
		for _, c := range containers {
			c.catalogue = cat.Name
			c.shadowed = seen[c.name()]
			seen[c.name()] = true
			all = append(all, c)
		}
	}
	// A stable sort keeps copies of a name in catalogue order.
	slices.SortStableFunc(all, compareContainers)
	return all, nil
}

// loadCatalogue reads apps.json at path and returns its containers, sorted by
// tool and then by version.
func loadCatalogue(path string) ([]container, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, fmt.Errorf("no catalogue at %s; run 'sciget refresh'", path)
	}
	if err != nil {
		return nil, fmt.Errorf("reading catalogue: %w", err)
	}
	containers, err := parseCatalogue(data)
	if err != nil {
		return nil, fmt.Errorf("reading catalogue %s: %w", path, err)
	}
	return containers, nil
}

// parseCatalogue returns the containers in data, the JSON of apps.json,
// sorted by tool and then by version.
func parseCatalogue(data []byte) ([]container, error) {
	var tools map[string]catalogueTool
	if err := json.Unmarshal(data, &tools); err != nil {
		return nil, err
	}

	byName := map[string]*container{}
	for tool, t := range tools {
		for key, app := range t.Apps {
			// Keys are "<label> <version>", except that entries with an
			// image may end in another word ("amico 2.1.0 arm64").
			image := app.Image
			if image == "" {
				i := strings.LastIndex(key, " ")
				if i < 0 {
					return nil, fmt.Errorf("catalogue entry %q has no version", key)
				}
				image = tool + "_" + key[i+1:]
			}
			toolVersion, ok := strings.CutPrefix(image, tool+"_")
			switch {
			case !ok:
				return nil, fmt.Errorf("catalogue entry %q has image %q, which does not start with %q",
					key, image, tool+"_")
			case toolVersion == "":
				return nil, fmt.Errorf("catalogue entry %q has no version", key)
			case app.BuildDate == "":
				return nil, fmt.Errorf("catalogue entry %q has no build date", key)
			}

			c := byName[tool+"/"+toolVersion]
			if c == nil {
				c = &container{tool: tool, version: toolVersion, image: image,
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
		return nil, errors.New("no containers")
	}

	containers := make([]container, 0, len(byName))
	for _, c := range byName {
		containers = append(containers, *c)
	}
	slices.SortFunc(containers, compareContainers)
	return containers, nil
}

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
