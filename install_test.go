package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestInstall(t *testing.T) {
	tests := []struct {
		name       string
		args       []string
		onShare    bool   // fsl/6.0.3 is on the share, not downloaded
		modulePath string // MODULEPATH; {modules} is module_dir
		wantCode   int
		wantStdout string // {path} is the container's path, {modules} module_dir
		wantStderr string
		wantModule bool
	}{
		{name: "downloaded container", args: []string{"install", "fsl/6.0.3"},
			wantStdout: "fsl/6.0.3 is already on the machine: {path}\n" +
				"Installed fsl/6.0.3: module fsl/6.0.3, 2 commands\n" +
				"To use sciget's modules, run: module use {modules}/neuro\n", wantModule: true},
		{name: "module folder already in MODULEPATH", args: []string{"install", "fsl/6.0.3"}, modulePath: "/other:{modules}/neuro/",
			wantStdout: "fsl/6.0.3 is already on the machine: {path}\nInstalled fsl/6.0.3: module fsl/6.0.3, 2 commands\n",
			wantModule: true},
		{name: "share container gets no module", args: []string{"install", "fsl/6.0.3"}, onShare: true,
			wantStdout: "fsl/6.0.3 is already on the machine: {path}\n" +
				"No module written for fsl/6.0.3: it is on the neuro share, which provides its modules\n"},
		{name: "unknown name", args: []string{"install", "nope/1"}, wantCode: 1,
			wantStderr: "sciget: no container named \"nope/1\""},
		{name: "no name", args: []string{"install"}, wantCode: 2,
			wantStderr: "sciget: requires at least 1 arg(s), only received 0"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c, cat, store := localCatalogue(t)
			base := filepath.Dir(store)
			moduleDir := filepath.Join(base, "modules")
			apptainer, _ := fakeFinderApptainer(t, false)
			if err := writeConfig(os.Getenv("SCIGET_CONFIG"), map[string]string{
				"cache_dir": filepath.Join(base, "cache"), "container_dir": store, "module_dir": moduleDir,
				"apptainer_path": apptainer,
			}); err != nil {
				t.Fatal(err)
			}
			t.Setenv("MODULEPATH", strings.ReplaceAll(tt.modulePath, "{modules}", moduleDir))
			layout, _ := defaultValue("container_layout")
			dir := store
			if tt.onShare {
				dir = cat.Dir
			}
			path := makeContainerPath(t, dir, layout, c, false)
			var stdout, stderr bytes.Buffer
			code := run(tt.args, &stdout, &stderr)
			if code != tt.wantCode {
				t.Errorf("code = %d, want %d; stderr %q", code, tt.wantCode, stderr.String())
			}
			want := strings.NewReplacer("{path}", path, "{modules}", moduleDir).Replace(tt.wantStdout)
			if stdout.String() != want {
				t.Errorf("stdout = %q, want %q", stdout.String(), want)
			}
			if !strings.HasPrefix(stderr.String(), tt.wantStderr) {
				t.Errorf("stderr = %q, want it to start with %q", stderr.String(), tt.wantStderr)
			}
			_, err := os.Stat(filepath.Join(moduleDir, "neuro", "fsl", "6.0.3.lua"))
			if (err == nil) != tt.wantModule {
				t.Errorf("module written: %v, want %v", err == nil, tt.wantModule)
			}
		})
	}
}
