package main

import (
	"errors"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
)

// Keys and clicks for driving the model.
var (
	tab      = tea.KeyPressMsg{Code: tea.KeyTab}
	shiftTab = tea.KeyPressMsg{Code: tea.KeyTab, Mod: tea.ModShift}
	enter    = tea.KeyPressMsg{Code: tea.KeyEnter}
	down     = tea.KeyPressMsg{Code: tea.KeyDown}
	right    = tea.KeyPressMsg{Code: tea.KeyRight}
	esc      = tea.KeyPressMsg{Code: tea.KeyEscape}
)

func typed(r rune) tea.KeyPressMsg { return tea.KeyPressMsg{Code: r, Text: string(r)} }

func press(x, y int) tea.MouseClickMsg { return tea.MouseClickMsg{X: x, Y: y, Button: tea.MouseLeft} }

func release(x, y int) tea.MouseReleaseMsg {
	return tea.MouseReleaseMsg{X: x, Y: y, Button: tea.MouseLeft}
}

// isQuit reports whether cmd is tea.Quit, without running it: other
// commands, such as the cursor's blink, sleep.
func isQuit(cmd tea.Cmd) bool {
	return cmd != nil && reflect.ValueOf(cmd).Pointer() == reflect.ValueOf(tea.Quit).Pointer()
}

func TestRing(t *testing.T) {
	all := []control{updateControl, quitControl, searchControl, clearControl, catsControl, refreshControl,
		appsControl, versionsControl, buttonsControl, activityControl}
	for _, tt := range []struct {
		name          string
		width, height int
		search        string
		launched      bool
		want          []control
	}{
		{name: "every control", width: 160, height: 45, search: "fsl", launched: true, want: all},
		{name: "Clear hidden without text", width: 160, height: 45, launched: true,
			want: slices.DeleteFunc(slices.Clone(all), func(c control) bool { return c == clearControl })},
		{name: "Activity skipped when hidden", width: 80, height: 24, search: "fsl", launched: true, want: all[:9]},
		{name: "Activity skipped when empty", width: 160, height: 45, search: "fsl", want: all[:9]},
		{name: "buttons skipped with no match", width: 160, height: 45, search: "zzz",
			want: []control{updateControl, quitControl, searchControl, clearControl, catsControl, refreshControl,
				appsControl, versionsControl}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			m := testModel(t, tt.width, tt.height, tuiContainers)
			m.search.SetValue(tt.search)
			m.filter()
			if tt.launched {
				m.launches = []launch{{id: 1, name: "fsl/6.0.7"}}
			}
			if got := m.ring(); !reflect.DeepEqual(got, tt.want) {
				t.Errorf("ring() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestUpdate(t *testing.T) {
	for _, tt := range []struct {
		name       string
		msgs       []tea.Msg
		wantFocus  control
		wantActive string
		wantSearch string
		wantApps   []string
		wantSel    string // the selected container
		wantQuit   bool   // the last message's command quits
	}{
		{name: "starts in search", wantFocus: searchControl, wantApps: []string{"fsl", "itksnap"}, wantSel: "fsl/6.0.7"},
		{name: "typing filters", msgs: []tea.Msg{typed('i'), typed('t'), typed('k')}, wantFocus: searchControl,
			wantSearch: "itk", wantApps: []string{"itksnap"}, wantSel: "itksnap/4.4.0"},
		{name: "typing from a list goes to search", msgs: []tea.Msg{tab, tab, typed('k')},
			wantFocus: searchControl, wantSearch: "k", wantApps: []string{"itksnap"}, wantSel: "itksnap/4.4.0"},
		{name: "Tab moves to the next control and its panel", msgs: []tea.Msg{tab},
			wantFocus: catsControl, wantActive: "software", wantApps: []string{"fsl", "itksnap"}, wantSel: "fsl/6.0.7"},
		{name: "Shift-Tab wraps backwards", msgs: []tea.Msg{shiftTab, shiftTab, shiftTab},
			wantFocus: buttonsControl, wantActive: "details", wantApps: []string{"fsl", "itksnap"}, wantSel: "fsl/6.0.7"},
		{name: "Enter on search moves to Name", msgs: []tea.Msg{enter, down},
			wantFocus: appsControl, wantActive: "software", wantApps: []string{"fsl", "itksnap"}, wantSel: "itksnap/4.4.0"},
		{name: "Right moves to Version", msgs: []tea.Msg{enter, right, down},
			wantFocus: versionsControl, wantActive: "software", wantApps: []string{"fsl", "itksnap"}, wantSel: "fsl/6.0.3"},
		{name: "click a category filters", msgs: []tea.Msg{press(5, 10)},
			wantFocus: catsControl, wantActive: "software", wantApps: []string{"itksnap"}, wantSel: "itksnap/4.4.0"},
		{name: "click a version", msgs: []tea.Msg{press(60, 9)},
			wantFocus: versionsControl, wantActive: "software", wantApps: []string{"fsl", "itksnap"}, wantSel: "fsl/6.0.3"},
		{name: "click in Details makes it active", msgs: []tea.Msg{press(100, 30)},
			wantFocus: searchControl, wantActive: "details", wantApps: []string{"fsl", "itksnap"}, wantSel: "fsl/6.0.7"},
		{name: "clicking search clears the active panel", msgs: []tea.Msg{press(100, 30), press(80, 4)},
			wantFocus: searchControl, wantApps: []string{"fsl", "itksnap"}, wantSel: "fsl/6.0.7"},
		{name: "Clear acts on release", msgs: []tea.Msg{typed('k'), press(118, 4), release(118, 4)},
			wantFocus: searchControl, wantApps: []string{"fsl", "itksnap"}, wantSel: "itksnap/4.4.0"},
		{name: "Clear pressed but released elsewhere", msgs: []tea.Msg{typed('k'), press(118, 4), release(80, 4)},
			wantFocus: searchControl, wantSearch: "k", wantApps: []string{"itksnap"}, wantSel: "itksnap/4.4.0"},
		{name: "Quit on release", msgs: []tea.Msg{press(150, 1), release(150, 1)}, wantQuit: true,
			wantFocus: searchControl, wantApps: []string{"fsl", "itksnap"}, wantSel: "fsl/6.0.7"},
		{name: "Quit needs its own press", msgs: []tea.Msg{press(80, 4), release(150, 1)},
			wantFocus: searchControl, wantApps: []string{"fsl", "itksnap"}, wantSel: "fsl/6.0.7"},
		{name: "pasting from a list goes to search", msgs: []tea.Msg{tab, tea.PasteMsg{Content: "itk"}},
			wantFocus: searchControl, wantSearch: "itk", wantApps: []string{"itksnap"}, wantSel: "itksnap/4.4.0"},
		{name: "Esc quits", msgs: []tea.Msg{esc}, wantQuit: true,
			wantFocus: searchControl, wantApps: []string{"fsl", "itksnap"}, wantSel: "fsl/6.0.7"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			m := testModel(t, 160, 45, tuiContainers)
			var cmd tea.Cmd
			for _, msg := range tt.msgs {
				cmd = m.handle(msg)
			}
			c, _ := m.selected()
			if m.focus != tt.wantFocus || m.active != tt.wantActive || m.search.Value() != tt.wantSearch ||
				!slices.Equal(m.apps, tt.wantApps) || c.name() != tt.wantSel {
				t.Errorf("focus %v, active %q, search %q, apps %v, selected %s", m.focus, m.active, m.search.Value(), m.apps, c.name())
			}
			if isQuit(cmd) != tt.wantQuit {
				t.Errorf("quits: %v, want %v", !tt.wantQuit, tt.wantQuit)
			}
		})
	}
}

func TestActivate(t *testing.T) {
	m := testModel(t, 160, 45, tuiContainers)
	if m.activate(refreshControl, 0) == nil || !m.refreshing || m.activate(refreshControl, 0) != nil {
		t.Error("Refresh: want one refresh, further presses ignored")
	}
	if m.activate(updateControl, 0) == nil || !m.checking || m.activate(updateControl, 0) != nil {
		t.Error("Check for updates: want one check, further presses ignored")
	}
	for from, to := range map[control]control{searchControl: appsControl, catsControl: appsControl,
		appsControl: versionsControl, versionsControl: buttonsControl} {
		m.activate(from, 0)
		if m.focus != to {
			t.Errorf("Enter on %v: focus %v, want %v", from, m.focus, to)
		}
	}
}

func TestKeepButton(t *testing.T) {
	m := testModel(t, 160, 45, tuiContainers)
	m.selectName("itksnap", "4.4.0")
	m.keepButton()
	m.btn, m.btnTop = 1, 1
	for _, tt := range []struct {
		name    string
		change  func()
		wantBtn int
	}{
		{name: "same container", change: func() { m.search.SetValue("itk"); m.filter() }, wantBtn: 1},
		{name: "search selects another", change: func() { m.search.SetValue("fsl"); m.filter() }},
		{name: "another version", change: func() { m.search.SetValue(""); m.filter(); m.selectName("fsl", "6.0.3") }},
		{name: "through Activity", change: func() { m.launches = []launch{{name: "itksnap/4.4.0"}}; m.jump(0) }},
	} {
		t.Run(tt.name, func(t *testing.T) {
			tt.change()
			m.keepButton()
			if m.btn != tt.wantBtn {
				t.Errorf("button %d, want %d", m.btn, tt.wantBtn)
			}
			m.btn, m.btnTop = 1, 1 // for the next row
		})
	}
}

func TestJump(t *testing.T) {
	m := testModel(t, 160, 45, tuiContainers)
	m.search.SetValue("itk")
	m.cat = 2
	m.filter()
	m.launches = []launch{{id: 1, name: "fsl/6.0.3", action: "Terminal"}}
	l := m.layout()
	m.handle(press(l.entries.x+1, l.entries.y))
	if c, _ := m.selected(); c.name() != "fsl/6.0.3" || m.search.Value() != "" || m.cat != 0 || m.active != "activity" {
		t.Errorf("selected %s, search %q, category %d, active %q", c.name(), m.search.Value(), m.cat, m.active)
	}
}

func TestStatusFor(t *testing.T) {
	m := launchModel(t)
	m.launches = []launch{
		{id: 2, name: "itksnap/4.4.0", action: "Open itksnap", state: "starting", detail: "Starting itksnap…"},
		{id: 1, name: "itksnap/4.4.0", action: "Open itksnap", state: "running"},
	}
	m.handle(exitMsg{1, errors.New("exit status 1")})
	if st := m.statusFor(); st.state != "starting" || m.launches[1].state != "failed" {
		t.Errorf("status %+v, older launch %+v", st, m.launches[1])
	}
}

func TestRefreshOutcome(t *testing.T) {
	failed := errors.New("timeout")
	for _, tt := range []struct {
		name    string
		results []refreshResult
		err     error
		want    callout
	}{
		{name: "up to date", results: []refreshResult{{name: "a"}}, want: callout{state: "refreshed", detail: "Catalogs up to date"}},
		{name: "updated", results: []refreshResult{{name: "a", changed: true}, {name: "b", changed: true}},
			want: callout{state: "refreshed", detail: "Updated 2 catalogs"}},
		{name: "partial", results: []refreshResult{{name: "a", changed: true}, {name: "b", err: failed}},
			want: callout{state: "failed", detail: "Refreshed 1, but b: timeout"}},
		{name: "all failed", results: []refreshResult{{name: "a", err: failed}},
			want: callout{state: "failed", detail: "Refresh failed: timeout"}},
		{name: "none in use", err: errNoneInUse, want: callout{state: "failed", detail: "Refresh failed: " + errNoneInUse.Error()}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if got := refreshOutcome(tt.results, tt.err); got != tt.want {
				t.Errorf("refreshOutcome() = %+v, want %+v", got, tt.want)
			}
		})
	}
}

func TestRefreshMsg(t *testing.T) {
	t.Run("selection kept, location looked up again", func(t *testing.T) {
		m := launchModel(t)
		m.handle(refreshMsg{results: []refreshResult{{name: "neuro"}}, containers: tuiContainers})
		if c, _ := m.selected(); c.name() != "itksnap/4.4.0" || m.locFor != "" || m.refreshing {
			t.Errorf("selected %s, locFor %q, refreshing %v", c.name(), m.locFor, m.refreshing)
		}
	})
	t.Run("a location from before the refresh is dropped", func(t *testing.T) {
		m := launchModel(t)
		old := m.locID
		m.handle(refreshMsg{results: []refreshResult{{name: "neuro"}}, containers: tuiContainers})
		m.handle(locationMsg{id: old, text: notHere})
		if m.location == notHere {
			t.Errorf("location %q, want the old reply dropped", m.location)
		}
	})
	t.Run("a held launch is cancelled", func(t *testing.T) {
		m := launchModel(t)
		m.location = checking
		m.launch(1)
		m.handle(refreshMsg{results: []refreshResult{{name: "neuro"}}, containers: tuiContainers})
		if cmd := m.handle(locationMsg{id: m.locID, text: onHere + " (downloaded)"}); cmd != nil || len(m.launches) != 0 {
			t.Errorf("command %v, launches %v, want nothing started", cmd != nil, m.launches)
		}
	})
	t.Run("a failed re-read keeps the list", func(t *testing.T) {
		m := launchModel(t)
		m.handle(refreshMsg{results: []refreshResult{{name: "neuro"}}, readErr: errors.New("broken")})
		if len(m.apps) != 2 || m.statusFor().detail != "Couldn't read the catalogs: broken" {
			t.Errorf("apps %v, status %+v", m.apps, m.statusFor())
		}
	})
	t.Run("an empty state recovers", func(t *testing.T) {
		m := newModel(tuiSettings{cacheDir: t.TempDir()}, nil, &catalogError{name: "neuro", err: &notDownloadedError{}}, tuiState{})
		m.handle(tea.WindowSizeMsg{Width: 160, Height: 45})
		next, _ := m.Update(refreshMsg{results: []refreshResult{{name: "neuro", changed: true}}, containers: tuiContainers})
		m = next.(model)
		if m.loadErr != nil || len(m.apps) != 2 || m.search.Width() < 20 {
			t.Errorf("loadErr %v, apps %v, search width %d", m.loadErr, m.apps, m.search.Width())
		}
	})
}

// TestRefreshCmd refreshes from a server that accepts the connection and
// never answers: the network timeout ends the refresh with an error.
func TestRefreshCmd(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			defer conn.Close() // held open, silent, until the test ends
		}
	}()
	base := t.TempDir()
	t.Setenv("SCIGET_CONFIG", filepath.Join(base, "config.json"))
	if err := writeConfig(os.Getenv("SCIGET_CONFIG"), map[string]string{
		"cache_dir": filepath.Join(base, "cache"), "network_timeout_seconds": "1"}); err != nil {
		t.Fatal(err)
	}
	path, err := catalogListPath()
	if err != nil {
		t.Fatal(err)
	}
	cat := catalog{Name: "neuro", URL: "https://" + ln.Addr().String() + "/apps.json", Use: true}
	if err := writeCatalogList(path, []catalog{cat}); err != nil {
		t.Fatal(err)
	}
	// An earlier copy, so the re-read after the refresh succeeds.
	dir := cat.dir(filepath.Join(base, "cache"))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "apps.json"), []byte(`{"fsl": {"apps": {"fsl 1": {"version": "1"}}}}`), 0o600); err != nil {
		t.Fatal(err)
	}

	m := testModel(t, 160, 45, tuiContainers)
	cmd := m.activate(refreshControl, 0)
	done := make(chan tea.Msg)
	go func() { done <- cmd() }()
	select {
	case msg := <-done:
		next, _ := m.Update(msg)
		m = next.(model)
	case <-time.After(10 * time.Second):
		t.Fatal("refresh still running after 10 seconds: no timeout")
	}
	if st := m.statusFor(); m.refreshing || m.spinning || st.state != "failed" || !strings.Contains(st.detail, "Timeout") {
		t.Errorf("refreshing %v, spinning %v, status %+v", m.refreshing, m.spinning, st)
	}
}

func TestRestoreState(t *testing.T) {
	st := tuiState{Search: "i", Category: "segmentation", App: "itksnap", Version: "4.4.0",
		Log: []stateEntry{{Name: "fsl/6.0.3", Action: "Terminal", Outcome: "finished"}}}
	m := newModel(tuiSettings{cacheDir: t.TempDir()}, tuiContainers, nil, st)
	if c, _ := m.selected(); c.name() != "itksnap/4.4.0" || m.categories[m.cat] != "segmentation" || len(m.earlier) != 1 {
		t.Errorf("selected %s, category %q, earlier %v", c.name(), m.categories[m.cat], m.earlier)
	}
	gone := newModel(tuiSettings{cacheDir: t.TempDir()}, tuiContainers, nil, tuiState{Category: "gone", App: "gone"})
	if c, _ := gone.selected(); c.name() != "fsl/6.0.7" || gone.cat != 0 {
		t.Errorf("missing names: selected %s, category %d", c.name(), gone.cat)
	}
	if m.search.KeyMap.Paste.Enabled() {
		t.Error("Ctrl-V would run a clipboard program")
	}

	// Quitting while the catalogs cannot be read keeps the selection.
	dir := t.TempDir()
	empty := newModel(tuiSettings{cacheDir: dir}, nil, &catalogError{name: "neuro", err: &notDownloadedError{}}, st)
	empty.save()
	if got := loadState(statePath(dir)); got.App != "itksnap" || got.Version != "4.4.0" ||
		got.Category != "segmentation" || got.Search != "i" || len(got.Log) != 1 {
		t.Errorf("saved from an empty state: %+v", got)
	}
}

func TestModuleText(t *testing.T) {
	tests := []struct {
		name       string
		onShare    bool
		buildDate  string // the module's build; no module when empty
		modulePath bool   // module_dir/neuro is in MODULEPATH
		want       string // {modules} is module_dir
	}{
		{name: "share", onShare: true, want: "the neuro share's own modules"},
		{name: "no module", want: "none · Install makes one"},
		{name: "module", buildDate: "20200905", modulePath: true, want: "module load fsl/6.0.3"},
		{name: "module folder not in MODULEPATH", buildDate: "20200905",
			want: "module load fsl/6.0.3 · first: module use {modules}/neuro"},
		{name: "module of an older build", buildDate: "20190101", modulePath: true,
			want: "module load fsl/6.0.3 (from an older build · Install updates it)"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c, cat, store := localCatalog(t)
			base := filepath.Dir(store)
			moduleDir := filepath.Join(base, "modules")
			if err := writeConfig(os.Getenv("SCIGET_CONFIG"), map[string]string{
				"cache_dir": filepath.Join(base, "cache"), "container_dir": store, "module_dir": moduleDir,
			}); err != nil {
				t.Fatal(err)
			}
			cfg, err := readConfig()
			if err != nil {
				t.Fatal(err)
			}
			if tt.onShare {
				layout, _ := defaultValue("container_layout")
				makeContainerPath(t, cat.Dir, layout, c, false)
			}
			if tt.buildDate != "" {
				built := c
				built.buildDate = tt.buildDate
				if _, err := writeModule(cfg, built, commandList{Found: []string{"bet"}}); err != nil {
					t.Fatal(err)
				}
			}
			t.Setenv("MODULEPATH", "")
			if tt.modulePath {
				t.Setenv("MODULEPATH", filepath.Join(moduleDir, "neuro"))
			}
			if got, want := moduleText(cfg, c), strings.ReplaceAll(tt.want, "{modules}", moduleDir); got != want {
				t.Errorf("moduleText = %q, want %q", got, want)
			}
		})
	}
}
