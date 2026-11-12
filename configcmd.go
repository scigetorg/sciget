package main

import (
	"fmt"
	"strings"

	"github.com/spf13/cobra"
)

// newConfigCommand builds "sciget config", which lists the settings, and its
// subcommands.
func newConfigCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "config",
		Short: "Show and change settings",
		Args:  noArgs,
		RunE:  runConfigList,
	}
	cmd.AddCommand(
		&cobra.Command{
			Use:               "get <key>",
			Short:             "Print the value of a setting",
			Args:              exactArgs(1),
			ValidArgsFunction: completeConfigKeys,
			RunE: func(cmd *cobra.Command, args []string) error {
				c, err := readConfig()
				if err != nil {
					return err
				}
				if _, err := findConfigKey(args[0]); err != nil {
					return usageError{err.Error()}
				}
				value, _, err := c.get(args[0])
				if err != nil {
					return err
				}
				_, err = fmt.Fprintln(cmd.OutOrStdout(), value)
				return err
			},
		},
		&cobra.Command{
			Use:   "set <key> <value>",
			Short: "Change a setting",
			Args:  exactArgs(2),
			ValidArgsFunction: func(cmd *cobra.Command, args []string, toComplete string) ([]string, cobra.ShellCompDirective) {
				if len(args) == 1 {
					return nil, cobra.ShellCompDirectiveDefault // the value: file names
				}
				return completeConfigKeys(cmd, args, toComplete)
			},
			RunE: func(cmd *cobra.Command, args []string) error {
				c, err := readConfig()
				if err != nil {
					return err
				}
				return c.set(args[0], args[1])
			},
		},
		&cobra.Command{
			Use:               "unset <key>",
			Short:             "Return a setting to its default",
			Args:              exactArgs(1),
			ValidArgsFunction: completeConfigKeys,
			RunE: func(cmd *cobra.Command, args []string) error {
				c, err := readConfig()
				if err != nil {
					return err
				}
				return c.unset(args[0])
			},
		},
		&cobra.Command{
			Use:   "list",
			Short: "List every setting with its value and source",
			Args:  noArgs,
			RunE:  runConfigList,
		},
	)
	return cmd
}

func runConfigList(cmd *cobra.Command, args []string) error {
	c, err := readConfig()
	if err != nil {
		return err
	}
	out := cmd.OutOrStdout()
	return writeConfigList(out, c, isTerminal(out))
}

// completeConfigKeys suggests key names for the first argument.
func completeConfigKeys(cmd *cobra.Command, args []string, toComplete string) ([]string, cobra.ShellCompDirective) {
	if len(args) > 0 {
		return nil, cobra.ShellCompDirectiveNoFileComp
	}
	var names []string
	for _, key := range configKeys {
		if strings.HasPrefix(key.name, toComplete) {
			names = append(names, key.name)
		}
	}
	return names, cobra.ShellCompDirectiveNoFileComp
}
