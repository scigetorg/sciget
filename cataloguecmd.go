package main

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/spf13/cobra"
)

// newCatalogueCommand builds "sciget catalogue", which lists the catalogues,
// and its subcommands.
func newCatalogueCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "catalogue",
		Short: "Add, use and list catalogues",
		Args:  noArgs,
		RunE:  runCatalogueList,
	}

	var addLast, forceUpdate bool
	var dir, layout string
	add := &cobra.Command{
		Use:               "add <name> <url>",
		Short:             "Add a catalogue and use it first",
		Args:              exactArgs(2),
		ValidArgsFunction: cobra.NoFileCompletions,
		RunE: func(cmd *cobra.Command, args []string) error {
			if cmd.Flags().Changed("layout") && !cmd.Flags().Changed("dir") {
				return usageError{"--layout requires --dir"}
			}
			if cmd.Flags().Changed("dir") && dir == "" {
				return usageError{"--dir must not be empty"}
			}
			if cmd.Flags().Changed("layout") {
				if err := checkLayout(layout); err != nil {
					return usageError{"invalid layout: " + err.Error()}
				}
			}
			if dir != "" {
				var err error
				dir, err = filepath.Abs(dir)
				if err != nil {
					return fmt.Errorf("making dir absolute: %w", err)
				}
			}
			return addCatalogue(cmd.OutOrStdout(), args[0], args[1], dir, layout, addLast, forceUpdate)
		},
	}
	add.Flags().BoolVar(&addLast, "append", false, "use it last instead of first")
	add.Flags().BoolVar(&forceUpdate, "force-update", false, "replace a catalogue with this name")
	add.Flags().StringVar(&dir, "dir", "", "read-only container directory")
	add.Flags().StringVar(&layout, "layout", "", "container path template in the directory")

	var useLast bool
	use := &cobra.Command{
		Use:   "use <name>",
		Short: "Use a catalogue first",
		Long: "Use a catalogue first, or last with --append. Of the catalogues in use, the first\n" +
			"listing a name wins. Unlike module use, the change is saved until changed again.",
		Args:              exactArgs(1),
		ValidArgsFunction: completeCatalogueNames,
		RunE: func(cmd *cobra.Command, args []string) error {
			return useCatalogue(cmd.OutOrStdout(), args[0], useLast)
		},
	}
	use.Flags().BoolVar(&useLast, "append", false, "use it last instead of first")

	cmd.AddCommand(
		add,
		&cobra.Command{
			Use:               "remove <name>",
			Short:             "Remove a catalogue and its downloaded copy",
			Args:              exactArgs(1),
			ValidArgsFunction: completeCatalogueNames,
			RunE: func(cmd *cobra.Command, args []string) error {
				return removeCatalogue(cmd.OutOrStdout(), args[0])
			},
		},
		use,
		&cobra.Command{
			Use:               "unuse <name>",
			Short:             "Stop using a catalogue, keeping it registered",
			Args:              exactArgs(1),
			ValidArgsFunction: completeCatalogueNames,
			RunE: func(cmd *cobra.Command, args []string) error {
				return unuseCatalogue(cmd.OutOrStdout(), args[0])
			},
		},
		&cobra.Command{
			Use:   "list",
			Short: "List every catalogue and its order",
			Args:  noArgs,
			RunE:  runCatalogueList,
		},
	)
	return cmd
}

func runCatalogueList(cmd *cobra.Command, args []string) error {
	_, _, catalogues, err := readCatalogues()
	if err != nil {
		return err
	}
	out := cmd.OutOrStdout()
	return writeCatalogueTable(out, catalogues, isTerminal(out))
}

// addCatalogue registers a catalogue and uses it first, or last if last is
// set. Changing its URL, dir or layout requires forceUpdate. Only a URL
// change removes the old cache directory.
func addCatalogue(w io.Writer, name, url, dir, layout string, last, forceUpdate bool) error {
	cacheDir, path, catalogues, err := readCatalogues()
	if err != nil {
		return err
	}
	if err := checkCatalogueName(name); err != nil {
		return usageError{fmt.Sprintf("invalid catalogue name %q: %s", name, err)}
	}
	if err := checkCatalogueURL(url); err != nil {
		return usageError{"invalid catalogue URL: " + err.Error()}
	}

	oldDir := ""
	if i := findCatalogue(catalogues, name); i >= 0 {
		old := catalogues[i]
		if old.URL == url && old.Dir == dir && old.Layout == layout {
			_, err := fmt.Fprintf(w, "Catalogue %q already exists with the same URL, dir and layout; nothing to do.\n", name)
			return err
		}
		if !forceUpdate {
			return fmt.Errorf("catalogue %q already exists with different URL, dir or layout; use --force-update to replace it, or choose another name",
				name)
		}
		if old.URL != url {
			oldDir = old.dir(cacheDir)
		}
		catalogues = slices.Delete(catalogues, i, i+1)
	}
	catalogues = placeCatalogue(catalogues, catalogue{Name: name, URL: url, Use: true, Dir: dir, Layout: layout}, last)
	if err := writeCatalogueList(path, catalogues); err != nil {
		return err
	}
	// Only once the list is written, so a failed write loses nothing.
	if oldDir != "" {
		if err := os.RemoveAll(oldDir); err != nil {
			return fmt.Errorf("removing catalogue cache: %w", err)
		}
	}
	_, err = fmt.Fprintf(w, "Catalogue %q added at the %s. Run 'sciget refresh' to download it.\n",
		name, topOrBottom(last))
	return err
}

// removeCatalogue unregisters a catalogue and deletes its cache directory.
func removeCatalogue(w io.Writer, name string) error {
	cacheDir, path, catalogues, err := readCatalogues()
	if err != nil {
		return err
	}
	i := findCatalogue(catalogues, name)
	if i < 0 {
		return fmt.Errorf("no catalogue named %q", name)
	}
	dir := catalogues[i].dir(cacheDir)
	if err := writeCatalogueList(path, slices.Delete(catalogues, i, i+1)); err != nil {
		return err
	}
	if err := os.RemoveAll(dir); err != nil {
		return fmt.Errorf("removing catalogue cache: %w", err)
	}
	_, err = fmt.Fprintf(w, "Catalogue %q removed.\n", name)
	return err
}

// useCatalogue puts a catalogue in use and moves it first, or last if last is
// set.
func useCatalogue(w io.Writer, name string, last bool) error {
	_, path, catalogues, err := readCatalogues()
	if err != nil {
		return err
	}
	i := findCatalogue(catalogues, name)
	if i < 0 {
		return fmt.Errorf("no catalogue named %q", name)
	}
	c := catalogues[i]
	c.Use = true
	catalogues = placeCatalogue(slices.Delete(catalogues, i, i+1), c, last)
	if err := writeCatalogueList(path, catalogues); err != nil {
		return err
	}
	_, err = fmt.Fprintf(w, "Catalogue %q in use at the %s. Run 'sciget refresh' to update it.\n",
		name, topOrBottom(last))
	return err
}

// unuseCatalogue stops using a catalogue. Its position and cache are kept.
func unuseCatalogue(w io.Writer, name string) error {
	_, path, catalogues, err := readCatalogues()
	if err != nil {
		return err
	}
	i := findCatalogue(catalogues, name)
	if i < 0 {
		return fmt.Errorf("no catalogue named %q", name)
	}
	catalogues[i].Use = false
	if err := writeCatalogueList(path, catalogues); err != nil {
		return err
	}
	_, err = fmt.Fprintf(w, "Catalogue %q no longer in use.\n", name)
	return err
}

func topOrBottom(last bool) string {
	if last {
		return "bottom"
	}
	return "top"
}

// completeCatalogueNames suggests registered catalogue names for the first
// argument.
func completeCatalogueNames(cmd *cobra.Command, args []string, toComplete string) ([]string, cobra.ShellCompDirective) {
	if len(args) > 0 {
		return nil, cobra.ShellCompDirectiveNoFileComp
	}
	_, _, catalogues, err := readCatalogues()
	if err != nil {
		return nil, cobra.ShellCompDirectiveError
	}
	var names []string
	for _, c := range catalogues {
		if strings.HasPrefix(c.Name, toComplete) {
			names = append(names, c.Name)
		}
	}
	return names, cobra.ShellCompDirectiveNoFileComp
}
