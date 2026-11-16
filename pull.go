package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"syscall"

	"github.com/spf13/cobra"
)

// pullContainer downloads c into the store unless it is already on the
// machine, and returns where it is and whether it was downloaded. It tries
// each of download_urls in turn until one gives a file of the right size
// (and SHA-256, if the catalogue has one) that Apptainer can open.
// progress, if not nil, receives the source's host and the bytes so far;
// warn receives a warning to show. Cancelling ctx stops the pull and
// deletes its temporary file.
func pullContainer(ctx context.Context, cfg config, c container, progress func(host string, done, total int64), warn func(string)) (location, bool, error) {
	loc, err := locate(cfg, c)
	var missing notOnMachineError
	if !errors.As(err, &missing) {
		return loc, false, err
	}

	dir, _, err := cfg.get("container_dir")
	if err != nil {
		return location{}, false, err
	}
	layout, _, err := cfg.get("container_layout")
	if err != nil {
		return location{}, false, err
	}
	relative, err := layoutPath(layout, c)
	if err != nil {
		return location{}, false, err
	}
	dest := filepath.Join(dir, relative)
	apptainer, _, err := cfg.get("apptainer_path")
	if err != nil {
		return location{}, false, err
	}
	// Checked first, so a missing Apptainer does not cost a download.
	apptainerPath, err := exec.LookPath(apptainer)
	if err != nil {
		return location{}, false, fmt.Errorf("apptainer not found: %q; install Apptainer or set apptainer_path", apptainer)
	}
	global, err := cfg.getList("apptainer_global_args")
	if err != nil {
		return location{}, false, err
	}
	warnMissing, err := cfg.getBool("warn_missing_checksum")
	if err != nil {
		return location{}, false, err
	}
	urls, err := downloadURLs(cfg, c)
	if err != nil {
		return location{}, false, err
	}
	client, err := newDownloadClient(cfg)
	if err != nil {
		return location{}, false, err
	}
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return location{}, false, err
	}

	var failures []string
	for _, u := range urls {
		host := u
		if parsed, err := url.Parse(u); err == nil {
			host = parsed.Host
		}
		var report func(done, total int64)
		if progress != nil {
			report = func(done, total int64) { progress(host, done, total) }
		}
		tmp, err := downloadFile(ctx, client, u, dest, c.published, report)
		if err == nil {
			err = checkImage(ctx, apptainerPath, global, tmp)
			if err == nil {
				err = os.Chmod(tmp, 0o644)
			}
			if err == nil {
				err = os.Rename(tmp, dest)
			}
			if err != nil {
				os.Remove(tmp)
			}
		}
		if ctx.Err() != nil {
			return location{}, false, fmt.Errorf("pulling %s: %w", c.name(), context.Cause(ctx))
		}
		if err != nil {
			failures = append(failures, fmt.Sprintf("%s: %v", host, err))
			continue
		}
		if c.published.sha256 == "" && warnMissing && warn != nil {
			warn(fmt.Sprintf("no checksum published for %s; checked its size and that Apptainer can open it", c.name()))
		}
		return location{path: dest}, true, nil
	}
	return location{}, false, fmt.Errorf("could not download %s:\n  %s", c.name(), strings.Join(failures, "\n  "))
}

// downloadURLs returns c's download URLs: each of download_urls with {file}
// replaced by the image's file name.
func downloadURLs(cfg config, c container) ([]string, error) {
	templates, err := cfg.getList("download_urls")
	if err != nil {
		return nil, err
	}
	file := url.PathEscape(c.fileName() + ".simg")
	urls := make([]string, len(templates))
	for i, t := range templates {
		urls[i] = strings.Replace(t, "{file}", file, 1)
	}
	return urls, nil
}

// checkImage returns an error unless Apptainer can run /bin/true in the
// image at path.
func checkImage(ctx context.Context, apptainer string, global []string, path string) error {
	args := append(append([]string{}, global...), "exec", path, "/bin/true")
	out, err := exec.CommandContext(ctx, apptainer, args...).CombinedOutput()
	if err == nil {
		return nil
	}
	err = fmt.Errorf("apptainer cannot open the download: %w", err)
	if out := strings.TrimSpace(string(out)); out != "" {
		lines := strings.Split(out, "\n")
		err = fmt.Errorf("%w: %s", err, printable(lines[len(lines)-1]))
	}
	return err
}

// pullWithProgress runs pullContainer, drawing its progress on stderr when
// stderr is a terminal and printing any warning there.
func pullWithProgress(ctx context.Context, stderr io.Writer, cfg config, c container) (location, bool, error) {
	var progress func(host string, done, total int64)
	drawn := false
	if isTerminal(stderr) {
		progress = func(host string, done, total int64) {
			drawn = true
			percent := int64(100)
			if total > 0 {
				percent = done * 100 / total
			}
			// "\x1b[K" clears what a longer earlier line left.
			fmt.Fprintf(stderr, "\rDownloading %s from %s: %s of %s (%d%%)\x1b[K",
				c.name(), host, humanSize(done), humanSize(total), percent)
		}
	}
	var warnings []string
	loc, downloaded, err := pullContainer(ctx, cfg, c, progress, func(s string) { warnings = append(warnings, s) })
	if drawn {
		fmt.Fprintln(stderr)
	}
	for _, w := range warnings {
		fmt.Fprintln(stderr, "warning: "+w)
	}
	return loc, downloaded, err
}

// pullOnRun is what running a container that is not on the machine does:
// with auto_pull set, pull it; when interactive, ask first; otherwise fail
// with the ways to get it. "Always" saves auto_pull before pulling. Ctrl-C
// stops the pull cleanly; while the question waits, it ends sciget as
// usual, since nothing has been written yet.
func pullOnRun(cfg config, c container, in io.Reader, stderr io.Writer, interactive bool) (location, error) {
	auto, err := cfg.getBool("auto_pull")
	if err != nil {
		return location{}, err
	}
	if !auto {
		if !interactive {
			return location{}, notOnMachineError{name: c.name()}
		}
		question, err := pullQuestion(cfg, c)
		if err != nil {
			return location{}, err
		}
		fmt.Fprint(stderr, question)
		// Only a whole line is an answer: end of input (Ctrl-D) or a read
		// error is not the empty answer that means Always.
		line, err := bufio.NewReader(in).ReadString('\n')
		if err != nil {
			fmt.Fprintln(stderr)
			return location{}, notOnMachineError{name: c.name()}
		}
		pull, always := answerPull(line)
		if !pull {
			return location{}, notOnMachineError{name: c.name()}
		}
		if always {
			// A fresh copy, so a failed save changes nothing in memory.
			fresh, err := readConfig()
			if err == nil {
				err = fresh.set("auto_pull", "true")
			}
			if err != nil {
				return location{}, fmt.Errorf("saving auto_pull: %w", err)
			}
			cfg = fresh
		}
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	loc, _, err := pullWithProgress(ctx, stderr, cfg, c)
	return loc, err
}

// pullQuestion returns the question asked before pulling c: where it was
// looked for, what may be wrong with the share, and where it would go.
func pullQuestion(cfg config, c container) (string, error) {
	store, _, err := cfg.get("container_dir")
	if err != nil {
		return "", err
	}
	_, _, catalogues, err := readCatalogues()
	if err != nil {
		return "", err
	}
	var where string
	switch cat, mounted := containerShare(catalogues, c); {
	case cat.Dir == "":
		where = c.name() + " hasn't been downloaded.\n"
	case !mounted:
		where = fmt.Sprintf("%s is not on this machine: the %s share (%s) is not mounted.\n"+
			"If it should be, fix CVMFS first.\n", c.name(), cat.Name, cat.Dir)
	default:
		where = fmt.Sprintf("%s is not on the %s share (%s) and hasn't been downloaded.\n"+
			"If the share should have it, check CVMFS first.\n", c.name(), cat.Name, cat.Dir)
	}
	var lines []string
	for _, line := range strings.Split(strings.TrimSuffix(where, "\n"), "\n") {
		lines = append(lines, printable(line))
	}
	return strings.Join(lines, "\n") + "\nDownload it to " + printable(store) + "?\n" +
		"[y]es, [n]o, [A]lways (yes, saved as the default): ", nil
}

// containerShare returns the catalogue c comes from, whose Dir is its share
// ("" for none), and whether that share's folder exists.
func containerShare(catalogues []catalogue, c container) (catalogue, bool) {
	i := findCatalogue(catalogues, c.catalogue)
	if i < 0 || catalogues[i].Dir == "" {
		return catalogue{}, false
	}
	_, err := os.Stat(catalogues[i].Dir)
	return catalogues[i], !errors.Is(err, fs.ErrNotExist)
}

// answerPull reads an answer to pullQuestion: an empty line or "a" means
// pull and save auto_pull, "y" means pull this once, anything else no.
func answerPull(line string) (pull, always bool) {
	switch strings.ToLower(strings.TrimSpace(line)) {
	case "", "a", "always":
		return true, true
	case "y", "yes":
		return true, false
	}
	return false, false
}

// pullAll pulls cs, up to parallel_pulls at a time, printing a line on
// stdout as each finishes. On a terminal, one line on stderr shows the
// bytes so far across all of them. Every failure is listed in the error.
func pullAll(ctx context.Context, stdout, stderr io.Writer, cfg config, cs []container) error {
	if len(cs) == 1 {
		loc, downloaded, err := pullWithProgress(ctx, stderr, cfg, cs[0])
		if err != nil {
			return err
		}
		return printPulled(stdout, cs[0], loc, downloaded)
	}
	limit, err := cfg.getNumber("parallel_pulls")
	if err != nil {
		return err
	}
	terminal := isTerminal(stderr)
	var (
		mu         sync.Mutex // guards the rest, and the output
		done, size = make([]int64, len(cs)), make([]int64, len(cs))
		failures   []string
		warnings   []string
		wg         sync.WaitGroup
	)
	// draw redraws the combined progress line; mu must be held.
	draw := func() {
		if terminal {
			fmt.Fprintf(stderr, "\rDownloading %d containers: %s of %s\x1b[K", len(cs), humanSize(sum(done)), humanSize(sum(size)))
		}
	}
	slots := make(chan struct{}, limit)
	for i, c := range cs {
		wg.Go(func() {
			slots <- struct{}{}
			defer func() { <-slots }()
			loc, downloaded, err := pullContainer(ctx, cfg, c, func(_ string, d, t int64) {
				mu.Lock()
				defer mu.Unlock()
				done[i], size[i] = d, t
				draw()
			}, func(s string) {
				mu.Lock()
				defer mu.Unlock()
				warnings = append(warnings, s)
			})
			mu.Lock()
			defer mu.Unlock()
			if terminal {
				fmt.Fprint(stderr, "\r\x1b[K")
			}
			if err == nil {
				err = printPulled(stdout, c, loc, downloaded)
			}
			if err != nil {
				failures = append(failures, err.Error())
			}
			draw()
		})
	}
	wg.Wait()
	if terminal {
		fmt.Fprint(stderr, "\r\x1b[K")
	}
	for _, w := range warnings {
		fmt.Fprintln(stderr, "warning: "+w)
	}
	if len(failures) > 0 {
		return fmt.Errorf("%d of %d containers failed:\n%s", len(failures), len(cs), strings.Join(failures, "\n"))
	}
	return nil
}

func sum(ns []int64) int64 {
	var total int64
	for _, n := range ns {
		total += n
	}
	return total
}

// printPulled prints the line for one finished pull.
func printPulled(stdout io.Writer, c container, loc location, downloaded bool) error {
	if !downloaded {
		_, err := fmt.Fprintf(stdout, "%s is already on the machine: %s\n", c.name(), loc.path)
		return err
	}
	_, err := fmt.Fprintf(stdout, "Pulled %s to %s\n", c.name(), loc.path)
	return err
}

func newPullCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "pull <tool/version>...",
		Short: "Download containers to this machine",
		Args: func(cmd *cobra.Command, args []string) error {
			if len(args) == 0 {
				return usageError{"requires at least 1 arg(s), only received 0"}
			}
			return nil
		},
		// Every argument is a name, so each is completed.
		ValidArgsFunction: func(cmd *cobra.Command, args []string, toComplete string) ([]string, cobra.ShellCompDirective) {
			return completeContainerNames(cmd, nil, toComplete)
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			containers, err := readContainers()
			if err != nil {
				return err
			}
			// Every name is checked before anything downloads.
			var cs []container
			for _, name := range args {
				c, err := lookupContainer(containers, name)
				if err != nil {
					return err
				}
				if !slices.ContainsFunc(cs, func(x container) bool { return x.name() == c.name() }) {
					cs = append(cs, c)
				}
			}
			cfg, err := readConfig()
			if err != nil {
				return err
			}
			ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
			defer stop()
			return pullAll(ctx, cmd.OutOrStdout(), cmd.ErrOrStderr(), cfg, cs)
		},
	}
}
