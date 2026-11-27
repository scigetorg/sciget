package main

import (
	"errors"
	"slices"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
)

func TestHelpOverlay(t *testing.T) {
	t.Run("? opens help when search empty, types into search when non-empty", func(t *testing.T) {
		m := testModel(t, 160, 45, tuiContainers)
		m.handle(typed('?'))
		if m.overlay != overlayHelp {
			t.Fatalf("overlay = %v, want overlayHelp", m.overlay)
		}
		// Esc closes overlay without quitting.
		if cmd := m.handle(esc); isQuit(cmd) || m.overlay != overlayNone {
			t.Fatalf("Esc inside overlay: quit=%v overlay=%v", isQuit(cmd), m.overlay)
		}

		// With text in search, typing ? appends to search instead of opening help.
		m.handle(typed('f'))
		m.handle(typed('?'))
		if m.overlay != overlayNone || m.search.Value() != "f?" {
			t.Errorf("search with text: overlay=%v search=%q", m.overlay, m.search.Value())
		}

		// F1 always opens help even when search has text.
		m.handle(tea.KeyPressMsg{Code: tea.KeyF1})
		if m.overlay != overlayHelp {
			t.Errorf("F1 did not open help: overlay=%v", m.overlay)
		}
	})

	t.Run("help pill click, scroll, outside click closes, renders within bounds", func(t *testing.T) {
		for _, size := range [][2]int{{160, 45}, {80, 24}} {
			m := testModel(t, size[0], size[1], tuiContainers)
			l := m.layout()
			m.handle(press(l.help.x+1, l.help.y+1))
			m.handle(release(l.help.x+1, l.help.y+1))
			if m.overlay != overlayHelp {
				t.Fatalf("%dx%d: clicking Help pill did not open help", size[0], size[1])
			}
			screen := escapeSequence.ReplaceAllString(m.View().Content, "")
			lines := strings.Split(screen, "\n")
			if len(lines) > size[1] {
				t.Errorf("%dx%d: %d lines > %d", size[0], size[1], len(lines), size[1])
			}
			for i, line := range lines {
				if w := lipgloss.Width(line); w > size[0] {
					t.Errorf("%dx%d: line %d width %d > %d", size[0], size[1], i, w, size[0])
				}
			}
			if !strings.Contains(screen, "Navigation") {
				t.Errorf("%dx%d: help screen missing Navigation heading", size[0], size[1])
			}

			// Scroll down and verify lower sections become reachable, then return to top.
			m.handle(down)
			if m.overlayScroll != 1 {
				t.Errorf("%dx%d: scroll after down = %d, want 1", size[0], size[1], m.overlayScroll)
			}
			m.handle(tea.KeyPressMsg{Code: tea.KeyPgDown})
			scrolled := escapeSequence.ReplaceAllString(m.View().Content, "")
			if !strings.Contains(screen+scrolled, "sciget add") {
				t.Errorf("%dx%d: help screen missing sciget add even after scrolling", size[0], size[1])
			}
			m.handle(tea.KeyPressMsg{Code: tea.KeyHome})
			if m.overlayScroll != 0 {
				t.Errorf("%dx%d: scroll after home = %d, want 0", size[0], size[1], m.overlayScroll)
			}

			// Clicking outside the overlay closes it without passing the click through.
			m.handle(press(0, 0))
			if m.overlay != overlayNone {
				t.Errorf("%dx%d: outside click did not close help", size[0], size[1])
			}
		}
	})
}

func TestAboutOverlay(t *testing.T) {
	containers := []container{{
		tool: "fsl", version: "6.0.7", buildDate: "20240101", image: "fsl_6.0.7",
		categories: []string{"imaging"}, catalog: "neuro",
		licences: []string{"FSL"}, dois: []string{"10.1016/j.neuroimage.2011.09.015"},
	}}
	m := testModel(t, 120, 40, containers)
	m.locFor, m.location, m.module = "fsl/6.0.7", onHere+" (downloaded)", "module load fsl/6.0.7"

	// Open About via the button.
	aboutIdx := slices.Index(buttonKinds(m), "about")
	if aboutIdx < 0 {
		t.Fatalf("no about button in %v", buttonKinds(m))
	}
	cmd := m.press(aboutIdx)
	if m.overlay != overlayAbout || !m.aboutLoading || cmd == nil {
		t.Fatalf("openAbout: overlay=%v loading=%v cmd=%v", m.overlay, m.aboutLoading, cmd != nil)
	}

	// Stale aboutMsg is dropped.
	m.handle(aboutMsg{id: m.aboutID - 1, name: "fsl/6.0.7", err: errors.New("stale")})
	if !m.aboutLoading || m.aboutErr != "" {
		t.Fatal("stale aboutMsg was not ignored")
	}

	// Matching aboutMsg populates commands and sanitized README.
	m.handle(aboutMsg{
		id:   m.aboutID,
		name: "fsl/6.0.7",
		list: commandList{
			Found: []string{"bet", "fslmaths", "bash"},
			Help:  "# FSL\nLine two\x1b[31m red\x1b[0m\n",
		},
	})
	if m.aboutLoading || m.aboutList == nil {
		t.Fatalf("after aboutMsg: loading=%v list=%v", m.aboutLoading, m.aboutList)
	}
	screen := escapeSequence.ReplaceAllString(m.View().Content, "")
	for _, want := range []string{
		"About · fsl/6.0.7",
		"https://doi.org/10.1016/j.neuroimage.2011.09.015",
		"sciget shell fsl/6.0.7",
		"bet fslmaths",
		"# FSL",
		"Line two red",
	} {
		if !strings.Contains(screen, want) {
			t.Errorf("About screen missing %q:\n%s", want, screen)
		}
	}

	// Missing container skips finder and shows explanation.
	m.closeOverlay()
	m.aboutFor = ""
	m.location = notHere
	if cmd := m.openAbout(); cmd != nil || m.aboutLoading {
		t.Errorf("missing container started finder: cmd=%v loading=%v", cmd != nil, m.aboutLoading)
	}
	screen = escapeSequence.ReplaceAllString(m.View().Content, "")
	if !strings.Contains(screen, "Not on this machine yet") {
		t.Errorf("missing container About screen lacks notice:\n%s", screen)
	}
}

func TestSessionStagingTUI(t *testing.T) {
	m := launchModel(t) // itksnap/4.4.0 selected & on machine
	stageIdx := slices.Index(buttonKinds(m), "stage")
	if stageIdx < 0 {
		t.Fatalf("missing stage button when on machine: %v", buttonKinds(m))
	}
	m.press(stageIdx)
	if !slices.Equal(m.staged, []string{"itksnap/4.4.0"}) {
		t.Fatalf("staged = %v, want [itksnap/4.4.0]", m.staged)
	}

	// Switch to fsl/6.0.7: Open a terminal label should reflect (+1) staged tool.
	m.selectName("fsl", "6.0.7")
	m.locFor, m.location = "fsl/6.0.7", onHere+" (downloaded)"
	bs := m.buttons()
	if bs[0].label != "›_ Open a terminal (+1)" || !strings.Contains(bs[0].caption, "itksnap/4.4.0") {
		t.Errorf("terminal button = %+v, want (+1) with itksnap", bs[0])
	}
	if tools := m.sessionToolsFor(container{tool: "fsl", version: "6.0.7"}); !slices.Equal(tools, []string{"fsl/6.0.7", "itksnap/4.4.0"}) {
		t.Errorf("sessionToolsFor = %v", tools)
	}

	// Stage fsl/6.0.7, then stage fsl/6.0.3 -> replaces fsl version in place.
	m.press(slices.Index(buttonKinds(m), "stage"))
	m.selectName("fsl", "6.0.3")
	m.locFor, m.location = "fsl/6.0.3", onHere+" (downloaded)"
	m.press(slices.Index(buttonKinds(m), "stage"))
	if !slices.Equal(m.staged, []string{"itksnap/4.4.0", "fsl/6.0.3"}) {
		t.Errorf("staged after version switch = %v", m.staged)
	}

	// Session bar and [× clear] appear in layout & ring and render within bounds.
	l := m.layout()
	if l.sessionClear.w == 0 || !slices.Contains(m.ring(), sessionClearControl) {
		t.Errorf("sessionClear rect=%v ring=%v", l.sessionClear, m.ring())
	}
	screen := escapeSequence.ReplaceAllString(m.View().Content, "")
	if !strings.Contains(screen, "Session (2):") || !strings.Contains(screen, "[× clear]") {
		t.Errorf("screen missing session bar:\n%s", screen)
	}

	// Refresh dropping fsl/6.0.3 removes it from staged list with a notice.
	reduced := []container{tuiContainers[4]} // only itksnap/4.4.0
	m.handle(refreshMsg{results: []refreshResult{{name: "neuro", changed: true}}, containers: reduced})
	if !slices.Equal(m.staged, []string{"itksnap/4.4.0"}) || !strings.Contains(m.statusFor().detail, "fsl/6.0.3") {
		t.Errorf("after refresh: staged=%v status=%+v", m.staged, m.statusFor())
	}

	// Clicking [× clear] clears staged list.
	l = m.layout()
	m.handle(press(l.sessionClear.x+1, l.sessionClear.y))
	m.handle(release(l.sessionClear.x+1, l.sessionClear.y))
	if len(m.staged) != 0 {
		t.Errorf("staged after clear = %v, want empty", m.staged)
	}

	// Resize between press and release clears pressed target.
	m.handle(press(l.quit.x+1, l.quit.y+1))
	m.handle(tea.WindowSizeMsg{Width: 140, Height: 40})
	if cmd := m.handle(release(l.quit.x+1, l.quit.y+1)); isQuit(cmd) {
		t.Error("resize between press and release should not activate Quit")
	}

	// Location change clears in-flight button press so shifted buttons can't misfire.
	m.pressed = target{ctl: buttonsControl, index: 1, panel: "details"}
	m.handle(locationMsg{id: m.locID, text: onHere + " (neuro share)", module: "none"})
	if m.pressed != (target{}) {
		t.Errorf("pressed not cleared after location change: %+v", m.pressed)
	}
}

func TestCompact80x24ButtonScrolling(t *testing.T) {
	m := testModel(t, 80, 24, tuiContainers)
	m.selectName("itksnap", "4.4.0")
	m.keepButton()
	m.locFor, m.location = "itksnap/4.4.0", onHere+" (downloaded)"
	m.setFocus(buttonsControl)

	count := m.buttonCount()
	if count < 4 {
		t.Fatalf("expected at least 4 buttons, got %d (%v)", count, buttonKinds(m))
	}
	// Navigate down to the very last button (About & commands) via Update so clampScroll runs each step.
	for i := 0; i < count-1; i++ {
		next, _ := m.Update(down)
		m = next.(model)
	}
	if m.btn != count-1 {
		t.Fatalf("btn = %d, want %d", m.btn, count-1)
	}
	if m.btnTop != count-1 {
		t.Errorf("at 80x24, btnTop = %d after clamping, want %d (last button must stay in view)", m.btnTop, count-1)
	}
	screen := escapeSequence.ReplaceAllString(m.View().Content, "")
	if !strings.Contains(screen, "About & commands") {
		t.Errorf("last button not visible at 80x24:\n%s", screen)
	}
}

func TestAboutCancelAndRefreshAndTooSmall(t *testing.T) {
	m := launchModel(t)
	// Start loading About, then close immediately before reply arrives.
	aboutIdx := slices.Index(buttonKinds(m), "about")
	m.press(aboutIdx)
	cancelledID := m.aboutID
	m.closeOverlay()
	// Late cancellation error must be dropped and NOT cached.
	m.handle(aboutMsg{id: cancelledID, name: "itksnap/4.4.0", err: errors.New("context canceled")})
	if m.aboutErr != "" {
		t.Fatalf("cancelled error was cached: %q", m.aboutErr)
	}
	// Reopening retries cleanly.
	if cmd := m.openAbout(); cmd == nil || !m.aboutLoading {
		t.Fatalf("reopen after cancel did not retry: cmd=%v loading=%v", cmd != nil, m.aboutLoading)
	}

	// Refresh while About is open cancels the old request and starts a new one.
	oldID := m.aboutID
	cmd := m.handle(refreshMsg{results: []refreshResult{{name: "neuro", changed: true}}, containers: tuiContainers})
	if cmd == nil || m.aboutID == oldID || !m.aboutLoading {
		t.Errorf("refresh while About open did not restart load: cmd=%v oldID=%d newID=%d loading=%v",
			cmd != nil, oldID, m.aboutID, m.aboutLoading)
	}

	// If About was opened while not on machine, arriving locationMsg (onHere) starts loading.
	m.cancelAbout()
	m.aboutList = nil
	m.aboutErr = ""
	m.location = notHere
	if cmd := m.handle(locationMsg{id: m.locID, text: onHere + " (downloaded)", module: "none"}); cmd == nil || !m.aboutLoading {
		t.Errorf("locationMsg(onHere) while About open did not trigger load: cmd=%v loading=%v", cmd != nil, m.aboutLoading)
	}

	// In tooSmall state, Esc quits even if an overlay was open.
	m.handle(tea.WindowSizeMsg{Width: 60, Height: 20})
	if q := m.handle(esc); !isQuit(q) {
		t.Error("Esc in tooSmall view did not quit while overlay was open")
	}

	// UTF-8 safe truncation never splits a multi-byte rune.
	s, cut := truncateUTF8("aéé", 2)
	if !cut || s != "a" {
		t.Errorf("truncateUTF8(%q, 2) = %q, %v; want %q, true", "aéé", s, cut, "a")
	}
}
