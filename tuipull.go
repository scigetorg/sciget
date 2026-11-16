package main

import (
	"context"
	"errors"
	"fmt"
	"image/color"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
)

// The TUI's pulls: asking first, downloading with progress in Activity,
// cancelling, and quitting only once they have stopped. Pulls run in the
// TUI's own process, so quitting stops them.

// button is one of the details panel's buttons. Normally they launch the
// selected container; while a question is asked, or the container is
// downloading, they answer it or cancel.
type button struct {
	kind           string // "terminal", "gui", "pull", "always", "yes", "no" or "cancel"
	label, caption string
	col            color.Color
}

// buttons returns the selected container's buttons.
func (m model) buttons() []button {
	c, ok := m.selected()
	if !ok {
		return nil
	}
	if m.asking == c.name() {
		return []button{
			{kind: "always", label: "√  Always yes", caption: "download, and from now on without asking", col: m.th.accent.GetForeground()},
			{kind: "yes", label: "√  Yes", caption: "download this time", col: m.th.accent.GetForeground()},
			{kind: "no", label: "×  No", caption: "don't download", col: m.th.muted.GetForeground()},
		}
	}
	if m.pullOf(c.name()) != nil {
		return []button{{kind: "cancel", label: "×  Cancel download", caption: "stop and delete the partial file", col: m.th.err.GetForeground()}}
	}
	missing := strings.HasPrefix(m.location, notHere)
	bs := []button{{kind: "terminal", label: "›_ Open a terminal", caption: "a shell here · type exit to return", col: m.th.term}}
	for _, e := range c.entries {
		bs = append(bs, button{kind: "gui", label: "►  Open " + strings.TrimSuffix(printable(e.label), "GUI"),
			caption: "opens a window on the desktop", col: m.th.gui})
	}
	if missing {
		for i := range bs {
			bs[i].col = m.th.muted.GetForeground()
		}
		bs = append(bs, button{kind: "pull", label: "▼  Download", caption: "into the store, without opening it", col: m.th.accent.GetForeground()})
	}
	return bs
}

// press does what button i of the selected container does.
func (m *model) press(i int) tea.Cmd {
	bs := m.buttons()
	c, ok := m.selected()
	if !ok || i >= len(bs) {
		return nil
	}
	switch bs[i].kind {
	case "pull":
		return m.requestPull(c)
	case "always", "yes", "no":
		return m.answer(c, bs[i].kind)
	case "cancel":
		if p := m.pullOf(c.name()); p != nil {
			p.cancel()
			m.find(p.id).detail = "Cancelling…"
		}
		return nil
	}
	return m.launch(i)
}

// pullRun is a pull in progress.
type pullRun struct {
	id     int // its Activity entry
	name   string
	cancel context.CancelFunc
	ch     chan pullProgressMsg
}

// Messages a pull produces.
type (
	pullProgressMsg struct {
		id          int
		host        string
		done, total int64
	}
	pullDoneMsg struct {
		id       int
		loc      location
		err      error
		warnings []string
	}
)

func (m model) pullOf(name string) *pullRun {
	for _, p := range m.pulls {
		if p.name == name {
			return p
		}
	}
	return nil
}

// requestPull pulls c, asking first unless auto_pull is set. Nothing is
// launched afterwards: a download can take minutes, and the user starts
// what they want once it is done.
func (m *model) requestPull(c container) tea.Cmd {
	if auto, _ := m.s.cfg.getBool("auto_pull"); auto {
		return m.startPull(c)
	}
	where, err := pullQuestion(m.s.cfg, c)
	if err != nil {
		m.notice = &callout{state: "failed", detail: err.Error()}
		return nil
	}
	// The CLI's question, without its last line of keys: the buttons
	// answer it here.
	where, _, _ = strings.Cut(where, "\n[y]es")
	m.asking, m.btn = c.name(), 0
	m.notice = &callout{state: "asking", detail: strings.ReplaceAll(where, "\n", " ")}
	return nil
}

// answer acts on the reply to requestPull's question.
func (m *model) answer(c container, reply string) tea.Cmd {
	m.asking, m.btn, m.notice = "", 0, nil
	switch reply {
	case "no":
		return nil
	case "always":
		// A fresh copy, so a failed save leaves the TUI's settings as
		// they were.
		fresh, err := readConfig()
		if err == nil {
			err = fresh.set("auto_pull", "true")
		}
		if err != nil {
			m.notice = &callout{state: "failed", detail: "Couldn't save auto_pull: " + err.Error()}
			return nil
		}
		m.s.cfg = fresh
	}
	return m.startPull(c)
}

// startPull starts pulling c in the background, as an Activity entry.
func (m *model) startPull(c container) tea.Cmd {
	limit, err := m.s.cfg.getNumber("parallel_pulls")
	if err != nil {
		m.notice = &callout{state: "failed", detail: err.Error()}
		return nil
	}
	if len(m.pulls) >= int(limit) {
		m.notice = &callout{state: "failed", detail: fmt.Sprintf(
			"%d downloads are running (parallel_pulls); wait for one to finish", len(m.pulls))}
		return nil
	}
	store, _, _ := m.s.cfg.get("container_dir")
	m.nextID++
	id := m.nextID
	m.launches = append([]launch{{id: id, name: c.name(), action: "Download", state: "downloading",
		detail: "Downloading " + c.name() + " to " + store + "…", at: time.Now()}}, m.launches...)
	ctx, cancel := context.WithCancel(context.Background())
	p := &pullRun{id: id, name: c.name(), cancel: cancel, ch: make(chan pullProgressMsg, 1)}
	m.pulls = append(m.pulls, p)
	m.notice = nil
	cfg, ch := m.s.cfg, p.ch
	run := func() tea.Msg {
		defer close(ch)
		var warnings []string
		loc, _, err := pullContainer(ctx, cfg, c, func(host string, done, total int64) {
			// Never wait for the TUI: while a Terminal has it paused, the
			// download carries on, and the newest progress is kept.
			msg := pullProgressMsg{id, host, done, total}
			select {
			case ch <- msg:
			default:
				select {
				case <-ch:
				default:
				}
				select {
				case ch <- msg:
				default:
				}
			}
		}, func(s string) { warnings = append(warnings, s) })
		cancel()
		return pullDoneMsg{id, loc, err, warnings}
	}
	return tea.Batch(run, waitProgress(ch))
}

// waitProgress returns a command that delivers a pull's next progress.
func waitProgress(ch chan pullProgressMsg) tea.Cmd {
	return func() tea.Msg {
		if msg, ok := <-ch; ok {
			return msg
		}
		return nil
	}
}

// handlePull moves pulls on as their messages arrive.
func (m *model) handlePull(msg tea.Msg) tea.Cmd {
	switch msg := msg.(type) {
	case pullProgressMsg:
		for _, p := range m.pulls {
			if p.id == msg.id {
				l := m.find(p.id)
				store, _, _ := m.s.cfg.get("container_dir")
				percent := int64(100)
				if msg.total > 0 {
					percent = msg.done * 100 / msg.total
				}
				l.detail = fmt.Sprintf("Downloading %s from %s to %s: %s of %s (%d%%)",
					l.name, msg.host, store, humanSize(msg.done), humanSize(msg.total), percent)
				return waitProgress(p.ch)
			}
		}
	case pullDoneMsg:
		i := -1
		for j, p := range m.pulls {
			if p.id == msg.id {
				i = j
			}
		}
		if i < 0 {
			return nil
		}
		p := m.pulls[i]
		m.pulls = append(m.pulls[:i], m.pulls[i+1:]...)
		// The buttons change now, so a click half done would land on
		// another one: it is forgotten, and the user clicks again.
		m.pressed = target{}
		switch {
		case errors.Is(msg.err, context.Canceled):
			m.finish(p.id, "failed", "Download cancelled")
		case msg.err != nil:
			m.finish(p.id, "failed", "Couldn't download: "+msg.err.Error())
		default:
			detail := "Downloaded to " + msg.loc.path
			for _, w := range msg.warnings {
				detail += " · warning: " + w
			}
			m.finish(p.id, "finished", detail)
		}
		if m.quitting {
			if len(m.pulls) == 0 {
				m.save()
				return tea.Quit
			}
			return nil
		}
		if c, _ := m.selected(); c.name() == p.name {
			m.locFor = "" // looked up again, now that it may be here
		}
	}
	return nil
}

// quitOrWarn quits, unless pulls are running: then the first press warns
// and the second stops them, quitting once they have cleaned up.
func (m *model) quitOrWarn() tea.Cmd {
	if len(m.pulls) == 0 {
		return m.quit()
	}
	if m.quitting {
		return nil
	}
	if m.notice == nil || m.notice.state != "quit" {
		what := "A download is"
		if len(m.pulls) > 1 {
			what = fmt.Sprintf("%d downloads are", len(m.pulls))
		}
		m.notice = &callout{state: "quit", detail: what + " in progress. Quitting stops it; press Quit again to stop and quit."}
		return nil
	}
	m.quitting = true
	m.notice = &callout{state: "quit", detail: "Stopping downloads…"}
	for _, p := range m.pulls {
		p.cancel()
	}
	return nil
}
