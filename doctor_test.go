package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestFindProblems(t *testing.T) {
	tests := []struct {
		name   string
		files  []string // created under container_dir
		layout string   // container_layout; the default when empty
		noDir  bool     // container_dir does not exist
		store  string   // container_dir's name; "store" when empty
		want   []string // the files each problem deletes, relative to container_dir
	}{
		{name: "nothing", files: []string{"fsl_1_2/fsl_1_2.simg"}},
		{name: "missing store", noDir: true},
		{name: "a leftover", files: []string{"fsl_1_2/fsl_1_2.simg", "fsl_1_2/fsl_1_2.simg.123.incomplete"},
			want: []string{"fsl_1_2/fsl_1_2.simg.123.incomplete"}},
		{name: "other files are not reported", files: []string{"fsl_1_2/notes.incomplete", "x.simg.1.incomplete"}},
		{name: "a store whose name looks like a pattern", store: "store[1]",
			files: []string{"fsl_1_2/fsl_1_2.simg.5.incomplete"}, want: []string{"fsl_1_2/fsl_1_2.simg.5.incomplete"}},
		{name: "a deeper layout", layout: "{tool}/{version}/{date}.sif",
			files: []string{"fsl/1/2.sif.9.incomplete"}, want: []string{"fsl/1/2.sif.9.incomplete"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			name := tt.store
			if name == "" {
				name = "store"
			}
			store := filepath.Join(t.TempDir(), name)
			if !tt.noDir {
				for _, f := range tt.files {
					path := filepath.Join(store, filepath.FromSlash(f))
					if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
						t.Fatal(err)
					}
					if err := os.WriteFile(path, []byte("x"), 0o644); err != nil {
						t.Fatal(err)
					}
				}
			}
			overrides := map[string]string{"container_dir": store}
			if tt.layout != "" {
				overrides["container_layout"] = tt.layout
			}
			problems, err := findProblems(config{overrides: overrides})
			if err != nil {
				t.Fatal(err)
			}
			var got []string
			for _, p := range problems {
				got = append(got, p.fix...)
			}
			if len(got) != len(tt.want) {
				t.Fatalf("fixes = %q, want %q", got, tt.want)
			}
			for i := range got {
				if want := shellJoin([]string{"rm", "--", filepath.Join(store, filepath.FromSlash(tt.want[i]))}); got[i] != want {
					t.Errorf("fix = %q, want %q", got[i], want)
				}
			}
		})
	}
}
