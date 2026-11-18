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

// newCatalogCommand builds "sciget catalog", which lists the catalogs,
// and its subcommands.
func newCatalogCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "catalog",
		Short: "Add, use and list catalogs",
		Args:  noArgs,
		RunE:  runCatalogList,
	}

	var addLast, forceUpdate bool
	var dir, layout string
	add := &cobra.Command{
		Use:               "add <name> <url>",
		Short:             "Add a catalog and use it first",
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
			c := catalog{Name: args[0], URL: args[1], Use: true, Dir: dir, Layout: layout}
			return addCatalog(cmd.OutOrStdout(), c, addLast, forceUpdate)
		},
	}
	add.Flags().BoolVar(&addLast, "append", false, "use it last instead of first")
	add.Flags().BoolVar(&forceUpdate, "force-update", false, "replace a catalog with this name")
	add.Flags().StringVar(&dir, "dir", "", "read-only container directory")
	add.Flags().StringVar(&layout, "layout", "", "container path template in the directory")

	var useLast bool
	use := &cobra.Command{
		Use:   "use <name>",
		Short: "Use a catalog first",
		Long: "Use a catalog first, or last with --append. Of the catalogs in use, the first\n" +
			"listing a name wins. Unlike module use, the change is saved until changed again.",
		Args:              exactArgs(1),
		ValidArgsFunction: completeCatalogNames,
		RunE: func(cmd *cobra.Command, args []string) error {
			return useCatalog(cmd.OutOrStdout(), args[0], useLast)
		},
	}
	use.Flags().BoolVar(&useLast, "append", false, "use it last instead of first")

	cmd.AddCommand(
		add,
		&cobra.Command{
			Use:               "remove <name>",
			Short:             "Remove a catalog and its downloaded copy",
			Args:              exactArgs(1),
			ValidArgsFunction: completeCatalogNames,
			RunE: func(cmd *cobra.Command, args []string) error {
				return removeCatalog(cmd.OutOrStdout(), args[0])
			},
		},
		use,
		&cobra.Command{
			Use:               "unuse <name>",
			Short:             "Stop using a catalog, keeping it registered",
			Args:              exactArgs(1),
			ValidArgsFunction: completeCatalogNames,
			RunE: func(cmd *cobra.Command, args []string) error {
				return unuseCatalog(cmd.OutOrStdout(), args[0])
			},
		},
		&cobra.Command{
			Use:   "list",
			Short: "List every catalog and its order",
			Args:  noArgs,
			RunE:  runCatalogList,
		},
	)
	return cmd
}

func runCatalogList(cmd *cobra.Command, args []string) error {
	_, _, catalogs, err := readCatalogs()
	if err != nil {
		return err
	}
	out := cmd.OutOrStdout()
	return writeCatalogTable(out, catalogs, isTerminal(out))
}

// addCatalog registers the catalog c and uses it first, or last if last
// is set. Changing an existing one's URL, dir or layout requires
// forceUpdate. Only a URL change removes the old cache directory.
func addCatalog(w io.Writer, c catalog, last, forceUpdate bool) error {
	cacheDir, path, catalogs, err := readCatalogs()
	if err != nil {
		return err
	}
	name := c.Name
	if err := checkCatalogName(name); err != nil {
		return usageError{fmt.Sprintf("invalid catalog name %q: %s", name, err)}
	}
	if err := checkCatalogURL(c.URL); err != nil {
		return usageError{"invalid catalog URL: " + err.Error()}
	}

	oldDir := ""
	if i := findCatalog(catalogs, name); i >= 0 {
		old := catalogs[i]
		if old.URL == c.URL && old.Dir == c.Dir && old.Layout == c.Layout {
			_, err := fmt.Fprintf(w, "Catalog %q already exists with the same settings; nothing to do.\n", name)
			return err
		}
		if !forceUpdate {
			return fmt.Errorf("catalog %q already exists with different settings; use --force-update to replace it, or choose another name",
				name)
		}
		if old.URL != c.URL {
			oldDir = old.dir(cacheDir)
		}
		catalogs = slices.Delete(catalogs, i, i+1)
	}
	catalogs = placeCatalog(catalogs, c, last)
	if err := writeCatalogList(path, catalogs); err != nil {
		return err
	}
	// Only once the list is written, so a failed write loses nothing.
	if oldDir != "" {
		if err := os.RemoveAll(oldDir); err != nil {
			return fmt.Errorf("removing catalog cache: %w", err)
		}
	}
	_, err = fmt.Fprintf(w, "Catalog %q added at the %s. Run 'sciget refresh' to download it.\n",
		name, topOrBottom(last))
	return err
}

// removeCatalog unregisters a catalog and deletes its cache directory.
func removeCatalog(w io.Writer, name string) error {
	cacheDir, path, catalogs, err := readCatalogs()
	if err != nil {
		return err
	}
	i := findCatalog(catalogs, name)
	if i < 0 {
		return fmt.Errorf("no catalog named %q", name)
	}
	dir := catalogs[i].dir(cacheDir)
	if err := writeCatalogList(path, slices.Delete(catalogs, i, i+1)); err != nil {
		return err
	}
	if err := os.RemoveAll(dir); err != nil {
		return fmt.Errorf("removing catalog cache: %w", err)
	}
	_, err = fmt.Fprintf(w, "Catalog %q removed.\n", name)
	return err
}

// useCatalog puts a catalog in use and moves it first, or last if last is
// set.
func useCatalog(w io.Writer, name string, last bool) error {
	_, path, catalogs, err := readCatalogs()
	if err != nil {
		return err
	}
	i := findCatalog(catalogs, name)
	if i < 0 {
		return fmt.Errorf("no catalog named %q", name)
	}
	c := catalogs[i]
	c.Use = true
	catalogs = placeCatalog(slices.Delete(catalogs, i, i+1), c, last)
	if err := writeCatalogList(path, catalogs); err != nil {
		return err
	}
	_, err = fmt.Fprintf(w, "Catalog %q in use at the %s. Run 'sciget refresh' to update it.\n",
		name, topOrBottom(last))
	return err
}

// unuseCatalog stops using a catalog. Its position and cache are kept.
func unuseCatalog(w io.Writer, name string) error {
	_, path, catalogs, err := readCatalogs()
	if err != nil {
		return err
	}
	i := findCatalog(catalogs, name)
	if i < 0 {
		return fmt.Errorf("no catalog named %q", name)
	}
	catalogs[i].Use = false
	if err := writeCatalogList(path, catalogs); err != nil {
		return err
	}
	_, err = fmt.Fprintf(w, "Catalog %q no longer in use.\n", name)
	return err
}

func topOrBottom(last bool) string {
	if last {
		return "bottom"
	}
	return "top"
}

// completeCatalogNames suggests registered catalog names for the first
// argument.
func completeCatalogNames(cmd *cobra.Command, args []string, toComplete string) ([]string, cobra.ShellCompDirective) {
	if len(args) > 0 {
		return nil, cobra.ShellCompDirectiveNoFileComp
	}
	_, _, catalogs, err := readCatalogs()
	if err != nil {
		return nil, cobra.ShellCompDirectiveError
	}
	var names []string
	for _, c := range catalogs {
		if strings.HasPrefix(c.Name, toComplete) {
			names = append(names, c.Name)
		}
	}
	return names, cobra.ShellCompDirectiveNoFileComp
}
