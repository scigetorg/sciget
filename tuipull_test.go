package main

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
)

// pullModel returns launchModel's model with itksnap not on this machine
// and its config file at a temporary path.
func pullModel(t *testing.T) model {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.json")
	t.Setenv("SCIGET_CONFIG", path)
	m := launchModel(t)
	m.location = notHere
	m.s.cfg = config{path: path, overrides: map[string]string{}}
	return m
}

func TestTUIPull(t *testing.T) {
	t.Run("Download button when not here", func(t *testing.T) {
		m := pullModel(t)
		if kinds := buttonKinds(m); kinds[len(kinds)-1] != "pull" {
			t.Errorf("buttons %v, want Download last", kinds)
		}
	})
	t.Run("no", func(t *testing.T) {
		m := pullModel(t)
		m.launch(0)
		if cmd := m.press(2); cmd != nil || m.asking != "" || len(m.pulls) != 0 || buttonKinds(m)[0] != "terminal" {
			t.Errorf("command %v, asking %q, pulls %d, buttons %v", cmd != nil, m.asking, len(m.pulls), buttonKinds(m))
		}
	})
	t.Run("yes starts a download, and nothing launches after", func(t *testing.T) {
		m := pullModel(t)
		m.launch(1)
		if cmd := m.press(1); cmd == nil || len(m.pulls) != 1 || m.launches[0].action != "Download" ||
			!slices.Equal(buttonKinds(m), []string{"cancel"}) {
			t.Fatalf("command %v, pulls %d, launches %v, buttons %v", cmd != nil, len(m.pulls), m.launches, buttonKinds(m))
		}
		if auto, _ := m.s.cfg.getBool("auto_pull"); auto {
			t.Error("yes saved auto_pull")
		}
		m.handle(pullDoneMsg{id: m.pulls[0].id, loc: location{path: "/store/x.simg", place: "store"}})
		if len(m.pulls) != 0 || m.launches[0].state != "finished" || m.waiting != nil || m.locFor != "" {
			t.Errorf("pulls %d, launch %+v, waiting %+v, locFor %q", len(m.pulls), m.launches[0], m.waiting, m.locFor)
		}
	})
	t.Run("always saves auto_pull", func(t *testing.T) {
		m := pullModel(t)
		m.launch(0)
		m.press(0)
		if auto, _ := m.s.cfg.getBool("auto_pull"); !auto || len(m.pulls) != 1 {
			t.Errorf("auto_pull %v, pulls %d", auto, len(m.pulls))
		}
		if saved, err := loadConfig(m.s.cfg.path); err != nil || saved.overrides["auto_pull"] != "true" {
			t.Errorf("saved %v, %v", saved.overrides, err)
		}
	})
	t.Run("a click half done when the download ends is forgotten", func(t *testing.T) {
		m := pullModel(t)
		m.s.cfg.overrides["auto_pull"] = "true"
		m.launch(0)
		m.pressed = target{ctl: buttonsControl, index: 0} // pressed on Cancel
		m.handle(pullDoneMsg{id: m.pulls[0].id, loc: location{path: "/x", place: "store"}})
		if m.pressed != (target{}) {
			t.Errorf("pressed %+v, want it forgotten", m.pressed)
		}
	})
	t.Run("a failed save changes nothing", func(t *testing.T) {
		m := pullModel(t)
		// A read-only config file can be read but not saved.
		if err := os.WriteFile(m.s.cfg.path, []byte("{}"), 0o400); err != nil {
			t.Fatal(err)
		}
		m.launch(0)
		m.press(0)
		if auto, _ := m.s.cfg.getBool("auto_pull"); auto || len(m.pulls) != 0 || !strings.HasPrefix(m.statusFor().detail, "Couldn't save auto_pull") {
			t.Errorf("auto_pull %v, pulls %d, status %+v", auto, len(m.pulls), m.statusFor())
		}
		m.launch(0)
		if m.asking == "" {
			t.Error("the next launch did not ask")
		}
	})
	t.Run("parallel_pulls is a limit", func(t *testing.T) {
		m := pullModel(t)
		m.s.cfg.overrides["parallel_pulls"] = "1"
		m.pulls = []*pullRun{{name: "other/1", cancel: func() {}}}
		m.s.cfg.overrides["auto_pull"] = "true"
		if cmd := m.launch(0); cmd != nil || len(m.pulls) != 1 || !strings.Contains(m.statusFor().detail, "wait for one to finish") {
			t.Errorf("command %v, pulls %d, status %+v", cmd != nil, len(m.pulls), m.statusFor())
		}
	})
	t.Run("quit asks, then stops the pulls and quits", func(t *testing.T) {
		m := pullModel(t)
		m.s.cfg.overrides["auto_pull"] = "true"
		m.launch(0)
		cancelled := false
		m.pulls[0].cancel = func() { cancelled = true }
		if cmd := m.quitOrWarn(); cmd != nil || m.statusFor().state != "quit" || m.quitting {
			t.Fatalf("first press: command %v, status %+v", cmd != nil, m.statusFor())
		}
		if cmd := m.quitOrWarn(); cmd != nil || !m.quitting || !cancelled {
			t.Fatalf("second press: command %v, quitting %v, cancelled %v", cmd != nil, m.quitting, cancelled)
		}
		cmd := m.handle(pullDoneMsg{id: m.pulls[0].id, err: context.Canceled})
		if cmd == nil {
			t.Fatal("no quit once the pull stopped")
		}
		if _, ok := cmd().(tea.QuitMsg); !ok || m.launches[0].detail != "Download cancelled" {
			t.Errorf("message %T, launch %+v", cmd(), m.launches[0])
		}
	})
	t.Run("progress never waits for the TUI", func(t *testing.T) {
		ch := make(chan pullProgressMsg, 1)
		ch <- pullProgressMsg{done: 1}
		// What the pull's progress function does when the channel is full.
		msg := pullProgressMsg{done: 2}
		select {
		case ch <- msg:
		default:
			<-ch
			ch <- msg
		}
		if got := <-ch; got.done != 2 {
			t.Errorf("kept %+v, want the newest", got)
		}
	})
}
