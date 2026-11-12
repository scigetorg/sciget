package main

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRun(t *testing.T) {
	const helpText = "Usage:\n  sciget [flags]\n  sciget [command]\n"
	const usageHint = "\nRun 'sciget help' for usage.\n"
	const catalogue = `{"fsl": {"categories": ["image registration"], "apps": {
		"fsl 6.0.3": {"version": "20200905", "exec": ""},
		"fsleyesGUI-fsl 6.0.3": {"version": "20200905", "exec": "fsleyes"},
		"fsl 6.0.3 arm64": {"version": "20260101", "image": "fsl_6.0.3_arm64", "show_in_applist": false}}}}`

	tests := []struct {
		name          string
		args          []string
		catalogue     string            // written to the cache; no file when empty
		config        string            // written to SCIGET_CONFIG; no file when empty
		serve         string            // served by a test server that the config names as catalogue_url
		readOnly      bool              // write config with mode 0o400
		env           map[string]string // set after the defaults
		wantConfig    string            // the config file afterwards, when set
		wantCode      int
		wantStdout    string // exact, unless wantStdoutHas is set
		wantStdoutHas string // checks only that stdout contains this
		wantStderr    string // exact, unless wantStderrHas is set
		wantStderrHas string
	}{
		{name: "no arguments", wantCode: 2,
			wantStderr: "sciget: no command given" + usageHint},
		{name: "help", args: []string{"help"}, wantStdoutHas: helpText},
		{name: "help for a command", args: []string{"help", "version"},
			wantStdoutHas: "Usage:\n  sciget version [flags]\n"},
		{name: "unknown help topic", args: []string{"help", "missing"},
			wantStderrHas: "Unknown help topic [`missing`]\n" + helpText},
		{name: "-h", args: []string{"-h"}, wantStdoutHas: helpText},
		{name: "--help", args: []string{"--help"}, wantStdoutHas: helpText},
		{name: "version", args: []string{"version"}, wantStdout: "sciget " + version + "\n"},
		{name: "--version", args: []string{"--version"}, wantStdout: "sciget " + version + "\n"},
		{name: "-v", args: []string{"-v"}, wantStdout: "sciget " + version + "\n"},
		{name: "--version after a command", args: []string{"missing", "--version"},
			wantStdout: "sciget " + version + "\n"},
		{name: "--version is for the root only", args: []string{"version", "--version"}, wantCode: 2,
			wantStderr: "sciget: unknown flag: --version" + usageHint},
		{name: "single-dash long flag", args: []string{"-version"}, wantCode: 2,
			wantStderr: "sciget: unknown shorthand flag: 'e' in -ersion" + usageHint},
		{name: "unknown command", args: []string{"missing"}, wantCode: 2,
			wantStderr: "sciget: unknown command \"missing\" for \"sciget\"" + usageHint},
		{name: "unknown flag", args: []string{"--bogus"}, wantCode: 2,
			wantStderr: "sciget: unknown flag: --bogus" + usageHint},
		{name: "version takes no arguments", args: []string{"version", "x"}, wantCode: 2,
			wantStderr: "sciget: unknown command \"x\" for \"sciget version\"" + usageHint},
		{name: "completion", args: []string{"completion", "bash"},
			wantStdoutHas: "bash completion V2 for sciget"},
		{name: "completion takes no arguments", args: []string{"completion", "bash", "x"}, wantCode: 2,
			wantStderr: "sciget: unknown command \"x\" for \"sciget completion bash\"" + usageHint},
		{name: "find without a catalogue", args: []string{"find"}, wantCode: 1,
			wantStderrHas: "/sciget/apps.json; run 'sciget refresh'\n"},
		{name: "find", args: []string{"find", "fsl"}, catalogue: catalogue,
			wantStdout: "fsl/6.0.3\timage registration\tyes\n"},
		{name: "find --all", args: []string{"find", "--all", "fsl"}, catalogue: catalogue,
			wantStdout: "fsl/6.0.3\timage registration\tyes\nfsl/6.0.3_arm64\timage registration\t\n"},
		{name: "find completes versions", args: []string{"__complete", "find", "fsl/"}, catalogue: catalogue,
			wantStdout: "fsl/6.0.3\tbuilt 20200905\n:4\n", wantStderrHas: "Completion ended with directive"},
		{name: "find --all completes hidden versions", args: []string{"__complete", "find", "--all", "fsl/"},
			catalogue: catalogue, wantStdout: "fsl/6.0.3\tbuilt 20200905\nfsl/6.0.3_arm64\tbuilt 20260101\n:4\n",
			wantStderrHas: "Completion ended with directive"},
		{name: "find completes tool names", args: []string{"__complete", "find", "fs"}, catalogue: catalogue,
			wantStdout: "fsl/\n:6\n", wantStderrHas: "Completion ended with directive"},
		{name: "find completion without a catalogue", args: []string{"__complete", "find", "fs"},
			wantStdout: ":1\n", wantStderrHas: "Completion ended with directive: ShellCompDirectiveError"},
		{name: "find reads cache_dir from the config", args: []string{"find"},
			config: `{"cache_dir": "/nowhere"}`, wantCode: 1,
			wantStderr: "sciget: no catalogue at /nowhere/apps.json; run 'sciget refresh'\n"},
		{name: "find with a broken config", args: []string{"find"}, catalogue: catalogue,
			config: `{"cache-dir": "/c"}`, wantCode: 1, wantStderrHas: "sciget: reading config "},
		{name: "find completion with a broken config", args: []string{"__complete", "find", "fs"},
			catalogue: catalogue, config: "{", wantStdout: ":1\n",
			wantStderrHas: "Completion ended with directive: ShellCompDirectiveError"},
		{name: "refresh", args: []string{"refresh"}, serve: catalogue, wantStdout: "Updated catalogue.\n"},
		{name: "refresh when up to date", args: []string{"refresh"}, serve: catalogue, catalogue: catalogue,
			wantStdout: "Already up-to-date.\n"},
		{name: "refresh takes no arguments", args: []string{"refresh", "x"}, wantCode: 2,
			wantStderr: "sciget: unknown command \"x\" for \"sciget refresh\"" + usageHint},
		{name: "refresh with a broken config", args: []string{"refresh"}, config: `{"bogus": "x"}`, wantCode: 1,
			wantStderrHas: `unknown key "bogus"`},
		{name: "config lists", args: []string{"config"}, config: `{"cache_dir": "/c"}`,
			wantStdout: "cache_dir\t/c\tconfig\ncatalogue_url\t" + defaultCatalogueURL + "\tdefault\n"},
		{name: "config list", args: []string{"config", "list"}, config: `{"cache_dir": "/c"}`,
			wantStdout: "cache_dir\t/c\tconfig\ncatalogue_url\t" + defaultCatalogueURL + "\tdefault\n"},
		{name: "config with an unknown subcommand", args: []string{"config", "bogus"}, wantCode: 2,
			wantStderr: "sciget: unknown command \"bogus\" for \"sciget config\"" + usageHint},
		{name: "config get a default", args: []string{"config", "get", "catalogue_url"},
			wantStdout: defaultCatalogueURL + "\n"},
		{name: "config get an override", args: []string{"config", "get", "cache_dir"},
			config: `{"cache_dir": "/c"}`, wantStdout: "/c\n"},
		// Each platform words this differently, but every message ends in
		// "defined", so a usage hint after it would fail the row.
		{name: "config get with a broken environment", args: []string{"config", "get", "cache_dir"},
			env: map[string]string{"XDG_CACHE_HOME": "", "HOME": "", "LocalAppData": ""}, wantCode: 1,
			wantStderrHas: "defined\n"},
		{name: "config get an unknown key", args: []string{"config", "get", "cache-dir"}, wantCode: 2,
			wantStderr: "sciget: unknown key \"cache-dir\"; valid keys: cache_dir, catalogue_url" + usageHint},
		{name: "config get without a key", args: []string{"config", "get"}, wantCode: 2,
			wantStderr: "sciget: accepts 1 arg(s), received 0" + usageHint},
		{name: "config set", args: []string{"config", "set", "cache_dir", "/c"},
			wantConfig: "{\n  \"cache_dir\": \"/c\"\n}\n"},
		{name: "config set keeps other keys", args: []string{"config", "set", "catalogue_url", "https://m.example/a.json"},
			config:     `{"cache_dir": "/c"}`,
			wantConfig: "{\n  \"cache_dir\": \"/c\",\n  \"catalogue_url\": \"https://m.example/a.json\"\n}\n"},
		{name: "config set on a read-only file", args: []string{"config", "set", "cache_dir", "/d"},
			config: `{"cache_dir": "/c"}`, readOnly: true, wantCode: 1,
			wantStderrHas: "sciget: writing config: ", wantConfig: `{"cache_dir": "/c"}`},
		{name: "config set an invalid value", args: []string{"config", "set", "catalogue_url", "example.org"},
			wantCode: 2, wantStderr: "sciget: invalid catalogue_url: must start with https:// or http://" + usageHint},
		{name: "config set leaves a file that is not a config", args: []string{"config", "set", "cache_dir", "/c"},
			config: "export X=1\n", wantCode: 1, wantStderrHas: "sciget: reading config ",
			wantConfig: "export X=1\n"},
		{name: "config set leaves a config with an unknown key", args: []string{"config", "set", "cache_dir", "/c"},
			config: `{"CACHE_DIR": "/x"}`, wantCode: 1,
			wantStderrHas: `unknown key "CACHE_DIR"`, wantConfig: `{"CACHE_DIR": "/x"}`},
		{name: "config unset of a key that is not set writes nothing", args: []string{"config", "unset", "cache_dir"},
			config:     `{"catalogue_url": "https://m.example/a.json"}`,
			wantConfig: `{"catalogue_url": "https://m.example/a.json"}`},
		{name: "config set an empty value", args: []string{"config", "set", "cache_dir", ""}, wantCode: 2,
			wantStderr: "sciget: invalid cache_dir: must be an absolute path" + usageHint},
		{name: "config list takes no arguments", args: []string{"config", "list", "x"}, wantCode: 2,
			wantStderr: "sciget: unknown command \"x\" for \"sciget config list\"" + usageHint},
		{name: "config unset", args: []string{"config", "unset", "cache_dir"},
			config:     `{"cache_dir": "/c", "catalogue_url": "https://m.example/a.json"}`,
			wantConfig: "{\n  \"catalogue_url\": \"https://m.example/a.json\"\n}\n"},
		{name: "config completes keys", args: []string{"__complete", "config", "get", "c"},
			wantStdout: "cache_dir\ncatalogue_url\n:4\n", wantStderrHas: "Completion ended with directive"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			setCacheHome(t, t.TempDir())
			cache, err := os.UserCacheDir()
			if err != nil {
				t.Fatal(err)
			}
			configFile := filepath.Join(t.TempDir(), "config.json")
			t.Setenv("SCIGET_CONFIG", configFile)
			for k, v := range tt.env {
				t.Setenv(k, v)
			}
			if tt.serve != "" {
				srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					io.WriteString(w, tt.serve)
				}))
				defer srv.Close()
				tt.config = fmt.Sprintf(`{"catalogue_url": %q}`, srv.URL)
			}
			if tt.readOnly && os.Geteuid() == 0 {
				t.Skip("root can write a read-only file")
			}
			if tt.config != "" {
				mode := os.FileMode(0o600)
				if tt.readOnly {
					mode = 0o400
				}
				if err := os.WriteFile(configFile, []byte(tt.config), mode); err != nil {
					t.Fatal(err)
				}
			}
			if tt.catalogue != "" {
				path := filepath.Join(cache, "sciget", "apps.json")
				if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, []byte(tt.catalogue), 0o644); err != nil {
					t.Fatal(err)
				}
			}

			var stdout, stderr bytes.Buffer
			code := run(tt.args, &stdout, &stderr)
			if code != tt.wantCode {
				t.Errorf("exit code = %d, want %d", code, tt.wantCode)
			}
			got := stdout.String()
			if tt.wantStdoutHas != "" {
				if !strings.Contains(got, tt.wantStdoutHas) {
					t.Errorf("stdout = %q, want it to contain %q", got, tt.wantStdoutHas)
				}
			} else if got != tt.wantStdout {
				t.Errorf("stdout = %q, want %q", got, tt.wantStdout)
			}
			got = stderr.String()
			if tt.wantStderrHas != "" {
				if !strings.Contains(got, tt.wantStderrHas) {
					t.Errorf("stderr = %q, want it to contain %q", got, tt.wantStderrHas)
				}
			} else if got != tt.wantStderr {
				t.Errorf("stderr = %q, want %q", got, tt.wantStderr)
			}
			if tt.wantConfig != "" {
				data, err := os.ReadFile(configFile)
				if err != nil {
					t.Fatal(err)
				}
				if string(data) != tt.wantConfig {
					t.Errorf("config file = %q, want %q", data, tt.wantConfig)
				}
			}
		})
	}
}

// setConfigHome points the user config directory at dir on every platform
// os.UserConfigDir supports.
func setConfigHome(t *testing.T, dir string) {
	t.Helper()
	for _, name := range []string{"XDG_CONFIG_HOME", "HOME", "AppData"} {
		t.Setenv(name, dir)
	}
}

// setCacheHome points the user cache directory at dir on every platform
// os.UserCacheDir supports, so no test reads or writes a real cache.
func setCacheHome(t *testing.T, dir string) {
	t.Helper()
	for _, name := range []string{"XDG_CACHE_HOME", "HOME", "LocalAppData"} {
		t.Setenv(name, dir)
	}
}

func TestExitCode(t *testing.T) {
	tests := []struct {
		name       string
		err        error
		wantCode   int
		wantStderr string
	}{
		{name: "no error"},
		{name: "wrapped usage error", err: fmt.Errorf("x: %w", usageError{"y"}), wantCode: 2,
			wantStderr: "sciget: x: y\nRun 'sciget help' for usage.\n"},
		{name: "ordinary error", err: errors.New("download failed"), wantCode: 1,
			wantStderr: "sciget: download failed\n"},
		{name: "joined errors, one per line", err: errors.Join(errors.New("a failed"), errors.New("b failed")),
			wantCode: 1, wantStderr: "sciget: a failed\nsciget: b failed\n"},
		{name: "two-line usage error", err: usageError{"a\nb"}, wantCode: 2,
			wantStderr: "sciget: a\nsciget: b\nRun 'sciget help' for usage.\n"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var stderr bytes.Buffer
			code := exitCode(tt.err, &stderr)
			if code != tt.wantCode {
				t.Errorf("exit code = %d, want %d", code, tt.wantCode)
			}
			if got := stderr.String(); got != tt.wantStderr {
				t.Errorf("stderr = %q, want %q", got, tt.wantStderr)
			}
		})
	}
}
