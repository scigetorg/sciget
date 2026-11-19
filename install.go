package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"slices"
	"strings"
	"syscall"

	"github.com/spf13/cobra"
)

// installContainer lists c's commands and writes its module, given where c
// is. It prints a line on stdout, and on stderr a warning for anything left
// out of the module, and reports whether it wrote one. A container only on
// a share gets no module: the share provides its own. One also in the
// caller's container_dir gets one, as when an admin's container_dir is the
// share every user reads.
func installContainer(ctx context.Context, stdout, stderr io.Writer, cfg config, c container, loc location) (bool, error) {
	if loc.share != "" {
		path, err := storePath(cfg, c)
		if err != nil {
			return false, err
		}
		if path == "" {
			_, err := fmt.Fprintf(stdout, "No module written for %s: it is on the %s share, which provides its modules\n", c.name(), loc.share)
			return false, err
		}
		loc = location{path: path}
	}
	list, err := listCommands(ctx, cfg, c, loc)
	if err != nil {
		return false, err
	}
	info, err := writeModule(cfg, c, list)
	if err != nil {
		return false, err
	}
	for _, s := range info.skipped {
		fmt.Fprintf(stderr, "warning: %s: left out %s\n", c.name(), s)
	}
	noun := "commands"
	if len(info.commands) == 1 {
		noun = "command"
	}
	_, err = fmt.Fprintf(stdout, "Installed %s: module %s, %d %s\n", c.name(), c.name(), len(info.commands), noun)
	return true, err
}

// storePath returns c's path in the caller's container_dir, or "" if it
// is not there.
func storePath(cfg config, c container) (string, error) {
	dir, _, err := cfg.get("container_dir")
	if err != nil {
		return "", err
	}
	layout, _, err := cfg.get("container_layout")
	if err != nil {
		return "", err
	}
	return findContainer(dir, layout, c)
}

// moduleUseLine returns the module use command for catalog's module
// folder, or "" when that folder is already in MODULEPATH.
func moduleUseLine(cfg config, catalog string) (string, error) {
	moduleDir, _, err := cfg.get("module_dir")
	if err != nil {
		return "", err
	}
	dir := filepath.Join(moduleDir, catalog)
	for _, p := range filepath.SplitList(os.Getenv("MODULEPATH")) {
		if p != "" && filepath.Clean(p) == dir {
			return "", nil
		}
	}
	return shellJoin([]string{"module", "use", dir}), nil
}

func newInstallCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "install <tool/version>...",
		Short: "Download containers and make their modules",
		Args: func(cmd *cobra.Command, args []string) error {
			if len(args) == 0 {
				return usageError{"requires at least 1 arg(s), only received 0"}
			}
			return nil
		},
		// Every argument is a name, so each is completed.
		ValidArgsFunction: func(cmd *cobra.Command, args []string, toComplete string) ([]string, cobra.ShellCompDirective) {
			return completeContainerNames(cmd, nil, toComplete)
		},
		RunE: func(cmd *cobra.Command, args []string) error {
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
			stdout, stderr := cmd.OutOrStdout(), cmd.ErrOrStderr()
			if err := pullAll(ctx, stdout, stderr, cfg, cs); err != nil {
				return err
			}
			var failures, installed []string // installed: catalogs given a module
			for _, c := range cs {
				loc, err := locate(cfg, c)
				written := false
				if err == nil {
					written, err = installContainer(ctx, stdout, stderr, cfg, c, loc)
				}
				if err != nil {
					failures = append(failures, err.Error())
				}
				if written && !slices.Contains(installed, c.catalog) {
					installed = append(installed, c.catalog)
				}
			}
			for _, catalog := range installed {
				line, err := moduleUseLine(cfg, catalog)
				if err != nil {
					return err
				}
				if line != "" {
					if _, err := fmt.Fprintf(stdout, "To use sciget's modules, run: %s\n", line); err != nil {
						return err
					}
				}
			}
			if len(failures) > 0 {
				return fmt.Errorf("%d of %d containers failed:\n%s", len(failures), len(cs), strings.Join(failures, "\n"))
			}
			return nil
		},
	}
}
