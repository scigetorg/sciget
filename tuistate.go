package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"time"
)

// maxStateEntries bounds the Activity log kept between sessions.
const maxStateEntries = 50

// tuiState is what the TUI keeps between sessions: the search, the
// selection by name, and the Activity log, newest first.
type tuiState struct {
	Search   string       `json:"search"`
	Category string       `json:"category"`
	App      string       `json:"app"`
	Version  string       `json:"version"`
	Log      []stateEntry `json:"log"`
}

// stateEntry is one launch in the saved Activity log.
type stateEntry struct {
	Name    string    `json:"name"`    // "itksnap/4.4.0"
	Action  string    `json:"action"`  // "Open itksnap", "Terminal"
	Outcome string    `json:"outcome"` // "running", "finished", "failed"
	At      time.Time `json:"at"`
}

// statePath returns the state file's path in the cache directory.
func statePath(cacheDir string) string { return filepath.Join(cacheDir, "tui-state.json") }

// loadState reads the state file at path. A missing or unreadable file is
// an empty state: the state is a convenience, never a reason to fail.
func loadState(path string) tuiState {
	var st tuiState
	data, err := os.ReadFile(path)
	if err != nil {
		return tuiState{}
	}
	if err := json.Unmarshal(data, &st); err != nil {
		return tuiState{}
	}
	return st
}

// saveState writes st to path. St's log holds this session's own launches;
// it is merged with the log already there and then with restored, the
// entries this session read at start, so two TUIs on the same home keep
// each other's entries. For a launch in more than one, st's copy wins, then
// the file's: a restored entry may be out of date. St's search and
// selection win. The newest maxStateEntries entries are kept. It writes a
// private temporary file in the same directory and renames it over path,
// so a crash never leaves a half-written file.
func saveState(path string, st tuiState, restored []stateEntry) error {
	st.Log = mergeLog(st.Log, slices.Concat(loadState(path).Log, restored))
	data, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, ".tui-state-*.json")
	if err != nil {
		return err
	}
	_, err = f.Write(append(data, '\n'))
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	if err == nil {
		err = os.Rename(f.Name(), path)
	}
	if err != nil {
		os.Remove(f.Name())
	}
	return err
}

// mergeLog returns the entries of mine and theirs, newest first, without
// repeats and at most maxStateEntries of them. An entry is the same launch
// when its name, action and time match; the first copy wins, mine before
// theirs.
func mergeLog(mine, theirs []stateEntry) []stateEntry {
	type launch struct {
		name, action string
		at           int64
	}
	seen := map[launch]bool{}
	var log []stateEntry
	for _, e := range slices.Concat(mine, theirs) {
		k := launch{e.Name, e.Action, e.At.UnixNano()}
		if !seen[k] {
			seen[k] = true
			log = append(log, e)
		}
	}
	slices.SortStableFunc(log, func(a, b stateEntry) int { return b.At.Compare(a.At) })
	return log[:min(len(log), maxStateEntries)]
}
