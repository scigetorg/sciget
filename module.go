package main

import (
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
	if err := os.RemoveAll(filepath.Dir(wrapperDir)); err != nil {
		return moduleInfo{}, fmt.Errorf("writing the module for %s: %w", c.name(), err)
	}
	if err := os.MkdirAll(wrapperDir, 0o755); err != nil {
		return moduleInfo{}, fmt.Errorf("writing the module for %s: %w", c.name(), err)
	}
	for _, name := range names {
		script := "#!/bin/sh\nexec " + shellJoin([]string{sciget, "apptainer", "exec", "--", c.name(), name}) + ` "$@"` + "\n"
		if err := os.WriteFile(filepath.Join(wrapperDir, name), []byte(script), 0o755); err != nil {
			return moduleInfo{}, fmt.Errorf("writing the module for %s: %w", c.name(), err)
		}
	}

	var b strings.Builder
	b.WriteString("-- Written by sciget install, which rewrites it.\n")
	fmt.Fprintf(&b, "help(%s)\n", luaString(list.Help))
	fmt.Fprintf(&b, "whatis(%s)\n", luaString("Container: "+c.fileName()))
	fmt.Fprintf(&b, "whatis(%s)\n", luaString("Commands: "+strings.Join(names, " ")))
	fmt.Fprintf(&b, "prepend_path(\"PATH\", %s)\n", luaString(wrapperDir))
	for _, setting := range list.Env {
		name, value, _ := strings.Cut(setting, "=")
		if checkEnvName(name) != nil {
			info.skipped = append(info.skipped, fmt.Sprintf("setting %q: not a variable name", printable(name)))
			continue
		}
		// BASEPATH stands for the container's root, so without it the value
		// is the path inside the container, which its commands see.
		value = strings.ReplaceAll(value, "BASEPATH", "")
		fmt.Fprintf(&b, "setenv(%s, %s)\n", luaString(name), luaString(value))
	}

	if err := writeFileAtomic(path, b.String()); err != nil {
		return moduleInfo{}, fmt.Errorf("writing the module for %s: %w", c.name(), err)
	}
	return info, nil
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
