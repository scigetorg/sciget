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
		name    string
		json    string // written to apps.json; no file when empty
		want    []string
		wantErr string
	}{
		{name: "missing file", wantErr: "no catalogue at "},
		{name: "invalid JSON", json: "{", wantErr: "reading catalogue "},
		{name: "null", json: "null", wantErr: " has no containers"},
		{name: "no entries", json: `{"fsl": {"apps": {}}}`, wantErr: " has no containers"},
		{name: "graphical entry shares the container",
			json: `{"fsl": {"categories": ["a", "b"], "apps": {
				"fsl 6.0.3": {"version": "20200905", "exec": ""},
				"fsleyesGUI-fsl 6.0.3": {"version": "20200905", "exec": "fsleyes"}}}}`,
			want: []string{"fsl/6.0.3 fsl_6.0.3_20200905 [a b] graphical"}},
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
		{name: "key without a release", json: `{"fsl": {"apps": {"fsl": {"version": "1"}}}}`,
			wantErr: `catalogue entry "fsl" has no release`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "apps.json")
			if tt.json != "" {
				if err := os.WriteFile(path, []byte(tt.json), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			containers, err := loadCatalogue(path)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("error = %v, want it to contain %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			var got []string
			for _, c := range containers {
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
