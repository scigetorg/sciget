package main

import (
	"bytes"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"
)

const testEESSIJSON = `{
  "schemaVersion": 1,
  "backend": "eessi",
  "explicitSelectionRequired": true,
  "cvmfsRepo": "software.eessi.io",
  "stacks": ["2025.06"],
  "tools": {
    "paraview": {
      "package": "ParaView",
      "categories": ["visualization"],
      "description": "3D visualization",
      "homepage": "https://www.paraview.org",
      "apps": {
        "ParaView 6.0.1 (EESSI/2025.06)": {
          "backend": "eessi",
          "stack": "2025.06",
          "module": "ParaView/6.0.1-foss-2025b",
          "software_version": "6.0.1",
          "toolchain": "foss/2025b",
          "arch": ["aarch64", "x86_64"],
          "gpu": false,
          "license": ["BSD-3-Clause"],
          "exec": ["paraview"],
          "gui": true,
          "show_in_menu": true
        }
      }
    },
    "gromacs": {
      "package": "GROMACS",
      "categories": ["molecular dynamics"],
      "apps": {
        "GROMACS 2026.2 (EESSI/2025.06)": {
          "backend": "eessi",
          "stack": "2025.06",
          "module": "GROMACS/2026.2-foss-2025b",
          "software_version": "2026.2",
          "arch": ["aarch64", "x86_64"],
          "gpu": false,
          "exec": ["gmx"],
          "gui": false
        },
        "GROMACS 2026.2 GPU (EESSI/2025.06)": {
          "backend": "eessi",
          "stack": "2025.06",
          "module": "GROMACS/2026.2-foss-2025b-CUDA-12.9.1",
          "software_version": "2026.2",
          "arch": ["x86_64"],
          "gpu": true,
          "gpu_targets": ["nvidia/cc80"],
          "exec": ["gmx"],
          "gui": false
        }
      }
    },
    "biopython": {
      "package": "Biopython",
      "categories": ["programming"],
      "apps": {
        "Biopython 1.86 (EESSI/2025.06)": {
          "backend": "eessi",
          "stack": "2025.06",
          "module": "Biopython/1.86-gfbf-2025b",
          "software_version": "1.86",
          "arch": ["aarch64", "x86_64"],
          "gpu": false,
          "exec": [],
          "gui": false
        }
      }
    }
  }
}`

func TestParseEESSICatalog(t *testing.T) {
	builds, err := parseEESSICatalog([]byte(testEESSIJSON), []string{"2025.06"})
	if err != nil {
		t.Fatalf("parseEESSICatalog failed: %v", err)
	}
	if len(builds) != 4 {
		t.Fatalf("got %d builds, want 4", len(builds))
	}
	// GROMACS CPU sorts before GPU.
	var gromacs []eessiBuild
	for _, b := range builds {
		if b.id == "gromacs" {
			gromacs = append(gromacs, b)
		}
	}
	if len(gromacs) != 2 || gromacs[0].gpu || !gromacs[1].gpu {
		t.Fatalf("unexpected GROMACS ordering: %+v", gromacs)
	}

	// Rejects stack outside domain eessi_stacks.
	if _, err := parseEESSICatalog([]byte(testEESSIJSON), []string{"2023.06"}); err == nil {
		t.Errorf("expected error for stack outside domain eessi_stacks")
	}

	badCases := []struct {
		name string
		json string
	}{
		{
			name: "missing explicitSelectionRequired",
			json: strings.Replace(testEESSIJSON, `"explicitSelectionRequired": true`, `"explicitSelectionRequired": false`, 1),
		},
		{
			name: "unsafe exec arg",
			json: strings.Replace(testEESSIJSON, `"exec": ["paraview"]`, `"exec": ["paraview;id"]`, 1),
		},
		{
			name: "option-shaped executable",
			json: strings.Replace(testEESSIJSON, `"exec": ["paraview"]`, `"exec": ["-a", "paraview"]`, 1),
		},
		{
			name: "path-qualified executable",
			json: strings.Replace(testEESSIJSON, `"exec": ["paraview"]`, `"exec": ["/usr/bin/paraview"]`, 1),
		},
		{
			name: "dot executable",
			json: strings.Replace(testEESSIJSON, `"exec": ["paraview"]`, `"exec": ["."]`, 1),
		},
		{
			name: "invalid stack",
			json: strings.Replace(testEESSIJSON, `"stack": "2025.06"`, `"stack": "bad"`, 1),
		},
		{
			name: "undeclared feed stack",
			json: strings.Replace(testEESSIJSON, `"stack": "2025.06"`, `"stack": "2023.06"`, 1),
		},
		{
			name: "invalid cvmfsRepo with traversal",
			json: strings.Replace(testEESSIJSON, `"software.eessi.io"`, `"bad..eessi.io"`, 1),
		},
		{
			name: "invalid module",
			json: strings.Replace(testEESSIJSON, `"ParaView/6.0.1-foss-2025b"`, `"ParaView"`, 1),
		},
		{
			name: "menu launcher with empty exec",
			json: strings.Replace(testEESSIJSON, `"exec": ["paraview"]`, `"exec": []`, 1),
		},
		{
			name: "blank app label",
			json: strings.Replace(testEESSIJSON, `"ParaView 6.0.1 (EESSI/2025.06)"`, `"   "`, 1),
		},
		{
			name: "escape sequence in description",
			json: strings.Replace(testEESSIJSON, `"3D visualization"`, `"3D \u001b[31mred"`, 1),
		},
		{
			name: "bidi override in app label",
			json: strings.Replace(testEESSIJSON, `"ParaView 6.0.1 (EESSI/2025.06)"`, `"ParaView \u202e6.0.1"`, 1),
		},
		{
			name: "null apps object",
			json: `{"schemaVersion":1,"backend":"eessi","explicitSelectionRequired":true,"cvmfsRepo":"software.eessi.io","stacks":["2025.06"],"tools":{"paraview":{"package":"ParaView","apps":null}}}`,
		},
		{
			name: "empty apps object",
			json: `{"schemaVersion":1,"backend":"eessi","explicitSelectionRequired":true,"cvmfsRepo":"software.eessi.io","stacks":["2025.06"],"tools":{"paraview":{"package":"ParaView","apps":{}}}}`,
		},
	}
	for _, tc := range badCases {
		if _, err := parseEESSICatalog([]byte(tc.json), nil); err == nil {
			t.Errorf("%s: expected error, got nil", tc.name)
		}
	}
}

func isolateTestEnv(t *testing.T, tmp string) {
	t.Helper()
	t.Setenv("SCIGET_CONFIG", "")
	t.Setenv("SCIGET_SYSTEM_DIR", filepath.Join(tmp, "no-system"))
	setCacheHome(t, filepath.Join(tmp, "cache"))
	setConfigHome(t, filepath.Join(tmp, "config"))
	t.Setenv("XDG_DATA_HOME", filepath.Join(tmp, "data"))
}

func TestRefreshAndEESSICommands(t *testing.T) {
	tmp := t.TempDir()
	isolateTestEnv(t, tmp)

	srcDir := filepath.Join(tmp, "src")
	if err := os.MkdirAll(srcDir, 0o755); err != nil {
		t.Fatal(err)
	}
	domainsFile := filepath.Join(srcDir, "domains.json")
	eessiFile := filepath.Join(srcDir, "preclinical-eessi.json")
	emptyAppsFile := filepath.Join(srcDir, "preclinical-apps.json")
	if err := os.WriteFile(eessiFile, []byte(testEESSIJSON), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(emptyAppsFile, []byte(`{}`), 0o644); err != nil {
		t.Fatal(err)
	}
	// EESSI-only domain with both empty {} apps.json and sibling eessi.json,
	// plus an EESSI-only domain omitting appJsonUrl.
	domainsJSON := `{
		"schemaVersion": 1,
		"domains": {
			"preclinical": {"appJsonUrl": "preclinical-apps.json", "eessiCatalogUrl": "preclinical-eessi.json", "eessi_stacks": ["2025.06"]},
			"molecular": {"eessiCatalogUrl": "preclinical-eessi.json", "eessi_stacks": ["2025.06"]}
		}
	}`
	if err := os.WriteFile(domainsFile, []byte(domainsJSON), 0o644); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr bytes.Buffer
	if code := run([]string{"catalog", "add", "sciget", domainsFile}, &stdout, &stderr); code != 0 {
		t.Fatalf("catalog add failed (%d): %s", code, stderr.String())
	}
	if strings.Contains(stdout.String(), "no apps list yet") {
		t.Fatalf("EESSI-only domains should not be reported empty on catalog add: %s", stdout.String())
	}
	stdout.Reset()
	stderr.Reset()
	if code := run([]string{"refresh"}, &stdout, &stderr); code != 0 {
		t.Fatalf("refresh failed (%d): %s", code, stderr.String())
	}
	if strings.Contains(stdout.String(), "no apps list yet") {
		t.Fatalf("EESSI-only domains should not report empty catalog: %s", stdout.String())
	}

	// Default container find stays empty (EESSI requires explicit selection).
	stdout.Reset()
	stderr.Reset()
	if code := run([]string{"find", "paraview"}, &stdout, &stderr); code != 0 {
		t.Fatalf("find failed (%d): %s", code, stderr.String())
	}
	if stdout.Len() != 0 {
		t.Fatalf("default container find should not include EESSI tools: %s", stdout.String())
	}

	// Explicit sciget eessi find returns curated tools.
	stdout.Reset()
	stderr.Reset()
	if code := run([]string{"eessi", "find", "gromacs", "--gpu"}, &stdout, &stderr); code != 0 {
		t.Fatalf("eessi find failed (%d): %s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "GROMACS/2026.2-foss-2025b-CUDA-12.9.1") {
		t.Fatalf("unexpected eessi find output: %s", stdout.String())
	}

	// sciget eessi info shows explicit non-DOI notice and load/launch commands.
	stdout.Reset()
	stderr.Reset()
	if code := run([]string{"eessi", "info", "paraview"}, &stdout, &stderr); code != 0 {
		t.Fatalf("eessi info failed (%d): %s", code, stderr.String())
	}
	for _, want := range []string{
		"software.eessi.io",
		"ParaView/6.0.1-foss-2025b",
		"(none — EESSI CVMFS module)",
		"module use /cvmfs/software.eessi.io/init/modules && module load EESSI/2025.06 && module load ParaView/6.0.1-foss-2025b",
		"sciget eessi launch --stack 2025.06 ParaView/6.0.1-foss-2025b",
	} {
		if !strings.Contains(stdout.String(), want) {
			t.Fatalf("eessi info missing %q:\n%s", want, stdout.String())
		}
	}

	// Library-only tool gives a clear module use + load hint instead of failing obscurely.
	stdout.Reset()
	stderr.Reset()
	if code := run([]string{"eessi", "launch", "biopython"}, &stdout, &stderr); code != 1 {
		t.Fatalf("expected exit 1 for library-only tool, got %d", code)
	}
	if !strings.Contains(stderr.String(), "module use /cvmfs/software.eessi.io/init/modules && module load EESSI/2025.06 && module load Biopython/1.86-gfbf-2025b") {
		t.Fatalf("missing module load hint in stderr: %s", stderr.String())
	}

	// Strict EESSI launch via real bash + mock Lmod: loads exact stack & module,
	// passes tool flags without requiring `--`, and FAILS instead of falling
	// back when an explicitly selected exact module is missing.
	t.Run("launch", func(t *testing.T) {
		if runtime.GOOS == "windows" {
			t.Skip("EESSI Lmod launcher requires a Unix shell")
		}
		var stdout, stderr bytes.Buffer
		repoRoot := filepath.Join(tmp, "cvmfs-eessi")
		modRoot := filepath.Join(repoRoot, "init", "modules")
		cpuModDir := filepath.Join(repoRoot, "versions", "2025.06", "software", "linux", "x86_64", "amd", "zen2", "modules", "all")
		accelModDir := filepath.Join(repoRoot, "versions", "2025.06", "software", "linux", "x86_64", "amd", "zen2", "accel", "nvidia", "cc80", "modules", "all")
		hostInjectDir := filepath.Join(repoRoot, "host_injections", "2025.06", "software", "linux", "x86_64", "amd", "zen2", "modules", "all")
		binDir := filepath.Join(repoRoot, "versions", "2025.06", "software", "linux", "x86_64", "amd", "zen2", "software", "bin")
		accelBinDir := filepath.Join(repoRoot, "versions", "2025.06", "software", "linux", "x86_64", "amd", "zen2", "accel", "nvidia", "cc80", "software", "bin")
		siteModDir := filepath.Join(tmp, "site-eessi-modules")
		siteHookBinDir := filepath.Join(tmp, "site-hook-bin")
		for _, d := range []string{modRoot, cpuModDir, accelModDir, hostInjectDir, binDir, accelBinDir, siteModDir, siteHookBinDir} {
			if err := os.MkdirAll(d, 0o755); err != nil {
				t.Fatal(err)
			}
		}
		for _, mf := range []string{
			filepath.Join(modRoot, "EESSI", "2025.06.lua"),
			filepath.Join(cpuModDir, "ParaView", "6.0.1-foss-2025b.lua"),
			filepath.Join(cpuModDir, "HostCheck", "1.0-foss-2025b.lua"),
			filepath.Join(cpuModDir, "GROMACS", "2026.2-foss-2025b.lua"),
			filepath.Join(cpuModDir, "GROMACS", "2026.2-foss-2025b-CUDA-12.9.1.lua"),
			filepath.Join(accelModDir, "GROMACS", "2026.2-foss-2025b-CUDA-12.9.1.lua"),
			filepath.Join(siteModDir, "ParaView", "6.0.1-foss-2025b.lua"),
		} {
			if err := os.MkdirAll(filepath.Dir(mf), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(mf, []byte("-- mock modulefile\n"), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		accelSymlinkToCPU := filepath.Join(accelModDir, "GROMACS", "2026.2-symlink-to-cpu.lua")
		if err := os.Symlink(filepath.Join(cpuModDir, "GROMACS", "2026.2-foss-2025b.lua"), accelSymlinkToCPU); err != nil {
			t.Fatal(err)
		}
		mockLmod := filepath.Join(tmp, "mock-lmod.sh")
		lmodLog := filepath.Join(tmp, "lmod.log")
		if err := os.WriteFile(mockLmod, []byte(`module() {
    if [ "$1" = "--force" ] && [ "$2" = "purge" ]; then
        LOADEDMODULES=""
        _LMFILES_=""
        return 0
    fi
    echo "LOAD:$2|MP:${MODULEPATH}|EXACT:${LMOD_EXACT_MATCH:-}" >> "${MOCK_LMOD_LOG}"
    if [ "$2" = "${MOCK_LMOD_FAIL:-}" ]; then
        echo "Lmod error: module not found: $2" >&2
        return 1
    fi
    if [ "$2" = "${MOCK_LMOD_ALIAS_STACK:-}" ]; then
        export LOADEDMODULES="${LOADEDMODULES:+${LOADEDMODULES}:}EESSI/2023.06"
        export _LMFILES_="${_LMFILES_:+${_LMFILES_}:}${MOCK_MOD_ROOT}/EESSI/2023.06.lua"
        return 0
    fi
    if [ "${MOCK_LMOD_EMPTY_LOADED:-}" = "1" ]; then
        export LOADEDMODULES=""
        export _LMFILES_=""
        return 0
    fi
    if [ "$2" = "${MOCK_LMOD_PREFIX_TARGET:-}" ]; then
        export LOADEDMODULES="${LOADEDMODULES:+${LOADEDMODULES}:}${2}-extra"
        export _LMFILES_="${_LMFILES_:+${_LMFILES_}:}${MOCK_CPU_MOD_DIR}/${2}-extra.lua"
        return 0
    fi
    export LOADEDMODULES="${LOADEDMODULES:+${LOADEDMODULES}:}$2"
    if [ "$2" = "EESSI/2025.06" ]; then
        export _LMFILES_="${_LMFILES_:+${_LMFILES_}:}${MOCK_MOD_ROOT}/EESSI/2025.06.lua"
        # Simulate real EESSI bootstrap prepending site modules, host_injections,
        # optional accelerator modules, and CPU software modules.
        if [ "${MOCK_EESSI_ACCEL:-1}" = "1" ]; then
            export MODULEPATH="${MOCK_SITE_MOD_DIR}:${MOCK_HOST_INJECT_DIR}:${MOCK_ACCEL_MOD_DIR}:${MOCK_CPU_MOD_DIR}:${MODULEPATH}"
        else
            export MODULEPATH="${MOCK_SITE_MOD_DIR}:${MOCK_HOST_INJECT_DIR}:${MOCK_CPU_MOD_DIR}:${MODULEPATH}"
        fi
        return 0
    fi
    if [ -n "${MOCK_LMOD_OVERRIDE_LMFILE:-}" ]; then
        export _LMFILES_="${_LMFILES_:+${_LMFILES_}:}${MOCK_LMOD_OVERRIDE_LMFILE}"
    elif [ "$2" = "GROMACS/2026.2-foss-2025b-CUDA-12.9.1" ]; then
        export _LMFILES_="${_LMFILES_:+${_LMFILES_}:}${MOCK_ACCEL_MOD_DIR}/$2.lua"
    else
        export _LMFILES_="${_LMFILES_:+${_LMFILES_}:}${MOCK_CPU_MOD_DIR}/$2.lua"
    fi
    if [ "$2" = "ParaView/6.0.1-foss-2025b" ] || [ "$2" = "HostCheck/1.0-foss-2025b" ] || [ "$2" = "GROMACS/2026.2-foss-2025b-CUDA-12.9.1" ]; then
        if [ -n "${MOCK_EESSI_BIN:-}" ]; then
            export PATH="${MOCK_EESSI_BIN}:${PATH}"
        fi
    fi
    if [ "$2" = "GROMACS/2026.2-foss-2025b-CUDA-12.9.1" ] && [ -n "${MOCK_EESSI_ACCEL_BIN:-}" ]; then
        export PATH="${MOCK_EESSI_ACCEL_BIN}:${PATH}"
    fi
    if [ "$2" = "ParaView/6.0.1-foss-2025b" ] && [ -n "${MOCK_SITE_ALIAS_BIN:-}" ]; then
        export PATH="${MOCK_SITE_ALIAS_BIN}:${PATH}"
    fi
    if [ "$2" = "ParaView/6.0.1-foss-2025b" ] && [ -n "${MOCK_SITE_MOD_ALIAS:-}" ]; then
        export MODULEPATH="${MOCK_SITE_MOD_ALIAS}:${MOCK_SITE_MOD_DIR}:${MODULEPATH}"
    fi
    # Simulate a host_injections site load hook prepending a site bin dir to PATH.
    if [ -n "${MOCK_SITE_HOOK_BIN:-}" ]; then
        export PATH="${MOCK_SITE_HOOK_BIN}:${PATH}"
    fi
    return 0
}
`), 0o644); err != nil {
			t.Fatal(err)
		}
		staleBinDir := filepath.Join(tmp, "stale-bin")
		if err := os.MkdirAll(staleBinDir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(staleBinDir, "paraview"), []byte("#!/bin/sh\necho STALE_SITE_BINARY\n"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(staleBinDir, "gmx"), []byte("#!/bin/sh\necho STALE_SITE_GMX\n"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(siteHookBinDir, "paraview"), []byte("#!/bin/sh\necho SITE_HOOK_HIJACK\n"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(siteHookBinDir, "gmx"), []byte("#!/bin/sh\necho SITE_HOOK_GMX\n"), 0o755); err != nil {
			t.Fatal(err)
		}
		// A site hook supplying a forged `readlink` on PATH must never influence
		// modulefile or executable canonicalization.
		if err := os.WriteFile(filepath.Join(siteHookBinDir, "readlink"), []byte("#!/bin/sh\necho \""+filepath.Join(accelModDir, "forged.lua")+"\"\n"), 0o755); err != nil {
			t.Fatal(err)
		}
		siteAliasBin := filepath.Join(tmp, "site-alias-bin")
		if err := os.Symlink(binDir, siteAliasBin); err != nil {
			t.Fatal(err)
		}
		siteModAlias := filepath.Join(tmp, "site-mod-alias")
		if err := os.Symlink(cpuModDir, siteModAlias); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(binDir, "paraview"), []byte("#!/bin/sh\necho \"PARAVIEW:$*|LD:${LD_LIBRARY_PATH:-}|PATH:${PATH}|MP:${MODULEPATH}\"\n"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(binDir, "gmx"), []byte("#!/bin/sh\necho \"CPU_GMX:$*\"\n"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(accelBinDir, "gmx"), []byte("#!/bin/sh\necho \"GMX_GPU:$*\"\n"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(filepath.Join(siteHookBinDir, "paraview"), filepath.Join(binDir, "escaped")); err != nil {
			t.Fatal(err)
		}
		t.Setenv("MODULEPATH", "/site/other/modules")
		t.Setenv("LD_LIBRARY_PATH", "/site/stale/lib")
		t.Setenv("EESSI_MODULE_ROOT", modRoot)
		t.Setenv("EESSI_LMOD_INIT", mockLmod)
		t.Setenv("MOCK_MOD_ROOT", modRoot)
		t.Setenv("MOCK_CPU_MOD_DIR", cpuModDir)
		t.Setenv("MOCK_ACCEL_MOD_DIR", accelModDir)
		t.Setenv("MOCK_HOST_INJECT_DIR", hostInjectDir)
		t.Setenv("MOCK_SITE_MOD_DIR", siteModDir)
		t.Setenv("MOCK_SITE_MOD_ALIAS", siteModAlias)
		t.Setenv("MOCK_SITE_HOOK_BIN", siteHookBinDir)
		t.Setenv("MOCK_SITE_ALIAS_BIN", siteAliasBin)
		t.Setenv("MOCK_LMOD_LOG", lmodLog)
		t.Setenv("MOCK_EESSI_BIN", binDir)
		t.Setenv("MOCK_EESSI_ACCEL_BIN", accelBinDir)
		t.Setenv("PATH", staleBinDir+string(os.PathListSeparator)+os.Getenv("PATH"))

		// Tool flags (--version, --gpu) after <tool> pass through to the binary without `--`,
		// inherited MODULEPATH is replaced by modroot, site/host_injections/accel paths
		// added by EESSI bootstrap or tool load are stripped/canonicalized, site load
		// hook PATH entries (including forged readlink and site symlink aliases) are
		// neutralized, stale LD_LIBRARY_PATH and PATH entries are scrubbed, and
		// LMOD_EXACT_MATCH=yes is enforced.
		stdout.Reset()
		stderr.Reset()
		if code := run([]string{"eessi", "launch", "--stack", "2025.06", "paraview", "--version", "--gpu"}, &stdout, &stderr); code != 0 {
			t.Fatalf("eessi launch failed (%d): %s", code, stderr.String())
		}
		gotLaunch := strings.TrimSpace(stdout.String())
		if !strings.HasPrefix(gotLaunch, "PARAVIEW:--version --gpu|LD:|PATH:") {
			t.Fatalf("got %q, want prefix %q", gotLaunch, "PARAVIEW:--version --gpu|LD:|PATH:")
		}
		if strings.Contains(gotLaunch, siteAliasBin) || strings.Contains(gotLaunch, siteHookBinDir) || strings.Contains(gotLaunch, siteModAlias) || strings.Contains(gotLaunch, siteModDir) {
			t.Fatalf("final PATH/MODULEPATH must not retain site dirs or aliases: %q", gotLaunch)
		}
		realModRoot, err := filepath.EvalSymlinks(modRoot)
		if err != nil {
			t.Fatal(err)
		}
		realCPUModDir, err := filepath.EvalSymlinks(cpuModDir)
		if err != nil {
			t.Fatal(err)
		}
		realAccelModDir, err := filepath.EvalSymlinks(accelModDir)
		if err != nil {
			t.Fatal(err)
		}
		logBytes, err := os.ReadFile(lmodLog)
		if err != nil {
			t.Fatal(err)
		}
		wantLog := "LOAD:EESSI/2025.06|MP:" + realModRoot + "|EXACT:yes\nLOAD:ParaView/6.0.1-foss-2025b|MP:" + realCPUModDir + ":" + realModRoot + "|EXACT:yes\n"
		if string(logBytes) != wantLog {
			t.Fatalf("lmod log = %q, want %q", string(logBytes), wantLog)
		}

		// An EESSI-side symlink executable pointing outside EESSI must be refused.
		escapedBuild := eessiBuild{
			id:        "escaped",
			pkg:       "HostCheck",
			cvmfsRepo: "software.eessi.io",
			stack:     "2025.06",
			module:    "HostCheck/1.0-foss-2025b",
			version:   "1.0",
			exec:      []string{"escaped"},
		}
		stdout.Reset()
		stderr.Reset()
		if err := runEESSILaunch(escapedBuild, nil, strings.NewReader(""), &stdout, &stderr); err == nil || stdout.Len() != 0 || !strings.Contains(stderr.String(), "resolved outside EESSI root") {
			t.Fatalf("symlink escaping EESSI root must be refused: err=%v out=%q errOut=%q", err, stdout.String(), stderr.String())
		}

		// If a same-named site modulefile still appears in _LMFILES_, launch aborts.
		t.Setenv("MOCK_LMOD_OVERRIDE_LMFILE", filepath.Join(siteModDir, "ParaView", "6.0.1-foss-2025b.lua"))
		stdout.Reset()
		stderr.Reset()
		if code := run([]string{"eessi", "launch", "--stack", "2025.06", "paraview", "--version"}, &stdout, &stderr); code == 0 || stdout.Len() != 0 || !strings.Contains(stderr.String(), "outside EESSI root") {
			t.Fatalf("site modulefile must be rejected: code=%d out=%q err=%q", code, stdout.String(), stderr.String())
		}
		t.Setenv("MOCK_LMOD_OVERRIDE_LMFILE", "")

		// GPU build requires an active EESSI accelerator modulepath; if unavailable,
		// launch fails BEFORE loading the tool module instead of falling back to CPU.
		t.Setenv("MOCK_EESSI_ACCEL", "0")
		stdout.Reset()
		stderr.Reset()
		if code := run([]string{"eessi", "launch", "--gpu", "gromacs", "--version"}, &stdout, &stderr); code == 0 || stdout.Len() != 0 || !strings.Contains(stderr.String(), "accelerator modulepath") {
			t.Fatalf("--gpu without accelerator modulepath must fail: code=%d out=%q err=%q", code, stdout.String(), stderr.String())
		}
		t.Setenv("MOCK_EESSI_ACCEL", "1")

		// Even when an accelerator modulepath is active, if Lmod resolves a --gpu
		// build to a CPU modulefile (directly or via an accel-side symlink to CPU),
		// launch aborts.
		for _, badLMFile := range []string{
			filepath.Join(cpuModDir, "GROMACS", "2026.2-foss-2025b-CUDA-12.9.1.lua"),
			accelSymlinkToCPU,
		} {
			t.Setenv("MOCK_LMOD_OVERRIDE_LMFILE", badLMFile)
			stdout.Reset()
			stderr.Reset()
			if code := run([]string{"eessi", "launch", "--gpu", "gromacs", "--version"}, &stdout, &stderr); code == 0 || stdout.Len() != 0 || !strings.Contains(stderr.String(), "--gpu requested accelerator module") {
				t.Fatalf("--gpu resolving to CPU modulefile (%s) must be rejected: code=%d out=%q err=%q", badLMFile, code, stdout.String(), stderr.String())
			}
		}
		t.Setenv("MOCK_LMOD_OVERRIDE_LMFILE", "")

		// Even with an accelerator modulefile loaded, --gpu must refuse to fall back
		// to a CPU dependency executable when no accelerator executable is present.
		t.Setenv("MOCK_EESSI_ACCEL_BIN", "")
		stdout.Reset()
		stderr.Reset()
		if code := run([]string{"eessi", "launch", "--gpu", "gromacs", "--version"}, &stdout, &stderr); code == 0 || strings.Contains(stdout.String(), "CPU_GMX") {
			t.Fatalf("--gpu must not launch CPU dependency executable: code=%d out=%q err=%q", code, stdout.String(), stderr.String())
		}
		t.Setenv("MOCK_EESSI_ACCEL_BIN", accelBinDir)

		// With an active accelerator modulepath, accel modulefile, and accel bin, --gpu succeeds.
		stdout.Reset()
		stderr.Reset()
		if code := run([]string{"eessi", "launch", "--gpu", "gromacs", "--version"}, &stdout, &stderr); code != 0 || strings.TrimSpace(stdout.String()) != "GMX_GPU:--version" {
			t.Fatalf("--gpu launch with accel module failed: code=%d out=%q err=%q", code, stdout.String(), stderr.String())
		}

		// If the EESSI module does not provide the binary on PATH, neither stale
		// site PATH entries nor host /usr/bin:/bin binaries may satisfy the exec.
		stdout.Reset()
		stderr.Reset()
		if code := run([]string{"eessi", "launch", "--stack", "2025.06", "gromacs", "--version"}, &stdout, &stderr); code == 0 || strings.Contains(stdout.String(), "STALE_SITE_GMX") {
			t.Fatalf("stale site PATH must not satisfy missing EESSI binary: code=%d out=%q err=%q", code, stdout.String(), stderr.String())
		}

		// Even when the loaded EESSI module adds an EESSI bin directory to PATH,
		// a binary that exists only in host /bin or /usr/bin (e.g. "uname") must be
		// refused with exit 127 by the filtered type -P lookup.
		hostOnlyBuild := eessiBuild{
			id:        "hostcheck",
			pkg:       "HostCheck",
			cvmfsRepo: "software.eessi.io",
			stack:     "2025.06",
			module:    "HostCheck/1.0-foss-2025b",
			version:   "1.0",
			exec:      []string{"uname"},
		}
		stdout.Reset()
		stderr.Reset()
		err = runEESSILaunch(hostOnlyBuild, nil, strings.NewReader(""), &stdout, &stderr)
		var exitErr *exec.ExitError
		if !errors.As(err, &exitErr) || exitErr.ExitCode() != 127 || stdout.Len() != 0 || !strings.Contains(stderr.String(), "refusing host fallback") {
			t.Fatalf("host /usr/bin/uname must be refused with exit 127 and fallback diagnostic: err=%v out=%q errOut=%q", err, stdout.String(), stderr.String())
		}

		// If Lmod still resolves a prefix-matched module despite LMOD_EXACT_MATCH,
		// post-load LOADEDMODULES verification aborts instead of executing.
		if err := os.WriteFile(lmodLog, nil, 0o644); err != nil {
			t.Fatal(err)
		}
		t.Setenv("MOCK_LMOD_PREFIX_TARGET", "ParaView/6.0.1-foss-2025b")
		stdout.Reset()
		stderr.Reset()
		if code := run([]string{"eessi", "launch", "--stack", "2025.06", "paraview", "--version"}, &stdout, &stderr); code == 0 {
			t.Fatalf("expected failure when Lmod loads prefix-matched module; stdout=%q", stdout.String())
		}
		if stdout.Len() != 0 {
			t.Fatalf("prefix-matched module must not execute binary: stdout=%q", stdout.String())
		}
		t.Setenv("MOCK_LMOD_PREFIX_TARGET", "")

		// If EESSI/2025.06 resolves via an alias to EESSI/2023.06, or LOADEDMODULES
		// is left empty, launch must abort instead of running under the wrong stack.
		t.Setenv("MOCK_LMOD_ALIAS_STACK", "EESSI/2025.06")
		stdout.Reset()
		stderr.Reset()
		if code := run([]string{"eessi", "launch", "--stack", "2025.06", "paraview", "--version"}, &stdout, &stderr); code == 0 || stdout.Len() != 0 {
			t.Fatalf("aliased EESSI stack must be rejected: code=%d out=%q err=%q", code, stdout.String(), stderr.String())
		}
		t.Setenv("MOCK_LMOD_ALIAS_STACK", "")

		t.Setenv("MOCK_LMOD_EMPTY_LOADED", "1")
		stdout.Reset()
		stderr.Reset()
		if code := run([]string{"eessi", "launch", "--stack", "2025.06", "paraview", "--version"}, &stdout, &stderr); code == 0 || stdout.Len() != 0 {
			t.Fatalf("empty LOADEDMODULES must be rejected: code=%d out=%q err=%q", code, stdout.String(), stderr.String())
		}
		t.Setenv("MOCK_LMOD_EMPTY_LOADED", "")

		// Missing exact GPU module MUST fail and NEVER fall back to unversioned GROMACS.
		if err := os.WriteFile(lmodLog, nil, 0o644); err != nil {
			t.Fatal(err)
		}
		t.Setenv("MOCK_LMOD_FAIL", "GROMACS/2026.2-foss-2025b-CUDA-12.9.1")
		stdout.Reset()
		stderr.Reset()
		if code := run([]string{"eessi", "launch", "--gpu", "gromacs", "--version"}, &stdout, &stderr); code == 0 {
			t.Fatalf("expected failure when exact GPU module is missing; stdout=%q", stdout.String())
		}
		if stdout.Len() != 0 {
			t.Fatalf("missing exact module must not execute fallback binary: stdout=%q", stdout.String())
		}
		logBytes, err = os.ReadFile(lmodLog)
		if err != nil {
			t.Fatal(err)
		}
		wantFailLog := "LOAD:EESSI/2025.06|MP:" + realModRoot + "|EXACT:yes\nLOAD:GROMACS/2026.2-foss-2025b-CUDA-12.9.1|MP:" + realAccelModDir + ":" + realCPUModDir + ":" + realModRoot + "|EXACT:yes\n"
		if string(logBytes) != wantFailLog {
			t.Fatalf("failed launch must not attempt fallback load; log=%q want=%q", string(logBytes), wantFailLog)
		}
		t.Setenv("MOCK_LMOD_FAIL", "")
	})

	// Malformed eessi.json fails refresh without clobbering the cached copy.
	if err := os.WriteFile(eessiFile, []byte(`{"schemaVersion": 1, "backend": "eessi", "explicitSelectionRequired": false}`), 0o644); err != nil {
		t.Fatal(err)
	}
	stdout.Reset()
	stderr.Reset()
	if code := run([]string{"refresh"}, &stdout, &stderr); code == 0 {
		t.Fatalf("expected refresh to fail on malformed eessi.json")
	}
	stdout.Reset()
	stderr.Reset()
	if code := run([]string{"eessi", "find", "paraview"}, &stdout, &stderr); code != 0 || !strings.Contains(stdout.String(), "ParaView/6.0.1-foss-2025b") {
		t.Fatalf("cached eessi.json should stay intact after failed refresh: code=%d out=%q err=%q", code, stdout.String(), stderr.String())
	}

	// Removing eessiCatalogUrl while preclinical still links an empty {} apps.json
	// retires EESSI cleanly instead of failing with "no containers".
	if err := os.WriteFile(domainsFile, []byte(`{"schemaVersion": 1, "domains": {"preclinical": {"appJsonUrl": "preclinical-apps.json"}, "molecular": {}}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	stdout.Reset()
	stderr.Reset()
	if code := run([]string{"refresh"}, &stdout, &stderr); code != 0 {
		t.Fatalf("refresh after removing eessiCatalogUrl failed (%d): %s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "updated.") || !strings.Contains(stdout.String(), "no apps list yet") {
		t.Fatalf("expected refresh to report updated + empty when EESSI-only domain retires EESSI: %s", stdout.String())
	}
	// A subsequent refresh of that retired domain with the same empty {} apps.json
	// must continue to succeed instead of failing with "no containers".
	stdout.Reset()
	stderr.Reset()
	if code := run([]string{"refresh"}, &stdout, &stderr); code != 0 {
		t.Fatalf("second refresh after EESSI retirement failed (%d): %s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "is up to date.") {
		t.Fatalf("expected second refresh to report up to date: %s", stdout.String())
	}
	// JSON null in apps.json must be rejected even on a retired EESSI-only domain.
	if err := os.WriteFile(emptyAppsFile, []byte(`null`), 0o644); err != nil {
		t.Fatal(err)
	}
	stdout.Reset()
	stderr.Reset()
	if code := run([]string{"refresh"}, &stdout, &stderr); code == 0 {
		t.Fatalf("expected refresh to reject JSON null in apps.json")
	}
	stdout.Reset()
	stderr.Reset()
	if code := run([]string{"eessi", "find", "paraview"}, &stdout, &stderr); code != 0 || stdout.Len() != 0 {
		t.Fatalf("stale eessi.json should be gone after refresh: code=%d out=%q err=%q", code, stdout.String(), stderr.String())
	}
}

func TestEESSICatalogPrecedenceAndMultiLauncher(t *testing.T) {
	tmp := t.TempDir()
	isolateTestEnv(t, tmp)

	srcDir := filepath.Join(tmp, "src")
	if err := os.MkdirAll(srcDir, 0o755); err != nil {
		t.Fatal(err)
	}
	// Catalog 1 has module Tool/1.0-z-second and TWO distinct launchers for one module.
	// Catalog 2 has module Tool/1.0-a-first (lexicographically earlier module name).
	// Catalog 1 MUST still win lookup because it is earlier in catalogs in use.
	eessi1 := `{
	  "schemaVersion": 1,
	  "backend": "eessi",
	  "explicitSelectionRequired": true,
	  "cvmfsRepo": "software.eessi.io",
	  "stacks": ["2025.06"],
	  "tools": {
	    "mytool": {
	      "package": "MyTool",
	      "categories": ["analysis"],
	      "apps": {
	        "MyTool 1.0": {
	          "backend": "eessi",
	          "stack": "2025.06",
	          "module": "MyTool/1.0-z-second",
	          "software_version": "1.0",
	          "arch": ["x86_64"],
	          "gpu": false,
	          "exec": ["mytool"]
	        }
	      }
	    },
	    "suite": {
	      "package": "Suite",
	      "categories": ["analysis"],
	      "apps": {
	        "Suite GUI": {
	          "backend": "eessi",
	          "stack": "2025.06",
	          "module": "Suite/2.0-foss-2025b",
	          "software_version": "2.0",
	          "arch": ["x86_64"],
	          "gpu": false,
	          "exec": ["suite-gui"],
	          "gui": true
	        },
	        "Suite CLI": {
	          "backend": "eessi",
	          "stack": "2025.06",
	          "module": "Suite/2.0-foss-2025b",
	          "software_version": "2.0",
	          "arch": ["x86_64"],
	          "gpu": false,
	          "exec": ["suite-cli"],
	          "gui": false
	        }
	      }
	    }
	  }
	}`
	eessi2 := `{
	  "schemaVersion": 1,
	  "backend": "eessi",
	  "explicitSelectionRequired": true,
	  "cvmfsRepo": "software.eessi.io",
	  "stacks": ["2025.06"],
	  "tools": {
	    "mytool": {
	      "package": "MyTool",
	      "categories": ["analysis"],
	      "apps": {
	        "MyTool 1.0": {
	          "backend": "eessi",
	          "stack": "2025.06",
	          "module": "MyTool/1.0-a-first",
	          "software_version": "1.0",
	          "arch": ["x86_64"],
	          "gpu": false,
	          "exec": ["mytool"]
	        }
	      }
	    }
	  }
	}`
	if err := os.WriteFile(filepath.Join(srcDir, "e1.json"), []byte(eessi1), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(srcDir, "e2.json"), []byte(eessi2), 0o644); err != nil {
		t.Fatal(err)
	}
	cat1 := filepath.Join(srcDir, "cat1.json")
	cat2 := filepath.Join(srcDir, "cat2.json")
	if err := os.WriteFile(cat1, []byte(`{"schemaVersion": 1, "eessiCatalogUrl": "e1.json", "eessi_stacks": ["2025.06"]}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cat2, []byte(`{"schemaVersion": 1, "eessiCatalogUrl": "e2.json", "eessi_stacks": ["2025.06"]}`), 0o644); err != nil {
		t.Fatal(err)
	}

	// Adding "second" first and "first" second places "first" at the top of
	// catalogs in use (highest precedence).
	var stdout, stderr bytes.Buffer
	for _, args := range [][]string{
		{"catalog", "add", "second", cat2},
		{"catalog", "add", "first", cat1},
		{"refresh"},
	} {
		stdout.Reset()
		stderr.Reset()
		if code := run(args, &stdout, &stderr); code != 0 {
			t.Fatalf("%v failed (%d): %s", args, code, stderr.String())
		}
	}

	// Earlier catalog "first" must win over "second" despite lexicographically later module name.
	stdout.Reset()
	stderr.Reset()
	if code := run([]string{"eessi", "info", "mytool/1.0"}, &stdout, &stderr); code != 0 {
		t.Fatalf("eessi info mytool/1.0 failed (%d): %s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "MyTool/1.0-z-second") {
		t.Fatalf("expected earlier catalog module MyTool/1.0-z-second, got:\n%s", stdout.String())
	}

	// Both distinct launchers in "suite" appear unshadowed in find.
	stdout.Reset()
	stderr.Reset()
	if code := run([]string{"eessi", "find", "suite"}, &stdout, &stderr); code != 0 {
		t.Fatalf("eessi find suite failed (%d): %s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "suite-gui") || !strings.Contains(stdout.String(), "suite-cli") || strings.Contains(stdout.String(), "(shadowed)") {
		t.Fatalf("both distinct launchers should appear unshadowed:\n%s", stdout.String())
	}

	// Ambiguous lookup without --launcher fails with choices; --launcher disambiguates.
	stdout.Reset()
	stderr.Reset()
	if code := run([]string{"eessi", "info", "suite"}, &stdout, &stderr); code == 0 {
		t.Fatalf("expected ambiguity error for suite without --launcher; out=%s", stdout.String())
	}
	if !strings.Contains(stderr.String(), "--launcher") {
		t.Fatalf("expected --launcher hint in error: %s", stderr.String())
	}
	stdout.Reset()
	stderr.Reset()
	if code := run([]string{"eessi", "info", "--launcher", "suite-gui", "suite"}, &stdout, &stderr); code != 0 {
		t.Fatalf("eessi info --launcher suite-gui failed (%d): %s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "Suite/2.0-foss-2025b") ||
		!strings.Contains(stdout.String(), "sciget eessi launch --stack 2025.06 --launcher 'Suite GUI' Suite/2.0-foss-2025b") {
		t.Fatalf("multi-launcher info must print disambiguated --launcher command:\n%s", stdout.String())
	}

	// Labels differing only by case with a shared executable remain uniquely addressable.
	caseEESSI := `{
	  "schemaVersion": 1,
	  "backend": "eessi",
	  "explicitSelectionRequired": true,
	  "cvmfsRepo": "software.eessi.io",
	  "stacks": ["2025.06"],
	  "tools": {
	    "cased": {
	      "package": "Cased",
	      "categories": ["analysis"],
	      "apps": {
	        "Cased": {
	          "backend": "eessi",
	          "stack": "2025.06",
	          "module": "Cased/1.0-foss-2025b",
	          "software_version": "1.0",
	          "arch": ["x86_64"],
	          "gpu": false,
	          "exec": ["cased", "--upper"]
	        },
	        "cased": {
	          "backend": "eessi",
	          "stack": "2025.06",
	          "module": "Cased/1.0-foss-2025b",
	          "software_version": "1.0",
	          "arch": ["x86_64"],
	          "gpu": false,
	          "exec": ["cased", "--lower"]
	        }
	      }
	    }
	  }
	}`
	builds, err := parseEESSICatalog([]byte(caseEESSI), []string{"2025.06"})
	if err != nil {
		t.Fatal(err)
	}
	upper, err := lookupEESSIBuild(builds, "cased", "", false, "", "Cased")
	if err != nil || strings.Join(upper.exec, " ") != "cased --upper" {
		t.Fatalf("exact-case --launcher Cased failed: err=%v exec=%v", err, upper.exec)
	}
	lower, err := lookupEESSIBuild(builds, "cased", "", false, "", "cased")
	if err != nil || strings.Join(lower.exec, " ") != "cased --lower" {
		t.Fatalf("exact-case --launcher cased failed: err=%v exec=%v", err, lower.exec)
	}

	// Cross-catalog sibling launchers, including labels differing only by case,
	// identical labels with distinct executables, AND identical module+label+exec[0]
	// with different exec args: formatLaunchCommand must always round-trip to
	// the exact build or omit the command.
	cross1 := `{
	  "schemaVersion": 1,
	  "backend": "eessi",
	  "explicitSelectionRequired": true,
	  "cvmfsRepo": "software.eessi.io",
	  "stacks": ["2025.06"],
	  "tools": {
	    "shared": {
	      "package": "Shared",
	      "categories": ["analysis"],
	      "apps": {
	        "Open GUI": {
	          "backend": "eessi",
	          "stack": "2025.06",
	          "module": "Shared/1.0-foss-2025b",
	          "software_version": "1.0",
	          "arch": ["x86_64"],
	          "gpu": false,
	          "exec": ["high"]
	        }
	      }
	    }
	  }
	}`
	cross2 := strings.ReplaceAll(strings.ReplaceAll(cross1, `"Open GUI"`, `"open gui"`), `"high"`, `"low"`)
	b1, err := parseEESSICatalog([]byte(cross1), []string{"2025.06"})
	if err != nil {
		t.Fatal(err)
	}
	b2, err := parseEESSICatalog([]byte(cross2), []string{"2025.06"})
	if err != nil {
		t.Fatal(err)
	}
	b1[0].catalog, b1[0].catalogOrder = "cat-a", 0
	b2[0].catalog, b2[0].catalogOrder = "cat-b", 1
	combined := append(b1, b2...)
	if got := formatLaunchCommand(combined, b2[0]); !strings.Contains(got, "--launcher 'open gui'") {
		t.Fatalf("cross-catalog Launch command missing --launcher 'open gui': %s", got)
	}

	// Identical label across two catalogs with distinct executables falls back to exec[0].
	crossSameLabel := strings.ReplaceAll(cross1, `"high"`, `"low"`)
	b3, err := parseEESSICatalog([]byte(crossSameLabel), []string{"2025.06"})
	if err != nil {
		t.Fatal(err)
	}
	b3[0].catalog, b3[0].catalogOrder = "cat-b", 1
	combinedSame := append(slices.Clone(b1), b3...)
	if got := formatLaunchCommand(combinedSame, b3[0]); !strings.Contains(got, "--launcher low") {
		t.Fatalf("expected --launcher low for identical-label distinct-exec sibling, got %q", got)
	}

	// Identical module, label, and exec[0] differing only in exec[1:] across
	// two catalogs uses --catalog so the printed command launches cat-b, not cat-a.
	crossDiffArgs := strings.ReplaceAll(cross1, `"exec": ["high"]`, `"exec": ["high", "--alt"]`)
	b4, err := parseEESSICatalog([]byte(crossDiffArgs), []string{"2025.06"})
	if err != nil {
		t.Fatal(err)
	}
	b4[0].catalog, b4[0].catalogOrder = "cat-b", 1
	combinedArgs := append(slices.Clone(b1), b4...)
	if got := formatLaunchCommand(combinedArgs, b4[0]); !strings.Contains(got, "--catalog cat-b") {
		t.Fatalf("expected --catalog cat-b for identical module/label/exec[0], got %q", got)
	}
	resolvedAlt, err := lookupEESSIBuild(combinedArgs, b4[0].module, b4[0].stack, b4[0].gpu, "cat-b", "")
	if err != nil || !sameEESSIBuild(resolvedAlt, b4[0]) {
		t.Fatalf("--catalog cat-b resolved to %+v (err=%v), want %+v", resolvedAlt, err, b4[0])
	}

	// A CPU build in a higher-priority catalog must not shadow a GPU build with
	// the same tool/stack/module/exec in a lower-priority catalog.
	gpu1 := strings.ReplaceAll(cross1, `"high"`, `"sim"`)
	gpu2 := strings.Replace(gpu1, `"gpu": false`, `"gpu": true`, 1)
	if err := os.WriteFile(filepath.Join(srcDir, "eessi-gpu-1.json"), []byte(gpu1), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(srcDir, "eessi-gpu-2.json"), []byte(gpu2), 0o644); err != nil {
		t.Fatal(err)
	}
	catGPU1 := filepath.Join(srcDir, "cat-gpu-1.json")
	catGPU2 := filepath.Join(srcDir, "cat-gpu-2.json")
	if err := os.WriteFile(catGPU1, []byte(`{"schemaVersion": 1, "eessiCatalogUrl": "eessi-gpu-1.json", "eessi_stacks": ["2025.06"]}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(catGPU2, []byte(`{"schemaVersion": 1, "eessiCatalogUrl": "eessi-gpu-2.json", "eessi_stacks": ["2025.06"]}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if code := run([]string{"catalog", "add", "gpu-low", catGPU2}, &stdout, &stderr); code != 0 {
		t.Fatalf("add gpu-low failed: %s", stderr.String())
	}
	if code := run([]string{"catalog", "add", "gpu-high", catGPU1}, &stdout, &stderr); code != 0 {
		t.Fatalf("add gpu-high failed: %s", stderr.String())
	}
	if code := run([]string{"refresh"}, &stdout, &stderr); code != 0 {
		t.Fatalf("refresh gpu catalogs failed: %s", stderr.String())
	}
	stdout.Reset()
	stderr.Reset()
	if code := run([]string{"eessi", "find", "--gpu", "shared"}, &stdout, &stderr); code != 0 || !strings.Contains(stdout.String(), "gpu-low") || strings.Contains(stdout.String(), "(shadowed)") {
		t.Fatalf("eessi find --gpu should list unshadowed GPU build in gpu-low: code=%d out=%q err=%q", code, stdout.String(), stderr.String())
	}
	stdout.Reset()
	stderr.Reset()
	if code := run([]string{"eessi", "info", "--gpu", "shared"}, &stdout, &stderr); code != 0 || !strings.Contains(stdout.String(), "gpu-low") {
		t.Fatalf("eessi info --gpu should resolve unshadowed GPU build in gpu-low: code=%d out=%q err=%q", code, stdout.String(), stderr.String())
	}
}

func TestInvalidEESSIInOldCacheDoesNotBreakContainerCommands(t *testing.T) {
	tmp := t.TempDir()
	isolateTestEnv(t, tmp)

	srcDir := filepath.Join(tmp, "src")
	if err := os.MkdirAll(srcDir, 0o755); err != nil {
		t.Fatal(err)
	}
	appsJSON := `{"fsl":{"categories":["structural imaging"],"apps":{"fsl 6.0.7":{"image":"fsl_6.0.7","version":"20260101","exec":"bet"}}}}`
	if err := os.WriteFile(filepath.Join(srcDir, "apps.json"), []byte(appsJSON), 0o644); err != nil {
		t.Fatal(err)
	}
	catFile := filepath.Join(srcDir, "cat.json")
	if err := os.WriteFile(catFile, []byte(`{"schemaVersion": 1, "appJsonUrl": "apps.json"}`), 0o644); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr bytes.Buffer
	if code := run([]string{"catalog", "add", "neuro", catFile}, &stdout, &stderr); code != 0 {
		t.Fatalf("catalog add failed: %s", stderr.String())
	}
	if code := run([]string{"refresh"}, &stdout, &stderr); code != 0 {
		t.Fatalf("refresh failed: %s", stderr.String())
	}

	// Simulate an older cached catalog.json with wrong-type EESSI fields
	// ("eessi_stacks": "bad", "eessiCatalogUrl": {}) plus a stale eessi.json.
	cacheRoot, used, err := usedCatalogs()
	if err != nil || len(used) != 1 {
		t.Fatalf("usedCatalogs failed: %v (%d)", err, len(used))
	}
	cacheDir := used[0].dir(cacheRoot)
	if err := os.WriteFile(filepath.Join(cacheDir, "catalog.json"), []byte(`{"schemaVersion": 1, "appJsonUrl": "apps.json", "eessi_stacks": "bad", "eessiCatalogUrl": {}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cacheDir, "eessi.json"), []byte(testEESSIJSON), 0o644); err != nil {
		t.Fatal(err)
	}

	// Ordinary container find still succeeds cleanly.
	stdout.Reset()
	stderr.Reset()
	if code := run([]string{"find", "fsl"}, &stdout, &stderr); code != 0 || !strings.Contains(stdout.String(), "fsl/6.0.7") {
		t.Fatalf("container find should ignore malformed EESSI fields in old cache: code=%d out=%q err=%q", code, stdout.String(), stderr.String())
	}

	// EESSI commands ignore leftover eessi.json when catalog.json omits eessiCatalogUrl.
	if err := os.WriteFile(filepath.Join(cacheDir, "catalog.json"), []byte(`{"schemaVersion": 1, "appJsonUrl": "apps.json"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	stdout.Reset()
	stderr.Reset()
	if code := run([]string{"eessi", "find"}, &stdout, &stderr); code != 0 || stdout.Len() != 0 {
		t.Fatalf("eessi find should ignore stale eessi.json when catalog.json omits eessiCatalogUrl: code=%d out=%q err=%q", code, stdout.String(), stderr.String())
	}
}

func TestRefreshRollbackOnRenameFailure(t *testing.T) {
	tmp := t.TempDir()
	isolateTestEnv(t, tmp)

	srcDir := filepath.Join(tmp, "src")
	if err := os.MkdirAll(srcDir, 0o755); err != nil {
		t.Fatal(err)
	}
	appsV1 := `{"fsl":{"categories":["structural imaging"],"apps":{"fsl 6.0.7":{"image":"fsl_6.0.7","version":"20260101","exec":"bet"}}}}`
	appsV2 := `{"fsl":{"categories":["structural imaging"],"apps":{"fsl 6.0.8":{"image":"fsl_6.0.8","version":"20260201","exec":"bet"}}}}`
	if err := os.WriteFile(filepath.Join(srcDir, "apps.json"), []byte(appsV1), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(srcDir, "eessi.json"), []byte(testEESSIJSON), 0o644); err != nil {
		t.Fatal(err)
	}
	catFile := filepath.Join(srcDir, "cat.json")
	if err := os.WriteFile(catFile, []byte(`{"schemaVersion": 1, "appJsonUrl": "apps.json", "eessiCatalogUrl": "eessi.json", "eessi_stacks": ["2025.06"]}`), 0o644); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr bytes.Buffer
	if code := run([]string{"catalog", "add", "neuro", catFile}, &stdout, &stderr); code != 0 {
		t.Fatalf("catalog add failed: %s", stderr.String())
	}
	if code := run([]string{"refresh"}, &stdout, &stderr); code != 0 {
		t.Fatalf("initial refresh failed: %s", stderr.String())
	}

	// Update both apps.json and eessi.json, let apps.json rename succeed, and
	// inject a rename error on eessi.json so the rollback path actually runs.
	if err := os.WriteFile(filepath.Join(srcDir, "apps.json"), []byte(appsV2), 0o644); err != nil {
		t.Fatal(err)
	}
	eessiV2 := strings.Replace(testEESSIJSON, `"3D visualization"`, `"3D visualization v2"`, 1)
	if err := os.WriteFile(filepath.Join(srcDir, "eessi.json"), []byte(eessiV2), 0o644); err != nil {
		t.Fatal(err)
	}
	cacheRoot, used, err := usedCatalogs()
	if err != nil || len(used) != 1 {
		t.Fatalf("usedCatalogs failed: %v", err)
	}
	cacheDir := used[0].dir(cacheRoot)
	appsPath := filepath.Join(cacheDir, "apps.json")
	eessiPath := filepath.Join(cacheDir, "eessi.json")

	oldRename := renameCatalogFile
	t.Cleanup(func() { renameCatalogFile = oldRename })
	appsRenamed := false
	eessiInjected := false
	appsRenamedCh := make(chan struct{})
	closeAppsRenamed := func() {
		select {
		case <-appsRenamedCh:
		default:
			close(appsRenamedCh)
		}
	}
	t.Cleanup(closeAppsRenamed)
	readerEntering := make(chan struct{})
	oldBeforeLock := beforeCatalogSharedLock
	t.Cleanup(func() { beforeCatalogSharedLock = oldBeforeLock })
	beforeCatalogSharedLock = func(dir string) {
		if dir == cacheDir {
			select {
			case <-readerEntering:
			default:
				close(readerEntering)
			}
		}
	}
	type readResult struct {
		containers []container
		err        error
	}
	readerDone := make(chan readResult, 1)
	go func() {
		<-appsRenamedCh
		cs, err := readContainers()
		readerDone <- readResult{containers: cs, err: err}
	}()
	renameCatalogFile = func(oldpath, newpath string) error {
		if newpath == appsPath {
			err := os.Rename(oldpath, newpath)
			if err == nil {
				appsRenamed = true
				closeAppsRenamed()
				// Wait deterministically until the concurrent reader reaches
				// the shared lock, and assert it blocks rather than reading v2.
				<-readerEntering
				select {
				case r := <-readerDone:
					t.Errorf("concurrent readContainers bypassed lock during commit: %+v", r)
					readerDone <- r
				case <-time.After(25 * time.Millisecond):
				}
			}
			return err
		}
		if newpath == eessiPath {
			eessiInjected = true
			return os.ErrPermission
		}
		return os.Rename(oldpath, newpath)
	}

	stdout.Reset()
	stderr.Reset()
	if code := run([]string{"refresh"}, &stdout, &stderr); code == 0 {
		t.Fatalf("expected refresh to fail when eessi.json rename fails")
	}
	if !appsRenamed || !eessiInjected {
		t.Fatalf("test did not exercise post-rename rollback: appsRenamed=%v eessiInjected=%v", appsRenamed, eessiInjected)
	}
	renameCatalogFile = oldRename

	// Concurrent container reader must wait for the rollback and never observe v2 (fsl/6.0.8).
	res := <-readerDone
	beforeCatalogSharedLock = oldBeforeLock
	if res.err != nil {
		t.Fatalf("concurrent readContainers failed: %v", res.err)
	}
	if len(res.containers) != 1 || res.containers[0].name() != "fsl/6.0.7" {
		t.Fatalf("concurrent readContainers observed uncommitted update: got %+v, want [fsl/6.0.7]", res.containers)
	}

	// apps.json must have been rolled back to v1 (never mixed v2 apps + v1 eessi).
	gotApps, err := os.ReadFile(appsPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(gotApps) != appsV1 {
		t.Fatalf("apps.json was not rolled back on eessi.json rename failure: got %s, want %s", string(gotApps), appsV1)
	}
	gotEESSI, err := os.ReadFile(eessiPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(gotEESSI) != testEESSIJSON {
		t.Fatalf("eessi.json should remain at v1 after failed rename")
	}

	// Catalog removal uses the external lock so it blocks while a refresh is
	// in progress; a re-added catalog's newer cache cannot be deleted by an
	// earlier refresh rolling back.
	inRefresh := make(chan struct{})
	releaseRefresh := make(chan struct{})
	closeInRefresh := func() {
		select {
		case <-inRefresh:
		default:
			close(inRefresh)
		}
	}
	closeReleaseRefresh := func() {
		select {
		case <-releaseRefresh:
		default:
			close(releaseRefresh)
		}
	}
	t.Cleanup(closeInRefresh)
	t.Cleanup(closeReleaseRefresh)
	renameCatalogFile = func(oldpath, newpath string) error {
		if newpath == appsPath {
			err := os.Rename(oldpath, newpath)
			if err == nil {
				closeInRefresh()
				<-releaseRefresh
			}
			return err
		}
		if newpath == eessiPath {
			return os.ErrPermission
		}
		return os.Rename(oldpath, newpath)
	}
	refreshDone := make(chan int, 1)
	go func() {
		var rOut, rErr bytes.Buffer
		refreshDone <- run([]string{"refresh"}, &rOut, &rErr)
	}()
	<-inRefresh
	removeDone := make(chan int, 1)
	go func() {
		var rmOut, rmErr bytes.Buffer
		removeDone <- run([]string{"catalog", "remove", "neuro"}, &rmOut, &rmErr)
	}()
	select {
	case code := <-removeDone:
		closeReleaseRefresh()
		t.Fatalf("catalog remove bypassed active refresh lock (exit %d)", code)
	case <-time.After(25 * time.Millisecond):
	}
	closeReleaseRefresh()
	if code := <-refreshDone; code == 0 {
		t.Fatalf("expected interrupted refresh to fail and roll back")
	}
	if code := <-removeDone; code != 0 {
		t.Fatalf("catalog remove failed after refresh released lock: exit %d", code)
	}
	renameCatalogFile = oldRename
	if _, err := os.Stat(cacheDir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("catalog cache should be removed after remove completes, stat err=%v", err)
	}
	// Re-add and refresh cleanly back to v1 for subsequent subtests.
	if err := os.WriteFile(filepath.Join(srcDir, "apps.json"), []byte(appsV1), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(srcDir, "eessi.json"), []byte(testEESSIJSON), 0o644); err != nil {
		t.Fatal(err)
	}
	stdout.Reset()
	stderr.Reset()
	if code := run([]string{"catalog", "add", "neuro", catFile}, &stdout, &stderr); code != 0 {
		t.Fatalf("re-add failed (%d): %s", code, stderr.String())
	}
	// Before the re-added catalog is refreshed (its external lock survives removal,
	// while its cache directory does not exist yet), EESSI commands must skip it
	// cleanly instead of failing.
	stdout.Reset()
	stderr.Reset()
	if code := run([]string{"eessi", "find"}, &stdout, &stderr); code != 0 {
		t.Fatalf("eessi find must skip unrefreshed re-added catalog (%d): %s", code, stderr.String())
	}
	stdout.Reset()
	stderr.Reset()
	if code := run([]string{"refresh"}, &stdout, &stderr); code != 0 {
		t.Fatalf("re-refresh failed (%d): %s", code, stderr.String())
	}

	// A delayed refresh that downloaded stale data before a remove/re-add and
	// newer refresh must refuse to overwrite the newer cache.
	staleEESSI := strings.Replace(testEESSIJSON, `"ParaView/6.0.1-foss-2025b"`, `"ParaView/5.0.1-foss-2025b"`, 1)
	catMetaBytes := []byte(`{"schemaVersion": 1, "appJsonUrl": "apps.json", "eessiCatalogUrl": "eessi.json", "eessi_stacks": ["2025.06"]}`)
	delayedPaused := make(chan struct{})
	resumeDelayed := make(chan struct{})
	delayedErr := make(chan error, 1)
	go func() {
		_, _, err := refreshCatalog(func(link string) ([]byte, string, error) {
			switch {
			case strings.HasSuffix(link, "cat.json"):
				close(delayedPaused)
				<-resumeDelayed
				return catMetaBytes, link, nil
			case strings.HasSuffix(link, "apps.json"):
				return []byte(appsV1), link, nil
			case strings.HasSuffix(link, "eessi.json"):
				return []byte(staleEESSI), link, nil
			default:
				return nil, "", os.ErrNotExist
			}
		}, used[0], cacheDir)
		delayedErr <- err
	}()
	<-delayedPaused
	stdout.Reset()
	stderr.Reset()
	if code := run([]string{"catalog", "remove", "neuro"}, &stdout, &stderr); code != 0 {
		close(resumeDelayed)
		t.Fatalf("catalog remove failed (%d): %s", code, stderr.String())
	}
	stdout.Reset()
	stderr.Reset()
	if code := run([]string{"catalog", "add", "neuro", catFile}, &stdout, &stderr); code != 0 {
		close(resumeDelayed)
		t.Fatalf("catalog re-add failed (%d): %s", code, stderr.String())
	}
	if _, _, err := refreshCatalog(func(link string) ([]byte, string, error) {
		switch {
		case strings.HasSuffix(link, "cat.json"):
			return catMetaBytes, link, nil
		case strings.HasSuffix(link, "apps.json"):
			return []byte(appsV1), link, nil
		case strings.HasSuffix(link, "eessi.json"):
			return []byte(testEESSIJSON), link, nil
		default:
			return nil, "", os.ErrNotExist
		}
	}, used[0], cacheDir); err != nil {
		close(resumeDelayed)
		t.Fatalf("newer refresh failed: %v", err)
	}
	close(resumeDelayed)
	if err := <-delayedErr; err == nil || !strings.Contains(err.Error(), "catalog cache changed during refresh") {
		t.Fatalf("delayed refresh must fail instead of overwriting newer cache: err=%v", err)
	}
	gotEESSI, err = os.ReadFile(eessiPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(gotEESSI), "ParaView/6.0.1-foss-2025b") || strings.Contains(string(gotEESSI), "5.0.1") {
		t.Fatalf("delayed refresh reverted newer cache: %s", string(gotEESSI))
	}

	// Lock file stores fixed-width non-empty generation without truncation.
	lockData, err := os.ReadFile(catalogLockPath(cacheDir))
	if err != nil || len(lockData) != 21 {
		t.Fatalf("expected 21-byte fixed-width lock generation, got %q (err=%v)", string(lockData), err)
	}

	// Pending catalog remove paused after writing catalogs.json and before
	// acquiring the cache lock must not delete a newly re-added and refreshed cache.
	removePaused := make(chan struct{})
	resumeRemove := make(chan struct{})
	oldBeforeRemove := beforeRemoveCatalogCache
	t.Cleanup(func() { beforeRemoveCatalogCache = oldBeforeRemove })
	beforeRemoveCatalogCache = func(dir string) {
		if dir == cacheDir {
			close(removePaused)
			<-resumeRemove
		}
	}
	pausedRemoveDone := make(chan int, 1)
	go func() {
		var rmOut, rmErr bytes.Buffer
		pausedRemoveDone <- run([]string{"catalog", "remove", "neuro"}, &rmOut, &rmErr)
	}()
	<-removePaused
	stdout.Reset()
	stderr.Reset()
	if code := run([]string{"catalog", "add", "neuro", catFile}, &stdout, &stderr); code != 0 {
		close(resumeRemove)
		t.Fatalf("re-add during paused remove failed (%d): %s", code, stderr.String())
	}
	stdout.Reset()
	stderr.Reset()
	if code := run([]string{"refresh"}, &stdout, &stderr); code != 0 {
		close(resumeRemove)
		t.Fatalf("refresh during paused remove failed (%d): %s", code, stderr.String())
	}
	close(resumeRemove)
	if code := <-pausedRemoveDone; code != 0 {
		t.Fatalf("paused remove failed: exit %d", code)
	}
	beforeRemoveCatalogCache = oldBeforeRemove
	if _, err := os.Stat(appsPath); err != nil {
		t.Fatalf("paused catalog remove deleted newly re-added cache: %v", err)
	}

	// Reverse race: a refresh that read the registry before removal and sampled
	// generation after removal must see the catalog is unregistered and refuse
	// to recreate the removed cache.
	stdout.Reset()
	stderr.Reset()
	if code := run([]string{"catalog", "remove", "neuro"}, &stdout, &stderr); code != 0 {
		t.Fatalf("remove for reverse-race check failed (%d): %s", code, stderr.String())
	}
	postRemoveGen, err := readCatalogLockGen(cacheDir)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := refreshCatalogAt(func(link string) ([]byte, string, error) {
		switch {
		case strings.HasSuffix(link, "cat.json"):
			return catMetaBytes, link, nil
		case strings.HasSuffix(link, "apps.json"):
			return []byte(appsV1), link, nil
		case strings.HasSuffix(link, "eessi.json"):
			return []byte(testEESSIJSON), link, nil
		default:
			return nil, "", os.ErrNotExist
		}
	}, used[0], cacheDir, postRemoveGen, func() (bool, error) {
		return catalogDirRegistered(cacheDir)
	}); err == nil || !strings.Contains(err.Error(), "removed during refresh") {
		t.Fatalf("refresh of removed catalog must fail instead of recreating cache: err=%v", err)
	}
	if _, err := os.Stat(cacheDir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("refresh recreated removed catalog cache: stat err=%v", err)
	}

	// Re-add and refresh back to v1 for the remaining subtests.
	stdout.Reset()
	stderr.Reset()
	if code := run([]string{"catalog", "add", "neuro", catFile}, &stdout, &stderr); code != 0 {
		t.Fatalf("re-add after reverse-race check failed (%d): %s", code, stderr.String())
	}
	stdout.Reset()
	stderr.Reset()
	if code := run([]string{"refresh"}, &stdout, &stderr); code != 0 {
		t.Fatalf("refresh after reverse-race check failed (%d): %s", code, stderr.String())
	}
	// Restore v2 in srcDir for the unreadable-file subtest below.
	if err := os.WriteFile(filepath.Join(srcDir, "apps.json"), []byte(appsV2), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(srcDir, "eessi.json"), []byte(eessiV2), 0o644); err != nil {
		t.Fatal(err)
	}

	// Older caches without an external lock file remain readable by container commands.
	lockPath := catalogLockPath(cacheDir)
	if err := os.Remove(lockPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
	legacyContainers, err := readContainers()
	if err != nil || len(legacyContainers) != 1 || legacyContainers[0].name() != "fsl/6.0.7" {
		t.Fatalf("older cache without lock file must stay readable: err=%v containers=%+v", err, legacyContainers)
	}

	// Preserve unreadable existing apps.json when staging/backup cannot read it.
	if runtime.GOOS != "windows" && os.Geteuid() != 0 {
		if err := os.Chmod(appsPath, 0o000); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Chmod(appsPath, 0o644) })
		stdout.Reset()
		stderr.Reset()
		if code := run([]string{"refresh"}, &stdout, &stderr); code == 0 {
			t.Fatalf("expected refresh to fail when existing apps.json is unreadable")
		}
		if err := os.Chmod(appsPath, 0o644); err != nil {
			t.Fatal(err)
		}
		gotApps, err = os.ReadFile(appsPath)
		if err != nil {
			t.Fatalf("unreadable apps.json must not be deleted on failed refresh: %v", err)
		}
		if string(gotApps) != appsV1 {
			t.Fatalf("apps.json modified on failed unreadable refresh: got %s, want %s", string(gotApps), appsV1)
		}
	}

	// If the directory becomes read-only after eessi.json is renamed and before
	// catalog.json is renamed (so rollback also fails), refresh reports the
	// rollback failure and both EESSI and container readers refuse to serve the
	// uncommitted cache until a later refresh recovers it.
	if runtime.GOOS != "windows" && os.Geteuid() != 0 {
		catV2 := `{"schemaVersion": 1, "title": "v2", "appJsonUrl": "apps.json", "eessiCatalogUrl": "eessi.json", "eessi_stacks": ["2025.06"]}`
		if err := os.WriteFile(catFile, []byte(catV2), 0o644); err != nil {
			t.Fatal(err)
		}
		eessiUncommitted := strings.Replace(testEESSIJSON, `"ParaView/6.0.1-foss-2025b"`, `"ParaView/9.9.9-uncommitted"`, 1)
		if err := os.WriteFile(filepath.Join(srcDir, "eessi.json"), []byte(eessiUncommitted), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(srcDir, "apps.json"), []byte(appsV1), 0o644); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Chmod(cacheDir, 0o755) })
		renameCatalogFile = func(oldpath, newpath string) error {
			err := os.Rename(oldpath, newpath)
			if err == nil && newpath == eessiPath {
				_ = os.Chmod(cacheDir, 0o555)
			}
			return err
		}
		stdout.Reset()
		stderr.Reset()
		if code := run([]string{"refresh"}, &stdout, &stderr); code == 0 {
			t.Fatalf("expected refresh to fail when cache dir turns read-only before catalog.json rename")
		}
		renameCatalogFile = oldRename
		if !strings.Contains(stderr.String(), "rollback incomplete") {
			t.Fatalf("expected rollback failure report in stderr: %s", stderr.String())
		}
		stdout.Reset()
		stderr.Reset()
		if code := run([]string{"eessi", "find", "paraview"}, &stdout, &stderr); code == 0 || strings.Contains(stdout.String(), "9.9.9-uncommitted") {
			t.Fatalf("eessi find must refuse to serve uncommitted build after failed rollback: code=%d out=%q err=%q", code, stdout.String(), stderr.String())
		}
		if cs, err := readContainers(); err == nil {
			t.Fatalf("readContainers must refuse incomplete rollback cache, got %+v", cs)
		}
		if err := os.Chmod(cacheDir, 0o755); err != nil {
			t.Fatal(err)
		}
		stdout.Reset()
		stderr.Reset()
		if code := run([]string{"refresh"}, &stdout, &stderr); code != 0 {
			t.Fatalf("recovery refresh failed (%d): %s", code, stderr.String())
		}
		stdout.Reset()
		stderr.Reset()
		if code := run([]string{"eessi", "find", "paraview"}, &stdout, &stderr); code != 0 || !strings.Contains(stdout.String(), "9.9.9-uncommitted") {
			t.Fatalf("eessi find should work cleanly after recovery refresh: code=%d out=%q err=%q", code, stdout.String(), stderr.String())
		}
		if cs, err := readContainers(); err != nil || len(cs) != 1 || cs[0].name() != "fsl/6.0.7" {
			t.Fatalf("readContainers should work cleanly after recovery refresh: err=%v containers=%+v", err, cs)
		}
	}

	// Concurrent catalog list writes and reads (including through a symlink to
	// catalogs.json) must never observe a truncated file (EOF).
	listPath := filepath.Join(tmp, "concurrent-catalogs.json")
	sampleList := []catalog{{Name: "neuro", URL: catFile, Use: true}}
	if err := writeCatalogList(listPath, sampleList, false); err != nil {
		t.Fatal(err)
	}
	readPath := listPath
	if runtime.GOOS != "windows" {
		linkDir := filepath.Join(tmp, "link-dir")
		if err := os.MkdirAll(linkDir, 0o755); err != nil {
			t.Fatal(err)
		}
		readPath = filepath.Join(linkDir, "catalogs.json")
		if err := os.Symlink(listPath, readPath); err != nil {
			t.Fatal(err)
		}
	}
	stopWrite := make(chan struct{})
	writeErrs := make(chan error, 1)
	go func() {
		for {
			select {
			case <-stopWrite:
				writeErrs <- nil
				return
			default:
				if err := writeCatalogList(listPath, sampleList, false); err != nil {
					writeErrs <- err
					return
				}
			}
		}
	}()
	for range 100 {
		cats, err := loadCatalogList(readPath)
		if err != nil || len(cats) != 1 || cats[0].Name != "neuro" {
			close(stopWrite)
			<-writeErrs
			t.Fatalf("concurrent loadCatalogList failed during write: err=%v cats=%+v", err, cats)
		}
	}
	close(stopWrite)
	if err := <-writeErrs; err != nil {
		t.Fatalf("concurrent writeCatalogList failed: %v", err)
	}

	// A writable catalogs.json inside a read-only (0555) directory, accessed
	// directly or through a symlink, must stay updatable without needing to
	// create any sibling lock file.
	if runtime.GOOS != "windows" && os.Geteuid() != 0 {
		roDir := filepath.Join(tmp, "ro-catalogs-dir")
		if err := os.MkdirAll(roDir, 0o755); err != nil {
			t.Fatal(err)
		}
		roFile := filepath.Join(roDir, "catalogs.json")
		if err := writeCatalogList(roFile, sampleList, false); err != nil {
			t.Fatal(err)
		}
		roLinkDir := filepath.Join(tmp, "ro-link-dir")
		if err := os.MkdirAll(roLinkDir, 0o755); err != nil {
			t.Fatal(err)
		}
		roLink := filepath.Join(roLinkDir, "catalogs.json")
		if err := os.Symlink(roFile, roLink); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(roDir, 0o555); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Chmod(roDir, 0o755) })
		updatedList := []catalog{{Name: "neuro", URL: catFile, Use: false}}
		if err := writeCatalogList(roLink, updatedList, false); err != nil {
			t.Fatalf("updating writable catalogs.json in 0555 dir through symlink failed: %v", err)
		}
		gotCats, err := loadCatalogList(roFile)
		if err != nil || len(gotCats) != 1 || gotCats[0].Use {
			t.Fatalf("expected updated catalogs.json with Use=false: err=%v cats=%+v", err, gotCats)
		}
	}

	// First write through a dangling symlink must publish complete JSON
	// atomically so concurrent readers never observe a zero-byte file (EOF).
	if runtime.GOOS != "windows" {
		for i := range 20 {
			danglingDir := filepath.Join(tmp, "dangling-first", string(rune('a'+i)))
			targetDir := filepath.Join(danglingDir, "target")
			if err := os.MkdirAll(targetDir, 0o755); err != nil {
				t.Fatal(err)
			}
			targetFile := filepath.Join(targetDir, "catalogs.json")
			if i == 0 {
				// Also verify a 255-byte target filename fits staging cleanly.
				targetFile = filepath.Join(targetDir, strings.Repeat("a", 250)+".json")
			}
			linkFile := filepath.Join(danglingDir, "link.json")
			if err := os.Symlink(targetFile, linkFile); err != nil {
				t.Fatal(err)
			}
			stopRead := make(chan struct{})
			readErrs := make(chan error, 1)
			go func() {
				for {
					select {
					case <-stopRead:
						readErrs <- nil
						return
					default:
						if _, err := loadCatalogList(linkFile); err != nil {
							readErrs <- err
							return
						}
					}
				}
			}()
			if err := writeCatalogList(linkFile, sampleList, false); err != nil {
				close(stopRead)
				<-readErrs
				t.Fatalf("first write through dangling symlink failed: %v", err)
			}
			close(stopRead)
			if err := <-readErrs; err != nil {
				t.Fatalf("concurrent read during dangling-symlink first write failed: %v", err)
			}
		}

		// Broken dangling symlinks (missing/../actual.json, or directory-only
		// targets ending in "/" or "/.") must fail instead of creating regular files.
		for _, tc := range []struct {
			name   string
			target string
			forbid string
		}{
			{"missing-dotdot", "missing/../actual.json", "actual.json"},
			{"trailing-slash", "dir-only/", "dir-only"},
			{"trailing-dot", "dir-dot/.", "dir-dot"},
		} {
			brokenDir := filepath.Join(tmp, "broken-dangling-"+tc.name)
			if err := os.MkdirAll(brokenDir, 0o755); err != nil {
				t.Fatal(err)
			}
			brokenLink := filepath.Join(brokenDir, "catalogs.json")
			if err := os.Symlink(tc.target, brokenLink); err != nil {
				t.Fatal(err)
			}
			if err := writeCatalogList(brokenLink, sampleList, false); err == nil {
				t.Fatalf("%s: expected error writing through broken symlink %q", tc.name, tc.target)
			}
			if _, err := os.Stat(filepath.Join(brokenDir, tc.forbid)); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("%s: broken symlink write must not create %s: stat err=%v", tc.name, tc.forbid, err)
			}
		}

		// Malformed apps.json with null tool apps ({"fsl":{"apps":null}}) must
		// NOT be treated as an empty EESSI-only container catalog or overwrite
		// a populated cache.
		badAppsDir := filepath.Join(tmp, "bad-apps-src")
		if err := os.MkdirAll(badAppsDir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(badAppsDir, "apps.json"), []byte(`{"fsl":{"apps":null}}`), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(badAppsDir, "eessi.json"), []byte(testEESSIJSON), 0o644); err != nil {
			t.Fatal(err)
		}
		badCatFile := filepath.Join(badAppsDir, "cat.json")
		if err := os.WriteFile(badCatFile, []byte(`{"schemaVersion": 1, "appJsonUrl": "apps.json", "eessiCatalogUrl": "eessi.json", "eessi_stacks": ["2025.06"]}`), 0o644); err != nil {
			t.Fatal(err)
		}
		if _, _, err := refreshCatalog(func(link string) ([]byte, string, error) {
			data, rerr := os.ReadFile(link)
			return data, link, rerr
		}, catalog{Name: "neuro", URL: badCatFile}, cacheDir); err == nil {
			t.Fatalf("refreshCatalog must reject malformed apps.json {\"fsl\":{\"apps\":null}} even when eessi.json is valid")
		}
		if gotApps, err := os.ReadFile(appsPath); err != nil || string(gotApps) != appsV1 {
			t.Fatalf("populated apps.json was overwritten by malformed null-apps catalog: err=%v got=%s", err, string(gotApps))
		}

		// Retiring EESSI (removing eessiCatalogUrl) and the last container
		// (publishing "{}" in apps.json) together on a previously populated
		// container+EESSI cache must succeed, remove stale eessi.json, and stay
		// healthy on subsequent refreshes.
		retireDir := filepath.Join(tmp, "retire-both-src")
		if err := os.MkdirAll(retireDir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(retireDir, "apps.json"), []byte(`{}`), 0o644); err != nil {
			t.Fatal(err)
		}
		retireCatFile := filepath.Join(retireDir, "cat.json")
		if err := os.WriteFile(retireCatFile, []byte(`{"schemaVersion": 1, "appJsonUrl": "apps.json"}`), 0o644); err != nil {
			t.Fatal(err)
		}
		fileRead := func(link string) ([]byte, string, error) {
			data, rerr := os.ReadFile(link)
			return data, link, rerr
		}
		if changed, empty, err := refreshCatalog(fileRead, catalog{Name: "neuro", URL: retireCatFile}, cacheDir); err != nil || !changed || !empty {
			t.Fatalf("retiring EESSI + last container failed: changed=%v empty=%v err=%v", changed, empty, err)
		}
		if _, err := os.Stat(eessiPath); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("stale eessi.json must be removed on combined retirement: stat err=%v", err)
		}
		if changed, empty, err := refreshCatalog(fileRead, catalog{Name: "neuro", URL: retireCatFile}, cacheDir); err != nil || changed || !empty {
			t.Fatalf("subsequent refresh after combined retirement failed: changed=%v empty=%v err=%v", changed, empty, err)
		}

		// Two cache roots sharing a catalogs/ subtree via symlink must resolve
		// to the exact same external lock file.
		sharedRoot := filepath.Join(tmp, "shared-cache")
		sharedCatalogs := filepath.Join(sharedRoot, "catalogs")
		if err := os.MkdirAll(sharedCatalogs, 0o755); err != nil {
			t.Fatal(err)
		}
		aliasRoot := filepath.Join(tmp, "alias-cache")
		if err := os.MkdirAll(aliasRoot, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(sharedCatalogs, filepath.Join(aliasRoot, "catalogs")); err != nil {
			t.Fatal(err)
		}
		lockA := catalogLockPath(filepath.Join(sharedCatalogs, "neuro-1234567890ab"))
		lockB := catalogLockPath(filepath.Join(aliasRoot, "catalogs", "neuro-1234567890ab"))
		if lockA != lockB {
			t.Fatalf("aliased cache roots split lock paths: %q != %q", lockA, lockB)
		}

		// Mixed-case Catalogs/ alias (as on case-insensitive macOS APFS or when
		// two cache roots alias a not-yet-created Catalogs target) must resolve
		// to the exact same sibling locks/ file BOTH before and after Catalogs exists.
		mixedTargetRoot := filepath.Join(tmp, "mixed-target")
		mixedUpper := filepath.Join(mixedTargetRoot, "Catalogs")
		mixedLowerSibling := filepath.Join(mixedTargetRoot, "catalogs")
		mixedRoot := filepath.Join(tmp, "mixed-cache")
		mixedAliasRoot := filepath.Join(tmp, "mixed-alias-cache")
		for _, d := range []string{mixedTargetRoot, mixedRoot, mixedAliasRoot} {
			if err := os.MkdirAll(d, 0o755); err != nil {
				t.Fatal(err)
			}
		}
		if err := os.Symlink(mixedUpper, filepath.Join(mixedRoot, "catalogs")); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(mixedUpper, filepath.Join(mixedAliasRoot, "catalogs")); err != nil {
			t.Fatal(err)
		}
		preDirect := catalogLockPath(filepath.Join(mixedRoot, "catalogs", "neuro-1234567890ab"))
		preUpper := catalogLockPath(filepath.Join(mixedUpper, "neuro-1234567890ab"))
		preLower := catalogLockPath(filepath.Join(mixedLowerSibling, "neuro-1234567890ab"))
		preAlias := catalogLockPath(filepath.Join(mixedAliasRoot, "catalogs", "neuro-1234567890ab"))
		if preDirect != preUpper || preDirect != preLower || preDirect != preAlias {
			t.Fatalf("pre-creation mixed-case aliases split lock paths: direct=%q upper=%q lower=%q alias=%q", preDirect, preUpper, preLower, preAlias)
		}
		if err := os.MkdirAll(mixedUpper, 0o755); err != nil {
			t.Fatal(err)
		}
		postDirect := catalogLockPath(filepath.Join(mixedRoot, "catalogs", "neuro-1234567890ab"))
		postUpper := catalogLockPath(filepath.Join(mixedUpper, "neuro-1234567890ab"))
		postLower := catalogLockPath(filepath.Join(mixedLowerSibling, "neuro-1234567890ab"))
		postAlias := catalogLockPath(filepath.Join(mixedAliasRoot, "catalogs", "neuro-1234567890ab"))
		if postDirect != preDirect || postDirect != postUpper || postDirect != postLower || postDirect != postAlias {
			t.Fatalf("mixed-case Catalogs aliases shifted/split lock paths: pre=%q direct=%q upper=%q lower=%q alias=%q", preDirect, postDirect, postUpper, postLower, postAlias)
		}
		if wantSuffix := filepath.Join("locks", "neuro-1234567890ab.lock"); !strings.HasSuffix(postDirect, wantSuffix) {
			t.Fatalf("expected lock in sibling locks/ dir (%q), got %q", wantSuffix, postDirect)
		}
	}

	// catalogs.json placed inside a folder named "catalogs" must serialize
	// readers and writers on the file itself across all platforms, including
	// mixed-case "catalogs" vs "Catalogs" paths on Windows.
	catNamedDir := filepath.Join(tmp, "cfg", "catalogs")
	if err := os.MkdirAll(catNamedDir, 0o755); err != nil {
		t.Fatal(err)
	}
	catNamedFile := filepath.Join(catNamedDir, "catalogs.json")
	if err := writeCatalogList(catNamedFile, sampleList, false); err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS == "windows" {
		altCaseFile := filepath.Join(tmp, "cfg", "Catalogs", "catalogs.json")
		stopAlt := make(chan struct{})
		altErrs := make(chan error, 1)
		go func() {
			for {
				select {
				case <-stopAlt:
					altErrs <- nil
					return
				default:
					if err := writeCatalogList(catNamedFile, sampleList, false); err != nil {
						altErrs <- err
						return
					}
				}
			}
		}()
		for range 100 {
			cats, err := loadCatalogList(altCaseFile)
			if err != nil || len(cats) != 1 || cats[0].Name != "neuro" {
				close(stopAlt)
				<-altErrs
				t.Fatalf("concurrent Windows mixed-case loadCatalogList failed: err=%v cats=%+v", err, cats)
			}
		}
		close(stopAlt)
		if err := <-altErrs; err != nil {
			t.Fatalf("concurrent Windows mixed-case writeCatalogList failed: %v", err)
		}
	}
}
