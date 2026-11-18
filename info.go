package main

import (
	"errors"
	"fmt"
	"strings"

	"github.com/spf13/cobra"
)

// containerInfo holds the display values for one container.
type containerInfo struct {
	name, catalog, build, categories, licence, doi, gui, location string
}

// readContainerInfo returns a winner's metadata and local location.
func readContainerInfo(name string) (containerInfo, error) {
	containers, err := readContainers()
	if err != nil {
		return containerInfo{}, err
	}
	c, err := lookupContainer(containers, name)
	if err != nil {
		return containerInfo{}, err
	}
	info := containerInfo{
		name: c.name(), catalog: c.catalog,
		build:      fmt.Sprintf("%s (%s)", c.buildDate, c.fileName()),
		categories: strings.Join(c.categories, ", "),
		licence:    strings.Join(c.licences, ", "), doi: strings.Join(c.dois, ", "), gui: "no",
	}
	var shadowed []string
	for _, other := range containers {
		if other.name() == name && other.shadowed {
			shadowed = append(shadowed, other.catalog)
		}
	}
	if len(shadowed) > 0 {
		info.catalog += " (also in: " + strings.Join(shadowed, ", ") + ", shadowed)"
	}
	if info.categories == "" {
		info.categories = "(none)"
	}
	if info.licence == "" {
		info.licence = "(not stated)"
	}
	if info.doi == "" {
		info.doi = "(none)"
	}
	if c.graphical {
		info.gui = "yes"
	}
	cfg, err := readConfig()
	if err != nil {
		return containerInfo{}, err
	}
	loc, err := locate(cfg, c)
	var missing notOnMachineError
	switch {
	case errors.As(err, &missing):
		info.location = "not on this machine"
	case err != nil:
		return containerInfo{}, err
	case loc.older:
		info.location = loc.path + " (an older build)"
	default:
		info.location = loc.path
	}
	return info, nil
}

func newInfoCommand() *cobra.Command {
	return &cobra.Command{
		Use:               "info <tool/version>",
		Short:             "Show a container's details and location",
		Args:              exactArgs(1),
		ValidArgsFunction: completeContainerNames,
		RunE: func(cmd *cobra.Command, args []string) error {
			info, err := readContainerInfo(args[0])
			if err != nil {
				return err
			}
			_, err = fmt.Fprintf(cmd.OutOrStdout(),
				"%-13s%s\n%-13s%s\n%-13s%s\n%-13s%s\n%-13s%s\n%-13s%s\n%-13s%s\n%-13s%s\n",
				"Name:", info.name, "Catalog:", info.catalog, "Build:", info.build,
				"Categories:", info.categories, "Licence:", info.licence, "DOI:", info.doi,
				"GUI:", info.gui, "Location:", info.location)
			return err
		},
	}
}
