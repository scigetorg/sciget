package main

import (
	"bytes"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadConfig(t *testing.T) {
	tests := []struct {
		name    string
		json    string // written to config.json; no file when empty
		want    map[string]string
		wantErr string
	}{
		{name: "missing file", want: map[string]string{}},
		{name: "empty object", json: "{}", want: map[string]string{}},
		{name: "overrides", json: `{"cache_dir": "/c", "catalogue_url": "http://m.example/a.json"}`,
			want: map[string]string{"cache_dir": "/c", "catalogue_url": "http://m.example/a.json"}},
		{name: "invalid JSON", json: "{", wantErr: "unexpected end of JSON input"},
		{name: "anything after the object", json: "{}{}", wantErr: "invalid character '{' after top-level value"},
		{name: "top-level null", json: "null", wantErr: "not a JSON object"},
		{name: "array", json: "[]", wantErr: "cannot unmarshal array"},
		{name: "unknown key", json: `{"instal_dir": "/i"}`,
			wantErr: `unknown key "instal_dir"; valid keys: cache_dir, catalogue_url`},
		{name: "keys match exactly", json: `{"CACHE_DIR": "/c"}`, wantErr: `unknown key "CACHE_DIR"`},
		{name: "kebab-case", json: `{"cache-dir": "/c"}`, wantErr: `unknown key "cache-dir"`},
		{name: "empty value", json: `{"cache_dir": ""}`, wantErr: "invalid cache_dir: must be an absolute path"},
		{name: "null value", json: `{"cache_dir": null}`, wantErr: "invalid cache_dir: must be an absolute path"},
		{name: "number value", json: `{"cache_dir": 5}`, wantErr: "cannot unmarshal number"},
		{name: "relative path", json: `{"cache_dir": "cache"}`, wantErr: "invalid cache_dir: must be an absolute path"},
		{name: "URL without a scheme", json: `{"catalogue_url": "m.example/a.json"}`,
			wantErr: "invalid catalogue_url: must start with https:// or http://"},
		{name: "URL without a host", json: `{"catalogue_url": "https:///a.json"}`,
			wantErr: "invalid catalogue_url: must name a host"},
		{name: "URL with a port but no host", json: `{"catalogue_url": "http://:80/a.json"}`,
			wantErr: "invalid catalogue_url: must name a host"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.json")
			if tt.json != "" {
				if err := os.WriteFile(path, []byte(tt.json), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			c, err := loadConfig(path)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("error = %v, want it to contain %q", err, tt.wantErr)
				}
				if !strings.HasPrefix(err.Error(), "reading config "+path+": ") {
					t.Errorf("error = %q, want it to name the file", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if !maps.Equal(c.overrides, tt.want) {
				t.Errorf("overrides = %v, want %v", c.overrides, tt.want)
			}
		})
	}
}

func TestConfigGet(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", "/xdg-cache")
	c := config{overrides: map[string]string{"catalogue_url": "https://m.example/a.json"}}
	tests := []struct {
		name       string
		key        string
		wantValue  string
		wantSource string
		wantErr    string
	}{
		{name: "default", key: "cache_dir", wantValue: "/xdg-cache/sciget", wantSource: "default"},
		{name: "override", key: "catalogue_url", wantValue: "https://m.example/a.json", wantSource: "config"},
		{name: "unknown key", key: "cache-dir", wantErr: `unknown key "cache-dir"`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			value, source, err := c.get(tt.key)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("error = %v, want it to contain %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if value != tt.wantValue || source != tt.wantSource {
				t.Errorf("get = %q, %q, want %q, %q", value, source, tt.wantValue, tt.wantSource)
			}
		})
	}
}

func TestConfigSet(t *testing.T) {
	cwd := t.TempDir()
	t.Chdir(cwd)
	tests := []struct {
		name         string
		overrides    map[string]string
		key, value   string
		want         string // the file afterwards
		wantUsageErr string
	}{
		{name: "new file", key: "cache_dir", value: "/c", want: "{\n  \"cache_dir\": \"/c\"\n}\n"},
		{name: "keeps other keys", overrides: map[string]string{"cache_dir": "/c"},
			key: "catalogue_url", value: "https://m.example/a.json",
			want: "{\n  \"cache_dir\": \"/c\",\n  \"catalogue_url\": \"https://m.example/a.json\"\n}\n"},
		{name: "relative path made absolute", key: "cache_dir", value: "cache",
			want: fmt.Sprintf("{\n  \"cache_dir\": %q\n}\n", filepath.Join(cwd, "cache"))},
		{name: "empty value", key: "cache_dir", wantUsageErr: "invalid cache_dir: must be an absolute path"},
		{name: "invalid URL", key: "catalogue_url", value: "ftp://m.example/a.json",
			wantUsageErr: "invalid catalogue_url: must start with https:// or http://"},
		{name: "unknown key", key: "CACHE_DIR", value: "/c", wantUsageErr: `unknown key "CACHE_DIR"`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "sciget", "config.json")
			c := config{path: path, overrides: map[string]string{}}
			maps.Copy(c.overrides, tt.overrides)
			err := c.set(tt.key, tt.value)
			if tt.wantUsageErr != "" {
				var usageErr usageError
				if !errors.As(err, &usageErr) || !strings.Contains(err.Error(), tt.wantUsageErr) {
					t.Fatalf("error = %v, want a usage error containing %q", err, tt.wantUsageErr)
				}
				if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
					t.Errorf("config file written after a failed set")
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
			if string(data) != tt.want {
				t.Errorf("file = %q, want %q", data, tt.want)
			}
		})
	}
}

func TestConfigUnset(t *testing.T) {
	const content = "{ }" // differs from what writeConfig writes, so a rewrite shows
	tests := []struct {
		name         string
		overrides    map[string]string
		key          string
		want         string
		wantUsageErr string
	}{
		{name: "keeps other keys", overrides: map[string]string{"cache_dir": "/c", "catalogue_url": "https://m.example/a.json"},
			key: "cache_dir", want: "{\n  \"catalogue_url\": \"https://m.example/a.json\"\n}\n"},
		{name: "last key", overrides: map[string]string{"cache_dir": "/c"}, key: "cache_dir", want: "{}\n"},
		{name: "not set, so nothing is written", key: "cache_dir", want: content},
		{name: "unknown key", key: "cache-dir", want: content, wantUsageErr: `unknown key "cache-dir"`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.json")
			if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
				t.Fatal(err)
			}
			c := config{path: path, overrides: map[string]string{}}
			maps.Copy(c.overrides, tt.overrides)
			err := c.unset(tt.key)
			if tt.wantUsageErr != "" {
				var usageErr usageError
				if !errors.As(err, &usageErr) || !strings.Contains(err.Error(), tt.wantUsageErr) {
					t.Fatalf("error = %v, want a usage error containing %q", err, tt.wantUsageErr)
				}
			} else if err != nil {
				t.Fatal(err)
			}
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if string(data) != tt.want {
				t.Errorf("file = %q, want %q", data, tt.want)
			}
		})
	}
}

func TestWriteConfig(t *testing.T) {
	overrides := map[string]string{"cache_dir": "/c"}
	const written = "{\n  \"cache_dir\": \"/c\"\n}\n"
	tests := []struct {
		name     string
		existing string      // written to the file first; no file when empty
		mode     os.FileMode // of the existing file
		symlink  bool        // the path is a symlink to another file
		want     string      // the file's content afterwards
		wantMode os.FileMode // checked when set
		wantErr  string
	}{
		{name: "new file in a new directory is private", want: written, wantMode: 0o600},
		{name: "read-only file is left alone", existing: "{}", mode: 0o400,
			want: "{}", wantErr: "writing config: "},
		{name: "symlink is followed", symlink: true, want: written},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.mode == 0o400 && os.Geteuid() == 0 {
				t.Skip("root can write a read-only file")
			}
			dir := t.TempDir()
			path := filepath.Join(dir, "sciget", "config.json")
			target := path
			if tt.existing != "" || tt.symlink {
				if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
					t.Fatal(err)
				}
			}
			if tt.symlink {
				target = filepath.Join(dir, "real.json")
				if err := os.Symlink(target, path); err != nil {
					t.Fatal(err)
				}
			}
			if tt.existing != "" {
				if err := os.WriteFile(path, []byte(tt.existing), tt.mode); err != nil {
					t.Fatal(err)
				}
			}

			err := writeConfig(path, overrides)
			if tt.wantErr != "" {
				if err == nil || !strings.HasPrefix(err.Error(), tt.wantErr) {
					t.Errorf("error = %v, want it to start with %q", err, tt.wantErr)
				}
			} else if err != nil {
				t.Fatal(err)
			}
			if data, _ := os.ReadFile(target); string(data) != tt.want {
				t.Errorf("file = %q, want %q", data, tt.want)
			}
			if tt.wantMode != 0 {
				info, err := os.Stat(target)
				if err != nil {
					t.Fatal(err)
				}
				if mode := info.Mode().Perm(); mode != tt.wantMode {
					t.Errorf("mode = %o, want %o", mode, tt.wantMode)
				}
			}
			if tt.symlink {
				if info, err := os.Lstat(path); err != nil || info.Mode()&os.ModeSymlink == 0 {
					t.Errorf("symlink replaced: %v", err)
				}
			}
		})
	}
}

func TestWriteConfigList(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", "/xdg-cache")
	c := config{overrides: map[string]string{"catalogue_url": "https://m.example/a.json"}}
	tests := []struct {
		name     string
		terminal bool
		want     string
	}{
		{name: "tab-separated when piped",
			want: "cache_dir\t/xdg-cache/sciget\tdefault\ncatalogue_url\thttps://m.example/a.json\tconfig\n"},
		{name: "aligned with a header on a terminal", terminal: true,
			want: fmt.Sprintf("%-15s%-26s%s\n", "KEY", "VALUE", "SOURCE") +
				fmt.Sprintf("%-15s%-26s%s\n", "cache_dir", "/xdg-cache/sciget", "default") +
				fmt.Sprintf("%-15s%-26s%s\n", "catalogue_url", "https://m.example/a.json", "config")},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var out bytes.Buffer
			if err := writeConfigList(&out, c, tt.terminal); err != nil {
				t.Fatal(err)
			}
			if got := out.String(); got != tt.want {
				t.Errorf("output =\n%s\nwant\n%s", got, tt.want)
			}
		})
	}
}
