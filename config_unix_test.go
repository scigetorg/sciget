//go:build unix

package main

import (
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

func TestWriteSystemFile(t *testing.T) {
	tests := []struct {
		name        string
		symlink     bool // path is a symlink to another file
		readOnly    bool // the folder exists and cannot be written
		wantErrHas  string
		wantContent string
	}{
		{name: "new folder and file are readable by everyone", wantContent: "{\n  \"a\": 1\n}\n"},
		{name: "a symlink at the path is replaced, not followed", symlink: true, wantContent: "{\n  \"a\": 1\n}\n"},
		{name: "no permission", readOnly: true, wantErrHas: "(--system needs root)"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.readOnly && os.Geteuid() == 0 {
				t.Skip("root ignores file modes")
			}
			// Root's umask is often 077; the files must still be readable.
			defer syscall.Umask(syscall.Umask(0o077))
			dir := filepath.Join(t.TempDir(), "sciget")
			path := filepath.Join(dir, "config.json")
			target := filepath.Join(t.TempDir(), "target")
			if tt.symlink || tt.readOnly {
				if err := os.Mkdir(dir, 0o755); err != nil {
					t.Fatal(err)
				}
			}
			if tt.symlink {
				if err := os.WriteFile(target, []byte("keep"), 0o644); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(target, path); err != nil {
					t.Fatal(err)
				}
			}
			if tt.readOnly {
				if err := os.Chmod(dir, 0o555); err != nil {
					t.Fatal(err)
				}
			}

			err := writeSystemFile(path, map[string]int{"a": 1})
			if tt.wantErrHas != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErrHas) {
					t.Fatalf("error = %v, want it to contain %q", err, tt.wantErrHas)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if string(data) != tt.wantContent {
				t.Errorf("content = %q, want %q", data, tt.wantContent)
			}
			modes := map[string]os.FileMode{path: 0o644}
			if !tt.symlink { // the folder is writeSystemFile's own
				modes[dir] = 0o755
			}
			for p, want := range modes {
				info, err := os.Lstat(p)
				if err != nil {
					t.Fatal(err)
				}
				if info.Mode().Perm() != want || info.Mode()&os.ModeSymlink != 0 {
					t.Errorf("%s mode = %v, want %v", p, info.Mode(), want)
				}
			}
			if tt.symlink {
				if data, _ := os.ReadFile(target); string(data) != "keep" {
					t.Errorf("symlink target = %q, want it untouched", data)
				}
			}
		})
	}
}
