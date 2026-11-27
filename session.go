package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"slices"
	"strings"
	"syscall"

	"github.com/spf13/cobra"
)

const (
	sessionModeModule   = "module"
	sessionModeWrappers = "wrappers"

	shellKindBash = "bash"
	shellKindZsh  = "zsh"
	shellKindSh   = "sh"

	sessionEvalHeader = "# SCIGET_SESSION_EVAL_V1\n"
)

// runShell replaces sciget with the host shell process; tests replace it.
var runShell = execShell

// detectLmod reports whether Lmod is available in this environment; tests
// replace it.
var detectLmod = lmodAvailable

// sessionState is serialized into SCIGET_SESSION so the live shell and nested
// shells track which tools sciget loaded and how to cleanly unload them.
// Command lists are deliberately omitted to keep the variable small and avoid
// hitting Linux's MAX_ARG_STRLEN / E2BIG limits.
type sessionState struct {
	Tools   []sessionTool          `json:"tools"`
	HostEnv map[string]envBaseline `json:"host_env,omitempty"`
}

// envBaseline records a host variable's value before any session tool set it.
type envBaseline struct {
	Value string `json:"value,omitempty"`
	Set   bool   `json:"set,omitempty"`
}

// sessionTool records one tool active in a sciget host session.
type sessionTool struct {
	Name       string            `json:"name"`
	Tool       string            `json:"tool"`
	Version    string            `json:"version"`
	Catalog    string            `json:"catalog,omitempty"`
	BuildDate  string            `json:"build_date,omitempty"`
	Mode       string            `json:"mode"` // sessionModeModule or sessionModeWrappers
	ModuleDir  string            `json:"module_dir,omitempty"`
	WrapperDir string            `json:"wrapper_dir,omitempty"`
	Env        []sessionEnvEntry `json:"env,omitempty"`
}

// sessionEnvEntry records one host environment variable set by a tool in
// wrapper mode.
type sessionEnvEntry struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

func execShell(path string, args, env []string) error {
	if err := syscall.Exec(path, args, env); err != nil {
		return fmt.Errorf("starting shell: %w", err)
	}
	return nil
}

func lmodAvailable() bool {
	if cmd := os.Getenv("LMOD_CMD"); cmd != "" {
		if st, err := os.Stat(cmd); err == nil && !st.IsDir() && st.Mode()&0o111 != 0 {
			return true
		}
	}
	if pkg := os.Getenv("LMOD_PKG"); pkg != "" {
		cmd := filepath.Join(pkg, "libexec", "lmod")
		if st, err := os.Stat(cmd); err == nil && !st.IsDir() && st.Mode()&0o111 != 0 {
			return true
		}
	}
	return false
}

func parseSessionState(raw string) sessionState {
	if strings.TrimSpace(raw) == "" {
		return sessionState{}
	}
	var st sessionState
	if err := json.Unmarshal([]byte(raw), &st); err != nil {
		return sessionState{}
	}
	return st
}

func cloneSessionState(st sessionState) sessionState {
	out := sessionState{Tools: slices.Clone(st.Tools)}
	if len(st.HostEnv) > 0 {
		out.HostEnv = make(map[string]envBaseline, len(st.HostEnv))
		for k, v := range st.HostEnv {
			out.HostEnv[k] = v
		}
	}
	return out
}

func marshalSessionState(st sessionState) string {
	if st.Tools == nil {
		st.Tools = []sessionTool{}
	}
	data, err := json.Marshal(st)
	if err != nil {
		return `{"tools":[]}`
	}
	return string(data)
}

// formatSessionPrompt returns a safe, compact display string for SCIGET_PROMPT
// built strictly from safePromptToken so prompt/shell expansion cannot fire.
func formatSessionPrompt(st sessionState) string {
	switch len(st.Tools) {
	case 0:
		return ""
	case 1:
		t := st.Tools[0]
		return safePromptToken(t.Tool) + "/" + safePromptToken(t.Version)
	case 2:
		t0, t1 := st.Tools[0], st.Tools[1]
		return safePromptToken(t0.Tool) + "/" + safePromptToken(t0.Version) + " + " +
			safePromptToken(t1.Tool) + "/" + safePromptToken(t1.Version)
	default:
		t0 := st.Tools[0]
		return fmt.Sprintf("%s/%s +%d", safePromptToken(t0.Tool), safePromptToken(t0.Version), len(st.Tools)-1)
	}
}

// sessionToolCommands reconstructs a loaded tool's command names on demand
// from its wrapper directory or command cache so SCIGET_SESSION stays small.
func sessionToolCommands(t sessionTool, cfg config) []string {
	if t.WrapperDir != "" {
		if entries, err := os.ReadDir(t.WrapperDir); err == nil {
			names := make([]string, 0, len(entries))
			for _, e := range entries {
				if !e.IsDir() {
					names = append(names, e.Name())
				}
			}
			slices.Sort(names)
			return names
		}
	}
	if t.Catalog != "" && t.Tool != "" && t.Version != "" && t.BuildDate != "" {
		c := container{catalog: t.Catalog, tool: t.Tool, version: t.Version, buildDate: t.BuildDate}
		if cachePath, err := commandCachePath(cfg, c, t.BuildDate); err == nil {
			if data, err := os.ReadFile(cachePath); err == nil {
				var list commandList
				if json.Unmarshal(data, &list) == nil {
					excludes, _ := cfg.getList("finder_excludes")
					names, _ := moduleCommands(list, excludes)
					return names
				}
			}
		}
	}
	return nil
}

// preparedTool is a container resolved and ready to activate in a host shell.
type preparedTool struct {
	c          container
	loc        location
	buildDate  string
	mode       string // sessionModeModule or sessionModeWrappers
	moduleDir  string
	wrapperDir string
	commands   []string
	env        []envSetting
}

// prepareSessionTool locates c (prompting to pull if needed) and prepares
// either Lmod module activation or cached direct wrappers without modifying
// module_dir.
func prepareSessionTool(ctx context.Context, cfg config, c container, stdin io.Reader, stderr io.Writer, interactive bool) (preparedTool, error) {
	loc, err := locate(cfg, c)
	var missing notOnMachineError
	if errors.As(err, &missing) {
		loc, err = pullOnRun(cfg, c, stdin, stderr, interactive)
	}
	if err != nil {
		return preparedTool{}, err
	}
	buildDate := loc.buildDate
	if buildDate == "" {
		buildDate = c.buildDate
	}
	ms := inspectModule(cfg, c, loc, nil)
	if !ms.shareOnly && !ms.unsafe && ms.exists && ms.matchesLocated &&
		detectLmod() && moduleFileSafeForSession(ms.file) {
		var cmds []string
		if cachePath, cacheErr := commandCachePath(cfg, c, buildDate); cacheErr == nil {
			if data, readErr := os.ReadFile(cachePath); readErr == nil {
				var list commandList
				if json.Unmarshal(data, &list) == nil {
					excludes, _ := cfg.getList("finder_excludes")
					cmds, _ = moduleCommands(list, excludes)
				}
			}
		}
		return preparedTool{
			c:         c,
			loc:       loc,
			buildDate: buildDate,
			mode:      sessionModeModule,
			moduleDir: ms.catalogDir,
			commands:  cmds,
		}, nil
	}
	list, err := listCommands(ctx, cfg, c, loc)
	if err != nil {
		return preparedTool{}, err
	}
	wrapperDir, info, envSettings, err := ensureSessionWrappers(cfg, c, buildDate, list)
	if err != nil {
		return preparedTool{}, err
	}
	return preparedTool{
		c:          c,
		loc:        loc,
		buildDate:  buildDate,
		mode:       sessionModeWrappers,
		wrapperDir: wrapperDir,
		commands:   info.commands,
		env:        envSettings,
	}, nil
}

// envSnapshot captures the current host environment for multi-tool planning.
type envSnapshot struct {
	values map[string]string
}

func newEnvSnapshot(environ []string) *envSnapshot {
	s := &envSnapshot{values: map[string]string{}}
	for _, entry := range environ {
		k, v, ok := strings.Cut(entry, "=")
		if !ok {
			continue
		}
		s.values[k] = v
	}
	return s
}

// findEnvOwner returns the newest tool in st setting varName and its value.
func findEnvOwner(st sessionState, varName string) (ownerName, val string, found bool) {
	for i := len(st.Tools) - 1; i >= 0; i-- {
		for _, e := range st.Tools[i].Env {
			if e.Name == varName {
				return st.Tools[i].Name, e.Value, true
			}
		}
	}
	return "", "", false
}

// emitRemoveWrapperTool emits POSIX shell statements to strip old's wrapper
// directory from the live $PATH and restore any environment variables it owned.
func emitRemoveWrapperTool(buf *strings.Builder, st *sessionState, env *envSnapshot, old sessionTool, stderr io.Writer) {
	if old.WrapperDir != "" {
		fmt.Fprintf(buf, "__sciget_path_remove %s\n", shellSingleQuote(old.WrapperDir))
	}
	for i := len(old.Env) - 1; i >= 0; i-- {
		e := old.Env[i]
		if checkEnvName(e.Name) != nil || isDeniedHostEnv(e.Name) {
			continue
		}
		cur, hasCur := env.values[e.Name]
		// If another remaining tool still sets this variable, either it
		// already superseded old (leave it alone) or old was on top (restore
		// the remaining tool's value).
		if _, ownerVal, hasOwner := findEnvOwner(*st, e.Name); hasOwner {
			if hasCur && cur == e.Value && ownerVal != e.Value {
				env.values[e.Name] = ownerVal
				fmt.Fprintf(buf, "export %s=%s\n", e.Name, shellSingleQuote(ownerVal))
			}
			continue
		}
		// Final owner leaving: consult and retire the recorded host baseline.
		base := st.HostEnv[e.Name]
		delete(st.HostEnv, e.Name)
		if !hasCur || cur != e.Value {
			if stderr != nil {
				fmt.Fprintf(stderr, "sciget: note: leaving %s unchanged (modified during session)\n", printable(e.Name))
			}
			continue
		}
		if base.Set {
			env.values[e.Name] = base.Value
			fmt.Fprintf(buf, "export %s=%s\n", e.Name, shellSingleQuote(base.Value))
		} else {
			delete(env.values, e.Name)
			fmt.Fprintf(buf, "unset %s\n", e.Name)
		}
	}
}

func emitRestoreWrapperTool(buf *strings.Builder, t sessionTool) {
	if t.WrapperDir != "" {
		fmt.Fprintf(buf, "  __sciget_path_prepend %s\n", shellSingleQuote(t.WrapperDir))
	}
	for _, e := range t.Env {
		if checkEnvName(e.Name) != nil || isDeniedHostEnv(e.Name) {
			continue
		}
		fmt.Fprintf(buf, "  export %s=%s\n", e.Name, shellSingleQuote(e.Value))
	}
}

func warnCommandCollisions(st sessionState, added preparedTool, cfg config, stderr io.Writer) {
	if stderr == nil || len(added.commands) == 0 || len(st.Tools) == 0 {
		return
	}
	newSet := make(map[string]bool, len(added.commands))
	for _, cmd := range added.commands {
		newSet[cmd] = true
	}
	for _, existing := range st.Tools {
		existingCmds := sessionToolCommands(existing, cfg)
		var shared []string
		for _, cmd := range existingCmds {
			if newSet[cmd] {
				shared = append(shared, cmd)
			}
		}
		if len(shared) == 0 {
			continue
		}
		preview := strings.Join(shared[:min(len(shared), 5)], ", ")
		if len(shared) > 5 {
			preview += fmt.Sprintf(" (+%d more)", len(shared)-5)
		}
		fmt.Fprintf(stderr, "sciget: note: %s shadows %s from %s\n",
			printable(added.c.name()), printable(preview), printable(existing.Name))
	}
}

// planSessionAdd generates POSIX shell code to add prepared tools to st
// (replacing any different version of the same tool). Both initial
// `sciget shell` startup and live `sciget add` use this single planner.
// All PATH mutations operate on the shell's live $PATH at runtime so rcfile
// additions and earlier module loads are preserved.
func planSessionAdd(st sessionState, env *envSnapshot, prepared []preparedTool, cfg config, stderr io.Writer, announce bool) (code string, next sessionState) {
	next = cloneSessionState(st)
	var buf strings.Builder
	hasModule := slices.ContainsFunc(prepared, func(p preparedTool) bool { return p.mode == sessionModeModule }) ||
		slices.ContainsFunc(prepared, func(p preparedTool) bool {
			return slices.ContainsFunc(next.Tools, func(t sessionTool) bool {
				return t.Tool == p.c.tool && t.Name != p.c.name() && t.Mode == sessionModeModule
			})
		})
	if hasModule {
		buf.WriteString("__sciget_plan_rc=0\n")
	}
	anyChanged := false

	for _, p := range prepared {
		name := p.c.name()
		if slices.ContainsFunc(next.Tools, func(t sessionTool) bool { return t.Name == name }) {
			if announce && stderr != nil {
				fmt.Fprintf(stderr, "sciget: %s is already loaded in this session\n", printable(name))
			}
			continue
		}
		if hasModule {
			buf.WriteString("if [ \"$__sciget_plan_rc\" -eq 0 ]; then\n")
		}

		var replaced *sessionTool
		beforeReplace := cloneSessionState(next)
		replacingOldModule := false
		if idx := slices.IndexFunc(next.Tools, func(t sessionTool) bool { return t.Tool == p.c.tool }); idx >= 0 {
			old := next.Tools[idx]
			next.Tools = append(next.Tools[:idx], next.Tools[idx+1:]...)
			replaced = &old
			if old.Mode == sessionModeModule {
				replacingOldModule = true
				fmt.Fprintf(&buf, "if __sciget_init_lmod && module unload %s; then\n", shellSingleQuote(old.Name))
			} else {
				emitRemoveWrapperTool(&buf, &next, env, old, stderr)
			}
		}
		warnCommandCollisions(next, p, cfg, stderr)

		if p.mode == sessionModeModule {
			beforeCommit := cloneSessionState(next)
			newEntry := sessionTool{
				Name:      name,
				Tool:      p.c.tool,
				Version:   p.c.version,
				Catalog:   p.c.catalog,
				BuildDate: p.buildDate,
				Mode:      sessionModeModule,
				ModuleDir: p.moduleDir,
			}
			next.Tools = append(next.Tools, newEntry)
			anyChanged = true

			var loadCond strings.Builder
			loadCond.WriteString("__sciget_init_lmod")
			if p.moduleDir != "" {
				fmt.Fprintf(&loadCond, " && module use %s", shellSingleQuote(p.moduleDir))
			}
			fmt.Fprintf(&loadCond, " && module load %s", shellSingleQuote(name))

			fmt.Fprintf(&buf, "if %s; then\n", loadCond.String())
			fmt.Fprintf(&buf, "  export SCIGET_SESSION=%s\n", shellSingleQuote(marshalSessionState(next)))
			fmt.Fprintf(&buf, "  export SCIGET_PROMPT=%s\n", shellSingleQuote(formatSessionPrompt(next)))
			if announce {
				msg := fmt.Sprintf("sciget: loaded %s (module)", printable(name))
				if replaced != nil {
					msg = fmt.Sprintf("sciget: replaced %s with %s (module)", printable(replaced.Name), printable(name))
				}
				fmt.Fprintf(&buf, "  printf '%%s\\n' %s >&2\n", shellSingleQuote(msg))
			}
			buf.WriteString("else\n")
			buf.WriteString("  __sciget_plan_rc=1\n")
			switch {
			case replaced != nil && replaced.Mode == sessionModeModule:
				fmt.Fprintf(&buf, "  if __sciget_init_lmod && module load %s 2>/dev/null; then\n", shellSingleQuote(replaced.Name))
				fmt.Fprintf(&buf, "    export SCIGET_SESSION=%s\n", shellSingleQuote(marshalSessionState(beforeReplace)))
				fmt.Fprintf(&buf, "    export SCIGET_PROMPT=%s\n", shellSingleQuote(formatSessionPrompt(beforeReplace)))
				fmt.Fprintf(&buf, "    printf '%%s\\n' %s >&2\n",
					shellSingleQuote(fmt.Sprintf("sciget: failed to load module %s (restored %s)", printable(name), printable(replaced.Name))))
				buf.WriteString("  else\n")
				fmt.Fprintf(&buf, "    export SCIGET_SESSION=%s\n", shellSingleQuote(marshalSessionState(beforeCommit)))
				fmt.Fprintf(&buf, "    export SCIGET_PROMPT=%s\n", shellSingleQuote(formatSessionPrompt(beforeCommit)))
				fmt.Fprintf(&buf, "    printf '%%s\\n' %s >&2\n",
					shellSingleQuote(fmt.Sprintf("sciget: failed to load module %s and could not restore %s", printable(name), printable(replaced.Name))))
				buf.WriteString("  fi\n")
			case replaced != nil && replaced.Mode == sessionModeWrappers:
				emitRestoreWrapperTool(&buf, *replaced)
				fmt.Fprintf(&buf, "  export SCIGET_SESSION=%s\n", shellSingleQuote(marshalSessionState(beforeReplace)))
				fmt.Fprintf(&buf, "  export SCIGET_PROMPT=%s\n", shellSingleQuote(formatSessionPrompt(beforeReplace)))
				fmt.Fprintf(&buf, "  printf '%%s\\n' %s >&2\n",
					shellSingleQuote(fmt.Sprintf("sciget: failed to load module %s (restored %s)", printable(name), printable(replaced.Name))))
			default:
				fmt.Fprintf(&buf, "  export SCIGET_SESSION=%s\n", shellSingleQuote(marshalSessionState(beforeCommit)))
				fmt.Fprintf(&buf, "  export SCIGET_PROMPT=%s\n", shellSingleQuote(formatSessionPrompt(beforeCommit)))
				fmt.Fprintf(&buf, "  printf '%%s\\n' %s >&2\n",
					shellSingleQuote(fmt.Sprintf("sciget: failed to load module %s", printable(name))))
			}
			buf.WriteString("fi\n")
		} else {
			if p.wrapperDir != "" {
				fmt.Fprintf(&buf, "__sciget_path_prepend %s\n", shellSingleQuote(p.wrapperDir))
			}
			var envEntries []sessionEnvEntry
			for _, s := range p.env {
				if checkEnvName(s.name) != nil || isDeniedHostEnv(s.name) {
					continue
				}
				cur, hasCur := env.values[s.name]
				if next.HostEnv == nil {
					next.HostEnv = map[string]envBaseline{}
				}
				if _, recorded := next.HostEnv[s.name]; !recorded {
					// Cap stored baseline size to keep SCIGET_SESSION bounded.
					if len(cur) <= maxHostEnvValue {
						next.HostEnv[s.name] = envBaseline{Value: cur, Set: hasCur}
					}
				}
				if hasCur && cur != s.value && stderr != nil {
					if owner, _, ok := findEnvOwner(next, s.name); ok {
						fmt.Fprintf(stderr, "sciget: note: %s overrides %s (previously set by %s)\n",
							printable(name), printable(s.name), printable(owner))
					}
				}
				envEntries = append(envEntries, sessionEnvEntry{Name: s.name, Value: s.value})
				env.values[s.name] = s.value
				fmt.Fprintf(&buf, "export %s=%s\n", s.name, shellSingleQuote(s.value))
			}
			newEntry := sessionTool{
				Name:       name,
				Tool:       p.c.tool,
				Version:    p.c.version,
				Catalog:    p.c.catalog,
				BuildDate:  p.buildDate,
				Mode:       sessionModeWrappers,
				WrapperDir: p.wrapperDir,
				Env:        envEntries,
			}
			next.Tools = append(next.Tools, newEntry)
			anyChanged = true
			fmt.Fprintf(&buf, "export SCIGET_SESSION=%s\n", shellSingleQuote(marshalSessionState(next)))
			fmt.Fprintf(&buf, "export SCIGET_PROMPT=%s\n", shellSingleQuote(formatSessionPrompt(next)))
			if announce {
				noun := "commands"
				if len(p.commands) == 1 {
					noun = "command"
				}
				msg := fmt.Sprintf("sciget: loaded %s (wrappers, %d %s)", printable(name), len(p.commands), noun)
				if replaced != nil {
					msg = fmt.Sprintf("sciget: replaced %s with %s (wrappers, %d %s)",
						printable(replaced.Name), printable(name), len(p.commands), noun)
				}
				fmt.Fprintf(&buf, "printf '%%s\\n' %s >&2\n", shellSingleQuote(msg))
			}
		}

		if replacingOldModule {
			buf.WriteString("else\n")
			buf.WriteString("  __sciget_plan_rc=1\n")
			fmt.Fprintf(&buf, "  printf '%%s\\n' %s >&2\n",
				shellSingleQuote(fmt.Sprintf("sciget: failed to unload module %s before loading %s", printable(replaced.Name), printable(name))))
			buf.WriteString("fi\n")
		}

		if hasModule {
			buf.WriteString("fi\n")
		}
	}

	if anyChanged {
		buf.WriteString("hash -r 2>/dev/null || true\n")
	}
	if hasModule {
		buf.WriteString("if [ \"$__sciget_plan_rc\" -ne 0 ]; then unset __sciget_plan_rc; false; else unset __sciget_plan_rc; true; fi\n")
	}
	return buf.String(), next
}

// planSessionRemove generates POSIX shell code to remove named tools (matched
// by exact tool/version or bare tool name) from st.
func planSessionRemove(st sessionState, env *envSnapshot, queries []string, stderr io.Writer) (code string, next sessionState, err error) {
	next = cloneSessionState(st)
	var buf strings.Builder
	buf.WriteString("__sciget_plan_rc=0\n")
	hasModule := false

	for _, q := range queries {
		idx := slices.IndexFunc(next.Tools, func(t sessionTool) bool {
			return t.Name == q || t.Tool == q
		})
		if idx < 0 {
			return "", st, fmt.Errorf("%q is not loaded in this session", q)
		}
		old := next.Tools[idx]
		next.Tools = append(next.Tools[:idx], next.Tools[idx+1:]...)

		if old.Mode == sessionModeModule {
			hasModule = true
			buf.WriteString("if [ \"$__sciget_plan_rc\" -eq 0 ]; then\n")
			fmt.Fprintf(&buf, "  if __sciget_init_lmod && module unload %s; then\n", shellSingleQuote(old.Name))
			fmt.Fprintf(&buf, "    export SCIGET_SESSION=%s\n", shellSingleQuote(marshalSessionState(next)))
			fmt.Fprintf(&buf, "    export SCIGET_PROMPT=%s\n", shellSingleQuote(formatSessionPrompt(next)))
			fmt.Fprintf(&buf, "    printf '%%s\\n' %s >&2\n", shellSingleQuote("sciget: removed "+printable(old.Name)))
			buf.WriteString("  else\n")
			buf.WriteString("    __sciget_plan_rc=1\n")
			fmt.Fprintf(&buf, "    printf '%%s\\n' %s >&2\n",
				shellSingleQuote("sciget: failed to unload module "+printable(old.Name)))
			buf.WriteString("  fi\n")
			buf.WriteString("fi\n")
			continue
		}

		if hasModule {
			buf.WriteString("if [ \"$__sciget_plan_rc\" -eq 0 ]; then\n")
		}
		emitRemoveWrapperTool(&buf, &next, env, old, stderr)
		fmt.Fprintf(&buf, "export SCIGET_SESSION=%s\n", shellSingleQuote(marshalSessionState(next)))
		fmt.Fprintf(&buf, "export SCIGET_PROMPT=%s\n", shellSingleQuote(formatSessionPrompt(next)))
		fmt.Fprintf(&buf, "printf '%%s\\n' %s >&2\n", shellSingleQuote("sciget: removed "+printable(old.Name)))
		if hasModule {
			buf.WriteString("fi\n")
		}
	}

	buf.WriteString("hash -r 2>/dev/null || true\n")
	buf.WriteString("if [ \"$__sciget_plan_rc\" -ne 0 ]; then unset __sciget_plan_rc; false; else unset __sciget_plan_rc; true; fi\n")
	return buf.String(), next, nil
}

// shellSpec identifies which interactive host shell sciget will launch.
type shellSpec struct {
	kind     string // shellKindBash, shellKindZsh, or shellKindSh
	path     string // resolved executable path
	fallback string // non-empty explanation when $SHELL was unsupported
}

func resolveHostShell(shellEnv string) (shellSpec, error) {
	requested := strings.TrimSpace(shellEnv)
	base := filepath.Base(requested)
	if requested != "" {
		switch base {
		case shellKindBash:
			if p, err := exec.LookPath(requested); err == nil {
				return shellSpec{kind: shellKindBash, path: p}, nil
			}
		case shellKindZsh:
			if p, err := exec.LookPath(requested); err == nil {
				return shellSpec{kind: shellKindZsh, path: p}, nil
			}
		case shellKindSh, "dash", "ksh", "mksh":
			if p, err := exec.LookPath(requested); err == nil {
				return shellSpec{kind: shellKindSh, path: p}, nil
			}
		}
	}
	reason := ""
	if requested != "" {
		reason = fmt.Sprintf("shell %q is not directly supported for sessions", base)
	}
	if p, err := exec.LookPath("bash"); err == nil {
		return shellSpec{kind: shellKindBash, path: p, fallback: reason}, nil
	}
	if p, err := exec.LookPath("sh"); err == nil {
		return shellSpec{kind: shellKindSh, path: p, fallback: reason}, nil
	}
	return shellSpec{}, errors.New("no supported shell (bash or sh) found on PATH")
}

// shellFunctionScript defines POSIX PATH helpers, the Lmod initializer, and
// the sciget() wrapper function shared by bash, zsh, and POSIX sh sessions.
// Note: __sciget_path_remove intentionally trims empty leading/trailing PATH
// segments (implicit current-directory lookup) while keeping all valid dirs.
const shellFunctionScript = `unalias sciget 2>/dev/null || true
__sciget_path_remove() {
  __sciget_new=""
  __sciget_rem="${PATH-}"
  while [ -n "$__sciget_rem" ]; do
    case "$__sciget_rem" in
    *:*) __sciget_part=${__sciget_rem%%:*}; __sciget_rem=${__sciget_rem#*:} ;;
    *)   __sciget_part=$__sciget_rem; __sciget_rem="" ;;
    esac
    if [ "$__sciget_part" != "$1" ]; then
      __sciget_new="${__sciget_new:+$__sciget_new:}$__sciget_part"
    fi
  done
  PATH=$__sciget_new
  export PATH
  unset __sciget_new __sciget_rem __sciget_part
}
__sciget_path_prepend() {
  __sciget_path_remove "$1"
  PATH="$1${PATH:+:$PATH}"
  export PATH
}
__sciget_init_lmod() {
  if type module >/dev/null 2>&1; then
    return 0
  fi
  if [ -n "${LMOD_PKG-}" ]; then
    for __sciget_init in "$LMOD_PKG/init/${__sciget_shell_kind:-sh}" "$LMOD_PKG/init/sh"; do
      if [ -r "$__sciget_init" ]; then
        . "$__sciget_init" 2>/dev/null || true
        break
      fi
    done
    unset __sciget_init
  fi
  if ! type module >/dev/null 2>&1 && [ -n "${LMOD_CMD-}" ] && [ -x "${LMOD_CMD}" ]; then
    module() {
      __sciget_lmod_out=$("$LMOD_CMD" "${__sciget_shell_kind:-sh}" "$@") || {
        set -- $?
        unset __sciget_lmod_out
        return "$1"
      }
      eval "$__sciget_lmod_out"
      set -- $?
      unset __sciget_lmod_out
      return "$1"
    }
  fi
  type module >/dev/null 2>&1
}
__sciget_update_sh_prompt() {
  [ "${SCIGET_DISABLE_PROMPT-}" = "1" ] && return 0
  if [ -n "${SCIGET_PROMPT-}" ]; then
    PS1="(sciget ${SCIGET_PROMPT}) ${__sciget_base_ps1-\$ }"
  else
    PS1="${__sciget_base_ps1-\$ }"
  fi
}
__sciget_is_help() {
  for __sciget_arg in "$@"; do
    case "$__sciget_arg" in
    -h|--help) unset __sciget_arg; return 0 ;;
    --) break ;;
    esac
  done
  unset __sciget_arg
  return 1
}
__sciget_apply_eval() {
  __sciget_code=$(command "$SCIGET_SELF" __session-eval "$@") || {
    set -- $?
    unset __sciget_code
    return "$1"
  }
  __sciget_nl='
'
  case "$__sciget_code" in
  "# SCIGET_SESSION_EVAL_V1${__sciget_nl}"*)
    unset __sciget_nl
    eval "$__sciget_code"
    set -- $?
    unset __sciget_code
    if [ "${__sciget_shell_kind-}" = "sh" ]; then __sciget_update_sh_prompt; fi
    return "$1"
    ;;
  "")
    unset __sciget_code __sciget_nl
    return 0
    ;;
  *)
    unset __sciget_code __sciget_nl
    printf '%s\n' 'sciget: unexpected output from session planner' >&2
    return 1
    ;;
  esac
}
sciget() {
  case "${1-}" in
  add)
    shift
    if __sciget_is_help "$@"; then
      command "$SCIGET_SELF" add "$@"
      return $?
    fi
    __sciget_apply_eval add "$@"
    return $?
    ;;
  session)
    case "${2-}" in
    add|remove)
      __sciget_sub=$2
      shift 2
      if __sciget_is_help "$@"; then
        command "$SCIGET_SELF" session "$__sciget_sub" "$@"
        set -- $?
        unset __sciget_sub
        return "$1"
      fi
      __sciget_apply_eval "$__sciget_sub" "$@"
      set -- $?
      unset __sciget_sub
      return "$1"
      ;;
    esac
    command "$SCIGET_SELF" "$@"
    ;;
  *)
    command "$SCIGET_SELF" "$@"
    ;;
  esac
}
`

const bashPromptScript = `__sciget_prompt_hook() {
  [ "${SCIGET_DISABLE_PROMPT-}" = "1" ] && return 0
  case "$PS1" in
    *'${SCIGET_PROMPT:'*) ;;
    *) PS1='${SCIGET_PROMPT:+(sciget $SCIGET_PROMPT) }'"$PS1" ;;
  esac
}
__sciget_prompt_hook
if declare -p PROMPT_COMMAND 2>/dev/null | grep -q 'declare -a'; then
  PROMPT_COMMAND+=(__sciget_prompt_hook)
elif [ -n "${PROMPT_COMMAND-}" ]; then
  PROMPT_COMMAND="${PROMPT_COMMAND}; __sciget_prompt_hook"
else
  PROMPT_COMMAND=__sciget_prompt_hook
fi
`

const zshPromptScript = `__sciget_last_prefix=""
__sciget_precmd() {
  if [ -n "$__sciget_last_prefix" ]; then
    PS1="${PS1#"$__sciget_last_prefix"}"
    __sciget_last_prefix=""
  fi
  [ "${SCIGET_DISABLE_PROMPT-}" = "1" ] && return 0
  if [ -n "${SCIGET_PROMPT-}" ]; then
    __sciget_last_prefix="(sciget ${SCIGET_PROMPT//\%/%%}) "
    PS1="${__sciget_last_prefix}${PS1}"
  fi
}
typeset -a precmd_functions
precmd_functions+=(__sciget_precmd)
__sciget_precmd
`

const shPromptScript = `__sciget_base_ps1="${PS1-\$ }"
__sciget_update_sh_prompt
`

func sessionBanner(st sessionState, nested bool) string {
	prefix := "sciget session"
	if nested {
		prefix = "sciget session (nested)"
	}
	if len(st.Tools) == 0 {
		return prefix + " · add tools: sciget add <tool/version> · exit to return"
	}
	parts := make([]string, len(st.Tools))
	for i, t := range st.Tools {
		parts[i] = printable(t.Name) + " (" + t.Mode + ")"
	}
	return prefix + ": " + strings.Join(parts, ", ") + " · add more: sciget add <tool/version> · exit to return"
}

// expandSimpleEnvPath expands leading "~/" and "$HOME" / "${HOME}" in a POSIX
// ENV path without running eval.
func expandSimpleEnvPath(raw string) string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return raw
	}
	if rest, ok := strings.CutPrefix(raw, "~/"); ok {
		return filepath.Join(home, rest)
	}
	if raw == "~" {
		return home
	}
	r := strings.NewReplacer("${HOME}", home, "$HOME", home)
	return r.Replace(raw)
}

// renderShellRC generates the interactive startup script content for spec.
func renderShellRC(spec shellSpec, tmpDir, selfPath, initCode string, finalState sessionState, nested bool) (rcContent, zshenvContent string) {
	banner := sessionBanner(finalState, nested)
	readySnippet := "if [ -n \"${SCIGET_SHELL_READY-}\" ]; then : > \"$SCIGET_SHELL_READY\" 2>/dev/null || true; unset SCIGET_SHELL_READY; fi\n"

	var body strings.Builder
	// Delete the temp startup directory first so even if the user's rcfile
	// execs another program (e.g. tmux), no temp files leak.
	fmt.Fprintf(&body, "rm -rf -- %s 2>/dev/null || true\n", shellSingleQuote(tmpDir))

	switch spec.kind {
	case shellKindBash:
		// Interactive bash already reads /etc/bash.bashrc (when built with
		// SYS_BASHRC) before --rcfile, so only source ~/.bashrc here.
		body.WriteString("[ -f \"$HOME/.bashrc\" ] && . \"$HOME/.bashrc\"\n")
	case shellKindZsh:
		// Restore ZDOTDIR from the post-.zshenv snapshot recorded by .zshenv so
		// an XDG ~/.zshenv that sets ZDOTDIR keeps working.
		body.WriteString("if [ -n \"${__sciget_had_zdot-}\" ]; then\n")
		body.WriteString("  export ZDOTDIR=\"${__sciget_zdot-}\"\n")
		body.WriteString("else\n")
		body.WriteString("  unset ZDOTDIR\n")
		body.WriteString("fi\n")
		body.WriteString("unset __sciget_had_zdot __sciget_zdot\n")
		body.WriteString("[ -f \"${ZDOTDIR:-$HOME}/.zshrc\" ] && . \"${ZDOTDIR:-$HOME}/.zshrc\"\n")
	case shellKindSh:
		origEnv, hadEnv := os.LookupEnv("ENV")
		if hadEnv {
			fmt.Fprintf(&body, "export ENV=%s\n", shellSingleQuote(origEnv))
			expanded := expandSimpleEnvPath(origEnv)
			fmt.Fprintf(&body, "[ -f %s ] && . %s\n", shellSingleQuote(expanded), shellSingleQuote(expanded))
		} else {
			body.WriteString("unset ENV\n")
		}
	}

	fmt.Fprintf(&body, "__sciget_shell_kind=%s\n", shellSingleQuote(spec.kind))
	fmt.Fprintf(&body, "export SCIGET_SELF=%s\n", shellSingleQuote(selfPath))
	body.WriteString(shellFunctionScript)
	body.WriteString("__sciget_init_ok=1\n")
	if initCode != "" {
		body.WriteString("{\n")
		body.WriteString(initCode)
		body.WriteString("} || __sciget_init_ok=0\n")
	} else {
		fmt.Fprintf(&body, "export SCIGET_SESSION=%s\n", shellSingleQuote(marshalSessionState(finalState)))
		fmt.Fprintf(&body, "export SCIGET_PROMPT=%s\n", shellSingleQuote(formatSessionPrompt(finalState)))
	}
	switch spec.kind {
	case shellKindBash:
		body.WriteString(bashPromptScript)
	case shellKindZsh:
		body.WriteString(zshPromptScript)
	default:
		body.WriteString(shPromptScript)
	}
	// Print the banner only from verified activation status so a failed module
	// load is never announced as loaded.
	body.WriteString("if [ \"$__sciget_init_ok\" -eq 1 ]; then\n")
	fmt.Fprintf(&body, "  printf '%%s\\n' %s >&2\n", shellSingleQuote(banner))
	body.WriteString("else\n")
	body.WriteString("  if [ -n \"${SCIGET_PROMPT-}\" ]; then\n")
	body.WriteString("    printf 'sciget session (partial: %s) · add more: sciget add <tool/version> · exit to return\\n' \"$SCIGET_PROMPT\" >&2\n")
	body.WriteString("  else\n")
	body.WriteString("    printf 'sciget session (some tools failed to load) · add tools: sciget add <tool/version> · exit to return\\n' >&2\n")
	body.WriteString("  fi\n")
	body.WriteString("fi\n")
	body.WriteString("unset __sciget_init_ok\n")
	body.WriteString(readySnippet)

	if spec.kind == shellKindZsh {
		var zshenv strings.Builder
		origZdot, hadZdot := os.LookupEnv("ZDOTDIR")
		if hadZdot {
			fmt.Fprintf(&zshenv, "export ZDOTDIR=%s\n", shellSingleQuote(origZdot))
			fmt.Fprintf(&zshenv, "[ -f %s ] && . %s\n",
				shellSingleQuote(filepath.Join(origZdot, ".zshenv")),
				shellSingleQuote(filepath.Join(origZdot, ".zshenv")))
		} else {
			zshenv.WriteString("unset ZDOTDIR\n")
			zshenv.WriteString("[ -f \"$HOME/.zshenv\" ] && . \"$HOME/.zshenv\"\n")
		}
		// Record effective ZDOTDIR after the user's .zshenv ran, then redirect
		// zsh to tmpDir so our .zshrc runs next.
		zshenv.WriteString("__sciget_had_zdot=\"${ZDOTDIR+1}\"\n")
		zshenv.WriteString("__sciget_zdot=\"${ZDOTDIR-}\"\n")
		fmt.Fprintf(&zshenv, "export ZDOTDIR=%s\n", shellSingleQuote(tmpDir))
		zshenvContent = zshenv.String()
	}
	return body.String(), zshenvContent
}

// writeShellStartupFiles creates a private temporary startup directory for
// spec and returns the argv and environment overrides needed to launch it.
func writeShellStartupFiles(spec shellSpec, selfPath, initCode string, finalState sessionState, nested bool) (tmpDir string, argv []string, extraEnv []string, err error) {
	tmpDir, err = os.MkdirTemp("", "sciget-shell-*")
	if err != nil {
		return "", nil, nil, fmt.Errorf("creating shell startup folder: %w", err)
	}
	if err := os.Chmod(tmpDir, 0o700); err != nil {
		os.RemoveAll(tmpDir)
		return "", nil, nil, err
	}

	rcContent, zshenvContent := renderShellRC(spec, tmpDir, selfPath, initCode, finalState, nested)

	switch spec.kind {
	case shellKindBash:
		rc := filepath.Join(tmpDir, "rc")
		if err := os.WriteFile(rc, []byte(rcContent), 0o600); err != nil {
			os.RemoveAll(tmpDir)
			return "", nil, nil, err
		}
		return tmpDir, []string{spec.path, "--rcfile", rc, "-i"}, nil, nil
	case shellKindZsh:
		if err := os.WriteFile(filepath.Join(tmpDir, ".zshenv"), []byte(zshenvContent), 0o600); err != nil {
			os.RemoveAll(tmpDir)
			return "", nil, nil, err
		}
		if err := os.WriteFile(filepath.Join(tmpDir, ".zshrc"), []byte(rcContent), 0o600); err != nil {
			os.RemoveAll(tmpDir)
			return "", nil, nil, err
		}
		return tmpDir, []string{spec.path, "-i"}, []string{"ZDOTDIR=" + tmpDir}, nil
	default:
		rc := filepath.Join(tmpDir, "rc")
		if err := os.WriteFile(rc, []byte(rcContent), 0o600); err != nil {
			os.RemoveAll(tmpDir)
			return "", nil, nil, err
		}
		return tmpDir, []string{spec.path, "-i"}, []string{"ENV=" + rc}, nil
	}
}

func mergeEnviron(base, overrides []string) []string {
	if len(overrides) == 0 {
		return base
	}
	overrideKeys := map[string]bool{}
	for _, kv := range overrides {
		if k, _, ok := strings.Cut(kv, "="); ok {
			overrideKeys[k] = true
		}
	}
	out := make([]string, 0, len(base)+len(overrides))
	for _, kv := range base {
		if k, _, ok := strings.Cut(kv, "="); ok && overrideKeys[k] {
			continue
		}
		out = append(out, kv)
	}
	return append(out, overrides...)
}

func selfExecutable() string {
	if p, err := os.Executable(); err == nil && p != "" {
		return p
	}
	if len(os.Args) > 0 && os.Args[0] != "" {
		return os.Args[0]
	}
	return "sciget"
}

// minArgs requires at least n positional arguments, returning usageError.
func minArgs(n int) cobra.PositionalArgs {
	return func(cmd *cobra.Command, args []string) error {
		if len(args) < n {
			return usageError{fmt.Sprintf("requires at least %d arg(s), only received %d", n, len(args))}
		}
		return nil
	}
}

func newShellCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "shell [tool/version...]",
		Short: "Start a host shell session with one or more tools loaded",
		ValidArgsFunction: func(cmd *cobra.Command, args []string, toComplete string) ([]string, cobra.ShellCompDirective) {
			return completeContainerNames(cmd, nil, toComplete)
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			stderr := cmd.ErrOrStderr()
			var cs []container
			if len(args) > 0 {
				var err error
				cs, err = lookupContainers(args)
				if err != nil {
					return err
				}
			}
			cfg, err := readConfig()
			if err != nil {
				return err
			}
			ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
			defer stop()

			interactive := isTerminal(os.Stdin) && isTerminal(stderr)
			prepared := make([]preparedTool, 0, len(cs))
			for _, c := range cs {
				p, err := prepareSessionTool(ctx, cfg, c, os.Stdin, stderr, interactive)
				if err != nil {
					return err
				}
				prepared = append(prepared, p)
			}

			spec, err := resolveHostShell(os.Getenv("SHELL"))
			if err != nil {
				return err
			}
			if spec.fallback != "" {
				fmt.Fprintf(stderr, "sciget: %s; starting %s\n", spec.fallback, spec.path)
			}

			rawState, inSession := os.LookupEnv("SCIGET_SESSION")
			curState := parseSessionState(rawState)
			nested := inSession && rawState != ""
			env := newEnvSnapshot(os.Environ())
			initCode, finalState := planSessionAdd(curState, env, prepared, cfg, stderr, false)

			tmpDir, argv, extraEnv, err := writeShellStartupFiles(spec, selfExecutable(), initCode, finalState, nested)
			if err != nil {
				return err
			}
			if err := runShell(spec.path, argv, mergeEnviron(os.Environ(), extraEnv)); err != nil {
				os.RemoveAll(tmpDir)
				return err
			}
			return nil
		},
	}
}

func newAddCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "add <tool/version>...",
		Short: "Add one or more tools to the active sciget shell session",
		Args:  minArgs(1),
		ValidArgsFunction: func(cmd *cobra.Command, args []string, toComplete string) ([]string, cobra.ShellCompDirective) {
			return completeContainerNames(cmd, nil, toComplete)
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			return outsideSessionError("add", args)
		},
	}
}

func outsideSessionError(action string, args []string) error {
	if os.Getenv("SCIGET_SESSION") != "" {
		return fmt.Errorf("sciget %s needs the session's sciget shell function; check `type sciget` or start a new session with `sciget shell`", action)
	}
	if action == "add" && len(args) > 0 {
		return fmt.Errorf("sciget add works inside a sciget shell; start one with: %s",
			shellJoin(append([]string{"sciget", "shell"}, args...)))
	}
	return fmt.Errorf("sciget %s works inside a sciget shell; start one with: sciget shell [tool/version...]", action)
}

func newSessionCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "session",
		Short: "Inspect or modify the active sciget shell session",
		Args:  noArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runSessionList(cmd.OutOrStdout())
		},
	}
	cmd.AddCommand(&cobra.Command{
		Use:   "list",
		Short: "List tools loaded in the active sciget session",
		Args:  noArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runSessionList(cmd.OutOrStdout())
		},
	})
	cmd.AddCommand(&cobra.Command{
		Use:   "add <tool/version>...",
		Short: "Add one or more tools to the active sciget session",
		Args:  minArgs(1),
		ValidArgsFunction: func(cmd *cobra.Command, args []string, toComplete string) ([]string, cobra.ShellCompDirective) {
			return completeContainerNames(cmd, nil, toComplete)
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			return outsideSessionError("add", args)
		},
	})
	cmd.AddCommand(&cobra.Command{
		Use:               "remove <tool|tool/version>...",
		Short:             "Remove one or more tools from the active sciget session",
		Args:              minArgs(1),
		ValidArgsFunction: completeSessionTools,
		RunE: func(cmd *cobra.Command, args []string) error {
			return outsideSessionError("session remove", args)
		},
	})
	return cmd
}

func completeSessionTools(cmd *cobra.Command, args []string, toComplete string) ([]string, cobra.ShellCompDirective) {
	st := parseSessionState(os.Getenv("SCIGET_SESSION"))
	var out []string
	for _, t := range st.Tools {
		if strings.HasPrefix(t.Name, toComplete) {
			out = append(out, t.Name)
		}
	}
	return out, cobra.ShellCompDirectiveNoFileComp
}

func runSessionList(out io.Writer) error {
	raw, ok := os.LookupEnv("SCIGET_SESSION")
	if !ok || raw == "" {
		_, err := fmt.Fprintln(out, "Not in a sciget session. Start one with: sciget shell [tool/version...]")
		return err
	}
	st := parseSessionState(raw)
	if len(st.Tools) == 0 {
		_, err := fmt.Fprintln(out, "No tools loaded in this session. Add one with: sciget add <tool/version>")
		return err
	}
	cfg, _ := readConfig()
	loadedModules := map[string]bool{}
	for _, mod := range strings.Split(os.Getenv("LOADEDMODULES"), ":") {
		if mod != "" {
			loadedModules[mod] = true
		}
	}
	var rows strings.Builder
	for _, t := range st.Tools {
		status := "active"
		if t.Mode == sessionModeModule && os.Getenv("LOADEDMODULES") != "" && !loadedModules[t.Name] {
			status = "unloaded (module)"
		}
		cmds := sessionToolCommands(t, cfg)
		detail := fmt.Sprintf("%d commands", len(cmds))
		if len(cmds) == 1 {
			detail = "1 command"
		} else if len(cmds) == 0 && t.Mode == sessionModeModule {
			detail = "Lmod module"
		}
		fmt.Fprintf(&rows, "%s\t%s\t%s\t%s\n", printable(t.Name), t.Mode, detail, status)
	}
	return writeTable(out, isTerminal(out), "TOOL\tMODE\tDETAILS\tSTATUS\n", rows.String())
}

// newSessionEvalCommand is the internal planner invoked by the session's
// sciget() shell function. Stdout carries only POSIX shell statements prefixed
// with sessionEvalHeader; help, prompts, warnings and errors go to stderr.
func newSessionEvalCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:    "__session-eval",
		Hidden: true,
		Args:   noArgs,
	}
	// Never let Cobra print help text onto stdout, which the shell function
	// captures for eval.
	cmd.SetHelpFunc(func(c *cobra.Command, _ []string) {
		fmt.Fprint(c.ErrOrStderr(), c.UsageString())
	})
	addCmd := &cobra.Command{
		Use:  "add <tool/version>...",
		Args: minArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			stderr := cmd.ErrOrStderr()
			cs, err := lookupContainers(args)
			if err != nil {
				return err
			}
			cfg, err := readConfig()
			if err != nil {
				return err
			}
			ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
			defer stop()

			interactive := isTerminal(os.Stdin) && isTerminal(stderr)
			prepared := make([]preparedTool, 0, len(cs))
			for _, c := range cs {
				p, err := prepareSessionTool(ctx, cfg, c, os.Stdin, stderr, interactive)
				if err != nil {
					return err
				}
				prepared = append(prepared, p)
			}
			curState := parseSessionState(os.Getenv("SCIGET_SESSION"))
			env := newEnvSnapshot(os.Environ())
			code, _ := planSessionAdd(curState, env, prepared, cfg, stderr, true)
			if code == "" {
				return nil
			}
			_, err = io.WriteString(cmd.OutOrStdout(), sessionEvalHeader+code)
			return err
		},
	}
	removeCmd := &cobra.Command{
		Use:  "remove <tool|tool/version>...",
		Args: minArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			curState := parseSessionState(os.Getenv("SCIGET_SESSION"))
			env := newEnvSnapshot(os.Environ())
			code, _, err := planSessionRemove(curState, env, args, cmd.ErrOrStderr())
			if err != nil {
				return err
			}
			if code == "" {
				return nil
			}
			_, err = io.WriteString(cmd.OutOrStdout(), sessionEvalHeader+code)
			return err
		},
	}
	cmd.AddCommand(addCmd, removeCmd)
	return cmd
}
