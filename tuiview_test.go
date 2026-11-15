package main

import (
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
)

// tuiContainers are what the TUI tests browse: fsl in two versions, and
// itksnap with one GUI entry. Hidden and shadowed containers are not shown.
var tuiContainers = []container{
	{tool: "fsl", version: "6.0.3", categories: []string{"imaging"}, catalogue: "neuro"},
	{tool: "fsl", version: "6.0.7", categories: []string{"imaging"}, catalogue: "neuro"},
	{tool: "fsl", version: "6.0.7", categories: []string{"imaging"}, catalogue: "other", shadowed: true},
	{tool: "hidden", version: "1", hidden: true},
	{tool: "itksnap", version: "4.4.0", categories: []string{"imaging", "segmentation"}, catalogue: "neuro",
		entries: []entry{{key: "itksnapGUI-itksnap 4.4.0", label: "itksnapGUI", exec: "itksnap"}}},
}

// testModel returns a TUI model of width by height cells showing
// containers, with its state in a temporary cache directory.
func testModel(t *testing.T, width, height int, containers []container) model {
	t.Helper()
	s := tuiSettings{cacheDir: t.TempDir(), self: "/sciget", launchCheck: 5 * time.Second, displayWait: 30 * time.Second}
	m := newModel(s, containers, nil, tuiState{})
	m.handle(tea.WindowSizeMsg{Width: width, Height: height})
	return m
}

func TestLayout(t *testing.T) {
	for _, tt := range []struct {
		name          string
		width, height int
		stacked       bool
		tooSmall      bool
		want          map[string]rect
	}{
		{name: "wide: side by side, Activity shown", width: 160, height: 45, want: map[string]rect{
			"software": {0, 6, 68, 39}, "details": {69, 6, 45, 39}, "activ": {115, 6, 45, 39},
			"search": {69, 3, 45, 3}, "clear": {115, 3, 12, 3},
			"cats": {1, 8, 28, 32}, "apps": {30, 8, 24, 36}, "versions": {55, 8, 12, 36},
			"refresh": {2, 41, 26, 3}, "fields": {72, 8, 39, 6}, "buttons": {72, 16, 39, 28},
			"callout": {118, 8, 39, 2}, "entries": {118, 11, 39, 33}}},
		{name: "medium: side by side, Activity hidden", width: 120, height: 40, want: map[string]rect{
			"software": {0, 6, 68, 34}, "details": {69, 6, 51, 34},
			"search": {69, 3, 38, 3}, "clear": {108, 3, 12, 3},
			"buttons": {72, 16, 45, 20}, "callout": {72, 37, 45, 2}}},
		{name: "compact: stacked at 80 by 24", width: 80, height: 24, stacked: true, want: map[string]rect{
			"software": {0, 6, 36, 18}, "details": {37, 6, 43, 18},
			"search": {37, 3, 30, 3}, "clear": {68, 3, 12, 3},
			"cats": {1, 8, 34, 2}, "refresh": {2, 11, 26, 3}, "apps": {1, 15, 34, 5}, "versions": {1, 21, 34, 2},
			"fields": {40, 8, 37, 6}, "buttons": {40, 16, 37, 4}, "callout": {40, 21, 37, 2}}},
		{name: "undersized", width: 79, height: 24, tooSmall: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			l := testModel(t, tt.width, tt.height, tuiContainers).layout()
			if l.stacked != tt.stacked || l.tooSmall != tt.tooSmall {
				t.Errorf("stacked %v, tooSmall %v, want %v, %v", l.stacked, l.tooSmall, tt.stacked, tt.tooSmall)
			}
			got := map[string]rect{"software": l.software, "details": l.details, "activ": l.activ,
				"search": l.search, "clear": l.clear, "cats": l.cats, "apps": l.apps, "versions": l.versions,
				"refresh": l.refresh, "fields": l.fields, "buttons": l.buttons, "callout": l.callout, "entries": l.entries}
			for name, want := range tt.want {
				if got[name] != want {
					t.Errorf("%s = %v, want %v", name, got[name], want)
				}
			}
		})
	}
}

func TestHit(t *testing.T) {
	wide, compact := []int{160, 45}, []int{80, 24}
	for _, tt := range []struct {
		name   string
		size   []int
		search string
		x, y   int
		want   target
	}{
		{name: "Quit", size: wide, x: 150, y: 1, want: target{ctl: quitControl}},
		{name: "Check for updates", size: wide, x: 125, y: 0, want: target{ctl: updateControl}},
		{name: "search box", size: wide, x: 80, y: 4, want: target{ctl: searchControl}},
		{name: "Clear hidden without text", size: wide, x: 118, y: 4},
		{name: "Clear with text", size: wide, search: "fsl", x: 118, y: 4, want: target{ctl: clearControl}},
		{name: "breadcrumb All", size: wide, x: 3, y: 4, want: target{ctl: crumbControl}},
		{name: "breadcrumb version", size: wide, x: 15, y: 4, want: target{ctl: crumbControl, index: 2}},
		{name: "header row", size: wide, x: 5, y: 7, want: target{panel: "software"}},
		{name: "category row", size: wide, x: 5, y: 9, want: target{ctl: catsControl, index: 1, panel: "software"}},
		{name: "divider", size: wide, x: 29, y: 9, want: target{panel: "software"}},
		{name: "app row", size: wide, x: 35, y: 9, want: target{ctl: appsControl, index: 1, panel: "software"}},
		{name: "past the apps", size: wide, x: 35, y: 10, want: target{panel: "software"}},
		{name: "version row", size: wide, x: 60, y: 8, want: target{ctl: versionsControl, panel: "software"}},
		{name: "Refresh", size: wide, x: 10, y: 42, want: target{ctl: refreshControl, panel: "software"}},
		{name: "gap between panels", size: wide, x: 68, y: 10},
		{name: "terminal button", size: wide, x: 75, y: 17, want: target{ctl: buttonsControl, panel: "details"}},
		{name: "beside a button", size: wide, x: 105, y: 17, want: target{panel: "details"}},
		{name: "only one button for fsl", size: wide, x: 75, y: 21, want: target{panel: "details"}},
		{name: "Activity, no entries", size: wide, x: 120, y: 11, want: target{panel: "activity"}},
		{name: "compact Category", size: compact, x: 5, y: 8, want: target{ctl: catsControl, panel: "software"}},
		{name: "compact Refresh", size: compact, x: 5, y: 12, want: target{ctl: refreshControl, panel: "software"}},
		{name: "compact Name header", size: compact, x: 5, y: 14, want: target{panel: "software"}},
		{name: "compact Name", size: compact, x: 5, y: 16, want: target{ctl: appsControl, index: 1, panel: "software"}},
		{name: "compact Version", size: compact, x: 5, y: 22, want: target{ctl: versionsControl, index: 1, panel: "software"}},
		{name: "compact button", size: compact, x: 45, y: 16, want: target{ctl: buttonsControl, panel: "details"}},
		{name: "undersized", size: []int{79, 24}, x: 5, y: 1},
	} {
		t.Run(tt.name, func(t *testing.T) {
			m := testModel(t, tt.size[0], tt.size[1], tuiContainers)
			m.search.SetValue(tt.search)
			if got := m.hit(m.layout(), tt.x, tt.y); got != tt.want {
				t.Errorf("hit(%d, %d) = %+v, want %+v", tt.x, tt.y, got, tt.want)
			}
		})
	}
}

func TestHitActivity(t *testing.T) {
	m := testModel(t, 160, 45, tuiContainers)
	m.launches = []launch{{id: 2, name: "itksnap/4.4.0"}, {id: 1, name: "fsl/6.0.3"}}
	m.earlier = []stateEntry{{Name: "fsl/6.0.7"}}
	l := m.layout()
	for _, tt := range []struct {
		name string
		y    int
		want target
	}{
		{name: "first entry", y: l.entries.y, want: target{ctl: activityControl, panel: "activity"}},
		{name: "its second row", y: l.entries.y + 1, want: target{ctl: activityControl, panel: "activity"}},
		{name: "blank row", y: l.entries.y + 2, want: target{panel: "activity"}},
		{name: "earlier entry", y: l.entries.y + 6, want: target{ctl: activityControl, index: 2, panel: "activity"}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if got := m.hit(l, l.entries.x+1, tt.y); got != tt.want {
				t.Errorf("hit = %+v, want %+v", got, tt.want)
			}
		})
	}
	m.actTop = 1 // scrolled: the top entry is the second
	if got := m.hit(l, l.entries.x+1, l.entries.y); got.index != 1 {
		t.Errorf("scrolled hit = %+v, want index 1", got)
	}
}

func TestButtonView(t *testing.T) {
	for _, tt := range []struct {
		name                string
		top, count, rows    int
		wantFirst, wantN    int
		wantAbove, wantBelw bool
	}{
		{name: "all fit", count: 2, rows: 7, wantN: 2},
		{name: "one, then more below", count: 3, rows: 4, wantN: 1, wantBelw: true},
		{name: "scrolled to the last", top: 2, count: 3, rows: 4, wantFirst: 2, wantN: 1, wantAbove: true},
		{name: "scrolled, more both sides", top: 1, count: 3, rows: 5, wantFirst: 1, wantN: 1, wantAbove: true, wantBelw: true},
		{name: "offset past the end", top: 9, count: 3, rows: 12, wantFirst: 2, wantN: 1, wantAbove: true},
		{name: "too short for both lines: the button wins", top: 1, count: 3, rows: 4, wantFirst: 1, wantN: 1, wantAbove: true},
		{name: "too short for either line", top: 1, count: 3, rows: 3, wantFirst: 1, wantN: 1},
		{name: "no buttons", rows: 4},
	} {
		t.Run(tt.name, func(t *testing.T) {
			first, n, above, below := buttonView(tt.top, tt.count, tt.rows)
			if first != tt.wantFirst || n != tt.wantN || above != tt.wantAbove || below != tt.wantBelw {
				t.Errorf("buttonView() = %d, %d, %v, %v", first, n, above, below)
			}
		})
	}
}

func TestFollowButton(t *testing.T) {
	for _, tt := range []struct {
		name                  string
		btn, top, count, rows int
		want                  int
	}{
		{name: "in view", btn: 0, count: 3, rows: 4},
		{name: "below", btn: 1, count: 3, rows: 4, want: 1},
		{name: "last, more above", btn: 2, count: 3, rows: 4, want: 2},
		{name: "above", btn: 0, top: 2, count: 3, rows: 4},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if got := followButton(tt.btn, tt.top, tt.count, tt.rows); got != tt.want {
				t.Errorf("followButton() = %d, want %d", got, tt.want)
			}
		})
	}
}

func TestClampScroll(t *testing.T) {
	m := testModel(t, 80, 24, tuiContainers)
	m.selectName("itksnap", "4.4.0")
	m.clampScroll()
	m.btnTop, m.appTop = 1, 5
	m.clampScroll()
	if m.btnTop != 1 || m.appTop != 0 {
		t.Errorf("at 80 by 24: btnTop %d, appTop %d, want 1, 0", m.btnTop, m.appTop)
	}
	m.handle(tea.WindowSizeMsg{Width: 160, Height: 45}) // room for both buttons
	m.clampScroll()
	if m.btnTop != 0 {
		t.Errorf("after a resize: btnTop %d, want 0", m.btnTop)
	}
	m.btn = 1
	m.handle(tea.WindowSizeMsg{Width: 80, Height: 24}) // room for one: the selected one stays in view
	m.clampScroll()
	if m.btnTop != 1 {
		t.Errorf("after shrinking: btnTop %d, want 1", m.btnTop)
	}
}

func TestCrumbs(t *testing.T) {
	m := testModel(t, 160, 45, tuiContainers)
	m.cat = 2 // segmentation
	m.filter()
	for _, tt := range []struct {
		name string
		room int
		want string
	}{
		{name: "all fit", room: 60, want: "All › segmentation › itksnap › 4.4.0"},
		{name: "All gives way", room: 34, want: "… › segmentation › itksnap › 4.4.0"},
		{name: "the category gives way", room: 30, want: "… › itksnap › 4.4.0"},
		{name: "the version is cut last", room: 6, want: "… › 4…"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			var texts []string
			for _, c := range m.crumbs(tt.room) {
				texts = append(texts, c.text)
			}
			if got := strings.Join(texts, " › "); got != tt.want {
				t.Errorf("crumbs(%d) = %q, want %q", tt.room, got, tt.want)
			}
		})
	}
}

func TestCalloutText(t *testing.T) {
	m := testModel(t, 160, 45, tuiContainers)
	long := strings.Repeat("word ", 40)
	for _, tt := range []struct {
		name   string
		notice callout
		rows   int
		want   []string
	}{
		{name: "ready", rows: 7, want: []string{"• Ready", "Pick an app, then a…"}},
		{name: "running, one line", notice: callout{state: "running", detail: long}, rows: 7,
			want: []string{"√ Running", "word word word word…"}},
		{name: "failure wraps and ends with the log", notice: callout{state: "failed", detail: long, log: "/cache/logs/x.log"},
			rows: 7, want: []string{"× Failed", "word word word word", "word word word word", "word word word word",
				"word word word word", "word word word word…", "/cache/logs/x.log"}},
		{name: "failure in two rows", notice: callout{state: "failed", detail: "short"}, rows: 2,
			want: []string{"× Failed", "short"}},
		{name: "escape sequences removed", notice: callout{state: "failed", detail: "bad\x1b[2J one"}, rows: 7,
			want: []string{"× Failed", "bad one"}},
		{name: "a long log path wraps whole", notice: callout{state: "failed", detail: "short", log: "/home/jo/.cache/sciget/logs/itksnap.log"},
			rows: 7, want: []string{"× Failed", "short", "/home/jo/.cache/scig", "et/logs/itksnap.log"}},
		{name: "no room for the whole path: its end", notice: callout{state: "failed", detail: "short", log: "/home/jo/.cache/sciget/logs/itksnap.log"},
			rows: 3, want: []string{"× Failed", "short", "…et/logs/itksnap.log"}},
		{name: "wide characters wrap by width", notice: callout{state: "failed", detail: "short", log: "/data/数据数据数据数据数据/x.log"},
			rows: 7, want: []string{"× Failed", "short", "/data/数据数据数据数", "据数据/x.log"}},
		{name: "wide characters cut by width", notice: callout{state: "failed", detail: "short", log: "/data/数据数据数据数据数据数据数据数据/x.log"},
			rows: 3, want: []string{"× Failed", "short", "…数据数据数据/x.log"}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			m.notice = nil
			if tt.notice.detail != "" {
				m.notice = &tt.notice
			}
			if got := m.calloutText(24, tt.rows); !reflect.DeepEqual(got, tt.want) {
				t.Errorf("calloutText() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestEmptyMessage(t *testing.T) {
	for _, tt := range []struct {
		name string
		err  error
		want string
		ctl  control
	}{
		{name: "no catalogues", err: errNoCatalogues, want: "No catalogues yet.", ctl: quitControl},
		{name: "none in use", err: errNoneInUse, want: "No catalogues are in use.", ctl: quitControl},
		{name: "not downloaded", err: &catalogueError{name: "neuro", err: &notDownloadedError{path: "/x"}},
			want: "The catalogue neuro hasn't been downloaded yet.", ctl: refreshControl},
		{name: "broken", err: &catalogueError{name: "neuro", err: errNoContainers},
			want: "The catalogue neuro couldn't be read: ", ctl: refreshControl},
		{name: "no containers", err: errNoContainers, want: "The catalogues in use have no containers", ctl: refreshControl},
		{name: "escape sequences removed", err: errors.New("build dates \x1b[31m2026\x1b[0m and \x1b]8;;https://x\x07y\x1b]8;;\x07 differ"),
			want: "build dates 2026 and y differ", ctl: refreshControl},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if got := emptyMessage(tt.err); !strings.HasPrefix(got, tt.want) {
				t.Errorf("emptyMessage() = %q, want it to start %q", got, tt.want)
			}
			s := tuiSettings{cacheDir: t.TempDir()}
			if m := newModel(s, nil, tt.err, tuiState{}); m.focus != tt.ctl || m.ring()[len(m.ring())-1] != tt.ctl {
				t.Errorf("focus %v, ring %v, want both to end on %v", m.focus, m.ring(), tt.ctl)
			}
		})
	}
}

func TestView(t *testing.T) {
	for _, tt := range []struct {
		name          string
		width, height int
		loadErr       error
		setup         func(m *model)
		want, lacks   []string
	}{
		{name: "wide", width: 160, height: 45, want: []string{"SCIGET", "Software (2)", "Details", "Activity", "Open a terminal"}},
		{name: "medium", width: 120, height: 40, want: []string{"Software (2)", "Details", "• Ready"}, lacks: []string{"Activity"}},
		{name: "compact", width: 80, height: 24, want: []string{"Category ─", "Name ─", "Version ─", "Open a terminal"}},
		{name: "compact, buttons scroll", width: 80, height: 24, setup: func(m *model) { m.selectName("itksnap", "4.4.0") },
			want: []string{"▼ 1 more"}},
		{name: "undersized", width: 60, height: 20, want: []string{"at least 80 by 24"}},
		{name: "empty state", width: 100, height: 30, loadErr: &catalogueError{name: "neuro", err: &notDownloadedError{}},
			want: []string{"hasn't been downloaded yet", "Refresh catalogues"}, lacks: []string{"Details"}},
		{name: "launches and a wrapped failure", width: 160, height: 45, setup: func(m *model) {
			m.launches = []launch{{id: 1, name: "fsl/6.0.7", action: "Terminal", state: "failed",
				detail: strings.Repeat("error ", 30), log: "/cache/logs/fsl-6.0.7-1.log"}}
			m.earlier = []stateEntry{{Name: "itksnap/4.4.0", Action: "Open itksnap"}}
		}, want: []string{"× Failed", "fsl-6.0.7-1.log", "Terminal · error", "itksnap/4.4.0"}},
		{name: "update result and hint shortened", width: 100, height: 30, setup: func(m *model) {
			m.updateText = "↑ v0.2.0 available · see github.com/scigetorg/sciget/releases"
		}, want: []string{"↑ v0.2.0 available"}, lacks: []string{"see github"}},
		{name: "a long failure is clipped", width: 100, height: 30, setup: func(m *model) {
			m.updateText = "couldn't check (" + strings.Repeat("x", 80) + ")"
		}, want: []string{"sciget dev · couldn't check (xxx", "…"}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			s := tuiSettings{cacheDir: t.TempDir()}
			var tm tea.Model = newModel(s, tuiContainers, tt.loadErr, tuiState{})
			tm, _ = tm.Update(tea.WindowSizeMsg{Width: tt.width, Height: tt.height})
			m := tm.(model)
			if tt.setup != nil {
				tt.setup(&m)
			}
			screen := escapeSequence.ReplaceAllString(m.View().Content, "")
			lines := strings.Split(screen, "\n")
			if len(lines) > tt.height {
				t.Errorf("%d lines, want at most %d", len(lines), tt.height)
			}
			for i, line := range lines {
				if w := lipgloss.Width(line); w > tt.width {
					t.Errorf("line %d is %d cells wide: %q", i, w, line)
				}
			}
			for _, s := range tt.want {
				if !strings.Contains(screen, s) {
					t.Errorf("screen lacks %q:\n%s", s, screen)
				}
			}
			for _, s := range tt.lacks {
				if strings.Contains(screen, s) {
					t.Errorf("screen has %q", s)
				}
			}
		})
	}
}
