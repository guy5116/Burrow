// Package tui is the Bubble Tea terminal UI (CLAUDE.md §9.2).
package tui

import (
	"context"
	"fmt"
	"io"
	"net"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"unicode/utf8"

	"charm.land/bubbles/v2/textinput"
	"charm.land/bubbles/v2/viewport"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"

	"github.com/guy5116/burrow/internal/core"
	"github.com/guy5116/burrow/internal/ui/common"
)

// Keys documents the key bindings (shown by /help and --help).
const Keys = `keys: enter send · tab/shift+tab next/previous contact · pgup/pgdn scroll · ctrl+c quit`

type evMsg struct{ ev core.Event }

// outMsg is the result of a queued command. A message that was sent comes
// back as mine, for the conversation it went to.
type outMsg struct {
	lines []string
	quit  bool
	to    core.PeerID
	mine  string
}

// noteMsg is a line for the system notes from anything that is not a command.
type noteMsg string

const (
	sidebarWidth = 26
	// A contact can send without end: each log keeps its newest lines, up to
	// maxLines and maxLogBytes, and drops the oldest.
	maxLines    = 5000
	maxLogBytes = 1 << 20
)

// entry is one line of a log. seq orders the lines of every log, so that
// the notes show among the conversation's lines in the order they happened.
type entry struct {
	seq   uint64
	text  string
	style func(...string) string // nil: plain
	image bool                   // placeholder or pixel rows: never wrapped
	rows  string                 // text fitted to width, kept until the width changes
	width int
}

// fit returns the entry fitted to width columns.
func (e *entry) fit(width int) string {
	if e.width != width || e.rows == "" {
		e.rows, e.width = e.text, width
		if !e.image {
			e.rows = wrap(e.text, width)
		}
		if e.style != nil {
			e.rows = e.style(e.rows)
		}
	}
	return e.rows
}

// chatLog is one conversation, or the notes.
type chatLog struct {
	lines []entry
	bytes int
}

// add appends a line and drops the oldest ones beyond the limits. The newest
// line always stays.
func (l *chatLog) add(e entry) {
	l.lines = append(l.lines, e)
	l.bytes += len(e.text)
	drop := 0
	for len(l.lines)-drop > maxLines || l.bytes > maxLogBytes && drop < len(l.lines)-1 {
		l.bytes -= len(l.lines[drop].text)
		drop++
	}
	if drop > 0 {
		l.lines = append(l.lines[:0], l.lines[drop:]...)
	}
}

// wrap fits a line into width columns. Every row after the first starts with
// the gutter that a message's own line breaks get, so a contact cannot pad
// text until its end wraps into what looks like a line of its own. A row
// without spaces, such as an invite, is cut without a gutter so that it can
// be copied (/connect drops the line breaks); a QR code is left for the
// viewport to cut, since wrapping would scramble it.
func wrap(line string, width int) string {
	if lipgloss.Width(line) <= width {
		return line
	}
	rows := strings.Split(line, "\n")
	for i, row := range rows {
		switch {
		case lipgloss.Width(row) <= width || isPicture(row):
		case !strings.Contains(row, " "):
			rows[i] = lipgloss.Wrap(row, width, "")
		default:
			rows[i] = strings.ReplaceAll(lipgloss.Wrap(row, width-lipgloss.Width(common.Gutter), ""), "\n", "\n"+common.Gutter)
		}
	}
	return strings.Join(rows, "\n")
}

// isPicture reports whether a row is drawn with block characters only.
func isPicture(row string) bool {
	return strings.Trim(row, " █▀▄") == ""
}

var (
	dim    = lipgloss.NewStyle().Faint(true)
	bold   = lipgloss.NewStyle().Bold(true)
	alert  = lipgloss.NewStyle().Foreground(lipgloss.Color("1")).Bold(true)
	status = lipgloss.NewStyle().Reverse(true)
)

type model struct {
	ctx       context.Context
	ctl       *common.Controller
	target    core.Target
	addrs     string
	contacts  []core.Contact
	logs      map[core.PeerID]*chatLog
	system    chatLog
	seq       uint64                 // of the last line added to any log
	unread    map[core.PeerID]string // sidebar marker: "!" an offer waits, "•" a message
	alert     string                 // shown in the status bar until the next Enter
	dirty     bool                   // the viewport must be filled again
	shown     *core.PeerID           // the conversation the viewport shows
	vp        viewport.Model
	in        textinput.Model
	w, h      int
	busy      int // commands still running
	order     inOrder
	typingTo  *core.PeerID // the contact who was last told that we are typing
	nextImage uint8        // Kitty image ids 1–255, reused in a cycle
}

// add appends entries to a conversation, or to the notes when peer is nil.
func (m *model) add(peer *core.PeerID, es ...entry) {
	l := &m.system
	if peer != nil {
		if l = m.logs[*peer]; l == nil {
			l = &chatLog{}
			m.logs[*peer] = l
		}
	}
	for _, e := range es {
		m.seq++
		e.seq = m.seq
		l.add(e)
	}
	if current, ok := m.ctl.Selected(); peer == nil || ok && current == *peer {
		m.dirty = true
	}
}

// log adds lines of text, drawn with style (nil: plain).
func (m *model) log(peer *core.PeerID, style func(...string) string, lines ...string) {
	for _, l := range lines {
		m.add(peer, entry{text: l, style: style})
	}
}

// note adds lines to the notes.
func (m *model) note(lines ...string) { m.log(nil, nil, lines...) }

// inOrder runs functions on their own goroutines, one at a time, in the order
// their tickets were drawn. Bubble Tea runs every command on a goroutine of
// its own; without this, two messages sent in quick succession could leave in
// the wrong order.
type inOrder struct {
	mu      sync.Mutex
	turn    *sync.Cond
	drawn   uint64
	serving uint64
}

// ticket reserves the next place in the queue. Call it from Update.
func (o *inOrder) ticket() uint64 {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.turn == nil {
		o.turn = sync.NewCond(&o.mu)
	}
	o.drawn++
	return o.drawn - 1
}

// run waits for ticket n's turn, runs fn, and lets the next one go.
func (o *inOrder) run(n uint64, fn func()) {
	o.mu.Lock()
	for o.serving != n {
		o.turn.Wait()
	}
	o.mu.Unlock()
	defer func() {
		o.mu.Lock()
		o.serving++
		o.mu.Unlock()
		o.turn.Broadcast()
	}()
	fn()
}

// Run starts the TUI and blocks until it exits.
func Run(ctx context.Context, ctl *common.Controller, target core.Target, addrs []net.Addr, stderr io.Writer) int {
	return run(ctx, ctl, target, addrs, stderr)
}

func run(ctx context.Context, ctl *common.Controller, target core.Target, addrs []net.Addr, stderr io.Writer, opts ...tea.ProgramOption) int {
	var as []string
	for _, a := range addrs {
		as = append(as, a.String())
	}
	in := textinput.New()
	in.Placeholder = "message or /command"
	in.Focus()
	m := &model{ctx: ctx, ctl: ctl, target: target, addrs: strings.Join(as, " "), logs: map[core.PeerID]*chatLog{},
		unread: map[core.PeerID]string{}, vp: viewport.New(), in: in}
	m.refreshContacts()
	p := tea.NewProgram(m, opts...)
	go func() {
		for {
			select {
			case ev := <-ctl.E.Events():
				p.Send(evMsg{ev})
			case <-ctx.Done():
				p.Quit()
				return
			}
		}
	}()
	if _, err := p.Run(); err != nil {
		fmt.Fprintln(stderr, "burrow: the full-screen chat needs a terminal ("+err.Error()+"); --plain works without one")
		return 1
	}
	return 0
}

func (m *model) Init() tea.Cmd {
	cmds := []tea.Cmd{textinput.Blink}
	if m.target.InviteBytes != nil {
		inv := m.target.InviteBytes
		m.target.InviteBytes = nil
		cmds = append(cmds, m.queue(func() outMsg { return outMsg{lines: m.ctl.ConnectInvite(m.ctx, inv)} }))
	} else if m.target.Invite != "" {
		cmds = append(cmds, m.exec("/connect "+m.target.Invite))
	} else if m.target.Name != "" {
		cmds = append(cmds, m.exec("/connect "+m.target.Name))
	}
	m.note("listening on "+m.addrs+" — your fingerprint "+m.ctl.E.Identity().Display, Keys, "type /help for commands")
	m.note(m.ctl.StartupNotes(false)...)
	return tea.Batch(cmds...)
}

// exec runs a controller command off the render thread.
func (m *model) exec(line string) tea.Cmd {
	return m.queue(func() outMsg {
		out, quit := m.ctl.Exec(m.ctx, line)
		return outMsg{lines: out, quit: quit}
	})
}

// queue runs fn off the render thread, after every command queued before it.
func (m *model) queue(fn func() outMsg) tea.Cmd {
	m.busy++
	n := m.order.ticket()
	return func() tea.Msg {
		var out outMsg
		m.order.run(n, func() { out = fn() })
		return out
	}
}

func (m *model) refreshContacts() {
	m.contacts = m.ctl.E.Contacts()
	sort.Slice(m.contacts, func(i, j int) bool { return m.contacts[i].Nickname < m.contacts[j].Nickname })
}

func (m *model) selectOffset(d int) {
	if len(m.contacts) == 0 {
		return
	}
	idx := -1
	if current, ok := m.ctl.Selected(); ok {
		for i, c := range m.contacts {
			if c.ID == current {
				idx = i
			}
		}
	}
	idx = ((idx+d)%len(m.contacts) + len(m.contacts)) % len(m.contacts)
	m.typing(false)
	m.ctl.Select(m.contacts[idx].ID)
}

// typing tells the selected contact that we started or stopped typing, once
// per change, and only when typing indicators are on.
func (m *model) typing(now bool) {
	current, selected := m.ctl.Selected()
	switch {
	case now && m.typingTo == nil && selected && m.ctl.TypingOn():
		m.ctl.E.SetTyping(current, true)
		m.typingTo = &current
	case !now && m.typingTo != nil:
		m.ctl.E.SetTyping(*m.typingTo, false)
		m.typingTo = nil
	}
}

func (m *model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.w, m.h = msg.Width, msg.Height
		m.layout()
	case tea.KeyPressMsg:
		switch msg.String() {
		case "ctrl+c":
			return m, tea.Quit
		case "tab":
			m.selectOffset(1)
		case "shift+tab":
			m.selectOffset(-1)
		case "pgup":
			m.vp.PageUp()
		case "pgdown":
			m.vp.PageDown()
		case "enter":
			line := m.in.Value()
			m.in.Reset()
			m.typing(false)
			m.setAlert("")
			if strings.TrimSpace(line) == "" {
				return m, nil
			}
			if line == "/quit" || line == "/q" {
				return m, tea.Quit
			}
			if strings.HasPrefix(line, "/view") {
				return m, m.view(strings.TrimSpace(strings.TrimPrefix(line, "/view")))
			}
			// The line is for the conversation on screen now, even if
			// another is selected by the time it runs.
			current, selected := m.ctl.Selected()
			return m, m.queue(func() outMsg {
				out, quit := m.ctl.ExecFor(m.ctx, line, current, selected)
				if selected && !strings.HasPrefix(line, "/") && len(out) == 1 && !strings.HasPrefix(out[0], "! ") {
					return outMsg{to: current, mine: line} // sent: show what was said, not that it was queued
				}
				return outMsg{lines: out, quit: quit}
			})
		default:
			var cmd tea.Cmd
			m.in, cmd = m.in.Update(msg)
			m.typing(m.in.Value() != "" && !strings.HasPrefix(m.in.Value(), "/"))
			return m, cmd
		}
	case noteMsg:
		m.note(string(msg))
	case outMsg:
		m.busy--
		if msg.quit {
			return m, tea.Quit
		}
		if msg.mine != "" {
			m.log(&msg.to, nil, common.Mine(msg.mine))
		}
		m.note(msg.lines...)
		m.refreshContacts()
	case evMsg:
		m.ctl.Observe(msg.ev)
		m.onEvent(msg.ev)
		if d, ok := msg.ev.(core.TransferDone); ok && d.Image && !d.Peer.Outgoing && common.DetectTerminal() != common.TermNone {
			m.render()
			return m, m.inline(&d.Peer.Peer, d.Path, common.DetectTerminal(), min(thumbCols, m.vp.Width()))
		}
	case inlineMsg:
		// The placeholder rows scroll with the conversation; the upload goes out raw, once.
		for _, l := range msg.img.Lines {
			m.add(msg.peer, entry{text: l, image: true})
		}
		m.render()
		if msg.img.Transmit == "" {
			return m, nil // half-block preview: nothing to upload
		}
		return m, tea.Raw(msg.img.Transmit)
	default:
		var cmd tea.Cmd
		m.in, cmd = m.in.Update(msg)
		return m, cmd
	}
	m.render()
	return m, nil
}

func (m *model) onEvent(ev core.Event) {
	line := m.ctl.Names.Line(ev)
	current, selected := m.ctl.Selected()
	unread := func(p core.PeerID, mark string) {
		if (!selected || p != current) && m.unread[p] != "!" {
			m.unread[p] = mark
		}
	}
	switch e := ev.(type) {
	case core.MessageReceived:
		m.log(&e.Peer, nil, line)
		unread(e.Peer, "•")
	case core.MessageStatus:
		if e.Status == core.StatusPending || e.Status == core.StatusFailed {
			m.log(&e.Peer, dim.Render, line)
		}
	case core.Typing:
		if line != "" {
			m.log(&e.Peer, dim.Render, line)
		}
	case core.ImageOffered:
		n := m.ctl.Number(e.ID)
		m.log(&e.Peer, bold.Render, fmt.Sprintf("* offer #%d, %s%s — /accept %d or /reject %d", n, common.Offer(e), captionOf(e.Caption), n, n))
		unread(e.Peer, "!")
	case core.TransferDone:
		p := &e.Peer.Peer
		switch {
		case e.Peer.Outgoing:
			m.log(p, dim.Render, "* "+common.Kind(e.Image)+" delivered")
		case !e.Image:
			m.log(p, nil, "* file saved: "+e.Path)
		default:
			m.log(p, nil, fmt.Sprintf("* image saved: %s  (/view %d)", e.Path, len(m.ctl.Images())))
		}
	case core.TransferFailed:
		m.log(&e.Peer.Peer, alert.Render, line)
	case core.NameCollision:
		m.log(nil, alert.Render, line)
		m.setAlert("NAME COLLISION: two contacts use the same name; see the notes")
	case core.HandshakeFailed:
		if e.Retry { // one of many while a contact is away
			m.log(nil, dim.Render, line)
		} else {
			m.log(nil, alert.Render, line)
		}
	default:
		if line != "" {
			m.note(line)
		}
	}
	m.refreshContacts()
	if !selected && len(m.contacts) == 1 {
		m.ctl.Select(m.contacts[0].ID)
	}
}

// setAlert shows text in a row above the status bar until the next Enter;
// "" removes the row.
func (m *model) setAlert(text string) {
	if text != m.alert {
		m.alert = text
		m.layout()
	}
}

func (m *model) layout() {
	w := m.w - sidebarWidth - 1
	if w < 20 {
		w = 20
	}
	h := m.h - 3
	if m.alert != "" {
		h-- // the alert takes a row of its own
	}
	if h < 3 {
		h = 3
	}
	m.vp.SetWidth(w)
	m.vp.SetHeight(h)
	m.in.SetWidth(m.w - 4)
	m.dirty = true
	m.render()
}

// render fills the viewport with the notes and the selected conversation,
// merged in the order their lines were added. It does nothing when neither
// changed: a busy conversation that is not on screen costs no rendering.
func (m *model) render() {
	current, selected := m.ctl.Selected()
	var conv []entry
	if selected {
		delete(m.unread, current)
		if l := m.logs[current]; l != nil {
			conv = l.lines
		}
		if m.shown == nil || *m.shown != current {
			m.shown, m.dirty = &current, true
		}
	} else if m.shown != nil {
		m.shown, m.dirty = nil, true
	}
	if !m.dirty {
		return
	}
	m.dirty = false
	w := m.vp.Width()
	rows := make([]string, 0, len(m.system.lines)+len(conv))
	notes := m.system.lines
	for len(notes) > 0 || len(conv) > 0 {
		if len(conv) == 0 || len(notes) > 0 && notes[0].seq < conv[0].seq {
			rows = append(rows, notes[0].fit(w))
			notes = notes[1:]
		} else {
			rows = append(rows, conv[0].fit(w))
			conv = conv[1:]
		}
	}
	m.vp.SetContent(strings.Join(rows, "\n"))
	m.vp.GotoBottom()
}

func (m *model) View() tea.View {
	side := []string{bold.Render("contacts")}
	current, selected := m.ctl.Selected()
	for _, c := range m.contacts {
		// Presence, verification and news stand in columns of their own,
		// before the name: whatever a contact called itself cannot reach them.
		mark, v, news := " ", "?", " "
		if c.Online {
			mark = "*"
		}
		if c.Verified {
			v = "✓"
		}
		if u := m.unread[c.ID]; u != "" {
			news = u
		}
		label := truncate(mark+v+news+" "+c.Nickname, sidebarWidth-1)
		if selected && c.ID == current {
			label = status.Render(label)
		}
		side = append(side, label)
	}
	sidebar := lipgloss.NewStyle().Width(sidebarWidth).Height(m.vp.Height()).Render(strings.Join(side, "\n"))
	body := lipgloss.JoinHorizontal(lipgloss.Top, sidebar, dim.Render("│"), m.vp.View())
	st := m.ctl.Status()
	if m.busy > 0 {
		st += " · working…"
	}
	rows := []string{body, status.Render(padRight(st, m.w)), "> " + m.in.View()}
	if m.alert != "" {
		rows = slices.Insert(rows, 1, alert.Render("! "+m.alert))
	}
	v := tea.NewView(strings.Join(rows, "\n"))
	v.AltScreen = true
	return v
}

func captionOf(c string) string {
	if c == "" {
		return ""
	}
	return fmt.Sprintf(" %q", c)
}

const (
	thumbCols = 32 // width of the preview shown when an image arrives
	viewCols  = 72 // width of /view
)

// view shows received image n in the conversation, larger than the preview.
func (m *model) view(arg string) tea.Cmd {
	imgs := m.ctl.Images()
	n, err := strconv.Atoi(arg)
	if err != nil || n < 1 || n > len(imgs) {
		return func() tea.Msg { return noteMsg("! usage: /view <n> (see /images)") }
	}
	kind := common.DetectTerminal()
	if kind == common.TermNone {
		return func() tea.Msg {
			return noteMsg("! this terminal cannot show images inline; the file is at " + imgs[n-1])
		}
	}
	return m.inline(nil, imgs[n-1], kind, min(viewCols, m.vp.Width()))
}

type inlineMsg struct {
	peer *core.PeerID // the conversation it belongs to; nil for the system notes
	img  common.InlineImage
}

// inline decodes an image off the render thread and prepares it for inline
// display, cols columns wide: Kitty Unicode placeholders where available,
// otherwise a half-block rendering (iTerm2 and other true-colour terminals).
func (m *model) inline(peer *core.PeerID, path string, kind common.TermImage, cols int) tea.Cmd {
	m.nextImage++
	if m.nextImage == 0 {
		m.nextImage = 1
	}
	id := m.nextImage
	return func() tea.Msg {
		img, err := core.DecodeImage(m.ctx, path, 20*cols)
		if err != nil {
			return noteMsg("! cannot show " + path + ": " + err.Error())
		}
		if kind == common.TermKitty {
			if in, err := common.KittyInline(img, id, cols); err == nil {
				return inlineMsg{peer: peer, img: in}
			}
			return nil
		}
		if lines := common.HalfBlocks(img, cols); len(lines) > 0 {
			return inlineMsg{peer: peer, img: common.InlineImage{Lines: lines}}
		}
		return nil
	}
}

// truncate cuts s to at most w columns without splitting a character.
func truncate(s string, w int) string {
	for lipgloss.Width(s) > w {
		_, size := utf8.DecodeLastRuneInString(s)
		s = s[:len(s)-size]
	}
	return s
}

func padRight(s string, w int) string {
	if n := w - lipgloss.Width(s); n > 0 {
		return s + strings.Repeat(" ", n)
	}
	return s
}
