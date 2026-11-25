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
	eessiExecBinPattern = regexp.MustCompile(`^[A-Za-z0-9._/@:+=][A-Za-z0-9._/@:+=-]{0,127}$`)
	eessiExecArgPattern = regexp.MustCompile(`^[A-Za-z0-9._/@:+=-]{1,128}$`)
	eessiRepoPattern    = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9-]*[a-z0-9])?(?:\.[a-z0-9](?:[a-z0-9-]*[a-z0-9])?)+$`)
)

// eessiStrictLaunchScript loads the exact requested EESSI stack and exact
// module from the declared CVMFS repository without falling back to an
// unversioned package name, then execs the validated command with `--`.
const eessiStrictLaunchScript = `set -euo pipefail
modroot="$1"
stack="$2"
mod="$3"
shift 3
if [ ! -d "${modroot}" ]; then
    echo "sciget eessi: EESSI repository is not mounted at ${modroot}" >&2
    exit 1
fi
lmod_init="${EESSI_LMOD_INIT:-}"
if [ -z "${lmod_init}" ]; then
    for candidate in "${LMOD_PKG:+${LMOD_PKG}/init/bash}" /opt/apps/lmod/lmod/init/bash /usr/share/lmod/lmod/init/bash /etc/profile.d/lmod.sh; do
        if [ -n "${candidate}" ] && [ -r "${candidate}" ]; then
            lmod_init="${candidate}"
            break
        fi
    done
fi
if [ -z "${lmod_init}" ] || [ ! -r "${lmod_init}" ]; then
    echo "sciget eessi: Lmod init script not found (set EESSI_LMOD_INIT or run in an Lmod-enabled shell)" >&2
    exit 1
fi
export FPATH="${FPATH:-}"
source "${lmod_init}"
case ":${MODULEPATH:-}:" in
*":${modroot}:"*) ;;
*) export MODULEPATH="${modroot}${MODULEPATH:+:${MODULEPATH}}" ;;
esac
export EESSI_MODULE_QUIET_LOAD=1
module load "EESSI/${stack}" >/dev/null
module load "${mod}" >/dev/null
exec -- "$@"
`

// eessiBuild is one curated EESSI Lmod module entry across catalogs in use.
type eessiBuild struct {
	id           string
	pkg          string
	label        string
	catalog      string
	catalogOrder int
	cvmfsRepo    string
	stack        string
	module       string
	version      string
	toolchain    string
	arch         []string
	gpu          bool
	gpuTargets   []string
	licences     []string
	categories   []string
	description  string
	homepage     string
	exec         []string
	gui          bool
	terminal     bool
	hidden       bool
	shadowed     bool
}

func (b eessiBuild) name() string { return b.id + "/" + b.version }

func (b eessiBuild) moduleRoot() string {
	if root := os.Getenv("EESSI_MODULE_ROOT"); root != "" {
		return root
	}
	return "/cvmfs/" + b.cvmfsRepo + "/init/modules"
}

func (b eessiBuild) loadCommand() string {
	prefix := ""
	if b.cvmfsRepo != "" && b.cvmfsRepo != "software.eessi.io" {
		prefix = fmt.Sprintf("module use /cvmfs/%s/init/modules && ", b.cvmfsRepo)
	}
	return fmt.Sprintf("%smodule load EESSI/%s && module load %s", prefix, b.stack, b.module)
}

func (b eessiBuild) launchCommand() string {
	if len(b.exec) == 0 {
		return ""
	}
	parts := []string{"sciget", "eessi", "launch", "--stack", b.stack}
	if b.gpu {
		parts = append(parts, "--gpu")
	}
	parts = append(parts, b.module)
	return shellJoin(parts)
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
// When allowedStacks is non-empty (from the owning domain's eessi_stacks),
// every build stack must also belong to allowedStacks.
func parseEESSICatalog(data []byte, allowedStacks []string) ([]eessiBuild, error) {
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
	declaredStacks := make(map[string]bool, len(doc.Stacks))
	for _, s := range doc.Stacks {
		if !eessiStackPattern.MatchString(s) {
			return nil, fmt.Errorf("invalid stack %q", s)
		}
		declaredStacks[s] = true
	}
	var domainStacks map[string]bool
	if len(allowedStacks) > 0 {
		domainStacks = make(map[string]bool, len(allowedStacks))
		for _, s := range allowedStacks {
			domainStacks[s] = true
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
			if strings.TrimSpace(label) == "" {
				return nil, fmt.Errorf("EESSI tool %q has blank app label", id)
			}
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
			if !declaredStacks[app.Stack] {
				return nil, fmt.Errorf("tool %q app %q uses undeclared stack %q", id, label, app.Stack)
			}
			if domainStacks != nil && !domainStacks[app.Stack] {
				return nil, fmt.Errorf("tool %q app %q uses stack %q outside domain eessi_stacks", id, label, app.Stack)
			}
			if !eessiModulePattern.MatchString(app.Module) {
				return nil, fmt.Errorf("tool %q app %q has invalid module %q", id, label, app.Module)
			}
			if strings.TrimSpace(app.SoftwareVersion) == "" {
				return nil, fmt.Errorf("tool %q app %q has empty software_version", id, label)
			}
			for i, arg := range app.Exec {
				if i == 0 {
					if !eessiExecBinPattern.MatchString(arg) {
						return nil, fmt.Errorf("tool %q app %q has invalid executable %q", id, label, arg)
					}
				} else if !eessiExecArgPattern.MatchString(arg) {
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
				cvmfsRepo:   doc.CVMFSRepo,
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
		if a.catalogOrder != b.catalogOrder {
			if a.catalogOrder < b.catalogOrder {
				return -1
			}
			return 1
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
		if c := strings.Compare(a.label, b.label); c != 0 {
			return c
		}
		return strings.Compare(strings.Join(a.exec, "\x00"), strings.Join(b.exec, "\x00"))
	})
}

// readEESSIBuilds reads cached eessi.json files from every catalog in use.
// Catalogs without an eessi.json (or whose catalog.json declares no
// eessiCatalogUrl) are skipped cleanly. Earlier catalogs in use take precedence.
func readEESSIBuilds() ([]eessiBuild, error) {
	cacheDir, used, err := usedCatalogs()
	if err != nil {
		return nil, err
	}
	var all []eessiBuild
	seen := map[string]bool{}
	for catIdx, cat := range used {
		dir := cat.dir(cacheDir)
		var allowedStacks []string
		catData, err := os.ReadFile(filepath.Join(dir, "catalog.json"))
		if err == nil {
			meta, err := parseCatalogJSON(catData)
			if err != nil {
				return nil, &catalogError{name: cat.Name, err: err}
			}
			if meta.EESSICatalogURL == "" {
				continue
			}
			if err := checkEESSIStacks(meta.EESSIStacks); err != nil {
				return nil, &catalogError{name: cat.Name, err: err}
			}
			allowedStacks = meta.EESSIStacks
		} else if !errors.Is(err, os.ErrNotExist) {
			return nil, &catalogError{name: cat.Name, err: err}
		}

		path := filepath.Join(dir, "eessi.json")
		data, err := os.ReadFile(path)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, &catalogError{name: cat.Name, err: err}
		}
		builds, err := parseEESSICatalog(data, allowedStacks)
		if err != nil {
			return nil, &catalogError{name: cat.Name, err: err}
		}
		catSeen := map[string]bool{}
		for i := range builds {
			builds[i].catalog = cat.Name
			builds[i].catalogOrder = catIdx
			key := builds[i].id + "\x00" + builds[i].stack + "\x00" + builds[i].module + "\x00" + strings.Join(builds[i].exec, "\x00")
			if seen[key] {
				builds[i].shadowed = true
			}
			catSeen[key] = true
			all = append(all, builds[i])
		}
		for k := range catSeen {
			seen[k] = true
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
		b.id, b.pkg, b.label, b.name(), b.module, b.stack,
		strings.Join(b.exec, " "), strings.Join(b.categories, " "),
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
		cmd := ""
		if len(b.exec) > 0 {
			cmd = b.exec[0]
		}
		fmt.Fprintf(&rows, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
			b.name(), b.stack, b.module, cmd, b.catalog, strings.Join(b.categories, ", "), gpu, gui, note)
	}
	return writeTable(w, terminal, "NAME\tSTACK\tMODULE\tCOMMAND\tCATALOG\tCATEGORIES\tGPU\tGUI\tNOTE\n", rows.String())
}

func matchesLauncher(b eessiBuild, launcher string) bool {
	if launcher == "" {
		return true
	}
	if strings.EqualFold(b.label, launcher) {
		return true
	}
	return len(b.exec) > 0 && b.exec[0] == launcher
}

func lookupEESSIBuild(builds []eessiBuild, query, stack string, gpu bool, launcher string) (eessiBuild, error) {
	if stack != "" && !eessiStackPattern.MatchString(stack) {
		return eessiBuild{}, usageError{fmt.Sprintf("invalid --stack %q (expected YYYY.MM)", stack)}
	}
	toolQuery, verQuery, hasSlash := strings.Cut(query, "/")
	var matched []eessiBuild
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
		if !matchesLauncher(b, launcher) {
			continue
		}
		ok := false
		if hasSlash {
			ok = (strings.EqualFold(b.id, toolQuery) && b.version == verQuery) || b.module == query
		} else {
			ok = strings.EqualFold(b.id, query) || strings.EqualFold(b.pkg, query)
		}
		if ok {
			matched = append(matched, b)
		}
	}
	if len(matched) == 0 {
		return eessiBuild{}, fmt.Errorf("no EESSI module %q found in catalogs in use (run 'sciget eessi find' or 'sciget refresh')", query)
	}

	// Honor catalog precedence: keep entries from the earliest catalog that matched,
	// then pick its top-ranked (stack, gpu, module) build.
	winCat := matched[0].catalogOrder
	for _, b := range matched {
		if b.catalogOrder < winCat {
			winCat = b.catalogOrder
		}
	}
	var inWinCat []eessiBuild
	for _, b := range matched {
		if b.catalogOrder == winCat {
			inWinCat = append(inWinCat, b)
		}
	}
	best := inWinCat[0]
	var sameBuild []eessiBuild
	for _, b := range inWinCat {
		if b.stack == best.stack && b.gpu == best.gpu && b.module == best.module {
			sameBuild = append(sameBuild, b)
		}
	}
	if len(sameBuild) > 1 {
		var choices []string
		for _, b := range sameBuild {
			if len(b.exec) > 0 {
				choices = append(choices, fmt.Sprintf("%q (%s)", b.label, b.exec[0]))
			} else {
				choices = append(choices, fmt.Sprintf("%q", b.label))
			}
		}
		return eessiBuild{}, fmt.Errorf("multiple EESSI launchers match %q in %s: %s; pick one with --launcher",
			query, best.module, strings.Join(choices, ", "))
	}
	return best, nil
}

func newEESSIInfoCommand() *cobra.Command {
	var stack, launcher string
	var gpu bool
	cmd := &cobra.Command{
		Use:   "info <tool[/version]|module>",
		Short: "Show details and Lmod load command for a curated EESSI module",
		Args:  exactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			builds, err := readEESSIBuilds()
			if err != nil {
				return err
			}
			b, err := lookupEESSIBuild(builds, args[0], stack, gpu, launcher)
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
				"%-13s%s (%s)\n%-13s%s\n%-13s%s\n%-13sEESSI/%s\n%-13s%s\n%-13s%s\n%-13s%s\n%-13s%s\n%-13s%s\n%-13s(none — EESSI CVMFS module)\n%-13s%s\n",
				"Name:", b.name(), b.pkg,
				"Catalog:", b.catalog,
				"Repository:", b.cvmfsRepo,
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
	cmd.Flags().StringVar(&launcher, "launcher", "", "select a specific launcher label or command when a module exposes multiple")
	return cmd
}

func runEESSILaunch(b eessiBuild, extraArgs []string, stdin io.Reader, stdout, stderr io.Writer) error {
	bashPath, err := exec.LookPath("bash")
	if err != nil {
		return fmt.Errorf("bash not found on PATH; load the module directly with:\n  %s", b.loadCommand())
	}
	argv := []string{"-c", eessiStrictLaunchScript, "sciget-eessi-launch", b.moduleRoot(), b.stack, b.module}
	argv = append(argv, b.exec...)
	argv = append(argv, extraArgs...)
	cmd := exec.Command(bashPath, argv...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = stdin, stdout, stderr
	return cmd.Run()
}

func newEESSILaunchCommand() *cobra.Command {
	var stack, launcher string
	var gpu bool
	cmd := &cobra.Command{
		Use:   "launch [flags] <tool[/version]|module> [args...]",
		Short: "Launch a curated EESSI tool with exact stack and module loading (explicit selection)",
		Args: func(cmd *cobra.Command, args []string) error {
			if err := cobra.MinimumNArgs(1)(cmd, args); err != nil {
				return usageError{err.Error()}
			}
			return nil
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			builds, err := readEESSIBuilds()
			if err != nil {
				return err
			}
			b, err := lookupEESSIBuild(builds, args[0], stack, gpu, launcher)
			if err != nil {
				return err
			}
			if len(b.exec) == 0 {
				return fmt.Errorf("%s has no direct launcher command; load it in a shell with:\n  %s", b.name(), b.loadCommand())
			}
			return runEESSILaunch(b, args[1:], os.Stdin, cmd.OutOrStdout(), cmd.ErrOrStderr())
		},
	}
	cmd.Flags().StringVar(&stack, "stack", "", "require a specific EESSI stack (YYYY.MM)")
	cmd.Flags().BoolVar(&gpu, "gpu", false, "select a GPU-enabled build")
	cmd.Flags().StringVar(&launcher, "launcher", "", "select a specific launcher label or command when a module exposes multiple")
	// Stop flag parsing at the first positional argument so tool flags
	// (such as --help or --gpu) pass straight to the launched EESSI binary.
	cmd.Flags().SetInterspersed(false)
	return cmd
}
