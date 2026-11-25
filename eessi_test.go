package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
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
	}
	for _, tc := range badCases {
		if _, err := parseEESSICatalog([]byte(tc.json), nil); err == nil {
			t.Errorf("%s: expected error, got nil", tc.name)
		}
	}
}

func TestRefreshAndEESSICommands(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(tmp, "config"))
	t.Setenv("XDG_CACHE_HOME", filepath.Join(tmp, "cache"))
	t.Setenv("XDG_DATA_HOME", filepath.Join(tmp, "data"))
	t.Setenv("SCIGET_SYSTEM_DIR", filepath.Join(tmp, "no-system"))

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
		"module load EESSI/2025.06 && module load ParaView/6.0.1-foss-2025b",
		"sciget eessi launch --stack 2025.06 ParaView/6.0.1-foss-2025b",
	} {
		if !strings.Contains(stdout.String(), want) {
			t.Fatalf("eessi info missing %q:\n%s", want, stdout.String())
		}
	}

	// Strict EESSI launch via real bash + mock Lmod: loads exact stack & module,
	// passes tool flags without requiring `--`, and FAILS instead of falling
	// back when an explicitly selected exact module is missing.
	modRoot := filepath.Join(tmp, "modroot")
	binDir := filepath.Join(tmp, "bin")
	if err := os.MkdirAll(modRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatal(err)
	}
	mockLmod := filepath.Join(tmp, "mock-lmod.sh")
	lmodLog := filepath.Join(tmp, "lmod.log")
	if err := os.WriteFile(mockLmod, []byte(`module() {
    if [ "$1" = "purge" ]; then
        LOADEDMODULES=""
        return 0
    fi
    echo "LOAD:$2|MP:${MODULEPATH}|EXACT:${LMOD_EXACT_MATCH:-}" >> "${MOCK_LMOD_LOG}"
    if [ "$2" = "${MOCK_LMOD_FAIL:-}" ]; then
        echo "Lmod error: module not found: $2" >&2
        return 1
    fi
    if [ "$2" = "${MOCK_LMOD_ALIAS_STACK:-}" ]; then
        export LOADEDMODULES="${LOADEDMODULES:+${LOADEDMODULES}:}EESSI/2023.06"
        return 0
    fi
    if [ "${MOCK_LMOD_EMPTY_LOADED:-}" = "1" ]; then
        export LOADEDMODULES=""
        return 0
    fi
    if [ "$2" = "${MOCK_LMOD_PREFIX_TARGET:-}" ]; then
        export LOADEDMODULES="${LOADEDMODULES:+${LOADEDMODULES}:}${2}-extra"
        return 0
    fi
    export LOADEDMODULES="${LOADEDMODULES:+${LOADEDMODULES}:}$2"
    return 0
}
`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(binDir, "paraview"), []byte("#!/bin/sh\necho \"PARAVIEW:$*\"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(binDir, "gmx"), []byte("#!/bin/sh\necho \"GMX:$*\"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("MODULEPATH", "/site/other/modules")
	t.Setenv("EESSI_MODULE_ROOT", modRoot)
	t.Setenv("EESSI_LMOD_INIT", mockLmod)
	t.Setenv("MOCK_LMOD_LOG", lmodLog)
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	// Tool flags (--version, --gpu) after <tool> pass through to the binary without `--`,
	// inherited MODULEPATH is replaced by modroot, and LMOD_EXACT_MATCH=yes is enforced.
	stdout.Reset()
	stderr.Reset()
	if code := run([]string{"eessi", "launch", "--stack", "2025.06", "paraview", "--version", "--gpu"}, &stdout, &stderr); code != 0 {
		t.Fatalf("eessi launch failed (%d): %s", code, stderr.String())
	}
	if got := strings.TrimSpace(stdout.String()); got != "PARAVIEW:--version --gpu" {
		t.Fatalf("got %q, want %q", got, "PARAVIEW:--version --gpu")
	}
	logBytes, err := os.ReadFile(lmodLog)
	if err != nil {
		t.Fatal(err)
	}
	wantLog := "LOAD:EESSI/2025.06|MP:" + modRoot + "|EXACT:yes\nLOAD:ParaView/6.0.1-foss-2025b|MP:" + modRoot + "|EXACT:yes\n"
	if string(logBytes) != wantLog {
		t.Fatalf("lmod log = %q, want %q", string(logBytes), wantLog)
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
	wantFailLog := "LOAD:EESSI/2025.06|MP:" + modRoot + "|EXACT:yes\nLOAD:GROMACS/2026.2-foss-2025b-CUDA-12.9.1|MP:" + modRoot + "|EXACT:yes\n"
	if string(logBytes) != wantFailLog {
		t.Fatalf("failed launch must not attempt fallback load; log=%q want=%q", string(logBytes), wantFailLog)
	}
	t.Setenv("MOCK_LMOD_FAIL", "")

	// Library-only tool gives a clear module load hint instead of failing obscurely.
	stdout.Reset()
	stderr.Reset()
	if code := run([]string{"eessi", "launch", "biopython"}, &stdout, &stderr); code != 1 {
		t.Fatalf("expected exit 1 for library-only tool, got %d", code)
	}
	if !strings.Contains(stderr.String(), "module load EESSI/2025.06 && module load Biopython/1.86-gfbf-2025b") {
		t.Fatalf("missing module load hint in stderr: %s", stderr.String())
	}

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

	// Removing eessiCatalogUrl from the domains removes stale eessi.json on refresh.
	if err := os.WriteFile(domainsFile, []byte(`{"schemaVersion": 1, "domains": {"preclinical": {}, "molecular": {}}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	stdout.Reset()
	stderr.Reset()
	if code := run([]string{"refresh"}, &stdout, &stderr); code != 0 {
		t.Fatalf("refresh after removing eessiCatalogUrl failed (%d): %s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "updated.") {
		t.Fatalf("expected refresh to report updated when stale eessi.json removed: %s", stdout.String())
	}
	stdout.Reset()
	stderr.Reset()
	if code := run([]string{"eessi", "find", "paraview"}, &stdout, &stderr); code != 0 || stdout.Len() != 0 {
		t.Fatalf("stale eessi.json should be gone after refresh: code=%d out=%q err=%q", code, stdout.String(), stderr.String())
	}
}

func TestEESSICatalogPrecedenceAndMultiLauncher(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(tmp, "config"))
	t.Setenv("XDG_CACHE_HOME", filepath.Join(tmp, "cache"))
	t.Setenv("XDG_DATA_HOME", filepath.Join(tmp, "data"))
	t.Setenv("SCIGET_SYSTEM_DIR", filepath.Join(tmp, "no-system"))

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
	upper, err := lookupEESSIBuild(builds, "cased", "", false, "Cased")
	if err != nil || strings.Join(upper.exec, " ") != "cased --upper" {
		t.Fatalf("exact-case --launcher Cased failed: err=%v exec=%v", err, upper.exec)
	}
	lower, err := lookupEESSIBuild(builds, "cased", "", false, "cased")
	if err != nil || strings.Join(lower.exec, " ") != "cased --lower" {
		t.Fatalf("exact-case --launcher cased failed: err=%v exec=%v", err, lower.exec)
	}

	// Cross-catalog sibling launcher: if two catalogs expose the same module
	// with different launchers, info --launcher on the lower-priority catalog
	// must include --launcher in its printed Launch command.
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
	        "First": {
	          "backend": "eessi",
	          "stack": "2025.06",
	          "module": "Shared/1.0-foss-2025b",
	          "software_version": "1.0",
	          "arch": ["x86_64"],
	          "gpu": false,
	          "exec": ["shared-first"]
	        }
	      }
	    }
	  }
	}`
	cross2 := strings.ReplaceAll(strings.ReplaceAll(cross1, `"First"`, `"Second"`), `"shared-first"`, `"shared-second"`)
	b1, err := parseEESSICatalog([]byte(cross1), []string{"2025.06"})
	if err != nil {
		t.Fatal(err)
	}
	b2, err := parseEESSICatalog([]byte(cross2), []string{"2025.06"})
	if err != nil {
		t.Fatal(err)
	}
	b1[0].catalogOrder = 0
	b2[0].catalogOrder = 1
	combined := append(b1, b2...)
	if !hasSiblingLaunchers(combined, b2[0]) {
		t.Fatalf("expected hasSiblingLaunchers to detect cross-catalog sibling on same module")
	}
	if got := b2[0].launchCommand(hasSiblingLaunchers(combined, b2[0])); !strings.Contains(got, "--launcher Second") {
		t.Fatalf("cross-catalog Launch command missing --launcher Second: %s", got)
	}
}

func TestInvalidEESSIInOldCacheDoesNotBreakContainerCommands(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(tmp, "config"))
	t.Setenv("XDG_CACHE_HOME", filepath.Join(tmp, "cache"))
	t.Setenv("XDG_DATA_HOME", filepath.Join(tmp, "data"))
	t.Setenv("SCIGET_SYSTEM_DIR", filepath.Join(tmp, "no-system"))

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
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(tmp, "config"))
	t.Setenv("XDG_CACHE_HOME", filepath.Join(tmp, "cache"))
	t.Setenv("XDG_DATA_HOME", filepath.Join(tmp, "data"))
	t.Setenv("SCIGET_SYSTEM_DIR", filepath.Join(tmp, "no-system"))

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

	// Update apps.json to v2 and force eessi.json rename to fail by replacing
	// the cached eessi.json with a non-empty directory.
	if err := os.WriteFile(filepath.Join(srcDir, "apps.json"), []byte(appsV2), 0o644); err != nil {
		t.Fatal(err)
	}
	cacheRoot, used, err := usedCatalogs()
	if err != nil || len(used) != 1 {
		t.Fatalf("usedCatalogs failed: %v", err)
	}
	cacheDir := used[0].dir(cacheRoot)
	eessiPath := filepath.Join(cacheDir, "eessi.json")
	if err := os.Remove(eessiPath); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(eessiPath, "blocker"), 0o755); err != nil {
		t.Fatal(err)
	}

	stdout.Reset()
	stderr.Reset()
	if code := run([]string{"refresh"}, &stdout, &stderr); code == 0 {
		t.Fatalf("expected refresh to fail when eessi.json rename fails")
	}

	// apps.json must have been rolled back to v1 (never mixed v2 apps + v1 catalog).
	gotApps, err := os.ReadFile(filepath.Join(cacheDir, "apps.json"))
	if err != nil {
		t.Fatal(err)
	}
	// Preserve unreadable existing apps.json when staging/backup cannot read it.
	if err := os.RemoveAll(eessiPath); err != nil {
		t.Fatal(err)
	}
	appsPath := filepath.Join(cacheDir, "apps.json")
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
