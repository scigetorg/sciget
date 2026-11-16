package main

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
)

func TestLayoutPath(t *testing.T) {
	layout, err := defaultValue("container_layout")
	if err != nil {
		t.Fatal(err)
	}
	c := container{tool: "fsl", version: "6.0.3", buildDate: "20200905"}
	tests := []struct {
		name   string
		layout string
		c      container
		want   string
		bad    bool
	}{
		{name: "default", layout: layout, c: c, want: "fsl_6.0.3_20200905/fsl_6.0.3_20200905.simg"},
		{name: "custom", layout: "{date}/{tool}/{version}", c: c, want: "20200905/fsl/6.0.3"},
		{name: "unknown placeholder", layout: "{tool}/{version}/{date}/{name}", c: c, bad: true},
		{name: "unclosed placeholder", layout: "{tool}/{version}/{date}/{", c: c, bad: true},
		{name: "missing tool", layout: "{version}/{date}", c: c, bad: true},
		{name: "missing version", layout: "{tool}/{date}", c: c, bad: true},
		{name: "missing date", layout: "{tool}/{version}", c: c, bad: true},
		{name: "absolute", layout: "/{tool}/{version}/{date}", c: c, bad: true},
		{name: "parent element", layout: "{tool}/../{version}/{date}", c: c, bad: true},
		{name: "empty element", layout: "{tool}//{version}/{date}", c: c, bad: true},
		{name: "trailing slash", layout: "{tool}/{version}/{date}/", c: c, bad: true},
		{name: "empty", c: c, bad: true},
		{name: "empty tool", layout: layout, c: container{version: "1", buildDate: "1"}, bad: true},
		{name: "empty version", layout: layout, c: container{tool: "fsl", buildDate: "1"}, bad: true},
		{name: "empty date", layout: layout, c: container{tool: "fsl", version: "1"}, bad: true},
		{name: "parent in tool", layout: layout, c: container{tool: "../x", version: "1", buildDate: "1"}, bad: true},
		{name: "slash in version", layout: layout, c: container{tool: "fsl", version: "a/b", buildDate: "1"}, bad: true},
		{name: "parent in date", layout: layout, c: container{tool: "fsl", version: "1", buildDate: ".."}, bad: true},
		{name: "embedded parent", layout: layout, c: container{tool: "fsl", version: "1..2", buildDate: "1"}, bad: true},
		{name: "NUL in date", layout: layout, c: container{tool: "fsl", version: "1", buildDate: "1\x00"}, bad: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := layoutPath(tt.layout, tt.c)
			if (err != nil) != tt.bad {
				t.Fatalf("error = %v, want error: %v", err, tt.bad)
			}
			if err != nil {
				return
			}
			if got != filepath.FromSlash(tt.want) {
				t.Errorf("path = %q, want %q", got, tt.want)
			}
		})
	}
}

// localCatalogue configures an isolated share, store and cached catalogue.
func localCatalogue(t *testing.T) (container, catalogue, string) {
	t.Helper()
	base := t.TempDir()
	t.Setenv("SCIGET_CONFIG", filepath.Join(base, "config.json"))
	store := filepath.Join(base, "store")
	if err := writeConfig(os.Getenv("SCIGET_CONFIG"), map[string]string{
		"cache_dir": filepath.Join(base, "cache"), "container_dir": store,
	}); err != nil {
		t.Fatal(err)
	}
	cat := catalogue{Name: "neuro", URL: exampleURL("neuro"), Use: true, Dir: filepath.Join(base, "share")}
	path, err := catalogueListPath()
	if err != nil {
		t.Fatal(err)
	}
	if err := writeCatalogueList(path, []catalogue{cat}); err != nil {
		t.Fatal(err)
	}
	cache := cat.dir(filepath.Join(base, "cache"))
	if err := os.MkdirAll(cache, 0o755); err != nil {
		t.Fatal(err)
	}
	const apps = `{"fsl":{"categories":["imaging"],"apps":{
		"fsl 6.0.3":{"version":"20200905","license":["MIT","Apache-2.0"],"doi":"https://doi.org/1"},
		"GUI-fsl 6.0.3":{"version":"20200905","exec":"gui","license":["MIT"],"doi":"https://doi.org/2"}}}}`
	if err := os.WriteFile(filepath.Join(cache, "apps.json"), []byte(apps), 0o600); err != nil {
		t.Fatal(err)
	}
	return container{tool: "fsl", version: "6.0.3", image: "fsl_6.0.3", buildDate: "20200905", catalogue: cat.Name}, cat, store
}

// makeContainerPath creates a regular container file or sandbox directory.
func makeContainerPath(t *testing.T, dir, layout string, c container, sandbox bool) string {
	t.Helper()
	relative, err := layoutPath(layout, c)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, relative)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if sandbox {
		err = os.Mkdir(path, 0o755)
	} else {
		err = os.WriteFile(path, nil, 0o600)
	}
	if err != nil {
		t.Fatal(err)
	}
	return path
}

func TestLocate(t *testing.T) {
	tests := []struct {
		name       string
		share      bool
		store      bool
		shareDir   bool
		sandbox    bool
		shadowed   bool
		symlink    bool
		special    bool
		brokenLink bool
		custom     bool
		badValue   bool
		noHome     bool
		wantPlace  string
		wantErr    bool
	}{
		{name: "share wins", share: true, store: true, wantPlace: "neuro"},
		{name: "sandbox", share: true, sandbox: true, wantPlace: "neuro"},
		{name: "missing share", store: true, wantPlace: "store"},
		{name: "share path missing", shareDir: true, store: true, wantPlace: "store"},
		{name: "neither"},
		{name: "shadowed share is not used", shadowed: true},
		{name: "symlink is followed", share: true, symlink: true, wantPlace: "neuro"},
		{name: "special is skipped", special: true, store: true, wantPlace: "store"},
		{name: "stat error", share: true, brokenLink: true, wantErr: true},
		{name: "share uses default despite store override", share: true, custom: true, wantPlace: "neuro"},
		{name: "custom store", store: true, custom: true, wantPlace: "store"},
		{name: "unsafe catalogue value", badValue: true, wantErr: true},
		{name: "share hit without a home directory", share: true, noHome: true, wantPlace: "neuro"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c, cat, store := localCatalogue(t)
			layout, err := defaultValue("container_layout")
			if err != nil {
				t.Fatal(err)
			}
			if tt.shareDir {
				if err := os.MkdirAll(cat.Dir, 0o755); err != nil {
					t.Fatal(err)
				}
			}
			wantPath := ""
			if tt.store {
				storeLayout, err := defaultValue("container_layout")
				if err != nil {
					t.Fatal(err)
				}
				if tt.custom {
					storeLayout = "{date}/{tool}-{version}"
					cfg, err := readConfig()
					if err != nil {
						t.Fatal(err)
					}
					if err := cfg.set("container_layout", storeLayout); err != nil {
						t.Fatal(err)
					}
				}
				wantPath = makeContainerPath(t, store, storeLayout, c, false)
			} else if tt.custom {
				cfg, err := readConfig()
				if err != nil {
					t.Fatal(err)
				}
				if err := cfg.set("container_layout", "{date}/{tool}-{version}"); err != nil {
					t.Fatal(err)
				}
			}
			if tt.share || tt.special {
				path := makeContainerPath(t, cat.Dir, layout, c, tt.sandbox)
				if tt.symlink || tt.brokenLink || tt.special {
					if err := os.Remove(path); err != nil {
						t.Fatal(err)
					}
				}
				switch {
				case tt.symlink:
					target := makeContainerPath(t, t.TempDir(), layout, c, false)
					if err := os.Symlink(target, path); err != nil {
						t.Fatal(err)
					}
				case tt.brokenLink:
					if err := os.Symlink(path, path); err != nil {
						t.Fatal(err)
					}
				case tt.special:
					if runtime.GOOS == "windows" {
						t.Skip("device symlink fixture")
					}
					if err := os.Symlink(os.DevNull, path); err != nil {
						t.Fatal(err)
					}
				}
				if tt.wantPlace == cat.Name {
					wantPath = path
				}
			}
			if tt.shadowed {
				other := catalogue{Name: "lab", URL: exampleURL("lab"), Use: true, Dir: t.TempDir()}
				path, err := catalogueListPath()
				if err != nil {
					t.Fatal(err)
				}
				if err := writeCatalogueList(path, []catalogue{cat, other}); err != nil {
					t.Fatal(err)
				}
				makeContainerPath(t, other.Dir, layout, c, false)
			}
			if tt.badValue {
				c.buildDate = "../x"
			}
			if tt.noHome {
				// Only cache_dir is set, so container_dir's default would fail.
				cache := filepath.Join(filepath.Dir(store), "cache")
				if err := writeConfig(os.Getenv("SCIGET_CONFIG"), map[string]string{"cache_dir": cache}); err != nil {
					t.Fatal(err)
				}
				t.Setenv("HOME", "")
				t.Setenv("XDG_DATA_HOME", "")
			}
			cfg, err := readConfig()
			if err != nil {
				t.Fatal(err)
			}
			got, err := locate(cfg, c)
			if tt.wantErr {
				var missing notOnMachineError
				if err == nil || errors.As(err, &missing) {
					t.Fatalf("error = %v, want a non-missing error", err)
				}
				return
			}
			if tt.wantPlace == "" {
				var missing notOnMachineError
				if !errors.As(err, &missing) || err.Error() != c.name()+" is not on this machine; to download it, run: sciget pull "+c.name()+", or let sciget download when needed: sciget config set auto_pull true" {
					t.Fatalf("error = %v, want missing container", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got.path != wantPath || got.place != tt.wantPlace {
				t.Errorf("location = %+v, want %s from %s", got, wantPath, tt.wantPlace)
			}
		})
	}
}

func TestApptainerArgs(t *testing.T) {
	base := launchSettings{
		apptainer:     "apptainer",
		global:        []string{"--silent"},
		verbArgs:      []string{"--cleanenv", "{env}", "{xauthority}", "{tmp}", "{pwd}"},
		extra:         []string{"--nv"},
		catalogue:     []string{"--bind", "/dev/fuse"},
		passEnv:       []string{"DISPLAY"},
		xauthorityVar: "XAUTHORITY",
		tmpVars:       []string{"TMP", "TMPDIR", "TEMP", "TEMPDIR"},
		tmpTarget:     "/tmp",
	}
	tests := []struct {
		name       string
		verb       string
		verbArgs   []string          // replaces base.verbArgs when set
		tmpTarget  string            // replaces base.tmpTarget when set
		env        map[string]string // set after clearing every variable the placeholders read
		xauthority string            // "file", "dir", "missing", or the name of a file to create: what XAUTHORITY names
		symlinkCwd bool              // run from a symlink to the working directory
		want       []string          // <cwd> and <xauth> stand for those paths
		wantErr    string
	}{
		{name: "exec", verb: "exec",
			want: []string{"apptainer", "--silent", "exec", "--cleanenv", "--pwd", "<cwd>", "--nv", "--bind", "/dev/fuse", "/s/fsl.simg", "bet", "--help"}},
		{name: "shell", verb: "shell",
			want: []string{"apptainer", "--silent", "shell", "--cleanenv", "--pwd", "<cwd>", "--nv", "--bind", "/dev/fuse", "/s/fsl.simg"}},
		{name: "literal verb args", verb: "shell", verbArgs: []string{"--contain"},
			want: []string{"apptainer", "--silent", "shell", "--contain", "--nv", "--bind", "/dev/fuse", "/s/fsl.simg"}},
		{name: "display passed", verb: "shell", verbArgs: []string{"{env}"}, env: map[string]string{"DISPLAY": ":0"},
			want: []string{"apptainer", "--silent", "shell", "--env", "DISPLAY=:0", "--nv", "--bind", "/dev/fuse", "/s/fsl.simg"}},
		{name: "display with a comma", verb: "shell", env: map[string]string{"DISPLAY": "a,B=b"},
			wantErr: `cannot pass DISPLAY to the container: its value contains ","`},
		{name: "display with a dollar", verb: "shell", env: map[string]string{"DISPLAY": "$(id)"},
			wantErr: `cannot pass DISPLAY to the container: its value contains "$"`},
		{name: "display with a backtick", verb: "shell", env: map[string]string{"DISPLAY": "`id`"},
			wantErr: "cannot pass DISPLAY to the container: its value contains \"`\""},
		{name: "display with a backslash", verb: "shell", env: map[string]string{"DISPLAY": `a\b`},
			wantErr: `cannot pass DISPLAY to the container: its value contains "\\"`},
		{name: "display with a double quote", verb: "shell", env: map[string]string{"DISPLAY": `a"b`},
			wantErr: `cannot pass DISPLAY to the container: its value contains "\""`},
		{name: "xauthority file", verb: "shell", verbArgs: []string{"{xauthority}"}, xauthority: "file",
			want: []string{"apptainer", "--silent", "shell", "--bind", "<xauth>:<xauth>:ro", "--env", "XAUTHORITY=<xauth>", "--nv", "--bind", "/dev/fuse", "/s/fsl.simg"}},
		{name: "xauthority missing", verb: "shell", verbArgs: []string{"{xauthority}"}, xauthority: "missing",
			want: []string{"apptainer", "--silent", "shell", "--nv", "--bind", "/dev/fuse", "/s/fsl.simg"}},
		{name: "xauthority directory", verb: "shell", verbArgs: []string{"{xauthority}"}, xauthority: "dir",
			want: []string{"apptainer", "--silent", "shell", "--nv", "--bind", "/dev/fuse", "/s/fsl.simg"}},
		{name: "xauthority with a colon", verb: "shell", verbArgs: []string{"{xauthority}"}, xauthority: "a:b",
			wantErr: `cannot bind XAUTHORITY into the container: its value contains ":"`},
		{name: "xauthority with a comma", verb: "shell", verbArgs: []string{"{xauthority}"}, xauthority: "a,b",
			wantErr: `cannot pass XAUTHORITY to the container: its value contains ","`},
		{name: "xauthority with a backslash", verb: "shell", verbArgs: []string{"{xauthority}"}, xauthority: `a\b`,
			wantErr: `cannot pass XAUTHORITY to the container: its value contains "\\"`},
		{name: "xauthority with a dollar", verb: "shell", verbArgs: []string{"{xauthority}"}, xauthority: "a$b",
			wantErr: `cannot pass XAUTHORITY to the container: its value contains "$"`},
		{name: "last tmp variable wins", verb: "shell", verbArgs: []string{"{tmp}"},
			env:  map[string]string{"TMP": "/a", "TEMPDIR": "/b"},
			want: []string{"apptainer", "--silent", "shell", "--bind", "/b:/tmp", "--nv", "--bind", "/dev/fuse", "/s/fsl.simg"}},
		{name: "tmp target", verb: "shell", verbArgs: []string{"{tmp}"}, tmpTarget: "/scratch",
			env:  map[string]string{"TMPDIR": "/a"},
			want: []string{"apptainer", "--silent", "shell", "--bind", "/a:/scratch", "--nv", "--bind", "/dev/fuse", "/s/fsl.simg"}},
		{name: "tmp with a comma", verb: "shell", env: map[string]string{"TMPDIR": "/a,b"},
			wantErr: `cannot bind TMPDIR into the container: its value contains ","`},
		{name: "tmp with a colon", verb: "shell", env: map[string]string{"TMPDIR": "/a:b"},
			wantErr: `cannot bind TMPDIR into the container: its value contains ":"`},
		{name: "tmp with a backslash", verb: "shell", env: map[string]string{"TMPDIR": `/a\b`},
			wantErr: `cannot bind TMPDIR into the container: its value contains "\\"`},
		{name: "physical working directory", verb: "shell", verbArgs: []string{"{pwd}"}, symlinkCwd: true,
			want: []string{"apptainer", "--silent", "shell", "--pwd", "<cwd>", "--nv", "--bind", "/dev/fuse", "/s/fsl.simg"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir, err := filepath.EvalSymlinks(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			cwd := filepath.Join(dir, "work")
			if err := os.Mkdir(cwd, 0o755); err != nil {
				t.Fatal(err)
			}
			t.Chdir(cwd)
			if tt.symlinkCwd {
				link := filepath.Join(dir, "link")
				if err := os.Symlink(cwd, link); err != nil {
					t.Fatal(err)
				}
				t.Chdir(link)
				t.Setenv("PWD", link) // os.Getwd prefers $PWD when it names the same directory
			}
			xauth := filepath.Join(dir, ".Xauthority")
			switch tt.xauthority {
			case "file":
				err = os.WriteFile(xauth, nil, 0o600)
			case "dir":
				err = os.Mkdir(xauth, 0o700)
			case "", "missing":
			default:
				xauth = filepath.Join(dir, tt.xauthority)
				err = os.WriteFile(xauth, nil, 0o600)
			}
			if err != nil {
				t.Fatal(err)
			}
			// After t.TempDir, which reads TMPDIR.
			for _, name := range []string{"DISPLAY", "XAUTHORITY", "TMP", "TMPDIR", "TEMP", "TEMPDIR"} {
				t.Setenv(name, "")
			}
			for name, value := range tt.env {
				t.Setenv(name, value)
			}
			if tt.xauthority != "" {
				t.Setenv("XAUTHORITY", xauth)
			}
			s := base
			if tt.verbArgs != nil {
				s.verbArgs = tt.verbArgs
			}
			if tt.tmpTarget != "" {
				s.tmpTarget = tt.tmpTarget
			}
			got, err := apptainerArgs(s, tt.verb, "/s/fsl.simg", []string{"bet", "--help"})
			if tt.wantErr != "" {
				if err == nil || err.Error() != tt.wantErr {
					t.Fatalf("error = %v, want %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			want := slices.Clone(tt.want)
			for i := range want {
				want[i] = strings.NewReplacer("<cwd>", cwd, "<xauth>", xauth).Replace(want[i])
			}
			if !slices.Equal(got, want) {
				t.Errorf("apptainerArgs() = %q\nwant %q", got, want)
			}
		})
	}
}

func TestShellJoin(t *testing.T) {
	for _, tt := range []struct {
		name string
		args []string
		want string
	}{
		{name: "bare", args: []string{"/usr/bin/apptainer", "--bind", "/a:/b,c", "x=1@%+"},
			want: "/usr/bin/apptainer --bind /a:/b,c x=1@%+"},
		{name: "space", args: []string{"echo", "a b"}, want: "echo 'a b'"},
		{name: "empty", args: []string{"echo", ""}, want: "echo ''"},
		{name: "single quote", args: []string{"echo", "it's"}, want: `echo 'it'\''s'`},
		{name: "shell characters", args: []string{"echo", "$HOME;*"}, want: "echo '$HOME;*'"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if got := shellJoin(tt.args); got != tt.want {
				t.Errorf("shellJoin() = %s, want %s", got, tt.want)
			}
		})
	}
}

func TestExecApptainer(t *testing.T) {
	for _, tt := range []struct {
		name, path string
	}{
		{name: "missing executable", path: filepath.Join(t.TempDir(), "missing")},
	} {
		t.Run(tt.name, func(t *testing.T) {
			err := execApptainer(tt.path, []string{"apptainer"}, nil)
			if !errors.Is(err, fs.ErrNotExist) || !strings.HasPrefix(err.Error(), "running apptainer: ") {
				t.Errorf("error = %v, want wrapped missing executable", err)
			}
		})
	}
}

func TestNotOnMachineError(t *testing.T) {
	tests := []struct {
		name string
		want string
	}{
		{name: "fsl/6.0.3", want: "fsl/6.0.3 is not on this machine; to download it, run: sciget pull fsl/6.0.3, or let sciget download when needed: sciget config set auto_pull true"},
		{name: "fsl/1; touch x", want: "fsl/1; touch x is not on this machine; to download it, run: sciget pull 'fsl/1; touch x', or let sciget download when needed: sciget config set auto_pull true"},
	}
	for _, tt := range tests {
		if got := (notOnMachineError{name: tt.name}).Error(); got != tt.want {
			t.Errorf("Error() = %q, want %q", got, tt.want)
		}
	}
}
