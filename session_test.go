package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
)

func TestFormatSessionPrompt(t *testing.T) {
	tests := []struct {
		name  string
		tools []sessionTool
		want  string
	}{
		{name: "empty", want: ""},
		{name: "one", tools: []sessionTool{{Tool: "fsl", Version: "6.0.7.8"}}, want: "fsl/6.0.7.8"},
		{name: "two", tools: []sessionTool{{Tool: "fsl", Version: "6.0.7"}, {Tool: "ants", Version: "2.5.1"}}, want: "fsl/6.0.7 + ants/2.5.1"},
		{name: "three", tools: []sessionTool{
			{Tool: "fsl", Version: "6.0.7"}, {Tool: "ants", Version: "2.5.1"}, {Tool: "mrtrix3", Version: "3.0.4"},
		}, want: "fsl/6.0.7 +2"},
		{name: "hostile characters sanitized", tools: []sessionTool{{Tool: "fsl$(id)", Version: "1'$(touch x)'"}}, want: "fsl__id_/1___touch_x__"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := formatSessionPrompt(sessionState{Tools: tt.tools}); got != tt.want {
				t.Errorf("formatSessionPrompt() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestPlanSessionAddAndRemove(t *testing.T) {
	t.Run("add two tools, replace version, out-of-order env removal, preserve modified env", func(t *testing.T) {
		dir := t.TempDir()
		fslDir := filepath.Join(dir, "fsl-6.0.3")
		antsDir := filepath.Join(dir, "ants-2.5.1")
		for _, d := range []string{fslDir, antsDir} {
			if err := os.MkdirAll(d, 0o755); err != nil {
				t.Fatal(err)
			}
		}
		if err := os.WriteFile(filepath.Join(fslDir, "bet"), nil, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(antsDir, "bet"), nil, 0o755); err != nil {
			t.Fatal(err)
		}

		env := newEnvSnapshot([]string{"PATH=/usr/bin:/bin", "SHARED=host0", "FSLDIR=/host/fsl"})
		var stderr bytes.Buffer

		t1 := preparedTool{
			c:          container{tool: "fsl", version: "6.0.3"},
			mode:       sessionModeWrappers,
			wrapperDir: fslDir,
			commands:   []string{"bet", "fslmaths"},
			env:        []envSetting{{name: "FSLDIR", value: "/opt/fsl-603"}, {name: "SHARED", value: "from-fsl"}},
		}
		t2 := preparedTool{
			c:          container{tool: "ants", version: "2.5.1"},
			mode:       sessionModeWrappers,
			wrapperDir: antsDir,
			commands:   []string{"antsRegistration", "bet"},
			env:        []envSetting{{name: "ANTSPATH", value: "/opt/ants"}, {name: "SHARED", value: "from-ants"}},
		}

		code, st := planSessionAdd(sessionState{}, env, []preparedTool{t1, t2}, config{}, &stderr, true)
		if len(st.Tools) != 2 {
			t.Fatalf("tools = %d, want 2", len(st.Tools))
		}
		if !strings.Contains(stderr.String(), "shadows bet from fsl/6.0.3") {
			t.Errorf("stderr = %q, want collision note for bet", stderr.String())
		}
		if !strings.Contains(code, "__sciget_path_prepend "+shellSingleQuote(fslDir)) ||
			!strings.Contains(code, "__sciget_path_prepend "+shellSingleQuote(antsDir)) {
			t.Errorf("code should use live __sciget_path_prepend:\n%s", code)
		}
		if strings.Contains(code, "export PATH=") {
			t.Errorf("code should not overwrite PATH with a static snapshot:\n%s", code)
		}

		// Adding same version again is a no-op.
		stderr.Reset()
		noopCode, st2 := planSessionAdd(st, env, []preparedTool{t1}, config{}, &stderr, true)
		if noopCode != "" || len(st2.Tools) != 2 || !strings.Contains(stderr.String(), "already loaded") {
			t.Errorf("duplicate add: code=%q, tools=%d, stderr=%q", noopCode, len(st2.Tools), stderr.String())
		}

		// Out-of-order removal: remove fsl (A) first while ants (B) still sets SHARED="from-ants".
		// SHARED must stay "from-ants" (not reverted to host0 yet).
		stderr.Reset()
		remA, stA, err := planSessionRemove(st, env, []string{"fsl"}, &stderr)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(remA, "SHARED=") || strings.Contains(remA, "unset SHARED") {
			t.Errorf("removing A disturbed B's SHARED value:\n%s", remA)
		}
		if !strings.Contains(remA, "export FSLDIR='/host/fsl'") {
			t.Errorf("removing A did not restore FSLDIR baseline:\n%s", remA)
		}

		// Now remove ants (B): SHARED must restore host baseline "host0", NOT A's "from-fsl"!
		stderr.Reset()
		remB, stB, err := planSessionRemove(stA, env, []string{"ants/2.5.1"}, &stderr)
		if err != nil {
			t.Fatal(err)
		}
		if len(stB.Tools) != 0 || !strings.Contains(remB, "export SHARED='host0'") || !strings.Contains(remB, "unset ANTSPATH") {
			t.Errorf("removing B did not restore host baseline:\n%s", remB)
		}

		// Manual user edit protection: if user modified a managed variable, leave it untouched.
		env2 := newEnvSnapshot([]string{"PATH=/usr/bin", "ANTSPATH=/user/custom"})
		stCustom := sessionState{
			Tools:   []sessionTool{{Name: "ants/2.5.1", Tool: "ants", Version: "2.5.1", Mode: sessionModeWrappers, WrapperDir: antsDir, Env: []sessionEnvEntry{{Name: "ANTSPATH", Value: "/opt/ants"}}}},
			HostEnv: map[string]envBaseline{"ANTSPATH": {Set: false}},
		}
		stderr.Reset()
		remCustom, _, err := planSessionRemove(stCustom, env2, []string{"ants"}, &stderr)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(remCustom, "unset ANTSPATH") || !strings.Contains(stderr.String(), "leaving ANTSPATH unchanged") {
			t.Errorf("manual edit was overwritten: code=%s stderr=%q", remCustom, stderr.String())
		}
	})

	t.Run("SCIGET_SESSION stays small even with 1000+ wrapped commands", func(t *testing.T) {
		many := make([]string, 1200)
		for i := range many {
			many[i] = fmt.Sprintf("fsl_long_command_name_%04d", i)
		}
		env := newEnvSnapshot([]string{"PATH=/usr/bin"})
		_, st := planSessionAdd(sessionState{}, env, []preparedTool{{
			c: container{tool: "fsl", version: "6.0.7"}, mode: sessionModeWrappers,
			wrapperDir: "/cache/wrappers/fsl", commands: many,
		}}, config{}, &bytes.Buffer{}, false)
		raw := marshalSessionState(st)
		if len(raw) > 512 {
			t.Errorf("SCIGET_SESSION too large (%d bytes) — commands leaked into env var", len(raw))
		}
	})

	t.Run("module failure restores replaced wrapper, aborts batch, and propagates non-zero exit", func(t *testing.T) {
		if runtime.GOOS == "windows" {
			t.Skip("POSIX shell")
		}
		env := newEnvSnapshot([]string{"PATH=/usr/bin"})
		oldWrap := sessionState{
			Tools: []sessionTool{{
				Name: "fsl/6.0.3", Tool: "fsl", Version: "6.0.3", Mode: sessionModeWrappers,
				WrapperDir: "/cache/fsl-6.0.3",
				Env:        []sessionEnvEntry{{Name: "FSLDIR", Value: "/opt/fsl-603"}},
			}},
			HostEnv: map[string]envBaseline{"FSLDIR": {Set: false}},
		}
		// Batch: replace fsl/6.0.3 (wrapper) with fsl/6.0.7 (module that fails), then add ants/2.5.1.
		code, _ := planSessionAdd(oldWrap, env, []preparedTool{
			{c: container{tool: "fsl", version: "6.0.7"}, mode: sessionModeModule, moduleDir: "/m/neuro"},
			{c: container{tool: "ants", version: "2.5.1"}, mode: sessionModeWrappers, wrapperDir: "/cache/ants"},
		}, config{}, &bytes.Buffer{}, true)

		// Run in real sh with a stub module() that fails on load.
		script := shellFunctionScript + `
module() {
  case "$1" in
  use) return 0 ;;
  load) return 1 ;;
  esac
}
PATH="/cache/fsl-6.0.3:/usr/bin"
export PATH
export FSLDIR="/opt/fsl-603"
` + code + `
rc=$?
printf 'RC=%d\nPATH=%s\nFSLDIR=%s\nSESSION=%s\n' "$rc" "$PATH" "$FSLDIR" "$SCIGET_SESSION"
`
		out, err := exec.Command("/bin/sh", "-c", script).CombinedOutput()
		if err != nil {
			t.Fatalf("sh failed: %v\n%s", err, out)
		}
		s := string(out)
		if !strings.Contains(s, "RC=1") {
			t.Errorf("expected RC=1 propagated past hash -r:\n%s", s)
		}
		if !strings.Contains(s, "PATH=/cache/fsl-6.0.3:/usr/bin") || !strings.Contains(s, "FSLDIR=/opt/fsl-603") {
			t.Errorf("replaced wrapper was not restored:\n%s", s)
		}
		_, sessionLine, ok := strings.Cut(s, "SESSION=")
		if !ok || strings.Contains(sessionLine, "ants/2.5.1") || strings.Contains(sessionLine, "fsl/6.0.7") || !strings.Contains(sessionLine, "fsl/6.0.3") {
			t.Errorf("failed batch SESSION state unexpected:\n%s", s)
		}
	})

	t.Run("mixed module then wrapper preserves both in live PATH", func(t *testing.T) {
		if runtime.GOOS == "windows" {
			t.Skip("POSIX shell")
		}
		env := newEnvSnapshot([]string{"PATH=/usr/bin"})
		code, _ := planSessionAdd(sessionState{}, env, []preparedTool{
			{c: container{tool: "fsl", version: "6.0.7"}, mode: sessionModeModule, moduleDir: "/m/neuro"},
			{c: container{tool: "ants", version: "2.5.1"}, mode: sessionModeWrappers, wrapperDir: "/cache/ants"},
		}, config{}, &bytes.Buffer{}, false)

		script := shellFunctionScript + `
module() {
  case "$1" in
  use) return 0 ;;
  load) PATH="/mod/fsl/bin:$PATH"; export PATH; return 0 ;;
  esac
}
PATH="/usr/bin"
export PATH
` + code + `
printf 'PATH=%s\n' "$PATH"
`
		out, err := exec.Command("/bin/sh", "-c", script).CombinedOutput()
		if err != nil {
			t.Fatalf("sh failed: %v\n%s", err, out)
		}
		if !strings.Contains(string(out), "PATH=/cache/ants:/mod/fsl/bin:/usr/bin") {
			t.Errorf("mixed module+wrapper lost PATH entry:\n%s", out)
		}
	})

	t.Run("LMOD_CMD fallback propagates failure even on empty stdout", func(t *testing.T) {
		if runtime.GOOS == "windows" {
			t.Skip("POSIX shell")
		}
		fakeLmod := filepath.Join(t.TempDir(), "lmod")
		if err := os.WriteFile(fakeLmod, []byte("#!/bin/sh\nexit 7\n"), 0o755); err != nil {
			t.Fatal(err)
		}
		script := shellFunctionScript + "\n__sciget_init_lmod && module load foo\nprintf 'RC=%d\\n' \"$?\"\n"
		cmd := exec.Command("/bin/sh", "-c", script)
		cmd.Env = mergeEnviron(os.Environ(), []string{"LMOD_CMD=" + fakeLmod, "LMOD_PKG="})
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("sh failed: %v\n%s", err, out)
		}
		if !strings.Contains(string(out), "RC=7") {
			t.Errorf("LMOD_CMD exit code masked:\n%s", out)
		}
	})
}

func TestSessionQuotingRealShell(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX shells only")
	}
	dir := t.TempDir()
	pwned := filepath.Join(dir, "pwned")
	hostileVer := "1'$(touch " + pwned + ")'`touch " + pwned + "`"
	wrapperDir := filepath.Join(dir, "w'dir")
	if err := os.MkdirAll(wrapperDir, 0o755); err != nil {
		t.Fatal(err)
	}
	env := newEnvSnapshot([]string{"PATH=/usr/bin:/bin"})
	p := preparedTool{
		c:          container{tool: "-x", version: hostileVer},
		mode:       sessionModeWrappers,
		wrapperDir: wrapperDir,
		commands:   []string{"tool"},
		env:        []envSetting{{name: "TEST_VAL", value: "a'b$(touch " + pwned + ")`id`"}},
	}
	code, _ := planSessionAdd(sessionState{}, env, []preparedTool{p}, config{}, &bytes.Buffer{}, true)

	for _, shell := range []string{"/bin/sh", "bash"} {
		shellPath, err := exec.LookPath(shell)
		if err != nil {
			continue
		}
		t.Run(shell, func(t *testing.T) {
			cmd := exec.Command(shellPath, "-c", shellFunctionScript+"\neval \"$CODE\"; printf '%s\\n%s\\n%s\\n' \"$TEST_VAL\" \"$SCIGET_PROMPT\" \"$SCIGET_SESSION\"")
			cmd.Env = append(os.Environ(), "CODE="+code)
			out, err := cmd.Output()
			if err != nil {
				t.Fatalf("%s failed: %v", shell, err)
			}
			if _, statErr := os.Stat(pwned); statErr == nil {
				t.Fatal("command injection created pwned file")
			}
			lines := strings.Split(strings.TrimSuffix(string(out), "\n"), "\n")
			if len(lines) != 3 {
				t.Fatalf("unexpected output lines %q", out)
			}
			if lines[0] != "a'b$(touch "+pwned+")`id`" {
				t.Errorf("TEST_VAL = %q, want literal value", lines[0])
			}
			if strings.ContainsAny(lines[1], "'$`") {
				t.Errorf("SCIGET_PROMPT = %q contains unsafe shell chars", lines[1])
			}
			var got sessionState
			if err := json.Unmarshal([]byte(lines[2]), &got); err != nil {
				t.Fatalf("invalid SCIGET_SESSION JSON %q: %v", lines[2], err)
			}
			if len(got.Tools) != 1 || got.Tools[0].Version != hostileVer {
				t.Errorf("parsed state = %+v, want version %q", got, hostileVer)
			}
		})
	}
}

func TestShellStartupFilesRealShells(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX shells only")
	}
	home := t.TempDir()
	// Hostile user .bashrc that defines an alias named sciget, adds a custom
	// bin dir to PATH (which wrapper startup MUST preserve!), and sets PS1.
	bashrc := "alias sciget='echo aliased'\nexport PATH=\"/user/rc/bin:$PATH\"\nPS1='user$ '\n"
	if err := os.WriteFile(filepath.Join(home, ".bashrc"), []byte(bashrc), 0o644); err != nil {
		t.Fatal(err)
	}
	fakeSelf := filepath.Join(home, "fake-sciget")
	fakeScript := `#!/bin/sh
if [ "$1" = "__session-eval" ] && [ "$2" = "add" ]; then
  printf '%s\n' "# SCIGET_SESSION_EVAL_V1"
  printf '%s\n' "export SCIGET_PROMPT='fsl/6.0.3 + ants/2.5.1'"
  printf '%s\n' "export ADDED_MARKER='yes'"
  exit 0
fi
printf 'DIRECT:%s\n' "$*"
`
	if err := os.WriteFile(fakeSelf, []byte(fakeScript), 0o755); err != nil {
		t.Fatal(err)
	}

	wrapperDir := filepath.Join(home, "w")
	env := newEnvSnapshot([]string{"PATH=/usr/bin:/bin"})
	initCode, finalState := planSessionAdd(sessionState{}, env, []preparedTool{{
		c: container{tool: "fsl", version: "6.0.3"}, mode: sessionModeWrappers, wrapperDir: wrapperDir, commands: []string{"bet"},
	}}, config{}, &bytes.Buffer{}, false)

	if bashPath, err := exec.LookPath("bash"); err == nil {
		t.Run("real bash --rcfile -i preserves bashrc PATH, handles add --help safely, creates sentinel", func(t *testing.T) {
			spec := shellSpec{kind: shellKindBash, path: bashPath}
			tmpDir, argv, extraEnv, err := writeShellStartupFiles(spec, fakeSelf, initCode, finalState, false)
			if err != nil {
				t.Fatal(err)
			}
			defer os.RemoveAll(tmpDir)
			readyDir := t.TempDir()
			readyFile := filepath.Join(readyDir, "ready")

			// Run the real argv (`bash --rcfile <rc> -i`) with commands piped on stdin!
			cmd := exec.Command(argv[0], argv[1:]...)
			cmd.Stdin = strings.NewReader(strings.Join([]string{
				`printf 'PATH_CHECK=%s\n' "$PATH"`,
				`sciget add --help`,
				`sciget add ants/2.5.1`,
				`printf 'PROMPT=%s\nMARKER=%s\nREADY=%s\nDIRECT=%s\n' "$SCIGET_PROMPT" "$ADDED_MARKER" "${SCIGET_SHELL_READY-unset}" "$(sciget version)"`,
				`exit 0`,
			}, "\n") + "\n")
			cmd.Env = mergeEnviron(os.Environ(), append(extraEnv, "HOME="+home, "PATH=/usr/bin:/bin", "SCIGET_SHELL_READY="+readyFile))
			out, err := cmd.CombinedOutput()
			if err != nil {
				t.Fatalf("bash -i failed: %v\n%s", err, out)
			}
			s := string(out)
			for _, want := range []string{
				"sciget session: fsl/6.0.3 (wrappers)",
				"PATH_CHECK=" + wrapperDir + ":/user/rc/bin:",
				"DIRECT:add --help",
				"PROMPT=fsl/6.0.3 + ants/2.5.1",
				"MARKER=yes",
				"READY=unset",
				"DIRECT=DIRECT:version",
			} {
				if !strings.Contains(s, want) {
					t.Errorf("bash -i output missing %q:\n%s", want, s)
				}
			}
			if _, err := os.Stat(readyFile); err != nil {
				t.Errorf("ready sentinel was not created: %v", err)
			}
			if _, err := os.Stat(tmpDir); !os.IsNotExist(err) {
				t.Errorf("temp startup dir %s was not cleaned up", tmpDir)
			}
		})
	}

	if shPath, err := exec.LookPath("sh"); err == nil {
		t.Run("sh ENV restored and live prompt updated", func(t *testing.T) {
			t.Setenv("ENV", "")
			os.Unsetenv("ENV")
			spec := shellSpec{kind: shellKindSh, path: shPath}
			tmpDir, argv, extraEnv, err := writeShellStartupFiles(spec, fakeSelf, initCode, finalState, false)
			if err != nil {
				t.Fatal(err)
			}
			defer os.RemoveAll(tmpDir)
			if len(argv) != 2 || argv[1] != "-i" || len(extraEnv) != 1 || !strings.HasPrefix(extraEnv[0], "ENV=") {
				t.Fatalf("sh argv=%v extraEnv=%v", argv, extraEnv)
			}
			rcPath := strings.TrimPrefix(extraEnv[0], "ENV=")
			cmd := exec.Command(shPath, "-c",
				`. "$1"; sciget add ants/2.5.1; printf 'ENV=%s\nPS1=%s\n' "${ENV-unset}" "$PS1"`,
				"sh", rcPath)
			cmd.Env = mergeEnviron(os.Environ(), append(extraEnv, "HOME="+home, "PS1=$ "))
			out, err := cmd.CombinedOutput()
			if err != nil {
				t.Fatalf("sh failed: %v\n%s", err, out)
			}
			s := string(out)
			if !strings.Contains(s, "ENV=unset") {
				t.Errorf("ENV leaked to child shell:\n%s", s)
			}
			if !strings.Contains(s, "PS1=(sciget fsl/6.0.3 + ants/2.5.1) $ ") {
				t.Errorf("sh PS1 did not update live:\n%s", s)
			}
			if _, err := os.Stat(tmpDir); !os.IsNotExist(err) {
				t.Errorf("temp startup dir %s was not cleaned up", tmpDir)
			}
		})
	}

	t.Run("zsh startup preserves ZDOTDIR set by ~/.zshenv", func(t *testing.T) {
		t.Setenv("ZDOTDIR", "")
		os.Unsetenv("ZDOTDIR")
		rc, zshenv := renderShellRC(shellSpec{kind: shellKindZsh, path: "/bin/zsh"}, "/tmp/sciget-z", fakeSelf, initCode, finalState, false)
		for _, want := range []string{`__sciget_had_zdot="${ZDOTDIR+1}"`, `__sciget_zdot="${ZDOTDIR-}"`, `export ZDOTDIR='/tmp/sciget-z'`} {
			if !strings.Contains(zshenv, want) {
				t.Errorf(".zshenv missing %q:\n%s", want, zshenv)
			}
		}
		for _, want := range []string{`export ZDOTDIR="${__sciget_zdot-}"`, `[ -f "${ZDOTDIR:-$HOME}/.zshrc" ]`, `unset __sciget_had_zdot __sciget_zdot`} {
			if !strings.Contains(rc, want) {
				t.Errorf(".zshrc missing %q:\n%s", want, rc)
			}
		}
		if zshPath, err := exec.LookPath("zsh"); err == nil {
			xdgZdot := filepath.Join(home, "xdg-zsh")
			if err := os.MkdirAll(xdgZdot, 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(home, ".zshenv"), []byte("export ZDOTDIR="+shellSingleQuote(xdgZdot)+"\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(xdgZdot, ".zshrc"), []byte("export ZSHRC_RAN=yes\nexport PATH=\"/xdg/zsh/bin:$PATH\"\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			tmpDir, argv, extraEnv, err := writeShellStartupFiles(shellSpec{kind: shellKindZsh, path: zshPath}, fakeSelf, initCode, finalState, false)
			if err != nil {
				t.Fatal(err)
			}
			defer os.RemoveAll(tmpDir)
			cmd := exec.Command(argv[0], argv[1:]...)
			cmd.Stdin = strings.NewReader("printf 'ZSHRC=%s\\nZDOT=%s\\nPATH=%s\\n' \"$ZSHRC_RAN\" \"$ZDOTDIR\" \"$PATH\"\nexit 0\n")
			cmd.Env = mergeEnviron(os.Environ(), append(extraEnv, "HOME="+home, "PATH=/usr/bin:/bin"))
			out, err := cmd.CombinedOutput()
			if err != nil {
				t.Fatalf("zsh -i failed: %v\n%s", err, out)
			}
			s := string(out)
			if !strings.Contains(s, "ZSHRC=yes") || !strings.Contains(s, "ZDOT="+xdgZdot) || !strings.Contains(s, "PATH="+wrapperDir+":/xdg/zsh/bin:") {
				t.Errorf("zsh -i did not preserve XDG ZDOTDIR/PATH:\n%s", s)
			}
		}
	})
}

func TestSessionCommandsCLI(t *testing.T) {
	t.Run("add outside session explains how to start one", func(t *testing.T) {
		t.Setenv("SCIGET_SESSION", "")
		var stdout, stderr bytes.Buffer
		code := run([]string{"add", "fsl/6.0.3"}, &stdout, &stderr)
		if code != 1 || !strings.Contains(stderr.String(), "sciget shell fsl/6.0.3") {
			t.Errorf("code=%d stderr=%q", code, stderr.String())
		}
	})

	t.Run("__session-eval --help writes nothing to stdout", func(t *testing.T) {
		var stdout, stderr bytes.Buffer
		_ = run([]string{"__session-eval", "add", "--help"}, &stdout, &stderr)
		if stdout.Len() != 0 {
			t.Errorf("__session-eval --help wrote to stdout: %q", stdout.String())
		}
	})

	t.Run("add with SCIGET_SESSION set warns about missing function", func(t *testing.T) {
		t.Setenv("SCIGET_SESSION", `{"tools":[]}`)
		var stdout, stderr bytes.Buffer
		code := run([]string{"add", "fsl/6.0.3"}, &stdout, &stderr)
		if code != 1 || !strings.Contains(stderr.String(), "shell function") {
			t.Errorf("code=%d stderr=%q", code, stderr.String())
		}
	})

	t.Run("session list outside, empty, active, and unloaded module", func(t *testing.T) {
		t.Setenv("SCIGET_SESSION", "")
		var out, errBuf bytes.Buffer
		if code := run([]string{"session"}, &out, &errBuf); code != 0 || !strings.Contains(out.String(), "Not in a sciget session") {
			t.Errorf("outside: code=%d out=%q err=%q", code, out.String(), errBuf.String())
		}

		t.Setenv("SCIGET_SESSION", `{"tools":[]}`)
		out.Reset()
		if code := run([]string{"session", "list"}, &out, &errBuf); code != 0 || !strings.Contains(out.String(), "No tools loaded") {
			t.Errorf("empty: code=%d out=%q", code, out.String())
		}

		wDir := t.TempDir()
		for _, name := range []string{"bet", "fslmaths"} {
			if err := os.WriteFile(filepath.Join(wDir, name), nil, 0o755); err != nil {
				t.Fatal(err)
			}
		}
		st := sessionState{Tools: []sessionTool{
			{Name: "fsl/6.0.3", Tool: "fsl", Version: "6.0.3", Mode: sessionModeWrappers, WrapperDir: wDir},
			{Name: "ants/2.5.1", Tool: "ants", Version: "2.5.1", Mode: sessionModeModule},
		}}
		t.Setenv("SCIGET_SESSION", marshalSessionState(st))
		t.Setenv("LOADEDMODULES", "other/1.0")
		out.Reset()
		if code := run([]string{"session"}, &out, &errBuf); code != 0 ||
			!strings.Contains(out.String(), "fsl/6.0.3\twrappers\t2 commands\tactive") ||
			!strings.Contains(out.String(), "ants/2.5.1\tmodule\tLmod module\tunloaded (module)") {
			t.Errorf("active: code=%d out=%q", code, out.String())
		}
	})

	t.Run("shell command prepares wrappers and invokes runShell", func(t *testing.T) {
		if runtime.GOOS == "windows" {
			t.Skip("uses /bin/sh")
		}
		c, _, store := localCatalog(t)
		base := filepath.Dir(store)
		apptainer, _ := fakeFinderApptainer(t, false)
		if err := writeConfig(os.Getenv("SCIGET_CONFIG"), map[string]string{
			"cache_dir": filepath.Join(base, "cache"), "container_dir": store,
			"module_dir": filepath.Join(base, "modules"), "apptainer_path": apptainer,
		}); err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(filepath.Join(store, c.fileName()), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(store, c.fileName(), c.fileName()+".simg"), nil, 0o644); err != nil {
			t.Fatal(err)
		}

		oldRunShell := runShell
		oldDetectLmod := detectLmod
		t.Cleanup(func() {
			runShell = oldRunShell
			detectLmod = oldDetectLmod
		})
		detectLmod = func() bool { return false }
		var gotPath string
		var gotArgs []string
		runShell = func(path string, args, env []string) error {
			gotPath = path
			gotArgs = args
			for i, a := range args {
				if a == "--rcfile" && i+1 < len(args) {
					os.RemoveAll(filepath.Dir(args[i+1]))
				}
			}
			return nil
		}

		t.Setenv("SHELL", "/usr/bin/fish-not-supported")
		t.Setenv("SCIGET_SESSION", "")
		var stdout, stderr bytes.Buffer
		if code := run([]string{"shell", "fsl/6.0.3"}, &stdout, &stderr); code != 0 {
			t.Fatalf("sciget shell failed: code=%d stderr=%q", code, stderr.String())
		}
		if gotPath == "" || len(gotArgs) == 0 {
			t.Fatalf("runShell not called: path=%q args=%v", gotPath, gotArgs)
		}
		if !strings.Contains(stderr.String(), "not directly supported") {
			t.Errorf("stderr = %q, want fallback shell explanation", stderr.String())
		}
		if _, err := os.Stat(filepath.Join(base, "modules", "neuro", "fsl", "6.0.3.lua")); !os.IsNotExist(err) {
			t.Error("sciget shell unexpectedly wrote a modulefile into module_dir")
		}
	})

	t.Run("prepareSessionTool chooses module mode only for safe local matching build with Lmod", func(t *testing.T) {
		if runtime.GOOS == "windows" {
			t.Skip("uses /bin/sh")
		}
		c, _, store := localCatalog(t)
		base := filepath.Dir(store)
		apptainer, _ := fakeFinderApptainer(t, false)
		if err := writeConfig(os.Getenv("SCIGET_CONFIG"), map[string]string{
			"cache_dir": filepath.Join(base, "cache"), "container_dir": store,
			"module_dir": filepath.Join(base, "modules"), "apptainer_path": apptainer,
		}); err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(filepath.Join(store, c.fileName()), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(store, c.fileName(), c.fileName()+".simg"), nil, 0o644); err != nil {
			t.Fatal(err)
		}
		cfg, err := readConfig()
		if err != nil {
			t.Fatal(err)
		}

		oldDetect := detectLmod
		t.Cleanup(func() { detectLmod = oldDetect })
		detectLmod = func() bool { return true }

		// No modulefile yet -> falls back to direct wrappers.
		p1, err := prepareSessionTool(context.Background(), cfg, c, strings.NewReader(""), &bytes.Buffer{}, false)
		if err != nil {
			t.Fatal(err)
		}
		if p1.mode != sessionModeWrappers || !reflect.DeepEqual(p1.commands, []string{"bet", "conda"}) {
			t.Errorf("without module: mode=%q commands=%v", p1.mode, p1.commands)
		}

		// After writeModule -> uses Lmod module mode.
		if _, err := writeModule(cfg, c, finderWant); err != nil {
			t.Fatal(err)
		}
		p2, err := prepareSessionTool(context.Background(), cfg, c, strings.NewReader(""), &bytes.Buffer{}, false)
		if err != nil {
			t.Fatal(err)
		}
		if p2.mode != sessionModeModule || p2.moduleDir != filepath.Join(base, "modules", "neuro") {
			t.Errorf("with module + Lmod: mode=%q moduleDir=%q", p2.mode, p2.moduleDir)
		}

		// Legacy module exporting a denied variable (e.g. PS0 or LD_PRELOAD) falls back to safe wrappers!
		modPath := filepath.Join(base, "modules", "neuro", "fsl", "6.0.3.lua")
		if err := os.WriteFile(modPath, []byte("setenv(\"LD_PRELOAD\", \"/evil.so\")\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		pLegacy, err := prepareSessionTool(context.Background(), cfg, c, strings.NewReader(""), &bytes.Buffer{}, false)
		if err != nil {
			t.Fatal(err)
		}
		if pLegacy.mode != sessionModeWrappers {
			t.Errorf("unsafe legacy module: mode=%q, want wrappers fallback", pLegacy.mode)
		}

		// With Lmod absent -> falls back to direct wrappers even when module exists.
		if _, err := writeModule(cfg, c, finderWant); err != nil {
			t.Fatal(err)
		}
		detectLmod = func() bool { return false }
		p3, err := prepareSessionTool(context.Background(), cfg, c, strings.NewReader(""), &bytes.Buffer{}, false)
		if err != nil {
			t.Fatal(err)
		}
		if p3.mode != sessionModeWrappers {
			t.Errorf("without Lmod: mode=%q, want wrappers", p3.mode)
		}
	})
}
