package main

import (
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
		{name: "apptainer args that disagree", json: `{"fsl":{"apps":{
				"fsl 1":{"version":"1","apptainer_args":["--bind","/dev/fuse"]},
				"GUI-fsl 1":{"version":"1"}}}}`,
			wantErr: "with apptainer_args"},
		{name: "empty apptainer args element", json: `{"fsl":{"apps":{"fsl 1":{"version":"1","apptainer_args":[""]}}}}`,
			wantErr: "has an empty or NUL apptainer_args element"},
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
