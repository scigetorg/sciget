package main

import (
	"errors"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestDisplaySocket(t *testing.T) {
	for _, tt := range []struct {
		display, want string
		local         bool
	}{
		{display: ":20", want: filepath.Join(x11SocketDir, "X20"), local: true},
		{display: ":20.0", want: filepath.Join(x11SocketDir, "X20"), local: true},
		{display: "host:10"},
		{display: ":"},
		{display: ":abc"},
		{display: ""},
	} {
		t.Run(tt.display, func(t *testing.T) {
			got, local := displaySocket(tt.display)
			if got != tt.want || local != tt.local {
				t.Errorf("displaySocket(%q) = %q, %v, want %q, %v", tt.display, got, local, tt.want, tt.local)
			}
		})
	}
}

func TestDisplayLive(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("unix sockets")
	}
	for _, tt := range []struct {
		name, display string
		listen, stale bool
		abstract      bool // listen on the abstract socket only (Linux)
		want          bool
	}{
		{name: "live", display: ":5", listen: true, want: true},
		{name: "abstract socket only", display: ":5", abstract: true, want: runtime.GOOS == "linux"},
		{name: "stale socket file", display: ":5", stale: true},
		{name: "missing", display: ":5"},
		{name: "remote is trusted", display: "host:10", want: true},
		{name: "unset", display: ""},
	} {
		t.Run(tt.name, func(t *testing.T) {
			// A short directory: unix socket paths are limited to about 100 bytes.
			dir, err := os.MkdirTemp("", "x11")
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { os.RemoveAll(dir) })
			old := x11SocketDir
			x11SocketDir = dir
			t.Cleanup(func() { x11SocketDir = old })
			switch {
			case tt.listen:
				l, err := net.Listen("unix", filepath.Join(dir, "X5"))
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { l.Close() })
			case tt.stale:
				if err := os.WriteFile(filepath.Join(dir, "X5"), nil, 0o600); err != nil {
					t.Fatal(err)
				}
			case tt.abstract:
				if runtime.GOOS != "linux" {
					return
				}
				l, err := net.Listen("unix", "@"+filepath.Join(dir, "X5"))
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { l.Close() })
			}
			if got := displayLive(tt.display); got != tt.want {
				t.Errorf("displayLive(%q) = %v, want %v", tt.display, got, tt.want)
			}
		})
	}
}

func TestLogFile(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "logs")
	for _, tt := range []struct {
		name, tool, version, wantPrefix string
	}{
		{name: "plain", tool: "itksnap", version: "4.4.0", wantPrefix: "itksnap-4.4.0-"},
		{name: "hostile values stay inside", tool: "../../etc", version: "a/b\x00", wantPrefix: ".._.._etc-a_b_-"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			f, err := logFile(dir, tt.tool, tt.version)
			if err != nil {
				t.Fatal(err)
			}
			f.Close()
			if filepath.Dir(f.Name()) != dir || !strings.HasPrefix(filepath.Base(f.Name()), tt.wantPrefix) {
				t.Errorf("log file %q, want %s/%s*", f.Name(), dir, tt.wantPrefix)
			}
		})
	}
	a, _ := logFile(dir, "x", "1")
	b, _ := logFile(dir, "x", "1")
	if a.Name() == b.Name() {
		t.Errorf("two launches share %q", a.Name())
	}
}

func TestStartDetached(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses /bin/sh")
	}
	for _, tt := range []struct {
		name, script, wantLast string
		wantCode               int
	}{
		{name: "success", script: "echo started", wantLast: "started"},
		{name: "failure keeps the last line", script: "echo one; echo 'cannot connect to X server :20' >&2; exit 3",
			wantLast: "cannot connect to X server :20", wantCode: 3},
		{name: "no output", script: "exit 1", wantCode: 1},
		{name: "colour codes and blank lines", script: "printf '\\033[31mfailed\\033[0m\\n\\n  \\n'; exit 2",
			wantLast: "failed", wantCode: 2},
		{name: "only the final 4 KiB", script: "echo early; head -c 5000 /dev/zero | tr '\\0' x; echo", wantLast: strings.Repeat("x", 4095)},
	} {
		t.Run(tt.name, func(t *testing.T) {
			log, err := logFile(t.TempDir(), "t", "1")
			if err != nil {
				t.Fatal(err)
			}
			defer log.Close()
			cmd, err := startDetached([]string{"/bin/sh", "-c", tt.script}, log)
			if err != nil {
				t.Fatal(err)
			}
			err = cmd.Wait()
			var exit *exec.ExitError
			code := 0
			if errors.As(err, &exit) {
				code = exit.ExitCode()
			}
			if code != tt.wantCode {
				t.Errorf("exit code %d, want %d", code, tt.wantCode)
			}
			if got := lastLine(log.Name()); got != tt.wantLast {
				t.Errorf("lastLine() = %q, want %q", got, tt.wantLast)
			}
		})
	}
}

func TestShellRun(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses /bin/sh")
	}
	for _, tt := range []struct {
		name       string
		script     string
		check      time.Duration
		wantErr    error
		wantPrompt bool
	}{
		{name: "clean exit", script: "exit 0", check: time.Minute},
		{name: "quick failure pauses", script: "exit 3", check: time.Minute, wantErr: quickFailure{code: 3}, wantPrompt: true},
		{name: "a later failure is a normal session", script: "exit 3", wantErr: &exec.ExitError{}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			var stderr strings.Builder
			r := &shellRun{cmd: exec.Command("/bin/sh", "-c", tt.script), name: "fsl/6.0.3", check: tt.check}
			r.SetStdin(strings.NewReader("\n"))
			r.SetStdout(io.Discard)
			r.SetStderr(&stderr)
			err := r.Run()
			if reflect.TypeOf(err) != reflect.TypeOf(tt.wantErr) || tt.wantErr == (quickFailure{code: 3}) && err != tt.wantErr {
				t.Errorf("Run() = %#v, want %#v", err, tt.wantErr)
			}
			const prompt = "sciget: the terminal in fsl/6.0.3 stopped (exit code 3). Press Enter to return."
			if got := strings.Contains(stderr.String(), prompt); got != tt.wantPrompt {
				t.Errorf("stderr = %q, want the prompt %v", stderr.String(), tt.wantPrompt)
			}
		})
	}

	t.Run("readyDir suppresses quickFailure after interactive startup", func(t *testing.T) {
		readyDir, err := os.MkdirTemp("", "sciget-test-ready-*")
		if err != nil {
			t.Fatal(err)
		}
		var stderr strings.Builder
		r := &shellRun{
			cmd:      exec.Command("/bin/sh", "-c", `: > "$SCIGET_SHELL_READY"; exit 3`),
			name:     "fsl/6.0.3",
			check:    time.Minute,
			readyDir: readyDir,
		}
		r.SetStdin(strings.NewReader(""))
		r.SetStdout(io.Discard)
		r.SetStderr(&stderr)
		err = r.Run()
		var exit *exec.ExitError
		if !errors.As(err, &exit) || exit.ExitCode() != 3 || stderr.Len() != 0 {
			t.Errorf("Run() = %#v, stderr = %q, want normal ExitError(3) without pause", err, stderr.String())
		}
		if _, statErr := os.Stat(readyDir); !os.IsNotExist(statErr) {
			t.Errorf("readyDir %s was not cleaned up", readyDir)
		}
	})

	t.Run("readyDir catches failure before startup completes even after check duration", func(t *testing.T) {
		readyDir, err := os.MkdirTemp("", "sciget-test-ready-*")
		if err != nil {
			t.Fatal(err)
		}
		var stderr strings.Builder
		r := &shellRun{
			cmd:      exec.Command("/bin/sh", "-c", "exit 3"),
			name:     "fsl/6.0.3",
			check:    0, // already elapsed: readyDir must still catch missing readiness
			readyDir: readyDir,
		}
		r.SetStdin(strings.NewReader("\n"))
		r.SetStdout(io.Discard)
		r.SetStderr(&stderr)
		err = r.Run()
		if err != (quickFailure{code: 3}) {
			t.Errorf("Run() = %#v, want quickFailure{3}", err)
		}
	})
}
