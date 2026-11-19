package main

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"
)

// problem is something doctor found, with the shell lines that fix it.
type problem struct {
	text string
	fix  []string
}

// findProblems returns the problems doctor reports. It changes nothing.
func findProblems(cfg config) ([]problem, error) {
	dir, _, err := cfg.get("container_dir")
	if err != nil {
		return nil, err
	}
	layout, _, err := cfg.get("container_layout")
	if err != nil {
		return nil, err
	}
	// Where pull writes its temporary files, whatever the layout. The
	// search runs inside dir, so dir's own name is never read as a pattern.
	pattern := strings.NewReplacer("{tool}", "*", "{version}", "*", "{date}", "*").Replace(layout)
	matches, err := fs.Glob(os.DirFS(dir), pattern+".*.incomplete")
	if err != nil {
		return nil, err
	}
	var problems []problem
	for _, match := range matches {
		path := filepath.Join(dir, filepath.FromSlash(match))
		info, err := os.Lstat(path)
		if err != nil {
			continue
		}
		problems = append(problems, problem{
			text: fmt.Sprintf("A download that was stopped left %s (%s).", path, humanSize(info.Size())),
			fix:  []string{shellJoin([]string{"rm", "--", path})},
		})
	}
	layerProblems, err := findLayerProblems(cfg)
	if err != nil {
		return nil, err
	}
	return append(problems, layerProblems...), nil
}

// findLayerProblems reports the user's entries that stop the system
// layer's changes reaching them: a catalog hiding a system catalog of the
// same name, and a setting equal to the system file's (most likely a
// copy).
func findLayerProblems(cfg config) ([]problem, error) {
	l, err := readCatalogLayers()
	if err != nil {
		return nil, err
	}
	var problems []problem
	for _, c := range l.user {
		if i := findCatalog(l.system, c.Name); i >= 0 && c.URL != "" {
			problems = append(problems, problem{
				text: fmt.Sprintf("Your catalog %s hides the system catalog of the same name (%s).", c.Name, l.system[i].URL),
				fix:  []string{shellJoin([]string{"sciget", "catalog", "remove", c.Name})},
			})
		}
	}
	for _, key := range configKeys {
		value, ok := cfg.overrides[key.name]
		if system, inSystem := cfg.system[key.name]; ok && inSystem && value == system {
			problems = append(problems, problem{
				text: fmt.Sprintf("Your config sets %s to the same value as %s, so later changes there will not reach you.",
					key.name, systemPath("config.json")),
				fix: []string{shellJoin([]string{"sciget", "config", "unset", key.name})},
			})
		}
	}
	return problems, nil
}

func newDoctorCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "doctor",
		Short: "Report problems and how to fix them",
		Long: "Report problems and how to fix them. Doctor changes nothing: " +
			"it prints the commands, and you run them.",
		Args: noArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := readConfig()
			if err != nil {
				return err
			}
			problems, err := findProblems(cfg)
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			if len(problems) == 0 {
				_, err = fmt.Fprintln(out, "No problems found.")
				return err
			}
			for _, p := range problems {
				// Paths come from the file system, so they are made safe to draw.
				text := printable(p.text) + " Fix it with:\n"
				for _, line := range p.fix {
					text += "  " + printable(line) + "\n"
				}
				if _, err := fmt.Fprint(out, text); err != nil {
					return err
				}
			}
			return nil
		},
	}
}
