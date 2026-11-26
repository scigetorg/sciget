package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// testCatalog is one catalog a TestRun row registers.
type testCatalog struct {
	name   string
	url    string // exampleURL(name), or the test server's, when empty
	domain string
	apps   string // apps.json in its cache dir; none when empty
	dir    string
	layout string
	unused bool // registered with "use": false
	// noMirrors leaves downloadUrls out of its cached catalog.json.
	noMirrors bool
}

// exampleDir holds the catalog JSONs exampleURL writes.
var exampleDir string

// testCatalogJSON is the catalog JSON of every test catalog.
const testCatalogJSON = `{"schemaVersion": 1, "appJsonUrl": "apps.json", "downloadUrls": ["https://example.org/{file}"]}`

// TestMain points the system layer at an empty folder, so no test reads
// the real /etc/sciget, and makes the folder exampleURL writes in.
func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "sciget-system-")
	if err != nil {
		panic(err)
	}
	systemDir = dir
	if exampleDir, err = os.MkdirTemp("", "sciget-examples-"); err != nil {
		panic(err)
	}
	code := m.Run()
	os.RemoveAll(dir)
	os.RemoveAll(exampleDir)
	os.Exit(code)
}

// setSystemDir points the system layer at dir for one test.
func setSystemDir(t *testing.T, dir string) {
	t.Helper()
	old := systemDir
	systemDir = dir
	t.Cleanup(func() { systemDir = old })
}

// exampleDomains returns the path of a local domains file with the keys
// eeg and neuro, writing it and each one's apps.json beside it if needed.
func exampleDomains(eeg, neuro string) string {
	dir := filepath.Join(exampleDir, "domains")
	files := map[string]string{
		"domains.json": `{"schemaVersion": 1, "domains": {
			"eeg": {"appJsonUrl": "eeg/apps.json", "downloadUrls": ["https://example.org/{file}"]},
			"neuro": {"appJsonUrl": "neuro/apps.json", "downloadUrls": ["https://example.org/{file}"], "registry": "x"}}}`,
		"eeg/apps.json":   eeg,
		"neuro/apps.json": neuro,
	}
	for name, data := range files {
		path := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			panic(err)
		}
		if err := os.WriteFile(path, []byte(data), 0o644); err != nil {
			panic(err)
		}
	}
	return filepath.Join(dir, "domains.json")
}

// exampleURL returns the path of a local catalog JSON for name, writing it
// if needed. A testCatalog gets it when no URL is given and nothing is
// served.
func exampleURL(name string) string {
	path := filepath.Join(exampleDir, name, "catalog.json")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		panic(err)
	}
	if err := os.WriteFile(path, []byte(testCatalogJSON), 0o644); err != nil {
		panic(err)
	}
	return path
}

func TestRun(t *testing.T) {
	const helpText = "Usage:\n  sciget [flags]\n  sciget [command]\n"
	const usageHint = "\nRun 'sciget help' for usage.\n"
	const fslCatalog = `{"fsl": {"categories": ["image registration"], "apps": {
		"fsl 6.0.3": {"version": "20200905", "exec": ""},
		"fsleyesGUI-fsl 6.0.3": {"version": "20200905", "exec": "fsleyes"},
		"fsl 6.0.3 arm64": {"version": "20260101", "image": "fsl_6.0.3_arm64", "show_in_applist": false}}}}`
	const labCatalog = `{"fsl": {"apps": {"fsl 6.0.3": {"version": "20240101"}}}}`
	const labCatalogJSONApps = `{"eeg": {"apps": {"eeg 1.0": {"version": "20240101"}}}}`
	const fslCatalogJSONApps = `{"fsl": {"apps": {"fsl 6.0.3": {"version": "20200905"}}}}`
	const hiddenCatalog = `{"fsl": {"apps": {"fsl 6.0.3": {"version": "1", "show_in_applist": false}}}}`
	mirrorURL := exampleURL("mirror")
	labURL := exampleURL("lab")
	const noCatalogs = "sciget: no catalogs; run 'sciget catalog add <name> <url>'\n"
	const noneInUse = "sciget: no catalogs in use; run 'sciget catalog use <name>'\n"
	neuroimaging := []testCatalog{{name: "neuroimaging", apps: fslCatalog}}
	const fslStored = "fsl_6.0.3_20200905/fsl_6.0.3_20200905.simg"
	const fuseCatalog = `{"fsl": {"apps": {"fsl 6.0.3": {"version": "20200905", "apptainer_args": ["--bind", "/dev/fuse"]}}}}`
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	physicalCwd, err := filepath.EvalSymlinks(cwd)
	if err != nil {
		t.Fatal(err)
	}
	domainsURL := exampleDomains(labCatalogJSONApps, fslCatalogJSONApps)
	badURL := filepath.Join(exampleDir, "bad.json")
	if err := os.WriteFile(badURL, []byte(`{"appJsonUrl": "apps.json"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	emptyURL := filepath.Join(exampleDir, "empty.json")
	if err := os.WriteFile(emptyURL, []byte(`{"schemaVersion": 1}`), 0o644); err != nil {
		t.Fatal(err)
	}
	// Beside domainsURL, so neuro's apps list is there; soon has none yet.
	placeholderURL := filepath.Join(filepath.Dir(domainsURL), "placeholder.json")
	if err := os.WriteFile(placeholderURL, []byte(`{"schemaVersion": 1, "domains": {
		"neuro": {"appJsonUrl": "neuro/apps.json", "downloadUrls": ["https://example.org/{file}"]},
		"soon": {"source_repositories": ["x"]}}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	relativeURL, err := filepath.Rel(cwd, exampleURL("relative"))
	if err != nil {
		t.Fatal(err)
	}
	// Every key after cache_dir at its default, as config list prints it
	// with XDG_DATA_HOME=/data.
	excludes, err := defaultValue("finder_excludes")
	if err != nil {
		t.Fatal(err)
	}
	otherDefaults := "container_dir\t/data/sciget/containers\tdefault\n" +
		"container_layout\t{tool}_{version}_{date}/{tool}_{version}_{date}.simg\tdefault\n" +
		"module_dir\t/data/sciget/modules\tdefault\n" +
		"module_sciget\tsciget\tdefault\n" +
		"finder_excludes\t" + excludes + "\tdefault\n" +
		"apptainer_path\tapptainer\tdefault\n" +
		"apptainer_global_args\t[\"--silent\"]\tdefault\n" +
		"apptainer_exec_args\t[\"--cleanenv\",\"{env}\",\"{xauthority}\",\"{tmp}\",\"{pwd}\"]\tdefault\n" +
		"apptainer_shell_args\t[\"--cleanenv\",\"{env}\",\"{xauthority}\",\"{tmp}\",\"{pwd}\"]\tdefault\n" +
		"apptainer_extra_args\t[]\tdefault\n" +
		"apptainer_pass_env\t[\"DISPLAY\"]\tdefault\n" +
		"xauthority_var\tXAUTHORITY\tdefault\n" +
		"tmp_vars\t[\"TMP\",\"TMPDIR\",\"TEMP\",\"TEMPDIR\"]\tdefault\n" +
		"tmp_target\t/tmp\tdefault\n" +
		"use_catalog_args\ttrue\tdefault\n" +
		"max_catalog_size\t67108864\tdefault\n" +
		"display_start_command\t[]\tdefault\n" +
		"display_start_seconds\t30\tdefault\n" +
		"launch_check_seconds\t5\tdefault\n" +
		"network_timeout_seconds\t30\tdefault\n" +
		"update_url\thttps://github.com/scigetorg/sciget/releases/latest\tdefault\n" +
		"warn_missing_checksum\tfalse\tdefault\n" +
		"auto_pull\tfalse\tdefault\n" +
		"parallel_pulls\t3\tdefault\n"
	var keyNames []string
	for _, key := range configKeys {
		keyNames = append(keyNames, key.name)
	}

	tests := []struct {
		name             string
		args             []string
		before           [][]string        // commands run first, each of which must succeed
		config           string            // written to SCIGET_CONFIG; no file when empty
		catalogs         []testCatalog     // written to catalogs.json, in order
		systemCatalogs   []testCatalog     // written to the system catalogs.json
		systemConfig     string            // written to the system config.json
		wantSystemList   []catalog         // the system catalogs.json afterwards, when set
		wantSystemConfig string            // the system config.json afterwards, when set
		list             string            // written to catalogs.json instead, when set
		readOnlyList     bool              // write catalogs.json with mode 0o400
		lockedCaches     bool              // cache_dir/catalogs allows no deletions
		serve            map[string]string // when set, a test server serves each body at /<name>/apps.json, with testCatalogJSON at /<name>/catalog.json; else 404
		readOnly         bool              // write config with mode 0o400
		env              map[string]string // set after the defaults
		stored           []string          // paths created under the default container_dir
		wantExec         []string          // argv handed to Apptainer, when set; <store> and <cwd> stand for those paths
		wantConfig       string            // the config file afterwards, when set
		wantList         []catalog         // catalogs.json afterwards, when set
		wantListSame     bool              // catalogs.json is byte for byte as written
		wantCaches       []string          // names of the dirs in cache_dir/catalogs afterwards, when set
		wantRequests     []string          // paths the server was asked for, when set
		wantCode         int
		wantStdout       string // exact, unless wantStdoutHas is set; <store> and <cwd> as in wantExec
		wantStdoutHas    string // checks only that stdout contains this
		wantStdoutLacks  string
		wantStderr       string // exact, unless wantStderrHas is set
		wantStderrHas    string
	}{
		{name: "no arguments", wantCode: 2,
			wantStderr: "sciget: no command given" + usageHint},
		{name: "ui without a terminal", args: []string{"ui"}, wantCode: 1,
			wantStderr: "sciget: sciget ui needs a terminal\n"},
		{name: "help", args: []string{"help"}, wantStdoutHas: helpText},
		{name: "help for a command", args: []string{"help", "version"},
			wantStdoutHas: "Usage:\n  sciget version [flags]\n"},
		{name: "unknown help topic", args: []string{"help", "missing"},
			wantStderrHas: "Unknown help topic [`missing`]\n" + helpText},
		{name: "-h", args: []string{"-h"}, wantStdoutHas: helpText},
		{name: "--help", args: []string{"--help"}, wantStdoutHas: helpText},
		{name: "version", args: []string{"version"}, wantStdout: "sciget " + version + "\n"},
		{name: "--version", args: []string{"--version"}, wantStdout: "sciget " + version + "\n"},
		{name: "-v", args: []string{"-v"}, wantStdout: "sciget " + version + "\n"},
		{name: "--version after a command", args: []string{"missing", "--version"},
			wantStdout: "sciget " + version + "\n"},
		{name: "--version is for the root only", args: []string{"version", "--version"}, wantCode: 2,
			wantStderr: "sciget: unknown flag: --version" + usageHint},
		{name: "single-dash long flag", args: []string{"-version"}, wantCode: 2,
			wantStderr: "sciget: unknown shorthand flag: 'e' in -ersion" + usageHint},
		{name: "unknown command", args: []string{"missing"}, wantCode: 2,
			wantStderr: "sciget: unknown command \"missing\" for \"sciget\"" + usageHint},
		{name: "unknown flag", args: []string{"--bogus"}, wantCode: 2,
			wantStderr: "sciget: unknown flag: --bogus" + usageHint},
		{name: "version takes no arguments", args: []string{"version", "x"}, wantCode: 2,
			wantStderr: "sciget: unknown command \"x\" for \"sciget version\"" + usageHint},
		{name: "completion", args: []string{"completion", "bash"},
			wantStdoutHas: "bash completion V2 for sciget"},
		{name: "completion takes no arguments", args: []string{"completion", "bash", "x"}, wantCode: 2,
			wantStderr: "sciget: unknown command \"x\" for \"sciget completion bash\"" + usageHint},

		{name: "find no match", args: []string{"find", "spm"}, catalogs: neuroimaging,
			wantStderr: "No containers match \"spm\".\n"},
		{name: "find no match joins words", args: []string{"find", "spm", "12"}, catalogs: neuroimaging,
			wantStderr: "No containers match \"spm 12\".\n"},
		{name: "info unknown", args: []string{"info", "fsl/9"}, catalogs: neuroimaging, wantCode: 1,
			wantStderr: "sciget: no container named \"fsl/9\"; run 'sciget find fsl' to list versions, or 'sciget refresh' to update the catalogs\n"},
		{name: "info", args: []string{"info", "fsl/6.0.3"}, catalogs: neuroimaging,
			wantStdout: "Name:        fsl/6.0.3\nCatalog:     neuroimaging\nBuild:       20200905 (fsl_6.0.3_20200905)\nCategories:  image registration\nLicence:     (not stated)\nDOI:         (none)\nGUI:         yes\nLocation:    not on this machine\n"},
		{name: "info missing argument", args: []string{"info"}, wantCode: 2,
			wantStderr: "sciget: accepts 1 arg(s), received 0" + usageHint},
		{name: "info completes names", args: []string{"__complete", "info", "fsl/"}, catalogs: neuroimaging,
			wantStdout: "fsl/6.0.3\tbuilt 20200905\n:4\n", wantStderrHas: "Completion ended with directive"},

		{name: "apptainer hidden from help", args: []string{"--help"}, wantStdoutHas: "info ", wantStdoutLacks: "apptainer"},
		{name: "apptainer exec", args: []string{"apptainer", "exec", "fsl/6.0.3", "bet", "--help"},
			catalogs: neuroimaging, config: `{"apptainer_path": "/bin/sh"}`, stored: []string{fslStored},
			wantExec: []string{"/bin/sh", "--silent", "exec", "--cleanenv", "--pwd", "<cwd>", "<store>/" + fslStored, "bet", "--help"}},
		{name: "apptainer shell", args: []string{"apptainer", "shell", "fsl/6.0.3"},
			catalogs: neuroimaging, config: `{"apptainer_path": "/bin/sh"}`, stored: []string{fslStored},
			wantExec: []string{"/bin/sh", "--silent", "shell", "--cleanenv", "--pwd", "<cwd>", "<store>/" + fslStored}},
		{name: "apptainer dry run", args: []string{"apptainer", "exec", "--dry-run", "fsl/6.0.3", "bet", "", "it's"},
			catalogs: neuroimaging, config: `{"apptainer_path": "/bin/sh"}`, stored: []string{fslStored},
			wantStdout: "/bin/sh --silent exec --cleanenv --pwd <cwd> <store>/" + fslStored + " bet '' 'it'\\''s'\n"},
		{name: "apptainer dry run with display and extra args", args: []string{"apptainer", "shell", "--dry-run", "fsl/6.0.3"},
			catalogs: neuroimaging, config: `{"apptainer_path": "/bin/sh", "apptainer_extra_args": ["--writable-tmpfs"]}`,
			stored: []string{fslStored}, env: map[string]string{"DISPLAY": ":1"},
			wantStdout: "/bin/sh --silent shell --cleanenv --env DISPLAY=:1 --pwd <cwd> --writable-tmpfs <store>/" + fslStored + "\n"},
		{name: "apptainer catalog args", args: []string{"apptainer", "shell", "--dry-run", "fsl/6.0.3"},
			catalogs: []testCatalog{{name: "neuroimaging", apps: fuseCatalog}}, config: `{"apptainer_path": "/bin/sh"}`,
			stored:     []string{fslStored},
			wantStdout: "/bin/sh --silent shell --cleanenv --pwd <cwd> --bind /dev/fuse <store>/" + fslStored + "\n"},
		{name: "apptainer catalog args turned off", args: []string{"apptainer", "shell", "--dry-run", "fsl/6.0.3"},
			catalogs: []testCatalog{{name: "neuroimaging", apps: fuseCatalog}},
			config:   `{"apptainer_path": "/bin/sh", "use_catalog_args": false}`, stored: []string{fslStored},
			wantStdout: "/bin/sh --silent shell --cleanenv --pwd <cwd> <store>/" + fslStored + "\n"},
		{name: "apptainer exec with overridden settings", args: []string{"apptainer", "exec", "--dry-run", "fsl/6.0.3", "true"},
			catalogs: neuroimaging, stored: []string{fslStored},
			config: `{"apptainer_path": "/bin/sh", "apptainer_global_args": ["--debug"],
				"apptainer_exec_args": ["--contain", "{env}", "{xauthority}", "{tmp}"], "apptainer_pass_env": ["MYVAR"],
				"xauthority_var": "MYXAUTH", "tmp_vars": ["MYTMP"], "tmp_target": "/scratch"}`,
			env: map[string]string{"MYVAR": "v", "MYXAUTH": filepath.Join(physicalCwd, "main_test.go"), "MYTMP": "/t", "DISPLAY": ":0"},
			wantStdout: "/bin/sh --debug exec --contain --env MYVAR=v --bind <cwd>/main_test.go:<cwd>/main_test.go:ro" +
				" --env MYXAUTH=<cwd>/main_test.go --bind /t:/scratch <store>/" + fslStored + " true\n"},
		{name: "apptainer shell with overridden settings", args: []string{"apptainer", "shell", "--dry-run", "fsl/6.0.3"},
			catalogs: neuroimaging, stored: []string{fslStored},
			config:     `{"apptainer_path": "/bin/sh", "apptainer_global_args": [], "apptainer_shell_args": ["--no-home"]}`,
			wantStdout: "/bin/sh shell --no-home <store>/" + fslStored + "\n"},
		{name: "apptainer display with a comma", args: []string{"apptainer", "shell", "fsl/6.0.3"},
			catalogs: neuroimaging, config: `{"apptainer_path": "/bin/sh"}`, stored: []string{fslStored},
			env: map[string]string{"DISPLAY": "a,b"}, wantCode: 1,
			wantStderr: "sciget: cannot pass DISPLAY to the container: its value contains \",\"\n"},
		{name: "apptainer exec unknown name", args: []string{"apptainer", "exec", "fsl/9", "bet"},
			catalogs: neuroimaging, wantCode: 1,
			wantStderr: "sciget: no container named \"fsl/9\"; run 'sciget find fsl' to list versions, or 'sciget refresh' to update the catalogs\n"},
		{name: "apptainer shell unknown name", args: []string{"apptainer", "shell", "fsl/9"},
			catalogs: neuroimaging, wantCode: 1,
			wantStderr: "sciget: no container named \"fsl/9\"; run 'sciget find fsl' to list versions, or 'sciget refresh' to update the catalogs\n"},
		{name: "apptainer unknown verb", args: []string{"apptainer", "exex", "fsl/6.0.3", "bet"}, wantCode: 2,
			wantStderr: "sciget: unknown command \"exex\" for \"sciget apptainer\"" + usageHint},
		{name: "apptainer alone shows help", args: []string{"apptainer"}, wantStdoutHas: "Run containers with Apptainer"},
		{name: "apptainer exec not on this machine", args: []string{"apptainer", "exec", "fsl/6.0.3", "bet"},
			catalogs: neuroimaging, wantCode: 1, wantStderr: "sciget: fsl/6.0.3 is not on this machine; to download it, run: sciget pull fsl/6.0.3, or let sciget download when needed: sciget config set auto_pull true\n"},
		{name: "apptainer exec not on this machine, no mirrors", args: []string{"apptainer", "exec", "fsl/6.0.3", "bet"},
			catalogs: []testCatalog{{name: "neuroimaging", apps: fslCatalog, noMirrors: true}}, wantCode: 1,
			wantStderr: "sciget: fsl/6.0.3 hasn't been downloaded. Catalog \"neuroimaging\" has no download mirrors, so there is nothing to run until the share has it or the catalog adds downloadUrls.\n"},
		{name: "apptainer dry run not on this machine, no mirrors", args: []string{"apptainer", "exec", "--dry-run", "fsl/6.0.3", "bet"},
			catalogs: []testCatalog{{name: "neuroimaging", apps: fslCatalog, noMirrors: true}}, wantCode: 1,
			wantStderrHas: "has no download mirrors, so there is nothing to run"},
		{name: "apptainer not found", args: []string{"apptainer", "shell", "fsl/6.0.3"},
			catalogs: neuroimaging, config: `{"apptainer_path": "no-such-apptainer"}`, stored: []string{fslStored},
			wantCode: 1, wantStderr: "sciget: apptainer not found: \"no-such-apptainer\"; install Apptainer or set apptainer_path\n"},
		{name: "apptainer flag before the name", args: []string{"apptainer", "exec", "--bind", "/x", "fsl/6.0.3", "bet"},
			catalogs: neuroimaging, wantCode: 2, wantStderrHas: "unknown flag: --bind"},
		{name: "apptainer exec needs a command", args: []string{"apptainer", "exec", "fsl/6.0.3"},
			catalogs: neuroimaging, wantCode: 2, wantStderrHas: "requires at least 2 arg(s)"},
		{name: "apptainer shell takes one name", args: []string{"apptainer", "shell", "fsl/6.0.3", "bash"},
			catalogs: neuroimaging, wantCode: 2, wantStderrHas: "accepts 1 arg(s), received 2"},

		{name: "find without catalogs", args: []string{"find"}, wantCode: 1, wantStderr: noCatalogs},
		{name: "find with none in use", args: []string{"find"}, wantCode: 1, wantStderr: noneInUse,
			catalogs: []testCatalog{{name: "neuroimaging", apps: fslCatalog, unused: true}}},
		{name: "find before a refresh", args: []string{"find"}, wantCode: 1,
			catalogs:      []testCatalog{{name: "neuroimaging"}},
			wantStderrHas: "/catalogs/neuroimaging-"},
		{name: "find", args: []string{"find", "fsl"}, catalogs: neuroimaging,
			wantStdout: "fsl/6.0.3\tneuroimaging\timage registration\tyes\t\n"},
		{name: "find --all", args: []string{"find", "--all", "fsl"}, catalogs: neuroimaging,
			wantStdout: "fsl/6.0.3\tneuroimaging\timage registration\tyes\t\n" +
				"fsl/6.0.3_arm64\tneuroimaging\timage registration\t\t\n"},
		{name: "find marks a shadowed copy", args: []string{"find", "fsl"},
			catalogs:   []testCatalog{{name: "mylab", apps: labCatalog}, {name: "neuroimaging", apps: fslCatalog}},
			wantStdout: "fsl/6.0.3\tmylab\t\t\t\nfsl/6.0.3\tneuroimaging\timage registration\tyes\t(shadowed)\n"},
		{name: "find follows catalog order", args: []string{"find", "fsl"},
			catalogs:   []testCatalog{{name: "neuroimaging", apps: fslCatalog}, {name: "mylab", apps: labCatalog}},
			wantStdout: "fsl/6.0.3\tneuroimaging\timage registration\tyes\t\nfsl/6.0.3\tmylab\t\t\t(shadowed)\n"},
		{name: "find follows a reorder by catalog use", before: [][]string{{"catalog", "use", "mylab"}},
			args:       []string{"find", "fsl"},
			catalogs:   []testCatalog{{name: "neuroimaging", apps: fslCatalog}, {name: "mylab", apps: labCatalog}},
			wantStdout: "fsl/6.0.3\tmylab\t\t\t\nfsl/6.0.3\tneuroimaging\timage registration\tyes\t(shadowed)\n"},
		{name: "a hidden copy still shadows", args: []string{"find", "fsl"},
			catalogs:   []testCatalog{{name: "a", apps: hiddenCatalog}, {name: "neuroimaging", apps: fslCatalog}},
			wantStdout: "fsl/6.0.3\tneuroimaging\timage registration\tyes\t(shadowed)\n"},
		{name: "find skips catalogs not in use", args: []string{"find", "fsl"},
			catalogs:   []testCatalog{{name: "mylab", apps: labCatalog, unused: true}, {name: "neuroimaging", apps: fslCatalog}},
			wantStdout: "fsl/6.0.3\tneuroimaging\timage registration\tyes\t\n"},
		{name: "find with one catalog not downloaded", args: []string{"find"}, wantCode: 1,
			catalogs:      []testCatalog{{name: "mylab"}, {name: "neuroimaging", apps: fslCatalog}},
			wantStderrHas: "/catalogs/mylab-"},
		{name: "find completes versions", args: []string{"__complete", "find", "fsl/"}, catalogs: neuroimaging,
			wantStdout: "fsl/6.0.3\tbuilt 20200905\n:4\n", wantStderrHas: "Completion ended with directive"},
		{name: "find completes the copy that runs", args: []string{"__complete", "find", "fsl/"},
			catalogs:   []testCatalog{{name: "mylab", apps: labCatalog}, {name: "neuroimaging", apps: fslCatalog}},
			wantStdout: "fsl/6.0.3\tbuilt 20240101\n:4\n", wantStderrHas: "Completion ended with directive"},
		{name: "find --all completes hidden versions", args: []string{"__complete", "find", "--all", "fsl/"},
			catalogs: neuroimaging, wantStdout: "fsl/6.0.3\tbuilt 20200905\nfsl/6.0.3_arm64\tbuilt 20260101\n:4\n",
			wantStderrHas: "Completion ended with directive"},
		{name: "find completes tool names", args: []string{"__complete", "find", "fs"}, catalogs: neuroimaging,
			wantStdout: "fsl/\n:6\n", wantStderrHas: "Completion ended with directive"},
		{name: "find completion without catalogs", args: []string{"__complete", "find", "fs"},
			wantStdout: ":1\n", wantStderrHas: "Completion ended with directive: ShellCompDirectiveError"},
		{name: "find reads cache_dir from the config", args: []string{"find"},
			config: `{"cache_dir": "/nowhere"}`, catalogs: []testCatalog{{name: "neuroimaging"}}, wantCode: 1,
			wantStderrHas: "sciget: no catalog at /nowhere/catalogs/neuroimaging-"},
		{name: "find with a broken config", args: []string{"find"}, catalogs: neuroimaging,
			config: `{"cache-dir": "/c"}`, wantCode: 1, wantStderrHas: "sciget: reading config "},
		{name: "find with a broken list", args: []string{"find"}, list: "{", wantCode: 1,
			wantStderrHas: "sciget: reading catalogs "},
		{name: "find completion with a broken config", args: []string{"__complete", "find", "fs"},
			catalogs: neuroimaging, config: "{", wantStdout: ":1\n",
			wantStderrHas: "Completion ended with directive: ShellCompDirectiveError"},

		{name: "refresh", args: []string{"refresh"}, catalogs: []testCatalog{{name: "neuroimaging"}},
			serve: map[string]string{"neuroimaging": fslCatalog}, wantStdout: "Catalog \"neuroimaging\" updated.\n"},
		{name: "refresh over max_catalog_size", args: []string{"refresh"}, catalogs: []testCatalog{{name: "neuroimaging"}},
			config: `{"max_catalog_size": 10}`, serve: map[string]string{"neuroimaging": fslCatalog}, wantCode: 1,
			wantStderrHas: "is larger than max_catalog_size (10 bytes)"},
		{name: "refresh when up to date", args: []string{"refresh"}, catalogs: neuroimaging,
			serve: map[string]string{"neuroimaging": fslCatalog}, wantStdout: "Catalog \"neuroimaging\" is up to date.\n"},
		{name: "refresh in order", args: []string{"refresh"},
			catalogs:     []testCatalog{{name: "mylab"}, {name: "neuroimaging", apps: fslCatalog}},
			serve:        map[string]string{"mylab": labCatalog, "neuroimaging": fslCatalog},
			wantRequests: []string{"/mylab/catalog.json", "/mylab/apps.json", "/neuroimaging/catalog.json", "/neuroimaging/apps.json"},
			wantStdout:   "Catalog \"mylab\" updated.\nCatalog \"neuroimaging\" is up to date.\n"},
		{name: "refresh carries on after a failure", args: []string{"refresh"},
			catalogs: []testCatalog{{name: "cryoem"}, {name: "neuroimaging"}},
			serve:    map[string]string{"neuroimaging": fslCatalog}, wantCode: 1,
			wantStdout:    "Catalog \"neuroimaging\" updated.\n",
			wantStderrHas: "sciget: catalog \"cryoem\": downloading catalog: http://127.0.0.1:"},
		{name: "refresh with two failures", args: []string{"refresh"},
			catalogs: []testCatalog{{name: "a"}, {name: "b"}}, serve: map[string]string{}, wantCode: 1,
			wantStderrHas: "/a/catalog.json returned 404 Not Found\nsciget: catalog \"b\": "},
		{name: "refresh skips catalogs not in use", args: []string{"refresh"},
			catalogs:     []testCatalog{{name: "old", unused: true}, {name: "neuroimaging"}},
			serve:        map[string]string{"old": fslCatalog, "neuroimaging": fslCatalog},
			wantRequests: []string{"/neuroimaging/catalog.json", "/neuroimaging/apps.json"},
			wantStdout:   "Catalog \"neuroimaging\" updated.\n"},
		{name: "refresh without catalogs", args: []string{"refresh"}, wantCode: 1, wantStderr: noCatalogs},
		{name: "refresh with none in use", args: []string{"refresh"}, wantCode: 1, wantStderr: noneInUse,
			catalogs: []testCatalog{{name: "neuroimaging", unused: true}}},
		{name: "refresh takes no arguments", args: []string{"refresh", "x"}, wantCode: 2,
			wantStderr: "sciget: unknown command \"x\" for \"sciget refresh\"" + usageHint},
		{name: "refresh with a broken config", args: []string{"refresh"}, config: `{"bogus": "x"}`, wantCode: 1,
			wantStderrHas: `unknown key "bogus"`},

		{name: "catalog add", args: []string{"catalog", "add", "neuroimaging", exampleURL("neuroimaging")},
			wantStdout: "Catalog \"neuroimaging\" added at the top. Run 'sciget refresh' to download it.\n",
			wantList:   []catalog{{Name: "neuroimaging", URL: exampleURL("neuroimaging"), Use: true}}},
		{name: "catalog add puts it first", args: []string{"catalog", "add", "mylab", exampleURL("mylab")},
			catalogs:   []testCatalog{{name: "neuroimaging"}},
			wantStdout: "Catalog \"mylab\" added at the top. Run 'sciget refresh' to download it.\n",
			wantList: []catalog{{Name: "mylab", URL: exampleURL("mylab"), Use: true},
				{Name: "neuroimaging", URL: exampleURL("neuroimaging"), Use: true}}},
		{name: "catalog add --append", args: []string{"catalog", "add", "--append", "mylab", exampleURL("mylab")},
			catalogs:   []testCatalog{{name: "neuroimaging"}},
			wantStdout: "Catalog \"mylab\" added at the bottom. Run 'sciget refresh' to download it.\n",
			wantList: []catalog{{Name: "neuroimaging", URL: exampleURL("neuroimaging"), Use: true},
				{Name: "mylab", URL: exampleURL("mylab"), Use: true}}},
		{name: "catalog add the same again", catalogs: []testCatalog{{name: "mylab"}, {name: "neuroimaging", unused: true}},
			args:         []string{"catalog", "add", "neuroimaging", exampleURL("neuroimaging")},
			wantListSame: true,
			wantStdout:   "Catalog \"neuroimaging\" already exists with the same settings; nothing to do.\n",
			wantList: []catalog{{Name: "mylab", URL: exampleURL("mylab"), Use: true},
				{Name: "neuroimaging", URL: exampleURL("neuroimaging")}}},
		{name: "catalog add a name with another URL", catalogs: []testCatalog{{name: "neuroimaging"}},
			args: []string{"catalog", "add", "neuroimaging", mirrorURL}, wantCode: 1, wantListSame: true,
			wantStderr: "sciget: catalog \"neuroimaging\" already exists with different settings; use --force-update to replace it, or choose another name\n",
			wantList:   []catalog{{Name: "neuroimaging", URL: exampleURL("neuroimaging"), Use: true}}},
		{name: "catalog add --force-update", args: []string{"catalog", "add", "--force-update", "neuroimaging", mirrorURL},
			catalogs:   []testCatalog{{name: "mylab", apps: labCatalog}, {name: "neuroimaging", apps: fslCatalog}},
			wantStdout: "Catalog \"neuroimaging\" added at the top. Run 'sciget refresh' to download it.\n",
			wantList: []catalog{{Name: "neuroimaging", URL: mirrorURL, Use: true},
				{Name: "mylab", URL: exampleURL("mylab"), Use: true}},
			wantCaches: []string{"mylab"}},
		{name: "catalog add --force-update with a read-only list", readOnlyList: true,
			args:     []string{"catalog", "add", "--force-update", "neuroimaging", mirrorURL},
			catalogs: neuroimaging, wantCode: 1, wantStderrHas: "sciget: writing catalogs: ",
			wantList:   []catalog{{Name: "neuroimaging", URL: exampleURL("neuroimaging"), Use: true}},
			wantCaches: []string{"neuroimaging"}},
		{name: "catalog add --force-update with a locked cache", lockedCaches: true,
			args:     []string{"catalog", "add", "--force-update", "neuroimaging", mirrorURL},
			catalogs: neuroimaging, wantCode: 1, wantStderrHas: "sciget: removing catalog cache: ",
			wantList: []catalog{{Name: "neuroimaging", URL: mirrorURL, Use: true}}},
		{name: "catalog add a bad name", args: []string{"catalog", "add", "My Lab", exampleURL("mylab")}, wantCode: 2,
			wantStderr: "sciget: invalid catalog name \"My Lab\": must be lowercase letters, digits, \"-\" and \"_\", " +
				"starting with a letter or digit" + usageHint},
		{name: "catalog add a missing file", args: []string{"catalog", "add", "mylab", "example.org/apps.json"}, wantCode: 1,
			wantStderr: "sciget: reading catalog: open " + filepath.Join(cwd, "example.org", "apps.json") + ": no such file or directory\n"},
		{name: "catalog add another scheme", args: []string{"catalog", "add", "mylab", "ftp://example.org/apps.json"}, wantCode: 2,
			wantStderr: "sciget: invalid catalog URL: must start with https:// or http://" + usageHint},
		{name: "catalog add a plain http URL", args: []string{"catalog", "add", "mylab", "http://mirror.local/apps.json"}, wantCode: 2,
			wantStderr: "sciget: invalid catalog URL: must use https; plain http is allowed only to this machine (localhost)" + usageHint},
		{name: "catalog add plain http to this machine is read", args: []string{"catalog", "add", "mylab", "http://localhost:1/catalog.json"},
			wantCode: 1, wantStderrHas: `sciget: downloading catalog: Get "http://localhost:1/catalog.json"`},
		{name: "catalog add a URL with credentials", args: []string{"catalog", "add", "mylab", "https://u:p@e.org/a.json"},
			wantCode: 2, wantStderr: "sciget: invalid catalog URL: must not contain credentials" + usageHint},
		{name: "catalog add without a URL", args: []string{"catalog", "add", "mylab"}, wantCode: 2,
			wantStderr: "sciget: accepts 2 arg(s), received 1" + usageHint},
		{name: "catalog add share", args: []string{"catalog", "add", "--dir", "/share", "--layout", "{tool}/{version}/{date}", "neuroimaging", exampleURL("neuroimaging")},
			wantStdout: "Catalog \"neuroimaging\" added at the top. Run 'sciget refresh' to download it.\n",
			wantList:   []catalog{{Name: "neuroimaging", URL: exampleURL("neuroimaging"), Use: true, Dir: "/share", Layout: "{tool}/{version}/{date}"}}},
		{name: "catalog relative dir", args: []string{"catalog", "add", "--dir", "share", "a", exampleURL("a")},
			wantStdout: "Catalog \"a\" added at the top. Run 'sciget refresh' to download it.\n",
			wantList:   []catalog{{Name: "a", URL: exampleURL("a"), Use: true, Dir: filepath.Join(cwd, "share")}}},
		{name: "catalog empty layout", args: []string{"catalog", "add", "--dir", "/share", "--layout", "", "a", exampleURL("a")}, wantCode: 2,
			wantStderrHas: "sciget: invalid layout:"},

		{name: "catalog share no-op", catalogs: []testCatalog{{name: "neuroimaging", dir: "/share", layout: "{tool}/{version}/{date}"}},
			args: []string{"catalog", "add", "--dir", "/share", "--layout", "{tool}/{version}/{date}", "neuroimaging", exampleURL("neuroimaging")}, wantListSame: true,
			wantStdout: "Catalog \"neuroimaging\" already exists with the same settings; nothing to do.\n"},
		{name: "catalog share change needs force", catalogs: []testCatalog{{name: "neuroimaging"}},
			args: []string{"catalog", "add", "--dir", "/share", "neuroimaging", exampleURL("neuroimaging")}, wantCode: 1, wantListSame: true,
			wantStderrHas: "use --force-update"},
		{name: "catalog share update keeps cache", catalogs: neuroimaging,
			args:       []string{"catalog", "add", "--force-update", "--dir", "/share", "neuroimaging", exampleURL("neuroimaging")},
			wantStdout: "Catalog \"neuroimaging\" added at the top. Run 'sciget refresh' to download it.\n",
			wantList:   []catalog{{Name: "neuroimaging", URL: exampleURL("neuroimaging"), Use: true, Dir: "/share"}}, wantCaches: []string{"neuroimaging"}},
		{name: "catalog layout change needs force", catalogs: []testCatalog{{name: "neuroimaging", dir: "/share"}},
			args: []string{"catalog", "add", "--dir", "/share", "--layout", "{tool}/{version}/{date}", "neuroimaging", exampleURL("neuroimaging")}, wantCode: 1, wantListSame: true,
			wantStderrHas: "use --force-update"},
		{name: "catalog layout update keeps cache", catalogs: []testCatalog{{name: "neuroimaging", dir: "/share", apps: fslCatalog}},
			args:       []string{"catalog", "add", "--force-update", "--dir", "/share", "--layout", "{tool}/{version}/{date}", "neuroimaging", exampleURL("neuroimaging")},
			wantStdout: "Catalog \"neuroimaging\" added at the top. Run 'sciget refresh' to download it.\n",
			wantList:   []catalog{{Name: "neuroimaging", URL: exampleURL("neuroimaging"), Use: true, Dir: "/share", Layout: "{tool}/{version}/{date}"}}, wantCaches: []string{"neuroimaging"}},
		{name: "catalog layout requires dir", args: []string{"catalog", "add", "--layout", "{tool}/{version}/{date}", "a", exampleURL("a")}, wantCode: 2,
			wantStderr: "sciget: --layout requires --dir" + usageHint},
		{name: "catalog bad layout", args: []string{"catalog", "add", "--dir", "/share", "--layout", "{name}", "a", exampleURL("a")}, wantCode: 2,
			wantStderrHas: "sciget: invalid layout:"},
		{name: "catalog empty dir", args: []string{"catalog", "add", "--dir", "", "a", exampleURL("a")}, wantCode: 2,
			wantStderr: "sciget: --dir must not be empty" + usageHint},
		{name: "catalog use keeps share", args: []string{"catalog", "use", "a"}, catalogs: []testCatalog{{name: "a", dir: "/share", layout: "{tool}/{version}/{date}", unused: true}},
			wantStdout: "Catalog \"a\" in use at the top. Run 'sciget refresh' to update it.\n",
			wantList:   []catalog{{Name: "a", URL: exampleURL("a"), Use: true, Dir: "/share", Layout: "{tool}/{version}/{date}"}}},
		{name: "catalog unuse keeps share", args: []string{"catalog", "unuse", "a"}, catalogs: []testCatalog{{name: "a", dir: "/share", layout: "{tool}/{version}/{date}"}},
			wantStdout: "Catalog \"a\" no longer in use.\n",
			wantList:   []catalog{{Name: "a", URL: exampleURL("a"), Dir: "/share", Layout: "{tool}/{version}/{date}"}}},

		{name: "catalog remove", args: []string{"catalog", "remove", "neuroimaging"},
			catalogs: []testCatalog{{name: "neuroimaging", apps: fslCatalog},
				{name: "neuro", url: exampleURL("neuroimaging"), apps: fslCatalog}, {name: "mylab", apps: labCatalog}},
			wantStdout: "Catalog \"neuroimaging\" removed.\n",
			wantList: []catalog{{Name: "neuro", URL: exampleURL("neuroimaging"), Use: true},
				{Name: "mylab", URL: exampleURL("mylab"), Use: true}},
			wantCaches: []string{"mylab", "neuro"}},
		{name: "catalog remove the last one", args: []string{"catalog", "remove", "neuroimaging"},
			catalogs: neuroimaging, wantStdout: "Catalog \"neuroimaging\" removed.\n",
			wantList: []catalog{}, wantCaches: []string{}},
		{name: "catalog remove an unknown name", args: []string{"catalog", "remove", "x"}, wantCode: 1,
			wantStderr: "sciget: no catalog named \"x\"\n"},
		{name: "catalog use", args: []string{"catalog", "use", "mylab"},
			catalogs:   []testCatalog{{name: "neuroimaging"}, {name: "mylab", unused: true}},
			wantStdout: "Catalog \"mylab\" in use at the top. Run 'sciget refresh' to update it.\n",
			wantList: []catalog{{Name: "mylab", URL: exampleURL("mylab"), Use: true},
				{Name: "neuroimaging", URL: exampleURL("neuroimaging"), Use: true}}},
		{name: "catalog use --append", args: []string{"catalog", "use", "--append", "mylab"},
			catalogs:   []testCatalog{{name: "mylab"}, {name: "neuroimaging"}},
			wantStdout: "Catalog \"mylab\" in use at the bottom. Run 'sciget refresh' to update it.\n",
			wantList: []catalog{{Name: "neuroimaging", URL: exampleURL("neuroimaging"), Use: true},
				{Name: "mylab", URL: exampleURL("mylab"), Use: true}}},
		{name: "catalog use an unknown name", args: []string{"catalog", "use", "x"}, wantCode: 1,
			wantStderr: "sciget: no catalog named \"x\"\n"},
		{name: "catalog unuse", args: []string{"catalog", "unuse", "neuroimaging"},
			catalogs:   []testCatalog{{name: "neuroimaging", apps: fslCatalog}, {name: "mylab"}},
			wantStdout: "Catalog \"neuroimaging\" no longer in use.\n",
			wantList: []catalog{{Name: "neuroimaging", URL: exampleURL("neuroimaging")},
				{Name: "mylab", URL: exampleURL("mylab"), Use: true}},
			wantCaches: []string{"neuroimaging"}},
		{name: "catalog unuse an unknown name", args: []string{"catalog", "unuse", "x"}, wantCode: 1,
			wantStderr: "sciget: no catalog named \"x\"\n"},
		{name: "catalog add a domains file", args: []string{"catalog", "add", "sci", domainsURL},
			wantStdout: "Catalog \"sci-eeg\" added at the top.\nCatalog \"sci-neuro\" added at the top. Run 'sciget refresh' to download them.\n",
			wantList: []catalog{{Name: "sci-eeg", URL: domainsURL, Domain: "eeg", Use: true},
				{Name: "sci-neuro", URL: domainsURL, Domain: "neuro", Use: true}}},
		{name: "catalog add a domains file --append", args: []string{"catalog", "add", "--append", "sci", domainsURL},
			catalogs:   []testCatalog{{name: "mylab"}},
			wantStdout: "Catalog \"sci-eeg\" added at the bottom.\nCatalog \"sci-neuro\" added at the bottom. Run 'sciget refresh' to download them.\n",
			wantList: []catalog{{Name: "mylab", URL: exampleURL("mylab"), Use: true},
				{Name: "sci-eeg", URL: domainsURL, Domain: "eeg", Use: true},
				{Name: "sci-neuro", URL: domainsURL, Domain: "neuro", Use: true}}},
		{name: "catalog add a domains file again adds only new keys", args: []string{"catalog", "add", "sci", domainsURL},
			catalogs: []testCatalog{{name: "sci-eeg", url: domainsURL, domain: "eeg"}},
			wantStdout: "Catalog \"sci-eeg\" already exists with the same settings; nothing to do.\n" +
				"Catalog \"sci-neuro\" added at the top. Run 'sciget refresh' to download it.\n",
			wantList: []catalog{{Name: "sci-neuro", URL: domainsURL, Domain: "neuro", Use: true},
				{Name: "sci-eeg", URL: domainsURL, Domain: "eeg", Use: true}}},
		{name: "catalog add a domains file with a key of another domain", args: []string{"catalog", "add", "sci", domainsURL},
			catalogs: []testCatalog{{name: "sci-eeg", url: domainsURL, domain: "neuro"}}, wantCode: 1, wantListSame: true,
			wantStderr: "sciget: catalog \"sci-eeg\" already exists with different settings; use --force-update to replace it, or choose another name\n"},
		{name: "catalog add a domains file with a system name", args: []string{"catalog", "add", "sci", domainsURL},
			list: "[]", systemCatalogs: []testCatalog{{name: "sci-neuro"}}, wantCode: 1, wantListSame: true,
			wantStderrHas: "sciget: catalog \"sci-neuro\" is set by "},
		{name: "catalog add a domains file with an empty domain", args: []string{"catalog", "add", "sci", placeholderURL},
			wantStdout: "Catalog \"sci-neuro\" added at the top.\n" +
				"Catalog \"sci-soon\" added at the top. It has no apps list yet. Run 'sciget refresh' to download them.\n",
			wantList: []catalog{{Name: "sci-neuro", URL: placeholderURL, Domain: "neuro", Use: true},
				{Name: "sci-soon", URL: placeholderURL, Domain: "soon", Use: true}}},
		{name: "catalog add an empty catalog", args: []string{"catalog", "add", "soon", emptyURL},
			wantStdout: "Catalog \"soon\" added at the top. It has no apps list yet. Run 'sciget refresh' to download it.\n",
			wantList:   []catalog{{Name: "soon", URL: emptyURL, Use: true}}},
		{name: "refresh an empty domain", args: []string{"refresh"},
			catalogs: []testCatalog{{name: "sci-neuro", url: placeholderURL, domain: "neuro"},
				{name: "sci-soon", url: placeholderURL, domain: "soon"}},
			wantStdout: "Catalog \"sci-neuro\" updated.\nCatalog \"sci-soon\" updated. It has no apps list yet.\n"},
		{name: "find beside an empty catalog", args: []string{"find", "fsl"},
			before: [][]string{{"refresh"}},
			catalogs: []testCatalog{{name: "sci-soon", url: placeholderURL, domain: "soon"},
				{name: "sci-neuro", url: placeholderURL, domain: "neuro"}},
			wantStdoutHas: "fsl/6.0.3"},
		{name: "catalog add an invalid catalog JSON", args: []string{"catalog", "add", "bad", badURL}, wantCode: 1,
			wantStderr: "sciget: " + badURL + " is not a valid catalog JSON: schemaVersion must be 1\n"},
		{name: "catalog add a relative file", args: []string{"catalog", "add", "rel", relativeURL},
			wantStdout: "Catalog \"rel\" added at the top. Run 'sciget refresh' to download it.\n",
			wantList:   []catalog{{Name: "rel", URL: exampleURL("relative"), Use: true}}},
		{name: "refresh a domain of a domains file", args: []string{"refresh"},
			catalogs:   []testCatalog{{name: "sci-neuro", url: domainsURL, domain: "neuro"}},
			wantStdout: "Catalog \"sci-neuro\" updated.\n"},
		{name: "find after refreshing a domain", args: []string{"find", "fsl"},
			before:        [][]string{{"refresh"}},
			catalogs:      []testCatalog{{name: "sci-neuro", url: domainsURL, domain: "neuro"}},
			wantStdoutHas: "fsl/6.0.3"},
		{name: "catalog list shows a domain", args: []string{"catalog", "list"},
			catalogs:   []testCatalog{{name: "sci-neuro", url: domainsURL, domain: "neuro"}},
			wantStdout: "sci-neuro\t1\tuser\t" + domainsURL + "#neuro\t\n"},
		{name: "download_urls is no longer a setting", args: []string{"find"}, config: `{"download_urls": []}`,
			catalogs: neuroimaging, wantCode: 1, wantStderrHas: `unknown key "download_urls"`},
		{name: "catalog list", args: []string{"catalog", "list"},
			catalogs: []testCatalog{{name: "mylab"}, {name: "old", unused: true}, {name: "neuroimaging"}},
			wantStdout: "mylab\t1\tuser\t" + exampleURL("mylab") + "\t\nold\t-\tuser\t" + exampleURL("old") +
				"\t\nneuroimaging\t2\tuser\t" + exampleURL("neuroimaging") + "\t\n"},
		{name: "catalog alone lists", args: []string{"catalog"}, catalogs: []testCatalog{{name: "mylab"}},
			wantStdout: "mylab\t1\tuser\t" + exampleURL("mylab") + "\t\n"},
		{name: "catalog list with a broken list", args: []string{"catalog", "list"}, list: "{", wantCode: 1,
			wantStderrHas: "sciget: reading catalogs "},
		{name: "catalog use completes names", args: []string{"__complete", "catalog", "use", "n"},
			catalogs:   []testCatalog{{name: "mylab"}, {name: "neuroimaging"}},
			wantStdout: "neuroimaging\n:4\n", wantStderrHas: "Completion ended with directive"},
		{name: "catalog remove completes names", args: []string{"__complete", "catalog", "remove", ""},
			catalogs:   []testCatalog{{name: "mylab"}, {name: "neuroimaging"}},
			wantStdout: "mylab\nneuroimaging\n:4\n", wantStderrHas: "Completion ended with directive"},
		{name: "catalog unuse completes names", args: []string{"__complete", "catalog", "unuse", "m"},
			catalogs:   []testCatalog{{name: "mylab"}, {name: "neuroimaging"}},
			wantStdout: "mylab\n:4\n", wantStderrHas: "Completion ended with directive"},
		{name: "catalog completion with a broken list", args: []string{"__complete", "catalog", "use", ""},
			list: "{", wantStdout: ":1\n", wantStderrHas: "Completion ended with directive: ShellCompDirectiveError"},

		// The system layer: catalogs and settings an admin set with --system.
		{name: "catalog list with a system catalog", args: []string{"catalog", "list"},
			catalogs: []testCatalog{{name: "mylab"}}, systemCatalogs: []testCatalog{{name: "neuro"}},
			wantStdout: "mylab\t1\tuser\t" + exampleURL("mylab") + "\t\nneuro\t2\tsystem\t" + exampleURL("neuro") + "\t\n"},
		{name: "a user catalog hides a system one of the same name", args: []string{"catalog", "list"},
			catalogs: []testCatalog{{name: "neuro", url: labURL}}, systemCatalogs: []testCatalog{{name: "neuro"}},
			wantStdout: "neuro\t1\tuser\t" + labURL + "\t\n"},
		{name: "catalog unuse of a system catalog records only that", args: []string{"catalog", "unuse", "neuro"},
			systemCatalogs: []testCatalog{{name: "neuro"}},
			wantStdout:     "Catalog \"neuro\" no longer in use.\n", wantList: []catalog{{Name: "neuro"}}},
		{name: "an unused system catalog is listed as unused", args: []string{"catalog", "list"},
			before: [][]string{{"catalog", "unuse", "neuro"}}, systemCatalogs: []testCatalog{{name: "neuro"}},
			wantStdout: "neuro\t-\tsystem\t" + exampleURL("neuro") + "\t\n"},
		{name: "catalog use of a system catalog", args: []string{"catalog", "use", "neuro"},
			list: `[{"name": "neuro", "use": false}]`, systemCatalogs: []testCatalog{{name: "neuro"}},
			wantStdout: "Catalog \"neuro\" in use. Run 'sciget refresh' to update it.\n",
			wantList:   []catalog{{Name: "neuro", Use: true}}},
		{name: "catalog use --append of a system catalog", args: []string{"catalog", "use", "--append", "neuro"},
			systemCatalogs: []testCatalog{{name: "neuro"}}, wantCode: 2,
			wantStderrHas: "--append cannot move it"},
		{name: "a switch for a system catalog that has gone is ignored", args: []string{"catalog", "list"},
			list: `[{"name": "neuro", "use": false}]`, wantStdout: ""},
		{name: "catalog remove of a system catalog", args: []string{"catalog", "remove", "neuro"},
			systemCatalogs: []testCatalog{{name: "neuro"}}, wantCode: 1,
			wantStderrHas: "catalogs.json; an admin can change it with --system\n"},
		{name: "catalog remove of a switch shows the system catalog", args: []string{"catalog", "remove", "neuro"},
			list: `[{"name": "neuro", "use": false}]`, systemCatalogs: []testCatalog{{name: "neuro"}},
			wantStdout: "Catalog \"neuro\" removed.\n", wantList: []catalog{}},
		{name: "catalog remove of a shadow keeps the cache it shares", args: []string{"catalog", "remove", "neuro"},
			catalogs: []testCatalog{{name: "neuro", apps: "[]"}}, systemCatalogs: []testCatalog{{name: "neuro", apps: "[]"}},
			wantStdout: "Catalog \"neuro\" removed.\n", wantCaches: []string{"neuro"}},
		{name: "catalog add of a system name", args: []string{"catalog", "add", "neuro", labURL},
			systemCatalogs: []testCatalog{{name: "neuro"}}, wantCode: 1, wantStderrHas: "catalogs.json; choose another name\n"},
		{name: "catalog add of a system name even with --force-update",
			args:           []string{"catalog", "add", "--force-update", "neuro", labURL},
			systemCatalogs: []testCatalog{{name: "neuro"}}, wantCode: 1, wantStderrHas: "choose another name"},
		{name: "catalog add of a system catalog as it is", args: []string{"catalog", "add", "neuro", exampleURL("neuro")},
			systemCatalogs: []testCatalog{{name: "neuro"}},
			wantStdout:     "Catalog \"neuro\" already exists with the same settings; nothing to do.\n"},
		{name: "catalog add --append stays before system catalogs", args: []string{"catalog", "list"},
			before:         [][]string{{"catalog", "add", "--append", "mylab", exampleURL("mylab")}},
			systemCatalogs: []testCatalog{{name: "neuro"}},
			wantStdout:     "mylab\t1\tuser\t" + exampleURL("mylab") + "\t\nneuro\t2\tsystem\t" + exampleURL("neuro") + "\t\n"},
		{name: "only system catalogs, none in use", args: []string{"find"},
			list: `[{"name": "neuro", "use": false}]`, systemCatalogs: []testCatalog{{name: "neuro"}},
			wantCode: 1, wantStderr: "sciget: no catalogs in use; run 'sciget catalog use <name>'\n"},
		{name: "catalog add --system", args: []string{"catalog", "add", "--system", "neuro", exampleURL("neuro")},
			wantStdout:     "Catalog \"neuro\" added at the top. Run 'sciget refresh' to download it.\n",
			wantSystemList: []catalog{{Name: "neuro", URL: exampleURL("neuro"), Use: true}}, wantCaches: []string{}},
		{name: "catalog add --system ignores a broken user list",
			args: []string{"catalog", "add", "--system", "neuro", exampleURL("neuro")}, list: "{", config: "{",
			wantStdout:     "Catalog \"neuro\" added at the top. Run 'sciget refresh' to download it.\n",
			wantSystemList: []catalog{{Name: "neuro", URL: exampleURL("neuro"), Use: true}}},
		{name: "catalog add --system --force-update keeps caches",
			args:           []string{"catalog", "add", "--system", "--force-update", "neuro", labURL},
			systemCatalogs: []testCatalog{{name: "neuro", apps: "[]"}},
			wantStdout:     "Catalog \"neuro\" added at the top. Run 'sciget refresh' to download it.\n",
			wantSystemList: []catalog{{Name: "neuro", URL: labURL, Use: true}}, wantCaches: []string{"neuro"}},
		{name: "catalog remove --system keeps caches", args: []string{"catalog", "remove", "--system", "neuro"},
			systemCatalogs: []testCatalog{{name: "neuro", apps: "[]"}},
			wantStdout:     "Catalog \"neuro\" removed.\n", wantSystemList: []catalog{}, wantCaches: []string{"neuro"}},
		{name: "catalog unuse --system", args: []string{"catalog", "unuse", "--system", "neuro"},
			systemCatalogs: []testCatalog{{name: "neuro"}},
			wantStdout:     "Catalog \"neuro\" no longer in use.\n", wantSystemList: []catalog{{Name: "neuro", URL: exampleURL("neuro")}}},
		{name: "catalog use --system --append", args: []string{"catalog", "use", "--system", "--append", "neuro"},
			systemCatalogs: []testCatalog{{name: "neuro"}, {name: "other"}},
			wantStdout:     "Catalog \"neuro\" in use at the bottom. Run 'sciget refresh' to update it.\n",
			wantSystemList: []catalog{{Name: "other", URL: exampleURL("other"), Use: true}, {Name: "neuro", URL: exampleURL("neuro"), Use: true}}},
		{name: "catalog remove completes only the user's", args: []string{"__complete", "catalog", "remove", ""},
			catalogs: []testCatalog{{name: "mylab"}}, systemCatalogs: []testCatalog{{name: "neuro"}},
			wantStdout: "mylab\n:4\n", wantStderrHas: "Completion ended with directive"},
		{name: "catalog use completes system catalogs too", args: []string{"__complete", "catalog", "use", ""},
			catalogs: []testCatalog{{name: "mylab"}}, systemCatalogs: []testCatalog{{name: "neuro"}},
			wantStdout: "mylab\nneuro\n:4\n", wantStderrHas: "Completion ended with directive"},
		{name: "--system completes the system's", args: []string{"__complete", "catalog", "remove", "--system", ""},
			catalogs: []testCatalog{{name: "mylab"}}, systemCatalogs: []testCatalog{{name: "neuro"}},
			wantStdout: "neuro\n:4\n", wantStderrHas: "Completion ended with directive"},
		{name: "config get of a system value prints only the value", args: []string{"config", "get", "display_start_command"},
			systemConfig: `{"display_start_command": ["/s"]}`, wantStdout: "[\"/s\"]\n"},
		{name: "config list names the system as the source", args: []string{"config", "list"},
			systemConfig: `{"display_start_command": ["/s"]}`, wantStdoutHas: "display_start_command\t[\"/s\"]\tsystem\n"},
		{name: "a user value wins over the system's", args: []string{"config", "get", "auto_pull"},
			config: `{"auto_pull": true}`, systemConfig: `{"auto_pull": false}`, wantStdout: "true\n"},
		{name: "config set --system", args: []string{"config", "set", "--system", "auto_pull", "true"},
			wantSystemConfig: "{\n  \"auto_pull\": true\n}\n"},
		{name: "config set --system keeps the user's file out", args: []string{"config", "set", "--system", "auto_pull", "true"},
			config: `{"parallel_pulls": 2}`, wantConfig: `{"parallel_pulls": 2}`, wantSystemConfig: "{\n  \"auto_pull\": true\n}\n"},
		{name: "config set --system of a per-user key", args: []string{"config", "set", "--system", "cache_dir", "/c"},
			wantCode: 2, wantStderr: "sciget: cache_dir can only be set per user" + usageHint},
		{name: "a per-user key in the system file", args: []string{"config", "get", "auto_pull"},
			systemConfig: `{"container_dir": "/c"}`, wantCode: 1, wantStderrHas: "container_dir can only be set per user\n"},
		{name: "an invalid system file", args: []string{"config", "list"},
			systemConfig: "{", wantCode: 1, wantStderrHas: "sciget: reading config "},
		{name: "config unset --system", args: []string{"config", "unset", "--system", "auto_pull"},
			systemConfig: `{"auto_pull": true}`, wantSystemConfig: "{}\n"},
		{name: "config unset of a key only the system sets writes nothing", args: []string{"config", "unset", "auto_pull"},
			config: "{ }", systemConfig: `{"auto_pull": true}`, wantConfig: "{ }"},
		{name: "config set does not copy system values", args: []string{"config", "set", "parallel_pulls", "2"},
			config: "{}", systemConfig: `{"auto_pull": true}`, wantConfig: "{\n  \"parallel_pulls\": 2\n}\n"},
		{name: "doctor names a user catalog hiding a system one", args: []string{"doctor"},
			catalogs: []testCatalog{{name: "neuro", url: labURL}}, systemCatalogs: []testCatalog{{name: "neuro"}},
			wantStdout: "Your catalog neuro hides the system catalog of the same name (" + exampleURL("neuro") + "). Fix it with:\n" +
				"  sciget catalog remove neuro\n"},
		{name: "doctor names a setting equal to the system's", args: []string{"doctor"},
			config: `{"auto_pull": true}`, systemConfig: `{"auto_pull": true}`,
			wantStdoutHas: "Your config sets auto_pull to the same value as "},
		{name: "doctor accepts a deliberate override", args: []string{"doctor"},
			config: `{"auto_pull": true}`, systemConfig: `{"auto_pull": false}`, wantStdout: "No problems found.\n"},

		{name: "config lists", args: []string{"config"}, config: `{"cache_dir": "/c"}`, env: map[string]string{"XDG_DATA_HOME": "/data"},
			wantStdout: "cache_dir\t/c\tuser\n" + otherDefaults},
		{name: "config list", args: []string{"config", "list"}, config: `{"cache_dir": "/c"}`, env: map[string]string{"XDG_DATA_HOME": "/data"},
			wantStdout: "cache_dir\t/c\tuser\n" + otherDefaults},
		{name: "config with an unknown subcommand", args: []string{"config", "bogus"}, wantCode: 2,
			wantStderr: "sciget: unknown command \"bogus\" for \"sciget config\"" + usageHint},
		{name: "config get a default", args: []string{"config", "get", "cache_dir"},
			wantStdoutHas: string(filepath.Separator) + "sciget\n"},
		{name: "config get an override", args: []string{"config", "get", "cache_dir"},
			config: `{"cache_dir": "/c"}`, wantStdout: "/c\n"},
		// Each platform words this differently, but every message ends in
		// "defined", so a usage hint after it would fail the row.
		{name: "config get with a broken environment", args: []string{"config", "get", "cache_dir"},
			env: map[string]string{"XDG_CACHE_HOME": "", "HOME": "", "LocalAppData": ""}, wantCode: 1,
			wantStderrHas: "defined\n"},
		{name: "config get an unknown key", args: []string{"config", "get", "cache-dir"}, wantCode: 2,
			wantStderr: "sciget: unknown key \"cache-dir\"; valid keys: " + strings.Join(keyNames, ", ") + usageHint},
		{name: "config get without a key", args: []string{"config", "get"}, wantCode: 2,
			wantStderr: "sciget: accepts 1 arg(s), received 0" + usageHint},
		{name: "config set", args: []string{"config", "set", "cache_dir", "/c"},
			wantConfig: "{\n  \"cache_dir\": \"/c\"\n}\n"},
		{name: "config set a list", args: []string{"config", "set", "apptainer_extra_args", `[ "--nv" ]`},
			wantConfig: "{\n  \"apptainer_extra_args\": [\n    \"--nv\"\n  ]\n}\n"},
		{name: "config set a number", args: []string{"config", "set", "max_catalog_size", "1024"},
			wantConfig: "{\n  \"max_catalog_size\": 1024\n}\n"},
		{name: "config set a bool", args: []string{"config", "set", "use_catalog_args", "false"},
			wantConfig: "{\n  \"use_catalog_args\": false\n}\n"},
		{name: "config set a list that is not JSON", args: []string{"config", "set", "apptainer_extra_args", "x"}, wantCode: 2,
			wantStderr: "sciget: invalid apptainer_extra_args: must be a JSON array of strings" + usageHint},
		{name: "config set a bool that is not true or false", args: []string{"config", "set", "use_catalog_args", "yes"}, wantCode: 2,
			wantStderr: "sciget: invalid use_catalog_args: must be true or false" + usageHint},
		{name: "config set a size over the cap", args: []string{"config", "set", "max_catalog_size", "2199023255552"}, wantCode: 2,
			wantStderr: "sciget: invalid max_catalog_size: must be from 1 to 1099511627776 (1 TiB)" + usageHint},
		{name: "config set an unknown placeholder", args: []string{"config", "set", "apptainer_exec_args", `["{foo}"]`}, wantCode: 2,
			wantStderr: "sciget: invalid apptainer_exec_args: \"{foo}\" is not a known placeholder" + usageHint},
		{name: "config set a placeholder outside the verb args", args: []string{"config", "set", "apptainer_extra_args", `["{pwd}"]`}, wantCode: 2,
			wantStderr: "sciget: invalid apptainer_extra_args: \"{pwd}\" must not contain { or }" + usageHint},
		{name: "config get a list", args: []string{"config", "get", "apptainer_global_args"}, wantStdout: "[\"--silent\"]\n"},
		{name: "config with a value of the wrong type", args: []string{"config", "get", "apptainer_extra_args"},
			config: `{"apptainer_extra_args": "--nv"}`, wantCode: 1,
			wantStderrHas: "invalid apptainer_extra_args: must be a JSON array of strings"},
		{name: "config set on a read-only file", args: []string{"config", "set", "cache_dir", "/d"},
			config: `{"cache_dir": "/c"}`, readOnly: true, wantCode: 1,
			wantStderrHas: "sciget: writing config: ", wantConfig: `{"cache_dir": "/c"}`},
		{name: "config set leaves a file that is not a config", args: []string{"config", "set", "cache_dir", "/c"},
			config: "export X=1\n", wantCode: 1, wantStderrHas: "sciget: reading config ",
			wantConfig: "export X=1\n"},
		{name: "config set leaves a config with an unknown key", args: []string{"config", "set", "cache_dir", "/c"},
			config: `{"CACHE_DIR": "/x"}`, wantCode: 1,
			wantStderrHas: `unknown key "CACHE_DIR"`, wantConfig: `{"CACHE_DIR": "/x"}`},
		{name: "config unset of a key that is not set writes nothing", args: []string{"config", "unset", "cache_dir"},
			config: "{ }", wantConfig: "{ }"},
		{name: "config set an empty value", args: []string{"config", "set", "cache_dir", ""}, wantCode: 2,
			wantStderr: "sciget: invalid cache_dir: must be an absolute path" + usageHint},
		{name: "config list takes no arguments", args: []string{"config", "list", "x"}, wantCode: 2,
			wantStderr: "sciget: unknown command \"x\" for \"sciget config list\"" + usageHint},
		{name: "config unset", args: []string{"config", "unset", "cache_dir"},
			config: `{"cache_dir": "/c"}`, wantConfig: "{}\n"},
		{name: "config completes keys", args: []string{"__complete", "config", "get", "c"},
			wantStdout: "cache_dir\ncontainer_dir\ncontainer_layout\n:4\n", wantStderrHas: "Completion ended with directive"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if (tt.readOnly || tt.readOnlyList || tt.lockedCaches) && os.Geteuid() == 0 {
				t.Skip("root ignores file modes")
			}
			setCacheHome(t, t.TempDir())
			t.Setenv("XDG_DATA_HOME", t.TempDir())
			cache, err := os.UserCacheDir()
			if err != nil {
				t.Fatal(err)
			}
			cacheDir := filepath.Join(cache, "sciget")
			configFile := filepath.Join(t.TempDir(), "config.json")
			listFile := filepath.Join(filepath.Dir(configFile), "catalogs.json")
			t.Setenv("SCIGET_CONFIG", configFile)
			// The placeholders read these; rows set them through env.
			for _, k := range []string{"DISPLAY", "XAUTHORITY", "TMP", "TMPDIR", "TEMP", "TEMPDIR"} {
				t.Setenv(k, "")
			}
			for k, v := range tt.env {
				t.Setenv(k, v)
			}
			store := filepath.Join(os.Getenv("XDG_DATA_HOME"), "sciget", "containers")
			for _, p := range tt.stored {
				if err := os.MkdirAll(filepath.Join(store, p), 0o755); err != nil {
					t.Fatal(err)
				}
			}
			var execArgs []string
			runApptainer = func(path string, args, env []string) error {
				execArgs = args
				return nil
			}
			defer func() { runApptainer = execApptainer }()

			base := ""
			var requests []string
			if tt.serve != nil {
				srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					requests = append(requests, r.URL.Path)
					name, file, _ := strings.Cut(strings.TrimPrefix(r.URL.Path, "/"), "/")
					body, ok := tt.serve[name]
					if file == "catalog.json" {
						body = testCatalogJSON
					}
					if !ok || file != "catalog.json" && file != "apps.json" {
						http.NotFound(w, r)
						return
					}
					io.WriteString(w, body)
				}))
				defer srv.Close()
				base = srv.URL
			}

			if tt.config != "" {
				mode := os.FileMode(0o600)
				if tt.readOnly {
					mode = 0o400
				}
				if err := os.WriteFile(configFile, []byte(tt.config), mode); err != nil {
					t.Fatal(err)
				}
			}
			sysDir := t.TempDir()
			setSystemDir(t, sysDir)
			if tt.systemConfig != "" {
				if err := os.WriteFile(filepath.Join(sysDir, "config.json"), []byte(tt.systemConfig), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			// buildList returns the catalogs as JSON, writing each one's
			// apps.json into the cache.
			buildList := func(tcs []testCatalog) string {
				var catalogs []catalog
				for _, tc := range tcs {
					c := catalog{Name: tc.name, URL: tc.url, Domain: tc.domain, Use: !tc.unused, Dir: tc.dir, Layout: tc.layout}
					switch {
					case c.URL != "":
					case base != "":
						c.URL = base + "/" + tc.name + "/catalog.json"
					default:
						c.URL = exampleURL(tc.name)
					}
					catalogs = append(catalogs, c)
					if tc.apps != "" {
						dir := c.dir(cacheDir)
						if err := os.MkdirAll(dir, 0o755); err != nil {
							t.Fatal(err)
						}
						if err := os.WriteFile(filepath.Join(dir, "apps.json"), []byte(tc.apps), 0o644); err != nil {
							t.Fatal(err)
						}
						catalogJSON := testCatalogJSON
						if tc.noMirrors {
							catalogJSON = `{"schemaVersion": 1, "appJsonUrl": "apps.json"}`
						}
						if err := os.WriteFile(filepath.Join(dir, "catalog.json"), []byte(catalogJSON), 0o600); err != nil {
							t.Fatal(err)
						}
					}
				}
				data, err := json.Marshal(catalogs)
				if err != nil {
					t.Fatal(err)
				}
				return string(data)
			}
			list := tt.list
			if tt.catalogs != nil {
				list = buildList(tt.catalogs)
			}
			if tt.systemCatalogs != nil {
				if err := os.WriteFile(filepath.Join(sysDir, "catalogs.json"), []byte(buildList(tt.systemCatalogs)), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			if list != "" {
				mode := os.FileMode(0o600)
				if tt.readOnlyList {
					mode = 0o400
				}
				if err := os.WriteFile(listFile, []byte(list), mode); err != nil {
					t.Fatal(err)
				}
			}
			if tt.lockedCaches {
				locked := filepath.Join(cacheDir, "catalogs")
				if err := os.Chmod(locked, 0o555); err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { os.Chmod(locked, 0o755) })
			}

			for _, args := range tt.before {
				var out bytes.Buffer
				if code := run(args, &out, &out); code != 0 {
					t.Fatalf("%q exited %d: %s", args, code, out.String())
				}
			}
			var stdout, stderr bytes.Buffer
			code := run(tt.args, &stdout, &stderr)
			paths := strings.NewReplacer("<store>", store, "<cwd>", physicalCwd)
			if tt.wantExec != nil {
				want := slices.Clone(tt.wantExec)
				for i, a := range want {
					want[i] = paths.Replace(a)
				}
				if !slices.Equal(execArgs, want) {
					t.Errorf("exec args = %q, want %q", execArgs, want)
				}
			} else if execArgs != nil {
				t.Errorf("exec args = %q, want no hand-over", execArgs)
			}
			if code != tt.wantCode {
				t.Errorf("exit code = %d, want %d", code, tt.wantCode)
			}
			got := stdout.String()
			if tt.wantStdoutLacks != "" && strings.Contains(got, tt.wantStdoutLacks) {
				t.Errorf("stdout contains %q: %q", tt.wantStdoutLacks, got)
			}
			if tt.wantStdoutHas != "" {
				if !strings.Contains(got, tt.wantStdoutHas) {
					t.Errorf("stdout = %q, want it to contain %q", got, tt.wantStdoutHas)
				}
			} else if want := paths.Replace(tt.wantStdout); got != want {
				t.Errorf("stdout = %q, want %q", got, want)
			}
			got = stderr.String()
			if tt.wantStderrHas != "" {
				if !strings.Contains(got, tt.wantStderrHas) {
					t.Errorf("stderr = %q, want it to contain %q", got, tt.wantStderrHas)
				}
			} else if got != tt.wantStderr {
				t.Errorf("stderr = %q, want %q", got, tt.wantStderr)
			}
			if tt.wantConfig != "" {
				data, err := os.ReadFile(configFile)
				if err != nil {
					t.Fatal(err)
				}
				if string(data) != tt.wantConfig {
					t.Errorf("config file = %q, want %q", data, tt.wantConfig)
				}
			}
			// The setup writes compact JSON and sciget indents it, so any
			// rewrite changes the bytes.
			if tt.wantListSame {
				data, err := os.ReadFile(listFile)
				if err != nil {
					t.Fatal(err)
				}
				if string(data) != list {
					t.Errorf("catalogs.json rewritten: %q, want %q", data, list)
				}
			}
			if tt.wantList != nil {
				catalogs, err := loadCatalogList(listFile)
				if err != nil {
					t.Fatal(err)
				}
				if !slices.Equal(catalogs, tt.wantList) {
					t.Errorf("catalogs.json = %v, want %v", catalogs, tt.wantList)
				}
			}
			if tt.wantSystemList != nil {
				catalogs, err := loadCatalogList(filepath.Join(sysDir, "catalogs.json"))
				if err != nil {
					t.Fatal(err)
				}
				if !slices.Equal(catalogs, tt.wantSystemList) {
					t.Errorf("system catalogs.json = %v, want %v", catalogs, tt.wantSystemList)
				}
			}
			if tt.wantSystemConfig != "" {
				data, err := os.ReadFile(filepath.Join(sysDir, "config.json"))
				if err != nil {
					t.Fatal(err)
				}
				if string(data) != tt.wantSystemConfig {
					t.Errorf("system config.json = %q, want %q", data, tt.wantSystemConfig)
				}
			}
			if tt.wantCaches != nil {
				entries, err := os.ReadDir(filepath.Join(cacheDir, "catalogs"))
				if err != nil && !errors.Is(err, os.ErrNotExist) {
					t.Fatal(err)
				}
				names := []string{}
				for _, e := range entries {
					if !e.IsDir() && strings.HasSuffix(e.Name(), ".lock") {
						continue
					}
					names = append(names, e.Name()[:strings.LastIndex(e.Name(), "-")])
				}
				slices.Sort(names)
				if !slices.Equal(names, tt.wantCaches) {
					t.Errorf("cache dirs = %q, want %q", names, tt.wantCaches)
				}
			}
			if tt.wantRequests != nil && !slices.Equal(requests, tt.wantRequests) {
				t.Errorf("requests = %q, want %q", requests, tt.wantRequests)
			}
		})
	}
}

// setConfigHome points the user config directory at dir on every platform
// os.UserConfigDir supports.
func setConfigHome(t *testing.T, dir string) {
	t.Helper()
	for _, name := range []string{"XDG_CONFIG_HOME", "HOME", "AppData"} {
		t.Setenv(name, dir)
	}
}

// setCacheHome points the user cache directory at dir on every platform
// os.UserCacheDir supports, so no test reads or writes a real cache.
func setCacheHome(t *testing.T, dir string) {
	t.Helper()
	for _, name := range []string{"XDG_CACHE_HOME", "HOME", "LocalAppData"} {
		t.Setenv(name, dir)
	}
}

func TestExitCode(t *testing.T) {
	tests := []struct {
		name       string
		err        error
		wantCode   int
		wantStderr string
	}{
		{name: "no error"},
		{name: "wrapped usage error", err: fmt.Errorf("x: %w", usageError{"y"}), wantCode: 2,
			wantStderr: "sciget: x: y\nRun 'sciget help' for usage.\n"},
		{name: "ordinary error", err: errors.New("download failed"), wantCode: 1,
			wantStderr: "sciget: download failed\n"},
		{name: "joined errors, one per line", err: errors.Join(errors.New("a failed"), errors.New("b failed")),
			wantCode: 1, wantStderr: "sciget: a failed\nsciget: b failed\n"},
		{name: "two-line usage error", err: usageError{"a\nb"}, wantCode: 2,
			wantStderr: "sciget: a\nsciget: b\nRun 'sciget help' for usage.\n"},
		{name: "escape sequences removed, lines kept", err: errors.New("404 Not \x1b[31mFound\x1b[0m\nb \x1b]0;x\x07failed"),
			wantCode: 1, wantStderr: "sciget: 404 Not Found\nsciget: b failed\n"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var stderr bytes.Buffer
			code := exitCode(tt.err, &stderr)
			if code != tt.wantCode {
				t.Errorf("exit code = %d, want %d", code, tt.wantCode)
			}
			if got := stderr.String(); got != tt.wantStderr {
				t.Errorf("stderr = %q, want %q", got, tt.wantStderr)
			}
		})
	}
}

func TestFindArchFiltering(t *testing.T) {
	const mixedCatalog = `{
		"amico": {"categories": ["diffusion imaging"], "apps": {
			"amico 2.1.0": {"version": "20250628", "image": "amico_2.1.0"},
			"amico 2.1.0 arm64": {"version": "20260512", "image": "amico_2.1.0_arm64", "architecture": "aarch64"}
		}},
		"fsl": {"categories": ["image registration"], "apps": {
			"fsl 6.0.7": {"version": "20260101", "architecture": "x86_64"}
		}},
		"fsl_arm64": {"categories": ["image registration"], "apps": {
			"fsl_arm64 6.0.7.23": {"version": "20261013", "image": "fsl_arm64_6.0.7.23"}
		}}
	}`

	base := t.TempDir()
	setConfigHome(t, filepath.Join(base, "config"))
	setCacheHome(t, filepath.Join(base, "cache"))
	t.Setenv("XDG_DATA_HOME", filepath.Join(base, "data"))
	t.Setenv("SCIGET_CONFIG", "")
	t.Setenv("SCIGET_SYSTEM_DIR", filepath.Join(base, "system"))

	cat := catalog{Name: "neuroimaging", URL: exampleURL("neuroimaging"), Use: true}
	listPath, err := catalogListPath()
	if err != nil {
		t.Fatal(err)
	}
	if err := writeCatalogList(listPath, []catalog{cat}, false); err != nil {
		t.Fatal(err)
	}
	cacheDir, err := defaultValue("cache_dir")
	if err != nil {
		t.Fatal(err)
	}
	dir := cat.dir(cacheDir)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "apps.json"), []byte(mixedCatalog), 0o600); err != nil {
		t.Fatal(err)
	}

	t.Run("x86_64 hides visible arm64 tools and versions", func(t *testing.T) {
		withHostArch(t, "x86_64")
		var out, errBuf bytes.Buffer
		if code := run([]string{"find"}, &out, &errBuf); code != 0 {
			t.Fatalf("find failed (%d): %s", code, errBuf.String())
		}
		want := "amico/2.1.0\tneuroimaging\tdiffusion imaging\t\t\n" +
			"fsl/6.0.7\tneuroimaging\timage registration\t\t\n"
		if out.String() != want {
			t.Errorf("find stdout =\n%s\nwant\n%s", out.String(), want)
		}

		out.Reset()
		errBuf.Reset()
		if code := run([]string{"find", "--all"}, &out, &errBuf); code != 0 {
			t.Fatalf("find --all failed (%d): %s", code, errBuf.String())
		}
		wantAll := "amico/2.1.0\tneuroimaging\tdiffusion imaging\t\t\n" +
			"amico/2.1.0_arm64\tneuroimaging\tdiffusion imaging\t\t\n" +
			"fsl/6.0.7\tneuroimaging\timage registration\t\t\n" +
			"fsl_arm64/6.0.7.23\tneuroimaging\timage registration\t\t\n"
		if out.String() != wantAll {
			t.Errorf("find --all stdout =\n%s\nwant\n%s", out.String(), wantAll)
		}

		out.Reset()
		errBuf.Reset()
		if code := run([]string{"__complete", "info", "fsl"}, &out, &errBuf); code != 0 {
			t.Fatalf("__complete failed (%d): %s", code, errBuf.String())
		}
		if out.String() != "fsl/\n:6\n" {
			t.Errorf("__complete info fsl =\n%s\nwant fsl/ only", out.String())
		}
	})

	t.Run("aarch64 hides explicit x86_64 and shows arm64", func(t *testing.T) {
		withHostArch(t, "aarch64")
		var out, errBuf bytes.Buffer
		if code := run([]string{"find"}, &out, &errBuf); code != 0 {
			t.Fatalf("find failed (%d): %s", code, errBuf.String())
		}
		want := "amico/2.1.0\tneuroimaging\tdiffusion imaging\t\t\n" +
			"amico/2.1.0_arm64\tneuroimaging\tdiffusion imaging\t\t\n" +
			"fsl_arm64/6.0.7.23\tneuroimaging\timage registration\t\t\n"
		if out.String() != want {
			t.Errorf("find stdout =\n%s\nwant\n%s", out.String(), want)
		}
	})
}

func TestArchCatalogPrecedence(t *testing.T) {
	const firstApps = `{
		"shared": {"apps": {"shared 1.0": {"version": "20260101", "architecture": "aarch64"}}},
		"hid": {"apps": {"hid 1.0": {"version": "20260101", "architecture": "x86_64", "show_in_applist": false}}},
		"armonly": {"apps": {"armonly 1.0": {"version": "20260101", "architecture": "aarch64"}}}
	}`
	const secondApps = `{
		"shared": {"apps": {"shared 1.0": {"version": "20260202", "architecture": "x86_64"}}},
		"hid": {"apps": {"hid 1.0": {"version": "20260202", "architecture": "x86_64"}}},
		"armonly": {"apps": {"armonly 1.0": {"version": "20260202", "architecture": "aarch64"}}}
	}`

	base := t.TempDir()
	setConfigHome(t, filepath.Join(base, "config"))
	setCacheHome(t, filepath.Join(base, "cache"))
	t.Setenv("XDG_DATA_HOME", filepath.Join(base, "data"))
	t.Setenv("SCIGET_CONFIG", "")
	t.Setenv("SCIGET_SYSTEM_DIR", filepath.Join(base, "system"))

	cats := []catalog{
		{Name: "first", URL: exampleURL("first"), Use: true},
		{Name: "second", URL: exampleURL("second"), Use: true},
	}
	listPath, err := catalogListPath()
	if err != nil {
		t.Fatal(err)
	}
	if err := writeCatalogList(listPath, cats, false); err != nil {
		t.Fatal(err)
	}
	cacheDir, err := defaultValue("cache_dir")
	if err != nil {
		t.Fatal(err)
	}
	for i, apps := range []string{firstApps, secondApps} {
		dir := cats[i].dir(cacheDir)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "apps.json"), []byte(apps), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	withHostArch(t, "x86_64")

	// Incompatible earlier entry must not shadow a compatible later entry.
	var out, errBuf bytes.Buffer
	if code := run([]string{"find", "shared"}, &out, &errBuf); code != 0 {
		t.Fatalf("find shared failed (%d): %s", code, errBuf.String())
	}
	if want := "shared/1.0\tsecond\t\t\t\n"; out.String() != want {
		t.Errorf("find shared =\n%q\nwant\n%q", out.String(), want)
	}

	// find --all marks the incompatible first copy shadowed and the compatible second unshadowed.
	out.Reset()
	errBuf.Reset()
	if code := run([]string{"find", "--all", "shared"}, &out, &errBuf); code != 0 {
		t.Fatalf("find --all shared failed (%d): %s", code, errBuf.String())
	}
	wantAll := "shared/1.0\tfirst\t\t\t(shadowed)\nshared/1.0\tsecond\t\t\t\n"
	if out.String() != wantAll {
		t.Errorf("find --all shared =\n%q\nwant\n%q", out.String(), wantAll)
	}

	// Exact lookup (info) and completion resolve to the compatible second catalog.
	out.Reset()
	errBuf.Reset()
	if code := run([]string{"info", "shared/1.0"}, &out, &errBuf); code != 0 {
		t.Fatalf("info shared/1.0 failed (%d): %s", code, errBuf.String())
	}
	if !strings.Contains(out.String(), "Catalog:     second (also in: first, shadowed)") ||
		!strings.Contains(out.String(), "20260202") {
		t.Errorf("info shared/1.0 should resolve compatible second catalog:\n%s", out.String())
	}

	out.Reset()
	errBuf.Reset()
	if code := run([]string{"__complete", "info", "shared/"}, &out, &errBuf); code != 0 {
		t.Fatalf("__complete shared/ failed (%d): %s", code, errBuf.String())
	}
	if want := "shared/1.0\tbuilt 20260202\n:4\n"; out.String() != want {
		t.Errorf("__complete shared/ = %q, want %q", out.String(), want)
	}

	// TUI sees shared/1.0 from second, not hidden by first's incompatible copy.
	cs, err := readContainers()
	if err != nil {
		t.Fatal(err)
	}
	m := newModel(tuiSettings{cacheDir: t.TempDir()}, cs, nil, tuiState{})
	if !slices.Equal(m.apps, []string{"shared"}) || m.byTool["shared"][0].catalog != "second" {
		t.Errorf("TUI apps = %v, shared = %+v", m.apps, m.byTool["shared"])
	}

	// A hidden compatible entry in first still shadows a visible compatible entry in second.
	out.Reset()
	errBuf.Reset()
	if code := run([]string{"find", "hid"}, &out, &errBuf); code != 0 {
		t.Fatalf("find hid failed (%d): %s", code, errBuf.String())
	}
	if want := "hid/1.0\tsecond\t\t\t(shadowed)\n"; out.String() != want {
		t.Errorf("find hid = %q, want %q", out.String(), want)
	}

	// When no catalog has a compatible copy, explicit lookup falls back to the first catalog.
	out.Reset()
	errBuf.Reset()
	if code := run([]string{"info", "armonly/1.0"}, &out, &errBuf); code != 0 {
		t.Fatalf("info armonly/1.0 failed (%d): %s", code, errBuf.String())
	}
	if !strings.Contains(out.String(), "Catalog:     first (also in: second, shadowed)") {
		t.Errorf("info armonly/1.0 should fall back to first catalog:\n%s", out.String())
	}
}

func TestAmbiguousLocalStoreGuard(t *testing.T) {
	armBody := []byte("arm64-image-bytes")
	x86Body := []byte("x8664-image-bytes")
	armSum := sha256.Sum256(armBody)
	x86Sum := sha256.Sum256(x86Body)
	armHex := hex.EncodeToString(armSum[:])
	x86Hex := hex.EncodeToString(x86Sum[:])

	firstApps := fmt.Sprintf(`{
		"same": {"apps": {"same 1.0": {"version": "20260101", "architecture": "aarch64", "sha256": %q}}},
		"samesandbox": {"apps": {"samesandbox 1.0": {"version": "20260101", "architecture": "aarch64", "sha256": %q}}},
		"sifonly": {"apps": {"sifonly 1.0": {"version": "20260101", "architecture": "aarch64"}}},
		"omitted": {"apps": {"omitted 1.0": {"version": "20260101", "architecture": "aarch64"}}},
		"anycur": {"apps": {"anycur 1.0": {"version": "20260101", "architecture": "aarch64"}}},
		"anyold": {"apps": {"anyold 1.0": {"version": "20250101", "architecture": "aarch64"}}},
		"oldshare": {"apps": {"oldshare 1.0": {"version": "20250101", "architecture": "aarch64"}}},
		"oldsandbox": {"apps": {"oldsandbox 1.0": {"version": "20250101", "architecture": "aarch64"}}},
		"oldsharedsandbox": {"apps": {"oldsharedsandbox 1.0": {"version": "20250101", "architecture": "aarch64"}}}
	}`, armHex, armHex)
	secondApps := fmt.Sprintf(`{
		"same": {"apps": {"same 1.0": {"version": "20260101", "architecture": "x86_64", "sha256": %q}}},
		"samesandbox": {"apps": {"samesandbox 1.0": {"version": "20260101", "architecture": "x86_64", "sha256": %q}}},
		"sifonly": {"apps": {"sifonly 1.0": {"version": "20260101", "architecture": "x86_64"}}},
		"omitted": {"apps": {"omitted 1.0": {"version": "20260101"}}},
		"anycur": {"apps": {"anycur 1.0": {"version": "20260101", "architecture": "any"}}},
		"anyold": {"apps": {"anyold 1.0": {"version": "20260202", "architecture": "any"}}},
		"oldshare": {"apps": {"oldshare 1.0": {"version": "20260202", "architecture": "x86_64"}}},
		"oldlocal": {"apps": {"oldlocal 1.0": {"version": "20260202", "architecture": "x86_64"}}},
		"oldsandbox": {"apps": {"oldsandbox 1.0": {"version": "20260202", "architecture": "x86_64"}}},
		"oldsharedsandbox": {"apps": {"oldsharedsandbox 1.0": {"version": "20260202", "architecture": "x86_64"}}},
		"loong": {"apps": {"loong 1.0": {"version": "20260101"}}}
	}`, x86Hex, x86Hex)
	thirdApps := `{
		"oldsharedsandbox": {"apps": {"oldsharedsandbox 1.0": {"version": "20250101", "architecture": "x86_64"}}}
	}`

	base := t.TempDir()
	setConfigHome(t, filepath.Join(base, "config"))
	setCacheHome(t, filepath.Join(base, "cache"))
	t.Setenv("XDG_DATA_HOME", filepath.Join(base, "data"))
	t.Setenv("SCIGET_CONFIG", "")
	t.Setenv("SCIGET_SYSTEM_DIR", filepath.Join(base, "system"))
	shareDir := filepath.Join(base, "share")

	cats := []catalog{
		{Name: "first", URL: exampleURL("first"), Use: true},
		{Name: "second", URL: exampleURL("second"), Dir: shareDir, Use: true},
		{Name: "third", URL: exampleURL("third"), Use: true},
	}
	listPath, err := catalogListPath()
	if err != nil {
		t.Fatal(err)
	}
	if err := writeCatalogList(listPath, cats, false); err != nil {
		t.Fatal(err)
	}
	cacheDir, err := defaultValue("cache_dir")
	if err != nil {
		t.Fatal(err)
	}
	for i, apps := range []string{firstApps, secondApps, thirdApps} {
		dir := cats[i].dir(cacheDir)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "apps.json"), []byte(apps), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	// Pre-populate container_dir with the ARM image for same/1.0 on the same build date.
	storeDir, err := defaultValue("container_dir")
	if err != nil {
		t.Fatal(err)
	}
	samePath := filepath.Join(storeDir, "same_1.0_20260101", "same_1.0_20260101.simg")
	if err := os.MkdirAll(filepath.Dir(samePath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(samePath, armBody, 0o644); err != nil {
		t.Fatal(err)
	}

	// Pre-populate container_dir with ARM64 SIF headers for sifonly/1.0, omitted/1.0, and oldlocal/1.0 (unlisted date).
	armSIF := make([]byte, 64)
	copy(armSIF[32:], "SIF_MAGIC\x00")
	copy(armSIF[42:], "01\x00")
	copy(armSIF[45:], "04") // aarch64
	for _, rel := range []string{
		"sifonly_1.0_20260101/sifonly_1.0_20260101.simg",
		"omitted_1.0_20260101/omitted_1.0_20260101.simg",
		"anycur_1.0_20260101/anycur_1.0_20260101.simg",
		"anyold_1.0_20250101/anyold_1.0_20250101.simg",
		"oldlocal_1.0_20240101/oldlocal_1.0_20240101.simg",
	} {
		p := filepath.Join(storeDir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, armSIF, 0o644); err != nil {
			t.Fatal(err)
		}
	}

	// Create local sandbox directories on dates listed by an incompatible catalog
	// (current shared date, older foreign-only date, and older shared date).
	for _, rel := range []string{
		"samesandbox_1.0_20260101/samesandbox_1.0_20260101.simg",
		"oldsandbox_1.0_20250101/oldsandbox_1.0_20250101.simg",
		"oldsharedsandbox_1.0_20250101/oldsharedsandbox_1.0_20250101.simg",
	} {
		if err := os.MkdirAll(filepath.Join(storeDir, filepath.FromSlash(rel)), 0o755); err != nil {
			t.Fatal(err)
		}
	}

	// Create a LoongArch (code "13") SIF fixture for loong/1.0 (omitted architecture).
	loongSIF := make([]byte, 64)
	copy(loongSIF[32:], "SIF_MAGIC\x00")
	copy(loongSIF[42:], "01\x00")
	copy(loongSIF[45:], "13") // loong64
	loongPath := filepath.Join(storeDir, "loong_1.0_20260101", "loong_1.0_20260101.simg")
	if err := os.MkdirAll(filepath.Dir(loongPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(loongPath, loongSIF, 0o644); err != nil {
		t.Fatal(err)
	}

	// Put a valid older x86_64 image on second's share on 20250101 (same date first lists ARM64).
	oldSharePath := filepath.Join(shareDir, "oldshare_1.0_20250101", "oldshare_1.0_20250101.simg")
	if err := os.MkdirAll(filepath.Dir(oldSharePath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(oldSharePath, x86Body, 0o644); err != nil {
		t.Fatal(err)
	}

	withHostArch(t, "x86_64")

	// info must not report the ARM cached file as the native x86_64 image.
	var out, errBuf bytes.Buffer
	if code := run([]string{"info", "same/1.0"}, &out, &errBuf); code != 0 {
		t.Fatalf("info same/1.0 failed: %s", errBuf.String())
	}
	if !strings.Contains(out.String(), "Catalog:     second") || !strings.Contains(out.String(), "Location:    not on this machine") {
		t.Errorf("info same/1.0 should reject foreign-checksum cached image:\n%s", out.String())
	}

	// storePath (used by install) must also reject the foreign-checksum cached image.
	cfg, err := readConfig()
	if err != nil {
		t.Fatal(err)
	}
	cs, err := readContainers()
	if err != nil {
		t.Fatal(err)
	}
	sameC, err := lookupContainer(cs, "same/1.0")
	if err != nil {
		t.Fatal(err)
	}
	if sp, err := storePath(cfg, sameC); err != nil || sp != "" {
		t.Errorf("storePath(same/1.0) = %q, %v; want empty for foreign cached image", sp, err)
	}

	// SIF header arch mismatch rejects foreign cached images both with explicit and omitted architecture,
	// and rejects older local ARM64 SIFs on unlisted dates.
	for _, name := range []string{"sifonly/1.0", "omitted/1.0", "anycur/1.0", "anyold/1.0", "oldlocal/1.0", "samesandbox/1.0", "oldsandbox/1.0", "oldsharedsandbox/1.0", "loong/1.0"} {
		out.Reset()
		errBuf.Reset()
		if code := run([]string{"info", name}, &out, &errBuf); code != 0 {
			t.Fatalf("info %s failed: %s", name, errBuf.String())
		}
		if !strings.Contains(out.String(), "Location:    not on this machine") {
			t.Errorf("info %s should reject foreign-arch SIF header:\n%s", name, out.String())
		}
	}

	// Older build on the selected catalog's share must NOT be skipped just because another catalog listed ARM64 on that date.
	out.Reset()
	errBuf.Reset()
	if code := run([]string{"info", "oldshare/1.0"}, &out, &errBuf); code != 0 {
		t.Fatalf("info oldshare/1.0 failed: %s", errBuf.String())
	}
	if !strings.Contains(out.String(), "Location:    "+oldSharePath+" (an older build)") {
		t.Errorf("info oldshare/1.0 should find older share build:\n%s", out.String())
	}

	// Replacing same/1.0 with the matching x86_64 bytes makes info and storePath accept it.
	if err := os.WriteFile(samePath, x86Body, 0o644); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	errBuf.Reset()
	if code := run([]string{"info", "same/1.0"}, &out, &errBuf); code != 0 {
		t.Fatalf("info same/1.0 failed after native write: %s", errBuf.String())
	}
	if !strings.Contains(out.String(), "Location:    "+samePath) {
		t.Errorf("info same/1.0 should accept matching native image:\n%s", out.String())
	}
	if sp, err := storePath(cfg, sameC); err != nil || sp != samePath {
		t.Errorf("storePath(same/1.0) after native write = %q, %v; want %q", sp, err, samePath)
	}
}

func TestPullAmbiguousStoreGuards(t *testing.T) {
	withHostArch(t, "x86_64")
	var requests int
	x86SIF := make([]byte, 64)
	copy(x86SIF[32:], "SIF_MAGIC\x00")
	copy(x86SIF[42:], "01\x00")
	copy(x86SIF[45:], "02") // x86_64

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		if strings.HasPrefix(r.URL.Path, "/verified_") {
			_, _ = w.Write(x86SIF)
			return
		}
		_, _ = w.Write([]byte("not-a-sif-and-no-checksum"))
	}))
	defer srv.Close()

	base := t.TempDir()
	setConfigHome(t, filepath.Join(base, "config"))
	setCacheHome(t, filepath.Join(base, "cache"))
	t.Setenv("XDG_DATA_HOME", filepath.Join(base, "data"))
	t.Setenv("SCIGET_SYSTEM_DIR", filepath.Join(base, "system"))
	cfgPath := filepath.Join(base, "config.json")
	t.Setenv("SCIGET_CONFIG", cfgPath)
	storeDir := filepath.Join(base, "containers")
	if err := writeConfig(cfgPath, map[string]string{
		"container_dir":  storeDir,
		"apptainer_path": "true",
	}); err != nil {
		t.Fatal(err)
	}

	firstApps := `{
		"sandbox": {"apps": {"sandbox 1.0": {"version": "20260101", "architecture": "aarch64"}}},
		"unverified": {"apps": {"unverified 1.0": {"version": "20260101", "architecture": "aarch64"}}},
		"verified": {"apps": {"verified 1.0": {"version": "20260101", "architecture": "aarch64"}}}
	}`
	secondApps := `{
		"sandbox": {"apps": {"sandbox 1.0": {"version": "20260101", "architecture": "x86_64"}}},
		"unverified": {"apps": {"unverified 1.0": {"version": "20260101", "architecture": "x86_64"}}},
		"verified": {"apps": {"verified 1.0": {"version": "20260101", "architecture": "x86_64"}}}
	}`
	catMeta := fmt.Sprintf(`{"schemaVersion": 1, "appJsonUrl": "apps.json", "downloadUrls": [%q]}`, srv.URL+"/{file}")
	cats := []catalog{
		{Name: "first", URL: exampleURL("first"), Use: true},
		{Name: "second", URL: exampleURL("second"), Use: true},
	}
	listPath, err := catalogListPath()
	if err != nil {
		t.Fatal(err)
	}
	if err := writeCatalogList(listPath, cats, false); err != nil {
		t.Fatal(err)
	}
	cacheDir, err := defaultValue("cache_dir")
	if err != nil {
		t.Fatal(err)
	}
	for i, apps := range []string{firstApps, secondApps} {
		dir := cats[i].dir(cacheDir)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "catalog.json"), []byte(catMeta), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "apps.json"), []byte(apps), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	// 1. Sandbox occupying dest fails before downloading and tells the user which sandbox to move.
	sandboxDest := filepath.Join(storeDir, "sandbox_1.0_20260101", "sandbox_1.0_20260101.simg")
	if err := os.MkdirAll(sandboxDest, 0o755); err != nil {
		t.Fatal(err)
	}
	var out, errBuf bytes.Buffer
	if code := run([]string{"pull", "sandbox/1.0"}, &out, &errBuf); code == 0 {
		t.Fatalf("pull sandbox/1.0 unexpectedly succeeded: out=%s", out.String())
	}
	if requests != 0 {
		t.Errorf("pull downloaded %d times despite sandbox directory at dest", requests)
	}
	if !strings.Contains(errBuf.String(), sandboxDest) || !strings.Contains(errBuf.String(), "is a directory") {
		t.Errorf("unexpected sandbox error:\n%s", errBuf.String())
	}

	// 2. Shared-date download without checksum or valid SIF header is rejected instead of published.
	out.Reset()
	errBuf.Reset()
	if code := run([]string{"pull", "unverified/1.0"}, &out, &errBuf); code == 0 {
		t.Fatalf("pull unverified/1.0 unexpectedly succeeded: out=%s", out.String())
	}
	if !strings.Contains(errBuf.String(), "does not verify") {
		t.Errorf("expected verification error, got:\n%s", errBuf.String())
	}
	unverifiedDest := filepath.Join(storeDir, "unverified_1.0_20260101", "unverified_1.0_20260101.simg")
	if _, err := os.Stat(unverifiedDest); !os.IsNotExist(err) {
		t.Errorf("rejected download should not be published at %s", unverifiedDest)
	}

	// 3. Shared-date download with a matching x86_64 SIF header succeeds and info reports it present.
	out.Reset()
	errBuf.Reset()
	if code := run([]string{"pull", "verified/1.0"}, &out, &errBuf); code != 0 {
		t.Fatalf("pull verified/1.0 failed (%d): %s", code, errBuf.String())
	}
	out.Reset()
	errBuf.Reset()
	if code := run([]string{"info", "verified/1.0"}, &out, &errBuf); code != 0 {
		t.Fatalf("info verified/1.0 failed: %s", errBuf.String())
	}
	verifiedDest := filepath.Join(storeDir, "verified_1.0_20260101", "verified_1.0_20260101.simg")
	if !strings.Contains(out.String(), "Location:    "+verifiedDest) {
		t.Errorf("info verified/1.0 should report published image:\n%s", out.String())
	}
}
