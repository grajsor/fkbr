package tui

import (
	"fmt"
	"path"
	"sort"
	"strings"
	"time"

	"github.com/mattn/go-runewidth"

	"fkbr/internal/api"
)

// level is what a pane is showing. ".." walks up: a directory, then the
// node's roots, then the list of nodes.
type level int

const (
	levelNodes level = iota
	levelRoots
	levelDir
)

// pageSize is how many entries are fetched per List call; more are loaded
// as the cursor approaches the end.
const pageSize = 2000

type row struct {
	name  string
	up    bool // the ".." row
	isDir bool
	entry api.Entry // levelDir only
	info  string    // short text for nodes/roots
	dim   bool      // offline node, read-only root
}

type pane struct {
	level    level
	node     string
	root     string
	path     string // root-relative, "." at the root
	rows     []row
	total    int // entries in the directory (levelDir)
	loaded   int // entries fetched so far
	cursor   int
	offset   int
	selected map[string]bool
	loading  bool
	err      string
	seq      int    // drops stale List responses
	want     string // name to put the cursor on after a reload
	// single: the node has one root, so the roots level is skipped and the
	// root is not shown in the title.
	single bool
}

func newPane() *pane { return &pane{level: levelNodes, path: ".", selected: map[string]bool{}} }

// title is shown in the pane border.
func (p *pane) title() string {
	switch p.level {
	case levelNodes:
		return "nodes"
	case levelRoots:
		return p.node + ":"
	}
	switch {
	case p.single && p.path == ".":
		return p.node + ":/"
	case p.single:
		return p.node + ":/" + p.path
	case p.path == ".":
		return p.node + ":" + p.root
	}
	return p.node + ":" + p.root + "/" + p.path
}

func (p *pane) current() *row {
	if p.cursor >= 0 && p.cursor < len(p.rows) {
		return &p.rows[p.cursor]
	}
	return nil
}

func (p *pane) move(delta, height int) {
	p.cursor = max(0, min(len(p.rows)-1, p.cursor+delta))
	p.scroll(height)
}

func (p *pane) scroll(height int) {
	if height < 1 {
		return
	}
	if p.cursor < p.offset {
		p.offset = p.cursor
	}
	if p.cursor >= p.offset+height {
		p.offset = p.cursor - height + 1
	}
	p.offset = max(0, min(p.offset, max(0, len(p.rows)-height)))
}

// selection returns selected entry names, or the entry under the cursor.
func (p *pane) selection() []string {
	if p.level != levelDir {
		return nil
	}
	var names []string
	for _, r := range p.rows {
		if !r.up && p.selected[r.name] {
			names = append(names, r.name)
		}
	}
	if len(names) == 0 {
		if r := p.current(); r != nil && !r.up {
			names = []string{r.name}
		}
	}
	return names
}

func (p *pane) relPath(name string) string {
	return path.Join(p.path, name)
}

func (p *pane) clearSelection() { p.selected = map[string]bool{} }

// setNodes fills a nodes-level pane, keeping the cursor on the same node.
func (p *pane) setNodes(nodes []api.Node) {
	keep := ""
	if r := p.current(); r != nil {
		keep = r.name
	}
	p.rows = p.rows[:0]
	for _, n := range nodes {
		state := "offline"
		if n.Online {
			state = "online"
		}
		info := state
		if len(n.Roles) > 0 {
			info += "  " + strings.Join(n.Roles, ",")
		}
		if n.Self {
			info += "  (this node)"
		}
		p.rows = append(p.rows, row{name: n.ID, isDir: true, info: info, dim: !n.Online})
	}
	p.cursorTo(keep)
}

func (p *pane) setRoots(n api.Node) {
	p.rows = []row{{name: "..", up: true, isDir: true}}
	for _, r := range n.Roots {
		access := "ro"
		if r.Writable {
			access = "rw"
		}
		p.rows = append(p.rows, row{name: r.ID, isDir: true, info: access + "  " + r.Path, dim: !r.Writable})
	}
	p.cursorTo(p.want)
	p.want = ""
}

// setEntries replaces (offset 0) or extends a directory listing. Within
// what is loaded, directories sort before files.
func (p *pane) setEntries(res *api.ListResult) {
	if res.Offset == 0 {
		keep := p.want
		if keep == "" {
			if r := p.current(); r != nil {
				keep = r.name
			}
		}
		p.rows = []row{{name: "..", up: true, isDir: true}}
		p.loaded = 0
		p.want = keep
		// Drop selections that no longer exist.
		present := map[string]bool{}
		for _, e := range res.Entries {
			present[e.Name] = true
		}
		for n := range p.selected {
			if !present[n] {
				delete(p.selected, n)
			}
		}
	}
	for _, e := range res.Entries {
		p.rows = append(p.rows, row{name: e.Name, isDir: e.Type == api.TypeDir, entry: e})
	}
	p.loaded += len(res.Entries)
	p.total = res.Total
	body := p.rows[1:]
	sort.SliceStable(body, func(i, j int) bool {
		if body[i].isDir != body[j].isDir {
			return body[i].isDir
		}
		return body[i].name < body[j].name
	})
	if res.Offset == 0 {
		p.cursorTo(p.want)
		p.want = ""
	}
}

func (p *pane) cursorTo(name string) {
	p.cursor = 0
	for i, r := range p.rows {
		if r.name == name {
			p.cursor = i
			return
		}
	}
}

func (p *pane) selectedBytes() (n int, bytes int64) {
	for _, r := range p.rows {
		if p.selected[r.name] {
			n++
			bytes += r.entry.Size
		}
	}
	return
}

// render draws the pane's content area (without border).
// render draws the rows. On the active cursor row a name that does not fit
// scrolls, marquee elapsed into its cycle (see anim.go).
func (p *pane) render(st *styles, width, height int, active bool, marquee time.Duration) []string {
	lines := make([]string, 0, height)
	if p.err != "" {
		lines = append(lines, st.err.Render(fit("! "+p.err, width)))
	}
	if p.loading && len(p.rows) == 0 {
		lines = append(lines, st.dim.Render(fit("loading…", width)))
	}
	p.scroll(height - len(lines))
	for i := p.offset; i < len(p.rows) && len(lines) < height; i++ {
		r := p.rows[i]
		cur := i == p.cursor && active
		text := p.formatRow(r, width, cur, marquee)
		marked := p.selected[r.name] && !r.up
		if cur {
			s := st.cursor
			if marked {
				s = st.cursorMarked
			}
			lines = append(lines, st.highlight(s, text))
			continue
		}
		s := st.file
		switch {
		case marked:
			s = st.marked
		case r.dim:
			s = st.dim
		case r.isDir:
			s = st.dir
		}
		lines = append(lines, s.Render(text))
	}
	for len(lines) < height {
		lines = append(lines, st.panel.Render(strings.Repeat(" ", width)))
	}
	return lines
}

func (p *pane) formatRow(r row, width int, scroll bool, marquee time.Duration) string {
	if p.level != levelDir || r.up {
		name := r.name
		if r.up {
			name = "/.."
		}
		if r.info == "" {
			if scroll {
				name = scrolled(name, width, marquee)
			}
			return fit(name, width)
		}
		nw := min(max(12, width/3), width)
		if scroll {
			name = scrolled(name, nw, marquee)
		}
		return fit(fit(name, nw)+" "+r.info, width)
	}
	// name | size | date
	const dateW = 12 // "Jan 02 15:04" or "Jan 02  2006"
	const sizeW = 7
	nameW := width - dateW - sizeW - 2
	name := r.name
	if r.isDir {
		name = "/" + name
	} else if r.entry.Type == api.TypeSymlink {
		name = "@" + name
	}
	if nameW < 8 { // too narrow for columns
		if scroll {
			name = scrolled(name, width, marquee)
		}
		return fit(name, width)
	}
	if scroll {
		name = scrolled(name, nameW, marquee)
	}
	size := humanSize(r.entry.Size)
	if r.isDir {
		size = "<DIR>"
	}
	return fit(name, nameW) + " " + runewidth.FillLeft(size, sizeW) + " " + shortTime(r.entry.MtimeNs)
}

// footer describes the selection or the entry under the cursor.
func (p *pane) footer(width int) string {
	if n, b := p.selectedBytes(); n > 0 {
		return fit(fmt.Sprintf("%d selected, %s", n, humanSize(b)), width)
	}
	r := p.current()
	switch {
	case p.level == levelDir && p.loaded < p.total:
		return fit(fmt.Sprintf("%d of %d entries loaded", p.loaded, p.total), width)
	case r == nil || r.up:
		return strings.Repeat(" ", width)
	case p.level != levelDir:
		return fit(r.name+"  "+r.info, width)
	}
	e := r.entry
	s := fmt.Sprintf("%s %d:%d %s", modeString(e), e.UID, e.GID, time.Unix(0, e.MtimeNs).Format("2006-01-02 15:04:05"))
	if e.LinkTarget != "" {
		s += " -> " + e.LinkTarget
	}
	return fit(s, width)
}

// clip truncates s to at most w terminal cells.
func clip(s string, w int) string {
	return runewidth.Truncate(s, max(1, w), "…")
}

// fit truncates or pads s to exactly w terminal cells.
func fit(s string, w int) string {
	if w <= 0 {
		return ""
	}
	if runewidth.StringWidth(s) > w {
		s = runewidth.Truncate(s, w, "…")
	}
	return runewidth.FillRight(s, w)
}

func humanSize(n int64) string {
	const unit = 1024
	if n < 100000 {
		return fmt.Sprint(n)
	}
	v, suffix := float64(n), ""
	for _, s := range []string{"K", "M", "G", "T", "P"} {
		v /= unit
		suffix = s
		if v < 1000 {
			break
		}
	}
	if v < 10 {
		return fmt.Sprintf("%.1f%s", v, suffix)
	}
	return fmt.Sprintf("%.0f%s", v, suffix)
}

func shortTime(ns int64) string {
	t := time.Unix(0, ns)
	if time.Since(t) < 180*24*time.Hour && t.Before(time.Now().Add(time.Hour)) {
		return t.Format("Jan 02 15:04")
	}
	return t.Format("Jan 02  2006")
}

func modeString(e api.Entry) string {
	b := []byte("----------")
	switch e.Type {
	case api.TypeDir:
		b[0] = 'd'
	case api.TypeSymlink:
		b[0] = 'l'
	case api.TypeOther:
		b[0] = '?'
	}
	const rwx = "rwxrwxrwx"
	for i := range 9 {
		if e.Mode&(1<<(8-i)) != 0 {
			b[i+1] = rwx[i]
		}
	}
	for _, sp := range []struct {
		bit      uint32
		pos      int
		set, off byte
	}{{0o4000, 3, 's', 'S'}, {0o2000, 6, 's', 'S'}, {0o1000, 9, 't', 'T'}} {
		if e.Mode&sp.bit != 0 {
			if b[sp.pos] == 'x' {
				b[sp.pos] = sp.set
			} else {
				b[sp.pos] = sp.off
			}
		}
	}
	return string(b)
}
