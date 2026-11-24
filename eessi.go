package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"github.com/spf13/cobra"
)

var (
	eessiStackPattern   = regexp.MustCompile(`^\d{4}\.\d{2}$`)
	eessiToolIDPattern  = regexp.MustCompile(`^[a-z0-9][a-z0-9._+-]{0,63}$`)
	eessiModulePattern  = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._+-]*/[A-Za-z0-9][A-Za-z0-9._+-]*$`)
	eessiExecArgPattern = regexp.MustCompile(`^[A-Za-z0-9._/@:+=-]{1,128}$`)
	eessiRepoPattern    = regexp.MustCompile(`^[a-z0-9][a-z0-9.-]*\.[a-z]{2,}$`)
)

// eessiLaunchExec runs eessi-launch and can be replaced in tests.
var eessiLaunchExec = func(bin string, argv []string, stdin io.Reader, stdout, stderr io.Writer) error {
	cmd := exec.Command(bin, argv...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = stdin, stdout, stderr
	return cmd.Run()
}

// eessiLookupLauncher resolves the eessi-launch helper binary; replaced in tests.
var eessiLookupLauncher = func() (string, error) {
	return exec.LookPath("eessi-launch")
}

// eessiBuild is one curated EESSI Lmod module entry across catalogs in use.
type eessiBuild struct {
	id          string
	pkg         string
	label       string
	catalog     string
	stack       string
	module      string
	version     string
	toolchain   string
	arch        []string
	gpu         bool
	gpuTargets  []string
	licences    []string
	categories  []string
	description string
	homepage    string
	exec        []string
	gui         bool
	terminal    bool
	hidden      bool
	shadowed    bool
}

func (b eessiBuild) name() string { return b.id + "/" + b.version }

func (b eessiBuild) loadCommand() string {
	return fmt.Sprintf("module load EESSI/%s && module load %s", b.stack, b.module)
}

func (b eessiBuild) launchCommand() string {
	if len(b.exec) == 0 {
		return ""
	}
	return fmt.Sprintf("eessi-launch --stack %s -- %s %s", b.stack, b.module, strings.Join(b.exec, " "))
}

type eessiCatalogFile struct {
	SchemaVersion             int                         `json:"schemaVersion"`
	Backend                   string                      `json:"backend"`
	ExplicitSelectionRequired bool                        `json:"explicitSelectionRequired"`
	CVMFSRepo                 string                      `json:"cvmfsRepo"`
	Stacks                    []string                    `json:"stacks"`
	Tools                     map[string]eessiCatalogTool `json:"tools"`
}

type eessiCatalogTool struct {
	Package     string                     `json:"package"`
	Categories  []string                   `json:"categories"`
	Description string                     `json:"description"`
	Homepage    string                     `json:"homepage"`
	Apps        map[string]eessiCatalogApp `json:"apps"`
}

type eessiCatalogApp struct {
	Backend         string   `json:"backend"`
	Stack           string   `json:"stack"`
	Module          string   `json:"module"`
	SoftwareVersion string   `json:"software_version"`
	Toolchain       string   `json:"toolchain"`
	Arch            []string `json:"arch"`
	GPU             bool     `json:"gpu"`
	GPUTargets      []string `json:"gpu_targets"`
	Licences        []string `json:"license"`
	Exec            []string `json:"exec"`
	GUI             bool     `json:"gui"`
	Terminal        bool     `json:"terminal"`
	ShowInMenu      bool     `json:"show_in_menu"`
	ShowInApplist   *bool    `json:"show_in_applist"`
}

// parseEESSICatalog validates and decodes a sibling eessi.json catalog.
func parseEESSICatalog(data []byte) ([]eessiBuild, error) {
	var doc eessiCatalogFile
	if err := json.Unmarshal(data, &doc); err != nil {
		return nil, err
	}
	switch {
	case doc.SchemaVersion > 1:
		return nil, fmt.Errorf("schemaVersion %d needs a newer sciget", doc.SchemaVersion)
	case doc.SchemaVersion != 1:
		return nil, errors.New("schemaVersion must be 1")
	case doc.Backend != "eessi":
		return nil, fmt.Errorf("backend must be \"eessi\", got %q", doc.Backend)
	case !doc.ExplicitSelectionRequired:
		return nil, errors.New("explicitSelectionRequired must be true")
	case !eessiRepoPattern.MatchString(doc.CVMFSRepo):
		return nil, fmt.Errorf("invalid cvmfsRepo %q", doc.CVMFSRepo)
	case len(doc.Stacks) == 0:
		return nil, errors.New("stacks must not be empty")
	case doc.Tools == nil:
		return nil, errors.New("missing tools object")
	}
	for _, s := range doc.Stacks {
		if !eessiStackPattern.MatchString(s) {
			return nil, fmt.Errorf("invalid stack %q", s)
		}
	}

	var builds []eessiBuild
	for id, tool := range doc.Tools {
		if !eessiToolIDPattern.MatchString(id) {
			return nil, fmt.Errorf("invalid tool ID %q", id)
		}
		if strings.TrimSpace(tool.Package) == "" {
			return nil, fmt.Errorf("tool %q has empty package name", id)
		}
		cleanDesc := strings.NewReplacer("\r", " ", "\n", " ", "\t", " ").Replace(tool.Description)
		toolTexts := slices.Concat([]string{id, tool.Package, cleanDesc, tool.Homepage}, tool.Categories)
		if slices.ContainsFunc(toolTexts, func(s string) bool { return printable(s) != s }) {
			return nil, fmt.Errorf("EESSI tool %q has a control character", id)
		}
		for label, app := range tool.Apps {
			appTexts := slices.Concat(
				[]string{label, app.SoftwareVersion, app.Toolchain},
				app.Arch, app.GPUTargets, app.Licences,
			)
			if slices.ContainsFunc(appTexts, func(s string) bool { return printable(s) != s }) {
				return nil, fmt.Errorf("EESSI entry %q of %q has a control character", label, id)
			}
			if app.Backend != "eessi" {
				return nil, fmt.Errorf("tool %q app %q has invalid backend %q", id, label, app.Backend)
			}
			if !eessiStackPattern.MatchString(app.Stack) {
				return nil, fmt.Errorf("tool %q app %q has invalid stack %q", id, label, app.Stack)
			}
			if !eessiModulePattern.MatchString(app.Module) {
				return nil, fmt.Errorf("tool %q app %q has invalid module %q", id, label, app.Module)
			}
			if strings.TrimSpace(app.SoftwareVersion) == "" {
				return nil, fmt.Errorf("tool %q app %q has empty software_version", id, label)
			}
			for _, arg := range app.Exec {
				if !eessiExecArgPattern.MatchString(arg) {
					return nil, fmt.Errorf("tool %q app %q has invalid exec argument %q", id, label, arg)
				}
			}
			if app.ShowInMenu && len(app.Exec) == 0 {
				return nil, fmt.Errorf("tool %q app %q sets show_in_menu without exec", id, label)
			}
			hidden := app.ShowInApplist != nil && !*app.ShowInApplist
			builds = append(builds, eessiBuild{
				id:          id,
				pkg:         strings.TrimSpace(tool.Package),
				label:       label,
				stack:       app.Stack,
				module:      app.Module,
				version:     strings.TrimSpace(app.SoftwareVersion),
				toolchain:   app.Toolchain,
				arch:        slices.Clone(app.Arch),
				gpu:         app.GPU,
				gpuTargets:  slices.Clone(app.GPUTargets),
				licences:    slices.Clone(app.Licences),
				categories:  slices.Clone(tool.Categories),
				description: strings.TrimSpace(cleanDesc),
				homepage:    strings.TrimSpace(tool.Homepage),
				exec:        slices.Clone(app.Exec),
				gui:         app.GUI,
				terminal:    app.Terminal,
				hidden:      hidden,
			})
		}
	}
	sortEESSIBuilds(builds)
	return builds, nil
}

func sortEESSIBuilds(builds []eessiBuild) {
	slices.SortStableFunc(builds, func(a, b eessiBuild) int {
		if c := strings.Compare(a.id, b.id); c != 0 {
			return c
		}
		if c := strings.Compare(b.stack, a.stack); c != 0 {
			return c
		}
		if a.gpu != b.gpu {
			if a.gpu {
				return 1
			}
			return -1
		}
		if c := strings.Compare(a.module, b.module); c != 0 {
			return c
		}
		return strings.Compare(a.label, b.label)
	})
}

// readEESSIBuilds reads cached eessi.json files from every catalog in use.
// Catalogs without an eessi.json are skipped cleanly.
func readEESSIBuilds() ([]eessiBuild, error) {
	cacheDir, used, err := usedCatalogs()
	if err != nil {
		return nil, err
	}
	var all []eessiBuild
	seen := map[string]bool{}
	for _, cat := range used {
		path := filepath.Join(cat.dir(cacheDir), "eessi.json")
		data, err := os.ReadFile(path)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, &catalogError{name: cat.Name, err: err}
		}
		builds, err := parseEESSICatalog(data)
		if err != nil {
			return nil, &catalogError{name: cat.Name, err: err}
		}
		for i := range builds {
			builds[i].catalog = cat.Name
			key := builds[i].id + "\x00" + builds[i].stack + "\x00" + builds[i].module
			if seen[key] {
				builds[i].shadowed = true
			} else {
				seen[key] = true
			}
			all = append(all, builds[i])
		}
	}
	sortEESSIBuilds(all)
	return all, nil
}

func newEESSICommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "eessi",
		Short: "Browse and launch curated EESSI CVMFS modules (explicit selection; no DOI)",
		Args:  noArgs,
	}
	cmd.AddCommand(newEESSIFindCommand())
	cmd.AddCommand(newEESSIInfoCommand())
	cmd.AddCommand(newEESSILaunchCommand())
	return cmd
}

func newEESSIFindCommand() *cobra.Command {
	var all, gpuOnly bool
	cmd := &cobra.Command{
		Use:   "find [words...]",
		Short: "List curated EESSI modules in the catalogs in use",
		RunE: func(cmd *cobra.Command, args []string) error {
			builds, err := readEESSIBuilds()
			if err != nil {
				return err
			}
			var matches []eessiBuild
			for _, b := range builds {
				if b.hidden && !all {
					continue
				}
				if gpuOnly && !b.gpu {
					continue
				}
				if !eessiMatches(b, args) {
					continue
				}
				matches = append(matches, b)
			}
			if len(matches) == 0 {
				_, err := fmt.Fprintf(cmd.ErrOrStderr(), "No EESSI modules match %q.\n", strings.Join(args, " "))
				return err
			}
			out := cmd.OutOrStdout()
			return writeEESSIBuilds(out, matches, isTerminal(out))
		},
	}
	cmd.Flags().BoolVar(&all, "all", false, "include hidden EESSI entries")
	cmd.Flags().BoolVar(&gpuOnly, "gpu", false, "show only GPU-enabled EESSI builds")
	return cmd
}

func eessiMatches(b eessiBuild, words []string) bool {
	text := strings.ToLower(strings.Join([]string{
		b.id, b.pkg, b.name(), b.module, b.stack, strings.Join(b.categories, " "),
	}, " "))
	for _, w := range words {
		if !strings.Contains(text, strings.ToLower(w)) {
			return false
		}
	}
	return true
}

func writeEESSIBuilds(w io.Writer, builds []eessiBuild, terminal bool) error {
	var rows strings.Builder
	for _, b := range builds {
		gpu := ""
		if b.gpu {
			gpu = "yes"
		}
		gui := ""
		if b.gui {
			gui = "yes"
		}
		note := ""
		if b.shadowed {
			note = "(shadowed)"
		}
		fmt.Fprintf(&rows, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
			b.name(), b.stack, b.module, b.catalog, strings.Join(b.categories, ", "), gpu, gui, note)
	}
	return writeTable(w, terminal, "NAME\tSTACK\tMODULE\tCATALOG\tCATEGORIES\tGPU\tGUI\tNOTE\n", rows.String())
}

func lookupEESSIBuild(builds []eessiBuild, query, stack string, gpu bool) (eessiBuild, error) {
	if stack != "" && !eessiStackPattern.MatchString(stack) {
		return eessiBuild{}, usageError{fmt.Sprintf("invalid --stack %q (expected YYYY.MM)", stack)}
	}
	toolQuery, verQuery, hasSlash := strings.Cut(query, "/")
	for _, b := range builds {
		if b.shadowed {
			continue
		}
		if stack != "" && b.stack != stack {
			continue
		}
		if gpu && !b.gpu {
			continue
		}
		if hasSlash {
			if strings.EqualFold(b.id, toolQuery) && b.version == verQuery {
				return b, nil
			}
			if b.module == query {
				return b, nil
			}
		} else if strings.EqualFold(b.id, query) || strings.EqualFold(b.pkg, query) {
			return b, nil
		}
	}
	return eessiBuild{}, fmt.Errorf("no EESSI module %q found in catalogs in use (run 'sciget eessi find' or 'sciget refresh')", query)
}

func newEESSIInfoCommand() *cobra.Command {
	var stack string
	var gpu bool
	cmd := &cobra.Command{
		Use:   "info <tool[/version]>",
		Short: "Show details and Lmod load command for a curated EESSI module",
		Args:  exactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			builds, err := readEESSIBuilds()
			if err != nil {
				return err
			}
			b, err := lookupEESSIBuild(builds, args[0], stack, gpu)
			if err != nil {
				return err
			}
			cats := strings.Join(b.categories, ", ")
			if cats == "" {
				cats = "(none)"
			}
			lic := strings.Join(b.licences, ", ")
			if lic == "" {
				lic = "(not stated)"
			}
			arch := strings.Join(b.arch, ", ")
			if arch == "" {
				arch = "(unknown)"
			}
			gpuText := "no"
			if b.gpu {
				gpuText = "yes"
				if len(b.gpuTargets) > 0 {
					gpuText += " (" + strings.Join(b.gpuTargets, ", ") + ")"
				}
			}
			out := cmd.OutOrStdout()
			_, err = fmt.Fprintf(out,
				"%-13s%s (%s)\n%-13s%s\n%-13sEESSI/%s\n%-13s%s\n%-13s%s\n%-13s%s\n%-13s%s\n%-13s%s\n%-13s(none — EESSI CVMFS module)\n%-13s%s\n",
				"Name:", b.name(), b.pkg,
				"Catalog:", b.catalog,
				"Stack:", b.stack,
				"Module:", b.module,
				"Arch:", arch,
				"GPU:", gpuText,
				"Categories:", cats,
				"Licence:", lic,
				"DOI:",
				"Load:", b.loadCommand(),
			)
			if err != nil {
				return err
			}
			if lc := b.launchCommand(); lc != "" {
				_, err = fmt.Fprintf(out, "%-13s%s\n", "Launch:", lc)
			}
			return err
		},
	}
	cmd.Flags().StringVar(&stack, "stack", "", "require a specific EESSI stack (YYYY.MM)")
	cmd.Flags().BoolVar(&gpu, "gpu", false, "select a GPU-enabled build")
	return cmd
}

func newEESSILaunchCommand() *cobra.Command {
	var stack string
	var gpu bool
	cmd := &cobra.Command{
		Use:   "launch <tool[/version]> [args...]",
		Short: "Launch a curated EESSI tool via eessi-launch (explicit selection)",
		Args:  cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			builds, err := readEESSIBuilds()
			if err != nil {
				return err
			}
			b, err := lookupEESSIBuild(builds, args[0], stack, gpu)
			if err != nil {
				return err
			}
			if len(b.exec) == 0 {
				return fmt.Errorf("%s has no direct launcher command; load it in a shell with:\n  %s", b.name(), b.loadCommand())
			}
			bin, err := eessiLookupLauncher()
			if err != nil {
				return fmt.Errorf("eessi-launch not found on PATH; load the module directly with:\n  %s", b.loadCommand())
			}
			argv := append([]string{"--stack", b.stack, "--", b.module}, b.exec...)
			argv = append(argv, args[1:]...)
			return eessiLaunchExec(bin, argv, os.Stdin, cmd.OutOrStdout(), cmd.ErrOrStderr())
		},
	}
	cmd.Flags().StringVar(&stack, "stack", "", "require a specific EESSI stack (YYYY.MM)")
	cmd.Flags().BoolVar(&gpu, "gpu", false, "select a GPU-enabled build")
	return cmd
}
