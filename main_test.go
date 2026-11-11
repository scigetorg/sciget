package main

import (
	"bytes"
	"errors"
	"fmt"
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
		"fsleyesGUI-fsl 6.0.3": {"version": "20200905", "exec": "fsleyes"}}}}`

	tests := []struct {
		name          string
		args          []string
		catalogue     string // written to the cache; no file when empty
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
			wantStderrHas: "sciget: no catalogue at "},
		{name: "find", args: []string{"find", "fsl"}, catalogue: catalogue,
			wantStdout: "fsl/6.0.3\timage registration\tyes\n"},
		{name: "find completes tool names", args: []string{"__complete", "find", "fs"}, catalogue: catalogue,
			wantStdout: "fsl/\n:6\n", wantStderrHas: "Completion ended with directive"},
		{name: "find completion without a catalogue", args: []string{"__complete", "find", "fs"},
			wantStdout: ":1\n", wantStderrHas: "Completion ended with directive: ShellCompDirectiveError"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cache := t.TempDir()
			t.Setenv("XDG_CACHE_HOME", cache)
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
		})
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
