package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestReadContainerInfo(t *testing.T) {
	for _, tt := range []struct {
		name     string
		local    bool
		shadowed bool
		statErr  bool
		noCats   bool
	}{
		{name: "missing container"},
		{name: "local container", local: true},
		{name: "shadowed metadata is excluded", shadowed: true},
		{name: "stat error is returned", local: true, statErr: true},
		{name: "no categories", noCats: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			c, cat, _ := localCatalog(t)
			layout, err := defaultValue("container_layout")
			if err != nil {
				t.Fatal(err)
			}
			wantLocation := "not on this machine"
			if tt.local {
				wantLocation = makeContainerPath(t, cat.Dir, layout, c, false)
			}
			if tt.statErr {
				if err := os.Remove(wantLocation); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(wantLocation, wantLocation); err != nil {
					t.Fatal(err)
				}
			}
			wantCategories := "imaging"
			if tt.noCats {
				cfg, err := readConfig()
				if err != nil {
					t.Fatal(err)
				}
				cacheDir, _, err := cfg.get("cache_dir")
				if err != nil {
					t.Fatal(err)
				}
				apps := `{"fsl":{"apps":{"fsl 6.0.3":{"version":"20200905","license":["MIT","Apache-2.0"],"doi":"https://doi.org/1"},
					"GUI-fsl 6.0.3":{"version":"20200905","exec":"gui","license":["MIT"],"doi":"https://doi.org/2"}}}}`
				if err := os.WriteFile(filepath.Join(cat.dir(cacheDir), "apps.json"), []byte(apps), 0o600); err != nil {
					t.Fatal(err)
				}
				wantCategories = "(none)"
			}
			wantCatalog := "neuro"
			if tt.shadowed {
				other := catalog{Name: "lab", URL: exampleURL("lab"), Use: true}
				path, err := catalogListPath()
				if err != nil {
					t.Fatal(err)
				}
				if err := writeCatalogList(path, []catalog{cat, other}, false); err != nil {
					t.Fatal(err)
				}
				cfg, err := readConfig()
				if err != nil {
					t.Fatal(err)
				}
				cacheDir, _, err := cfg.get("cache_dir")
				if err != nil {
					t.Fatal(err)
				}
				dir := other.dir(cacheDir)
				if err := os.MkdirAll(dir, 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(dir, "apps.json"), []byte(`{"fsl":{"apps":{"fsl 6.0.3":{"version":"1","license":["other"],"doi":"other"}}}}`), 0o600); err != nil {
					t.Fatal(err)
				}
				wantCatalog += " (also in: lab, shadowed)"
			}
			got, err := readContainerInfo(c.name())
			if tt.statErr {
				if err == nil || !strings.Contains(err.Error(), "locating fsl/6.0.3:") {
					t.Fatalf("error = %v, want stat error", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			want := containerInfo{name: c.name(), catalog: wantCatalog,
				build: "20200905 (fsl_6.0.3_20200905)", categories: wantCategories,
				licence: "Apache-2.0, MIT", doi: "https://doi.org/1, https://doi.org/2", gui: "yes", location: wantLocation}
			if got != want {
				t.Errorf("info = %+v, want %+v", got, want)
			}
		})
	}
}
