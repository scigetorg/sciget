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
	unused bool   // registered with "use": false
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

	tests := []struct {
		name          string
		args          []string
		before        [][]string        // commands run first, each of which must succeed
		config        string            // written to SCIGET_CONFIG; no file when empty
		catalogues    []testCatalogue   // written to catalogues.json, in order
		list          string            // written to catalogues.json instead, when set
		readOnlyList  bool              // write catalogues.json with mode 0o400
		lockedCaches  bool              // cache_dir/catalogues allows no deletions
		serve         map[string]string // when set, a test server serves each body at /<name>/apps.json, else 404
		readOnly      bool              // write config with mode 0o400
		env           map[string]string // set after the defaults
		wantConfig    string            // the config file afterwards, when set
		wantList      []catalogue       // catalogues.json afterwards, when set
		wantListSame  bool              // catalogues.json is byte for byte as written
		wantCaches    []string          // names of the dirs in cache_dir/catalogues afterwards, when set
		wantRequests  []string          // paths the server was asked for, when set
		wantCode      int
		wantStdout    string // exact, unless wantStdoutHas is set
		wantStdoutHas string // checks only that stdout contains this
		wantStderr    string // exact, unless wantStderrHas is set
		wantStderrHas string
	}{
		{name: "no arguments", wantCode: 2,
			wantStderr: "sciget: no command given" + usageHint},
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
			wantStdout:   "Catalogue \"neuroimaging\" already exists with the same URL; nothing to do.\n",
			wantList: []catalogue{{Name: "mylab", URL: exampleURL("mylab"), Use: true},
				{Name: "neuroimaging", URL: exampleURL("neuroimaging")}}},
		{name: "catalogue add a name with another URL", catalogues: []testCatalogue{{name: "neuroimaging"}},
			args: []string{"catalogue", "add", "neuroimaging", mirrorURL}, wantCode: 1, wantListSame: true,
			wantStderr: "sciget: catalogue \"neuroimaging\" already exists with URL " + exampleURL("neuroimaging") +
				"; use --force-update to replace it, or choose another name\n",
			wantList: []catalogue{{Name: "neuroimaging", URL: exampleURL("neuroimaging"), Use: true}}},
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
		{name: "catalogue add a URL with credentials", args: []string{"catalogue", "add", "mylab", "https://u:p@e.org/a.json"},
			wantCode: 2, wantStderr: "sciget: invalid catalogue URL: must not contain credentials" + usageHint},
		{name: "catalogue add without a URL", args: []string{"catalogue", "add", "mylab"}, wantCode: 2,
			wantStderr: "sciget: accepts 2 arg(s), received 1" + usageHint},
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
			wantStdout: "mylab\t1\t" + exampleURL("mylab") + "\nold\t-\t" + exampleURL("old") +
				"\nneuroimaging\t2\t" + exampleURL("neuroimaging") + "\n"},
		{name: "catalogue alone lists", args: []string{"catalogue"}, catalogues: []testCatalogue{{name: "mylab"}},
			wantStdout: "mylab\t1\t" + exampleURL("mylab") + "\n"},
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

		{name: "config lists", args: []string{"config"}, config: `{"cache_dir": "/c"}`,
			wantStdout: "cache_dir\t/c\tconfig\n"},
		{name: "config list", args: []string{"config", "list"}, config: `{"cache_dir": "/c"}`,
			wantStdout: "cache_dir\t/c\tconfig\n"},
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
			wantStderr: "sciget: unknown key \"cache-dir\"; valid keys: cache_dir" + usageHint},
		{name: "config get without a key", args: []string{"config", "get"}, wantCode: 2,
			wantStderr: "sciget: accepts 1 arg(s), received 0" + usageHint},
		{name: "config set", args: []string{"config", "set", "cache_dir", "/c"},
			wantConfig: "{\n  \"cache_dir\": \"/c\"\n}\n"},
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
			wantStdout: "cache_dir\n:4\n", wantStderrHas: "Completion ended with directive"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if (tt.readOnly || tt.readOnlyList || tt.lockedCaches) && os.Geteuid() == 0 {
				t.Skip("root ignores file modes")
			}
			setCacheHome(t, t.TempDir())
			cache, err := os.UserCacheDir()
			if err != nil {
				t.Fatal(err)
			}
			cacheDir := filepath.Join(cache, "sciget")
			configFile := filepath.Join(t.TempDir(), "config.json")
			listFile := filepath.Join(filepath.Dir(configFile), "catalogues.json")
			t.Setenv("SCIGET_CONFIG", configFile)
			for k, v := range tt.env {
				t.Setenv(k, v)
			}

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
					c := catalogue{Name: tc.name, URL: tc.url, Use: !tc.unused}
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
			if code != tt.wantCode {
				t.Errorf("exit code = %d, want %d", code, tt.wantCode)
			}
			got := stdout.String()
			if tt.wantStdoutHas != "" {
				if !strings.Contains(got, tt.wantStdoutHas) {
					t.Errorf("stdout = %q, want it to contain %q", got, tt.wantStdoutHas)
				}
			} else if got != tt.wantStdout {
				t.Errorf("stdout = %q, want %q", got, tt.wantStdout)
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
