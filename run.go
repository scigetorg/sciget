package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"syscall"

	"github.com/spf13/cobra"
)

// runApptainer hands over to Apptainer and can be replaced by tests.
var runApptainer = execApptainer

// location is where a container was found: its path, the catalog whose
// share holds it ("" when it is in the store), whether it is another
// build than the catalog's (usually an older one), and the build date of
// the image found on disk.
type location struct {
	path      string
	share     string
	older     bool
	buildDate string
}

// notOnMachineError identifies a known container missing from local storage.
type notOnMachineError struct{ name string }

func (e notOnMachineError) Error() string {
	// Quoted, because the name comes from a catalog and the line is
	// meant to be pasted into a shell.
	return e.name + " is not on this machine; to download it, run: " + shellJoin([]string{"sciget", "pull", e.name}) +
		", or let sciget download when needed: sciget config set auto_pull true"
}

// unsafeLayoutValue reports whether v cannot fill a layout: it is empty, or
// could name another directory or vanish from a path (".").
func unsafeLayoutValue(v string) bool {
	return v == "" || v == "." || strings.Contains(v, "..") || strings.ContainsAny(v, "/\x00"+string(filepath.Separator))
}

// layoutPath fills a validated layout with safe container values.
func layoutPath(layout string, c container) (string, error) {
	if err := checkLayout(layout); err != nil {
		return "", fmt.Errorf("container %q has an invalid layout: %w", c.name(), err)
	}
	for _, value := range []string{c.tool, c.version, c.buildDate} {
		if unsafeLayoutValue(value) {
			return "", fmt.Errorf("catalog entry %q has an unsafe layout value %q", c.name(), value)
		}
	}
	path := strings.NewReplacer("{tool}", c.tool, "{version}", c.version, "{date}", c.buildDate).Replace(layout)
	if !filepath.IsLocal(path) {
		return "", fmt.Errorf("catalog entry %q has an unsafe container path", c.name())
	}
	return filepath.FromSlash(path), nil
}

// locate searches the winner's catalog share, then the store cfg names,
// for c's build. Failing that, it searches both for another build of c's
// tool and version, so a catalog refresh never stops a container on the
// machine from running.
func locate(cfg config, c container) (location, error) {
	_, catalogs, err := readCatalogs()
	if err != nil {
		return location{}, err
	}
	var share catalog
	var shareLayout string
	if i := findCatalog(catalogs, c.catalog); i >= 0 && catalogs[i].Dir != "" {
		share = catalogs[i]
		shareLayout = share.Layout
		if shareLayout == "" {
			// A share is laid out by its publisher, so the user's
			// container_layout does not apply.
			if shareLayout, err = defaultValue("container_layout"); err != nil {
				return location{}, err
			}
		}
		path, err := findContainer(share.Dir, shareLayout, c)
		if err != nil {
			return location{}, err
		}
		if path != "" {
			return location{path: path, share: share.Name, buildDate: c.buildDate}, nil
		}
	}
	// The store is resolved only after the share misses, so a share hit
	// works even when the store's default cannot be expanded.
	dir, _, err := cfg.get("container_dir")
	if err != nil {
		return location{}, err
	}
	layout, _, err := cfg.get("container_layout")
	if err != nil {
		return location{}, err
	}
	path, err := findStoreContainer(dir, layout, c)
	if err != nil {
		return location{}, err
	}
	if path != "" {
		return location{path: path, buildDate: c.buildDate}, nil
	}
	if share.Dir != "" {
		path, date, err := otherBuildIn(share.Dir, shareLayout, c, false)
		if err != nil {
			return location{}, err
		}
		if path != "" {
			return location{path: path, share: share.Name, older: true, buildDate: date}, nil
		}
	}
	path, date, err := otherBuildIn(dir, layout, c, true)
	if err != nil {
		return location{}, err
	}
	if path != "" {
		return location{path: path, older: true, buildDate: date}, nil
	}
	return location{}, notOnMachineError{name: c.name()}
}

// findStoreContainer returns c's path in container_dir if a matching image
// is present, rejecting files whose SIF header indicates another architecture
// and (for ambiguous store paths shared across architectures) verifying
// published size and SHA-256.
func findStoreContainer(dir, layout string, c container) (string, error) {
	path, err := findContainer(dir, layout, c)
	if err != nil || path == "" {
		return path, err
	}
	ok, err := matchesLocalStore(path, c, c.ambiguousStore, c.ambiguousStore)
	if err != nil {
		return "", err
	}
	if !ok {
		return "", nil
	}
	return path, nil
}

// expectedStoreArch returns the CPU architecture expected for a local SIF
// image backing c. Containers with unspecified or "any" architecture expect
// the host's own architecture.
func expectedStoreArch(c container) string {
	switch arch := c.effectiveArch(); arch {
	case "", "any":
		return hostArch()
	default:
		return arch
	}
}

// matchesLocalStore checks whether a file or sandbox in container_dir matches c:
// it always checks a SIF header's architecture (64-byte read), checks published
// size and SHA-256 when checkPublished is true, and requires positive
// verification (a matching published SHA-256 or a matching SIF architecture
// header) when requireVerified is true so unverifiable sandbox directories and
// unverified files cannot leak across incompatible architectures.
func matchesLocalStore(path string, c container, checkPublished, requireVerified bool) (bool, error) {
	info, err := os.Stat(path)
	if err != nil {
		return false, fmt.Errorf("locating %s: %w", c.name(), err)
	}
	wantArch := expectedStoreArch(c)
	if !info.Mode().IsRegular() {
		return !requireVerified, nil
	}
	if checkPublished && c.published.size > 0 && info.Size() != c.published.size {
		return false, nil
	}
	f, err := os.Open(path)
	if err != nil {
		return false, fmt.Errorf("locating %s: %w", c.name(), err)
	}
	defer f.Close()
	hasSHA := checkPublished && c.published.sha256 != ""
	if wantArch != "" {
		var hdr [64]byte
		gotArch := ""
		if n, _ := io.ReadFull(f, hdr[:]); n >= 47 {
			gotArch = sifArch(hdr[:n])
		}
		if gotArch != "" && gotArch != wantArch {
			return false, nil
		}
		if requireVerified && !hasSHA && gotArch != wantArch {
			return false, nil
		}
		if hasSHA {
			if _, err := f.Seek(0, io.SeekStart); err != nil {
				return false, fmt.Errorf("locating %s: %w", c.name(), err)
			}
		}
	}
	if hasSHA {
		h := sha256.New()
		if _, err := io.Copy(h, f); err != nil {
			return false, fmt.Errorf("locating %s: %w", c.name(), err)
		}
		if hex.EncodeToString(h.Sum(nil)) != c.published.sha256 {
			return false, nil
		}
	}
	return true, nil
}

// sifArch extracts the normalized CPU architecture from a Singularity Image
// Format (SIF) header, or "" if hdr is not a SIF header with a known arch.
func sifArch(hdr []byte) string {
	if len(hdr) < 47 || !bytes.Equal(hdr[32:42], []byte("SIF_MAGIC\x00")) {
		return ""
	}
	switch string(hdr[45:47]) {
	case "01":
		return "386"
	case "02":
		return "x86_64"
	case "03":
		return "arm"
	case "04":
		return "aarch64"
	case "05":
		return "ppc64"
	case "06":
		return "ppc64le"
	case "07":
		return "mips"
	case "08":
		return "mipsle"
	case "09":
		return "mips64"
	case "10":
		return "mips64le"
	case "11":
		return "s390x"
	case "12":
		return "riscv64"
	case "13":
		return "loong64"
	default:
		return ""
	}
}

// otherBuild returns the path of the newest build of c's tool and version in
// dir other than c's own, or "" if there is none. Candidates whose SIF header
// indicates a mismatched architecture are skipped. A build date is eight digits.
func otherBuild(dir, layout string, c container) (string, error) {
	path, _, err := otherBuildIn(dir, layout, c, true)
	return path, err
}

func otherBuildIn(dir, layout string, c container, isStore bool) (string, string, error) {
	if _, err := layoutPath(layout, c); err != nil {
		return "", "", err
	}
	// The layout is escaped first: its own characters are literal too.
	pattern := strings.NewReplacer("{tool}", globEscape(c.tool), "{version}", globEscape(c.version),
		"{date}", strings.Repeat("[0-9]", 8)).Replace(globEscape(layout))
	matches, err := filepath.Glob(filepath.Join(globEscape(dir), filepath.FromSlash(pattern)))
	if err != nil {
		return "", "", fmt.Errorf("locating %s: %w", c.name(), err)
	}
	best, bestDate := "", ""
	for _, match := range matches {
		// Each eight characters of the match is tried as the date, and kept
		// only if the layout gives back the match, so a layout that names
		// the date twice must have the same date in both places.
		for i := 0; i+8 <= len(match); i++ {
			other := c
			other.buildDate = match[i : i+8]
			if other.buildDate == c.buildDate {
				continue
			}
			relative, err := layoutPath(layout, other)
			if err != nil || filepath.Join(dir, relative) != match || other.buildDate <= bestDate {
				continue
			}
			path, err := findContainer(dir, layout, other)
			if err != nil {
				return "", "", err
			}
			if path != "" {
				requireVerified := isStore && slices.Contains(c.foreignDates, other.buildDate)
				ok, err := matchesLocalStore(path, other, false, requireVerified)
				if err != nil {
					return "", "", err
				}
				if !ok {
					path = ""
				}
			}
			if path != "" {
				best, bestDate = path, other.buildDate
			}
		}
	}
	return best, bestDate, nil
}

// globEscape escapes the characters filepath.Glob treats as special.
func globEscape(s string) string {
	return strings.NewReplacer(`\`, `\\`, "*", `\*`, "?", `\?`, "[", `\[`).Replace(s)
}

// findContainer returns c's path in dir, or "" if no regular file or
// directory is there.
func findContainer(dir, layout string, c container) (string, error) {
	relative, err := layoutPath(layout, c)
	if err != nil {
		return "", err
	}
	path := filepath.Join(dir, relative)
	info, err := os.Stat(path)
	if errors.Is(err, fs.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("locating %s: %w", c.name(), err)
	}
	if !info.Mode().IsRegular() && !info.IsDir() {
		return "", nil
	}
	return path, nil
}

// execApptainer replaces sciget with the Apptainer binary at path. It
// returns only on failure.
func execApptainer(path string, args, env []string) error {
	if err := syscall.Exec(path, args, env); err != nil {
		return fmt.Errorf("running apptainer: %w", err)
	}
	return nil
}

// launchSettings holds the config values that shape an Apptainer command
// line.
type launchSettings struct {
	apptainer     string   // apptainer_path, as configured
	global        []string // apptainer_global_args
	verbArgs      []string // apptainer_exec_args or apptainer_shell_args
	extra         []string // apptainer_extra_args
	catalog       []string // the container's apptainer_args, if use_catalog_args
	passEnv       []string // apptainer_pass_env
	xauthorityVar string
	tmpVars       []string
	tmpTarget     string
	prompt        string // PS1 for shell verb, unless overridden by user/config
}

// safePromptToken reduces s to safe ASCII characters [A-Za-z0-9._+-] (capped
// at 32 bytes) so it can be embedded in PS1 without shell/prompt expansion.
func safePromptToken(s string) string {
	const maxLen = 32
	var b strings.Builder
	for _, r := range s {
		if b.Len() >= maxLen {
			break
		}
		if 'a' <= r && r <= 'z' || 'A' <= r && r <= 'Z' || '0' <= r && r <= '9' ||
			r == '.' || r == '_' || r == '+' || r == '-' {
			b.WriteRune(r)
		} else {
			b.WriteByte('_')
		}
	}
	if b.Len() == 0 {
		return "container"
	}
	return b.String()
}

// containerShellPrompt returns a safe Apptainer shell prompt naming c.
func containerShellPrompt(c container) string {
	return fmt.Sprintf("[%s/%s] Apptainer> ", safePromptToken(c.tool), safePromptToken(c.version))
}

// hasPromptOverride reports whether the caller or configured Apptainer
// arguments already specify a custom container prompt.
func hasPromptOverride(s launchSettings) bool {
	if os.Getenv("APPTAINERENV_PS1") != "" || os.Getenv("SINGULARITYENV_PS1") != "" {
		return true
	}
	for _, list := range [][]string{s.global, s.verbArgs, s.extra, s.catalog} {
		for _, arg := range list {
			if strings.HasPrefix(arg, "PS1=") || strings.HasPrefix(arg, "--env=PS1=") ||
				strings.Contains(arg, ",PS1=") {
				return true
			}
		}
	}
	return false
}

// readLaunchSettings reads the settings for running c with verb.
func readLaunchSettings(cfg config, verb string, c container) (launchSettings, error) {
	var s launchSettings
	var err error
	if s.apptainer, _, err = cfg.get("apptainer_path"); err != nil {
		return s, err
	}
	for name, list := range map[string]*[]string{
		"apptainer_global_args":       &s.global,
		"apptainer_" + verb + "_args": &s.verbArgs,
		"apptainer_extra_args":        &s.extra,
		"apptainer_pass_env":          &s.passEnv,
		"tmp_vars":                    &s.tmpVars,
	} {
		if *list, err = cfg.getList(name); err != nil {
			return s, err
		}
	}
	if s.xauthorityVar, _, err = cfg.get("xauthority_var"); err != nil {
		return s, err
	}
	if s.tmpTarget, _, err = cfg.get("tmp_target"); err != nil {
		return s, err
	}
	useCatalog, err := cfg.getBool("use_catalog_args")
	if err != nil {
		return s, err
	}
	if useCatalog {
		s.catalog = c.apptainerArgs
	}
	if verb == "shell" && !hasPromptOverride(s) {
		s.prompt = containerShellPrompt(c)
	}
	return s, nil
}

// apptainerArgs builds argv for running image with verb: the binary as
// configured, global arguments, the verb, its arguments with placeholders
// expanded, the shell prompt (if any), extra and catalog arguments, the image
// and, for exec, the command.
func apptainerArgs(s launchSettings, verb, image string, command []string) ([]string, error) {
	args := append([]string{s.apptainer}, s.global...)
	args = append(args, verb)
	for _, arg := range s.verbArgs {
		if expand, ok := placeholderArgs[arg]; ok {
			expanded, err := expand(s)
			if err != nil {
				return nil, err
			}
			args = append(args, expanded...)
			continue
		}
		args = append(args, arg)
	}
	if verb == "shell" && s.prompt != "" {
		args = append(args, "--env", "PS1="+s.prompt)
	}
	args = append(args, s.extra...)
	args = append(args, s.catalog...)
	args = append(args, image)
	if verb == "exec" {
		args = append(args, command...)
	}
	return args, nil
}

// placeholderArgs maps each placeholder a verb argument may be to the
// arguments it expands to at run time.
var placeholderArgs = map[string]func(launchSettings) ([]string, error){
	"{env}":        envArgs,
	"{xauthority}": xauthorityArgs,
	"{tmp}":        tmpArgs,
	"{pwd}":        pwdArgs,
}

// envArgs passes each non-empty variable in passEnv through --cleanenv.
func envArgs(s launchSettings) ([]string, error) {
	var args []string
	for _, name := range s.passEnv {
		value := os.Getenv(name)
		if value == "" {
			continue
		}
		if err := checkEnvValue(name, value); err != nil {
			return nil, err
		}
		args = append(args, "--env", name+"="+value)
	}
	return args, nil
}

// xauthorityArgs binds the X authority file read-only and passes its path,
// if the variable names a regular file.
func xauthorityArgs(s launchSettings) ([]string, error) {
	path := os.Getenv(s.xauthorityVar)
	if path == "" {
		return nil, nil
	}
	if info, err := os.Stat(path); err != nil || !info.Mode().IsRegular() {
		return nil, nil
	}
	if err := checkEnvValue(s.xauthorityVar, path); err != nil {
		return nil, err
	}
	if err := checkBindPath(s.xauthorityVar, path); err != nil {
		return nil, err
	}
	return []string{"--bind", path + ":" + path + ":ro", "--env", s.xauthorityVar + "=" + path}, nil
}

// tmpArgs binds the directory named by the last non-empty variable in
// tmpVars to tmpTarget.
func tmpArgs(s launchSettings) ([]string, error) {
	var name, dir string
	for _, v := range s.tmpVars {
		if value := os.Getenv(v); value != "" {
			name, dir = v, value
		}
	}
	if dir == "" {
		return nil, nil
	}
	if err := checkBindPath(name, dir); err != nil {
		return nil, err
	}
	return []string{"--bind", dir + ":" + s.tmpTarget}, nil
}

// pwdArgs starts the container in the physical working directory.
func pwdArgs(launchSettings) ([]string, error) {
	dir, err := os.Getwd()
	if err == nil {
		dir, err = filepath.EvalSymlinks(dir)
	}
	if err != nil {
		return nil, fmt.Errorf("finding the current directory: %w", err)
	}
	return []string{"--pwd", dir}, nil
}

// checkEnvValue rejects a value Apptainer would misread in --env: it splits
// the value on commas and evaluates it in the container.
func checkEnvValue(name, value string) error {
	if i := strings.IndexAny(value, ",$`\\\""); i >= 0 {
		return fmt.Errorf("cannot pass %s to the container: its value contains %q", name, value[i:i+1])
	}
	return nil
}

// checkBindPath rejects a path Apptainer would misread in --bind.
func checkBindPath(name, path string) error {
	if i := strings.IndexAny(path, bindSeparators); i >= 0 {
		return fmt.Errorf("cannot bind %s into the container: its value contains %q", name, path[i:i+1])
	}
	return nil
}

// shellJoin quotes args for a POSIX shell and joins them, so the line can
// be pasted and run.
func shellJoin(args []string) string {
	quoted := make([]string, len(args))
	for i, arg := range args {
		quoted[i] = arg
		if arg == "" || strings.ContainsFunc(arg, needsQuoting) {
			quoted[i] = "'" + strings.ReplaceAll(arg, "'", `'\''`) + "'"
		}
	}
	return strings.Join(quoted, " ")
}

// needsQuoting reports whether r is outside the characters a shell word
// may hold bare.
func needsQuoting(r rune) bool {
	return !('a' <= r && r <= 'z' || 'A' <= r && r <= 'Z' || '0' <= r && r <= '9' ||
		strings.ContainsRune("_@%+=:,./-", r))
}

func newApptainerCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:    "apptainer",
		Short:  "Run containers with Apptainer",
		Hidden: true,
		Args:   noArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return cmd.Help()
		},
	}
	cmd.AddCommand(newApptainerVerbCommand("exec"), newApptainerVerbCommand("shell"))
	return cmd
}

// newApptainerVerbCommand builds an exec or shell command with shared launch checks.
func newApptainerVerbCommand(verb string) *cobra.Command {
	var dryRun bool
	cmd := &cobra.Command{
		Use:               verb + " <tool/version>",
		Short:             "Open a container shell",
		Args:              exactArgs(1),
		ValidArgsFunction: completeContainerNames,
		RunE: func(cmd *cobra.Command, args []string) error {
			containers, err := readContainers()
			if err != nil {
				return err
			}
			c, err := lookupContainer(containers, args[0])
			if err != nil {
				return err
			}
			cfg, err := readConfig()
			if err != nil {
				return err
			}
			loc, err := locate(cfg, c)
			var missing notOnMachineError
			// With no mirrors, pullOnRun only explains, so a dry run shows it too.
			if errors.As(err, &missing) && (!dryRun || len(c.mirrors) == 0) {
				stderr := cmd.ErrOrStderr()
				loc, err = pullOnRun(cfg, c, os.Stdin, stderr, isTerminal(os.Stdin) && isTerminal(stderr))
			}
			if err != nil {
				return err
			}
			settings, err := readLaunchSettings(cfg, verb, c)
			if err != nil {
				return err
			}
			path, err := exec.LookPath(settings.apptainer)
			if err != nil {
				return fmt.Errorf("apptainer not found: %q; install Apptainer or set apptainer_path", settings.apptainer)
			}
			argv, err := apptainerArgs(settings, verb, loc.path, args[1:])
			if err != nil {
				return err
			}
			if dryRun {
				// The resolved path, so the printed line runs the same binary.
				line := shellJoin(append([]string{path}, argv[1:]...))
				_, err := fmt.Fprintln(cmd.OutOrStdout(), line)
				return err
			}
			if verb == "shell" && isTerminal(cmd.ErrOrStderr()) {
				where := c.fileName()
				if loc.buildDate != "" && loc.buildDate != c.buildDate {
					where = c.image + "_" + loc.buildDate
				}
				if loc.share != "" {
					where += ", " + loc.share + " share"
				}
				fmt.Fprintf(cmd.ErrOrStderr(), "sciget: container shell in %s (%s). Type exit to return.\n",
					printable(c.name()), printable(where))
			}
			return runApptainer(path, argv, os.Environ())
		},
	}
	if verb == "exec" {
		cmd.Use += " <command> [args...]"
		cmd.Short = "Run a command in a container"
		cmd.Args = func(cmd *cobra.Command, args []string) error {
			if err := cobra.MinimumNArgs(2)(cmd, args); err != nil {
				return usageError{err.Error()}
			}
			return nil
		}
	}
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "print the Apptainer command instead of running it")
	cmd.Flags().SetInterspersed(false)
	return cmd
}

// completeContainerNames suggests names only for the first argument.
func completeContainerNames(cmd *cobra.Command, args []string, toComplete string) ([]string, cobra.ShellCompDirective) {
	if len(args) > 0 {
		return nil, cobra.ShellCompDirectiveNoFileComp
	}
	containers, err := readContainers()
	if err != nil {
		return nil, cobra.ShellCompDirectiveError
	}
	return completeNames(containers, toComplete, false)
}
