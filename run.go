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

// layoutPath fills a validated layout with safe container values.
func layoutPath(layout string, c container) (string, error) {
	if err := checkLayout(layout); err != nil {
		return "", fmt.Errorf("container %q has an invalid layout: %w", c.name(), err)
	}
	for _, value := range []string{c.tool, c.version, c.buildDate} {
		if value == "" || strings.Contains(value, "..") || strings.ContainsAny(value, "/\x00"+string(filepath.Separator)) {
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

// apptainerArgs builds argv with the configured binary name as argv[0].
func apptainerArgs(apptainer, verb, image string, command []string) []string {
	args := []string{apptainer, verb, image}
	if verb == "exec" {
		args = append(args, command...)
	}
	return args
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
			apptainer, _, err := cfg.get("apptainer_path")
			if err != nil {
				return err
			}
			path, err := exec.LookPath(apptainer)
			if err != nil {
				return fmt.Errorf("apptainer not found: %q; install Apptainer or set apptainer_path", apptainer)
			}
			return runApptainer(path, apptainerArgs(apptainer, verb, loc.path, args[1:]), os.Environ())
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
