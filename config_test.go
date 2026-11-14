package main

import (
	"bytes"
	"encoding/json"
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
		dir     bool   // create config.json as a directory to force a read fail
		want    map[string]string
		wantErr string
	}{
		{name: "missing file", want: map[string]string{}},
		{name: "empty object", json: "{}", want: map[string]string{}},
		{name: "overrides", json: `{"cache_dir": "/c"}`, want: map[string]string{"cache_dir": "/c"}},
		{name: "path is a directory", dir: true, wantErr: "reading config: "},
		{name: "invalid JSON", json: "{", wantErr: "unexpected end of JSON input"},
		{name: "anything after the object", json: "{}{}", wantErr: "invalid character '{' after top-level value"},
		{name: "top-level null", json: "null", wantErr: "not a JSON object"},
		{name: "array", json: "[]", wantErr: "cannot unmarshal array"},
		{name: "unknown key", json: `{"instal_dir": "/i"}`,
			wantErr: `unknown key "instal_dir"; valid keys: cache_dir`},
		{name: "removed key", json: `{"catalogue_url": "https://m.example/a.json"}`,
			wantErr: `unknown key "catalogue_url"`},
		{name: "keys match exactly", json: `{"CACHE_DIR": "/c"}`, wantErr: `unknown key "CACHE_DIR"`},
		{name: "kebab-case", json: `{"cache-dir": "/c"}`, wantErr: `unknown key "cache-dir"`},
		{name: "empty value", json: `{"cache_dir": ""}`, wantErr: "invalid cache_dir: must be an absolute path"},
		{name: "null value", json: `{"cache_dir": null}`, wantErr: "invalid cache_dir: must be an absolute path"},
		{name: "number value", json: `{"cache_dir": 5}`, wantErr: "cannot unmarshal number"},
		{name: "container path", json: `{"container_dir": "/containers"}`, want: map[string]string{"container_dir": "/containers"}},
		{name: "relative container path", json: `{"container_dir": "containers"}`, wantErr: "invalid container_dir"},
		{name: "layout", json: `{"container_layout": "{tool}/{version}/{date}"}`, want: map[string]string{"container_layout": "{tool}/{version}/{date}"}},
		{name: "bad layout", json: `{"container_layout": "{name}"}`, wantErr: "invalid container_layout"},
		{name: "binary name", json: `{"apptainer_path": "singularity"}`, want: map[string]string{"apptainer_path": "singularity"}},
		{name: "relative binary path", json: `{"apptainer_path": "bin/apptainer"}`, wantErr: "invalid apptainer_path"},
		{name: "relative path", json: `{"cache_dir": "cache"}`, wantErr: "invalid cache_dir: must be an absolute path"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.json")
			if tt.json != "" {
				if err := os.WriteFile(path, []byte(tt.json), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			if tt.dir {
				if err := os.Mkdir(path, 0o755); err != nil {
					t.Fatal(err)
				}
			}
			c, err := loadConfig(path)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("error = %v, want it to contain %q", err, tt.wantErr)
				}
				// os errors are *fs.PathError, which already include the path.
				prefix := "reading config " + path + ": "
				if tt.dir {
					prefix = "reading config: "
				}
				if !strings.HasPrefix(err.Error(), prefix) || !strings.Contains(err.Error(), path) {
					t.Errorf("error = %q, want it to start with %q and name the file", err, prefix)
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

func TestConfigPath(t *testing.T) {
	setConfigHome(t, t.TempDir())
	dir, err := os.UserConfigDir()
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name string
		env  string // SCIGET_CONFIG
		want string
	}{
		{name: "default", want: filepath.Join(dir, "sciget", "config.json")},
		{name: "SCIGET_CONFIG", env: "/etc/sciget.json", want: "/etc/sciget.json"},
		{name: "relative SCIGET_CONFIG used as given", env: "sciget.json", want: "sciget.json"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("SCIGET_CONFIG", tt.env)
			got, err := configPath()
			if err != nil {
				t.Fatal(err)
			}
			if got != tt.want {
				t.Errorf("configPath = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestConfigGet(t *testing.T) {
	setCacheHome(t, "/home")
	cache, err := os.UserCacheDir()
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name       string
		overrides  map[string]string
		key        string
		wantValue  string
		wantSource string
		wantErr    string
	}{
		{name: "default", key: "cache_dir", wantValue: filepath.Join(cache, "sciget"), wantSource: "default"},
		{name: "override", overrides: map[string]string{"cache_dir": "/c"}, key: "cache_dir",
			wantValue: "/c", wantSource: "config"},
		{name: "unknown key", key: "cache-dir", wantErr: `unknown key "cache-dir"`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := config{overrides: tt.overrides}
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
		{name: "replaces the value", overrides: map[string]string{"cache_dir": "/c"},
			key: "cache_dir", value: "/d", want: "{\n  \"cache_dir\": \"/d\"\n}\n"},
		{name: "relative path made absolute", key: "cache_dir", value: "cache",
			want: fmt.Sprintf("{\n  \"cache_dir\": %q\n}\n", filepath.Join(cwd, "cache"))},
		{name: "relative container directory", key: "container_dir", value: "containers",
			want: fmt.Sprintf("{\n  \"container_dir\": %q\n}\n", filepath.Join(cwd, "containers"))},
		{name: "bad layout", key: "container_layout", value: "{tool}/{version}", wantUsageErr: "invalid container_layout"},
		{name: "relative binary path", key: "apptainer_path", value: "bin/apptainer", wantUsageErr: "invalid apptainer_path"},
		{name: "empty value", key: "cache_dir", wantUsageErr: "invalid cache_dir: must be an absolute path"},
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
	setCacheHome(t, "/home")
	t.Setenv("XDG_DATA_HOME", "/data")
	cache, err := os.UserCacheDir()
	if err != nil {
		t.Fatal(err)
	}
	layout, err := defaultValue("container_layout")
	if err != nil {
		t.Fatal(err)
	}
	cacheDir := filepath.Join(cache, "sciget")
	containerDir := filepath.Join("/data", "sciget", "containers")
	tests := []struct {
		name      string
		overrides map[string]string
		terminal  bool
		want      string
	}{
		{name: "tab-separated when piped", want: "cache_dir\t" + cacheDir + "\tdefault\n"},
		{name: "from the config", overrides: map[string]string{"cache_dir": "/c"}, want: "cache_dir\t/c\tconfig\n"},
		{name: "aligned with a header on a terminal", terminal: true,
			want: fmt.Sprintf("%-18s%-*s%s\n", "KEY", len(layout)+2, "VALUE", "SOURCE") +
				fmt.Sprintf("%-18s%-*s%s\n", "cache_dir", len(layout)+2, cacheDir, "default") +
				fmt.Sprintf("%-18s%-*s%s\n", "container_dir", len(layout)+2, containerDir, "default") +
				fmt.Sprintf("%-18s%-*s%s\n", "container_layout", len(layout)+2, layout, "default") +
				fmt.Sprintf("%-18s%-*s%s\n", "apptainer_path", len(layout)+2, "apptainer", "default")},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			want := tt.want
			if !tt.terminal {
				want += "container_dir\t" + containerDir + "\tdefault\ncontainer_layout\t" + layout + "\tdefault\napptainer_path\tapptainer\tdefault\n"
			}
			c := config{overrides: tt.overrides}
			var out bytes.Buffer
			if err := writeConfigList(&out, c, tt.terminal); err != nil {
				t.Fatal(err)
			}
			if got := out.String(); got != want {
				t.Errorf("output = %q, want %q", got, want)
			}
		})
	}
}

func TestDefaultValue(t *testing.T) {
	setCacheHome(t, t.TempDir())
	setConfigHome(t, t.TempDir())
	var defaults map[string]string
	if err := json.Unmarshal(defaultsJSON, &defaults); err != nil {
		t.Fatal(err)
	}
	if len(defaults) != len(configKeys) {
		t.Fatalf("defaults has %d keys, configKeys has %d", len(defaults), len(configKeys))
	}
	for name := range defaults {
		if _, err := findConfigKey(name); err != nil {
			t.Fatal(err)
		}
	}
	for _, key := range configKeys {
		t.Run(key.name, func(t *testing.T) {
			if _, ok := defaults[key.name]; !ok {
				t.Fatal("missing default")
			}
			value, err := defaultValue(key.name)
			if err != nil {
				t.Fatal(err)
			}
			if err := key.check(value); err != nil {
				t.Fatal(err)
			}
		})
	}
	for _, tt := range []struct {
		name, key, cacheHome, dataHome, want string
	}{
		{name: "XDG data home", key: "container_dir", dataHome: "/data",
			want: filepath.Join("/data", "sciget", "containers")},
		{name: "home fallback", key: "container_dir",
			want: filepath.Join(os.Getenv("HOME"), ".local", "share", "sciget", "containers")},
		{name: "braces in a base directory", key: "cache_dir", cacheHome: "/scratch/{project}",
			want: filepath.Join("/scratch/{project}", "sciget")},
		{name: "placeholder in a base directory", key: "container_dir", dataHome: "/d/{user_cache_dir}",
			want: filepath.Join("/d/{user_cache_dir}", "sciget", "containers")},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if tt.cacheHome != "" {
				t.Setenv("XDG_CACHE_HOME", tt.cacheHome)
			}
			t.Setenv("XDG_DATA_HOME", tt.dataHome)
			value, err := defaultValue(tt.key)
			if err != nil || value != tt.want {
				t.Errorf("default = %q, %v, want %q", value, err, tt.want)
			}
		})
	}
}

func TestCheckApptainerPath(t *testing.T) {
	for _, tt := range []struct {
		name, value string
		bad         bool
	}{
		{name: "name", value: "apptainer"},
		{name: "alternative name", value: "singularity"},
		{name: "absolute", value: filepath.Join(t.TempDir(), "apptainer")},
		{name: "relative", value: "bin/apptainer", bad: true},
		{name: "dot relative", value: "./apptainer", bad: true},
		{name: "empty", bad: true},
		{name: "NUL", value: "apptainer\x00", bad: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if err := checkApptainerPath(tt.value); (err != nil) != tt.bad {
				t.Errorf("error = %v, want error: %v", err, tt.bad)
			}
		})
	}
}
