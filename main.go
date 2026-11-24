// Sciget installs containerised scientific software as desktop launchers and
// runs the containers through Apptainer.
package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/spf13/cobra"
)

// version is replaced at build time with -ldflags "-X main.version=v1.2.3".
var version = "dev"

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

// run executes sciget with args and returns the exit code.
func run(args []string, stdout, stderr io.Writer) int {
	if args == nil {
		args = []string{} // cobra reads os.Args when given nil
	}
	root := newRootCommand()
	root.SetArgs(args)
	root.SetOut(stdout)
	root.SetErr(stderr)
	addCompletionCommand(root)
	addHelpCommand(root)
	return exitCode(root.Execute(), stderr)
}

// exitCode prints err to stderr and returns 0 on success, 2 for a usage
// error and 1 for any other failure.
func exitCode(err error, stderr io.Writer) int {
	if err == nil {
		return 0
	}

	// A joined error has one failure per line; each gets the prefix. An
	// error can quote text from outside sciget, so each line is cleaned.
	lines := strings.Split(err.Error(), "\n")
	for i, line := range lines {
		lines[i] = printable(line)
	}
	message := strings.Join(lines, "\nsciget: ")
	var usageErr usageError
	if errors.As(err, &usageErr) {
		fmt.Fprintf(stderr, "sciget: %s\nRun 'sciget help' for usage.\n", message)
		return 2
	}

	fmt.Fprintf(stderr, "sciget: %s\n", message)
	return 1
}

// newRootCommand builds the command tree. Every command is added here.
func newRootCommand() *cobra.Command {
	root := &cobra.Command{
		Use:   "sciget",
		Short: "Install and run containerised scientific software",

		Version: version,

		// Errors and usage are printed by exitCode, not cobra.
		SilenceErrors: true,
		SilenceUsage:  true,

		Args: noArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if isTerminal(os.Stdin) && isTerminal(cmd.OutOrStdout()) {
				return runTUI(cmd.OutOrStdout())
			}
			return usageError{"no command given"}
		},
	}
	root.SetVersionTemplate("sciget {{.Version}}\n")
	root.SetFlagErrorFunc(func(cmd *cobra.Command, err error) error {
		return usageError{err.Error()}
	})

	root.AddCommand(newVersionCommand())
	root.AddCommand(newFindCommand())
	root.AddCommand(newConfigCommand())
	root.AddCommand(newRefreshCommand())
	root.AddCommand(newCatalogCommand())
	root.AddCommand(newInfoCommand())
	root.AddCommand(newPullCommand())
	root.AddCommand(newInstallCommand())
	root.AddCommand(newDoctorCommand())
	root.AddCommand(newApptainerCommand())
	root.AddCommand(newEESSICommand())
	root.AddCommand(newUICommand())
	return root
}

// addCompletionCommand adds cobra's completion command, with noArgs on it and
// its shell subcommands. It must run after SetOut, which the command captures.
func addCompletionCommand(root *cobra.Command) {
	root.InitDefaultCompletionCmd()
	for _, cmd := range root.Commands() {
		if cmd.Name() == "completion" {
			cmd.Args = noArgs
			for _, shell := range cmd.Commands() {
				shell.Args = noArgs
			}
		}
	}
}

// addHelpCommand adds cobra's help command. For an unknown topic it prints
// "Unknown help topic" and the usage to stderr.
func addHelpCommand(root *cobra.Command) {
	root.InitDefaultHelpCmd()
	for _, cmd := range root.Commands() {
		if cmd.Name() == "help" {
			cobraRun := cmd.Run
			cmd.Run = func(cmd *cobra.Command, args []string) {
				if topic, _, _ := root.Find(args); len(args) > 0 && topic == root {
					root.InitDefaultHelpFlag() // the usage lists these flags
					root.InitDefaultVersionFlag()
					fmt.Fprintf(cmd.ErrOrStderr(), "Unknown help topic %#q\n%s", args, root.UsageString())
					return
				}
				cobraRun(cmd, args)
			}
		}
	}
}

func newVersionCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print the version",
		Args:  noArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			_, err := fmt.Fprintf(cmd.OutOrStdout(), "sciget %s\n", version)
			return err
		},
	}
}

// noArgs rejects positional arguments with a usageError.
func noArgs(cmd *cobra.Command, args []string) error {
	if len(args) > 0 {
		return usageError{fmt.Sprintf("unknown command %q for %q", args[0], cmd.CommandPath())}
	}
	return nil
}

// exactArgs requires exactly n positional arguments, returning a usageError
// otherwise.
func exactArgs(n int) cobra.PositionalArgs {
	return func(cmd *cobra.Command, args []string) error {
		if len(args) != n {
			return usageError{fmt.Sprintf("accepts %d arg(s), received %d", n, len(args))}
		}
		return nil
	}
}

// usageError marks an invocation mistake, which exits with status 2.
type usageError struct{ msg string }

func (e usageError) Error() string { return e.msg }
