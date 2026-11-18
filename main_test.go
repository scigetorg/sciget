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

// testCatalogue is one catalogue a TestRun row registers.
type testCatalogue struct {
	name   string
	url    string // exampleURL(name), or the test server's, when empty
	apps   string // apps.json in its cache dir; none when empty
	dir    string
	layout string
	unused bool // registered with "use": false
}

// exampleURL is the URL a testCatalogue gets when none is given and nothing
// is served.
func exampleURL(name string) string { return "https://example.org/" + name + "/apps.json" }

func TestRun(t *testing.T) {
	const helpText = "Usage:\n  sciget [flags]\n  sciget [command]\n"
	const usageHint = "\nRun 'sciget help' for usage.\n"
	const fslCatalogue = `{"fsl": {"categories": ["image registration"], "apps": {
		"fsl 6.0.3": {"version": "20200905", "exec": ""},
		"fsleyesGUI-fsl 6.0.3": {"version": "20200905", "exec": "fsleyes"},
		"fsl 6.0.3 arm64": {"version": "20260101", "image": "fsl_6.0.3_arm64", "show_in_applist": false}}}}`
	const labCatalogue = `{"fsl": {"apps": {"fsl 6.0.3": {"version": "20240101"}}}}`
	const hiddenCatalogue = `{"fsl": {"apps": {"fsl 6.0.3": {"version": "1", "show_in_applist": false}}}}`
	const mirrorURL = "https://mirror.example.org/neuroimaging/apps.json"
	const noCatalogues = "sciget: no catalogues; run 'sciget catalogue add <name> <url>'\n"
	const noneInUse = "sciget: no catalogues in use; run 'sciget catalogue use <name>'\n"
	neuroimaging := []testCatalogue{{name: "neuroimaging", apps: fslCatalogue}}
	const fslStored = "fsl_6.0.3_20200905/fsl_6.0.3_20200905.simg"
	const fuseCatalogue = `{"fsl": {"apps": {"fsl 6.0.3": {"version": "20200905", "apptainer_args": ["--bind", "/dev/fuse"]}}}}`
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
		"use_catalogue_args\ttrue\tdefault\n" +
		"max_catalogue_size\t67108864\tdefault\n" +
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
		name            string
		args            []string
		before          [][]string        // commands run first, each of which must succeed
		config          string            // written to SCIGET_CONFIG; no file when empty
		catalogues      []testCatalogue   // written to catalogues.json, in order
		list            string            // written to catalogues.json instead, when set
		readOnlyList    bool              // write catalogues.json with mode 0o400
		lockedCaches    bool              // cache_dir/catalogues allows no deletions
		serve           map[string]string // when set, a test server serves each body at /<name>/apps.json, else 404
		readOnly        bool              // write config with mode 0o400
		env             map[string]string // set after the defaults
		stored          []string          // paths created under the default container_dir
		wantExec        []string          // argv handed to Apptainer, when set; <store> and <cwd> stand for those paths
		wantConfig      string            // the config file afterwards, when set
		wantList        []catalogue       // catalogues.json afterwards, when set
		wantListSame    bool              // catalogues.json is byte for byte as written
		wantCaches      []string          // names of the dirs in cache_dir/catalogues afterwards, when set
		wantRequests    []string          // paths the server was asked for, when set
		wantCode        int
		wantStdout      string // exact, unless wantStdoutHas is set; <store> and <cwd> as in wantExec
		wantStdoutHas   string // checks only that stdout contains this
		wantStdoutLacks string
		wantStderr      string // exact, unless wantStderrHas is set
		wantStderrHas   string
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

		{name: "find no match", args: []string{"find", "spm"}, catalogues: neuroimaging,
			wantStderr: "No containers match \"spm\".\n"},
		{name: "find no match joins words", args: []string{"find", "spm", "12"}, catalogues: neuroimaging,
			wantStderr: "No containers match \"spm 12\".\n"},
		{name: "info unknown", args: []string{"info", "fsl/9"}, catalogues: neuroimaging, wantCode: 1,
			wantStderr: "sciget: no container named \"fsl/9\"; run 'sciget find fsl' to list versions, or 'sciget refresh' to update the catalogues\n"},
		{name: "info", args: []string{"info", "fsl/6.0.3"}, catalogues: neuroimaging,
			wantStdout: "Name:        fsl/6.0.3\nCatalogue:   neuroimaging\nBuild:       20200905 (fsl_6.0.3_20200905)\nCategories:  image registration\nLicence:     (not stated)\nDOI:         (none)\nGUI:         yes\nLocation:    not on this machine\n"},
		{name: "info missing argument", args: []string{"info"}, wantCode: 2,
			wantStderr: "sciget: accepts 1 arg(s), received 0" + usageHint},
		{name: "info completes names", args: []string{"__complete", "info", "fsl/"}, catalogues: neuroimaging,
			wantStdout: "fsl/6.0.3\tbuilt 20200905\n:4\n", wantStderrHas: "Completion ended with directive"},

		{name: "apptainer hidden from help", args: []string{"--help"}, wantStdoutHas: "info ", wantStdoutLacks: "apptainer"},
		{name: "apptainer exec", args: []string{"apptainer", "exec", "fsl/6.0.3", "bet", "--help"},
			catalogues: neuroimaging, config: `{"apptainer_path": "/bin/sh"}`, stored: []string{fslStored},
			wantExec: []string{"/bin/sh", "--silent", "exec", "--cleanenv", "--pwd", "<cwd>", "<store>/" + fslStored, "bet", "--help"}},
		{name: "apptainer shell", args: []string{"apptainer", "shell", "fsl/6.0.3"},
			catalogues: neuroimaging, config: `{"apptainer_path": "/bin/sh"}`, stored: []string{fslStored},
			wantExec: []string{"/bin/sh", "--silent", "shell", "--cleanenv", "--pwd", "<cwd>", "<store>/" + fslStored}},
		{name: "apptainer dry run", args: []string{"apptainer", "exec", "--dry-run", "fsl/6.0.3", "bet", "", "it's"},
			catalogues: neuroimaging, config: `{"apptainer_path": "/bin/sh"}`, stored: []string{fslStored},
			wantStdout: "/bin/sh --silent exec --cleanenv --pwd <cwd> <store>/" + fslStored + " bet '' 'it'\\''s'\n"},
		{name: "apptainer dry run with display and extra args", args: []string{"apptainer", "shell", "--dry-run", "fsl/6.0.3"},
			catalogues: neuroimaging, config: `{"apptainer_path": "/bin/sh", "apptainer_extra_args": ["--writable-tmpfs"]}`,
			stored: []string{fslStored}, env: map[string]string{"DISPLAY": ":1"},
			wantStdout: "/bin/sh --silent shell --cleanenv --env DISPLAY=:1 --pwd <cwd> --writable-tmpfs <store>/" + fslStored + "\n"},
		{name: "apptainer catalogue args", args: []string{"apptainer", "shell", "--dry-run", "fsl/6.0.3"},
			catalogues: []testCatalogue{{name: "neuroimaging", apps: fuseCatalogue}}, config: `{"apptainer_path": "/bin/sh"}`,
			stored:     []string{fslStored},
			wantStdout: "/bin/sh --silent shell --cleanenv --pwd <cwd> --bind /dev/fuse <store>/" + fslStored + "\n"},
		{name: "apptainer catalogue args turned off", args: []string{"apptainer", "shell", "--dry-run", "fsl/6.0.3"},
			catalogues: []testCatalogue{{name: "neuroimaging", apps: fuseCatalogue}},
			config:     `{"apptainer_path": "/bin/sh", "use_catalogue_args": false}`, stored: []string{fslStored},
			wantStdout: "/bin/sh --silent shell --cleanenv --pwd <cwd> <store>/" + fslStored + "\n"},
		{name: "apptainer exec with overridden settings", args: []string{"apptainer", "exec", "--dry-run", "fsl/6.0.3", "true"},
			catalogues: neuroimaging, stored: []string{fslStored},
			config: `{"apptainer_path": "/bin/sh", "apptainer_global_args": ["--debug"],
				"apptainer_exec_args": ["--contain", "{env}", "{xauthority}", "{tmp}"], "apptainer_pass_env": ["MYVAR"],
				"xauthority_var": "MYXAUTH", "tmp_vars": ["MYTMP"], "tmp_target": "/scratch"}`,
			env: map[string]string{"MYVAR": "v", "MYXAUTH": filepath.Join(physicalCwd, "main_test.go"), "MYTMP": "/t", "DISPLAY": ":0"},
			wantStdout: "/bin/sh --debug exec --contain --env MYVAR=v --bind <cwd>/main_test.go:<cwd>/main_test.go:ro" +
				" --env MYXAUTH=<cwd>/main_test.go --bind /t:/scratch <store>/" + fslStored + " true\n"},
		{name: "apptainer shell with overridden settings", args: []string{"apptainer", "shell", "--dry-run", "fsl/6.0.3"},
			catalogues: neuroimaging, stored: []string{fslStored},
			config:     `{"apptainer_path": "/bin/sh", "apptainer_global_args": [], "apptainer_shell_args": ["--no-home"]}`,
			wantStdout: "/bin/sh shell --no-home <store>/" + fslStored + "\n"},
		{name: "apptainer display with a comma", args: []string{"apptainer", "shell", "fsl/6.0.3"},
			catalogues: neuroimaging, config: `{"apptainer_path": "/bin/sh"}`, stored: []string{fslStored},
			env: map[string]string{"DISPLAY": "a,b"}, wantCode: 1,
			wantStderr: "sciget: cannot pass DISPLAY to the container: its value contains \",\"\n"},
		{name: "apptainer exec unknown name", args: []string{"apptainer", "exec", "fsl/9", "bet"},
			catalogues: neuroimaging, wantCode: 1,
			wantStderr: "sciget: no container named \"fsl/9\"; run 'sciget find fsl' to list versions, or 'sciget refresh' to update the catalogues\n"},
		{name: "apptainer shell unknown name", args: []string{"apptainer", "shell", "fsl/9"},
			catalogues: neuroimaging, wantCode: 1,
			wantStderr: "sciget: no container named \"fsl/9\"; run 'sciget find fsl' to list versions, or 'sciget refresh' to update the catalogues\n"},
		{name: "apptainer unknown verb", args: []string{"apptainer", "exex", "fsl/6.0.3", "bet"}, wantCode: 2,
			wantStderr: "sciget: unknown command \"exex\" for \"sciget apptainer\"" + usageHint},
		{name: "apptainer alone shows help", args: []string{"apptainer"}, wantStdoutHas: "Run containers with Apptainer"},
		{name: "apptainer exec not on this machine", args: []string{"apptainer", "exec", "fsl/6.0.3", "bet"},
			catalogues: neuroimaging, wantCode: 1, wantStderr: "sciget: fsl/6.0.3 is not on this machine; to download it, run: sciget pull fsl/6.0.3, or let sciget download when needed: sciget config set auto_pull true\n"},
		{name: "apptainer not found", args: []string{"apptainer", "shell", "fsl/6.0.3"},
			catalogues: neuroimaging, config: `{"apptainer_path": "no-such-apptainer"}`, stored: []string{fslStored},
			wantCode: 1, wantStderr: "sciget: apptainer not found: \"no-such-apptainer\"; install Apptainer or set apptainer_path\n"},
		{name: "apptainer flag before the name", args: []string{"apptainer", "exec", "--bind", "/x", "fsl/6.0.3", "bet"},
			catalogues: neuroimaging, wantCode: 2, wantStderrHas: "unknown flag: --bind"},
		{name: "apptainer exec needs a command", args: []string{"apptainer", "exec", "fsl/6.0.3"},
			catalogues: neuroimaging, wantCode: 2, wantStderrHas: "requires at least 2 arg(s)"},
		{name: "apptainer shell takes one name", args: []string{"apptainer", "shell", "fsl/6.0.3", "bash"},
			catalogues: neuroimaging, wantCode: 2, wantStderrHas: "accepts 1 arg(s), received 2"},

		{name: "find without catalogues", args: []string{"find"}, wantCode: 1, wantStderr: noCatalogues},
		{name: "find with none in use", args: []string{"find"}, wantCode: 1, wantStderr: noneInUse,
			catalogues: []testCatalogue{{name: "neuroimaging", apps: fslCatalogue, unused: true}}},
		{name: "find before a refresh", args: []string{"find"}, wantCode: 1,
			catalogues:    []testCatalogue{{name: "neuroimaging"}},
			wantStderrHas: "/catalogues/neuroimaging-77a7c3e239d9/apps.json; run 'sciget refresh'\n"},
		{name: "find", args: []string{"find", "fsl"}, catalogues: neuroimaging,
			wantStdout: "fsl/6.0.3\tneuroimaging\timage registration\tyes\t\n"},
		{name: "find --all", args: []string{"find", "--all", "fsl"}, catalogues: neuroimaging,
			wantStdout: "fsl/6.0.3\tneuroimaging\timage registration\tyes\t\n" +
				"fsl/6.0.3_arm64\tneuroimaging\timage registration\t\t\n"},
		{name: "find marks a shadowed copy", args: []string{"find", "fsl"},
			catalogues: []testCatalogue{{name: "mylab", apps: labCatalogue}, {name: "neuroimaging", apps: fslCatalogue}},
			wantStdout: "fsl/6.0.3\tmylab\t\t\t\nfsl/6.0.3\tneuroimaging\timage registration\tyes\t(shadowed)\n"},
		{name: "find follows catalogue order", args: []string{"find", "fsl"},
			catalogues: []testCatalogue{{name: "neuroimaging", apps: fslCatalogue}, {name: "mylab", apps: labCatalogue}},
			wantStdout: "fsl/6.0.3\tneuroimaging\timage registration\tyes\t\nfsl/6.0.3\tmylab\t\t\t(shadowed)\n"},
		{name: "find follows a reorder by catalogue use", before: [][]string{{"catalogue", "use", "mylab"}},
			args:       []string{"find", "fsl"},
			catalogues: []testCatalogue{{name: "neuroimaging", apps: fslCatalogue}, {name: "mylab", apps: labCatalogue}},
			wantStdout: "fsl/6.0.3\tmylab\t\t\t\nfsl/6.0.3\tneuroimaging\timage registration\tyes\t(shadowed)\n"},
		{name: "a hidden copy still shadows", args: []string{"find", "fsl"},
			catalogues: []testCatalogue{{name: "a", apps: hiddenCatalogue}, {name: "neuroimaging", apps: fslCatalogue}},
			wantStdout: "fsl/6.0.3\tneuroimaging\timage registration\tyes\t(shadowed)\n"},
		{name: "find skips catalogues not in use", args: []string{"find", "fsl"},
			catalogues: []testCatalogue{{name: "mylab", apps: labCatalogue, unused: true}, {name: "neuroimaging", apps: fslCatalogue}},
			wantStdout: "fsl/6.0.3\tneuroimaging\timage registration\tyes\t\n"},
		{name: "find with one catalogue not downloaded", args: []string{"find"}, wantCode: 1,
			catalogues:    []testCatalogue{{name: "mylab"}, {name: "neuroimaging", apps: fslCatalogue}},
			wantStderrHas: "/catalogues/mylab-"},
		{name: "find completes versions", args: []string{"__complete", "find", "fsl/"}, catalogues: neuroimaging,
			wantStdout: "fsl/6.0.3\tbuilt 20200905\n:4\n", wantStderrHas: "Completion ended with directive"},
		{name: "find completes the copy that runs", args: []string{"__complete", "find", "fsl/"},
			catalogues: []testCatalogue{{name: "mylab", apps: labCatalogue}, {name: "neuroimaging", apps: fslCatalogue}},
			wantStdout: "fsl/6.0.3\tbuilt 20240101\n:4\n", wantStderrHas: "Completion ended with directive"},
		{name: "find --all completes hidden versions", args: []string{"__complete", "find", "--all", "fsl/"},
			catalogues: neuroimaging, wantStdout: "fsl/6.0.3\tbuilt 20200905\nfsl/6.0.3_arm64\tbuilt 20260101\n:4\n",
			wantStderrHas: "Completion ended with directive"},
		{name: "find completes tool names", args: []string{"__complete", "find", "fs"}, catalogues: neuroimaging,
			wantStdout: "fsl/\n:6\n", wantStderrHas: "Completion ended with directive"},
		{name: "find completion without catalogues", args: []string{"__complete", "find", "fs"},
			wantStdout: ":1\n", wantStderrHas: "Completion ended with directive: ShellCompDirectiveError"},
		{name: "find reads cache_dir from the config", args: []string{"find"},
			config: `{"cache_dir": "/nowhere"}`, catalogues: []testCatalogue{{name: "neuroimaging"}}, wantCode: 1,
			wantStderr: "sciget: no catalogue at /nowhere/catalogues/neuroimaging-77a7c3e239d9/apps.json; " +
				"run 'sciget refresh'\n"},
		{name: "find with a broken config", args: []string{"find"}, catalogues: neuroimaging,
			config: `{"cache-dir": "/c"}`, wantCode: 1, wantStderrHas: "sciget: reading config "},
		{name: "find with a broken list", args: []string{"find"}, list: "{", wantCode: 1,
			wantStderrHas: "sciget: reading catalogues "},
		{name: "find completion with a broken config", args: []string{"__complete", "find", "fs"},
			catalogues: neuroimaging, config: "{", wantStdout: ":1\n",
			wantStderrHas: "Completion ended with directive: ShellCompDirectiveError"},

		{name: "refresh", args: []string{"refresh"}, catalogues: []testCatalogue{{name: "neuroimaging"}},
			serve: map[string]string{"neuroimaging": fslCatalogue}, wantStdout: "Catalogue \"neuroimaging\" updated.\n"},
		{name: "refresh over max_catalogue_size", args: []string{"refresh"}, catalogues: []testCatalogue{{name: "neuroimaging"}},
			config: `{"max_catalogue_size": 10}`, serve: map[string]string{"neuroimaging": fslCatalogue}, wantCode: 1,
			wantStderrHas: "is larger than max_catalogue_size (10 bytes)"},
		{name: "refresh when up to date", args: []string{"refresh"}, catalogues: neuroimaging,
			serve: map[string]string{"neuroimaging": fslCatalogue}, wantStdout: "Catalogue \"neuroimaging\" is up to date.\n"},
		{name: "refresh in order", args: []string{"refresh"},
			catalogues:   []testCatalogue{{name: "mylab"}, {name: "neuroimaging", apps: fslCatalogue}},
			serve:        map[string]string{"mylab": labCatalogue, "neuroimaging": fslCatalogue},
			wantRequests: []string{"/mylab/apps.json", "/neuroimaging/apps.json"},
			wantStdout:   "Catalogue \"mylab\" updated.\nCatalogue \"neuroimaging\" is up to date.\n"},
		{name: "refresh carries on after a failure", args: []string{"refresh"},
			catalogues: []testCatalogue{{name: "cryoem"}, {name: "neuroimaging"}},
			serve:      map[string]string{"neuroimaging": fslCatalogue}, wantCode: 1,
			wantStdout:    "Catalogue \"neuroimaging\" updated.\n",
			wantStderrHas: "sciget: catalogue \"cryoem\": downloading catalogue: http://127.0.0.1:"},
		{name: "refresh with two failures", args: []string{"refresh"},
			catalogues: []testCatalogue{{name: "a"}, {name: "b"}}, serve: map[string]string{}, wantCode: 1,
			wantStderrHas: "/a/apps.json returned 404 Not Found\nsciget: catalogue \"b\": "},
		{name: "refresh skips catalogues not in use", args: []string{"refresh"},
			catalogues:   []testCatalogue{{name: "old", unused: true}, {name: "neuroimaging"}},
			serve:        map[string]string{"old": fslCatalogue, "neuroimaging": fslCatalogue},
			wantRequests: []string{"/neuroimaging/apps.json"},
			wantStdout:   "Catalogue \"neuroimaging\" updated.\n"},
		{name: "refresh without catalogues", args: []string{"refresh"}, wantCode: 1, wantStderr: noCatalogues},
		{name: "refresh with none in use", args: []string{"refresh"}, wantCode: 1, wantStderr: noneInUse,
			catalogues: []testCatalogue{{name: "neuroimaging", unused: true}}},
		{name: "refresh takes no arguments", args: []string{"refresh", "x"}, wantCode: 2,
			wantStderr: "sciget: unknown command \"x\" for \"sciget refresh\"" + usageHint},
		{name: "refresh with a broken config", args: []string{"refresh"}, config: `{"bogus": "x"}`, wantCode: 1,
			wantStderrHas: `unknown key "bogus"`},

		{name: "catalogue add", args: []string{"catalogue", "add", "neuroimaging", exampleURL("neuroimaging")},
			wantStdout: "Catalogue \"neuroimaging\" added at the top. Run 'sciget refresh' to download it.\n",
			wantList:   []catalogue{{Name: "neuroimaging", URL: exampleURL("neuroimaging"), Use: true}}},
		{name: "catalogue add puts it first", args: []string{"catalogue", "add", "mylab", exampleURL("mylab")},
			catalogues: []testCatalogue{{name: "neuroimaging"}},
			wantStdout: "Catalogue \"mylab\" added at the top. Run 'sciget refresh' to download it.\n",
			wantList: []catalogue{{Name: "mylab", URL: exampleURL("mylab"), Use: true},
				{Name: "neuroimaging", URL: exampleURL("neuroimaging"), Use: true}}},
		{name: "catalogue add --append", args: []string{"catalogue", "add", "--append", "mylab", exampleURL("mylab")},
			catalogues: []testCatalogue{{name: "neuroimaging"}},
			wantStdout: "Catalogue \"mylab\" added at the bottom. Run 'sciget refresh' to download it.\n",
			wantList: []catalogue{{Name: "neuroimaging", URL: exampleURL("neuroimaging"), Use: true},
				{Name: "mylab", URL: exampleURL("mylab"), Use: true}}},
		{name: "catalogue add the same again", catalogues: []testCatalogue{{name: "mylab"}, {name: "neuroimaging", unused: true}},
			args:         []string{"catalogue", "add", "neuroimaging", exampleURL("neuroimaging")},
			wantListSame: true,
			wantStdout:   "Catalogue \"neuroimaging\" already exists with the same settings; nothing to do.\n",
			wantList: []catalogue{{Name: "mylab", URL: exampleURL("mylab"), Use: true},
				{Name: "neuroimaging", URL: exampleURL("neuroimaging")}}},
		{name: "catalogue add a name with another URL", catalogues: []testCatalogue{{name: "neuroimaging"}},
			args: []string{"catalogue", "add", "neuroimaging", mirrorURL}, wantCode: 1, wantListSame: true,
			wantStderr: "sciget: catalogue \"neuroimaging\" already exists with different settings; use --force-update to replace it, or choose another name\n",
			wantList:   []catalogue{{Name: "neuroimaging", URL: exampleURL("neuroimaging"), Use: true}}},
		{name: "catalogue add --force-update", args: []string{"catalogue", "add", "--force-update", "neuroimaging", mirrorURL},
			catalogues: []testCatalogue{{name: "mylab", apps: labCatalogue}, {name: "neuroimaging", apps: fslCatalogue}},
			wantStdout: "Catalogue \"neuroimaging\" added at the top. Run 'sciget refresh' to download it.\n",
			wantList: []catalogue{{Name: "neuroimaging", URL: mirrorURL, Use: true},
				{Name: "mylab", URL: exampleURL("mylab"), Use: true}},
			wantCaches: []string{"mylab"}},
		{name: "catalogue add --force-update with a read-only list", readOnlyList: true,
			args:       []string{"catalogue", "add", "--force-update", "neuroimaging", mirrorURL},
			catalogues: neuroimaging, wantCode: 1, wantStderrHas: "sciget: writing catalogues: ",
			wantList:   []catalogue{{Name: "neuroimaging", URL: exampleURL("neuroimaging"), Use: true}},
			wantCaches: []string{"neuroimaging"}},
		{name: "catalogue add --force-update with a locked cache", lockedCaches: true,
			args:       []string{"catalogue", "add", "--force-update", "neuroimaging", mirrorURL},
			catalogues: neuroimaging, wantCode: 1, wantStderrHas: "sciget: removing catalogue cache: ",
			wantList: []catalogue{{Name: "neuroimaging", URL: mirrorURL, Use: true}}},
		{name: "catalogue add a bad name", args: []string{"catalogue", "add", "My Lab", exampleURL("mylab")}, wantCode: 2,
			wantStderr: "sciget: invalid catalogue name \"My Lab\": must be lowercase letters, digits, \"-\" and \"_\", " +
				"starting with a letter or digit" + usageHint},
		{name: "catalogue add a bad URL", args: []string{"catalogue", "add", "mylab", "example.org/apps.json"}, wantCode: 2,
			wantStderr: "sciget: invalid catalogue URL: must start with https:// or http://" + usageHint},
		{name: "catalogue add a plain http URL", args: []string{"catalogue", "add", "mylab", "http://mirror.local/apps.json"}, wantCode: 2,
			wantStderr: "sciget: invalid catalogue URL: must use https; plain http is allowed only to this machine (localhost)" + usageHint},
		{name: "catalogue add plain http to this machine", args: []string{"catalogue", "add", "mylab", "http://localhost:8080/apps.json"},
			wantStdout: "Catalogue \"mylab\" added at the top. Run 'sciget refresh' to download it.\n",
			wantList:   []catalogue{{Name: "mylab", URL: "http://localhost:8080/apps.json", Use: true}}},
		{name: "catalogue add a URL with credentials", args: []string{"catalogue", "add", "mylab", "https://u:p@e.org/a.json"},
			wantCode: 2, wantStderr: "sciget: invalid catalogue URL: must not contain credentials" + usageHint},
		{name: "catalogue add without a URL", args: []string{"catalogue", "add", "mylab"}, wantCode: 2,
			wantStderr: "sciget: accepts 2 arg(s), received 1" + usageHint},
		{name: "catalogue add share", args: []string{"catalogue", "add", "--dir", "/share", "--layout", "{tool}/{version}/{date}", "neuroimaging", exampleURL("neuroimaging")},
			wantStdout: "Catalogue \"neuroimaging\" added at the top. Run 'sciget refresh' to download it.\n",
			wantList:   []catalogue{{Name: "neuroimaging", URL: exampleURL("neuroimaging"), Use: true, Dir: "/share", Layout: "{tool}/{version}/{date}"}}},
		{name: "catalogue relative dir", args: []string{"catalogue", "add", "--dir", "share", "a", exampleURL("a")},
			wantStdout: "Catalogue \"a\" added at the top. Run 'sciget refresh' to download it.\n",
			wantList:   []catalogue{{Name: "a", URL: exampleURL("a"), Use: true, Dir: filepath.Join(cwd, "share")}}},
		{name: "catalogue empty layout", args: []string{"catalogue", "add", "--dir", "/share", "--layout", "", "a", exampleURL("a")}, wantCode: 2,
			wantStderrHas: "sciget: invalid layout:"},

		{name: "catalogue share no-op", catalogues: []testCatalogue{{name: "neuroimaging", dir: "/share", layout: "{tool}/{version}/{date}"}},
			args: []string{"catalogue", "add", "--dir", "/share", "--layout", "{tool}/{version}/{date}", "neuroimaging", exampleURL("neuroimaging")}, wantListSame: true,
			wantStdout: "Catalogue \"neuroimaging\" already exists with the same settings; nothing to do.\n"},
		{name: "catalogue share change needs force", catalogues: []testCatalogue{{name: "neuroimaging"}},
			args: []string{"catalogue", "add", "--dir", "/share", "neuroimaging", exampleURL("neuroimaging")}, wantCode: 1, wantListSame: true,
			wantStderrHas: "use --force-update"},
		{name: "catalogue share update keeps cache", catalogues: neuroimaging,
			args:       []string{"catalogue", "add", "--force-update", "--dir", "/share", "neuroimaging", exampleURL("neuroimaging")},
			wantStdout: "Catalogue \"neuroimaging\" added at the top. Run 'sciget refresh' to download it.\n",
			wantList:   []catalogue{{Name: "neuroimaging", URL: exampleURL("neuroimaging"), Use: true, Dir: "/share"}}, wantCaches: []string{"neuroimaging"}},
		{name: "catalogue layout change needs force", catalogues: []testCatalogue{{name: "neuroimaging", dir: "/share"}},
			args: []string{"catalogue", "add", "--dir", "/share", "--layout", "{tool}/{version}/{date}", "neuroimaging", exampleURL("neuroimaging")}, wantCode: 1, wantListSame: true,
			wantStderrHas: "use --force-update"},
		{name: "catalogue layout update keeps cache", catalogues: []testCatalogue{{name: "neuroimaging", dir: "/share", apps: fslCatalogue}},
			args:       []string{"catalogue", "add", "--force-update", "--dir", "/share", "--layout", "{tool}/{version}/{date}", "neuroimaging", exampleURL("neuroimaging")},
			wantStdout: "Catalogue \"neuroimaging\" added at the top. Run 'sciget refresh' to download it.\n",
			wantList:   []catalogue{{Name: "neuroimaging", URL: exampleURL("neuroimaging"), Use: true, Dir: "/share", Layout: "{tool}/{version}/{date}"}}, wantCaches: []string{"neuroimaging"}},
		{name: "catalogue layout requires dir", args: []string{"catalogue", "add", "--layout", "{tool}/{version}/{date}", "a", exampleURL("a")}, wantCode: 2,
			wantStderr: "sciget: --layout requires --dir" + usageHint},
		{name: "catalogue bad layout", args: []string{"catalogue", "add", "--dir", "/share", "--layout", "{name}", "a", exampleURL("a")}, wantCode: 2,
			wantStderrHas: "sciget: invalid layout:"},
		{name: "catalogue empty dir", args: []string{"catalogue", "add", "--dir", "", "a", exampleURL("a")}, wantCode: 2,
			wantStderr: "sciget: --dir must not be empty" + usageHint},
		{name: "catalogue use keeps share", args: []string{"catalogue", "use", "a"}, catalogues: []testCatalogue{{name: "a", dir: "/share", layout: "{tool}/{version}/{date}", unused: true}},
			wantStdout: "Catalogue \"a\" in use at the top. Run 'sciget refresh' to update it.\n",
			wantList:   []catalogue{{Name: "a", URL: exampleURL("a"), Use: true, Dir: "/share", Layout: "{tool}/{version}/{date}"}}},
		{name: "catalogue unuse keeps share", args: []string{"catalogue", "unuse", "a"}, catalogues: []testCatalogue{{name: "a", dir: "/share", layout: "{tool}/{version}/{date}"}},
			wantStdout: "Catalogue \"a\" no longer in use.\n",
			wantList:   []catalogue{{Name: "a", URL: exampleURL("a"), Dir: "/share", Layout: "{tool}/{version}/{date}"}}},

		{name: "catalogue remove", args: []string{"catalogue", "remove", "neuroimaging"},
			catalogues: []testCatalogue{{name: "neuroimaging", apps: fslCatalogue},
				{name: "neuro", url: exampleURL("neuroimaging"), apps: fslCatalogue}, {name: "mylab", apps: labCatalogue}},
			wantStdout: "Catalogue \"neuroimaging\" removed.\n",
			wantList: []catalogue{{Name: "neuro", URL: exampleURL("neuroimaging"), Use: true},
				{Name: "mylab", URL: exampleURL("mylab"), Use: true}},
			wantCaches: []string{"mylab", "neuro"}},
		{name: "catalogue remove the last one", args: []string{"catalogue", "remove", "neuroimaging"},
			catalogues: neuroimaging, wantStdout: "Catalogue \"neuroimaging\" removed.\n",
			wantList: []catalogue{}, wantCaches: []string{}},
		{name: "catalogue remove an unknown name", args: []string{"catalogue", "remove", "x"}, wantCode: 1,
			wantStderr: "sciget: no catalogue named \"x\"\n"},
		{name: "catalogue use", args: []string{"catalogue", "use", "mylab"},
			catalogues: []testCatalogue{{name: "neuroimaging"}, {name: "mylab", unused: true}},
			wantStdout: "Catalogue \"mylab\" in use at the top. Run 'sciget refresh' to update it.\n",
			wantList: []catalogue{{Name: "mylab", URL: exampleURL("mylab"), Use: true},
				{Name: "neuroimaging", URL: exampleURL("neuroimaging"), Use: true}}},
		{name: "catalogue use --append", args: []string{"catalogue", "use", "--append", "mylab"},
			catalogues: []testCatalogue{{name: "mylab"}, {name: "neuroimaging"}},
			wantStdout: "Catalogue \"mylab\" in use at the bottom. Run 'sciget refresh' to update it.\n",
			wantList: []catalogue{{Name: "neuroimaging", URL: exampleURL("neuroimaging"), Use: true},
				{Name: "mylab", URL: exampleURL("mylab"), Use: true}}},
		{name: "catalogue use an unknown name", args: []string{"catalogue", "use", "x"}, wantCode: 1,
			wantStderr: "sciget: no catalogue named \"x\"\n"},
		{name: "catalogue unuse", args: []string{"catalogue", "unuse", "neuroimaging"},
			catalogues: []testCatalogue{{name: "neuroimaging", apps: fslCatalogue}, {name: "mylab"}},
			wantStdout: "Catalogue \"neuroimaging\" no longer in use.\n",
			wantList: []catalogue{{Name: "neuroimaging", URL: exampleURL("neuroimaging")},
				{Name: "mylab", URL: exampleURL("mylab"), Use: true}},
			wantCaches: []string{"neuroimaging"}},
		{name: "catalogue unuse an unknown name", args: []string{"catalogue", "unuse", "x"}, wantCode: 1,
			wantStderr: "sciget: no catalogue named \"x\"\n"},
		{name: "catalogue list", args: []string{"catalogue", "list"},
			catalogues: []testCatalogue{{name: "mylab"}, {name: "old", unused: true}, {name: "neuroimaging"}},
			wantStdout: "mylab\t1\t" + exampleURL("mylab") + "\t\nold\t-\t" + exampleURL("old") +
				"\t\nneuroimaging\t2\t" + exampleURL("neuroimaging") + "\t\n"},
		{name: "catalogue alone lists", args: []string{"catalogue"}, catalogues: []testCatalogue{{name: "mylab"}},
			wantStdout: "mylab\t1\t" + exampleURL("mylab") + "\t\n"},
		{name: "catalogue list with a broken list", args: []string{"catalogue", "list"}, list: "{", wantCode: 1,
			wantStderrHas: "sciget: reading catalogues "},
		{name: "catalogue use completes names", args: []string{"__complete", "catalogue", "use", "n"},
			catalogues: []testCatalogue{{name: "mylab"}, {name: "neuroimaging"}},
			wantStdout: "neuroimaging\n:4\n", wantStderrHas: "Completion ended with directive"},
		{name: "catalogue remove completes names", args: []string{"__complete", "catalogue", "remove", ""},
			catalogues: []testCatalogue{{name: "mylab"}, {name: "neuroimaging"}},
			wantStdout: "mylab\nneuroimaging\n:4\n", wantStderrHas: "Completion ended with directive"},
		{name: "catalogue unuse completes names", args: []string{"__complete", "catalogue", "unuse", "m"},
			catalogues: []testCatalogue{{name: "mylab"}, {name: "neuroimaging"}},
			wantStdout: "mylab\n:4\n", wantStderrHas: "Completion ended with directive"},
		{name: "catalogue completion with a broken list", args: []string{"__complete", "catalogue", "use", ""},
			list: "{", wantStdout: ":1\n", wantStderrHas: "Completion ended with directive: ShellCompDirectiveError"},

		{name: "config lists", args: []string{"config"}, config: `{"cache_dir": "/c"}`, env: map[string]string{"XDG_DATA_HOME": "/data"},
			wantStdout: "cache_dir\t/c\tconfig\n" + otherDefaults},
		{name: "config list", args: []string{"config", "list"}, config: `{"cache_dir": "/c"}`, env: map[string]string{"XDG_DATA_HOME": "/data"},
			wantStdout: "cache_dir\t/c\tconfig\n" + otherDefaults},
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
		{name: "config set a number", args: []string{"config", "set", "max_catalogue_size", "1024"},
			wantConfig: "{\n  \"max_catalogue_size\": 1024\n}\n"},
		{name: "config set a bool", args: []string{"config", "set", "use_catalogue_args", "false"},
			wantConfig: "{\n  \"use_catalogue_args\": false\n}\n"},
		{name: "config set a list that is not JSON", args: []string{"config", "set", "apptainer_extra_args", "x"}, wantCode: 2,
			wantStderr: "sciget: invalid apptainer_extra_args: must be a JSON array of strings" + usageHint},
		{name: "config set a bool that is not true or false", args: []string{"config", "set", "use_catalogue_args", "yes"}, wantCode: 2,
			wantStderr: "sciget: invalid use_catalogue_args: must be true or false" + usageHint},
		{name: "config set a size over the cap", args: []string{"config", "set", "max_catalogue_size", "2199023255552"}, wantCode: 2,
			wantStderr: "sciget: invalid max_catalogue_size: must be from 1 to 1099511627776 (1 TiB)" + usageHint},
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
			listFile := filepath.Join(filepath.Dir(configFile), "catalogues.json")
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
			list := tt.list
			if tt.catalogues != nil {
				var catalogues []catalogue
				for _, tc := range tt.catalogues {
					c := catalogue{Name: tc.name, URL: tc.url, Use: !tc.unused, Dir: tc.dir, Layout: tc.layout}
					switch {
					case c.URL != "":
					case base != "":
						c.URL = base + "/" + tc.name + "/apps.json"
					default:
						c.URL = exampleURL(tc.name)
					}
					catalogues = append(catalogues, c)
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
				data, err := json.Marshal(catalogues)
				if err != nil {
					t.Fatal(err)
				}
				list = string(data)
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
				locked := filepath.Join(cacheDir, "catalogues")
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
					t.Errorf("catalogues.json rewritten: %q, want %q", data, list)
				}
			}
			if tt.wantList != nil {
				catalogues, err := loadCatalogueList(listFile)
				if err != nil {
					t.Fatal(err)
				}
				if !slices.Equal(catalogues, tt.wantList) {
					t.Errorf("catalogues.json = %v, want %v", catalogues, tt.wantList)
				}
			}
			if tt.wantCaches != nil {
				entries, err := os.ReadDir(filepath.Join(cacheDir, "catalogues"))
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
