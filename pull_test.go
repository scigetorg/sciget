package main

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func TestPullContainer(t *testing.T) {
	const good, bad = "a good image", "a bad image"
	// source is one download server: it serves body, or status if set.
	type source struct {
		body   string
		status int
	}
	tests := []struct {
		name        string
		sources     []source
		inStore     bool   // the container is already in the store
		apptainer   string // apptainer_path; the fake script when empty
		warnMissing bool
		want        string // the pulled file's content; nothing pulled when empty
		wantWarn    bool
		wantErr     []string
		wantGets    int // GET requests across all sources
	}{
		{name: "pull", sources: []source{{body: good}}, want: good, wantGets: 1},
		{name: "first source missing", sources: []source{{status: http.StatusNotFound}, {body: good}}, want: good, wantGets: 1},
		{name: "Apptainer cannot open the first", sources: []source{{body: bad}, {body: good}}, want: good, wantGets: 2},
		{name: "already in the store", sources: []source{{body: good}}, inStore: true},
		{name: "warning when asked", sources: []source{{body: good}}, warnMissing: true, want: good, wantWarn: true, wantGets: 1},
		{name: "every source fails", sources: []source{{status: http.StatusNotFound}, {body: bad}},
			wantErr: []string{"could not download fsl/6.0.3:", "404", "apptainer cannot open the download", "not a SIF"}, wantGets: 1},
		{name: "no Apptainer", sources: []source{{body: good}}, apptainer: "no-such-apptainer",
			wantErr: []string{"apptainer not found"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c, _, store := localCatalogue(t)
			gets := 0
			var urls []string
			for _, s := range tt.sources {
				srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if r.URL.Path != "/fsl_6.0.3_20200905.simg" || s.status != 0 {
						w.WriteHeader(max(s.status, http.StatusNotFound))
						return
					}
					w.Header().Set("Content-Length", strconv.Itoa(len(s.body)))
					if r.Method == http.MethodGet {
						gets++
						w.Write([]byte(s.body))
					}
				}))
				defer srv.Close()
				urls = append(urls, `"`+srv.URL+`/{file}"`)
			}
			// The fake Apptainer opens only images that start with "a good".
			apptainer := tt.apptainer
			if apptainer == "" {
				apptainer = filepath.Join(t.TempDir(), "apptainer")
				script := "#!/bin/sh\n" +
					`[ "$1" = --silent ] && [ "$2" = exec ] && [ "$4" = /bin/true ] || exit 2` + "\n" +
					`case "$(head -c 6 "$3")" in "a good") exit 0;; esac` + "\n" +
					"echo 'FATAL: not a SIF' >&2; exit 1\n"
				if err := os.WriteFile(apptainer, []byte(script), 0o755); err != nil {
					t.Fatal(err)
				}
			}
			base := filepath.Dir(store)
			if err := writeConfig(os.Getenv("SCIGET_CONFIG"), map[string]string{
				"cache_dir": filepath.Join(base, "cache"), "container_dir": store,
				"apptainer_path": apptainer, "download_urls": "[" + strings.Join(urls, ",") + "]",
				"warn_missing_checksum": strconv.FormatBool(tt.warnMissing),
			}); err != nil {
				t.Fatal(err)
			}
			cfg, err := readConfig()
			if err != nil {
				t.Fatal(err)
			}
			layout, _, _ := cfg.get("container_layout")
			var existing string
			if tt.inStore {
				existing = makeContainerPath(t, store, layout, c, false)
			}

			var warnings []string
			loc, downloaded, err := pullContainer(cfg, c, nil, func(s string) { warnings = append(warnings, s) })
			if tt.wantErr != nil {
				if err == nil {
					t.Fatal("no error")
				}
				for _, want := range tt.wantErr {
					if !strings.Contains(err.Error(), want) {
						t.Errorf("err = %q, want it to contain %q", err, want)
					}
				}
			} else if err != nil {
				t.Fatal(err)
			}
			if gets != tt.wantGets {
				t.Errorf("GET requests = %d, want %d", gets, tt.wantGets)
			}
			if (len(warnings) > 0) != tt.wantWarn {
				t.Errorf("warnings = %q, want some: %v", warnings, tt.wantWarn)
			}
			switch {
			case tt.inStore:
				if downloaded || loc.path != existing {
					t.Errorf("got %+v, downloaded %v; want the existing %s", loc, downloaded, existing)
				}
			case tt.want != "":
				got, err := os.ReadFile(loc.path)
				if err != nil || string(got) != tt.want || !downloaded || loc.place != "store" {
					t.Errorf("pulled %q (%v) at %+v, downloaded %v; want %q in the store", got, err, loc, downloaded, tt.want)
				}
				if found, err := locate(cfg, c); err != nil || found.path != loc.path {
					t.Errorf("locate = %+v, %v; want %s", found, err, loc.path)
				}
				if info, err := os.Stat(loc.path); err != nil || info.Mode().Perm() != 0o644 {
					t.Errorf("mode = %v, %v; want 0644", info.Mode(), err)
				}
			}
			leftovers, _ := filepath.Glob(filepath.Join(store, "*", "*.incomplete"))
			if len(leftovers) > 0 {
				t.Errorf("leftover files: %q", leftovers)
			}
		})
	}
}

func TestDownloadURLs(t *testing.T) {
	cfg := config{overrides: map[string]string{"download_urls": `["https://a.example/c/{file}","https://b.example/{file}?x=1"]`}}
	c := container{tool: "fsl", version: "6.0.3", image: "fsl_6.0.3", buildDate: "20200905"}
	got, err := downloadURLs(cfg, c)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"https://a.example/c/fsl_6.0.3_20200905.simg", "https://b.example/fsl_6.0.3_20200905.simg?x=1"}
	if strings.Join(got, " ") != strings.Join(want, " ") {
		t.Errorf("got %q, want %q", got, want)
	}
}
