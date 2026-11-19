package main

import (
	"bytes"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
)

// configKey is one setting the config file may hold.
type configKey struct {
	name  string
	kind  valueKind
	check func(value string) error // receives the text form; nil accepts any value of the kind
	path  bool                     // config set makes a relative value absolute
}

// valueKind is the JSON type of a setting's value. The zero value is text.
type valueKind int

const (
	textKind   valueKind = iota // a JSON string
	numberKind                  // a JSON integer
	boolKind                    // JSON true or false
	listKind                    // a JSON array of strings
)

// configKeys lists every setting, in the order config list shows them.
var configKeys = []configKey{
	{name: "cache_dir", check: checkAbsolute, path: true},
	{name: "container_dir", check: checkAbsolute, path: true},
	{name: "container_layout", check: checkLayout},
	{name: "module_dir", check: checkAbsolute, path: true},
	{name: "module_sciget", check: checkApptainerPath},
	{name: "finder_excludes", kind: listKind, check: eachElement(checkArg)},
	{name: "apptainer_path", check: checkApptainerPath},
	{name: "apptainer_global_args", kind: listKind, check: eachElement(checkArg)},
	{name: "apptainer_exec_args", kind: listKind, check: eachElement(checkVerbArg)},
	{name: "apptainer_shell_args", kind: listKind, check: eachElement(checkVerbArg)},
	{name: "apptainer_extra_args", kind: listKind, check: eachElement(checkArg)},
	{name: "apptainer_pass_env", kind: listKind, check: eachElement(checkEnvName)},
	{name: "xauthority_var", check: checkEnvName},
	{name: "tmp_vars", kind: listKind, check: eachElement(checkEnvName)},
	{name: "tmp_target", check: checkBindTarget},
	{name: "use_catalog_args", kind: boolKind},
	{name: "max_catalog_size", kind: numberKind, check: checkCatalogSize},
	{name: "display_start_command", kind: listKind, check: eachElement(checkArg)},
	{name: "display_start_seconds", kind: numberKind, check: checkSeconds},
	{name: "launch_check_seconds", kind: numberKind, check: checkSeconds},
	{name: "network_timeout_seconds", kind: numberKind, check: checkSeconds},
	{name: "update_url", check: checkHTTPSURL},
	{name: "download_urls", kind: listKind, check: eachElement(checkDownloadURL)},
	{name: "warn_missing_checksum", kind: boolKind},
	{name: "auto_pull", kind: boolKind},
	{name: "parallel_pulls", kind: numberKind, check: checkParallelPulls},
}

//go:embed defaults.json
var defaultsJSON []byte

// systemDir is the folder of the system layer: the config and catalog
// list an admin or image writes with --system, read under the user's.
var systemDir = "/etc/sciget"

// systemPath returns the path of the named file in systemDir, or "" when
// systemDir is not absolute (on Windows, /etc/sciget is relative to the
// current drive), so there is no system layer.
func systemPath(name string) string {
	if !filepath.IsAbs(systemDir) {
		return ""
	}
	return filepath.Join(systemDir, name)
}

// config is a loaded config file: its path and the keys it sets, each in
// its text form. The user's config also holds the system file's keys, under
// its own. Keys neither sets have their default.
type config struct {
	path      string
	overrides map[string]string // the keys the file at path sets
	system    map[string]string // the system file's keys; nil when path is the system file
	isSystem  bool              // path is the system file
}

// readConfig loads the config file at configPath, over the system file.
func readConfig() (config, error) {
	path, err := configPath()
	if err != nil {
		return config{}, err
	}
	return loadConfig(path)
}

// readSystemConfig loads the system file alone, for --system.
func readSystemConfig() (config, error) {
	path := systemPath("config.json")
	if path == "" {
		return config{}, errors.New("this system has no system folder")
	}
	overrides, err := readConfigFile(path, true)
	if err != nil {
		return config{}, err
	}
	return config{path: path, overrides: overrides, isSystem: true}, nil
}

// configPath returns SCIGET_CONFIG if it is set, otherwise config.json in
// the user's config directory.
func configPath() (string, error) {
	if path := os.Getenv("SCIGET_CONFIG"); path != "" {
		return path, nil
	}
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", fmt.Errorf("finding the config directory: %w", err)
	}
	return filepath.Join(dir, "sciget", "config.json"), nil
}

// loadConfig reads the config file at path, over the system file.
func loadConfig(path string) (config, error) {
	system := map[string]string{}
	if systemFile := systemPath("config.json"); systemFile != "" {
		var err error
		if system, err = readConfigFile(systemFile, true); err != nil {
			return config{}, err
		}
	}
	overrides, err := readConfigFile(path, false)
	if err != nil {
		return config{}, err
	}
	return config{path: path, overrides: overrides, system: system}, nil
}

// readConfigFile returns the keys the config file at path sets, each in its
// text form. A missing file sets nothing. Anything but a JSON object of
// known keys with valid values of their kinds is an error, as is a per-user
// key in the system file.
func readConfigFile(path string, system bool) (map[string]string, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return map[string]string{}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("reading config: %w", err)
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, fmt.Errorf("reading config %s: %w", path, err)
	}
	if raw == nil {
		return nil, fmt.Errorf("reading config %s: not a JSON object", path)
	}
	overrides := map[string]string{}
	for _, name := range slices.Sorted(maps.Keys(raw)) {
		key, err := findConfigKey(name)
		if err == nil && system && key.path {
			err = perUserError(name)
		}
		if err != nil {
			return nil, fmt.Errorf("reading config %s: %w", path, err)
		}
		value, err := valueText(key.kind, raw[name])
		if err == nil {
			err = key.validate(value)
		}
		if err != nil {
			return nil, fmt.Errorf("reading config %s: invalid %s: %w", path, name, err)
		}
		overrides[name] = value
	}
	return overrides, nil
}

// perUserError is why a folder key may not be set system-wide: sciget
// writes into these folders, so one shared by every user would let one
// user change what another runs.
func perUserError(name string) error {
	return fmt.Errorf("%s can only be set per user", name)
}

// get returns the text form of the named key's value and where it came
// from: "user" or "system" for the file that sets it, otherwise "default".
func (c config) get(name string) (value, source string, err error) {
	key, err := findConfigKey(name)
	if err != nil {
		return "", "", err
	}
	if v, ok := c.overrides[name]; ok {
		if c.isSystem {
			return v, "system", nil
		}
		return v, "user", nil
	}
	if v, ok := c.system[name]; ok {
		return v, "system", nil
	}
	v, err := defaultValue(key.name)
	return v, "default", err
}

// getList returns the value of the named list key.
func (c config) getList(name string) ([]string, error) {
	value, _, err := c.get(name)
	if err != nil {
		return nil, err
	}
	var list []string
	if err := json.Unmarshal([]byte(value), &list); err != nil {
		return nil, fmt.Errorf("reading %s: %w", name, err)
	}
	return list, nil
}

// getNumber returns the value of the named number key.
func (c config) getNumber(name string) (int64, error) {
	value, _, err := c.get(name)
	if err != nil {
		return 0, err
	}
	n, err := strconv.ParseInt(value, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("reading %s: %w", name, err)
	}
	return n, nil
}

// getBool returns the value of the named bool key.
func (c config) getBool(name string) (bool, error) {
	value, _, err := c.get(name)
	return value == "true", err
}

// set checks value, given in its text form, stores it under name and
// writes the file. A relative path is made absolute first. An unknown name
// or invalid value is a usageError.
func (c config) set(name, value string) error {
	key, err := findConfigKey(name)
	if err != nil {
		return usageError{err.Error()}
	}
	if key.path && c.isSystem {
		return usageError{perUserError(name).Error()}
	}
	if key.path && value != "" {
		if value, err = filepath.Abs(value); err != nil {
			return fmt.Errorf("making %s absolute: %w", name, err)
		}
	}
	if key.kind != textKind {
		// Canonical form, so "[ \"a\" ]" is stored and printed as ["a"].
		if value, err = valueText(key.kind, []byte(value)); err != nil {
			return usageError{fmt.Sprintf("invalid %s: %s", name, err)}
		}
	}
	if err := key.validate(value); err != nil {
		return usageError{fmt.Sprintf("invalid %s: %s", name, err)}
	}
	c.overrides[name] = value
	return c.write()
}

// unset removes the named key from the file, if the file sets it. An
// unknown name is a usageError.
func (c config) unset(name string) error {
	if _, err := findConfigKey(name); err != nil {
		return usageError{err.Error()}
	}
	if _, ok := c.overrides[name]; !ok {
		return nil
	}
	delete(c.overrides, name)
	return c.write()
}

// write saves the keys c's file sets.
func (c config) write() error {
	if c.isSystem {
		return writeSystemConfig(c.path, c.overrides)
	}
	return writeConfig(c.path, c.overrides)
}

// writeConfig writes overrides, each in its text form, to the config file
// at path, in place, each as its kind's JSON type.
func writeConfig(path string, overrides map[string]string) error {
	values, err := configValues(overrides)
	if err == nil {
		err = writeJSONFile(path, values)
	}
	if err != nil {
		return fmt.Errorf("writing config: %w", err)
	}
	return nil
}

// writeSystemConfig writes overrides to the system config file at path, as
// writeSystemFile does.
func writeSystemConfig(path string, overrides map[string]string) error {
	values, err := configValues(overrides)
	if err == nil {
		err = writeSystemFile(path, values)
	}
	if err != nil {
		return fmt.Errorf("writing config: %w", err)
	}
	return nil
}

// configValues returns overrides with each value as its kind's JSON type.
func configValues(overrides map[string]string) (map[string]any, error) {
	values := map[string]any{}
	for name, value := range overrides {
		key, err := findConfigKey(name)
		if err != nil {
			return nil, err
		}
		if key.kind == textKind {
			values[name] = value
		} else {
			values[name] = json.RawMessage(value)
		}
	}
	return values, nil
}

// writeJSONFile writes v as indented JSON to the file at path in place, so a
// read-only file stays read-only and a symlink is followed. A new file is
// private to the user.
func writeJSONFile(path string, v any) error {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	_, err = f.Write(append(data, '\n'))
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	return err
}

// writeSystemFile writes v as indented JSON to the system file at path,
// for every user to read: through a temporary file renamed into place, so
// no user reads part of one and a symlink at path is replaced, not
// followed. A folder it creates is readable by everyone, whatever root's
// umask. Permission errors say that --system needs root.
func writeSystemFile(path string, v any) error {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	dir := filepath.Dir(path)
	if _, err := os.Stat(dir); errors.Is(err, fs.ErrNotExist) {
		err = os.MkdirAll(dir, 0o755)
		if err == nil {
			err = os.Chmod(dir, 0o755)
		}
		if err != nil {
			return systemWriteError(err)
		}
	}
	return systemWriteError(writeFileAtomic(path, string(data)+"\n"))
}

// systemWriteError adds a hint to a permission error from a --system write.
func systemWriteError(err error) error {
	if errors.Is(err, fs.ErrPermission) {
		return fmt.Errorf("%w (--system needs root)", err)
	}
	return err
}

// writeConfigList prints every key with its value and source, as a table.
func writeConfigList(w io.Writer, c config, terminal bool) error {
	var rows strings.Builder
	for _, key := range configKeys {
		value, source, err := c.get(key.name)
		if err != nil {
			return err
		}
		fmt.Fprintf(&rows, "%s\t%s\t%s\n", key.name, value, source)
	}
	return writeTable(w, terminal, "KEY\tVALUE\tSOURCE\n", rows.String())
}

// findConfigKey returns the key with exactly this name.
func findConfigKey(name string) (configKey, error) {
	var names []string
	for _, key := range configKeys {
		if key.name == name {
			return key, nil
		}
		names = append(names, key.name)
	}
	return configKey{}, fmt.Errorf("unknown key %q; valid keys: %s", name, strings.Join(names, ", "))
}

// defaultValue returns the text form of a key's default from
// defaults.json, with base directories expanded in path keys.
func defaultValue(name string) (string, error) {
	var defaults map[string]json.RawMessage
	if err := json.Unmarshal(defaultsJSON, &defaults); err != nil {
		return "", fmt.Errorf("reading defaults: %w", err)
	}
	key, err := findConfigKey(name)
	if err != nil {
		return "", err
	}
	raw, ok := defaults[name]
	if !ok {
		return "", fmt.Errorf("no default for %s", name)
	}
	value, err := valueText(key.kind, raw)
	if err != nil {
		return "", fmt.Errorf("invalid default for %s: %w", name, err)
	}
	if key.path {
		if value, err = expandBaseDirs(value); err != nil {
			return "", fmt.Errorf("invalid default for %s: %w", name, err)
		}
	}
	if err := key.validate(value); err != nil {
		return "", fmt.Errorf("invalid default for %s: %w", name, err)
	}
	return value, nil
}

// validate checks a value in its text form with the key's check, if any.
func (k configKey) validate(value string) error {
	if k.check == nil {
		return nil
	}
	return k.check(value)
}

// valueText returns a value read from a JSON file in its text form: a text
// value as is, any other kind as compact JSON. The text form is what config
// get prints, config set takes and checks receive.
func valueText(kind valueKind, raw []byte) (string, error) {
	// Unmarshal turns null into a zero value without error.
	if string(bytes.TrimSpace(raw)) == "null" {
		return "", errors.New("must not be null")
	}
	switch kind {
	case numberKind:
		var n int64
		if err := json.Unmarshal(raw, &n); err != nil {
			return "", errors.New("must be a whole number")
		}
		return strconv.FormatInt(n, 10), nil
	case boolKind:
		var b bool
		if err := json.Unmarshal(raw, &b); err != nil {
			return "", errors.New("must be true or false")
		}
		return strconv.FormatBool(b), nil
	case listKind:
		var list []string
		if err := json.Unmarshal(raw, &list); err != nil {
			return "", errors.New("must be a JSON array of strings")
		}
		var out bytes.Buffer
		enc := json.NewEncoder(&out)
		enc.SetEscapeHTML(false) // keep "&" and "<" readable in config list
		if err := enc.Encode(list); err != nil {
			return "", err
		}
		return strings.TrimSuffix(out.String(), "\n"), nil
	}
	var text string
	if err := json.Unmarshal(raw, &text); err != nil {
		return "", errors.New("must be a JSON string")
	}
	return text, nil
}

// baseDirs lists the placeholders a default path may use.
var baseDirs = []struct {
	placeholder string
	dir         func() (string, error)
}{
	{placeholder: "{user_cache_dir}", dir: os.UserCacheDir},
	{placeholder: "{user_config_dir}", dir: os.UserConfigDir},
	{placeholder: "{user_data_dir}", dir: userDataDir},
}

// expandBaseDirs replaces the baseDirs placeholders in template, resolving
// only those it uses.
func expandBaseDirs(template string) (string, error) {
	// Check the template, not the result: a base directory may contain braces.
	remaining := template
	for _, b := range baseDirs {
		remaining = strings.ReplaceAll(remaining, b.placeholder, "")
	}
	if strings.ContainsAny(remaining, "{}") {
		return "", errors.New("unknown placeholder")
	}
	var pairs []string
	for _, b := range baseDirs {
		if !strings.Contains(template, b.placeholder) {
			continue
		}
		dir, err := b.dir()
		if err != nil {
			return "", fmt.Errorf("expanding %s: %w", b.placeholder, err)
		}
		pairs = append(pairs, b.placeholder, filepath.ToSlash(dir))
	}
	// One pass, so a placeholder inside a base directory is left alone.
	return filepath.FromSlash(strings.NewReplacer(pairs...).Replace(template)), nil
}

// userDataDir returns $XDG_DATA_HOME, else ~/.local/share, on every platform.
func userDataDir() (string, error) {
	if dir := os.Getenv("XDG_DATA_HOME"); dir != "" {
		return dir, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".local", "share"), nil
}

func checkApptainerPath(value string) error {
	if value == "" || strings.ContainsRune(value, 0) || !filepath.IsAbs(value) && strings.ContainsAny(value, "/"+string(filepath.Separator)) {
		return errors.New("must be a bare name or an absolute path")
	}
	return nil
}

// checkLayout validates the placeholders and relative path of a layout.
func checkLayout(value string) error {
	remaining := value
	for _, placeholder := range []string{"{tool}", "{version}", "{date}"} {
		if !strings.Contains(value, placeholder) {
			return fmt.Errorf("must contain %s", placeholder)
		}
		remaining = strings.ReplaceAll(remaining, placeholder, "x")
	}
	if strings.ContainsAny(remaining, "{}") {
		return errors.New("unknown layout placeholder")
	}
	if filepath.IsAbs(remaining) || filepath.VolumeName(remaining) != "" {
		return errors.New("must be a relative path")
	}
	for _, element := range strings.Split(filepath.ToSlash(remaining), "/") {
		if element == "" || element == ".." || strings.ContainsRune(element, 0) {
			return errors.New("must have no empty or '..' path elements or NUL")
		}
	}
	return nil
}

// eachElement turns a check of one string into a check of a list's text
// form.
func eachElement(check func(string) error) func(string) error {
	return func(value string) error {
		var list []string
		if err := json.Unmarshal([]byte(value), &list); err != nil {
			return err
		}
		for _, element := range list {
			if err := check(element); err != nil {
				return fmt.Errorf("%q %w", element, err)
			}
		}
		return nil
	}
}

// checkArg checks one literal Apptainer argument.
func checkArg(value string) error {
	switch {
	case value == "":
		return errors.New("must not be empty")
	case strings.ContainsRune(value, 0):
		return errors.New("must not contain NUL")
	case strings.ContainsAny(value, "{}"):
		return errors.New("must not contain { or }")
	}
	return nil
}

// checkVerbArg checks one verb argument: a placeholder or a literal.
func checkVerbArg(value string) error {
	if _, ok := placeholderArgs[value]; ok {
		return nil
	}
	if strings.HasPrefix(value, "{") && strings.HasSuffix(value, "}") {
		return errors.New("is not a known placeholder")
	}
	return checkArg(value)
}

// checkEnvName checks an environment variable name.
func checkEnvName(value string) error {
	for i, r := range value {
		if r == '_' || 'A' <= r && r <= 'Z' || 'a' <= r && r <= 'z' || i > 0 && '0' <= r && r <= '9' {
			continue
		}
		return errors.New("must be an environment variable name")
	}
	if value == "" {
		return errors.New("must be an environment variable name")
	}
	return nil
}

// bindSeparators are the characters Apptainer splits a --bind value on.
const bindSeparators = `,:\`

// checkBindTarget checks a path used as the target of a --bind.
func checkBindTarget(value string) error {
	if err := checkAbsolute(value); err != nil {
		return err
	}
	if strings.ContainsAny(value, bindSeparators) {
		return errors.New(`must not contain "," ":" or "\"`)
	}
	return nil
}

// maxCatalogLimit bounds max_catalog_size, so that limit+1 cannot
// overflow when a download reads one byte past it.
const maxCatalogLimit = 1 << 40

func checkCatalogSize(value string) error {
	n, err := strconv.ParseInt(value, 10, 64)
	if err != nil || n < 1 || n > maxCatalogLimit {
		return fmt.Errorf("must be from 1 to %d (1 TiB)", int64(maxCatalogLimit))
	}
	return nil
}

// checkSeconds checks a number of seconds from 1 to 3600.
func checkSeconds(value string) error {
	n, err := strconv.ParseInt(value, 10, 64)
	if err != nil || n < 1 || n > 3600 {
		return errors.New("must be from 1 to 3600 seconds")
	}
	return nil
}

func checkParallelPulls(value string) error {
	n, err := strconv.ParseInt(value, 10, 64)
	if err != nil || n < 1 || n > 8 {
		return errors.New("must be from 1 to 8")
	}
	return nil
}

// checkHTTPSURL checks an https URL with a host and no user information.
func checkHTTPSURL(value string) error {
	u, err := url.Parse(value)
	if err != nil || u.Scheme != "https" || u.Host == "" {
		return errors.New("must be an https URL")
	}
	if u.User != nil {
		return errors.New("must not contain a user name or password")
	}
	return nil
}

// checkDownloadURL checks a download URL template: a catalog URL (see
// checkCatalogURL) with {file} exactly once.
func checkDownloadURL(value string) error {
	if strings.Count(value, "{file}") != 1 {
		return errors.New("must contain {file} exactly once")
	}
	return checkCatalogURL(value)
}

func checkAbsolute(value string) error {
	if !filepath.IsAbs(value) {
		return errors.New("must be an absolute path")
	}
	return nil
}
