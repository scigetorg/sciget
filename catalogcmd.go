package main

import (
	"errors"
	"fmt"
	"io"
	"maps"
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

	var addLast, forceUpdate, addSystem bool
	var dir, layout string
	add := &cobra.Command{
		Use:   "add <name> <url or file>",
		Short: "Add a catalog and use it first",
		Args:  exactArgs(2),
		ValidArgsFunction: func(cmd *cobra.Command, args []string, toComplete string) ([]string, cobra.ShellCompDirective) {
			if len(args) == 1 {
				return nil, cobra.ShellCompDirectiveDefault // a file
			}
			return nil, cobra.ShellCompDirectiveNoFileComp
		},
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
			link := args[1]
			if !isURL(link) {
				var err error
				if link, err = filepath.Abs(link); err != nil {
					return fmt.Errorf("making the file path absolute: %w", err)
				}
			}
			c := catalog{Name: args[0], URL: link, Use: true, Dir: dir, Layout: layout}
			return addCatalog(cmd.OutOrStdout(), c, addLast, forceUpdate, addSystem)
		},
	}
	add.Flags().BoolVar(&addLast, "append", false, "use it last instead of first")
	add.Flags().BoolVar(&forceUpdate, "force-update", false, "replace a catalog with this name")
	add.Flags().StringVar(&dir, "dir", "", "read-only container directory")
	add.Flags().StringVar(&layout, "layout", "", "container path template in the directory")
	add.Flags().BoolVar(&addSystem, "system", false, systemFlagUsage)

	var useLast, useSystem bool
	use := &cobra.Command{
		Use:   "use <name>",
		Short: "Use a catalog first",
		Long: "Use a catalog first, or last with --append. Of the catalogs in use, the first\n" +
			"listing a name wins. Unlike module use, the change is saved until changed again.\n" +
			"A system catalog keeps its place, after your own.",
		Args:              exactArgs(1),
		ValidArgsFunction: completeCatalogNames,
		RunE: func(cmd *cobra.Command, args []string) error {
			return useCatalog(cmd.OutOrStdout(), args[0], true, useLast, useSystem)
		},
	}
	use.Flags().BoolVar(&useLast, "append", false, "use it last instead of first")
	use.Flags().BoolVar(&useSystem, "system", false, systemFlagUsage)

	var removeSystem bool
	remove := &cobra.Command{
		Use:               "remove <name>",
		Short:             "Remove a catalog and its downloaded copy",
		Args:              exactArgs(1),
		ValidArgsFunction: completeCatalogNames,
		RunE: func(cmd *cobra.Command, args []string) error {
			return removeCatalog(cmd.OutOrStdout(), args[0], removeSystem)
		},
	}
	remove.Flags().BoolVar(&removeSystem, "system", false, systemFlagUsage)

	var unuseSystem bool
	unuse := &cobra.Command{
		Use:               "unuse <name>",
		Short:             "Stop using a catalog, keeping it registered",
		Args:              exactArgs(1),
		ValidArgsFunction: completeCatalogNames,
		RunE: func(cmd *cobra.Command, args []string) error {
			return useCatalog(cmd.OutOrStdout(), args[0], false, false, unuseSystem)
		},
	}
	unuse.Flags().BoolVar(&unuseSystem, "system", false, systemFlagUsage)

	cmd.AddCommand(
		add,
		remove,
		use,
		unuse,
		&cobra.Command{
			Use:   "list",
			Short: "List every catalog and its order",
			Args:  noArgs,
			RunE:  runCatalogList,
		},
	)
	return cmd
}

// systemFlagUsage is the help for every --system flag.
const systemFlagUsage = "change the system file in /etc/sciget, for every user (needs root)"

func runCatalogList(cmd *cobra.Command, args []string) error {
	_, catalogs, err := readCatalogs()
	if err != nil {
		return err
	}
	out := cmd.OutOrStdout()
	return writeCatalogTable(out, catalogs, isTerminal(out))
}

// catalogFile is the catalogs.json a command changes: the user's, or with
// --system the system's.
type catalogFile struct {
	path     string
	catalogs []catalog
	system   bool
	layers   catalogLayers // both layers, for the user's file; unset for the system's
}

// readCatalogFile reads the file a command changes. With system set it
// reads the system file alone, so no user file is involved.
func readCatalogFile(system bool) (catalogFile, error) {
	if system {
		path, catalogs, err := readSystemCatalogs()
		if err == nil && path == "" {
			err = errors.New("this system has no system folder")
		}
		if err != nil {
			return catalogFile{}, err
		}
		return catalogFile{path: path, catalogs: catalogs, system: true}, nil
	}
	l, err := readCatalogLayers()
	if err != nil {
		return catalogFile{}, err
	}
	return catalogFile{path: l.userPath, catalogs: l.user, layers: l}, nil
}

func (f catalogFile) write() error {
	return writeCatalogList(f.path, f.catalogs, f.system)
}

// systemCatalog returns the system catalog called name, when f is the
// user's file and the system has one.
func (f catalogFile) systemCatalog(name string) (catalog, bool) {
	i := findCatalog(f.layers.system, name)
	if f.system || i < 0 {
		return catalog{}, false
	}
	return f.layers.system[i], true
}

// addCatalog registers the catalog c and uses it first, or last if last
// is set. It reads c's file first: a domains file adds one catalog per
// key, named "<name>-<key>". Changing an existing catalog's settings
// requires forceUpdate, and replacing its source removes its old cache
// directory, in the user's file only. A user may not add a system
// catalog's name. If any catalog cannot be added, none is.
func addCatalog(w io.Writer, c catalog, last, forceUpdate, system bool) error {
	f, err := readCatalogFile(system)
	if err != nil {
		return err
	}
	if err := checkCatalogName(c.Name); err != nil {
		return usageError{fmt.Sprintf("invalid catalog name %q: %s", c.Name, err)}
	}
	if err := checkLink(c.URL); err != nil {
		return usageError{"invalid catalog URL: " + err.Error()}
	}
	catalogs, empty, err := newCatalogs(c, system)
	if err != nil {
		return err
	}
	if !last {
		// Each is placed first in turn, so place them in reverse.
		slices.Reverse(catalogs)
	}

	var added, same, oldDirs []string
	for _, c := range catalogs {
		if s, ok := f.systemCatalog(c.Name); ok {
			if sameSettings(s, c) {
				same = append(same, c.Name)
				continue
			}
			return fmt.Errorf("catalog %q is set by %s; choose another name", c.Name, f.layers.systemPath)
		}
		if i := findCatalog(f.catalogs, c.Name); i >= 0 {
			old := f.catalogs[i]
			if sameSettings(old, c) {
				same = append(same, c.Name)
				continue
			}
			// An entry with no URL is left from a system catalog that has gone.
			if old.URL != "" && !forceUpdate {
				return fmt.Errorf("catalog %q already exists with different settings; use --force-update to replace it, or choose another name",
					c.Name)
			}
			if old.URL != "" && !system && old.dir(f.layers.cacheDir) != c.dir(f.layers.cacheDir) {
				oldDirs = append(oldDirs, old.dir(f.layers.cacheDir))
			}
			f.catalogs = slices.Delete(f.catalogs, i, i+1)
		}
		f.catalogs = placeCatalog(f.catalogs, c, last)
		added = append(added, c.Name)
	}
	if len(added) > 0 {
		if err := f.write(); err != nil {
			return err
		}
	}
	// Only once the list is written, so a failed write loses nothing.
	for _, dir := range oldDirs {
		if err := removeCatalogCache(dir); err != nil {
			return fmt.Errorf("removing catalog cache: %w", err)
		}
	}

	slices.Sort(same)
	for _, name := range same {
		if _, err := fmt.Fprintf(w, "Catalog %q already exists with the same settings; nothing to do.\n", name); err != nil {
			return err
		}
	}
	slices.Sort(added)
	for i, name := range added {
		hint := ""
		if slices.Contains(empty, name) {
			hint = " It has no apps list yet."
		}
		if i == len(added)-1 {
			if len(added) > 1 {
				hint += " Run 'sciget refresh' to download them."
			} else {
				hint += " Run 'sciget refresh' to download it."
			}
		}
		if _, err := fmt.Fprintf(w, "Catalog %q added at the %s.%s\n", name, topOrBottom(last), hint); err != nil {
			return err
		}
	}
	return nil
}

// newCatalogs reads the file c names and returns the catalogs adding it
// makes: c itself for a catalog JSON, or for a domains file one per key,
// in key order, and the names of those that are empty catalogs. It checks
// each catalog JSON as refresh would. With system set, it reads with the
// system's settings alone.
func newCatalogs(c catalog, system bool) (catalogs []catalog, empty []string, err error) {
	cfg, err := readConfigFor(system)
	if err != nil {
		return nil, nil, err
	}
	maxSize, err := cfg.getNumber("max_catalog_size")
	if err != nil {
		return nil, nil, err
	}
	client, err := newHTTPClient(cfg)
	if err != nil {
		return nil, nil, err
	}
	client.CheckRedirect = httpsOnly
	data, base, err := readLink(client, c.URL, maxSize)
	if err != nil {
		return nil, nil, err
	}
	if !isDomainsFile(data) {
		_, apps, err := checkCatalogJSON(data, base)
		if err != nil {
			return nil, nil, fmt.Errorf("%s is not a valid catalog JSON: %w", c.URL, err)
		}
		eessiMeta, _ := parseCatalogEESSI(data)
		if apps == "" && eessiMeta.EESSICatalogURL == "" {
			empty = []string{c.Name}
		}
		return []catalog{c}, empty, nil
	}
	parts, err := splitDomains(data)
	if err != nil {
		return nil, nil, fmt.Errorf("%s is not a valid domains file: %w", c.URL, err)
	}
	for _, key := range slices.Sorted(maps.Keys(parts)) {
		d := c
		d.Name, d.Domain = c.Name+"-"+key, key
		if err := checkCatalogName(key); err != nil {
			return nil, nil, fmt.Errorf("domain %q in %s is not a valid catalog name: %w", key, c.URL, err)
		}
		_, apps, err := checkCatalogJSON(parts[key], base)
		if err != nil {
			return nil, nil, fmt.Errorf("domain %q in %s: %w", key, c.URL, err)
		}
		eessiMeta, _ := parseCatalogEESSI(parts[key])
		if apps == "" && eessiMeta.EESSICatalogURL == "" {
			empty = append(empty, d.Name)
		}
		catalogs = append(catalogs, d)
	}
	return catalogs, empty, nil
}

// removeCatalog unregisters a catalog. In the user's file it also deletes
// the catalog's cache directory, unless a system catalog shares it; removing
// a user entry for a system catalog shows the system's again. A user may
// not remove a system catalog.
func removeCatalog(w io.Writer, name string, system bool) error {
	f, err := readCatalogFile(system)
	if err != nil {
		return err
	}
	i := findCatalog(f.catalogs, name)
	if i < 0 {
		if _, ok := f.systemCatalog(name); ok {
			return fmt.Errorf("catalog %q is set by %s; an admin can change it with --system", name, f.layers.systemPath)
		}
		return fmt.Errorf("no catalog named %q", name)
	}
	c := f.catalogs[i]
	f.catalogs = slices.Delete(f.catalogs, i, i+1)
	if err := f.write(); err != nil {
		return err
	}
	if s, shared := f.systemCatalog(name); c.URL != "" && !system && !(shared && s.URL == c.URL && s.Domain == c.Domain) {
		if err := removeCatalogCache(c.dir(f.layers.cacheDir)); err != nil {
			return fmt.Errorf("removing catalog cache: %w", err)
		}
	}
	_, err = fmt.Fprintf(w, "Catalog %q removed.\n", name)
	return err
}

// beforeRemoveCatalogCache runs immediately before removeCatalogCache acquires
// the catalog lock; tests may set it to synchronize removal races.
var beforeRemoveCatalogCache = func(dir string) {}

// removeCatalogCache deletes a catalog's cache directory under its external
// lock if no registered catalog still points to dir, and bumps the lock
// generation so any delayed refresh started before removal refuses to
// overwrite or recreate the cache.
func removeCatalogCache(dir string) error {
	beforeRemoveCatalogCache(dir)
	lk, err := lockCatalogDirForRemove(dir)
	if err != nil {
		return err
	}
	defer lk.unlock()
	registered, err := catalogDirRegistered(dir)
	if err != nil {
		return err
	}
	if registered {
		return nil
	}
	if err := lk.bump(); err != nil {
		return err
	}
	return os.RemoveAll(dir)
}

// useCatalog puts a catalog in use, moving it first or (last) last, or with
// use unset stops using it, keeping its position and cache. For a system
// catalog, the user's file records only whether it is in use: its position
// stays the system's.
func useCatalog(w io.Writer, name string, use, last, system bool) error {
	f, err := readCatalogFile(system)
	if err != nil {
		return err
	}
	i := findCatalog(f.catalogs, name)
	if _, ok := f.systemCatalog(name); ok && (i < 0 || f.catalogs[i].URL == "") {
		if last {
			return usageError{fmt.Sprintf("catalog %q keeps the place %s gives it; --append cannot move it", name, f.layers.systemPath)}
		}
		if i < 0 {
			f.catalogs = append(f.catalogs, catalog{Name: name})
			i = len(f.catalogs) - 1
		}
		f.catalogs[i].Use = use
		if err := f.write(); err != nil {
			return err
		}
		return printUse(w, name, use, "")
	}
	if i < 0 || f.catalogs[i].URL == "" {
		return fmt.Errorf("no catalog named %q", name)
	}
	c := f.catalogs[i]
	c.Use = use
	if use {
		f.catalogs = placeCatalog(slices.Delete(f.catalogs, i, i+1), c, last)
	} else {
		f.catalogs[i] = c
	}
	if err := f.write(); err != nil {
		return err
	}
	return printUse(w, name, use, " at the "+topOrBottom(last))
}

// printUse reports a use or unuse; where is " at the top" or similar, or "".
func printUse(w io.Writer, name string, use bool, where string) error {
	if !use {
		_, err := fmt.Fprintf(w, "Catalog %q no longer in use.\n", name)
		return err
	}
	_, err := fmt.Fprintf(w, "Catalog %q in use%s. Run 'sciget refresh' to update it.\n", name, where)
	return err
}

func topOrBottom(last bool) string {
	if last {
		return "bottom"
	}
	return "top"
}

// completeCatalogNames suggests, for the first argument, the catalog names
// the command can change: with --system the system's; for remove the
// user's own entries; otherwise every catalog.
func completeCatalogNames(cmd *cobra.Command, args []string, toComplete string) ([]string, cobra.ShellCompDirective) {
	if len(args) > 0 {
		return nil, cobra.ShellCompDirectiveNoFileComp
	}
	system, _ := cmd.Flags().GetBool("system")
	var catalogs []catalog
	if system {
		_, list, err := readSystemCatalogs()
		if err != nil {
			return nil, cobra.ShellCompDirectiveError
		}
		catalogs = list
	} else {
		l, err := readCatalogLayers()
		if err != nil {
			return nil, cobra.ShellCompDirectiveError
		}
		catalogs = l.combined()
		if cmd.Name() == "remove" {
			catalogs = l.user
		}
	}
	var names []string
	for _, c := range catalogs {
		if strings.HasPrefix(c.Name, toComplete) {
			names = append(names, c.Name)
		}
	}
	return names, cobra.ShellCompDirectiveNoFileComp
}
