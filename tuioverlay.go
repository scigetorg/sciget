package main

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
)

// overlayKind names the modal overlay currently open over the TUI.
type overlayKind int

const (
	overlayNone overlayKind = iota
	overlayHelp
	overlayAbout
)

// maxReadmeBytes caps the README text loaded into the About viewer.
const maxReadmeBytes = 64 << 10

// aboutMsg delivers the asynchronous command list and README for a container.
type aboutMsg struct {
	id   int
	name string
	list commandList
	err  error
}

// openHelp opens the scrollable TUI Help overlay.
func (m *model) openHelp() {
	m.cancelAbout()
	m.overlay = overlayHelp
	m.overlayScroll = 0
	m.pressed = target{}
}

// openAbout opens the per-container About & commands overlay and starts an
// asynchronous finder/cache load when the container is on this machine.
func (m *model) openAbout() tea.Cmd {
	c, ok := m.selected()
	if !ok {
		return nil
	}
	m.overlay = overlayAbout
	m.overlayScroll = 0
	m.pressed = target{}
	if m.aboutFor == c.name() && !m.aboutLoading && m.aboutList != nil {
		return nil
	}
	m.cancelAbout()
	m.aboutFor = c.name()
	m.aboutList = nil
	m.aboutErr = ""
	if strings.HasPrefix(m.location, notHere) {
		return nil
	}
	m.aboutID++
	id := m.aboutID
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	m.aboutCancel = cancel
	m.aboutLoading = true
	cfg := m.s.cfg
	return func() tea.Msg {
		defer cancel()
		loc, err := locate(cfg, c)
		if err != nil {
			return aboutMsg{id: id, name: c.name(), err: err}
		}
		list, err := listCommands(ctx, cfg, c, loc)
		return aboutMsg{id: id, name: c.name(), list: list, err: err}
	}
}

func (m *model) cancelAbout() {
	if m.aboutCancel != nil {
		m.aboutCancel()
		m.aboutCancel = nil
	}
	if m.aboutLoading {
		m.aboutID++
		m.aboutLoading = false
	}
}

func (m *model) closeOverlay() {
	m.cancelAbout()
	m.overlay = overlayNone
	m.overlayScroll = 0
	m.pressed = target{}
}

// overlayRect returns the centered modal rectangle for the current screen size.
func (m model) overlayRect() rect {
	ow := min(max(40, m.width-4), 80)
	oh := min(max(14, m.height-4), 36)
	return rect{(m.width - ow) / 2, (m.height - oh) / 2, ow, oh}
}

func (m model) overlayBodyHeight(r rect) int {
	// Top border, blank margin, footer rule, footer line, bottom border.
	return max(1, r.h-4)
}

func (m model) overlayCloseRect(r rect) rect {
	const closeW = 9 // "× Close" + padding
	return rect{r.x + r.w - closeW - 2, r.y, closeW, 1}
}

// handleOverlay routes keyboard and mouse events while an overlay is open.
// Returns handled=true for input events consumed by the modal.
func (m *model) handleOverlay(msg tea.Msg) (handled bool, cmd tea.Cmd) {
	if m.overlay == overlayNone || m.layout().tooSmall {
		return false, nil
	}
	scrollBy := func(delta int, abs int) {
		r := m.overlayRect()
		bodyH := m.overlayBodyHeight(r)
		maxScroll := max(0, len(m.overlayLines(r.w-4))-bodyH)
		switch {
		case abs == 0:
			m.overlayScroll = 0
		case abs > 0:
			m.overlayScroll = maxScroll
		default:
			m.overlayScroll = clamp(m.overlayScroll+delta, 0, maxScroll)
		}
	}
	switch msg := msg.(type) {
	case tea.KeyPressMsg:
		switch msg.String() {
		case "esc", "enter", "q", "f1":
			m.closeOverlay()
		case "?":
			if m.overlay == overlayHelp {
				m.closeOverlay()
			}
		case "up", "k":
			scrollBy(-1, -1)
		case "down", "j":
			scrollBy(1, -1)
		case "pgup", "b":
			bodyH := m.overlayBodyHeight(m.overlayRect())
			scrollBy(-max(1, bodyH-1), -1)
		case "pgdown", " ", "f":
			bodyH := m.overlayBodyHeight(m.overlayRect())
			scrollBy(max(1, bodyH-1), -1)
		case "home", "g":
			scrollBy(0, 0)
		case "end", "G":
			scrollBy(0, 1)
		case "ctrl+c":
			m.closeOverlay()
			return true, m.quitOrWarn()
		}
		return true, nil
	case tea.PasteMsg:
		return true, nil
	case tea.MouseWheelMsg:
		step := 3
		if msg.Button == tea.MouseWheelUp {
			step = -3
		}
		scrollBy(step, -1)
		return true, nil
	case tea.MouseClickMsg:
		if msg.Button == tea.MouseLeft {
			r := m.overlayRect()
			if !r.has(msg.X, msg.Y) || m.overlayCloseRect(r).has(msg.X, msg.Y) {
				m.closeOverlay()
			}
		}
		return true, nil
	case tea.MouseReleaseMsg:
		return true, nil
	}
	return false, nil
}

func (m model) overlayTitle() string {
	if m.overlay == overlayHelp {
		return "Help"
	}
	if c, ok := m.selected(); ok {
		return "About · " + printable(c.name())
	}
	return "About"
}

type overlayLine struct {
	text  string
	style string // "", "heading", "muted", "accent", "err"
}

func (m model) overlayLines(width int) []overlayLine {
	if m.overlay == overlayHelp {
		return m.helpOverlayLines(width)
	}
	return m.aboutOverlayLines(width)
}

func appendWrapped(dst []overlayLine, text, style string, width int) []overlayLine {
	if width <= 0 {
		return dst
	}
	for _, line := range wrapAll(text, width) {
		dst = append(dst, overlayLine{text: line, style: style})
	}
	return dst
}

// wrapAll word-wraps s into lines of at most width cells without truncation.
func wrapAll(s string, width int) []string {
	if width <= 0 {
		return nil
	}
	words := strings.Fields(s)
	if len(words) == 0 {
		return []string{""}
	}
	var out []string
	line := ""
	for _, w := range words {
		if lipgloss.Width(w) > width {
			if line != "" {
				out = append(out, line)
				line = ""
			}
			chunks := chop(w, width)
			out = append(out, chunks[:len(chunks)-1]...)
			line = chunks[len(chunks)-1]
			continue
		}
		if line != "" && lipgloss.Width(line+" "+w) > width {
			out = append(out, line)
			line = w
		} else if line == "" {
			line = w
		} else {
			line += " " + w
		}
	}
	if line != "" {
		out = append(out, line)
	}
	return out
}

type cliHelpEntry struct{ name, short string }

var (
	cliHelpOnce    sync.Once
	cliHelpEntries []cliHelpEntry
)

func cachedCLIHelp() []cliHelpEntry {
	cliHelpOnce.Do(func() {
		for _, c := range newRootCommand().Commands() {
			if c.Hidden {
				continue
			}
			cliHelpEntries = append(cliHelpEntries, cliHelpEntry{name: "sciget " + c.Name(), short: c.Short})
		}
	})
	return cliHelpEntries
}

func (m model) helpOverlayLines(w int) []overlayLine {
	var out []overlayLine
	heading := func(title string) {
		if len(out) > 0 {
			out = append(out, overlayLine{})
		}
		out = append(out, overlayLine{text: title, style: "heading"})
	}
	para := func(text string) {
		out = appendWrapped(out, text, "", w)
	}
	item := func(key, desc string) {
		out = appendWrapped(out, fmt.Sprintf("%-22s %s", key, desc), "", w)
	}

	heading("Navigation")
	item("Type anywhere", "Filter software by name, version, or category")
	item("Tab / Shift+Tab", "Move keyboard focus across controls")
	item("Arrow keys", "Move within lists, buttons, or columns")
	item("Enter / Click", "Select or activate the focused control")
	item("F1 or ?", "Open help (? types into search when search is focused and non-empty)")
	item("Esc", "Close an open overlay, or quit sciget")
	item("Ctrl+C", "Quit sciget")

	heading("Terminal vs. Container shell")
	para("Open a terminal starts your host shell with the tool's Lmod module loaded (or cached wrappers when Lmod is unavailable or the container is on a share). Your host tools (git, editors, Slurm) stay available and you can combine multiple tools in one session.")
	out = append(out, overlayLine{})
	para("Open container shell enters the Apptainer container directly with its name and version in the prompt. Use it when you need the container's internal Python/R environment or want to inspect the container filesystem.")

	heading("Multi-tool sessions")
	para("In the TUI, click + Add to session on any available container to stage multiple tools, then click Open a terminal (+N) to launch one shell with all of them loaded.")
	out = append(out, overlayLine{})
	para("Inside an active session shell, manage tools live without losing your shell state:")
	item("sciget add <t/v>", "Load another tool (or switch versions) in this shell")
	item("sciget session", "List tools currently loaded in this session")
	item("sciget session remove", "Unload a tool and restore PATH / environment")
	item("exit", "Leave the session shell and return to the TUI")

	heading("CLI commands")
	for _, e := range cachedCLIHelp() {
		item(e.name, e.short)
	}
	return out
}

// truncateUTF8 cuts s to at most limit bytes on a valid UTF-8 rune boundary.
func truncateUTF8(s string, limit int) (string, bool) {
	if len(s) <= limit {
		return s, false
	}
	i := limit
	for i > 0 && !utf8.RuneStart(s[i]) {
		i--
	}
	return s[:i], true
}

func formatDOI(doi string) string {
	d := strings.TrimSpace(printable(doi))
	if d == "" {
		return ""
	}
	if strings.HasPrefix(d, "http://") || strings.HasPrefix(d, "https://") {
		return d
	}
	return "https://doi.org/" + d
}

func (m model) aboutOverlayLines(w int) []overlayLine {
	c, ok := m.selected()
	if !ok {
		return []overlayLine{{text: "No container selected.", style: "muted"}}
	}
	var out []overlayLine
	heading := func(title string) {
		if len(out) > 0 {
			out = append(out, overlayLine{})
		}
		out = append(out, overlayLine{text: title, style: "heading"})
	}
	field := func(label, val string) {
		if val == "" {
			val = "(none)"
		}
		out = appendWrapped(out, fmt.Sprintf("%-13s %s", label+":", printable(val)), "", w)
	}

	heading("Container details")
	field("Name", c.name())
	field("Build", fmt.Sprintf("%s (%s)", c.buildDate, c.fileName()))
	field("Catalog", c.catalog)
	field("Categories", strings.Join(c.categories, ", "))
	lic := strings.Join(c.licences, ", ")
	if lic == "" {
		lic = "(not stated)"
	}
	field("Licence", lic)
	var dois []string
	for _, d := range c.dois {
		if f := formatDOI(d); f != "" {
			dois = append(dois, f)
		}
	}
	field("DOI", strings.Join(dois, ", "))
	if len(c.entries) > 0 {
		var guis []string
		for _, e := range c.entries {
			guis = append(guis, strings.TrimSuffix(printable(e.label), "GUI"))
		}
		field("GUI apps", strings.Join(guis, ", "))
	}
	field("Location", m.location)
	field("Module", m.module)

	heading("Use from the command line")
	out = appendWrapped(out, "Host session:     "+shellJoin([]string{"sciget", "shell", c.name()}), "accent", w)
	out = appendWrapped(out, "Container shell:  "+shellJoin([]string{"sciget", "apptainer", "shell", c.name()}), "muted", w)
	if strings.HasPrefix(m.module, "module load ") {
		out = appendWrapped(out, "Lmod module:      "+m.module, "muted", w)
	}

	heading("Commands & README")
	switch {
	case strings.HasPrefix(m.location, notHere):
		out = appendWrapped(out, "Not on this machine yet. Install or pull this container to inspect its commands and README.", "muted", w)
	case m.aboutLoading:
		out = append(out, overlayLine{text: m.spin.View() + " Inspecting container commands and README…", style: "accent"})
	case m.aboutErr != "" && m.aboutFor == c.name():
		out = appendWrapped(out, "Couldn't inspect container: "+printable(m.aboutErr), "err", w)
	case m.aboutList != nil && m.aboutFor == c.name():
		excludes, _ := m.s.cfg.getList("finder_excludes")
		names, _ := moduleCommands(*m.aboutList, excludes)
		out = append(out, overlayLine{text: fmt.Sprintf("Commands (%d)", len(names)), style: "accent"})
		if len(names) == 0 {
			out = append(out, overlayLine{text: "(no wrapped commands found)", style: "muted"})
		} else {
			out = appendWrapped(out, strings.Join(names, "  "), "", w)
		}
		out = append(out, overlayLine{})
		out = append(out, overlayLine{text: "README", style: "accent"})
		readme, truncated := truncateUTF8(m.aboutList.Help, maxReadmeBytes)
		if strings.TrimSpace(readme) == "" {
			out = append(out, overlayLine{text: "(no /README.md in this container)", style: "muted"})
		} else {
			for _, rawLine := range strings.Split(strings.ReplaceAll(readme, "\r\n", "\n"), "\n") {
				clean := printable(strings.ReplaceAll(rawLine, "\t", "    "))
				if strings.TrimSpace(clean) == "" {
					out = append(out, overlayLine{})
					continue
				}
				out = appendWrapped(out, clean, "", w)
			}
			if truncated {
				out = append(out, overlayLine{text: "… (README truncated at 64 KiB)", style: "muted"})
			}
		}
	}
	return out
}

func (m model) viewOverlay(put draw) {
	if m.overlay == overlayNone {
		return
	}
	r := m.overlayRect()
	bodyH := m.overlayBodyHeight(r)
	contentW := max(1, r.w-4)
	allLines := m.overlayLines(contentW)
	maxScroll := max(0, len(allLines)-bodyH)
	scroll := clamp(m.overlayScroll, 0, maxScroll)

	bg := lipgloss.NewStyle().Background(m.th.band)
	edge := bg.Inherit(m.th.accent)
	title := bg.Inherit(m.th.accent).Bold(true).Render(fit(m.overlayTitle(), max(1, r.w-16)))
	closeText := bg.Inherit(m.th.muted).Bold(true).Render(" × Close ")
	fillCount := max(0, r.w-6-lipgloss.Width(title)-lipgloss.Width(closeText))

	rows := make([]string, 0, r.h)
	rows = append(rows, edge.Render("┌─ ")+title+edge.Render(" "+strings.Repeat("─", fillCount))+closeText+edge.Render("─┐"))

	for i := range bodyH {
		idx := scroll + i
		var rendered string
		if idx < len(allLines) {
			ln := allLines[idx]
			st := bg.Inherit(m.th.text)
			switch ln.style {
			case "heading":
				st = bg.Inherit(m.th.heading)
			case "muted":
				st = bg.Inherit(m.th.muted)
			case "accent":
				st = bg.Inherit(m.th.accent).Bold(true)
			case "err":
				st = bg.Inherit(m.th.err)
			}
			rendered = st.Render(pad(fit(ln.text, contentW), contentW))
		} else {
			rendered = bg.Render(strings.Repeat(" ", contentW))
		}
		rows = append(rows, edge.Render("│")+bg.Render(" ")+rendered+bg.Render(" ")+edge.Render("│"))
	}

	rows = append(rows, edge.Render("├"+strings.Repeat("─", r.w-2)+"┤"))
	hint := "↑↓ / PgUp / PgDn / wheel scroll · Esc close"
	if len(allLines) > bodyH {
		hint = fmt.Sprintf("%d–%d of %d · ↑↓ / wheel scroll · Esc close",
			scroll+1, min(len(allLines), scroll+bodyH), len(allLines))
	}
	footer := bg.Inherit(m.th.muted).Render(pad(fit(hint, contentW), contentW))
	rows = append(rows, edge.Render("│")+bg.Render(" ")+footer+bg.Render(" ")+edge.Render("│"))
	rows = append(rows, edge.Render("└"+strings.Repeat("─", r.w-2)+"┘"))

	put(r.x, r.y, strings.Join(rows, "\n"))
}
