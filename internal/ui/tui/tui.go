// Package tui is the Bubble Tea terminal UI (CLAUDE.md §9.2).
package tui

import (
	"bufio"
	"context"
	"fmt"
	"net"
	"os"
	"sort"
	"strconv"
	"strings"

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
type outMsg struct {
	lines []string
	quit  bool
}

const sidebarWidth = 26

var (
	dim    = lipgloss.NewStyle().Faint(true)
	bold   = lipgloss.NewStyle().Bold(true)
	alert  = lipgloss.NewStyle().Foreground(lipgloss.Color("1")).Bold(true)
	status = lipgloss.NewStyle().Reverse(true)
)

type model struct {
	ctx        context.Context
	ctl        *common.Controller
	target     core.Target
	addrs      string
	contacts   []core.Contact
	logs       map[core.PeerID][]string
	system     []string
	vp         viewport.Model
	in         textinput.Model
	w, h       int
	busy       bool
	prog       *tea.Program
	showInline func()
}

// Run starts the TUI and blocks until it exits.
func Run(ctx context.Context, ctl *common.Controller, target core.Target, addrs []net.Addr) int {
	var as []string
	for _, a := range addrs {
		as = append(as, a.String())
	}
	in := textinput.New()
	in.Placeholder = "message or /command"
	in.Focus()
	m := &model{ctx: ctx, ctl: ctl, target: target, addrs: strings.Join(as, " "), logs: map[core.PeerID][]string{},
		vp: viewport.New(), in: in}
	m.refreshContacts()
	p := tea.NewProgram(m)
	m.prog = p
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
		return 1
	}
	return 0
}

func (m *model) Init() tea.Cmd {
	cmds := []tea.Cmd{textinput.Blink}
	if m.target.Invite != "" {
		cmds = append(cmds, m.exec("/connect "+m.target.Invite))
	} else if m.target.Name != "" {
		cmds = append(cmds, m.exec("/connect "+m.target.Name))
	}
	m.system = append(m.system, "listening on "+m.addrs+" — your fingerprint "+m.ctl.E.Identity().Display, Keys, "type /help for commands")
	return tea.Batch(cmds...)
}

// exec runs a controller command off the render thread.
func (m *model) exec(line string) tea.Cmd {
	return func() tea.Msg {
		out, quit := m.ctl.Exec(m.ctx, line)
		return outMsg{lines: out, quit: quit}
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
	if m.ctl.Current != nil {
		for i, c := range m.contacts {
			if c.ID == *m.ctl.Current {
				idx = i
			}
		}
	}
	idx = ((idx+d)%len(m.contacts) + len(m.contacts)) % len(m.contacts)
	id := m.contacts[idx].ID
	m.ctl.Current = &id
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
			if strings.TrimSpace(line) == "" {
				return m, nil
			}
			if line == "/quit" || line == "/q" {
				return m, tea.Quit
			}
			if strings.HasPrefix(line, "/view") {
				return m, m.view(strings.TrimSpace(strings.TrimPrefix(line, "/view")))
			}
			m.busy = true
			return m, m.exec(line)
		default:
			var cmd tea.Cmd
			m.in, cmd = m.in.Update(msg)
			if m.ctl.Typing && m.ctl.Current != nil && msg.Text != "" {
				m.ctl.E.SetTyping(*m.ctl.Current, true)
			}
			return m, cmd
		}
	case viewMsg:
		if m.showInline != nil && m.prog != nil {
			show := m.showInline
			m.showInline = nil
			_ = m.prog.ReleaseTerminal()
			show()
			_ = m.prog.RestoreTerminal()
		}
		return m, nil
	case outMsg:
		m.busy = false
		if msg.quit {
			return m, tea.Quit
		}
		m.system = append(m.system, msg.lines...)
		m.refreshContacts()
	case evMsg:
		m.ctl.Observe(msg.ev)
		m.onEvent(msg.ev)
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
	switch e := ev.(type) {
	case core.MessageReceived:
		m.logs[e.Peer] = append(m.logs[e.Peer], line)
	case core.MessageStatus:
		if e.Status == core.StatusPending || e.Status == core.StatusFailed {
			m.logs[e.Peer] = append(m.logs[e.Peer], dim.Render(line))
		}
	case core.Typing:
		if line != "" {
			m.logs[e.Peer] = append(m.logs[e.Peer], dim.Render(line))
		}
	case core.ImageOffered:
		m.logs[e.Peer] = append(m.logs[e.Peer], bold.Render(fmt.Sprintf("* image offer #%d: %s %s %d×%d%s — /accept %d or /reject %d",
			m.ctl.Number(e.ID), common.Size(e.Size), common.FormatName(e.Format), e.Width, e.Height, captionOf(e.Caption), m.ctl.Number(e.ID), m.ctl.Number(e.ID))))
	case core.TransferDone:
		if e.Peer.Outgoing {
			m.logs[e.Peer.Peer] = append(m.logs[e.Peer.Peer], dim.Render("* image delivered"))
		} else {
			n := len(m.ctl.Images())
			m.logs[e.Peer.Peer] = append(m.logs[e.Peer.Peer], fmt.Sprintf("* image saved: %s  (/view %d)", e.Path, n))
		}
	case core.TransferFailed:
		m.logs[e.Peer.Peer] = append(m.logs[e.Peer.Peer], alert.Render(line))
	case core.NameCollision, core.HandshakeFailed:
		m.system = append(m.system, alert.Render(line))
	default:
		if line != "" {
			m.system = append(m.system, line)
		}
	}
	m.refreshContacts()
	if m.ctl.Current == nil && len(m.contacts) == 1 {
		id := m.contacts[0].ID
		m.ctl.Current = &id
	}
}

func (m *model) layout() {
	w := m.w - sidebarWidth - 1
	if w < 20 {
		w = 20
	}
	h := m.h - 3
	if h < 3 {
		h = 3
	}
	m.vp.SetWidth(w)
	m.vp.SetHeight(h)
	m.in.SetWidth(m.w - 4)
	m.render()
}

func (m *model) render() {
	var lines []string
	if m.ctl.Current != nil {
		lines = m.logs[*m.ctl.Current]
	}
	all := append(append([]string{}, m.system...), lines...)
	if len(m.system) > 0 && len(lines) > 0 {
		all = append(append([]string{}, m.system...), append([]string{dim.Render("────")}, lines...)...)
	}
	m.vp.SetContent(strings.Join(all, "\n"))
	m.vp.GotoBottom()
}

func (m *model) View() tea.View {
	side := []string{bold.Render("contacts")}
	for _, c := range m.contacts {
		mark := " "
		if c.Online {
			mark = "*"
		}
		v := " "
		if c.Verified {
			v = "✓"
		}
		label := fmt.Sprintf("%s%s %s", mark, v, c.Nickname)
		if len(label) > sidebarWidth-1 {
			label = label[:sidebarWidth-1]
		}
		if m.ctl.Current != nil && c.ID == *m.ctl.Current {
			label = status.Render(label)
		}
		side = append(side, label)
	}
	sidebar := lipgloss.NewStyle().Width(sidebarWidth).Height(m.vp.Height()).Render(strings.Join(side, "\n"))
	body := lipgloss.JoinHorizontal(lipgloss.Top, sidebar, dim.Render("│"), m.vp.View())
	st := "me " + m.ctl.E.Identity().ID.Short()
	if m.ctl.Current != nil {
		if c, err := m.ctl.E.Contact(*m.ctl.Current); err == nil {
			ver := "UNVERIFIED"
			if c.Verified {
				ver = "verified"
			}
			on := "offline"
			if c.Online {
				on = "online"
			}
			st += fmt.Sprintf(" · talking to %s (%s, %s, %s)", c.Nickname, c.ID.Short(), ver, on)
		}
	}
	if m.busy {
		st += " · working…"
	}
	v := tea.NewView(strings.Join([]string{body, status.Render(padRight(st, m.w)), "> " + m.in.View()}, "\n"))
	v.AltScreen = true
	return v
}

func captionOf(c string) string {
	if c == "" {
		return ""
	}
	return fmt.Sprintf(" %q", c)
}

// view shows received image n inline when the terminal supports it. The
// terminal is released for the duration; any key returns to the chat.
func (m *model) view(arg string) tea.Cmd {
	imgs := m.ctl.Images()
	n, err := strconv.Atoi(arg)
	if err != nil || n < 1 || n > len(imgs) {
		return func() tea.Msg { return outMsg{lines: []string{"! usage: /view <n> (see /images)"}} }
	}
	kind := common.DetectTerminal()
	if kind == common.TermNone {
		return func() tea.Msg {
			return outMsg{lines: []string{"! this terminal cannot show images inline; the file is at " + imgs[n-1]}}
		}
	}
	return func() tea.Msg {
		img, err := core.DecodeImage(imgs[n-1], 800)
		if err != nil {
			return outMsg{lines: []string{"! cannot decode image: " + err.Error()}}
		}
		m.showInline = func() {
			_ = common.WriteImage(os.Stdout, img, kind)
			fmt.Fprint(os.Stdout, "\n(press Enter to return)")
			_, _ = bufio.NewReader(os.Stdin).ReadString('\n')
		}
		return viewMsg{}
	}
}

type viewMsg struct{}

func padRight(s string, w int) string {
	if n := w - lipgloss.Width(s); n > 0 {
		return s + strings.Repeat(" ", n)
	}
	return s
}
