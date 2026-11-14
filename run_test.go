package main

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
)

func TestLayoutPath(t *testing.T) {
	layout, err := defaultValue("container_layout")
	if err != nil {
		t.Fatal(err)
	}
	c := container{tool: "fsl", version: "6.0.3", buildDate: "20200905"}
	tests := []struct {
		name   string
		layout string
		c      container
		want   string
		bad    bool
	}{
		{name: "default", layout: layout, c: c, want: "fsl_6.0.3_20200905/fsl_6.0.3_20200905.simg"},
		{name: "custom", layout: "{date}/{tool}/{version}", c: c, want: "20200905/fsl/6.0.3"},
		{name: "unknown placeholder", layout: "{tool}/{version}/{date}/{name}", c: c, bad: true},
		{name: "unclosed placeholder", layout: "{tool}/{version}/{date}/{", c: c, bad: true},
		{name: "missing tool", layout: "{version}/{date}", c: c, bad: true},
		{name: "missing version", layout: "{tool}/{date}", c: c, bad: true},
		{name: "missing date", layout: "{tool}/{version}", c: c, bad: true},
		{name: "absolute", layout: "/{tool}/{version}/{date}", c: c, bad: true},
		{name: "parent element", layout: "{tool}/../{version}/{date}", c: c, bad: true},
		{name: "empty element", layout: "{tool}//{version}/{date}", c: c, bad: true},
		{name: "trailing slash", layout: "{tool}/{version}/{date}/", c: c, bad: true},
		{name: "empty", c: c, bad: true},
		{name: "empty tool", layout: layout, c: container{version: "1", buildDate: "1"}, bad: true},
		{name: "empty version", layout: layout, c: container{tool: "fsl", buildDate: "1"}, bad: true},
		{name: "empty date", layout: layout, c: container{tool: "fsl", version: "1"}, bad: true},
		{name: "parent in tool", layout: layout, c: container{tool: "../x", version: "1", buildDate: "1"}, bad: true},
		{name: "slash in version", layout: layout, c: container{tool: "fsl", version: "a/b", buildDate: "1"}, bad: true},
		{name: "parent in date", layout: layout, c: container{tool: "fsl", version: "1", buildDate: ".."}, bad: true},
		{name: "embedded parent", layout: layout, c: container{tool: "fsl", version: "1..2", buildDate: "1"}, bad: true},
		{name: "NUL in date", layout: layout, c: container{tool: "fsl", version: "1", buildDate: "1\x00"}, bad: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := layoutPath(tt.layout, tt.c)
			if (err != nil) != tt.bad {
				t.Fatalf("error = %v, want error: %v", err, tt.bad)
			}
			if err != nil {
				return
			}
			if got != filepath.FromSlash(tt.want) {
				t.Errorf("path = %q, want %q", got, tt.want)
			}
		})
	}
}

// localCatalogue configures an isolated share, store and cached catalogue.
func localCatalogue(t *testing.T) (container, catalogue, string) {
	t.Helper()
	base := t.TempDir()
	t.Setenv("SCIGET_CONFIG", filepath.Join(base, "config.json"))
	store := filepath.Join(base, "store")
	if err := writeConfig(os.Getenv("SCIGET_CONFIG"), map[string]string{
		"cache_dir": filepath.Join(base, "cache"), "container_dir": store,
	}); err != nil {
		t.Fatal(err)
	}
	cat := catalogue{Name: "neuro", URL: exampleURL("neuro"), Use: true, Dir: filepath.Join(base, "share")}
	path, err := catalogueListPath()
	if err != nil {
		t.Fatal(err)
	}
	if err := writeCatalogueList(path, []catalogue{cat}); err != nil {
		t.Fatal(err)
	}
	cache := cat.dir(filepath.Join(base, "cache"))
	if err := os.MkdirAll(cache, 0o755); err != nil {
		t.Fatal(err)
	}
	const apps = `{"fsl":{"categories":["imaging"],"apps":{
		"fsl 6.0.3":{"version":"20200905","license":["MIT","Apache-2.0"],"doi":"https://doi.org/1"},
		"GUI-fsl 6.0.3":{"version":"20200905","exec":"gui","license":["MIT"],"doi":"https://doi.org/2"}}}}`
	if err := os.WriteFile(filepath.Join(cache, "apps.json"), []byte(apps), 0o600); err != nil {
		t.Fatal(err)
	}
	return container{tool: "fsl", version: "6.0.3", image: "fsl_6.0.3", buildDate: "20200905", catalogue: cat.Name}, cat, store
}

// makeContainerPath creates a regular container file or sandbox directory.
func makeContainerPath(t *testing.T, dir, layout string, c container, sandbox bool) string {
	t.Helper()
	relative, err := layoutPath(layout, c)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, relative)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if sandbox {
		err = os.Mkdir(path, 0o755)
	} else {
		err = os.WriteFile(path, nil, 0o600)
	}
	if err != nil {
		t.Fatal(err)
	}
	return path
}

func TestLocate(t *testing.T) {
	tests := []struct {
		name       string
		share      bool
		store      bool
		shareDir   bool
		sandbox    bool
		shadowed   bool
		symlink    bool
		special    bool
		brokenLink bool
		custom     bool
		badValue   bool
		noHome     bool
		wantPlace  string
		wantErr    bool
	}{
		{name: "share wins", share: true, store: true, wantPlace: "neuro"},
		{name: "sandbox", share: true, sandbox: true, wantPlace: "neuro"},
		{name: "missing share", store: true, wantPlace: "store"},
		{name: "share path missing", shareDir: true, store: true, wantPlace: "store"},
		{name: "neither"},
		{name: "shadowed share is not used", shadowed: true},
		{name: "symlink is followed", share: true, symlink: true, wantPlace: "neuro"},
		{name: "special is skipped", special: true, store: true, wantPlace: "store"},
		{name: "stat error", share: true, brokenLink: true, wantErr: true},
		{name: "share uses default despite store override", share: true, custom: true, wantPlace: "neuro"},
		{name: "custom store", store: true, custom: true, wantPlace: "store"},
		{name: "unsafe catalogue value", badValue: true, wantErr: true},
		{name: "share hit without a home directory", share: true, noHome: true, wantPlace: "neuro"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c, cat, store := localCatalogue(t)
			layout, err := defaultValue("container_layout")
			if err != nil {
				t.Fatal(err)
			}
			if tt.shareDir {
				if err := os.MkdirAll(cat.Dir, 0o755); err != nil {
					t.Fatal(err)
				}
			}
			wantPath := ""
			if tt.store {
				storeLayout, err := defaultValue("container_layout")
				if err != nil {
					t.Fatal(err)
				}
				if tt.custom {
					storeLayout = "{date}/{tool}-{version}"
					cfg, err := readConfig()
					if err != nil {
						t.Fatal(err)
					}
					if err := cfg.set("container_layout", storeLayout); err != nil {
						t.Fatal(err)
					}
				}
				wantPath = makeContainerPath(t, store, storeLayout, c, false)
			} else if tt.custom {
				cfg, err := readConfig()
				if err != nil {
					t.Fatal(err)
				}
				if err := cfg.set("container_layout", "{date}/{tool}-{version}"); err != nil {
					t.Fatal(err)
				}
			}
			if tt.share || tt.special {
				path := makeContainerPath(t, cat.Dir, layout, c, tt.sandbox)
				if tt.symlink || tt.brokenLink || tt.special {
					if err := os.Remove(path); err != nil {
						t.Fatal(err)
					}
				}
				switch {
				case tt.symlink:
					target := makeContainerPath(t, t.TempDir(), layout, c, false)
					if err := os.Symlink(target, path); err != nil {
						t.Fatal(err)
					}
				case tt.brokenLink:
					if err := os.Symlink(path, path); err != nil {
						t.Fatal(err)
					}
				case tt.special:
					if runtime.GOOS == "windows" {
						t.Skip("device symlink fixture")
					}
					if err := os.Symlink(os.DevNull, path); err != nil {
						t.Fatal(err)
					}
				}
				if tt.wantPlace == cat.Name {
					wantPath = path
				}
			}
			if tt.shadowed {
				other := catalogue{Name: "lab", URL: exampleURL("lab"), Use: true, Dir: t.TempDir()}
				path, err := catalogueListPath()
				if err != nil {
					t.Fatal(err)
				}
				if err := writeCatalogueList(path, []catalogue{cat, other}); err != nil {
					t.Fatal(err)
				}
				makeContainerPath(t, other.Dir, layout, c, false)
			}
			if tt.badValue {
				c.buildDate = "../x"
			}
			if tt.noHome {
				// Only cache_dir is set, so container_dir's default would fail.
				cache := filepath.Join(filepath.Dir(store), "cache")
				if err := writeConfig(os.Getenv("SCIGET_CONFIG"), map[string]string{"cache_dir": cache}); err != nil {
					t.Fatal(err)
				}
				t.Setenv("HOME", "")
				t.Setenv("XDG_DATA_HOME", "")
			}
			cfg, err := readConfig()
			if err != nil {
				t.Fatal(err)
			}
			got, err := locate(cfg, c)
			if tt.wantErr {
				var missing notOnMachineError
				if err == nil || errors.As(err, &missing) {
					t.Fatalf("error = %v, want a non-missing error", err)
				}
				return
			}
			if tt.wantPlace == "" {
				var missing notOnMachineError
				if !errors.As(err, &missing) || err.Error() != c.name()+" is not on this machine" {
					t.Fatalf("error = %v, want missing container", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got.path != wantPath || got.place != tt.wantPlace {
				t.Errorf("location = %+v, want %s from %s", got, wantPath, tt.wantPlace)
			}
		})
	}
}

func TestApptainerArgs(t *testing.T) {
	tests := []struct {
		name    string
		verb    string
		command []string
		want    []string
	}{
		{name: "exec", verb: "exec", command: []string{"bet", "--help"},
			want: []string{"apptainer", "exec", "/s/fsl.simg", "bet", "--help"}},
		{name: "shell", verb: "shell", want: []string{"apptainer", "shell", "/s/fsl.simg"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := apptainerArgs("apptainer", tt.verb, "/s/fsl.simg", tt.command)
			if !slices.Equal(got, tt.want) {
				t.Errorf("apptainerArgs() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestExecApptainer(t *testing.T) {
	for _, tt := range []struct {
		name, path string
	}{
		{name: "missing executable", path: filepath.Join(t.TempDir(), "missing")},
	} {
		t.Run(tt.name, func(t *testing.T) {
			err := execApptainer(tt.path, []string{"apptainer"}, nil)
			if !errors.Is(err, fs.ErrNotExist) || !strings.HasPrefix(err.Error(), "running apptainer: ") {
				t.Errorf("error = %v, want wrapped missing executable", err)
			}
		})
	}
}
