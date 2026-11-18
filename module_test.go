package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"testing"
)

func TestModuleCommands(t *testing.T) {
	tests := []struct {
		name        string
		list        commandList
		excludes    []string
		wantNames   []string
		wantSkipped []string
	}{
		{name: "excludes apply to found names only",
			list:     commandList{Found: []string{"bet", "bash", "conda"}, Kept: []string{"conda"}},
			excludes: []string{"bash", "conda"}, wantNames: []string{"bet", "conda"}},
		{name: "sorted without duplicates",
			list: commandList{Found: []string{"b", "a", "b"}, Kept: []string{"a"}}, wantNames: []string{"a", "b"}},
		{name: "unsafe names skipped",
			list:      commandList{Found: []string{"ok_1.2+x-y", "$(id)", ".hidden", "-n", "a b", "a:b", "é"}},
			wantNames: []string{"ok_1.2+x-y"}, wantSkipped: []string{"$(id)", "-n", ".hidden", "a b", "a:b", "é"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			names, skipped := moduleCommands(tt.list, tt.excludes)
			if !reflect.DeepEqual(names, tt.wantNames) || !reflect.DeepEqual(skipped, tt.wantSkipped) {
				t.Errorf("got %q, skipped %q; want %q, skipped %q", names, skipped, tt.wantNames, tt.wantSkipped)
			}
		})
	}
}

func TestLuaString(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{name: "plain", in: "fsl 6", want: `"fsl 6"`},
		{name: "quote and backslash", in: `a"b\c`, want: `"a\"b\\c"`},
		{name: "newline cannot end the line", in: "a\n\")os.exit()--", want: `"a\010\")os.exit()--"`},
		{name: "escape before a digit", in: "\x001", want: `"\0001"`},
		{name: "UTF-8 kept", in: "café", want: `"café"`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := luaString(tt.in); got != tt.want {
				t.Errorf("luaString(%q) = %s, want %s", tt.in, got, tt.want)
			}
		})
	}
}

func TestWriteModule(t *testing.T) {
	tests := []struct {
		name        string
		env         []string
		wantEnv     string // the setenv lines
		wantSkipped []string
	}{
		{name: "BASEPATH gives the path inside the container", env: []string{"FSLDIR=BASEPATH/opt/fsl", "MODE=fast"},
			wantEnv: "setenv(\"FSLDIR\", \"/opt/fsl\")\nsetenv(\"MODE\", \"fast\")\n"},
		{name: "bad name skipped", env: []string{"1X=a", "X-Y=b"},
			wantSkipped: []string{`setting "1X": not a variable name`, `setting "X-Y": not a variable name`}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c, _, store := localCatalog(t)
			base := filepath.Dir(store)
			moduleDir := filepath.Join(base, "modules")
			// The fake sciget prints each argument it gets on its own line.
			sciget := filepath.Join(base, "fake sciget")
			if err := os.WriteFile(sciget, []byte("#!/bin/sh\nprintf '%s\\n' \"$@\"\n"), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := writeConfig(os.Getenv("SCIGET_CONFIG"), map[string]string{
				"cache_dir": filepath.Join(base, "cache"), "container_dir": store,
				"module_dir": moduleDir, "module_sciget": sciget, "finder_excludes": `["bash"]`,
			}); err != nil {
				t.Fatal(err)
			}
			cfg, err := readConfig()
			if err != nil {
				t.Fatal(err)
			}
			wrappers := filepath.Join(moduleDir, ".wrappers", "neuro", "fsl", "6.0.3", "20200905")
			// A wrapper left by an earlier run, and another build's folder,
			// must go.
			olderDir := filepath.Join(filepath.Dir(wrappers), "20190101")
			for _, dir := range []string{wrappers, olderDir} {
				if err := os.MkdirAll(dir, 0o755); err != nil {
					t.Fatal(err)
				}
			}
			if err := os.WriteFile(filepath.Join(wrappers, "oldtool"), nil, 0o755); err != nil {
				t.Fatal(err)
			}

			list := commandList{Found: []string{"bet", "bash", "$(id)"}, Kept: []string{"conda"}, Env: tt.env, Help: "FSL\nhelp"}
			info, err := writeModule(cfg, c, list)
			if err != nil {
				t.Fatal(err)
			}
			if want := []string{"bet", "conda"}; !reflect.DeepEqual(info.commands, want) {
				t.Errorf("commands = %q, want %q", info.commands, want)
			}
			wantSkipped := append([]string{`command "$(id)": not a safe command name`}, tt.wantSkipped...)
			if !reflect.DeepEqual(info.skipped, wantSkipped) {
				t.Errorf("skipped = %q, want %q", info.skipped, wantSkipped)
			}

			got, err := os.ReadFile(filepath.Join(moduleDir, "neuro", "fsl", "6.0.3.lua"))
			if err != nil {
				t.Fatal(err)
			}
			want := "-- Written by sciget install, which rewrites it.\n" +
				"help(\"FSL\\010help\")\n" +
				"whatis(\"Container: fsl_6.0.3_20200905\")\n" +
				"whatis(\"Commands: bet conda\")\n" +
				"prepend_path(\"PATH\", " + luaString(wrappers) + ")\n" +
				tt.wantEnv
			if string(got) != want {
				t.Errorf("modulefile:\n%s\nwant:\n%s", got, want)
			}

			if _, err := os.Stat(olderDir); err == nil {
				t.Error("another build's wrapper folder was left")
			}
			entries, err := os.ReadDir(wrappers)
			if err != nil {
				t.Fatal(err)
			}
			var files []string
			for _, e := range entries {
				files = append(files, e.Name())
			}
			if want := []string{"bet", "conda"}; !reflect.DeepEqual(files, want) {
				t.Errorf("wrappers = %q, want %q", files, want)
			}
			// The wrapper passes its arguments through untouched.
			out, err := exec.Command(filepath.Join(wrappers, "bet"), "a b", "$(id)", "").Output()
			if err != nil {
				t.Fatal(err)
			}
			if want := "apptainer\nexec\n--\nfsl/6.0.3\nbet\na b\n$(id)\n\n"; string(out) != want {
				t.Errorf("wrapper ran sciget with %q, want %q", out, want)
			}
		})
	}
}

func TestOutdatedModules(t *testing.T) {
	tests := []struct {
		name      string
		buildDate string // the module's build; no module when empty
		want      []string
	}{
		{name: "no module"},
		{name: "module of the catalog's build", buildDate: "20200905"},
		{name: "module of another build", buildDate: "20190101", want: []string{"fsl/6.0.3"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c, _, store := localCatalog(t)
			base := filepath.Dir(store)
			if err := writeConfig(os.Getenv("SCIGET_CONFIG"), map[string]string{
				"cache_dir": filepath.Join(base, "cache"), "container_dir": store, "module_dir": filepath.Join(base, "modules"),
			}); err != nil {
				t.Fatal(err)
			}
			cfg, err := readConfig()
			if err != nil {
				t.Fatal(err)
			}
			if tt.buildDate != "" {
				c.buildDate = tt.buildDate
				if _, err := writeModule(cfg, c, commandList{Found: []string{"bet"}}); err != nil {
					t.Fatal(err)
				}
			}
			got, err := outdatedModules(cfg)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("outdatedModules = %q, want %q", got, tt.want)
			}
		})
	}
}
