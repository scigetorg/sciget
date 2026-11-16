package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"
	"time"

	"charm.land/bubbles/v2/spinner"
	"charm.land/bubbles/v2/textinput"
	tea "charm.land/bubbletea/v2"
	"github.com/spf13/cobra"
)

func newUICommand() *cobra.Command {
	return &cobra.Command{
		Use:   "ui",
		Short: "Browse and launch containers in a full-screen interface",
		Args:  noArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runTUI(cmd.OutOrStdout())
		},
	}
}

// runTUI opens the TUI. Stdin and out must both be terminals. Catalogue
// problems do not stop it; they become its empty and error states.
func runTUI(out io.Writer) error {
	if !isTerminal(os.Stdin) || !isTerminal(out) {
		return errors.New("sciget ui needs a terminal")
	}
	s, err := readTUISettings()
	if err != nil {
		return err
	}
	containers, err := readContainers()
	m := newModel(s, containers, err, loadState(statePath(s.cacheDir)))
	if problems, _ := findProblems(s.cfg); len(problems) > 0 {
		m.notice = &callout{detail: fmt.Sprintf("sciget doctor found %d problem(s), such as a stopped download; run sciget doctor", len(problems))}
	}
	_, err = tea.NewProgram(m, tea.WithOutput(out)).Run()
	return err
}

// tuiSettings are the settings the TUI reads once, at start.
type tuiSettings struct {
	cfg          config
	cacheDir     string
	self         string   // this binary, which every launch runs
	displayStart []string // display_start_command
	displayWait  time.Duration
	launchCheck  time.Duration
}

func readTUISettings() (tuiSettings, error) {
	s := tuiSettings{}
	var err error
	if s.cfg, err = readConfig(); err != nil {
		return s, err
	}
	if s.cacheDir, _, err = s.cfg.get("cache_dir"); err != nil {
		return s, err
	}
	if s.displayStart, err = s.cfg.getList("display_start_command"); err != nil {
		return s, err
	}
	wait, err := s.cfg.getNumber("display_start_seconds")
	if err != nil {
		return s, err
	}
	check, err := s.cfg.getNumber("launch_check_seconds")
	if err != nil {
		return s, err
	}
	s.displayWait, s.launchCheck = time.Duration(wait)*time.Second, time.Duration(check)*time.Second
	s.self, err = os.Executable()
	return s, err
}

// control is something on screen that can take focus or a click.
type control int

// Controls, in focus-ring order; crumbControl takes clicks only.
const (
	noControl control = iota
	updateControl
	quitControl
	searchControl
	clearControl
	catsControl
	refreshControl
	appsControl
	versionsControl
	buttonsControl
	activityControl
	crumbControl
)

// launch is one action in this session's Activity, from click to outcome.
type launch struct {
	id     int
	name   string   // the container, "itksnap/4.4.0"
	action string   // "Open itksnap" or "Terminal"
	argv   []string // a GUI command, started once the display answers
	state  string   // "starting", "running", "finished" or "failed"
	detail string   // the status callout's text
	log    string   // a GUI command's log file
	at     time.Time
}

// callout is what the status callout shows. Its state is a launch's, or
// "refreshing", "refreshed", or "" for ready.
type callout struct{ state, detail, log string }

// breathe is the spinner: a dot that grows and shrinks, in symbols basic
// fonts carry.
var breathe = spinner.Spinner{Frames: []string{"·", "∙", "•", "●", "•", "∙"}, FPS: time.Second / 8}

// notHere is LOCATION for a container that is not on this machine.
const notHere = "Not on this machine"

// LOCATION while it is being found, and the start of it once found.
const (
	checking = "checking…"
	onHere   = "On this machine"
)

// model is the TUI's whole state. Selections are indexes into categories,
// apps, a version list (byTool, newest first) and the buttons.
type model struct {
	s          tuiSettings
	loadErr    error // the catalogues could not be read: an empty state
	visible    []container
	categories []string       // "" first, meaning All
	counts     map[string]int // apps per category; "" is All
	apps       []string
	byTool     map[string][]container

	search                        textinput.Model
	focus                         control
	active                        string // "software", "details", "activity" or ""
	cat, app, ver, btn, act       int
	catTop, appTop, verTop        int
	btnTop, actTop                int
	pressed                       target // where the left button went down
	width, height, hoverX, hoverY int
	dark                          bool
	th                            theme
	spin                          spinner.Model
	spinning, touched             bool

	launches []launch     // this session's, newest first
	earlier  []stateEntry // restored from earlier sessions
	notice   *callout     // shown instead of the selection's status until the selection changes
	location string       // LOCATION for locFor, looked up afresh each time it is selected
	locFor   string       // the container location belongs to
	locID    int          // the latest lookup; replies from older ones are dropped
	waiting  *pending     // a launch held until its container's location is known
	nextID   int
	btnRows  int      // the button viewport's height when last laid out
	btnFor   string   // the container btn belongs to
	start    tuiState // the state restored at start, kept while the catalogues cannot be read

	refreshing, checking bool
	updateText           string // the update check's result
	desktopBusy          bool   // display_start_command is starting the desktop
	desktopUntil         time.Time
	desktopLog           string // display_start_command's log
	queue                []int  // GUI launches waiting for the desktop
	saveReported         bool

	asking   string     // the container whose pull question the buttons show
	pulls    []*pullRun // running, oldest first
	quitting bool       // Quit was confirmed; quit once the pulls have stopped
}

func newModel(s tuiSettings, containers []container, loadErr error, st tuiState) model {
	m := model{s: s, counts: map[string]int{}, byTool: map[string][]container{},
		dark: true, th: newTheme(true), spin: spinner.New(spinner.WithSpinner(breathe)),
		focus: searchControl, hoverX: -1, hoverY: -1, earlier: st.Log, start: st}
	m.search = textinput.New()
	m.search.Prompt = " Search: "
	m.search.Placeholder = "name, version or category"
	m.search.SetStyles(m.th.input)
	// Ctrl-V would run a clipboard program; the terminal's own paste works.
	m.search.KeyMap.Paste.SetEnabled(false)
	m.search.Focus()
	m.search.SetValue(st.Search)
	m.setContainers(containers, loadErr)
	if i := slices.Index(m.categories, st.Category); i > 0 {
		m.cat = i
		m.filter()
	}
	m.selectName(st.App, st.Version)
	if m.loadErr != nil {
		m.focus = m.emptyControl()
	}
	return m
}

// setContainers shows containers, or the empty state for err, keeping the
// selection where its container still exists.
func (m *model) setContainers(containers []container, err error) {
	m.loadErr = err
	if err == nil && !slices.ContainsFunc(containers, func(c container) bool { return !c.hidden && !c.shadowed }) {
		m.loadErr = errNoContainers
	}
	tool, version := m.selectedNames()
	category := ""
	if m.cat < len(m.categories) {
		category = m.categories[m.cat]
	}
	m.visible, m.categories = nil, []string{""}
	clear(m.counts)
	counted := map[[2]string]bool{} // category and tool
	for _, c := range containers {
		if c.hidden || c.shadowed {
			continue
		}
		m.visible = append(m.visible, c)
		for _, cat := range append([]string{""}, c.categories...) {
			if m.counts[cat] == 0 && cat != "" {
				m.categories = append(m.categories, cat)
			}
			if !counted[[2]string{cat, c.tool}] {
				counted[[2]string{cat, c.tool}] = true
				m.counts[cat]++
			}
		}
	}
	slices.Sort(m.categories[1:])
	m.cat = max(0, slices.Index(m.categories, category))
	m.filter()
	m.selectName(tool, version)
}

// errNoContainers is the empty state for catalogues with nothing to show.
var errNoContainers = errors.New("the catalogues in use have no containers to show")

// filter recomputes the apps and their versions from the search and the
// category, keeping the selected app if it still matches. Counts are apps.
func (m *model) filter() {
	tool, version := m.selectedNames()
	words, category := strings.Fields(m.search.Value()), m.categories[m.cat]
	m.apps = nil
	clear(m.byTool)
	for _, c := range m.visible {
		if category != "" && !slices.Contains(c.categories, category) || !matches(c, words) {
			continue
		}
		if m.byTool[c.tool] == nil {
			m.apps = append(m.apps, c.tool)
		}
		m.byTool[c.tool] = append([]container{c}, m.byTool[c.tool]...)
	}
	m.app, m.ver, m.appTop, m.verTop = 0, 0, 0, 0
	m.selectName(tool, version)
}

// selectName selects the app tool, and its version if it is listed.
func (m *model) selectName(tool, version string) {
	if i := slices.Index(m.apps, tool); i >= 0 {
		m.app = i
		m.ver = max(0, slices.IndexFunc(m.byTool[tool], func(c container) bool { return c.version == version }))
	}
}

func (m model) selectedNames() (tool, version string) {
	c, _ := m.selected()
	return c.tool, c.version
}

func (m model) selected() (container, bool) {
	if m.app >= len(m.apps) || m.ver >= len(m.byTool[m.apps[m.app]]) {
		return container{}, false
	}
	return m.byTool[m.apps[m.app]][m.ver], true
}

// buttonCount is the selected container's buttons: the terminal first,
// then one per entry.
func (m model) buttonCount() int { return len(m.buttons()) }

// emptyControl is where focus starts in an empty state: Refresh when it
// can help, otherwise Quit.
func (m model) emptyControl() control {
	if errors.Is(m.loadErr, errNoCatalogues) || errors.Is(m.loadErr, errNoneInUse) {
		return quitControl
	}
	return refreshControl
}

func (m model) Init() tea.Cmd {
	return tea.Batch(textinput.Blink, tea.RequestBackgroundColor)
}

// Update handles msg, then looks up the selected container's location and
// keeps the spinner running while anything is in progress.
func (m model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	cmd := m.handle(msg)
	m.keepButton()
	c, _ := m.selected()
	if m.waiting != nil && c.name() != m.waiting.name {
		m.waiting = nil // the selection moved on
	}
	if m.asking != "" && c.name() != m.asking {
		m.asking = "" // so did the question
	}
	cmds := []tea.Cmd{cmd, m.lookUpLocation()}
	if busy := m.refreshing || m.checking || m.desktopBusy || m.waiting != nil || len(m.pulls) > 0 ||
		slices.ContainsFunc(m.launches, func(l launch) bool { return l.state == "starting" }); busy && !m.spinning {
		m.spinning = true
		cmds = append(cmds, m.spin.Tick)
	} else if !busy {
		m.spinning = false
	}
	m.clampScroll()
	m.search.SetWidth(max(1, m.layout().search.w-3-len(m.search.Prompt)))
	return m, tea.Batch(cmds...)
}

// keepButton keeps the selected button with its container: when another
// container is selected, by any route, the selection returns to its
// terminal button.
func (m *model) keepButton() {
	if c, _ := m.selected(); c.name() != m.btnFor {
		m.btn, m.btnTop, m.btnFor = 0, 0, c.name()
	}
	m.btn = clamp(m.btn, 0, max(0, m.buttonCount()-1)) // the buttons change with a question or a pull
}

func (m *model) handle(msg tea.Msg) tea.Cmd {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
	case tea.BackgroundColorMsg:
		m.dark = msg.IsDark()
		m.th = newTheme(m.dark)
		m.search.SetStyles(m.th.input)
	case spinner.TickMsg:
		if m.spinning {
			var cmd tea.Cmd
			m.spin, cmd = m.spin.Update(msg)
			return cmd
		}
	case tea.KeyPressMsg:
		return m.key(msg)
	case tea.PasteMsg:
		if m.loadErr == nil {
			m.touched = true
			m.setFocus(searchControl)
			return m.updateSearch(msg)
		}
	case tea.MouseMotionMsg:
		m.hoverX, m.hoverY = msg.X, msg.Y
	case tea.MouseClickMsg:
		return m.click(msg.Mouse())
	case tea.MouseReleaseMsg:
		t, pressed := m.hit(m.layout(), msg.X, msg.Y), m.pressed
		m.pressed = target{}
		if msg.Button == tea.MouseLeft && t == pressed && t.ctl != noControl {
			switch t.ctl {
			case updateControl, quitControl, clearControl, refreshControl, buttonsControl:
				return m.activate(t.ctl, t.index)
			}
		}
	case tea.MouseWheelMsg:
		step := 3
		if msg.Button == tea.MouseWheelUp {
			step = -3
		}
		l := m.layout()
		switch {
		case l.cats.has(msg.X, msg.Y):
			m.catTop += step
		case l.apps.has(msg.X, msg.Y):
			m.appTop += step
		case l.versions.has(msg.X, msg.Y):
			m.verTop += step
		case l.buttons.has(msg.X, msg.Y):
			m.btnTop += step / 3
		case l.entries.has(msg.X, msg.Y):
			m.actTop += step / 3
		}
	case locationMsg:
		if msg.id != m.locID {
			return nil
		}
		m.location = msg.text
		if w := m.waiting; w != nil && w.name == m.locFor {
			m.waiting = nil
			if c, _ := m.selected(); c.name() == w.name {
				return m.launch(w.btn)
			}
		}
	case updateMsg:
		m.checking, m.updateText = false, msg.text
	case refreshMsg:
		m.refreshing = false
		outcome := refreshOutcome(msg.results, msg.err)
		m.notice = &outcome
		if msg.readErr != nil && m.loadErr == nil {
			m.notice = &callout{state: "failed", detail: "Couldn't read the catalogues: " + msg.readErr.Error()}
			return nil
		}
		m.setContainers(msg.containers, msg.readErr)
		m.locFor = "" // looked up again, from the new catalogues
		m.locID++
		m.waiting = nil // its button may now be another entry
		if m.loadErr != nil {
			m.focus = m.emptyControl()
		}
	default:
		// The search's own messages, such as a clipboard read.
		var cmd tea.Cmd
		if m.loadErr == nil {
			cmd = m.updateSearch(msg)
		}
		return tea.Batch(cmd, m.handleLaunch(msg), m.handlePull(msg))
	}
	return nil
}

// updateSearch passes msg to the search box and refilters if its text
// changed.
func (m *model) updateSearch(msg tea.Msg) tea.Cmd {
	before := m.search.Value()
	var cmd tea.Cmd
	m.search, cmd = m.search.Update(msg)
	if m.search.Value() != before {
		m.notice = nil
		m.filter()
	}
	return cmd
}

// key handles a key press: the focus ring, moves within lists and
// buttons, and typing into the search from anywhere.
func (m *model) key(msg tea.KeyPressMsg) tea.Cmd {
	m.touched = true
	switch key := msg.String(); key {
	case "esc", "ctrl+c":
		return m.quitOrWarn()
	case "tab", "shift+tab":
		ring := m.ring()
		i := slices.Index(ring, m.focus)
		if key == "tab" {
			i = (i + 1) % len(ring)
		} else {
			i = (i - 1 + len(ring)) % len(ring)
		}
		m.setFocus(ring[i])
		return nil
	case "enter":
		return m.activate(m.focus, m.btn)
	case "up", "down":
		m.move(map[string]int{"up": -1, "down": 1}[key])
		return nil
	case "left", "right":
		if m.focus != searchControl {
			cols := []control{catsControl, appsControl, versionsControl}
			if i := slices.Index(cols, m.focus); i >= 0 {
				m.setFocus(cols[clamp(i+map[string]int{"left": -1, "right": 1}[key], 0, 2)])
			}
			return nil
		}
	}
	if m.loadErr != nil {
		return nil
	}
	if m.focus != searchControl && (msg.Text != "" || msg.String() == "backspace") {
		m.setFocus(searchControl)
	}
	if m.focus == searchControl {
		return m.updateSearch(msg)
	}
	return nil
}

// ring is the focus ring: every visible control in screen order.
func (m model) ring() []control {
	if m.loadErr != nil {
		if m.emptyControl() == refreshControl {
			return []control{updateControl, quitControl, refreshControl}
		}
		return []control{updateControl, quitControl}
	}
	ring := []control{updateControl, quitControl, searchControl}
	if m.search.Value() != "" {
		ring = append(ring, clearControl)
	}
	ring = append(ring, catsControl, refreshControl, appsControl, versionsControl)
	if m.buttonCount() > 0 {
		ring = append(ring, buttonsControl)
	}
	if m.layout().activ.w > 0 && len(m.launches)+len(m.earlier) > 0 {
		ring = append(ring, activityControl)
	}
	return ring
}

// setFocus moves the focus to c and makes c's panel the active one; the
// search has none.
func (m *model) setFocus(c control) {
	m.focus = c
	switch c {
	case searchControl:
		m.active = ""
	case catsControl, refreshControl, appsControl, versionsControl:
		m.active = "software"
	case buttonsControl:
		m.active = "details"
	case activityControl:
		m.active = "activity"
	}
	if c == searchControl {
		m.search.Focus()
	} else {
		m.search.Blur()
	}
}

// move changes the selection in the focused list by delta, keeping it in
// view.
func (m *model) move(delta int) {
	l := m.layout()
	switch m.focus {
	case catsControl:
		m.cat = clamp(m.cat+delta, 0, len(m.categories)-1)
		m.catTop = follow(m.cat, m.catTop, l.cats.h)
		m.notice = nil
		m.filter()
	case appsControl:
		if len(m.apps) == 0 {
			return
		}
		m.selectName(m.apps[clamp(m.app+delta, 0, len(m.apps)-1)], "")
		m.appTop = follow(m.app, m.appTop, l.apps.h)
		m.notice = nil
	case versionsControl:
		if tool, _ := m.selectedNames(); tool != "" {
			m.ver = clamp(m.ver+delta, 0, len(m.byTool[tool])-1)
			m.verTop = follow(m.ver, m.verTop, l.versions.h)
			m.notice = nil
		}
	case buttonsControl:
		m.btn = clamp(m.btn+delta, 0, m.buttonCount()-1)
		m.btnTop = followButton(m.btn, m.btnTop, m.buttonCount(), l.buttons.h)
	case activityControl:
		m.act = clamp(m.act+delta, 0, len(m.launches)+len(m.earlier)-1)
		m.actTop = follow(m.act, m.actTop, max(1, (l.entries.h+1)/3))
	}
}

// click acts on a press: lists, the breadcrumb, the search box and Activity
// select at once; buttons and pills wait for the release.
func (m *model) click(mouse tea.Mouse) tea.Cmd {
	if mouse.Button != tea.MouseLeft {
		return nil
	}
	l := m.layout()
	t := m.hit(l, mouse.X, mouse.Y)
	m.pressed, m.touched = t, true
	if t.panel != "" {
		m.active = t.panel
	}
	switch t.ctl {
	case searchControl:
		m.setFocus(searchControl)
	case catsControl:
		m.setFocus(catsControl)
		m.cat, m.notice = t.index, nil
		m.filter()
	case appsControl:
		m.setFocus(appsControl)
		m.selectName(m.apps[t.index], "")
		m.notice = nil
	case versionsControl:
		m.setFocus(versionsControl)
		if t.index != m.ver {
			m.ver, m.notice = t.index, nil
		}
	case buttonsControl:
		m.setFocus(buttonsControl)
		m.btn = t.index
	case activityControl:
		m.setFocus(activityControl)
		m.act = t.index
		m.jump(t.index)
	case crumbControl:
		c := l.crumbs[t.index]
		if c.all {
			m.cat = 0
			m.filter()
		}
		m.setFocus(c.ctl)
	}
	return nil
}

// activate does what the control c does: index is the button or Activity
// entry. Enter calls it for the focused control.
func (m *model) activate(c control, index int) tea.Cmd {
	switch c {
	case updateControl:
		if !m.checking {
			m.checking = true
			return checkForUpdate(m.s.cfg)
		}
	case quitControl:
		return m.quitOrWarn()
	case clearControl:
		m.search.SetValue("")
		m.filter()
		m.setFocus(searchControl)
	case refreshControl:
		if !m.refreshing {
			m.refreshing = true
			m.notice = &callout{state: "refreshing", detail: "Downloading the latest catalogues"}
			return refreshCmd
		}
	case searchControl, catsControl, appsControl, versionsControl:
		next := map[control]control{searchControl: appsControl, catsControl: appsControl,
			appsControl: versionsControl, versionsControl: buttonsControl}[c]
		if next != buttonsControl || m.buttonCount() > 0 {
			m.setFocus(next)
		}
	case buttonsControl:
		return m.press(index)
	case activityControl:
		m.jump(m.act)
	}
	return nil
}

// jump clears the search and category and selects the app and version of
// Activity entry i: this session's entries first, then earlier ones.
func (m *model) jump(i int) {
	name := ""
	if i < len(m.launches) {
		name = m.launches[i].name
	} else if i-len(m.launches) < len(m.earlier) {
		name = m.earlier[i-len(m.launches)].Name
	}
	tool, version, _ := strings.Cut(name, "/")
	m.search.SetValue("")
	m.cat, m.notice = 0, nil
	m.filter()
	m.selectName(tool, version)
	m.appTop = follow(m.app, m.appTop, m.layout().apps.h)
}

// quit saves the state and ends the program.
func (m *model) quit() tea.Cmd {
	m.save()
	return tea.Quit
}

// save writes the search, selection and Activity log to the state file. A
// failure is reported once, in the status callout.
func (m *model) save() {
	tool, version := m.selectedNames()
	st := tuiState{Search: m.search.Value(), Category: m.categories[m.cat], App: tool, Version: version}
	if m.loadErr != nil {
		// Nothing is shown, so the selection is the one restored.
		st = tuiState{Search: m.start.Search, Category: m.start.Category, App: m.start.App, Version: m.start.Version}
	}
	for _, l := range m.launches {
		st.Log = append(st.Log, stateEntry{Name: l.name, Action: l.action, Outcome: l.state, At: l.at})
	}
	if err := saveState(statePath(m.s.cacheDir), st, m.earlier); err != nil && !m.saveReported {
		m.saveReported = true
		m.notice = &callout{state: "failed", detail: "Couldn't save the TUI's state: " + err.Error()}
	}
}

// statusFor returns the status callout's content: a notice, else the
// selected container's newest launch, else Ready with a hint.
func (m model) statusFor() callout {
	if m.notice != nil {
		return *m.notice
	}
	c, _ := m.selected()
	for _, l := range m.launches {
		if l.name == c.name() {
			return callout{state: l.state, detail: l.detail, log: l.log}
		}
	}
	switch {
	case len(m.apps) == 0 && m.search.Value() != "":
		return callout{detail: "No matches · click × Clear"}
	case m.focus == buttonsControl:
		return callout{detail: "Enter to launch · Tab to move"}
	case !m.touched:
		return callout{detail: "Pick an app, then a launch button"}
	}
	return callout{}
}

// refreshOutcome describes a refresh in the status callout.
func refreshOutcome(results []refreshResult, err error) callout {
	if err != nil {
		return callout{state: "failed", detail: "Refresh failed: " + err.Error()}
	}
	var failed []refreshResult
	updated := 0
	for _, r := range results {
		if r.err != nil {
			failed = append(failed, r)
		} else if r.changed {
			updated++
		}
	}
	switch {
	case len(failed) == len(results) && len(failed) > 0:
		return callout{state: "failed", detail: "Refresh failed: " + failed[0].err.Error()}
	case len(failed) > 0:
		return callout{state: "failed", detail: fmt.Sprintf("Refreshed %d, but %s: %v",
			len(results)-len(failed), failed[0].name, failed[0].err)}
	case updated == 0:
		return callout{state: "refreshed", detail: "Catalogues up to date"}
	case updated == 1:
		return callout{state: "refreshed", detail: "Updated 1 catalogue"}
	}
	return callout{state: "refreshed", detail: fmt.Sprintf("Updated %d catalogues", updated)}
}

// locationMsg is LOCATION from lookup id.
type locationMsg struct {
	id   int
	text string
}

// lookUpLocation returns a command finding the selected container's
// location afresh, once each time it is selected: nothing is cached, so
// LOCATION shows the machine as it is now (a share that has gone away, a
// container just pulled). locate reads the disk, which may be a slow
// share, so it never runs in Update.
func (m *model) lookUpLocation() tea.Cmd {
	c, ok := m.selected()
	if !ok || c.name() == m.locFor {
		return nil
	}
	m.locFor, m.location = c.name(), checking
	m.locID++
	cfg, id := m.s.cfg, m.locID
	return func() tea.Msg { return locationMsg{id, locationText(cfg, c)} }
}

// locationText describes where c is: on this machine, missing (and why, if
// its share is not mounted), or why it could not be checked.
func locationText(cfg config, c container) string {
	loc, err := locate(cfg, c)
	var missing notOnMachineError
	switch {
	case errors.As(err, &missing):
		if _, _, catalogues, err := readCatalogues(); err == nil {
			if cat, mounted := containerShare(catalogues, c); cat.Dir != "" && !mounted {
				return fmt.Sprintf("%s: the %s share (%s) is not mounted", notHere, cat.Name, cat.Dir)
			}
		}
		return notHere
	case err != nil:
		return "couldn't check: " + err.Error()
	case loc.place == "store":
		return onHere + " (downloaded)"
	}
	return onHere + " (" + loc.place + " share)"
}

// refreshMsg is the outcome of Refresh catalogues and the re-read after it.
type refreshMsg struct {
	results    []refreshResult
	err        error
	containers []container
	readErr    error
}

func refreshCmd() tea.Msg {
	results, err := refreshCatalogues()
	containers, readErr := readContainers()
	return refreshMsg{results, err, containers, readErr}
}

// updateMsg is the update check's result, for the navbar.
type updateMsg struct{ text string }

func checkForUpdate(cfg config) tea.Cmd {
	return func() tea.Msg {
		url, _, err := cfg.get("update_url")
		if err != nil {
			return updateMsg{"couldn't check (" + err.Error() + ")"}
		}
		client, err := newHTTPClient(cfg)
		if err != nil {
			return updateMsg{"couldn't check (" + err.Error() + ")"}
		}
		client.CheckRedirect = httpsOnly
		latest, err := latestRelease(client, url)
		if err != nil {
			return updateMsg{"couldn't check (" + strings.TrimPrefix(err.Error(), "checking for updates: ") + ")"}
		}
		return updateMsg{updateResult(version, latest)}
	}
}
