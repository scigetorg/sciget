package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestLoadCatalogue(t *testing.T) {
	tests := []struct {
		name           string
		json           string // written to apps.json; no file when empty
		dir            bool   // create apps.json as a directory to force a read fail
		want           []string
		licences, dois []string
		apptainerArgs  []string
		entries        []string // "label:exec" per entry, checked when set
		published      download
		wantErr        string
	}{
		{name: "missing file", wantErr: "no catalogue at "},
		{name: "path is a directory", dir: true, wantErr: "reading catalogue: "},
		{name: "invalid JSON", json: "{", wantErr: "unexpected end of JSON input"},
		{name: "null", json: "null", wantErr: "no containers"},
		{name: "no entries", json: `{"fsl": {"apps": {}}}`, wantErr: "no containers"},
		{name: "graphical entry shares the container",
			json: `{"fsl": {"categories": ["a", "b"], "apps": {
				"fsl 6.0.3": {"version": "20200905", "exec": ""},
				"fsleyesGUI-fsl 6.0.3": {"version": "20200905", "exec": "fsleyes"}}}}`,
			want: []string{"fsl/6.0.3 fsl_6.0.3_20200905 [a b] graphical"}},
		{name: "metadata is distinct and sorted",
			json: `{"fsl":{"apps":{
				"fsl 1":{"version":"1","license":["MIT","Apache-2.0"],"doi":"b"},
				"GUI-fsl 1":{"version":"1","license":["MIT","BSD"],"doi":"a"},
				"other-fsl 1":{"version":"1","doi":"b"}}}}`,
			want: []string{"fsl/1 fsl_1_1 []"}, licences: []string{"Apache-2.0", "BSD", "MIT"}, dois: []string{"a", "b"}},
		{name: "apptainer args", json: `{"fsl":{"apps":{
				"fsl 1":{"version":"1","apptainer_args":["--bind","/dev/fuse"]},
				"GUI-fsl 1":{"version":"1","apptainer_args":["--bind","/dev/fuse"]}}}}`,
			want: []string{"fsl/1 fsl_1_1 []"}, apptainerArgs: []string{"--bind", "/dev/fuse"}},
		{name: "size and SHA-256", json: `{"fsl":{"apps":{
				"fsl 1":{"version":"1","size":42,"sha256":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"},
				"GUI-fsl 1":{"version":"1","size":42,"sha256":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}}}}`,
			want: []string{"fsl/1 fsl_1_1 []"}, published: download{size: 42, sha256: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}},
		{name: "SHA-256 in capitals", json: `{"fsl":{"apps":{"fsl 1":{"version":"1","sha256":"AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"}}}}`,
			wantErr: "not 64 lowercase hex digits"},
		{name: "short SHA-256", json: `{"fsl":{"apps":{"fsl 1":{"version":"1","sha256":"abc"}}}}`,
			wantErr: "not 64 lowercase hex digits"},
		{name: "negative size", json: `{"fsl":{"apps":{"fsl 1":{"version":"1","size":-1}}}}`,
			wantErr: "negative size"},
		{name: "SHA-256s that disagree", json: `{"fsl":{"apps":{
				"fsl 1":{"version":"1","sha256":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"},
				"GUI-fsl 1":{"version":"1"}}}}`,
			wantErr: "different sizes or SHA-256s"},
		{name: "apptainer args that disagree", json: `{"fsl":{"apps":{
				"fsl 1":{"version":"1","apptainer_args":["--bind","/dev/fuse"]},
				"GUI-fsl 1":{"version":"1"}}}}`,
			wantErr: "with apptainer_args"},
		{name: "escape in a tool name", json: "{\"fsl\\u001b[2J\": {\"apps\": {\"fsl 1\": {\"version\": \"1\"}}}}",
			wantErr: "has a control character"},
		{name: "bidi override in an entry key", json: `{"fsl": {"apps": {"GUI\u202e-fsl 1": {"version": "1"}}}}`,
			wantErr: `catalogue entry "GUI\u202e-fsl 1" of "fsl" has a control character`},
		{name: "newline in a version", json: `{"fsl": {"apps": {"fsl 1\n": {"version": "1"}}}}`,
			wantErr: "has a control character"},
		{name: "escape in a category", json: `{"fsl": {"categories": ["a\u001b]0;x\u0007"], "apps": {"fsl 1": {"version": "1"}}}}`,
			wantErr: "has a control character"},
		{name: "escape in a licence", json: `{"fsl": {"apps": {"fsl 1": {"version": "1", "license": ["\u001b[2J"]}}}}`,
			wantErr: "has a control character"},
		{name: "escape in a DOI", json: `{"fsl": {"apps": {"fsl 1": {"version": "1", "doi": "\u001b[31m10.1/x"}}}}`,
			wantErr: "has a control character"},
		{name: "escape in a build date", json: `{"fsl": {"apps": {"fsl 1": {"version": "\u001b[31m1"}}}}`,
			wantErr: "has a control character"},
		{name: "escape in exec", json: `{"fsl": {"apps": {"GUI-fsl 1": {"version": "1", "exec": "x\u001b[2J"}}}}`,
			wantErr: "has a control character"},
		{name: "slash in a tool", json: `{"a/b": {"apps": {"a/b 1": {"version": "1"}}}}`,
			wantErr: `with "/" or ".."`},
		{name: "dots in a version", json: `{"fsl": {"apps": {"fsl ..": {"version": "1"}}}}`,
			wantErr: `with "/" or ".."`},
		{name: "dot as a version", json: `{"fsl": {"apps": {"fsl .": {"version": "1"}}}}`,
			wantErr: `or that is "."`},
		{name: "path as a build date", json: `{"fsl": {"apps": {"fsl 1": {"version": "../.."}}}}`,
			wantErr: `with "/" or ".."`},
		{name: "empty apptainer args element", json: `{"fsl":{"apps":{"fsl 1":{"version":"1","apptainer_args":[""]}}}}`,
			wantErr: `apptainer_args element ""`},
		{name: "apptainer args: a word after an option without a value", json: `{"fsl":{"apps":{"fsl 1":{"version":"1","apptainer_args":["--nv","/other.sif"]}}}}`,
			wantErr: `apptainer_args element "/other.sif"`},
		{name: "apptainer args: an option as a value", json: `{"fsl":{"apps":{"fsl 1":{"version":"1","apptainer_args":["--bind","--bind","/other.sif"]}}}}`,
			wantErr: `apptainer_args option "--bind" without a value`},
		{name: "apptainer args: a trailing value option", json: `{"fsl":{"apps":{"fsl 1":{"version":"1","apptainer_args":["--nv","--bind"]}}}}`,
			wantErr: `apptainer_args option "--bind" without a value`},
		{name: "apptainer args: an empty value", json: `{"fsl":{"apps":{"fsl 1":{"version":"1","apptainer_args":["-B",""]}}}}`,
			wantErr: `apptainer_args option "-B" without a value`},
		{name: "apptainer args: a lone dash", json: `{"fsl":{"apps":{"fsl 1":{"version":"1","apptainer_args":["-"]}}}}`,
			wantErr: `apptainer_args element "-"`},
		{name: "apptainer args: an end of options", json: `{"fsl":{"apps":{"fsl 1":{"version":"1","apptainer_args":["--","/other.sif"]}}}}`,
			wantErr: `apptainer_args element "--"`},
		{name: "apptainer args: -B and its value", json: `{"fsl":{"apps":{"fsl 1":{"version":"1","apptainer_args":["-B","/a:/b","--nv"]}}}}`,
			want: []string{"fsl/1 fsl_1_1 []"}, apptainerArgs: []string{"-B", "/a:/b", "--nv"}},
		{name: "apptainer args: a word first", json: `{"fsl":{"apps":{"fsl 1":{"version":"1","apptainer_args":["/other.sif"]}}}}`,
			wantErr: `apptainer_args element "/other.sif", which is not an option or its value`},
		{name: "apptainer args: a word after option=value", json: `{"fsl":{"apps":{"fsl 1":{"version":"1","apptainer_args":["--bind=/a","/other.sif"]}}}}`,
			wantErr: `apptainer_args element "/other.sif"`},
		{name: "apptainer args: two words", json: `{"fsl":{"apps":{"fsl 1":{"version":"1","apptainer_args":["--bind","/a","/other.sif"]}}}}`,
			wantErr: `apptainer_args element "/other.sif"`},
		{name: "entries: visible ones with a command, in key order",
			json: `{"fsl": {"apps": {
				"fsl 6.0.3": {"version": "1"},
				"fsleyesGUI-fsl 6.0.3": {"version": "1", "exec": "fsleyes"},
				"bet-fsl 6.0.3": {"version": "1", "exec": "bet"},
				"hidden-fsl 6.0.3": {"version": "1", "exec": "x", "show_in_applist": false},
				"blank-fsl 6.0.3": {"version": "1", "exec": "  "}}}}`,
			want: []string{"fsl/6.0.3 fsl_6.0.3_1 [] graphical"}, entries: []string{"bet:bet", "fsleyesGUI:fsleyes"}},
		{name: "label with a space",
			json: `{"napari": {"apps": {"napari GUI-napari 0.9.2": {"version": "20250101", "exec": "napari"}}}}`,
			want: []string{"napari/0.9.2 napari_0.9.2_20250101 [] graphical"}},
		{name: "image names the container",
			json: `{"amico": {"apps": {"amico 2.1.0 arm64":
				{"version": "20260512", "image": "amico_2.1.0_arm64", "show_in_applist": false}}}}`,
			want: []string{"amico/2.1.0_arm64 amico_2.1.0_arm64_20260512 [] hidden"}},
		{name: "visible if any entry is",
			json: `{"fsl": {"apps": {
				"fsl 6.0.3": {"version": "1", "show_in_applist": false},
				"fsleyesGUI-fsl 6.0.3": {"version": "1", "show_in_applist": true}}}}`,
			want: []string{"fsl/6.0.3 fsl_6.0.3_1 []"}},
		{name: "sorted by tool, then by version",
			json: `{"fsl": {"apps": {"fsl 6.0.10": {"version": "2"}, "fsl 6.0.9": {"version": "1"}}},
				"afni": {"apps": {"afni 21.2.00": {"version": "3"}}}}`,
			want: []string{"afni/21.2.00 afni_21.2.00_3 []", "fsl/6.0.9 fsl_6.0.9_1 []", "fsl/6.0.10 fsl_6.0.10_2 []"}},
		{name: "conflicting build dates",
			json:    `{"fsl": {"apps": {"fsl 6.0.3": {"version": "1"}, "fsleyesGUI-fsl 6.0.3": {"version": "2"}}}}`,
			wantErr: "catalogue lists fsl/6.0.3 with build dates "},
		{name: "key without a version", json: `{"fsl": {"apps": {"fsl": {"version": "1"}}}}`,
			wantErr: `catalogue entry "fsl" has no version`},
		{name: "key ending in a space", json: `{"fsl": {"apps": {"fsl ": {"version": "1"}}}}`,
			wantErr: `catalogue entry "fsl " has no version`},
		{name: "image without a version", json: `{"fsl": {"apps": {"fsl x": {"version": "1", "image": "fsl_"}}}}`,
			wantErr: `catalogue entry "fsl x" has no version`},
		{name: "image for another tool", json: `{"fsl": {"apps": {"fsl 1.0": {"version": "1", "image": "notfsl_1.0"}}}}`,
			wantErr: `catalogue entry "fsl 1.0" has image "notfsl_1.0", which does not start with "fsl_"`},
		{name: "no build date", json: `{"fsl": {"apps": {"fsl 6.0.3": {"exec": ""}}}}`,
			wantErr: `catalogue entry "fsl 6.0.3" has no build date`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "apps.json")
			if tt.json != "" {
				if err := os.WriteFile(path, []byte(tt.json), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			if tt.dir {
				if err := os.Mkdir(path, 0o755); err != nil {
					t.Fatal(err)
				}
			}
			containers, err := loadCatalogue(path)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("error = %v, want it to contain %q", err, tt.wantErr)
				}
				prefix := "reading catalogue " + path + ": "
				switch {
				case tt.dir:
					// os errors are *fs.PathError, which already include the path.
					prefix = "reading catalogue: "
				case tt.json == "":
					prefix = "no catalogue at " + path + "; run 'sciget refresh'"
				}
				if !strings.HasPrefix(err.Error(), prefix) {
					t.Errorf("error = %q, want it to start with %q", err, prefix)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			var got []string
			for _, c := range containers {
				if !slices.Equal(c.licences, tt.licences) || !slices.Equal(c.dois, tt.dois) {
					t.Errorf("metadata = %v, %v, want %v, %v", c.licences, c.dois, tt.licences, tt.dois)
				}
				if tt.entries != nil {
					var got []string
					for _, e := range c.entries {
						got = append(got, e.label+":"+e.exec)
					}
					if !slices.Equal(got, tt.entries) {
						t.Errorf("entries = %q, want %q", got, tt.entries)
					}
				}
				if c.published != tt.published {
					t.Errorf("published = %+v, want %+v", c.published, tt.published)
				}
				if !slices.Equal(c.apptainerArgs, tt.apptainerArgs) {
					t.Errorf("apptainer args = %q, want %q", c.apptainerArgs, tt.apptainerArgs)
				}
				s := fmt.Sprintf("%s %s %v", c.name(), c.fileName(), c.categories)
				if c.graphical {
					s += " graphical"
				}
				if c.hidden {
					s += " hidden"
				}
				got = append(got, s)
			}
			if !slices.Equal(got, tt.want) {
				t.Errorf("containers = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestReadContainers(t *testing.T) {
	use := []catalogue{{Name: "neuro", URL: exampleURL("neuro"), Use: true}}
	for _, tt := range []struct {
		name       string
		catalogues []catalogue
		apps       string // the catalogue's apps.json; not downloaded when empty
		wantIs     error
		wantName   string // the catalogueError's name
		missing    bool   // a notDownloadedError
		want       int
	}{
		{name: "no catalogues", wantIs: errNoCatalogues},
		{name: "none in use", catalogues: []catalogue{{Name: "neuro", URL: exampleURL("neuro")}}, wantIs: errNoneInUse},
		{name: "not downloaded", catalogues: use, wantName: "neuro", missing: true},
		{name: "broken", catalogues: use, apps: "{", wantName: "neuro"},
		{name: "read", catalogues: use, apps: `{"fsl": {"apps": {"fsl 1": {"version": "1"}}}}`, want: 1},
	} {
		t.Run(tt.name, func(t *testing.T) {
			base := t.TempDir()
			t.Setenv("SCIGET_CONFIG", filepath.Join(base, "config.json"))
			cacheDir := filepath.Join(base, "cache")
			if err := writeConfig(os.Getenv("SCIGET_CONFIG"), map[string]string{"cache_dir": cacheDir}); err != nil {
				t.Fatal(err)
			}
			path, err := catalogueListPath()
			if err != nil {
				t.Fatal(err)
			}
			if err := writeCatalogueList(path, tt.catalogues); err != nil {
				t.Fatal(err)
			}
			if tt.apps != "" {
				dir := tt.catalogues[0].dir(cacheDir)
				if err := os.MkdirAll(dir, 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(dir, "apps.json"), []byte(tt.apps), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			got, err := readContainers()
			if tt.wantIs != nil && !errors.Is(err, tt.wantIs) {
				t.Errorf("error = %v, want %v", err, tt.wantIs)
			}
			var catErr *catalogueError
			if errors.As(err, &catErr) != (tt.wantName != "") || tt.wantName != "" && catErr.name != tt.wantName {
				t.Errorf("error = %v, want a catalogueError named %q", err, tt.wantName)
			}
			var missing *notDownloadedError
			if errors.As(err, &missing) != tt.missing {
				t.Errorf("error = %v, want notDownloadedError %v", err, tt.missing)
			}
			if len(got) != tt.want {
				t.Errorf("read %d containers, want %d", len(got), tt.want)
			}
		})
	}
}

func TestCompareVersions(t *testing.T) {
	tests := []struct {
		name string
		a, b string
		want int
	}{
		{name: "equal", a: "6.0.3", b: "6.0.3"},
		{name: "numbers, not text", a: "6.0.9", b: "6.0.10", want: -1},
		{name: "numbers, not text, reversed", a: "6.0.10", b: "6.0.9", want: 1},
		{name: "shorter first", a: "1.2", b: "1.2.1", want: -1},
		{name: "leading zeros", a: "21.02", b: "21.2"},
		{name: "text compared as text", a: "1.0rc1", b: "1.0rc2", want: -1},
		{name: "prefix", a: "v1.0.20240202", b: "v1.0.20250101", want: -1},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := compareVersions(tt.a, tt.b); got != tt.want {
				t.Errorf("compareVersions(%q, %q) = %d, want %d", tt.a, tt.b, got, tt.want)
			}
		})
	}
}

func TestLookupContainer(t *testing.T) {
	for _, tt := range []struct {
		name          string
		query         string
		wantCatalogue string
		wantErr       string
	}{
		{name: "first wins", query: "fsl/6.0.3", wantCatalogue: "neuro"},
		{name: "unknown version", query: "fsl/9", wantErr: "no container named \"fsl/9\"; run 'sciget find fsl'"},
		{name: "unknown tool", query: "spm", wantErr: "no container named \"spm\"; run 'sciget find spm'"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			got, err := lookupContainer(testContainers, tt.query)
			if tt.wantErr != "" {
				if err == nil || !strings.HasPrefix(err.Error(), tt.wantErr) {
					t.Fatalf("error = %v, want %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got.catalogue != tt.wantCatalogue {
				t.Errorf("catalogue = %q, want %q", got.catalogue, tt.wantCatalogue)
			}
		})
	}
}

func TestPrintable(t *testing.T) {
	for _, tt := range []struct {
		name, in, want string
	}{
		{name: "plain", in: "itksnap 4.4.0", want: "itksnap 4.4.0"},
		{name: "whole escape sequence", in: "evil\x1b[2Jname", want: "evilname"},
		{name: "colour codes", in: "\x1b[31merror\x1b[0m: x", want: "error: x"},
		{name: "OSC title", in: "a\x1b]0;title\x07b", want: "ab"},
		{name: "bidi override and zero width", in: "a\u202eb\u200bc", want: "abc"},
		{name: "newline and tab", in: "a\nb\tc", want: "abc"},
		{name: "DEL and C1", in: "a\x7fb\u0085c\u009bd", want: "abcd"},
		{name: "unicode kept", in: "Zürich › ✓", want: "Zürich › ✓"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if got := printable(tt.in); got != tt.want {
				t.Errorf("printable(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}
