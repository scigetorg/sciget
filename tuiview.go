package main

import (
	"errors"
	"fmt"
	"image/color"
	"strings"

	"charm.land/bubbles/v2/textinput"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
)

// Sizes of the fixed pieces, in cells.
const (
	navRows       = 3
	logoWidth     = 12
	pillWidth     = 12 // Quit and Clear
	updateWidth   = 25
	refreshWidth  = 26
	buttonWidth   = 30
	buttonPitch   = 4  // a button and the blank row after it
	minPanel      = 36 // the narrowest Details or Activity
	catsWidth     = 28
	appsWidth     = 24
	versionsWidth = 12
	maxCallout    = 7 // a failure's headline and up to six wrapped lines
)

type rect struct{ x, y, w, h int }

func (r rect) has(x, y int) bool { return x >= r.x && x < r.x+r.w && y >= r.y && y < r.y+r.h }

// crumb is one segment of the breadcrumb path, "All › <category> › <app>
// › <version>"; a click focuses ctl's column. A leading "…" has noControl.
type crumb struct {
	rect
	text string
	ctl  control
	all  bool // All also resets the category
}

// layout holds every rectangle on screen. It is computed from the window
// size and the text that sizes things (the breadcrumb, the status
// callout); View draws from it and hit maps a point back through it, so a
// click always lands on what is drawn.
type layout struct {
	tooSmall, stacked        bool
	quit, update, version    rect
	crumbs                   []crumb
	search, clear            rect
	software, details, activ rect // panel frames; activ.w is 0 when Activity is hidden
	cats, apps, versions     rect // each list's rows, below its header
	refresh, fields, buttons rect
	callout, entries         rect
}

// layout computes the screen's rectangles. Two switches shape it: Activity
// shows when the room right of Software halves into two of at least
// minPanel cells, and the Software sub-columns stack under 120 cells.
func (m model) layout() layout {
	w, h := m.width, m.height
	var l layout
	if w < 80 || h < 24 {
		l.tooSmall = true
		return l
	}
	l.quit = rect{w - 1 - pillWidth, 0, pillWidth, navRows}
	l.update = rect{l.quit.x - 1 - updateWidth, 0, updateWidth, navRows}
	l.version = rect{logoWidth + 2, 1, l.update.x - logoWidth - 5, 1}
	top := navRows + 3
	if m.loadErr != nil {
		if m.emptyControl() == refreshControl {
			l.refresh = rect{(w - refreshWidth) / 2, top + 4, refreshWidth, 3}
		}
		l.callout = rect{(w - 60) / 2, top + 8, 60, len(m.calloutText(60, maxCallout))}
		return l
	}

	l.stacked = w < 120
	sw := catsWidth + appsWidth + versionsWidth + 4 // the frame and two dividers
	if l.stacked {
		sw = minPanel
	}
	l.software = rect{0, top, sw, h - top}
	rest := w - sw - 1
	if half := (rest - 1) / 2; half >= minPanel {
		l.details = rect{sw + 1, top, half, h - top}
		l.activ = rect{sw + 2 + half, top, rest - half - 1, h - top}
		l.search = rect{l.details.x, navRows, half, 3}
		l.clear = rect{l.activ.x, navRows, pillWidth, 3}
	} else {
		l.details = rect{sw + 1, top, rest, h - top}
		l.search = rect{l.details.x, navRows, rest - pillWidth - 1, 3}
		l.clear = rect{l.search.x + l.search.w + 1, navRows, pillWidth, 3}
	}
	l.crumbs = m.crumbs(l.search.x - 4)

	y, inner := top+1, h-top-2 // inside the frames
	if !l.stacked {
		l.cats = rect{1, y + 1, catsWidth, inner - 5}
		l.apps = rect{2 + catsWidth, y + 1, appsWidth, inner - 1}
		l.versions = rect{3 + catsWidth + appsWidth, y + 1, versionsWidth, inner - 1}
	} else {
		// Three header rows and Refresh's four leave the rest, a quarter
		// each to Category and Version and the remainder to Name.
		avail := inner - 7
		c := max(2, avail/4)
		l.cats = rect{1, y + 1, sw - 2, c}
		l.apps = rect{1, y + c + 6, sw - 2, avail - 2*c}
		l.versions = rect{1, l.apps.y + l.apps.h + 1, sw - 2, c}
	}
	l.refresh = rect{2, l.cats.y + l.cats.h + 1, refreshWidth, 3}

	cx, cw, bottom := l.details.x+3, l.details.w-6, h-1 // bottom: the frames' last row
	l.fields = rect{cx, y + 1, cw, 7}
	l.buttons = rect{cx, y + 9, cw, bottom - y - 9} // after the fields, a blank row and the rule
	if l.activ.w > 0 {
		ax, aw := l.activ.x+3, l.activ.w-6
		l.callout = rect{ax, y + 1, aw, len(m.calloutText(aw, maxCallout))}
		l.entries = rect{ax, l.callout.y + l.callout.h + 1, aw, bottom - l.callout.y - l.callout.h - 1}
	} else {
		k := len(m.calloutText(cw, max(2, l.buttons.h-5)))
		l.callout = rect{cx, bottom - k, cw, k}
		l.buttons.h -= k + 1
	}
	return l
}

// crumbs lays out the breadcrumb from column 2 in room cells. When it is
// too long, segments give way from the left to a "…".
func (m model) crumbs(room int) []crumb {
	all := []crumb{{text: "All", ctl: catsControl, all: true}}
	if cat := m.categories[m.cat]; cat != "" {
		all = append(all, crumb{text: printable(cat), ctl: catsControl})
	}
	if c, ok := m.selected(); ok {
		all = append(all, crumb{text: c.tool, ctl: appsControl}, crumb{text: c.version, ctl: versionsControl})
	}
	parts := all
	width := func(cs []crumb) int {
		n := 0
		for _, c := range cs {
			n += lipgloss.Width(c.text) + 3 // and " › "
		}
		return n - 3
	}
	for i := 1; width(parts) > room && i < len(all); i++ {
		parts = append([]crumb{{text: "…"}}, all[i:]...)
	}
	last := &parts[len(parts)-1]
	last.text = fit(last.text, room-width(parts[:len(parts)-1])-3)
	x := 2
	for i := range parts {
		parts[i].rect = rect{x, navRows + 1, lipgloss.Width(parts[i].text), 1}
		x += parts[i].w + 3
	}
	return parts
}

// target is what a point on screen holds: a control (with the list item,
// button or Activity entry index) and the panel it is in.
type target struct {
	ctl   control
	index int
	panel string
}

// hit returns the target at x, y, from the layout, scroll offsets and item
// counts.
func (m model) hit(l layout, x, y int) target {
	in := func(r rect) bool { return r.has(x, y) }
	switch {
	case l.tooSmall:
		return target{}
	case in(l.quit):
		return target{ctl: quitControl}
	case in(l.update):
		return target{ctl: updateControl}
	case in(l.refresh):
		return target{ctl: refreshControl, panel: "software"}
	case m.loadErr != nil:
		return target{}
	case in(l.search):
		return target{ctl: searchControl}
	case in(l.clear) && m.search.Value() != "":
		return target{ctl: clearControl}
	}
	for i, c := range l.crumbs {
		if in(c.rect) && c.ctl != noControl {
			return target{ctl: crumbControl, index: i}
		}
	}
	var t target
	switch {
	case in(l.software):
		t.panel = "software"
	case in(l.details):
		t.panel = "details"
	case in(l.activ):
		t.panel = "activity"
	}
	tool, _ := m.selectedNames()
	item := func(c control, r rect, top, count int) {
		if i := top + y - r.y; i < count {
			t.ctl, t.index = c, i
		}
	}
	switch {
	case in(l.cats):
		item(catsControl, l.cats, m.catTop, len(m.categories))
	case in(l.apps):
		item(appsControl, l.apps, m.appTop, len(m.apps))
	case in(l.versions):
		item(versionsControl, l.versions, m.verTop, len(m.byTool[tool]))
	case in(l.buttons) && x < l.buttons.x+buttonWidth:
		first, n, above, _ := buttonView(m.btnTop, m.buttonCount(), l.buttons.h)
		row := y - l.buttons.y
		if above {
			row--
		}
		if i := first + row/buttonPitch; row >= 0 && row%buttonPitch < 3 && i < first+n {
			t.ctl, t.index = buttonsControl, i
		}
	case in(l.entries):
		row := y - l.entries.y
		if i := m.actTop + row/3; row%3 < 2 && i < len(m.launches)+len(m.earlier) {
			t.ctl, t.index = activityControl, i
		}
	}
	return t
}

// buttonView returns the first button shown in a viewport rows high, how
// many show, and whether some are hidden above or below; each hidden side
// takes a row for its "N more" line.
func buttonView(top, count, rows int) (first, n int, above, below bool) {
	first = clamp(top, 0, max(0, count-1))
	above = first > 0
	if above {
		rows--
	}
	n = min(count-first, (rows+1)/buttonPitch) // the last button needs no blank row
	if first+n < count {
		below = true
		n = max(0, rows/buttonPitch)
	}
	if n == 0 && count > 0 {
		// Too short for a button and both lines: the button wins, then "▲".
		n, below = 1, false
		above = above && rows >= buttonPitch-1
	}
	return first, n, above, below
}

// followButton returns the viewport offset that keeps button btn in view.
func followButton(btn, top, count, rows int) int {
	if btn < top {
		return btn
	}
	for top < btn {
		if first, n, _, _ := buttonView(top, count, rows); btn < first+n {
			break
		}
		top++
	}
	return top
}

// clampScroll keeps every scroll offset within its list after a resize or
// a change of items, and scrolls the buttons back when room allows. When
// the button area changes height, it scrolls the selected button into view.
func (m *model) clampScroll() {
	l := m.layout()
	tool, _ := m.selectedNames()
	m.catTop = clamp(m.catTop, 0, max(0, len(m.categories)-l.cats.h))
	m.appTop = clamp(m.appTop, 0, max(0, len(m.apps)-l.apps.h))
	m.verTop = clamp(m.verTop, 0, max(0, len(m.byTool[tool])-l.versions.h))
	m.actTop = clamp(m.actTop, 0, max(0, len(m.launches)+len(m.earlier)-1))
	count := m.buttonCount()
	m.btnTop = clamp(m.btnTop, 0, max(0, count-1))
	for m.btnTop > 0 {
		if _, _, _, below := buttonView(m.btnTop-1, count, l.buttons.h); below {
			break
		}
		m.btnTop--
	}
	if l.buttons.h != m.btnRows {
		m.btnRows = l.buttons.h
		m.btnTop = followButton(m.btn, m.btnTop, count, l.buttons.h)
	}
}

// theme holds the styles for a dark or light terminal background.
type theme struct {
	text, muted, heading, label, accent, ok, err, rule lipgloss.Style
	selected, dimSelected                              lipgloss.Style
	band, term, gui, onFill                            color.Color
	input                                              textinput.Styles
}

func newTheme(dark bool) theme {
	c := func(light, darkHex string) color.Color {
		return lipgloss.LightDark(dark)(lipgloss.Color(light), lipgloss.Color(darkHex))
	}
	fg := func(col color.Color) lipgloss.Style { return lipgloss.NewStyle().Foreground(col) }
	muted := c("#545861", "#888b96")
	t := theme{
		text:        fg(c("#353841", "#c0c2c7")),
		muted:       fg(muted),
		heading:     fg(c("#24272f", "#eceef2")).Bold(true),
		label:       fg(muted).Bold(true),
		accent:      fg(c("#b85c00", "#d97706")),
		ok:          fg(c("#2f7d32", "#7ec699")),
		err:         fg(c("#b3261e", "#f28b82")),
		rule:        fg(c("#d5d7dc", "#353841")),
		selected:    fg(c("#4d2400", "#ffd0a3")).Background(c("#ffe0c3", "#4d2400")),
		dimSelected: fg(c("#353841", "#c0c2c7")).Background(c("#eceef2", "#353841")),
		band:        c("#e8e8e8", "#23262f"),
		term:        c("#1a6fb5", "#6cb6ff"),
		gui:         c("#0f766e", "#5bbfb2"),
		onFill:      c("#ffffff", "#17181c"),
	}
	t.input = textinput.DefaultStyles(dark)
	for _, s := range []*textinput.StyleState{&t.input.Focused, &t.input.Blurred} {
		s.Text, s.Prompt, s.Placeholder = fg(c("#17181c", "#eceef2")), t.muted, fg(c("#888b96", "#545861"))
	}
	return t
}

// pill draws a rounded, width-wide control in col on bg (nil for none);
// filled, a solid block of col, three rows high.
func (t theme) pill(label string, col color.Color, width int, filled bool, bg color.Color) string {
	text := fit(label, width-4)
	if filled {
		// Plain coloured cells: block-element glyphs leave seams in some fonts.
		fill := lipgloss.NewStyle().Background(col).Foreground(t.onFill).Bold(true)
		blank := fill.Render(strings.Repeat(" ", width))
		return blank + "\n" + fill.Render(pad("  "+text, width)) + "\n" + blank
	}
	edge := lipgloss.NewStyle().Foreground(col)
	if bg != nil {
		edge = edge.Background(bg)
	}
	return edge.Border(lipgloss.RoundedBorder()).BorderForeground(col).BorderBackground(bg).
		Bold(true).Width(width).Padding(0, 1).Render(text)
}

// draw places one block on the screen at x, y.
type draw func(x, y int, block string)

func (m model) View() tea.View {
	v := tea.NewView("")
	v.AltScreen = true
	v.MouseMode = tea.MouseModeAllMotion // all motion, for hover
	if m.width == 0 {
		return v
	}
	l := m.layout()
	if l.tooSmall {
		v.SetContent(fit(fmt.Sprintf("Please make the window at least 80 by 24 (it is %d by %d) · Esc quits",
			m.width, m.height), m.width))
		return v
	}
	var layers []*lipgloss.Layer
	put := func(x, y int, block string) {
		layers = append(layers, lipgloss.NewLayer(block).X(x).Y(y).Z(len(layers)))
	}
	m.viewNavbar(put, l)
	if m.loadErr != nil {
		message := m.th.heading.Render(fit(emptyMessage(m.loadErr), m.width-4))
		put((m.width-lipgloss.Width(message))/2, navRows+2, message)
		if l.refresh.w > 0 {
			put(l.refresh.x, l.refresh.y, m.refreshPill())
		}
		m.viewCallout(put, l.callout)
	} else {
		m.viewSearchRow(put, l)
		m.viewSoftware(put, l)
		m.viewDetails(put, l)
		m.viewActivity(put, l)
	}
	v.SetContent(lipgloss.NewCompositor(layers...).Render())
	return v
}

// emptyMessage is the empty state's message for err, without control
// characters: an error can quote a catalog's text.
func emptyMessage(err error) string {
	return printable(emptyText(err))
}

func emptyText(err error) string {
	var catErr *catalogError
	var missing *notDownloadedError
	switch {
	case errors.Is(err, errNoCatalogs):
		return "No catalogs yet. Add one with 'sciget catalog add <name> <url>'."
	case errors.Is(err, errNoneInUse):
		return "No catalogs are in use. Turn one on with 'sciget catalog use <name>'."
	case errors.As(err, &catErr) && errors.As(err, &missing):
		return "The catalog " + catErr.name + " hasn't been downloaded yet."
	case errors.As(err, &catErr):
		return "The catalog " + catErr.name + " couldn't be read: " + catErr.err.Error() + "."
	case errors.Is(err, errNoContainers):
		return "The catalogs in use have no containers to show."
	}
	return err.Error()
}

func (m model) viewNavbar(put draw, l layout) {
	band := lipgloss.NewStyle().Background(m.th.band)
	put(0, 0, band.Width(m.width).Height(navRows).Render(""))
	put(0, 0, lipgloss.NewStyle().Background(lipgloss.Color("#6779af")).Foreground(lipgloss.Color("#eceef2")).
		Bold(true).Width(logoWidth).Height(navRows).Align(lipgloss.Center, lipgloss.Center).Render("SCIGET"))

	// The update result gives way first by its releases hint, then whole.
	name, result := "sciget "+version, printable(m.updateText)
	if m.checking {
		result = "checking…"
	}
	short, _, _ := strings.Cut(result, " · see ")
	resultStyle := m.th.muted
	switch {
	case strings.HasPrefix(result, "√"):
		resultStyle = m.th.ok
	case strings.HasPrefix(result, "↑"):
		resultStyle = m.th.accent
	}
	if lipgloss.Width(name+" · "+result) > l.version.w {
		result = fit(short, l.version.w-lipgloss.Width(name+" · "))
	}
	text := band.Inherit(m.th.text).Bold(true).Render(fit(name, l.version.w))
	if result != "" && result != "…" {
		text += band.Inherit(m.th.muted).Render(" · ") + band.Inherit(resultStyle).Render(result)
	}
	put(l.version.x+l.version.w-lipgloss.Width(text), 1, text)

	label := "  Check for updates" // the spinner's cell left blank, so the text stays put
	if m.checking {
		label = m.spin.View() + " Checking…"
	}
	put(l.update.x, 0, m.th.pill(label, m.focusColour(updateControl, m.th.text.GetForeground()), updateWidth, false, m.th.band))
	put(l.quit.x, 0, m.th.pill("×  Quit", m.focusColour(quitControl, m.th.err.GetForeground()), pillWidth,
		m.pressed.ctl == quitControl, m.th.band))
}

// focusColour is col, or the accent while c has the keyboard focus.
func (m model) focusColour(c control, col color.Color) color.Color {
	if m.focus == c && m.touched {
		return m.th.accent.GetForeground()
	}
	return col
}

func (m model) viewSearchRow(put draw, l layout) {
	for i, c := range l.crumbs {
		if i > 0 {
			put(c.x-3, c.y, m.th.muted.Render(" › "))
		}
		hovered := c.ctl != noControl && c.has(m.hoverX, m.hoverY)
		put(c.x, c.y, m.th.text.Underline(hovered).Render(c.text))
	}
	border := m.th.muted.GetForeground()
	if m.focus == searchControl {
		border = m.th.accent.GetForeground()
	}
	put(l.search.x, l.search.y, lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).BorderForeground(border).
		Width(l.search.w).Render(m.search.View()))
	if m.search.Value() != "" {
		put(l.clear.x, l.clear.y, m.th.pill("×  Clear", m.focusColour(clearControl, m.th.muted.GetForeground()),
			pillWidth, m.pressed.ctl == clearControl, nil))
	}
}

func (m model) viewSoftware(put draw, l layout) {
	put(l.software.x, l.software.y, m.frame(fmt.Sprintf("Software (%d)", len(m.apps)), l.software, m.active == "software"))
	head := func(r rect, text string) {
		line := " " + text
		if l.stacked {
			line += " " + strings.Repeat("─", max(0, r.w-len(text)-3))
		}
		put(r.x, r.y-1, m.th.muted.Render(line))
	}
	head(l.cats, "Category")
	head(l.apps, "Name")
	head(l.versions, "Version")
	if !l.stacked {
		for _, x := range []int{l.apps.x - 1, l.versions.x - 1} {
			put(x, l.software.y+1, m.th.rule.Render(strings.TrimSuffix(strings.Repeat("│\n", l.software.h-2), "\n")))
		}
	}
	cats := make([]string, len(m.categories))
	for i, cat := range m.categories {
		name, count := printable(cat), fmt.Sprint(m.counts[cat])
		if cat == "" {
			name = "All"
		}
		name = fit(name, l.cats.w-3-len(count))
		cats[i] = name + strings.Repeat(" ", max(1, l.cats.w-2-lipgloss.Width(name)-len(count))) + count
	}
	tool, _ := m.selectedNames()
	var versions []string
	for _, c := range m.byTool[tool] {
		versions = append(versions, c.version)
	}
	m.viewList(put, l.cats, cats, m.cat, m.catTop, catsControl)
	m.viewList(put, l.apps, m.apps, m.app, m.appTop, appsControl)
	m.viewList(put, l.versions, versions, m.ver, m.verTop, versionsControl)
	put(l.refresh.x, l.refresh.y, m.refreshPill())
}

func (m model) refreshPill() string {
	label := "  Refresh catalogs" // as Check for updates
	if m.refreshing {
		label = m.spin.View() + " Refreshing…"
	}
	return m.th.pill(label, m.focusColour(refreshControl, m.th.text.GetForeground()), refreshWidth, false, nil)
}

// viewList draws a list's visible rows: the selection as a tinted row, bold
// while its list has focus, and the row under the pointer underlined.
func (m model) viewList(put draw, r rect, items []string, sel, top int, c control) {
	for row := 0; row < r.h && top+row < len(items); row++ {
		i := top + row
		cell := pad(" "+fit(printable(items[i]), r.w-2), r.w)
		style := m.th.text.Underline(r.has(m.hoverX, m.hoverY) && m.hoverY == r.y+row)
		switch {
		case i == sel && m.focus == c:
			style = m.th.selected.Bold(true)
		case i == sel:
			style = m.th.dimSelected
		}
		put(r.x, r.y+row, style.Render(cell))
	}
}

func (m model) viewDetails(put draw, l layout) {
	put(l.details.x, l.details.y, m.frame("Details", l.details, m.active == "details"))
	if l.activ.w == 0 {
		m.viewCallout(put, l.callout)
	}
	c, ok := m.selected()
	if !ok {
		put(l.fields.x, l.fields.y, m.th.muted.Render("No containers match."))
		return
	}
	location, locStyle := m.location, m.th.muted
	switch {
	case strings.HasPrefix(location, onHere):
		location, locStyle = "√ "+location, m.th.ok
	case location == notHere:
		location, locStyle = "▼ Not downloaded", m.th.accent.Bold(true)
	case strings.HasPrefix(location, notHere+": "):
		location, locStyle = "× "+strings.TrimPrefix(location, notHere+": the "), m.th.err
	case strings.HasPrefix(location, "couldn't check"):
		location, locStyle = "× Couldn't check: "+strings.TrimPrefix(location, "couldn't check: "), m.th.err
	}
	for i, f := range []struct {
		label, value string
		style        lipgloss.Style
	}{
		{"NAME", c.tool, m.th.accent.Bold(true)},
		{"VERSION", c.version, m.th.accent.Bold(true)},
		{"BUILT", c.buildDate, m.th.text},
		{"CATALOG", c.catalog, m.th.text},
		{"CATEGORIES", strings.Join(c.categories, " · "), m.th.text},
		{"LOCATION", location, locStyle},
		{"MODULE", m.module, m.th.text},
	} {
		put(l.fields.x, l.fields.y+i, m.th.accent.Render("│ ")+m.th.label.Render(fmt.Sprintf("%-12s", f.label))+
			f.style.Render(fit(printable(f.value), l.fields.w-14)))
	}
	put(l.fields.x, l.fields.y+7, m.th.rule.Render(strings.Repeat("─", l.fields.w)))

	first, n, above, below := buttonView(m.btnTop, m.buttonCount(), l.buttons.h)
	y := l.buttons.y
	if above {
		put(l.buttons.x, y, m.th.muted.Render(fmt.Sprintf("▲ %d more", first)))
		y++
	}
	bs := m.buttons()
	for i := first; i < first+n; i++ {
		b := bs[i]
		put(l.buttons.x, y, m.th.pill(b.label, b.col, buttonWidth, i == m.btn && m.focus == buttonsControl, nil))
		if room := l.buttons.w - buttonWidth - 2; room >= 12 {
			put(l.buttons.x+buttonWidth+2, y+1, m.th.muted.Render(fit(b.caption, room)))
		}
		y += buttonPitch
	}
	if below {
		put(l.buttons.x, l.buttons.y+l.buttons.h-1, m.th.muted.Render(fmt.Sprintf("▼ %d more", m.buttonCount()-first-n)))
	}
}

func (m model) viewActivity(put draw, l layout) {
	if l.activ.w == 0 {
		return
	}
	put(l.activ.x, l.activ.y, m.frame("Activity", l.activ, m.active == "activity"))
	m.viewCallout(put, l.callout)
	r := l.entries
	if len(m.launches)+len(m.earlier) == 0 {
		put(r.x+2, r.y, m.th.muted.Render("Every launch is listed here."))
		return
	}
	for i := m.actTop; i < len(m.launches)+len(m.earlier); i++ {
		y := r.y + 3*(i-m.actTop)
		if y+1 >= r.y+r.h {
			break
		}
		var icon, name, detail string
		iconStyle, nameStyle := m.th.muted, m.th.muted
		if i < len(m.launches) {
			e := m.launches[i]
			name, detail = e.name, e.action+" · "+e.at.Format("15:04")
			icon, iconStyle, nameStyle = m.spin.View(), m.th.accent, m.th.text.Bold(true)
			switch e.state {
			case "running", "finished":
				icon, iconStyle = "√", m.th.ok
			case "failed":
				icon, iconStyle, detail = "×", m.th.err, e.action+" · "+e.detail
			}
		} else {
			e := m.earlier[i-len(m.launches)]
			icon, name, detail = " ", e.Name, e.Action+" · "+e.At.Format("Mon 15:04")
		}
		chevron := m.th.text.Bold(true).Render("›")
		if (rect{r.x, y, r.w, 2}).has(m.hoverX, m.hoverY) || m.focus == activityControl && i == m.act {
			nameStyle, chevron = nameStyle.Underline(true), m.th.accent.Bold(true).Render("›")
		}
		put(r.x, y, iconStyle.Render(icon)+" "+nameStyle.Render(fit(printable(name), r.w-6))+" "+chevron)
		put(r.x+2, y+1, m.th.muted.Render(fit(printable(detail), r.w-2)))
	}
}

// calloutText returns the status callout's lines, unstyled: the headline,
// then the detail. A failure wraps, up to six lines and at most rows in
// all, ending with its log's path.
func (m model) calloutText(width, rows int) []string {
	st := m.statusFor()
	head := map[string]string{"": "• Ready", "starting": "… Starting", "refreshing": "… Refreshing",
		"running": "√ Running", "finished": "√ Finished", "refreshed": "√ Refreshed", "failed": "× Failed",
		"downloading": "… Downloading", "asking": "› Download it?", "quit": "! Quit?"}[st.state]
	w := width - 4 // the bar and an indent
	if st.state != "failed" && st.state != "asking" && st.state != "quit" {
		return []string{head, fit(printable(st.detail), w)}
	}
	n := max(1, min(rows, maxCallout)-1)
	if st.log == "" {
		return append([]string{head}, wrap(printable(st.detail), w, n)...)
	}
	path := chop(printable(st.log), w)
	if len(path) > n-1 {
		// No room for all of it: its end, which names the file.
		path = []string{"…" + tail(printable(st.log), w-1)}
	}
	return append(append([]string{head}, wrap(printable(st.detail), w, max(1, n-len(path)))...), path...)
}

// chop cuts s into lines of at most width cells, for text with no spaces
// to wrap at, such as a path.
func chop(s string, width int) []string {
	var lines []string
	line := ""
	for _, r := range s {
		if line != "" && lipgloss.Width(line+string(r)) > width {
			lines = append(lines, line)
			line = ""
		}
		line += string(r)
	}
	return append(lines, line)
}

// tail returns the end of s that fits in width cells.
func tail(s string, width int) string {
	r := []rune(s)
	i := len(r)
	for i > 0 && lipgloss.Width(string(r[i-1:])) <= width {
		i--
	}
	return string(r[i:])
}

func (m model) viewCallout(put draw, r rect) {
	lines := m.calloutText(r.w, r.h)
	col := m.th.ok
	switch m.statusFor().state {
	case "":
		col = m.th.muted
	case "starting", "refreshing", "downloading", "asking":
		col = m.th.accent
	case "failed", "quit":
		col = m.th.err
	}
	bar := col.Render("│ ")
	put(r.x, r.y, bar+col.Bold(true).Render(lines[0]))
	for i, line := range lines[1:] {
		put(r.x, r.y+1+i, bar+m.th.muted.Render("  "+line))
	}
}

// frame draws a square-cornered panel frame for r with title set into its
// top edge, in the accent while the panel is active.
func (m model) frame(title string, r rect, active bool) string {
	edge, heading := m.th.muted, m.th.label
	if active {
		edge, heading = m.th.accent, m.th.accent.Bold(true)
	}
	lines := []string{edge.Render("┌─ ") + heading.Render(title) +
		edge.Render(" "+strings.Repeat("─", max(0, r.w-5-lipgloss.Width(title)))+"┐")}
	for range r.h - 2 {
		lines = append(lines, edge.Render("│")+strings.Repeat(" ", r.w-2)+edge.Render("│"))
	}
	return strings.Join(append(lines, edge.Render("└"+strings.Repeat("─", r.w-2)+"┘")), "\n")
}

// fit shortens s to width cells, ending in "…" when cut.
func fit(s string, width int) string {
	if lipgloss.Width(s) <= width {
		return s
	}
	if width <= 0 {
		return ""
	}
	r := []rune(s)
	for lipgloss.Width(string(r)) > width-1 {
		r = r[:len(r)-1]
	}
	return string(r) + "…"
}

// pad fills s with spaces to width cells.
func pad(s string, width int) string {
	return s + strings.Repeat(" ", max(0, width-lipgloss.Width(s)))
}

// wrap word-wraps s to width and returns at most rows lines, the last ending
// in "…" if text was cut.
func wrap(s string, width, rows int) []string {
	var lines []string
	line := ""
	for _, w := range strings.Fields(s) {
		if line != "" && lipgloss.Width(line+" "+w) > width {
			lines = append(lines, line)
			line = ""
		}
		line = strings.TrimSpace(line + " " + fit(w, width))
	}
	lines = append(lines, line)
	if len(lines) > rows {
		lines = lines[:rows]
		lines[rows-1] = fit(lines[rows-1]+" …", width)
	}
	return lines
}

func clamp(v, lo, hi int) int { return max(lo, min(v, hi)) }

// follow returns the offset that keeps index within rows visible items.
func follow(index, top, rows int) int {
	if index < top {
		return index
	}
	if index >= top+rows {
		return index - rows + 1
	}
	return top
}
