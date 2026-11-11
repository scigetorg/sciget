package main

import (
	"io"
	"os"
	"text/tabwriter"

	"golang.org/x/term"
)

// writeTable writes rows of tab-separated columns. On a terminal it adds the
// header and aligns the columns; otherwise it writes the rows as they are,
// for scripts.
func writeTable(w io.Writer, terminal bool, header, rows string) error {
	if !terminal {
		_, err := io.WriteString(w, rows)
		return err
	}
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	if _, err := io.WriteString(tw, header+rows); err != nil {
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
