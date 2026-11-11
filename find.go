package main

import (
	"fmt"
	"io"
	"os"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"
	"golang.org/x/term"
)

func newFindCommand() *cobra.Command {
	var all bool
	cmd := &cobra.Command{
		Use:   "find [words...]",
		Short: "List the containers in the catalogue",
		RunE: func(cmd *cobra.Command, args []string) error {
			containers, err := readCatalogue()
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			return writeContainers(out, matchContainers(containers, args, all), isTerminal(out))
		},
		ValidArgsFunction: func(cmd *cobra.Command, args []string, toComplete string) ([]string, cobra.ShellCompDirective) {
			containers, err := readCatalogue()
			if err != nil {
				return nil, cobra.ShellCompDirectiveError
			}
			return completeNames(containers, toComplete)
		},
	}
	cmd.Flags().BoolVar(&all, "all", false, "include hidden containers")
	return cmd
}

// matchContainers returns the containers whose name contains every word,
// ignoring case. Hidden containers are left out unless all is set.
func matchContainers(containers []container, words []string, all bool) []container {
	var found []container
	for _, c := range containers {
		if c.hidden && !all {
			continue
		}
		name := strings.ToLower(c.name())
		matches := true
		for _, w := range words {
			if !strings.Contains(name, strings.ToLower(w)) {
				matches = false
				break
			}
		}
		if matches {
			found = append(found, c)
		}
	}
	return found
}

// writeContainers prints one line per container. For a terminal it adds a
// header and aligns the columns; otherwise it writes tab-separated values.
func writeContainers(w io.Writer, containers []container, terminal bool) error {
	var b strings.Builder
	if terminal {
		b.WriteString("NAME\tCATEGORIES\tGUI\n")
	}
	for _, c := range containers {
		gui := ""
		if c.graphical {
			gui = "yes"
		}
		fmt.Fprintf(&b, "%s\t%s\t%s\n", c.name(), strings.Join(c.categories, ", "), gui)
	}

	if !terminal {
		_, err := io.WriteString(w, b.String())
		return err
	}
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	if _, err := io.WriteString(tw, b.String()); err != nil {
		return err
	}
	return tw.Flush()
}

// isTerminal reports whether w is a terminal.
func isTerminal(w io.Writer) bool {
	f, ok := w.(*os.File)
	if !ok {
		return false
	}
	return term.IsTerminal(int(f.Fd()))
}

// completeNames suggests container names in two stages: without a "/", tool
// names ending in "/"; with one, that tool's names, each described by its
// build date. Hidden containers are not suggested.
func completeNames(containers []container, toComplete string) ([]string, cobra.ShellCompDirective) {
	tool, _, hasSlash := strings.Cut(toComplete, "/")
	var suggestions []string
	for _, c := range containers {
		if c.hidden {
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
