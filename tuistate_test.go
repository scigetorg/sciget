package main

import (
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"testing"
	"time"
)

func TestSaveState(t *testing.T) {
	at := time.Date(2026, 11, 14, 10, 42, 0, 0, time.UTC)
	many := make([]stateEntry, maxStateEntries+5)
	for i := range many {
		many[i] = stateEntry{Name: fmt.Sprintf("app/%d", i), Action: "Terminal", Outcome: "finished", At: at}
	}
	e := func(name, outcome string) stateEntry {
		return stateEntry{Name: name, Action: "Terminal", Outcome: outcome, At: at}
	}
	for _, tt := range []struct {
		name     string
		st       tuiState
		disk     []stateEntry // already in the file
		restored []stateEntry
		wantLog  int
		want     []stateEntry // the saved log, when not st's
	}{
		{name: "round trip", st: tuiState{Search: "itk", Category: "image segmentation", App: "itksnap", Version: "4.4.0",
			Log: []stateEntry{{Name: "itksnap/4.4.0", Action: "Open itksnap", Outcome: "running", At: at}}}, wantLog: 1},
		{name: "empty", wantLog: 0},
		{name: "log capped, newest kept", st: tuiState{Log: many}, wantLog: maxStateEntries},
		{name: "this session's outcome beats the file's", st: tuiState{Log: []stateEntry{e("a", "finished")}},
			disk: []stateEntry{e("a", "running")}, wantLog: 1, want: []stateEntry{e("a", "finished")}},
		{name: "the file's outcome beats a restored one", disk: []stateEntry{e("a", "finished")},
			restored: []stateEntry{e("a", "running")}, wantLog: 1, want: []stateEntry{e("a", "finished")}},
		{name: "restored entries kept when the file lost them", restored: []stateEntry{e("a", "running")},
			wantLog: 1, want: []stateEntry{e("a", "running")}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			dir := filepath.Join(t.TempDir(), "cache")
			path := statePath(dir)
			if tt.disk != nil {
				if err := saveState(path, tuiState{Log: tt.disk}, nil); err != nil {
					t.Fatal(err)
				}
			}
			if err := saveState(path, tt.st, tt.restored); err != nil {
				t.Fatal(err)
			}
			got := loadState(path)
			if len(got.Log) != tt.wantLog {
				t.Fatalf("log has %d entries, want %d", len(got.Log), tt.wantLog)
			}
			want := tt.st
			if len(want.Log) > maxStateEntries {
				want.Log = want.Log[:maxStateEntries]
			}
			if tt.want != nil {
				want.Log = tt.want
			}
			if !reflect.DeepEqual(got, want) && !(len(got.Log) == 0 && len(want.Log) == 0) {
				t.Errorf("state = %+v, want %+v", got, want)
			}
			if info, err := os.Stat(path); err != nil {
				t.Error(err)
			} else if runtime.GOOS != "windows" && info.Mode().Perm() != 0o600 {
				t.Errorf("state file mode %v, want 0600", info.Mode())
			}
			// Only the state file remains: no temporary file left behind.
			files, err := os.ReadDir(dir)
			if err != nil || len(files) != 1 || files[0].Name() != "tui-state.json" {
				t.Errorf("files in the cache directory: %v, %v", files, err)
			}
		})
	}
}

func TestMergeLog(t *testing.T) {
	at := func(m int) time.Time { return time.Date(2026, 11, 14, 10, m, 0, 0, time.UTC) }
	e := func(name, outcome string, m int) stateEntry {
		return stateEntry{Name: name, Action: "Terminal", Outcome: outcome, At: at(m)}
	}
	many := make([]stateEntry, maxStateEntries)
	for i := range many {
		many[i] = e(fmt.Sprintf("old/%d", i), "finished", 0)
	}
	for _, tt := range []struct {
		name         string
		mine, theirs []stateEntry
		want         []string // name:outcome, newest first
	}{
		{name: "two writers interleave by time", mine: []stateEntry{e("a", "running", 3), e("a", "finished", 1)},
			theirs: []stateEntry{e("b", "failed", 2)}, want: []string{"a:running", "b:failed", "a:finished"}},
		{name: "the same launch once, mine wins", mine: []stateEntry{e("a", "finished", 1)},
			theirs: []stateEntry{e("a", "running", 1)}, want: []string{"a:finished"}},
		{name: "newest kept", mine: []stateEntry{e("new", "running", 5)}, theirs: many, want: []string{"new:running"}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			got := mergeLog(tt.mine, tt.theirs)
			var names []string
			for _, g := range got[:len(tt.want)] {
				names = append(names, g.Name+":"+g.Outcome)
			}
			if !reflect.DeepEqual(names, tt.want) || len(got) > maxStateEntries {
				t.Errorf("mergeLog() starts %v (%d entries), want %v", names, len(got), tt.want)
			}
		})
	}
}

func TestLoadState(t *testing.T) {
	for _, tt := range []struct {
		name, content string
		missing       bool
	}{
		{name: "missing file", missing: true},
		{name: "corrupt file", content: "{not json"},
		{name: "wrong shape", content: `[1, 2]`},
	} {
		t.Run(tt.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "tui-state.json")
			if !tt.missing {
				if err := os.WriteFile(path, []byte(tt.content), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			if got := loadState(path); !reflect.DeepEqual(got, tuiState{}) {
				t.Errorf("loadState() = %+v, want an empty state", got)
			}
		})
	}
}
