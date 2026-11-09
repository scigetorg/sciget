// Sciget installs containerised scientific software as desktop launchers and
// runs the containers through Apptainer.
package main

import (
	"errors"
	"fmt"
	"io"
	"os"
)

// version is replaced at build time with -ldflags "-X main.version=v1.2.3".
var version = "dev"

const usage = `sciget installs and runs containerised scientific software.

Usage:
  sciget <command> [arguments]

Commands:
  help        Show this help
  version     Print the version
`

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

// run is main without the process exit, so tests can call it.
func run(args []string, stdout, stderr io.Writer) int {
	return exitCode(dispatch(args, stdout), stderr)
}

// exitCode prints err to stderr and returns 0 on success, 2 for a usage
// error and 1 for any other failure.
func exitCode(err error, stderr io.Writer) int {
	if err == nil {
		return 0
	}

	var usageErr usageError
	if errors.As(err, &usageErr) {
		fmt.Fprintf(stderr, "sciget: %s\nRun 'sciget help' for usage.\n", err)
		return 2
	}

	fmt.Fprintf(stderr, "sciget: %s\n", err)
	return 1
}

// dispatch picks the command named by args[0] and runs it.
func dispatch(args []string, stdout io.Writer) error {
	if len(args) == 0 {
		return usageError{"no command given"}
	}

	switch args[0] {
	case "help", "-h", "--help":
		_, err := io.WriteString(stdout, usage)
		return err
	case "version", "-version", "--version":
		_, err := fmt.Fprintf(stdout, "sciget %s\n", version)
		return err
	default:
		return usageError{fmt.Sprintf("unknown command %q", args[0])}
	}
}

// usageError marks an invocation mistake, which exits with status 2.
type usageError struct{ msg string }

func (e usageError) Error() string { return e.msg }
