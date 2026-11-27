package main

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
)

// The TUI's launch flow: what starts when a button is pressed, and how its
// outcome is recorded. The processes themselves start in launch.go.

// pending is a launch clicked while its container's location was still
// being checked.
type pending struct {
	name  string
	kind  string
	entry int
}

// Messages a launch produces, each carrying its launch id.
type (
	displayMsg struct {
		id   int
		live bool
	}
	desktopMsg struct {
		cmd *exec.Cmd
		log string
		err error
	}
	desktopExitMsg struct { // display_start_command ended
		log string // its attempt's log, which names the attempt
		err error
	}
	desktopPollMsg struct{ live bool }
	startedMsg     struct {
		id  int
		cmd *exec.Cmd
		log string
		err error
	}
	exitMsg struct {
		id  int
		err error
	}
	timerMsg struct{ id int }
	shellMsg struct {
		id  int
		err error
	}
)

// launch starts a terminal session, container shell, or GUI entry for the
// selected container once it is verified on this machine.
func (m *model) launch(kind string, entry int) tea.Cmd {
	c, ok := m.selected()
	if !ok {
		return nil
	}
	if kind == "gui" && (entry < 0 || entry >= len(c.entries)) {
		return nil
	}
	switch loc := m.location; {
	case strings.HasPrefix(loc, notHere):
		return m.requestPull(c)
	case loc == checking || loc == "":
		m.waiting = &pending{name: c.name(), kind: kind, entry: entry}
		m.notice = &callout{state: "starting", detail: "Checking " + c.name() + " is on this machine…"}
		return nil
	case !strings.HasPrefix(loc, onHere):
		m.notice = &callout{state: "failed", detail: "Couldn't check " + c.name() + " is on this machine: " + strings.TrimPrefix(loc, "couldn't check: ")}
		return nil
	}
	m.closeOverlay()
	m.nextID++
	m.notice = nil
	l := launch{id: m.nextID, name: c.name(), state: "starting", at: time.Now()}
	switch kind {
	case "terminal":
		tools := m.sessionToolsFor(c)
		l.action = "Terminal"
		if len(tools) > 1 {
			l.detail = "Starting session (" + strings.Join(tools, ", ") + ")…"
		} else {
			l.detail = "Starting a shell with " + c.name() + "…"
		}
		m.launches = append([]launch{l}, m.launches...)
		argv := append([]string{"shell", "--"}, tools...)
		readyDir := ""
		if dir, err := os.MkdirTemp("", "sciget-ready-*"); err == nil {
			if os.Chmod(dir, 0o700) == nil {
				readyDir = dir
			} else {
				os.RemoveAll(dir)
			}
		}
		run := &shellRun{cmd: exec.Command(m.s.self, argv...), name: c.name(), check: m.s.launchCheck, readyDir: readyDir}
		return tea.Exec(run, func(err error) tea.Msg { return shellMsg{l.id, err} })
	case "container":
		l.action, l.detail = "Container shell", "Starting a container shell in "+c.name()+"…"
		m.launches = append([]launch{l}, m.launches...)
		run := &shellRun{cmd: exec.Command(m.s.self, "apptainer", "shell", "--", c.name()), name: c.name(), check: m.s.launchCheck}
		return tea.Exec(run, func(err error) tea.Msg { return shellMsg{l.id, err} })
	case "gui":
		e := c.entries[entry]
		l.action = "Open " + strings.TrimSuffix(e.label, "GUI")
		l.argv = append([]string{m.s.self, "apptainer", "exec", "--", c.name()}, strings.Fields(e.exec)...)
		l.detail = "Starting " + l.action[len("Open "):] + "…"
		m.launches = append([]launch{l}, m.launches...)
		display := os.Getenv("DISPLAY")
		switch {
		case display == "":
			m.finish(l.id, "failed", "No display is set")
		case m.desktopBusy:
			m.queue = append(m.queue, l.id)
		default:
			return func() tea.Msg { return displayMsg{l.id, displayLive(display)} }
		}
		return nil
	default:
		return nil
	}
}

// handleLaunch moves launches on as their messages arrive. The exit is
// always final; the timer only promotes starting to running.
func (m *model) handleLaunch(msg tea.Msg) tea.Cmd {
	switch msg := msg.(type) {
	case displayMsg:
		switch {
		case msg.live:
			return m.startGUI(msg.id)
		case m.desktopBusy:
			m.queue = append(m.queue, msg.id)
		case len(m.s.displayStart) == 0:
			m.finish(msg.id, "failed", "No display answers at "+os.Getenv("DISPLAY"))
		default:
			m.desktopBusy, m.desktopUntil, m.queue = true, time.Now().Add(m.s.displayWait), []int{msg.id}
			m.desktopLog = "" // until this attempt's log exists, no exit is its own
			m.find(msg.id).detail = "Starting the desktop…"
			argv, logs := m.s.displayStart, filepath.Join(m.s.cacheDir, "logs")
			return func() tea.Msg {
				log, err := logFile(logs, "desktop", "start")
				if err != nil {
					return desktopMsg{err: err}
				}
				defer log.Close()
				cmd, err := startDetached(argv, log)
				return desktopMsg{cmd, log.Name(), err}
			}
		}
	case desktopMsg:
		m.desktopLog = msg.log
		if msg.err != nil {
			return m.settleDesktop(false, "Couldn't start the desktop: "+msg.err.Error())
		}
		wait := func() tea.Msg { return desktopExitMsg{msg.log, msg.cmd.Wait()} } // reaps the starter whenever it ends
		return tea.Batch(wait, pollDisplay())
	case desktopExitMsg:
		// Exiting is no failure (a script may start the desktop in the
		// background and return), but an error exit ends the wait at once.
		if msg.err != nil && m.desktopBusy && msg.log == m.desktopLog { // not an earlier attempt's
			last := lastLine(m.desktopLog)
			if last == "" {
				last = "exit code " + exitCodeOf(msg.err)
			}
			return m.settleDesktop(false, "The desktop didn't start: "+last)
		}
	case desktopPollMsg:
		if !m.desktopBusy {
			return nil // settled already
		}
		switch {
		case msg.live:
			return m.settleDesktop(true, "")
		case time.Now().After(m.desktopUntil):
			return m.settleDesktop(false, fmt.Sprintf("The desktop didn't start within %d seconds", int(m.s.displayWait.Seconds())))
		}
		return pollDisplay()
	case startedMsg:
		if msg.err != nil {
			m.finish(msg.id, "failed", "Couldn't start: "+msg.err.Error())
			return nil
		}
		m.find(msg.id).log = msg.log
		return tea.Batch(
			func() tea.Msg { return exitMsg{msg.id, msg.cmd.Wait()} },
			tea.Tick(m.s.launchCheck, func(time.Time) tea.Msg { return timerMsg{msg.id} }))
	case timerMsg:
		if l := m.find(msg.id); l.state == "starting" {
			m.finish(msg.id, "running", l.action[len("Open "):]+" is running")
		}
	case exitMsg:
		l := m.find(msg.id)
		label := l.action[len("Open "):]
		if msg.err == nil {
			m.finish(msg.id, "finished", label+" finished")
			return nil
		}
		last := lastLine(l.log)
		if last == "" {
			last = "exited with code " + exitCodeOf(msg.err) + " (no output)"
		}
		m.finish(msg.id, "failed", label+" exited: "+last)
	case shellMsg:
		var quick quickFailure
		var exit *exec.ExitError
		name := m.find(msg.id).name
		switch {
		case errors.As(msg.err, &quick):
			m.finish(msg.id, "failed", fmt.Sprintf("The terminal in %s failed to start (exit code %d)", name, quick.code))
		case msg.err != nil && !errors.As(msg.err, &exit):
			m.finish(msg.id, "failed", msg.err.Error())
		default:
			m.finish(msg.id, "finished", "Back from the shell in "+name)
		}
	}
	return nil
}

func pollDisplay() tea.Cmd {
	display := os.Getenv("DISPLAY")
	return tea.Tick(500*time.Millisecond, func(time.Time) tea.Msg { return desktopPollMsg{displayLive(display)} })
}

// settleDesktop ends a desktop start: the queued launches start, or fail
// with why and the desktop start's log.
func (m *model) settleDesktop(ready bool, why string) tea.Cmd {
	queue := m.queue
	m.desktopBusy, m.queue = false, nil
	var cmds []tea.Cmd
	for _, id := range queue {
		if ready {
			cmds = append(cmds, m.startGUI(id))
		} else {
			m.find(id).log = m.desktopLog
			m.finish(id, "failed", why)
		}
	}
	return tea.Batch(cmds...)
}

// startGUI returns a command starting launch id's GUI command, detached,
// with a new log file.
func (m *model) startGUI(id int) tea.Cmd {
	l := m.find(id)
	tool, version, _ := strings.Cut(l.name, "/")
	logs, argv := filepath.Join(m.s.cacheDir, "logs"), l.argv
	return func() tea.Msg {
		log, err := logFile(logs, tool, version)
		if err != nil {
			return startedMsg{id: id, err: err}
		}
		defer log.Close()
		cmd, err := startDetached(argv, log)
		return startedMsg{id, cmd, log.Name(), err}
	}
}

// find returns launch id. Every message's id comes from a launch, so it
// is always there.
func (m *model) find(id int) *launch {
	i := slices.IndexFunc(m.launches, func(l launch) bool { return l.id == id })
	return &m.launches[i]
}

// finish records an outcome for launch id and saves the state.
func (m *model) finish(id int, state, detail string) {
	l := m.find(id)
	l.state, l.detail = state, detail
	m.save()
}

// exitCodeOf returns err's exit code as text, or "?" without one.
func exitCodeOf(err error) string {
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		return fmt.Sprint(exit.ExitCode())
	}
	return "?"
}
