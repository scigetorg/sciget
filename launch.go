package main

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// x11SocketDir holds local X11 displays' sockets; tests replace it.
var x11SocketDir = "/tmp/.X11-unix"

// startProcess starts cmd and can be replaced by tests.
var startProcess = func(cmd *exec.Cmd) error { return cmd.Start() }

// displaySocket returns the X11 socket for a local display (":20" or
// ":20.0"), and false for any other form, such as "host:10".
func displaySocket(display string) (string, bool) {
	num, ok := strings.CutPrefix(display, ":")
	if !ok || num == "" {
		return "", false
	}
	num, _, _ = strings.Cut(num, ".")
	for _, r := range num {
		if r < '0' || r > '9' {
			return "", false
		}
	}
	return filepath.Join(x11SocketDir, "X"+num), true
}

// displayLive reports whether display can show a window: a local display
// must answer on its socket (a socket file left by a stopped server does
// not), or on Linux on the abstract socket of the same name; any other
// non-empty form is trusted.
func displayLive(display string) bool {
	if display == "" {
		return false
	}
	path, local := displaySocket(display)
	if !local {
		return true
	}
	conn, err := net.DialTimeout("unix", path, time.Second)
	if err != nil && runtime.GOOS == "linux" {
		// An X server can listen on the abstract socket alone, with no file.
		conn, err = net.DialTimeout("unix", "@"+path, time.Second)
	}
	if err != nil {
		return false
	}
	conn.Close()
	return true
}

// logFile creates a new log file for one launch in dir, named after tool
// and version reduced to letters, digits, ".", "_" and "-", so a catalog
// value cannot leave dir and concurrent launches never share a file.
func logFile(dir, tool, version string) (*os.File, error) {
	safe := func(s string) string {
		return strings.Map(func(r rune) rune {
			if 'a' <= r && r <= 'z' || 'A' <= r && r <= 'Z' || '0' <= r && r <= '9' || r == '.' || r == '_' || r == '-' {
				return r
			}
			return '_'
		}, s)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	return os.CreateTemp(dir, safe(tool)+"-"+safe(version)+"-*.log")
}

// startDetached starts argv in its own session, stdin from /dev/null and
// output to log, so it outlives the TUI and the terminal. The caller waits
// for it, which also reaps it.
func startDetached(argv []string, log *os.File) (*exec.Cmd, error) {
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Stdout, cmd.Stderr = log, log
	detach(cmd)
	if err := startProcess(cmd); err != nil {
		return nil, err
	}
	return cmd, nil
}

// maxTail is how much of the end of a log lastLine reads.
const maxTail = 4 << 10

// lastLine returns the last non-blank line within the final 4 KiB of the
// file at path, cleaned for display, for reporting why a launch failed. It
// returns "" when there is none.
func lastLine(path string) string {
	f, err := os.Open(path)
	if err != nil {
		return err.Error()
	}
	defer f.Close()
	if info, err := f.Stat(); err == nil && info.Size() > maxTail {
		if _, err := f.Seek(-maxTail, io.SeekEnd); err != nil {
			return err.Error()
		}
	}
	data, err := io.ReadAll(io.LimitReader(f, maxTail))
	if err != nil {
		return err.Error()
	}
	lines := strings.Split(string(data), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		if line := strings.TrimSpace(printable(lines[i])); line != "" {
			return line
		}
	}
	return ""
}

// quickFailure is a shell that ended with an error before
// launch_check_seconds had passed, so it almost certainly never started.
type quickFailure struct{ code int }

func (q quickFailure) Error() string { return fmt.Sprintf("exit code %d", q.code) }

// shellRun runs a container's or host session's shell in the terminal while
// Bubble Tea has released it (a tea.ExecCommand). When readyDir is set, the
// session startup script creates <readyDir>/ready once interactive startup
// succeeds, so any non-zero exit before readiness pauses until Enter
// regardless of elapsed time (and any exit after readiness is treated as a
// normal session). Without readyDir (container shells), failures within check
// pause until Enter.
type shellRun struct {
	cmd      *exec.Cmd
	name     string
	check    time.Duration
	readyDir string
}

func (r *shellRun) SetStdin(in io.Reader)   { r.cmd.Stdin = in }
func (r *shellRun) SetStdout(out io.Writer) { r.cmd.Stdout = out }
func (r *shellRun) SetStderr(out io.Writer) { r.cmd.Stderr = out }

func (r *shellRun) Run() error {
	var readyFile string
	if r.readyDir != "" {
		defer os.RemoveAll(r.readyDir)
		readyFile = filepath.Join(r.readyDir, "ready")
		base := r.cmd.Env
		if len(base) == 0 {
			base = os.Environ()
		}
		r.cmd.Env = mergeEnviron(base, []string{"SCIGET_SHELL_READY=" + readyFile})
	}
	started := time.Now()
	err := r.cmd.Run()
	var exit *exec.ExitError
	if !errors.As(err, &exit) {
		return err
	}
	if readyFile != "" {
		if _, statErr := os.Stat(readyFile); statErr == nil {
			// The session shell started cleanly; a non-zero exit status is
			// just the last command the user ran before exiting.
			return err
		}
	} else if time.Since(started) >= r.check {
		return err
	}
	fmt.Fprintf(r.cmd.Stderr, "\nsciget: the terminal in %s stopped (exit code %d). Press Enter to return.",
		r.name, exit.ExitCode())
	bufio.NewReader(r.cmd.Stdin).ReadString('\n')
	return quickFailure{code: exit.ExitCode()}
}
