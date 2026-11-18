package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strings"
)

// commandList is what a container offers its module: the commands to wrap,
// the settings to set and the help text.
type commandList struct {
	Found []string `json:"found"` // executables the finder saw, before finder_excludes
	Kept  []string `json:"kept"`  // names never excluded: DEPLOY_BINS
	Env   []string `json:"env"`   // DEPLOY_ENV_ settings as NAME=value, without the prefix
	Help  string   `json:"help"`
}

// finderScript lists, in a container, its environment, the executable files
// and symlinks directly in each DEPLOY_PATH folder, and /README.md, each part
// after a marker line.
const finderScript = `env
echo ` + filesMarker + `
set -f
IFS=:
for d in $DEPLOY_PATH; do
	[ -n "$d" ] && find "$d" -maxdepth 1 -executable \( -type f -o -type l \) 2>/dev/null
done
echo ` + readmeMarker + `
cat /README.md 2>/dev/null
exit 0
`

const (
	filesMarker  = "SCIGET_FINDER_FILES"
	readmeMarker = "SCIGET_FINDER_README"
)

// listCommands returns the command list of c, found at loc: from the cache,
// else by running the finder in the container and caching what it prints.
func listCommands(ctx context.Context, cfg config, c container, loc location) (commandList, error) {
	cachePath, err := commandCachePath(cfg, c)
	if err != nil {
		return commandList{}, err
	}
	if data, err := os.ReadFile(cachePath); err == nil {
		var list commandList
		if json.Unmarshal(data, &list) == nil {
			return list, nil
		}
	}
	list, err := runFinder(ctx, cfg, loc.path)
	if err != nil {
		return commandList{}, fmt.Errorf("listing the commands of %s: %w", c.name(), err)
	}
	if err := writeJSONFile(cachePath, list); err != nil {
		return commandList{}, fmt.Errorf("caching the commands of %s: %w", c.name(), err)
	}
	return list, nil
}

// commandCachePath returns where the finder's output for c is cached: one
// file per catalog, tool, version and build.
func commandCachePath(cfg config, c container) (string, error) {
	cacheDir, _, err := cfg.get("cache_dir")
	if err != nil {
		return "", err
	}
	for _, value := range []string{c.catalog, c.tool, c.version, c.buildDate} {
		if unsafeLayoutValue(value) {
			return "", fmt.Errorf("catalog entry %q has an unsafe cache path value %q", c.name(), value)
		}
	}
	return filepath.Join(cacheDir, "commands", c.catalog, c.tool, c.version, c.buildDate+".json"), nil
}

// runFinder runs finderScript in the image at image and parses what it
// prints.
func runFinder(ctx context.Context, cfg config, image string) (commandList, error) {
	apptainer, _, err := cfg.get("apptainer_path")
	if err != nil {
		return commandList{}, err
	}
	apptainerPath, err := exec.LookPath(apptainer)
	if err != nil {
		return commandList{}, fmt.Errorf("apptainer not found: %q; install Apptainer or set apptainer_path", apptainer)
	}
	global, err := cfg.getList("apptainer_global_args")
	if err != nil {
		return commandList{}, err
	}
	extra, err := cfg.getList("apptainer_extra_args")
	if err != nil {
		return commandList{}, err
	}
	// --cleanenv keeps the host's variables out; the image's own are still set.
	args := append(append(append([]string{}, global...), "exec", "--cleanenv"), extra...)
	args = append(args, image, "/bin/sh", "-c", finderScript)
	cmd := exec.CommandContext(ctx, apptainerPath, args...)
	var stderr strings.Builder
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		if errLines := lines(strings.TrimSpace(stderr.String())); len(errLines) > 0 {
			return commandList{}, fmt.Errorf("%w: %s", err, printable(errLines[len(errLines)-1]))
		}
		return commandList{}, err
	}
	return parseFinder(string(out))
}

// parseFinder parses the output of finderScript.
func parseFinder(out string) (commandList, error) {
	// The leading newline lets the first marker match even with no
	// environment printed.
	envPart, rest, ok1 := strings.Cut("\n"+out, "\n"+filesMarker+"\n")
	filesPart, help, ok2 := strings.Cut("\n"+rest, "\n"+readmeMarker+"\n")
	if !ok1 || !ok2 {
		return commandList{}, errors.New("unexpected output from the finder")
	}
	var list commandList
	for _, line := range lines(envPart) {
		if setting, ok := strings.CutPrefix(line, "DEPLOY_ENV_"); ok {
			list.Env = append(list.Env, setting)
		}
		if bins, ok := strings.CutPrefix(line, "DEPLOY_BINS="); ok {
			for _, bin := range strings.Split(bins, ":") {
				if bin != "" {
					list.Kept = append(list.Kept, bin)
				}
			}
		}
	}
	for _, line := range lines(filesPart) {
		list.Found = append(list.Found, path.Base(line))
	}
	list.Help = help
	return list, nil
}

// lines returns the non-empty lines of s.
func lines(s string) []string {
	var out []string
	for _, line := range strings.Split(s, "\n") {
		if line != "" {
			out = append(out, line)
		}
	}
	return out
}
