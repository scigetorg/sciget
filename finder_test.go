package main

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestParseFinder(t *testing.T) {
	tests := []struct {
		name    string
		out     string
		want    commandList
		wantErr bool
	}{
		{name: "all parts",
			out: "PATH=/bin\nDEPLOY_BINS=conda::pip\nDEPLOY_ENV_FSLDIR=BASEPATH/opt/fsl\n" +
				filesMarker + "\n/opt/fsl/bin/bet\n/opt/fsl/bin/fslmaths\n" + readmeMarker + "\n# FSL\nhelp\n",
			want: commandList{Found: []string{"bet", "fslmaths"}, Kept: []string{"conda", "pip"},
				Env: []string{"FSLDIR=BASEPATH/opt/fsl"}, Help: "# FSL\nhelp\n"}},
		{name: "nothing found", out: filesMarker + "\n" + readmeMarker + "\n"},
		{name: "no markers", out: "PATH=/bin\n", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseFinder(tt.out)
			if (err != nil) != tt.wantErr {
				t.Fatalf("err = %v, want error: %v", err, tt.wantErr)
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("got %+v, want %+v", got, tt.want)
			}
		})
	}
}

func TestListCommands(t *testing.T) {
	tests := []struct {
		name      string
		cached    bool // run the finder once before the call
		failing   bool // the fake Apptainer fails
		want      commandList
		wantRuns  int // finder runs, including any for cached
		wantErrIn string
	}{
		{name: "runs the finder", want: finderWant, wantRuns: 1},
		{name: "cached", cached: true, want: finderWant, wantRuns: 1},
		{name: "finder fails", failing: true, wantRuns: 1, wantErrIn: "listing the commands of fsl/6.0.3"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c, _, store := localCatalog(t)
			base := filepath.Dir(store)
			apptainer, runs := fakeFinderApptainer(t, tt.failing)
			if err := writeConfig(os.Getenv("SCIGET_CONFIG"), map[string]string{
				"cache_dir": filepath.Join(base, "cache"), "container_dir": store, "apptainer_path": apptainer,
			}); err != nil {
				t.Fatal(err)
			}
			cfg, err := readConfig()
			if err != nil {
				t.Fatal(err)
			}
			loc := location{path: filepath.Join(store, "fsl.simg")}
			if tt.cached {
				if _, err := listCommands(context.Background(), cfg, c, loc); err != nil {
					t.Fatal(err)
				}
			}

			got, err := listCommands(context.Background(), cfg, c, loc)
			if tt.wantErrIn != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErrIn) {
					t.Errorf("err = %v, want it to contain %q", err, tt.wantErrIn)
				}
			} else if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("got %+v, want %+v", got, tt.want)
			}
			if n := runs(); n != tt.wantRuns {
				t.Errorf("finder runs = %d, want %d", n, tt.wantRuns)
			}
		})
	}
}

func TestListCommandsOlderBuildCache(t *testing.T) {
	c, _, store := localCatalog(t)
	base := filepath.Dir(store)
	apptainer, runs := fakeFinderApptainer(t, false)
	cacheDir := filepath.Join(base, "cache")
	if err := writeConfig(os.Getenv("SCIGET_CONFIG"), map[string]string{
		"cache_dir": cacheDir, "container_dir": store, "apptainer_path": apptainer,
	}); err != nil {
		t.Fatal(err)
	}
	cfg, err := readConfig()
	if err != nil {
		t.Fatal(err)
	}
	// Pre-populate a legacy v1 cache file under commands/ and verify v2 ignores it.
	legacyPath := filepath.Join(cacheDir, "commands", "neuro", "fsl", "6.0.3", c.buildDate+".json")
	if err := os.MkdirAll(filepath.Dir(legacyPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(legacyPath, []byte(`{"found":["stale-legacy"]}`), 0o644); err != nil {
		t.Fatal(err)
	}
	// An older build with known buildDate caches under its own date in commands-v2, never c.buildDate.
	olderLoc := location{path: filepath.Join(store, "old.simg"), older: true, buildDate: "20190101"}
	gotOlder, err := listCommands(context.Background(), cfg, c, olderLoc)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(gotOlder, finderWant) {
		t.Errorf("gotOlder = %+v, want %+v", gotOlder, finderWant)
	}
	if _, err := os.Stat(filepath.Join(cacheDir, "commands-v2", "neuro", "fsl", "6.0.3", "20190101.json")); err != nil {
		t.Errorf("older build cache missing: %v", err)
	}
	if _, err := os.Stat(filepath.Join(cacheDir, "commands-v2", "neuro", "fsl", "6.0.3", c.buildDate+".json")); !os.IsNotExist(err) {
		t.Error("older build poisoned catalog's current buildDate cache")
	}

	// An older build with unknown buildDate skips caching entirely.
	unknownOlder := location{path: filepath.Join(store, "unknown.simg"), older: true}
	if _, err := listCommands(context.Background(), cfg, c, unknownOlder); err != nil {
		t.Fatal(err)
	}
	if runs() != 2 {
		t.Errorf("runs = %d, want 2", runs())
	}
	if _, err := os.Stat(filepath.Join(cacheDir, "commands-v2", "neuro", "fsl", "6.0.3", c.buildDate+".json")); !os.IsNotExist(err) {
		t.Error("unknown older build wrote into catalog's current buildDate cache")
	}
}

// finderWant is what the fake Apptainer's container offers.
var finderWant = commandList{Found: []string{"bet"}, Kept: []string{"conda"}, Env: []string{"FSLDIR=BASEPATH/opt/fsl"}}

// fakeFinderApptainer writes an Apptainer that runs the finder's script on
// the host, with a DEPLOY_PATH holding one executable, a plain file and a
// folder, and returns its path and a count of its runs.
func fakeFinderApptainer(t *testing.T, failing bool) (string, func() int) {
	t.Helper()
	dir := t.TempDir()
	bin := filepath.Join(dir, "bin")
	if err := os.MkdirAll(filepath.Join(bin, "subdir"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bin, "bet"), nil, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bin, "notes.txt"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	log := filepath.Join(dir, "runs")
	script := "#!/bin/sh\necho run >> " + shellJoin([]string{log}) + "\n"
	if failing {
		script += "echo 'FATAL: could not open image' >&2; exit 255\n"
	} else {
		// Arguments: --silent exec --cleanenv <image> /bin/sh -c <script>.
		script += `[ "$1 $2 $3" = "--silent exec --cleanenv" ] || exit 2` + "\nshift 4\n" +
			"exec env -i PATH=\"$PATH\" DEPLOY_PATH=" + shellJoin([]string{bin + "::/no/such/dir"}) +
			" DEPLOY_BINS=conda DEPLOY_ENV_FSLDIR=BASEPATH/opt/fsl \"$@\"\n"
	}
	apptainer := filepath.Join(dir, "apptainer")
	if err := os.WriteFile(apptainer, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return apptainer, func() int {
		data, _ := os.ReadFile(log)
		return strings.Count(string(data), "run")
	}
}
