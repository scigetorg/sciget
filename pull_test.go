package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
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
		olderBuild  bool   // an older build of it is in the store
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
		{name: "an older build is still pulled", sources: []source{{body: good}}, olderBuild: true, want: good, wantGets: 1},
		{name: "warning when asked", sources: []source{{body: good}}, warnMissing: true, want: good, wantWarn: true, wantGets: 1},
		{name: "every source fails", sources: []source{{status: http.StatusNotFound}, {body: bad}},
			wantErr: []string{"could not download fsl/6.0.3:", "404", "apptainer cannot open the download", "not a SIF"}, wantGets: 1},
		{name: "no Apptainer", sources: []source{{body: good}}, apptainer: "no-such-apptainer",
			wantErr: []string{"apptainer not found"}},
		{name: "no mirrors, before no Apptainer", apptainer: "no-such-apptainer",
			wantErr: []string{`catalog "neuro" has no download mirrors`}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c, _, store := localCatalog(t)
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
				urls = append(urls, srv.URL+"/{file}")
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
				"apptainer_path":        apptainer,
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
			if tt.olderBuild {
				older := c
				older.buildDate = "20190101"
				makeContainerPath(t, store, layout, older, false)
			}

			var warnings []string
			c.mirrors = urls
			loc, downloaded, err := pullContainer(context.Background(), cfg, c, nil, func(s string) { warnings = append(warnings, s) })
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
				if err != nil || string(got) != tt.want || !downloaded || loc.share != "" {
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
	tests := []struct {
		name    string
		mirrors []string
		want    []string
		wantErr string
	}{
		{name: "each mirror", mirrors: []string{"https://a.example/c/{file}", "https://b.example/{file}?x=1"},
			want: []string{"https://a.example/c/fsl_6.0.3_20200905.simg", "https://b.example/fsl_6.0.3_20200905.simg?x=1"}},
		{name: "none", wantErr: `catalog "neuro" has no download mirrors`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := container{tool: "fsl", version: "6.0.3", image: "fsl_6.0.3", buildDate: "20200905", catalog: "neuro", mirrors: tt.mirrors}
			got, err := downloadURLs(c)
			if tt.wantErr != "" {
				if err == nil || err.Error() != tt.wantErr {
					t.Errorf("error = %v, want %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if !slices.Equal(got, tt.want) {
				t.Errorf("got %q, want %q", got, tt.want)
			}
		})
	}
}

func TestAnswerPull(t *testing.T) {
	tests := []struct {
		line         string
		pull, always bool
	}{
		{line: "\n", pull: true, always: true},
		{line: "a\n", pull: true, always: true},
		{line: " Always \n", pull: true, always: true},
		{line: "y\n", pull: true},
		{line: "YES\n", pull: true},
		{line: "n\n"},
		{line: "maybe\n"},
	}
	for _, tt := range tests {
		if pull, always := answerPull(tt.line); pull != tt.pull || always != tt.always {
			t.Errorf("answerPull(%q) = %v, %v; want %v, %v", tt.line, pull, always, tt.pull, tt.always)
		}
	}
}

// failingReader fails every read.
type failingReader struct{}

func (failingReader) Read([]byte) (int, error) { return 0, errors.New("broken") }

func TestPullOnRun(t *testing.T) {
	const good = "a good image"
	tests := []struct {
		name        string
		auto        bool
		interactive bool
		in          io.Reader
		share       string // "missing" (the default), "present" or "none"
		noMirrors   bool   // the catalog has no download mirrors
		wantPull    bool
		wantAuto    bool     // auto_pull in the file afterwards
		wantAsked   []string // in the question
		wantErr     string   // asks nothing and fails with this
	}{
		{name: "not interactive", in: strings.NewReader("\n")},
		{name: "auto_pull set", auto: true, wantPull: true, wantAuto: true},
		{name: "Enter means Always", interactive: true, in: strings.NewReader("\n"), wantPull: true, wantAuto: true,
			wantAsked: []string{"the neuro share", "is not mounted", "[y]es, [n]o, [A]lways (yes, saved as the default): "}},
		{name: "yes, once", interactive: true, in: strings.NewReader("y\n"), wantPull: true},
		{name: "no", interactive: true, in: strings.NewReader("n\n")},
		{name: "Ctrl-D", interactive: true, in: strings.NewReader("")},
		{name: "Ctrl-D after typing", interactive: true, in: strings.NewReader("a")},
		{name: "read error", interactive: true, in: failingReader{}},
		{name: "share present", interactive: true, in: strings.NewReader("n\n"), share: "present",
			wantAsked: []string{"is not on the neuro share", "and hasn't been downloaded", "check CVMFS first"}},
		{name: "no share", interactive: true, in: strings.NewReader("n\n"), share: "none",
			wantAsked: []string{"fsl/6.0.3 hasn't been downloaded.\n"}},
		{name: "no mirrors, so nothing to ask", interactive: true, in: strings.NewReader("y\n"), noMirrors: true,
			wantErr: `is not mounted. If it should be, fix CVMFS first. Catalog "neuro" has no download mirrors, so there is nothing to run`},
		{name: "no mirrors, with auto_pull set", auto: true, wantAuto: true, noMirrors: true,
			wantErr: `has no download mirrors, so there is nothing to run`},
		{name: "no mirrors, not interactive", noMirrors: true, wantErr: `has no download mirrors, so there is nothing to run`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c, cat, store := localCatalog(t)
			switch tt.share {
			case "present":
				if err := os.MkdirAll(cat.Dir, 0o755); err != nil {
					t.Fatal(err)
				}
			case "none":
				cat.Dir = ""
				path, _ := catalogListPath()
				if err := writeCatalogList(path, []catalog{cat}, false); err != nil {
					t.Fatal(err)
				}
			}
			gets := 0
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Length", strconv.Itoa(len(good)))
				if r.Method == http.MethodGet {
					gets++
					w.Write([]byte(good))
				}
			}))
			defer srv.Close()
			apptainer := filepath.Join(t.TempDir(), "apptainer")
			if err := os.WriteFile(apptainer, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
				t.Fatal(err)
			}
			path := os.Getenv("SCIGET_CONFIG")
			if err := writeConfig(path, map[string]string{
				"cache_dir": filepath.Join(filepath.Dir(store), "cache"), "container_dir": store,
				"apptainer_path": apptainer,
				"auto_pull":      strconv.FormatBool(tt.auto),
			}); err != nil {
				t.Fatal(err)
			}
			cfg, err := readConfig()
			if err != nil {
				t.Fatal(err)
			}

			var stderr strings.Builder
			c.mirrors = []string{srv.URL + "/{file}"}
			if tt.noMirrors {
				c.mirrors = nil
			}
			loc, err := pullOnRun(cfg, c, tt.in, &stderr, tt.interactive)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) || stderr.Len() > 0 || gets > 0 {
					t.Errorf("error = %v, asked %q, GETs %d; want %q, nothing asked or fetched", err, stderr.String(), gets, tt.wantErr)
				}
				return
			}
			var missing notOnMachineError
			if tt.wantPull && (err != nil || loc.share != "") || !tt.wantPull && !errors.As(err, &missing) {
				t.Errorf("got %+v, %v; want pulled: %v", loc, err, tt.wantPull)
			}
			if (gets > 0) != tt.wantPull {
				t.Errorf("GET requests = %d, want some: %v", gets, tt.wantPull)
			}
			after, err := loadConfig(path)
			if err != nil {
				t.Fatal(err)
			}
			if auto, _ := after.getBool("auto_pull"); auto != tt.wantAuto {
				t.Errorf("auto_pull = %v, want %v", auto, tt.wantAuto)
			}
			for _, want := range tt.wantAsked {
				if !strings.Contains(stderr.String(), want) {
					t.Errorf("asked %q, want it to contain %q", stderr.String(), want)
				}
			}
		})
	}
}

func TestPullContainerCancel(t *testing.T) {
	c, _, store := localCatalog(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", "1000000")
		if r.Method == http.MethodGet {
			w.Write(make([]byte, 1000))
			w.(http.Flusher).Flush()
			<-r.Context().Done() // stall until the client gives up
		}
	}))
	defer srv.Close()
	if err := writeConfig(os.Getenv("SCIGET_CONFIG"), map[string]string{
		"cache_dir": filepath.Join(filepath.Dir(store), "cache"), "container_dir": store,
		"apptainer_path": "true",
	}); err != nil {
		t.Fatal(err)
	}
	cfg, err := readConfig()
	if err != nil {
		t.Fatal(err)
	}
	c.mirrors = []string{srv.URL + "/{file}", srv.URL + "/again/{file}"}
	_, _, err = pullContainer(ctx, cfg, c, func(string, int64, int64) { cancel() }, nil)
	if !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want context.Canceled", err)
	}
	if leftovers, _ := filepath.Glob(filepath.Join(store, "*", "*.incomplete")); len(leftovers) > 0 {
		t.Errorf("leftover files: %q", leftovers)
	}
}

func TestPullAll(t *testing.T) {
	c, cat, store := localCatalog(t)
	// Four containers: fsl/1 to fsl/4.
	var apps []string
	var cs []container
	for i := 1; i <= 4; i++ {
		v := strconv.Itoa(i)
		apps = append(apps, `"fsl `+v+`":{"version":"20200905"}`)
		cs = append(cs, container{tool: "fsl", version: v, image: "fsl_" + v, buildDate: "20200905", catalog: c.catalog})
	}
	cache := cat.dir(filepath.Join(filepath.Dir(store), "cache"))
	if err := os.WriteFile(filepath.Join(cache, "apps.json"), []byte(`{"fsl":{"apps":{`+strings.Join(apps, ",")+`}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	const good = "a good image"
	var mu sync.Mutex
	active, most := 0, 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "fsl_4_") {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Length", strconv.Itoa(len(good)))
		if r.Method != http.MethodGet {
			return
		}
		mu.Lock()
		active++
		most = max(most, active)
		mu.Unlock()
		time.Sleep(50 * time.Millisecond)
		w.Write([]byte(good))
		mu.Lock()
		active--
		mu.Unlock()
	}))
	defer srv.Close()
	if err := writeConfig(os.Getenv("SCIGET_CONFIG"), map[string]string{
		"cache_dir": filepath.Join(filepath.Dir(store), "cache"), "container_dir": store,
		"apptainer_path": "true", "parallel_pulls": "2",
	}); err != nil {
		t.Fatal(err)
	}
	cfg, err := readConfig()
	if err != nil {
		t.Fatal(err)
	}
	layout, _, _ := cfg.get("container_layout")
	makeContainerPath(t, store, layout, cs[0], false) // fsl/1 is already there

	var stdout, stderr strings.Builder
	for i := range cs {
		cs[i].mirrors = []string{srv.URL + "/{file}"}
	}
	err = pullAll(context.Background(), &stdout, &stderr, cfg, cs)
	if err == nil || !strings.Contains(err.Error(), "1 of 4 containers failed") || !strings.Contains(err.Error(), "fsl/4") {
		t.Errorf("err = %v, want fsl/4 to fail", err)
	}
	for _, want := range []string{"fsl/1 is already on the machine", "Pulled fsl/2 to", "Pulled fsl/3 to"} {
		if !strings.Contains(stdout.String(), want) {
			t.Errorf("stdout = %q, want it to contain %q", stdout.String(), want)
		}
	}
	if most != 2 {
		t.Errorf("at most %d downloads ran at once, want 2", most)
	}
}

func TestPullCancelledBeforePublish(t *testing.T) {
	body := []byte("valid-image-body")
	sum := sha256.Sum256(body)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", strconv.Itoa(len(body)))
		if r.Method == http.MethodGet {
			_, _ = w.Write(body)
		}
	}))
	defer srv.Close()

	c, _, store := localCatalog(t)
	c.mirrors = []string{srv.URL + "/{file}"}
	c.published = download{size: int64(len(body)), sha256: hex.EncodeToString(sum[:])}
	c.ambiguousStore = true
	if err := writeConfig(os.Getenv("SCIGET_CONFIG"), map[string]string{
		"cache_dir":      filepath.Join(filepath.Dir(store), "cache"),
		"container_dir":  store,
		"apptainer_path": "true",
	}); err != nil {
		t.Fatal(err)
	}
	cfg, err := readConfig()
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancelCause(context.Background())
	defer cancel(nil)
	_, downloaded, err := pullContainer(ctx, cfg, c, func(_ string, done, total int64) {
		if done == total {
			cancel(errors.New("interrupt signal received"))
		}
	}, nil)
	if err == nil || downloaded {
		t.Fatalf("pullContainer = downloaded %v, err %v; want cancellation error", downloaded, err)
	}
	layout, _, _ := cfg.get("container_layout")
	rel, _ := layoutPath(layout, c)
	dest := filepath.Join(store, rel)
	if _, err := os.Stat(dest); !os.IsNotExist(err) {
		t.Errorf("cancelled pull must not publish %s", dest)
	}
	if matches, _ := filepath.Glob(filepath.Join(filepath.Dir(dest), "*.incomplete")); len(matches) != 0 {
		t.Errorf("cancelled pull left temporary files: %v", matches)
	}
}
