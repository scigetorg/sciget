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

	var setSystem bool
	set := &cobra.Command{
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
			c, err := readConfigFor(setSystem)
			if err != nil {
				return err
			}
			return c.set(args[0], args[1])
		},
	}
	set.Flags().BoolVar(&setSystem, "system", false, systemFlagUsage)

	var unsetSystem bool
	unset := &cobra.Command{
		Use:               "unset <key>",
		Short:             "Remove a setting from your config",
		Args:              exactArgs(1),
		ValidArgsFunction: completeConfigKeys,
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := readConfigFor(unsetSystem)
			if err != nil {
				return err
			}
			return c.unset(args[0])
		},
	}
	unset.Flags().BoolVar(&unsetSystem, "system", false, systemFlagUsage)

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
		set,
		unset,
		&cobra.Command{
			Use:   "list",
			Short: "List every setting with its value and source",
			Args:  noArgs,
			RunE:  runConfigList,
		},
	)
	return cmd
}

// readConfigFor reads the user's config, or with system set the system
// file alone.
func readConfigFor(system bool) (config, error) {
	if system {
		return readSystemConfig()
	}
	return readConfig()
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
