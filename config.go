package main

import (
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
	"strings"
)

// defaultCatalogueURL is where the catalogue is downloaded from unless the
// config sets catalogue_url.
const defaultCatalogueURL = "https://apps-json.neurodesk.workers.dev/neuroimaging/apps.json"

// configKey is one setting the config file may hold.
type configKey struct {
	name         string
	defaultValue func() (string, error)
	check        func(value string) error
	path         bool // config set makes a relative value absolute
}

// configKeys lists every setting, in the order config list shows them.
var configKeys = []configKey{
	{name: "cache_dir", defaultValue: defaultCacheDir, check: checkAbsolute, path: true},
	{name: "catalogue_url", defaultValue: func() (string, error) { return defaultCatalogueURL, nil },
		check: checkURL},
}

// config is a loaded config file: its path and the keys it sets. Keys it
// does not set have their default.
type config struct {
	path      string
	overrides map[string]string
}

// readConfig loads the config file at configPath.
func readConfig() (config, error) {
	path, err := configPath()
	if err != nil {
		return config{}, err
	}
	return loadConfig(path)
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

// loadConfig reads the config file at path. A missing file sets nothing.
// Anything but a JSON object of known keys with valid string values is an
// error.
func loadConfig(path string) (config, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return config{path: path, overrides: map[string]string{}}, nil
	}
	if err != nil {
		return config{}, fmt.Errorf("reading config: %w", err)
	}
	var overrides map[string]string
	if err := json.Unmarshal(data, &overrides); err != nil {
		return config{}, fmt.Errorf("reading config %s: %w", path, err)
	}
	if overrides == nil {
		return config{}, fmt.Errorf("reading config %s: not a JSON object", path)
	}
	for _, name := range slices.Sorted(maps.Keys(overrides)) {
		key, err := findConfigKey(name)
		if err != nil {
			return config{}, fmt.Errorf("reading config %s: %w", path, err)
		}
		if err := key.check(overrides[name]); err != nil {
			return config{}, fmt.Errorf("reading config %s: invalid %s: %w", path, name, err)
		}
	}
	return config{path: path, overrides: overrides}, nil
}

// get returns the value of the named key and where it came from: "config"
// if the file sets it, otherwise "default".
func (c config) get(name string) (value, source string, err error) {
	key, err := findConfigKey(name)
	if err != nil {
		return "", "", err
	}
	if v, ok := c.overrides[name]; ok {
		return v, "config", nil
	}
	v, err := key.defaultValue()
	return v, "default", err
}

// set checks value, stores it under name and writes the file. A relative
// path is made absolute first. An unknown name or invalid value is a
// usageError.
func (c config) set(name, value string) error {
	key, err := findConfigKey(name)
	if err != nil {
		return usageError{err.Error()}
	}
	if key.path && value != "" {
		if value, err = filepath.Abs(value); err != nil {
			return fmt.Errorf("making %s absolute: %w", name, err)
		}
	}
	if err := key.check(value); err != nil {
		return usageError{fmt.Sprintf("invalid %s: %s", name, err)}
	}
	c.overrides[name] = value
	return writeConfig(c.path, c.overrides)
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
	return writeConfig(c.path, c.overrides)
}

// writeConfig writes overrides to the config file at path, in place.
func writeConfig(path string, overrides map[string]string) error {
	if err := writeJSONFile(path, overrides); err != nil {
		return fmt.Errorf("writing config: %w", err)
	}
	return nil
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

func defaultCacheDir() (string, error) {
	dir, err := os.UserCacheDir()
	if err != nil {
		return "", fmt.Errorf("finding the cache directory: %w", err)
	}
	return filepath.Join(dir, "sciget"), nil
}

func checkAbsolute(value string) error {
	if !filepath.IsAbs(value) {
		return errors.New("must be an absolute path")
	}
	return nil
}

func checkURL(value string) error {
	u, err := url.Parse(value)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") {
		return errors.New("must start with https:// or http://")
	}
	if u.Hostname() == "" {
		return errors.New("must name a host")
	}
	return nil
}
