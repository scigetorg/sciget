package main

import (
	"fmt"
	"io"
	"strings"

	"github.com/spf13/cobra"
)

func newFindCommand() *cobra.Command {
	var all bool
	cmd := &cobra.Command{
		Use:   "find [words...]",
		Short: "List the containers in the catalogs in use",
		RunE: func(cmd *cobra.Command, args []string) error {
			containers, err := readContainers()
			if err != nil {
				return err
			}
			matches := matchContainers(containers, args, all)
			if len(matches) == 0 {
				_, err := fmt.Fprintf(cmd.ErrOrStderr(), "No containers match %q.\n", strings.Join(args, " "))
				return err
			}
			out := cmd.OutOrStdout()
			return writeContainers(out, matches, isTerminal(out))
		},
		ValidArgsFunction: func(cmd *cobra.Command, args []string, toComplete string) ([]string, cobra.ShellCompDirective) {
			containers, err := readContainers()
			if err != nil {
				return nil, cobra.ShellCompDirectiveError
			}
			return completeNames(containers, toComplete, all)
		},
	}
	cmd.Flags().BoolVar(&all, "all", false, "include hidden containers")
	return cmd
}

// matchContainers returns the containers matching every word (see
// matches). Hidden containers are left out unless all is set.
func matchContainers(containers []container, words []string, all bool) []container {
	var found []container
	for _, c := range containers {
		if (!c.hidden || all) && matches(c, words) {
			found = append(found, c)
		}
	}
	return found
}

// matches reports whether every word appears, ignoring case, in c's name
// or one of its categories. find and the TUI share it.
func matches(c container, words []string) bool {
	text := strings.ToLower(c.name() + " " + strings.Join(c.categories, " "))
	for _, w := range words {
		if !strings.Contains(text, strings.ToLower(w)) {
			return false
		}
	}
	return true
}

// writeContainers prints one line per container, noting those that are
// shadowed. For a terminal it adds a header and aligns the columns; otherwise
// it writes tab-separated values.
func writeContainers(w io.Writer, containers []container, terminal bool) error {
	var rows strings.Builder
	for _, c := range containers {
		gui := ""
		if c.graphical {
			gui = "yes"
		}
		note := ""
		if c.shadowed {
			note = "(shadowed)"
		}
		fmt.Fprintf(&rows, "%s\t%s\t%s\t%s\t%s\n",
			c.name(), c.catalog, strings.Join(c.categories, ", "), gui, note)
	}
	return writeTable(w, terminal, "NAME\tCATALOG\tCATEGORIES\tGUI\tNOTE\n", rows.String())
}

// completeNames suggests container names in two stages: without a "/", tool
// names ending in "/"; with one, that tool's names, each described by its
// build date. Shadowed containers are never suggested, and hidden ones only
// if all is set.
func completeNames(containers []container, toComplete string, all bool) ([]string, cobra.ShellCompDirective) {
	tool, _, hasSlash := strings.Cut(toComplete, "/")
	var suggestions []string
	for _, c := range containers {
		if c.shadowed || c.hidden && !all {
			continue
		}
		switch {
		case !hasSlash && strings.HasPrefix(c.tool, toComplete):
			// containers is sorted by tool, so repeats are adjacent.
			if len(suggestions) == 0 || suggestions[len(suggestions)-1] != c.tool+"/" {
				suggestions = append(suggestions, c.tool+"/")
			}
		case hasSlash && c.tool == tool && strings.HasPrefix(c.name(), toComplete):
			suggestions = append(suggestions, c.name()+"\tbuilt "+c.buildDate)
		}
	}
	if !hasSlash {
		return suggestions, cobra.ShellCompDirectiveNoSpace | cobra.ShellCompDirectiveNoFileComp
	}
	return suggestions, cobra.ShellCompDirectiveNoFileComp
}
