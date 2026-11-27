package main

import (
	"errors"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"testing"

	tea "charm.land/bubbletea/v2"
)

// launchModel returns a model with itksnap selected and on this machine.
func launchModel(t *testing.T) model {
	m := testModel(t, 160, 45, tuiContainers)
	m.selectName("itksnap", "4.4.0")
	m.locFor, m.location = "itksnap/4.4.0", "On this machine (downloaded)"
	return m
}

func TestLaunch(t *testing.T) {
	t.Run("not on this machine asks first", func(t *testing.T) {
		t.Setenv("SCIGET_CONFIG", filepath.Join(t.TempDir(), "config.json"))
		m := launchModel(t)
		m.location = notHere
		if cmd := m.launch("gui", 0); cmd != nil || len(m.launches) != 0 || m.asking != "itksnap/4.4.0" ||
			m.statusFor().state != "asking" || !slices.Equal(buttonKinds(m), []string{"always", "yes", "no"}) {
			t.Errorf("command %v, launches %v, asking %q, status %+v, buttons %v", cmd != nil, m.launches, m.asking, m.statusFor(), buttonKinds(m))
		}
	})
	t.Run("a failed check starts nothing", func(t *testing.T) {
		m := launchModel(t)
		m.location = "couldn't check: permission denied"
		if cmd := m.launch("terminal", 0); cmd != nil || len(m.launches) != 0 ||
			m.statusFor().detail != "Couldn't check itksnap/4.4.0 is on this machine: permission denied" {
			t.Errorf("command %v, launches %v, status %+v", cmd != nil, m.launches, m.statusFor())
		}
	})
	t.Run("held while checking, then started", func(t *testing.T) {
		m := launchModel(t)
		m.location = checking
		if cmd := m.launch("terminal", 0); cmd != nil || len(m.launches) != 0 || m.waiting == nil {
			t.Fatalf("while checking: command %v, launches %v, waiting %v", cmd != nil, m.launches, m.waiting)
		}
		if cmd := m.handle(locationMsg{id: m.locID, text: onHere + " (downloaded)"}); cmd == nil ||
			len(m.launches) != 1 || m.launches[0].action != "Terminal" || m.waiting != nil {
			t.Errorf("once found: command %v, launches %v, waiting %v", cmd != nil, m.launches, m.waiting)
		}
	})
	t.Run("held while checking, then not here", func(t *testing.T) {
		m := launchModel(t)
		m.location = checking
		t.Setenv("SCIGET_CONFIG", filepath.Join(t.TempDir(), "config.json"))
		m.launch("terminal", 0)
		if cmd := m.handle(locationMsg{id: m.locID, text: notHere}); cmd != nil || len(m.launches) != 0 || m.asking != "itksnap/4.4.0" {
			t.Errorf("command %v, launches %v, asking %q", cmd != nil, m.launches, m.asking)
		}
	})
	t.Run("DISPLAY unset fails at once", func(t *testing.T) {
		t.Setenv("DISPLAY", "")
		m := launchModel(t)
		m.s.displayStart = []string{"start-desktop"}
		if cmd := m.launch("gui", 0); cmd != nil || m.launches[0].state != "failed" || m.launches[0].detail != "No display is set" {
			t.Errorf("command %v, launch %+v", cmd != nil, m.launches[0])
		}
	})
	t.Run("the terminal runs the session shell", func(t *testing.T) {
		m := launchModel(t)
		if cmd := m.launch("terminal", 0); cmd == nil || m.launches[0].action != "Terminal" || m.launches[0].state != "starting" {
			t.Errorf("command %v, launch %+v", cmd != nil, m.launches[0])
		}
	})
	t.Run("container shell runs apptainer shell", func(t *testing.T) {
		m := launchModel(t)
		if cmd := m.launch("container", 0); cmd == nil || m.launches[0].action != "Container shell" || m.launches[0].state != "starting" {
			t.Errorf("command %v, launch %+v", cmd != nil, m.launches[0])
		}
	})
}

func TestSettleDesktop(t *testing.T) {
	t.Run("ready", func(t *testing.T) {
		if runtime.GOOS == "windows" {
			t.Skip("X11 sockets")
		}
		x11SocketDir = t.TempDir()
		t.Cleanup(func() { x11SocketDir = "/tmp/.X11-unix" })
		t.Setenv("DISPLAY", ":7")
		m := launchModel(t)
		m.s.displayStart = []string{"start-desktop"}

		// The display does not answer: the first launch starts the desktop,
		// the second queues behind it.
		m.handle(m.launch("gui", 0)())
		if !m.desktopBusy || m.launches[0].detail != "Starting the desktop…" {
			t.Fatalf("desktop busy %v, launch %+v", m.desktopBusy, m.launches[0])
		}
		if cmd := m.launch("gui", 0); cmd != nil || !slices.Equal(m.queue, []int{1, 2}) {
			t.Fatalf("second launch: command %v, queue %v", cmd != nil, m.queue)
		}

		// The display answers: both start.
		ln, err := net.Listen("unix", filepath.Join(x11SocketDir, "X7"))
		if err != nil {
			t.Fatal(err)
		}
		defer ln.Close()
		if cmd := m.handle(pollDisplay()()); cmd == nil || m.desktopBusy || len(m.queue) != 0 {
			t.Errorf("after the display answers: command %v, busy %v, queue %v", cmd != nil, m.desktopBusy, m.queue)
		}
	})
	t.Run("never ready", func(t *testing.T) {
		m := launchModel(t)
		m.launches = []launch{{id: 1, name: "itksnap/4.4.0", action: "Open itksnap", state: "starting"}}
		m.desktopBusy, m.queue = true, []int{1}
		m.handle(desktopPollMsg{live: false}) // the deadline, zero, has passed
		if m.desktopBusy || m.launches[0].state != "failed" || m.launches[0].detail != "The desktop didn't start within 30 seconds" {
			t.Errorf("busy %v, launch %+v", m.desktopBusy, m.launches[0])
		}
	})
	t.Run("an earlier attempt's exit during a new start", func(t *testing.T) {
		m := launchModel(t)
		m.s.displayStart = []string{"start-desktop"}
		m.launches = []launch{{id: 1, name: "itksnap/4.4.0", action: "Open itksnap", state: "starting"}}
		m.desktopLog = "/logs/earlier.log"
		m.handle(displayMsg{1, false}) // starts a new attempt
		m.handle(desktopExitMsg{"/logs/earlier.log", errors.New("exit status 1")})
		if !m.desktopBusy || m.launches[0].state != "starting" {
			t.Errorf("busy %v, launch %+v, want the new attempt still waiting", m.desktopBusy, m.launches[0])
		}
	})
	t.Run("the start command fails", func(t *testing.T) {
		m := launchModel(t)
		m.launches = []launch{{id: 1, name: "itksnap/4.4.0", action: "Open itksnap", state: "starting"}}
		m.desktopBusy, m.queue = true, []int{1}
		m.desktopLog = filepath.Join(t.TempDir(), "desktop-start.log")
		if err := os.WriteFile(m.desktopLog, []byte("Fatal server error: lock file exists\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		m.handle(desktopExitMsg{m.desktopLog, nil}) // exit 0: still waiting for the display
		if !m.desktopBusy {
			t.Fatal("a clean exit ended the wait")
		}
		m.handle(desktopExitMsg{"/logs/an-earlier-attempt.log", errors.New("exit status 1")})
		if !m.desktopBusy {
			t.Fatal("an earlier attempt's exit ended the wait")
		}
		m.handle(desktopExitMsg{m.desktopLog, errors.New("exit status 1")})
		if l := m.launches[0]; m.desktopBusy || l.state != "failed" ||
			l.detail != "The desktop didn't start: Fatal server error: lock file exists" || l.log != m.desktopLog {
			t.Errorf("busy %v, launch %+v", m.desktopBusy, l)
		}
		if cmd := m.handle(desktopPollMsg{live: false}); cmd != nil {
			t.Error("polling went on after the desktop settled")
		}
	})
}

func TestHandleLaunch(t *testing.T) {
	failure := errors.New("exit status 3")
	for _, tt := range []struct {
		name       string
		action     string // "Open itksnap" when empty
		msgs       []tea.Msg
		log        string
		want       string // the final state
		wantDetail string
	}{
		{name: "error exit before the timer", msgs: []tea.Msg{exitMsg{1, failure}}, log: "one\ncannot open display\n",
			want: "failed", wantDetail: "itksnap exited: cannot open display"},
		{name: "clean exit before the timer", msgs: []tea.Msg{exitMsg{1, nil}}, want: "finished", wantDetail: "itksnap finished"},
		{name: "timer, then an error exit", msgs: []tea.Msg{timerMsg{1}, exitMsg{1, failure}}, log: "\x1b[31mlicence error\x1b[0m",
			want: "failed", wantDetail: "itksnap exited: licence error"},
		{name: "timer, then a clean exit", msgs: []tea.Msg{timerMsg{1}, exitMsg{1, nil}}, want: "finished"},
		{name: "a timer after the exit is ignored", msgs: []tea.Msg{exitMsg{1, failure}, timerMsg{1}}, want: "failed",
			wantDetail: "itksnap exited: exited with code ? (no output)"},
		{name: "timer only", msgs: []tea.Msg{timerMsg{1}}, want: "running", wantDetail: "itksnap is running"},
		{name: "back from the shell", action: "Terminal", msgs: []tea.Msg{shellMsg{1, nil}}, want: "finished",
			wantDetail: "Back from the shell in itksnap/4.4.0"},
		{name: "a shell exit code is a normal session", action: "Terminal", msgs: []tea.Msg{shellMsg{1, &exec.ExitError{}}},
			want: "finished"},
		{name: "a shell that failed to start", action: "Terminal", msgs: []tea.Msg{shellMsg{1, quickFailure{code: 255}}},
			want: "failed", wantDetail: "The terminal in itksnap/4.4.0 failed to start (exit code 255)"},
		{name: "shell, another error", action: "Terminal", msgs: []tea.Msg{shellMsg{1, errors.New("no sciget")}},
			want: "failed", wantDetail: "no sciget"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			m := launchModel(t)
			log := filepath.Join(t.TempDir(), "x.log")
			if err := os.WriteFile(log, []byte(tt.log), 0o600); err != nil {
				t.Fatal(err)
			}
			action := tt.action
			if action == "" {
				action = "Open itksnap"
			}
			m.launches = []launch{{id: 1, name: "itksnap/4.4.0", action: action, state: "starting", log: log}}
			for _, msg := range tt.msgs {
				m.handle(msg)
			}
			if got := m.launches[0]; got.state != tt.want || tt.wantDetail != "" && got.detail != tt.wantDetail {
				t.Errorf("launch = %s %q, want %s %q", got.state, got.detail, tt.want, tt.wantDetail)
			}
			if st := loadState(statePath(m.s.cacheDir)); len(st.Log) != 1 || st.Log[0].Outcome != tt.want {
				t.Errorf("saved log %+v, want one entry %s", st.Log, tt.want)
			}
		})
	}
}

// buttonKinds returns the kinds of the selected container's buttons.
func buttonKinds(m model) []string {
	var kinds []string
	for _, b := range m.buttons() {
		kinds = append(kinds, b.kind)
	}
	return kinds
}
