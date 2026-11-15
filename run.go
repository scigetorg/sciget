package main

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/spf13/cobra"
)

// runApptainer hands over to Apptainer and can be replaced by tests.
var runApptainer = execApptainer

// location identifies a container path and its catalogue share or the store.
type location struct {
	path  string
	place string
}

// notOnMachineError identifies a known container missing from local storage.
type notOnMachineError struct{ name string }

func (e notOnMachineError) Error() string { return e.name + " is not on this machine" }

// unsafeLayoutValue reports whether v cannot fill a layout: it is empty, or
// could name another directory.
func unsafeLayoutValue(v string) bool {
	return v == "" || strings.Contains(v, "..") || strings.ContainsAny(v, "/\x00"+string(filepath.Separator))
}

// layoutPath fills a validated layout with safe container values.
func layoutPath(layout string, c container) (string, error) {
	if err := checkLayout(layout); err != nil {
		return "", fmt.Errorf("container %q has an invalid layout: %w", c.name(), err)
	}
	for _, value := range []string{c.tool, c.version, c.buildDate} {
		if unsafeLayoutValue(value) {
			return "", fmt.Errorf("catalogue entry %q has an unsafe layout value %q", c.name(), value)
		}
	}
	path := strings.NewReplacer("{tool}", c.tool, "{version}", c.version, "{date}", c.buildDate).Replace(layout)
	if !filepath.IsLocal(path) {
		return "", fmt.Errorf("catalogue entry %q has an unsafe container path", c.name())
	}
	return filepath.FromSlash(path), nil
}

// locate searches the winner's catalogue share, then the store cfg names.
func locate(cfg config, c container) (location, error) {
	_, _, catalogues, err := readCatalogues()
	if err != nil {
		return location{}, err
	}
	if i := findCatalogue(catalogues, c.catalogue); i >= 0 && catalogues[i].Dir != "" {
		cat := catalogues[i]
		layout := cat.Layout
		if layout == "" {
			// A share is laid out by its publisher, so the user's
			// container_layout does not apply.
			if layout, err = defaultValue("container_layout"); err != nil {
				return location{}, err
			}
		}
		path, err := findContainer(cat.Dir, layout, c)
		if err != nil {
			return location{}, err
		}
		if path != "" {
			return location{path: path, place: cat.Name}, nil
		}
	}
	// The store is resolved only after the share misses, so a share hit
	// works even when the store's default cannot be expanded.
	dir, _, err := cfg.get("container_dir")
	if err != nil {
		return location{}, err
	}
	layout, _, err := cfg.get("container_layout")
	if err != nil {
		return location{}, err
	}
	path, err := findContainer(dir, layout, c)
	if err != nil {
		return location{}, err
	}
	if path == "" {
		return location{}, notOnMachineError{name: c.name()}
	}
	return location{path: path, place: "store"}, nil
}

// findContainer returns c's path in dir, or "" if no regular file or
// directory is there.
func findContainer(dir, layout string, c container) (string, error) {
	relative, err := layoutPath(layout, c)
	if err != nil {
		return "", err
	}
	path := filepath.Join(dir, relative)
	info, err := os.Stat(path)
	if errors.Is(err, fs.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("locating %s: %w", c.name(), err)
	}
	if !info.Mode().IsRegular() && !info.IsDir() {
		return "", nil
	}
	return path, nil
}

// execApptainer replaces sciget with the Apptainer binary at path. It
// returns only on failure.
func execApptainer(path string, args, env []string) error {
	if err := syscall.Exec(path, args, env); err != nil {
		return fmt.Errorf("running apptainer: %w", err)
	}
	return nil
}

// launchSettings holds the config values that shape an Apptainer command
// line.
type launchSettings struct {
	apptainer     string   // apptainer_path, as configured
	global        []string // apptainer_global_args
	verbArgs      []string // apptainer_exec_args or apptainer_shell_args
	extra         []string // apptainer_extra_args
	catalogue     []string // the container's apptainer_args, if use_catalogue_args
	passEnv       []string // apptainer_pass_env
	xauthorityVar string
	tmpVars       []string
	tmpTarget     string
}

// readLaunchSettings reads the settings for running c with verb.
func readLaunchSettings(cfg config, verb string, c container) (launchSettings, error) {
	var s launchSettings
	var err error
	if s.apptainer, _, err = cfg.get("apptainer_path"); err != nil {
		return s, err
	}
	for name, list := range map[string]*[]string{
		"apptainer_global_args":       &s.global,
		"apptainer_" + verb + "_args": &s.verbArgs,
		"apptainer_extra_args":        &s.extra,
		"apptainer_pass_env":          &s.passEnv,
		"tmp_vars":                    &s.tmpVars,
	} {
		if *list, err = cfg.getList(name); err != nil {
			return s, err
		}
	}
	if s.xauthorityVar, _, err = cfg.get("xauthority_var"); err != nil {
		return s, err
	}
	if s.tmpTarget, _, err = cfg.get("tmp_target"); err != nil {
		return s, err
	}
	useCatalogue, err := cfg.getBool("use_catalogue_args")
	if err != nil {
		return s, err
	}
	if useCatalogue {
		s.catalogue = c.apptainerArgs
	}
	return s, nil
}

// apptainerArgs builds argv for running image with verb: the binary as
// configured, global arguments, the verb, its arguments with placeholders
// expanded, extra and catalogue arguments, the image and, for exec, the
// command.
func apptainerArgs(s launchSettings, verb, image string, command []string) ([]string, error) {
	args := append([]string{s.apptainer}, s.global...)
	args = append(args, verb)
	for _, arg := range s.verbArgs {
		if expand, ok := placeholderArgs[arg]; ok {
			expanded, err := expand(s)
			if err != nil {
				return nil, err
			}
			args = append(args, expanded...)
			continue
		}
		args = append(args, arg)
	}
	args = append(args, s.extra...)
	args = append(args, s.catalogue...)
	args = append(args, image)
	if verb == "exec" {
		args = append(args, command...)
	}
	return args, nil
}

// placeholderArgs maps each placeholder a verb argument may be to the
// arguments it expands to at run time.
var placeholderArgs = map[string]func(launchSettings) ([]string, error){
	"{env}":        envArgs,
	"{xauthority}": xauthorityArgs,
	"{tmp}":        tmpArgs,
	"{pwd}":        pwdArgs,
}

// envArgs passes each non-empty variable in passEnv through --cleanenv.
func envArgs(s launchSettings) ([]string, error) {
	var args []string
	for _, name := range s.passEnv {
		value := os.Getenv(name)
		if value == "" {
			continue
		}
		if err := checkEnvValue(name, value); err != nil {
			return nil, err
		}
		args = append(args, "--env", name+"="+value)
	}
	return args, nil
}

// xauthorityArgs binds the X authority file read-only and passes its path,
// if the variable names a regular file.
func xauthorityArgs(s launchSettings) ([]string, error) {
	path := os.Getenv(s.xauthorityVar)
	if path == "" {
		return nil, nil
	}
	if info, err := os.Stat(path); err != nil || !info.Mode().IsRegular() {
		return nil, nil
	}
	if err := checkEnvValue(s.xauthorityVar, path); err != nil {
		return nil, err
	}
	if err := checkBindPath(s.xauthorityVar, path); err != nil {
		return nil, err
	}
	return []string{"--bind", path + ":" + path + ":ro", "--env", s.xauthorityVar + "=" + path}, nil
}

// tmpArgs binds the directory named by the last non-empty variable in
// tmpVars to tmpTarget.
func tmpArgs(s launchSettings) ([]string, error) {
	var name, dir string
	for _, v := range s.tmpVars {
		if value := os.Getenv(v); value != "" {
			name, dir = v, value
		}
	}
	if dir == "" {
		return nil, nil
	}
	if err := checkBindPath(name, dir); err != nil {
		return nil, err
	}
	return []string{"--bind", dir + ":" + s.tmpTarget}, nil
}

// pwdArgs starts the container in the physical working directory.
func pwdArgs(launchSettings) ([]string, error) {
	dir, err := os.Getwd()
	if err == nil {
		dir, err = filepath.EvalSymlinks(dir)
	}
	if err != nil {
		return nil, fmt.Errorf("finding the current directory: %w", err)
	}
	return []string{"--pwd", dir}, nil
}

// checkEnvValue rejects a value Apptainer would misread in --env: it splits
// the value on commas and evaluates it in the container.
func checkEnvValue(name, value string) error {
	if i := strings.IndexAny(value, ",$`\\\""); i >= 0 {
		return fmt.Errorf("cannot pass %s to the container: its value contains %q", name, value[i:i+1])
	}
	return nil
}

// checkBindPath rejects a path Apptainer would misread in --bind.
func checkBindPath(name, path string) error {
	if i := strings.IndexAny(path, bindSeparators); i >= 0 {
		return fmt.Errorf("cannot bind %s into the container: its value contains %q", name, path[i:i+1])
	}
	return nil
}

// shellJoin quotes args for a POSIX shell and joins them, so the line can
// be pasted and run.
func shellJoin(args []string) string {
	quoted := make([]string, len(args))
	for i, arg := range args {
		quoted[i] = arg
		if arg == "" || strings.ContainsFunc(arg, needsQuoting) {
			quoted[i] = "'" + strings.ReplaceAll(arg, "'", `'\''`) + "'"
		}
	}
	return strings.Join(quoted, " ")
}

// needsQuoting reports whether r is outside the characters a shell word
// may hold bare.
func needsQuoting(r rune) bool {
	return !('a' <= r && r <= 'z' || 'A' <= r && r <= 'Z' || '0' <= r && r <= '9' ||
		strings.ContainsRune("_@%+=:,./-", r))
}

func newApptainerCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:    "apptainer",
		Short:  "Run containers with Apptainer",
		Hidden: true,
		Args:   noArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return cmd.Help()
		},
	}
	cmd.AddCommand(newApptainerVerbCommand("exec"), newApptainerVerbCommand("shell"))
	return cmd
}

// newApptainerVerbCommand builds an exec or shell command with shared launch checks.
func newApptainerVerbCommand(verb string) *cobra.Command {
	var dryRun bool
	cmd := &cobra.Command{
		Use:               verb + " <tool/version>",
		Short:             "Open a container shell",
		Args:              exactArgs(1),
		ValidArgsFunction: completeContainerNames,
		RunE: func(cmd *cobra.Command, args []string) error {
			containers, err := readContainers()
			if err != nil {
				return err
			}
			c, err := lookupContainer(containers, args[0])
			if err != nil {
				return err
			}
			cfg, err := readConfig()
			if err != nil {
				return err
			}
			loc, err := locate(cfg, c)
			if err != nil {
				return err
			}
			settings, err := readLaunchSettings(cfg, verb, c)
			if err != nil {
				return err
			}
			path, err := exec.LookPath(settings.apptainer)
			if err != nil {
				return fmt.Errorf("apptainer not found: %q; install Apptainer or set apptainer_path", settings.apptainer)
			}
			argv, err := apptainerArgs(settings, verb, loc.path, args[1:])
			if err != nil {
				return err
			}
			if dryRun {
				// The resolved path, so the printed line runs the same binary.
				line := shellJoin(append([]string{path}, argv[1:]...))
				_, err := fmt.Fprintln(cmd.OutOrStdout(), line)
				return err
			}
			return runApptainer(path, argv, os.Environ())
		},
	}
	if verb == "exec" {
		cmd.Use += " <command> [args...]"
		cmd.Short = "Run a command in a container"
		cmd.Args = func(cmd *cobra.Command, args []string) error {
			if err := cobra.MinimumNArgs(2)(cmd, args); err != nil {
				return usageError{err.Error()}
			}
			return nil
		}
	}
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "print the Apptainer command instead of running it")
	cmd.Flags().SetInterspersed(false)
	return cmd
}

// completeContainerNames suggests names only for the first argument.
func completeContainerNames(cmd *cobra.Command, args []string, toComplete string) ([]string, cobra.ShellCompDirective) {
	if len(args) > 0 {
		return nil, cobra.ShellCompDirectiveNoFileComp
	}
	containers, err := readContainers()
	if err != nil {
		return nil, cobra.ShellCompDirectiveError
	}
	return completeNames(containers, toComplete, false)
}
