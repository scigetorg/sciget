package main

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

// moduleCommands returns the names c's module wraps, sorted: the list's
// Found names not in excludes, and its Kept names. It also returns the names
// skipped because validCommandName refuses them.
func moduleCommands(list commandList, excludes []string) (names, skipped []string) {
	candidates := slices.Clone(list.Kept)
	for _, name := range list.Found {
		if !slices.Contains(excludes, name) {
			candidates = append(candidates, name)
		}
	}
	for _, name := range candidates {
		if validCommandName(name) {
			names = append(names, name)
		} else {
			skipped = append(skipped, name)
		}
	}
	slices.Sort(names)
	slices.Sort(skipped)
	return slices.Compact(names), slices.Compact(skipped)
}

// validCommandName reports whether name may become a wrapper: letters,
// digits, ".", "_", "+" and "-", not starting with "." or "-". A wrapper's
// name is a file name and goes into a shell script and a modulefile.
func validCommandName(name string) bool {
	if name == "" || name[0] == '.' || name[0] == '-' {
		return false
	}
	for _, r := range name {
		if !('a' <= r && r <= 'z' || 'A' <= r && r <= 'Z' || '0' <= r && r <= '9' || strings.ContainsRune("._+-", r)) {
			return false
		}
	}
	return true
}

// moduleInfo is what writeModule reports: the commands wrapped and anything
// it skipped.
type moduleInfo struct {
	commands []string
	skipped  []string // command names and settings left out, each described
}

// envSetting is a single validated host environment variable from a
// container's DEPLOY_ENV_ metadata.
type envSetting struct {
	name  string
	value string
}

// maxHostEnvValue caps single DEPLOY_ENV_ values exported onto the host so
// container metadata cannot bloat SCIGET_SESSION past Linux's MAX_ARG_STRLEN.
const maxHostEnvValue = 4096

// deniedHostEnvNames are uppercase host environment variable names that
// container DEPLOY_ENV_ metadata must never overwrite on the host (checked
// case-insensitively so zsh lowercase tied arrays like path/fpath are also
// blocked).
var deniedHostEnvNames = map[string]bool{
	"PATH": true, "CDPATH": true, "FPATH": true, "MANPATH": true,
	"MODULEPATH": true, "MODULE_PATH": true, "PSVAR": true, "WATCH": true,
	"PS0": true, "PS1": true, "PS2": true, "PS3": true, "PS4": true,
	"RPS1": true, "RPS2": true, "PROMPT": true, "PROMPT2": true,
	"PROMPT3": true, "PROMPT4": true, "RPROMPT": true, "RPROMPT2": true,
	"SPROMPT": true, "PROMPT_COMMAND": true,
	"ENV": true, "BASH_ENV": true, "ZDOTDIR": true, "IFS": true,
	"SHELL": true, "HOME": true, "USER": true, "LOGNAME": true,
	"TERM": true, "INPUTRC": true, "HISTFILE": true, "HISTCONTROL": true,
	"HISTIGNORE": true, "FCEDIT": true, "TMOUT": true,
	"PAGER": true, "MANPAGER": true, "EDITOR": true, "VISUAL": true,
	"GCONV_PATH": true, "LOCPATH": true, "HOSTALIASES": true, "RES_OPTIONS": true,
	"MAIL": true, "MAILPATH": true, "GLOBIGNORE": true, "EXECIGNORE": true, "FIGNORE": true,
	"SSL_CERT_FILE": true, "REQUESTS_CA_BUNDLE": true, "GNUPGHOME": true, "MAKEFLAGS": true,
	"HTTP_PROXY": true, "HTTPS_PROXY": true, "ALL_PROXY": true, "NO_PROXY": true,
	"TMPDIR": true, "TMP": true, "TEMP": true, "TEMPDIR": true,
	"DISPLAY": true, "XAUTHORITY": true, "PWD": true, "OLDPWD": true, "SHLVL": true,
}

// isDeniedHostEnv reports whether name is a reserved or execution-sensitive
// host variable that container DEPLOY_ENV_ settings must not mutate.
func isDeniedHostEnv(name string) bool {
	upper := strings.ToUpper(name)
	if deniedHostEnvNames[upper] {
		return true
	}
	for _, prefix := range []string{
		"LD_", "DYLD_", "LMOD_", "SCIGET_", "__SCIGET_", "_SCIGET",
		"APPTAINER", "SINGULARITY", "BASH_", "GIT_", "SSH_", "LESS",
		"PYTHON", "PERL", "RUBY", "NODE_", "JAVA_", "_JAVA_", "ZDOT",
		"XDG_", "PIP_", "NPM_CONFIG_",
	} {
		if strings.HasPrefix(upper, prefix) {
			return true
		}
	}
	for _, suffix := range []string{"_OPTIONS", "_PRELOAD", "_ASKPASS", "_EDITOR", "_PAGER"} {
		if strings.HasSuffix(upper, suffix) {
			return true
		}
	}
	return false
}

// moduleEnv parses and validates list.Env into safe host env settings (with
// BASEPATH stripped as in writeModule) and describes any skipped entries.
func moduleEnv(list commandList) (settings []envSetting, skipped []string) {
	for _, setting := range list.Env {
		name, value, _ := strings.Cut(setting, "=")
		if checkEnvName(name) != nil {
			skipped = append(skipped, fmt.Sprintf("setting %q: not a variable name", printable(name)))
			continue
		}
		if isDeniedHostEnv(name) {
			skipped = append(skipped, fmt.Sprintf("setting %q: reserved environment variable", printable(name)))
			continue
		}
		// BASEPATH stands for the container's root, so without it the value
		// is the path inside the container, which its commands see.
		value = strings.ReplaceAll(value, "BASEPATH", "")
		if len(value) > maxHostEnvValue {
			skipped = append(skipped, fmt.Sprintf("setting %q: value too large for host environment", printable(name)))
			continue
		}
		settings = append(settings, envSetting{name: name, value: value})
	}
	return settings, skipped
}

// writeWrapperFiles writes one POSIX wrapper script per command in names into
// dir. When useSelfEnv is true, wrappers prefer $SCIGET_SELF (set by sciget
// shell) and fall back to sciget, so a non-PATH binary works in sessions.
func writeWrapperFiles(dir, sciget string, c container, names []string, useSelfEnv bool) error {
	for _, name := range names {
		var script string
		if useSelfEnv {
			script = "#!/bin/sh\n_sciget=${SCIGET_SELF}\n[ -n \"$_sciget\" ] || _sciget=" +
				shellSingleQuote(sciget) + "\nexec \"$_sciget\" " +
				shellJoin([]string{"apptainer", "exec", "--", c.name(), name}) + ` "$@"` + "\n"
		} else {
			script = "#!/bin/sh\nexec " + shellJoin([]string{sciget, "apptainer", "exec", "--", c.name(), name}) + ` "$@"` + "\n"
		}
		if err := os.WriteFile(filepath.Join(dir, name), []byte(script), 0o755); err != nil {
			return err
		}
	}
	return nil
}

// shellSingleQuote always wraps s in POSIX single quotes, escaping embedded
// single quotes. Unlike shellJoin, the result is unconditionally quoted, safe
// for generated shell code and assignments.
func shellSingleQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// writeModule writes c's module, <module_dir>/<catalog>/<tool>/<version>.lua,
// and a wrapper for each command in the build's own folder,
// <module_dir>/.wrappers/<catalog>/<tool>/<version>/<date>/. It first
// deletes the folders of every build of that version, so no earlier wrapper
// is left.
func writeModule(cfg config, c container, list commandList) (moduleInfo, error) {
	moduleDir, _, err := cfg.get("module_dir")
	if err != nil {
		return moduleInfo{}, err
	}
	sciget, _, err := cfg.get("module_sciget")
	if err != nil {
		return moduleInfo{}, err
	}
	excludes, err := cfg.getList("finder_excludes")
	if err != nil {
		return moduleInfo{}, err
	}
	for _, value := range []string{c.catalog, c.tool, c.version, c.buildDate} {
		if unsafeLayoutValue(value) {
			return moduleInfo{}, fmt.Errorf("catalog entry %q has an unsafe module path value %q", c.name(), value)
		}
	}
	var info moduleInfo
	names, skipped := moduleCommands(list, excludes)
	info.commands = names
	for _, name := range skipped {
		info.skipped = append(info.skipped, fmt.Sprintf("command %q: not a safe command name", printable(name)))
	}

	path, wrapperDir := modulePaths(moduleDir, c)
	if strings.ContainsRune(wrapperDir, os.PathListSeparator) {
		return moduleInfo{}, fmt.Errorf("writing the module for %s: wrapper path %q contains %q", c.name(), wrapperDir, string(os.PathListSeparator))
	}
	if err := os.RemoveAll(filepath.Dir(wrapperDir)); err != nil {
		return moduleInfo{}, fmt.Errorf("writing the module for %s: %w", c.name(), err)
	}
	if err := os.MkdirAll(wrapperDir, 0o755); err != nil {
		return moduleInfo{}, fmt.Errorf("writing the module for %s: %w", c.name(), err)
	}
	if err := writeWrapperFiles(wrapperDir, sciget, c, names, false); err != nil {
		return moduleInfo{}, fmt.Errorf("writing the module for %s: %w", c.name(), err)
	}

	settings, envSkipped := moduleEnv(list)
	info.skipped = append(info.skipped, envSkipped...)

	var b strings.Builder
	b.WriteString("-- Written by sciget install, which rewrites it.\n")
	fmt.Fprintf(&b, "help(%s)\n", luaString(list.Help))
	fmt.Fprintf(&b, "whatis(%s)\n", luaString("Container: "+c.fileName()))
	fmt.Fprintf(&b, "whatis(%s)\n", luaString("Commands: "+strings.Join(names, " ")))
	fmt.Fprintf(&b, "prepend_path(\"PATH\", %s)\n", luaString(wrapperDir))
	for _, s := range settings {
		fmt.Fprintf(&b, "setenv(%s, %s)\n", luaString(s.name), luaString(s.value))
	}

	if err := writeFileAtomic(path, b.String()); err != nil {
		return moduleInfo{}, fmt.Errorf("writing the module for %s: %w", c.name(), err)
	}
	return info, nil
}

// ensureSessionWrappers creates (or reuses) an immutable wrapper directory
// under <cache_dir>/wrappers/ for c at buildDate, publishing it atomically so
// concurrent sessions never race or disturb user module_dir. Path components
// are sanitized and hashed so catalog values cannot inject PATH separators.
func ensureSessionWrappers(cfg config, c container, buildDate string, list commandList) (string, moduleInfo, []envSetting, error) {
	cacheDir, _, err := cfg.get("cache_dir")
	if err != nil {
		return "", moduleInfo{}, nil, err
	}
	if strings.ContainsRune(cacheDir, os.PathListSeparator) {
		return "", moduleInfo{}, nil, fmt.Errorf("preparing wrappers for %s: cache_dir %q contains %q", c.name(), cacheDir, string(os.PathListSeparator))
	}
	sciget, _, err := cfg.get("module_sciget")
	if err != nil {
		return "", moduleInfo{}, nil, err
	}
	excludes, err := cfg.getList("finder_excludes")
	if err != nil {
		return "", moduleInfo{}, nil, err
	}
	if buildDate == "" {
		buildDate = c.buildDate
	}
	for _, value := range []string{c.catalog, c.tool, c.version, buildDate} {
		if unsafeLayoutValue(value) {
			return "", moduleInfo{}, nil, fmt.Errorf("catalog entry %q has an unsafe wrapper path value %q", c.name(), value)
		}
	}
	var info moduleInfo
	names, skipped := moduleCommands(list, excludes)
	info.commands = names
	for _, name := range skipped {
		info.skipped = append(info.skipped, fmt.Sprintf("command %q: not a safe command name", printable(name)))
	}
	settings, envSkipped := moduleEnv(list)
	info.skipped = append(info.skipped, envSkipped...)

	const wrapperTemplateVersion = "v1"
	h := sha256.New()
	h.Write([]byte(wrapperTemplateVersion))
	h.Write([]byte{0})
	h.Write([]byte(sciget))
	h.Write([]byte{0})
	h.Write([]byte(c.catalog))
	h.Write([]byte{0})
	h.Write([]byte(c.name()))
	h.Write([]byte{0})
	h.Write([]byte(buildDate))
	for _, name := range names {
		h.Write([]byte{0})
		h.Write([]byte(name))
	}
	tag := hex.EncodeToString(h.Sum(nil)[:8])
	dirName := fmt.Sprintf("%s_%s_%s_%s_%s",
		safePromptToken(c.catalog), safePromptToken(c.tool), safePromptToken(c.version), safePromptToken(buildDate), tag)
	parent := filepath.Join(cacheDir, "wrappers")
	target := filepath.Join(parent, dirName)
	if strings.ContainsRune(target, os.PathListSeparator) {
		return "", moduleInfo{}, nil, fmt.Errorf("preparing wrappers for %s: wrapper path contains %q", c.name(), string(os.PathListSeparator))
	}
	if st, err := os.Stat(target); err == nil && st.IsDir() {
		return target, info, settings, nil
	}
	if err := os.MkdirAll(parent, 0o755); err != nil {
		return "", moduleInfo{}, nil, fmt.Errorf("preparing wrappers for %s: %w", c.name(), err)
	}
	tmp, err := os.MkdirTemp(parent, ".tmp-*")
	if err != nil {
		return "", moduleInfo{}, nil, fmt.Errorf("preparing wrappers for %s: %w", c.name(), err)
	}
	if err := os.Chmod(tmp, 0o755); err != nil {
		os.RemoveAll(tmp)
		return "", moduleInfo{}, nil, fmt.Errorf("preparing wrappers for %s: %w", c.name(), err)
	}
	if err := writeWrapperFiles(tmp, sciget, c, names, true); err != nil {
		os.RemoveAll(tmp)
		return "", moduleInfo{}, nil, fmt.Errorf("preparing wrappers for %s: %w", c.name(), err)
	}
	if err := os.Rename(tmp, target); err != nil {
		os.RemoveAll(tmp)
		if st, statErr := os.Stat(target); statErr == nil && st.IsDir() {
			return target, info, settings, nil
		}
		return "", moduleInfo{}, nil, fmt.Errorf("preparing wrappers for %s: %w", c.name(), err)
	}
	return target, info, settings, nil
}

// moduleFileSafeForSession checks that an existing sciget-generated .lua
// modulefile does not export any denied host environment variables or a PATH
// entry containing a PATH separator (for example, a legacy module written
// before these checks existed).
func moduleFileSafeForSession(path string) bool {
	data, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if rest, ok := strings.CutPrefix(line, `setenv("`); ok {
			name, _, ok := strings.Cut(rest, `"`)
			if !ok || checkEnvName(name) != nil || isDeniedHostEnv(name) {
				return false
			}
		}
		if rest, ok := strings.CutPrefix(line, `prepend_path("PATH", "`); ok {
			dir, _, ok := strings.Cut(rest, `"`)
			if !ok || strings.ContainsRune(dir, os.PathListSeparator) {
				return false
			}
		}
	}
	return true
}

// moduleState describes the module status for a container in module_dir.
type moduleState struct {
	shareOnly      bool   // on a share and not in the caller's container_dir
	shareName      string // share catalog name when shareOnly
	unsafe         bool   // catalog values cannot form a module path
	exists         bool   // modulefile exists in module_dir
	matchesCatalog bool   // wrapper dir for the catalog's current build exists
	matchesLocated bool   // wrapper dir for the located build on disk exists
	file           string // path to .lua modulefile
	catalogDir     string // <module_dir>/<catalog>
}

// inspectModule reports c's module status using the same rules for the TUI
// and session startup so they cannot disagree.
func inspectModule(cfg config, c container, loc location, locErr error) moduleState {
	var st moduleState
	if locErr == nil && loc.share != "" {
		if path, err := storePath(cfg, c); err != nil || path == "" {
			st.shareOnly = true
			st.shareName = loc.share
			return st
		}
	}
	moduleDir, _, err := cfg.get("module_dir")
	if err != nil || slices.ContainsFunc([]string{c.catalog, c.tool, c.version, c.buildDate}, unsafeLayoutValue) {
		st.unsafe = true
		return st
	}
	file, catalogWrappers := modulePaths(moduleDir, c)
	st.file = file
	st.catalogDir = filepath.Join(moduleDir, c.catalog)
	if _, err := os.Stat(st.file); err != nil {
		return st
	}
	st.exists = true
	if _, err := os.Stat(catalogWrappers); err == nil {
		st.matchesCatalog = true
	}
	locDate := c.buildDate
	if locErr == nil && loc.buildDate != "" {
		locDate = loc.buildDate
	}
	if !unsafeLayoutValue(locDate) {
		located := c
		located.buildDate = locDate
		_, locatedWrappers := modulePaths(moduleDir, located)
		if _, err := os.Stat(locatedWrappers); err == nil {
			st.matchesLocated = true
		}
	}
	return st
}

// modulePaths returns c's modulefile and its build's wrapper folder in
// moduleDir. c's values must be safe path elements.
func modulePaths(moduleDir string, c container) (file, wrappers string) {
	return filepath.Join(moduleDir, c.catalog, c.tool, c.version+".lua"),
		filepath.Join(moduleDir, ".wrappers", c.catalog, c.tool, c.version, c.buildDate)
}

// outdatedModules returns the names of the containers whose module sciget
// wrote from another build than the catalog's.
func outdatedModules(cfg config) ([]string, error) {
	moduleDir, _, err := cfg.get("module_dir")
	if err != nil {
		return nil, err
	}
	containers, err := readContainers()
	if err != nil {
		return nil, err
	}
	var names []string
	for _, c := range containers {
		if c.shadowed || slices.ContainsFunc([]string{c.catalog, c.tool, c.version, c.buildDate}, unsafeLayoutValue) {
			continue
		}
		file, wrappers := modulePaths(moduleDir, c)
		if _, err := os.Stat(file); err != nil {
			continue
		}
		if _, err := os.Stat(wrappers); err != nil {
			names = append(names, c.name())
		}
	}
	return names, nil
}

// writeFileAtomic writes content to a temporary file beside path and renames
// it into place, so a reader sees the old file or the new, never part of one.
func writeFileAtomic(path, content string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".tmp-*")
	if err != nil {
		return err
	}
	_, err = f.WriteString(content)
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	if err == nil {
		err = os.Chmod(f.Name(), 0o644)
	}
	if err == nil {
		err = os.Rename(f.Name(), path)
	}
	if err != nil {
		os.Remove(f.Name())
	}
	return err
}

// luaString returns s as a double-quoted Lua string literal. Backslash and
// quote are escaped, and every control byte is written as a decimal escape,
// so no value can end the string or the line.
func luaString(s string) string {
	var b strings.Builder
	b.WriteByte('"')
	for i := 0; i < len(s); i++ {
		switch c := s[i]; {
		case c == '\\' || c == '"':
			b.WriteByte('\\')
			b.WriteByte(c)
		case c < 0x20 || c == 0x7f:
			fmt.Fprintf(&b, "\\%03d", c)
		default:
			b.WriteByte(c)
		}
	}
	b.WriteByte('"')
	return b.String()
}
