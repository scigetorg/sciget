package main

import (
	"errors"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"
)

// pullContainer downloads c into the store unless it is already on the
// machine, and returns where it is and whether it was downloaded. It tries
// each of download_urls in turn until one gives a file of the right size
// (and SHA-256, if the catalogue has one) that Apptainer can open.
// progress, if not nil, receives the source's host and the bytes so far;
// warn receives a warning to show.
func pullContainer(cfg config, c container, progress func(host string, done, total int64), warn func(string)) (location, bool, error) {
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
		tmp, err := downloadFile(client, u, dest, c.published, report)
		if err == nil {
			err = checkImage(apptainerPath, global, tmp)
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
		if err != nil {
			failures = append(failures, fmt.Sprintf("%s: %v", host, err))
			continue
		}
		if c.published.sha256 == "" && warnMissing && warn != nil {
			warn(fmt.Sprintf("no checksum published for %s; checked its size and that Apptainer can open it", c.name()))
		}
		return location{path: dest, place: "store"}, true, nil
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
func checkImage(apptainer string, global []string, path string) error {
	args := append(append([]string{}, global...), "exec", path, "/bin/true")
	out, err := exec.Command(apptainer, args...).CombinedOutput()
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

func newPullCommand() *cobra.Command {
	return &cobra.Command{
		Use:               "pull <tool/version>",
		Short:             "Download a container into the store",
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
			stderr := cmd.ErrOrStderr()
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
			loc, downloaded, err := pullContainer(cfg, c, progress, func(s string) { warnings = append(warnings, s) })
			if drawn {
				fmt.Fprintln(stderr)
			}
			for _, w := range warnings {
				fmt.Fprintln(stderr, "warning: "+w)
			}
			if err != nil {
				return err
			}
			if !downloaded {
				_, err = fmt.Fprintf(cmd.OutOrStdout(), "%s is already on the machine: %s\n", c.name(), loc.path)
				return err
			}
			_, err = fmt.Fprintf(cmd.OutOrStdout(), "Pulled %s to %s\n", c.name(), loc.path)
			return err
		},
	}
}
