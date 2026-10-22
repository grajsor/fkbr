package tui

// An ssh swarm's home node (api.Linker): its hosts in the node list,
// linking one (F7 there) and unlinking (F8). Also where the panes start
// (Options.StartInDirs).

import (
	"slices"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"fkbr/internal/api"
)

// linksEvery is how often the hosts' link state is read again.
const linksEvery = 2 * time.Second

type linksTickMsg struct{}

func linksTick() tea.Cmd {
	return tea.Tick(linksEvery, func(time.Time) tea.Msg { return linksTickMsg{} })
}

// linker is the client's api.Linker, if this node links ssh hosts.
func (m *model) linker() api.Linker {
	l, ok := m.c.(api.Linker)
	if !ok {
		return nil
	}
	if _, err := l.Links(m.ctx); err != nil {
		return nil // a node on a share
	}
	return l
}

func (m *model) fetchLinks() []api.Link {
	l, ok := m.c.(api.Linker)
	if !ok {
		return nil
	}
	ls, err := l.Links(m.ctx)
	if err != nil {
		return nil
	}
	return ls
}

// linkRows are the hosts whose node is not in the list yet, with why.
func linkRows(nodes []api.Node, links []api.Link) []row {
	var rows []row
	for _, l := range links {
		if l.Node != "" && slices.ContainsFunc(nodes, func(n api.Node) bool { return n.ID == l.Node }) {
			continue // its node's row says how it is linked
		}
		info := "ssh: " + l.State
		if l.Error != "" {
			info += ": " + l.Error
		}
		rows = append(rows, row{name: l.Host, info: info, dim: true, link: true})
	}
	return rows
}

// placePanes opens the panes, once, in this node's directory and in
// another online node or this node's local root.
func (m *model) placePanes() tea.Cmd {
	if !m.startPanes || m.placed {
		return nil
	}
	var self *api.Node
	for _, n := range m.sortedNodes() {
		if n.Self {
			self = &n
			break
		}
	}
	if self == nil || len(self.Roots) == 0 {
		return nil // not registered yet: the next node list
	}
	m.placed = true
	var cmds []tea.Cmd
	open := func(i int, n api.Node, root string) {
		p := m.panes[i]
		p.level, p.node, p.root, p.path = levelDir, n.ID, root, "."
		p.rows, p.err, p.single = nil, "", len(n.Roots) == 1
		p.clearSelection()
		cmds = append(cmds, m.load(i, 0))
	}
	open(0, *self, self.Roots[0].ID)
	for _, n := range m.sortedNodes() {
		if !n.Self && n.Online && len(n.Roots) > 0 {
			open(1, n, n.Roots[0].ID)
			return tea.Batch(cmds...)
		}
	}
	for _, r := range self.Roots {
		if r.Local {
			open(1, *self, r.ID)
			break
		}
	}
	return tea.Batch(cmds...)
}

// openConnect asks for a host to link (F7 in a node list).
func (m *model) openConnect() {
	l := m.linker()
	m.dlg = newInputDialog("Link a host over ssh (kept in the config file)", "host[:/dir]: ", func(host string) tea.Cmd {
		return func() tea.Msg {
			if err := l.AddLink(m.ctx, host, true); err != nil {
				return statusMsg{text: "link " + host + ": " + err.Error(), err: true}
			}
			return statusMsg{text: "linking " + host + "; it shows in the node list once it is up"}
		}
	})
}

// openDisconnect unlinks the host under the cursor (F8 in a node list;
// marks there are @dest, not for this).
func (m *model) openDisconnect() {
	r := m.panes[m.active].current()
	if r == nil || r.up {
		return
	}
	host := m.hostOf(r.name)
	if host == "" {
		m.setStatus(r.name+" is not linked over ssh; F8 in the node list unlinks ssh hosts", true)
		return
	}
	l := m.linker()
	m.dlg = &confirmDialog{
		title: "Unlink " + host + "? Its node leaves, and it is removed from the config file.",
		onYes: func() tea.Cmd {
			return tea.Batch(func() tea.Msg {
				if err := l.RemoveLink(m.ctx, host); err != nil {
					return statusMsg{text: "unlink " + host + ": " + err.Error(), err: true}
				}
				return statusMsg{text: "unlinked " + host}
			}, m.loadNodes())
		},
	}
}

// hostOf is the ssh host of a node list row: a host with no node yet, or
// a node linked over ssh.
func (m *model) hostOf(name string) string {
	for _, l := range m.links {
		if l.Host == name || (l.Node != "" && l.Node == name) {
			return l.Host
		}
	}
	return ""
}
