package main

import (
	"bytes"
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
	apps   string // apps.json in its cache dir; none when empty
	dir    string
	layout string
	unused bool // registered with "use": false
}

// TestMain points the system layer at an empty folder, so no test reads
// the real /etc/sciget.
func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "sciget-system-")
	if err != nil {
		panic(err)
	}
	systemDir = dir
	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}

// setSystemDir points the system layer at dir for one test.
func setSystemDir(t *testing.T, dir string) {
	t.Helper()
	old := systemDir
	systemDir = dir
	t.Cleanup(func() { systemDir = old })
}

// exampleURL is the URL a testCatalog gets when none is given and nothing
// is served.
func exampleURL(name string) string { return "https://example.org/" + name + "/apps.json" }

func TestRun(t *testing.T) {
	const helpText = "Usage:\n  sciget [flags]\n  sciget [command]\n"
	const usageHint = "\nRun 'sciget help' for usage.\n"
	const fslCatalog = `{"fsl": {"categories": ["image registration"], "apps": {
		"fsl 6.0.3": {"version": "20200905", "exec": ""},
		"fsleyesGUI-fsl 6.0.3": {"version": "20200905", "exec": "fsleyes"},
		"fsl 6.0.3 arm64": {"version": "20260101", "image": "fsl_6.0.3_arm64", "show_in_applist": false}}}}`
	const labCatalog = `{"fsl": {"apps": {"fsl 6.0.3": {"version": "20240101"}}}}`
	const hiddenCatalog = `{"fsl": {"apps": {"fsl 6.0.3": {"version": "1", "show_in_applist": false}}}}`
	const mirrorURL = "https://mirror.example.org/neuroimaging/apps.json"
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
		"download_urls\t[\"https://object-store.rc.nectar.org.au/v1/AUTH_dead991e1fa847e3afcca2d3a7041f5d/neurodesk/{file}\",\"https://neurocontainers.s3.us-east-2.amazonaws.com/{file}\"]\tdefault\n" +
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
		serve            map[string]string // when set, a test server serves each body at /<name>/apps.json, else 404
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
			wantStderrHas: "/catalogs/neuroimaging-77a7c3e239d9/apps.json; run 'sciget refresh'\n"},
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
			wantStderr: "sciget: no catalog at /nowhere/catalogs/neuroimaging-77a7c3e239d9/apps.json; " +
				"run 'sciget refresh'\n"},
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
			wantRequests: []string{"/mylab/apps.json", "/neuroimaging/apps.json"},
			wantStdout:   "Catalog \"mylab\" updated.\nCatalog \"neuroimaging\" is up to date.\n"},
		{name: "refresh carries on after a failure", args: []string{"refresh"},
			catalogs: []testCatalog{{name: "cryoem"}, {name: "neuroimaging"}},
			serve:    map[string]string{"neuroimaging": fslCatalog}, wantCode: 1,
			wantStdout:    "Catalog \"neuroimaging\" updated.\n",
			wantStderrHas: "sciget: catalog \"cryoem\": downloading catalog: http://127.0.0.1:"},
		{name: "refresh with two failures", args: []string{"refresh"},
			catalogs: []testCatalog{{name: "a"}, {name: "b"}}, serve: map[string]string{}, wantCode: 1,
			wantStderrHas: "/a/apps.json returned 404 Not Found\nsciget: catalog \"b\": "},
		{name: "refresh skips catalogs not in use", args: []string{"refresh"},
			catalogs:     []testCatalog{{name: "old", unused: true}, {name: "neuroimaging"}},
			serve:        map[string]string{"old": fslCatalog, "neuroimaging": fslCatalog},
			wantRequests: []string{"/neuroimaging/apps.json"},
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
		{name: "catalog add a bad URL", args: []string{"catalog", "add", "mylab", "example.org/apps.json"}, wantCode: 2,
			wantStderr: "sciget: invalid catalog URL: must start with https:// or http://" + usageHint},
		{name: "catalog add a plain http URL", args: []string{"catalog", "add", "mylab", "http://mirror.local/apps.json"}, wantCode: 2,
			wantStderr: "sciget: invalid catalog URL: must use https; plain http is allowed only to this machine (localhost)" + usageHint},
		{name: "catalog add plain http to this machine", args: []string{"catalog", "add", "mylab", "http://localhost:8080/apps.json"},
			wantStdout: "Catalog \"mylab\" added at the top. Run 'sciget refresh' to download it.\n",
			wantList:   []catalog{{Name: "mylab", URL: "http://localhost:8080/apps.json", Use: true}}},
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
			catalogs: []testCatalog{{name: "neuro", url: "https://lab.example/neuro.json"}}, systemCatalogs: []testCatalog{{name: "neuro"}},
			wantStdout: "neuro\t1\tuser\thttps://lab.example/neuro.json\t\n"},
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
		{name: "catalog add of a system name", args: []string{"catalog", "add", "neuro", "https://lab.example/neuro.json"},
			systemCatalogs: []testCatalog{{name: "neuro"}}, wantCode: 1, wantStderrHas: "catalogs.json; choose another name\n"},
		{name: "catalog add of a system name even with --force-update",
			args:           []string{"catalog", "add", "--force-update", "neuro", "https://lab.example/neuro.json"},
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
			args:           []string{"catalog", "add", "--system", "--force-update", "neuro", "https://lab.example/neuro.json"},
			systemCatalogs: []testCatalog{{name: "neuro", apps: "[]"}},
			wantStdout:     "Catalog \"neuro\" added at the top. Run 'sciget refresh' to download it.\n",
			wantSystemList: []catalog{{Name: "neuro", URL: "https://lab.example/neuro.json", Use: true}}, wantCaches: []string{"neuro"}},
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
			catalogs: []testCatalog{{name: "neuro", url: "https://lab.example/neuro.json"}}, systemCatalogs: []testCatalog{{name: "neuro"}},
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
					name := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/"), "/apps.json")
					body, ok := tt.serve[name]
					if !ok {
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
					c := catalog{Name: tc.name, URL: tc.url, Use: !tc.unused, Dir: tc.dir, Layout: tc.layout}
					switch {
					case c.URL != "":
					case base != "":
						c.URL = base + "/" + tc.name + "/apps.json"
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
