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
		{name: "null value", json: `{"cache_dir": null}`, wantErr: "invalid cache_dir: must not be null"},
		{name: "number value", json: `{"cache_dir": 5}`, wantErr: "invalid cache_dir: must be a JSON string"},
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
	verbArgs := `["--cleanenv","{env}","{xauthority}","{tmp}","{pwd}"]`
	const updateURL = "https://github.com/scigetorg/sciget/releases/latest"
	// Every key after cache_dir, at its default.
	rest := [][2]string{
		{"container_dir", filepath.Join("/data", "sciget", "containers")},
		{"container_layout", layout},
		{"apptainer_path", "apptainer"},
		{"apptainer_global_args", `["--silent"]`},
		{"apptainer_exec_args", verbArgs},
		{"apptainer_shell_args", verbArgs},
		{"apptainer_extra_args", "[]"},
		{"apptainer_pass_env", `["DISPLAY"]`},
		{"xauthority_var", "XAUTHORITY"},
		{"tmp_vars", `["TMP","TMPDIR","TEMP","TEMPDIR"]`},
		{"tmp_target", "/tmp"},
		{"use_catalogue_args", "true"},
		{"max_catalogue_size", "67108864"},
		{"display_start_command", "[]"},
		{"display_start_seconds", "30"},
		{"launch_check_seconds", "5"},
		{"network_timeout_seconds", "30"},
		{"update_url", updateURL},
		{"download_urls", `["https://object-store.rc.nectar.org.au/v1/AUTH_dead991e1fa847e3afcca2d3a7041f5d/neurodesk/{file}","https://neurocontainers.s3.us-east-2.amazonaws.com/{file}"]`},
		{"warn_missing_checksum", "false"},
		{"auto_pull", "false"},
		{"parallel_pulls", "3"},
	}
	var piped, aligned strings.Builder
	width := len(cacheDir)
	for _, row := range rest {
		width = max(width, len(row[1]))
	}
	width += 2 // the longest value and the gap
	fmt.Fprintf(&aligned, "%-25s%-*s%s\n", "KEY", width, "VALUE", "SOURCE")
	fmt.Fprintf(&aligned, "%-25s%-*s%s\n", "cache_dir", width, cacheDir, "default")
	for _, row := range rest {
		fmt.Fprintf(&piped, "%s\t%s\tdefault\n", row[0], row[1])
		fmt.Fprintf(&aligned, "%-25s%-*s%s\n", row[0], width, row[1], "default")
	}
	tests := []struct {
		name      string
		overrides map[string]string
		terminal  bool
		want      string
	}{
		{name: "tab-separated when piped", want: "cache_dir\t" + cacheDir + "\tdefault\n" + piped.String()},
		{name: "from the config", overrides: map[string]string{"cache_dir": "/c"}, want: "cache_dir\t/c\tconfig\n" + piped.String()},
		{name: "aligned with a header on a terminal", terminal: true, want: aligned.String()},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := config{overrides: tt.overrides}
			var out bytes.Buffer
			if err := writeConfigList(&out, c, tt.terminal); err != nil {
				t.Fatal(err)
			}
			if got := out.String(); got != tt.want {
				t.Errorf("output = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestDefaultValue(t *testing.T) {
	setCacheHome(t, t.TempDir())
	setConfigHome(t, t.TempDir())
	var defaults map[string]json.RawMessage
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
			value, err := defaultValue(key.name) // decodes it as key.kind
			if err != nil {
				t.Fatal(err)
			}
			if err := key.validate(value); err != nil {
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

func TestValueText(t *testing.T) {
	for _, tt := range []struct {
		name    string
		kind    valueKind
		json    string
		want    string
		wantErr string
	}{
		{name: "text", json: `"/c"`, want: "/c"},
		{name: "text null", json: "null", wantErr: "must not be null"},
		{name: "number", kind: numberKind, json: "1024", want: "1024"},
		{name: "number fraction", kind: numberKind, json: "1.5", wantErr: "must be a whole number"},
		{name: "number string", kind: numberKind, json: `"1"`, wantErr: "must be a whole number"},
		{name: "bool", kind: boolKind, json: "false", want: "false"},
		{name: "bool string", kind: boolKind, json: `"true"`, wantErr: "must be true or false"},
		{name: "list compacted", kind: listKind, json: `[ "--bind", "a&b" ]`, want: `["--bind","a&b"]`},
		{name: "empty list", kind: listKind, json: "[]", want: "[]"},
		{name: "list null", kind: listKind, json: " null ", wantErr: "must not be null"},
		{name: "list of numbers", kind: listKind, json: "[1]", wantErr: "must be a JSON array of strings"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			got, err := valueText(tt.kind, []byte(tt.json))
			if tt.wantErr != "" {
				if err == nil || err.Error() != tt.wantErr {
					t.Fatalf("error = %v, want %q", err, tt.wantErr)
				}
				return
			}
			if err != nil || got != tt.want {
				t.Errorf("valueText() = %q, %v, want %q", got, err, tt.want)
			}
		})
	}
}

func TestLaunchChecks(t *testing.T) {
	for _, tt := range []struct {
		name  string
		check func(string) error
		value string
		bad   bool
	}{
		{name: "arg", check: checkArg, value: "--nv"},
		{name: "empty arg", check: checkArg, bad: true},
		{name: "NUL arg", check: checkArg, value: "a\x00", bad: true},
		{name: "arg with a brace", check: checkArg, value: "{pwd}", bad: true},
		{name: "verb arg placeholder", check: checkVerbArg, value: "{tmp}"},
		{name: "verb arg literal", check: checkVerbArg, value: "--cleanenv"},
		{name: "verb arg unknown placeholder", check: checkVerbArg, value: "{home}", bad: true},
		{name: "verb arg brace inside", check: checkVerbArg, value: "a{pwd}", bad: true},
		{name: "env name", check: checkEnvName, value: "WAYLAND_DISPLAY2"},
		{name: "env name leading digit", check: checkEnvName, value: "2X", bad: true},
		{name: "env name with a dash", check: checkEnvName, value: "A-B", bad: true},
		{name: "empty env name", check: checkEnvName, bad: true},
		{name: "bind target", check: checkBindTarget, value: "/scratch"},
		{name: "relative bind target", check: checkBindTarget, value: "tmp", bad: true},
		{name: "bind target with a colon", check: checkBindTarget, value: "/a:b", bad: true},
		{name: "bind target with a comma", check: checkBindTarget, value: "/a,b", bad: true},
		{name: "size", check: checkCatalogueSize, value: "1"},
		{name: "size at the cap", check: checkCatalogueSize, value: "1099511627776"},
		{name: "size zero", check: checkCatalogueSize, value: "0", bad: true},
		{name: "size over the cap", check: checkCatalogueSize, value: "1099511627777", bad: true},
		{name: "list elements", check: eachElement(checkEnvName), value: `["TMP","TMPDIR"]`},
		{name: "list with a bad element", check: eachElement(checkEnvName), value: `["TMP","1"]`, bad: true},
		{name: "seconds", check: checkSeconds, value: "30"},
		{name: "seconds zero", check: checkSeconds, value: "0", bad: true},
		{name: "seconds over an hour", check: checkSeconds, value: "3601", bad: true},
		{name: "https URL", check: checkHTTPSURL, value: "https://example.org/x"},
		{name: "http URL", check: checkHTTPSURL, value: "http://example.org/x", bad: true},
		{name: "URL without host", check: checkHTTPSURL, value: "https:///x", bad: true},
		{name: "URL with user information", check: checkHTTPSURL, value: "https://me:secret@example.org/x", bad: true},
		{name: "download URL", check: checkDownloadURL, value: "https://example.org/c/{file}"},
		{name: "download URL to this machine", check: checkDownloadURL, value: "http://127.0.0.1:8080/{file}"},
		{name: "download URL without {file}", check: checkDownloadURL, value: "https://example.org/c/", bad: true},
		{name: "download URL with {file} twice", check: checkDownloadURL, value: "https://example.org/{file}/{file}", bad: true},
		{name: "download URL over plain http", check: checkDownloadURL, value: "http://example.org/{file}", bad: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if err := tt.check(tt.value); (err != nil) != tt.bad {
				t.Errorf("error = %v, want error: %v", err, tt.bad)
			}
		})
	}
}
