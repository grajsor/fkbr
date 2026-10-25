package tui

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"

	"fkbr/internal/api"
	"fkbr/internal/config"
	"fkbr/internal/exclude"
	"fkbr/internal/product"
	"fkbr/internal/safetext"
)

// The settings dialog (F11): the settings people change most, kept in the
// config file (Options.ConfigPath: -config, $FKBR_CONFIG, fkbr.conf
// beside the binary, else ~/.fkbr.conf). Only the keys changed are
// written (config.SaveSettings), the rest of the file stays as it is.
// The TUI's own settings apply when saved; the node's when it starts.

// prefs are the TUI's own settings and where they are kept.
type prefs struct {
	configPath string
	overridden map[string]string // config key -> the flag that sets it this run
	colors     string
	mouse      bool
	ifExists   int // index in conflictPolicies
	compress   bool
	exclude    string
}

func prefsFrom(o Options) prefs {
	p := prefs{configPath: o.ConfigPath, overridden: o.Overridden, colors: o.Colors, mouse: !o.NoMouse,
		compress: o.Compress, exclude: o.Exclude}
	p.ifExists = max(0, slices.Index(conflictPolicies, api.ConflictPolicy(o.IfExists)))
	return p
}

type settingKind int

const (
	choiceSetting settingKind = iota // cycles through its choices
	textSetting                      // typed in, checked
	infoSetting                      // shown only
)

// settingRow is one line of the dialog.
type settingRow struct {
	section string // starts a section with this title
	key     string // the config key
	label   string
	kind    settingKind
	choices []string // a choice's values; the first is the default
	def     string   // a text's default, in words
	help    string   // shown while the row has the cursor
	now     bool     // applies when saved (else when fkbr starts again)
	// load is the row's value in a config file ("": not set); save turns
	// a value back into what goes in the file (nil: the key goes).
	load func(c *config.Config) string
	save func(v string) (any, error)
}

func settingRows() []settingRow {
	str := func(get func(c *config.Config) string) func(*config.Config) string { return get }
	return []settingRow{
		{section: "APPEARANCE · applies at once", key: "tui.colors", label: "Colours", kind: choiceSetting,
			choices: config.TUIColors, now: true,
			help: "auto finds out what the terminal shows; none: no colour at all",
			load: str(func(c *config.Config) string { return c.TUI.Colors }), save: choiceValue(config.TUIColors)},
		{key: "tui.mouse", label: "Mouse", kind: choiceSetting, choices: []string{"on", "off"}, now: true,
			help: "off leaves the mouse to the terminal: select text without holding Shift",
			load: func(c *config.Config) string { return onOffPtr(c.TUI.Mouse) }, save: boolValue(true)},
		{section: "COPY AND SYNC · applies at once", key: "tui.if_exists", label: "If a file exists", kind: choiceSetting,
			choices: config.TUIIfExists, now: true,
			help: "what F5 and F6 start with when a file is already at the destination",
			load: str(func(c *config.Config) string { return c.TUI.IfExists }), save: choiceValue(config.TUIIfExists)},
		{key: "tui.compress", label: "Compress (zstd)", kind: choiceSetting, choices: []string{"off", "on"}, now: true,
			help: "F5, F6 and F4 start with zstd on: less to move, more CPU",
			load: func(c *config.Config) string { return onIf(c.TUI.Compress) }, save: boolValue(false)},
		{key: "tui.exclude", label: "Exclude", kind: textSetting, def: "nothing", now: true,
			help: "what the exclude field starts with, separated by spaces: .git *.tmp cache/",
			load: str(func(c *config.Config) string { return c.TUI.Exclude }), save: func(v string) (any, error) {
				if _, err := exclude.New(strings.Fields(v)); err != nil {
					return nil, err
				}
				return strings.Join(strings.Fields(v), " "), nil
			}},

		{section: "THIS NODE · when it starts again", key: "node_id", label: "Node name", kind: textSetting,
			def:  "the host's name",
			help: "how the other nodes name this one; under a new name the old one stays listed until forgotten",
			load: str(func(c *config.Config) string { return c.NodeID }), save: func(v string) (any, error) {
				if !config.ValidID(v) {
					return nil, errors.New("lower-case letters, digits, . _ - (up to 63)")
				}
				return v, nil
			}},
		{key: "nfs_root", label: "Share", kind: textSetting, def: "none: this host on its own",
			help: product.Expand("the NFS directory {name} uses when started without one; not with ssh hosts"),
			load: str(func(c *config.Config) string { return c.NFSRoot }), save: func(v string) (any, error) {
				if !filepath.IsAbs(v) {
					return nil, errors.New("an absolute path, such as /mnt/nfs/xfer")
				}
				if fi, err := os.Stat(v); err != nil || !fi.IsDir() {
					return nil, fmt.Errorf("%s is not a directory here", v)
				}
				return filepath.Clean(v), nil
			}},
		{key: "roles", label: "Roles", kind: textSetting, def: "none",
			help: "tags for @role destinations and for 'Writes from', separated by spaces",
			load: func(c *config.Config) string { return strings.Join(c.Roles, " ") }, save: func(v string) (any, error) {
				var out []string
				for _, r := range strings.FieldsFunc(v, sep) {
					if !config.ValidID(r) || r == api.GroupAll || r == api.GroupDest {
						return nil, fmt.Errorf("%q: lower-case letters, digits, . _ -; not all or dest", r)
					}
					out = append(out, r)
				}
				return out, nil
			}},
		{key: "accept_writes_from", label: "Writes from", kind: textSetting, def: "any node",
			help: "the nodes and @roles that may copy into this one, separated by spaces",
			load: func(c *config.Config) string { return strings.Join(c.AcceptWritesFrom, " ") }, save: func(v string) (any, error) {
				var out []string
				for _, a := range strings.FieldsFunc(v, sep) {
					if !config.ValidID(strings.TrimPrefix(a, "@")) {
						return nil, fmt.Errorf("%q is not a node name or @role", a)
					}
					out = append(out, a)
				}
				return out, nil
			}},
		{key: "local_root", label: "Root local", kind: textSetting, def: "/",
			help: "the folder only this host's TUI and CLI see, as root local; none: no such root",
			load: str(func(c *config.Config) string { return c.LocalRoot }), save: func(v string) (any, error) {
				if v == "none" {
					return v, nil
				}
				if !filepath.IsAbs(v) {
					return nil, errors.New("an absolute path, or none")
				}
				if fi, err := os.Stat(v); err != nil || !fi.IsDir() {
					return nil, fmt.Errorf("%s is not a directory here", v)
				}
				return filepath.Clean(v), nil
			}},
		{key: "transfer.allow_setid", label: "Keep setuid/setgid", kind: choiceSetting, choices: []string{"off", "on"},
			help: "keep these bits on files copied or saved here (off: removed, so no peer can plant one)",
			load: func(c *config.Config) string { return onIf(c.Transfer.AllowSetID) }, save: boolValue(false)},
		{key: "keep_done", label: "Keep finished jobs", kind: textSetting, def: "1h",
			help: "how long finished jobs and sync plans stay listed (30m, 1h, 1d)",
			load: durLoad(func(c *config.Config) time.Duration { return c.KeepDone }), save: durValue(time.Minute)},

		{section: "TRANSFERS · when the node starts again", key: "transfer.max_jobs", label: "Jobs at once", kind: textSetting, def: "2",
			help: "how many jobs this node sends at a time, and receives at a time; more wait, queued",
			load: intLoad(func(c *config.Config) int { return c.Transfer.MaxJobs }), save: intValue(1, 64)},
		{key: "transfer.chunk_size", label: "Chunk size", kind: textSetting, def: "128MiB (ssh: 32MiB)",
			help: "the largest piece of a transfer staged at a time; at least 1MiB",
			load: sizeLoad(func(c *config.Config) config.ByteSize { return c.Transfer.ChunkSize }), save: sizeValue(1 << 20)},
		{key: "transfer.job_quota", label: "Staging per job", kind: textSetting, def: "1GiB (ssh: 128MiB)",
			help: "the most one job stages before it waits for its targets; at least the chunk size",
			load: sizeLoad(func(c *config.Config) config.ByteSize { return c.Transfer.JobQuota }), save: sizeValue(1 << 20)},
		{key: "transfer.staging_quota", label: "Staging in all", kind: textSetting, def: "4GiB (ssh: RAM/4, ≤512MiB)",
			help: "the most all of this node's jobs stage together; at least the job's",
			load: sizeLoad(func(c *config.Config) config.ByteSize { return c.Transfer.StagingQuota }), save: sizeValue(1 << 20)},
		{key: "transfer.detach_after", label: "Give up on a target after", kind: textSetting, def: "10m",
			help: "a target this long without progress is dropped (catch it up later); a share full this long fails the copy",
			load: durLoad(func(c *config.Config) time.Duration { return c.Transfer.DetachAfter }), save: durValue(time.Minute)},
		{key: "sync.max_deletes", label: "Sync deletion limit", kind: textSetting, def: "1000 per target",
			help: "a mirror sync that deletes more on one target must be forced; -1: no limit",
			load: intLoad(func(c *config.Config) int { return c.Sync.MaxDeletes }), save: intValue(-1, 1<<30)},

		{section: "SSH · when fkbr starts again", key: "ssh_bin", label: product.Name + " on the hosts", kind: textSetting,
			def:  product.Name + " on their PATH",
			help: "where " + product.Name + " is on the ssh hosts; ~/ is their home: ~/bin/" + product.Name,
			load: str(func(c *config.Config) string { return c.SSHBin }), save: func(v string) (any, error) { return v, nil }},
		{key: "ssh", label: "Hosts", kind: infoSetting,
			help: "F7 in a node list links a host and keeps it here; F8 forgets it",
			load: func(c *config.Config) string {
				switch len(c.SSH) {
				case 0:
					return "none yet: F7 in a node list"
				case 1:
					return c.SSH[0]
				}
				return fmt.Sprintf("%d: %s", len(c.SSH), strings.Join(c.SSH, ", "))
			}},
	}
}

func sep(r rune) bool { return r == ',' || r == ' ' }

func onOffPtr(b *bool) string {
	if b == nil {
		return ""
	}
	return map[bool]string{true: "on", false: "off"}[*b]
}

func onIf(b bool) string {
	if b {
		return "on"
	}
	return ""
}

func choiceValue(choices []string) func(string) (any, error) {
	return func(v string) (any, error) {
		if !slices.Contains(choices, v) {
			return nil, fmt.Errorf("one of %s", strings.Join(choices, ", "))
		}
		return v, nil
	}
}

func boolValue(def bool) func(string) (any, error) {
	return func(v string) (any, error) { return v == "on", nil }
}

func intLoad(get func(*config.Config) int) func(*config.Config) string {
	return func(c *config.Config) string {
		if v := get(c); v != 0 {
			return strconv.Itoa(v)
		}
		return ""
	}
}

func intValue(lo, hi int) func(string) (any, error) {
	return func(v string) (any, error) {
		n, err := strconv.Atoi(v)
		if err != nil || n < lo || n > hi || n == 0 {
			return nil, fmt.Errorf("a number from %d", lo)
		}
		return n, nil
	}
}

func sizeLoad(get func(*config.Config) config.ByteSize) func(*config.Config) string {
	return func(c *config.Config) string {
		if v := get(c); v != 0 {
			return v.String()
		}
		return ""
	}
}

func sizeValue(least config.ByteSize) func(string) (any, error) {
	return func(v string) (any, error) {
		b, err := config.ParseByteSize(v)
		if err != nil || b < least {
			return nil, fmt.Errorf("a size of at least %s, such as 64MiB or 2GiB", least)
		}
		return b.String(), nil
	}
}

func durLoad(get func(*config.Config) time.Duration) func(*config.Config) string {
	return func(c *config.Config) string {
		if v := get(c); v != 0 {
			return shortDur(v)
		}
		return ""
	}
}

// durValue reads 30s, 10m, 1h30m and also 2d.
func durValue(least time.Duration) func(string) (any, error) {
	return func(v string) (any, error) {
		d, err := parseDur(v)
		if err != nil || d < least {
			return nil, fmt.Errorf("a time of at least %s, such as 10m, 1h or 2d", shortDur(least))
		}
		return shortDur(d), nil
	}
}

func parseDur(v string) (time.Duration, error) {
	if days, ok := strings.CutSuffix(v, "d"); ok {
		n, err := strconv.Atoi(days)
		return time.Duration(n) * 24 * time.Hour, err
	}
	return time.ParseDuration(v)
}

// shortDur writes 1h, 10m, 1h30m, not 1h0m0s.
func shortDur(d time.Duration) string {
	s := d.String()
	for _, z := range []string{"m0s", "h0m"} {
		if strings.HasSuffix(s, z) {
			s = strings.TrimSuffix(s, z[1:])
		}
	}
	return s
}

type settingsDialog struct {
	rows   []settingRow
	values []string // as shown and saved ("": the default)
	loaded []string // as read, to tell what changed
	focus  int
	top    int // first line shown
	height int // lines shown at the last view
	edit   *textinput.Model
	err    string
	asking bool // Esc with changes not saved
	path   string
	over   map[string]string
}

func newSettingsDialog(m *model) *settingsDialog {
	d := &settingsDialog{rows: settingRows(), path: m.prefs.configPath, over: m.prefs.overridden}
	c := &config.Config{}
	if d.path != "" {
		if got, err := config.Read(d.path); err == nil {
			c = got
		} else if !errors.Is(err, os.ErrNotExist) {
			d.err = err.Error()
		}
	} else {
		d.err = "no config file to keep settings in (no home directory?): pass -config FILE"
	}
	for _, r := range d.rows {
		v := r.load(c)
		if r.kind == choiceSetting && v == r.choices[0] {
			v = ""
		}
		d.values = append(d.values, v)
	}
	d.loaded = slices.Clone(d.values)
	return d
}

func (d *settingsDialog) changed() []int {
	var out []int
	for i := range d.rows {
		if d.values[i] != d.loaded[i] {
			out = append(out, i)
		}
	}
	return out
}

func (d *settingsDialog) update(m *model, msg tea.KeyMsg) (bool, tea.Cmd) {
	if d.edit != nil {
		switch msg.String() {
		case "enter":
			r := d.rows[d.focus]
			v := strings.TrimSpace(d.edit.Value())
			if v != "" {
				if _, err := r.save(v); err != nil {
					d.err = r.label + ": " + err.Error()
					return false, nil
				}
				saved, _ := r.save(v)
				v = textOf(saved)
			}
			d.values[d.focus], d.edit, d.err = v, nil, ""
		case "esc":
			d.edit, d.err = nil, ""
		default:
			var cmd tea.Cmd
			*d.edit, cmd = d.edit.Update(msg)
			return false, cmd
		}
		return false, nil
	}
	if d.asking {
		switch msg.String() {
		case "y", "Y":
			d.asking = false
			return d.saveAll(m)
		case "n", "N":
			return true, nil
		case "esc":
			d.asking = false
		}
		return false, nil
	}
	d.err = ""
	r := d.rows[d.focus]
	switch msg.String() {
	case "esc", "f11":
		if len(d.changed()) == 0 {
			return true, nil
		}
		d.asking = true
	case "ctrl+s":
		return d.saveAll(m)
	case "up", "k":
		d.focus = max(0, d.focus-1)
	case "down", "j", "tab":
		d.focus = min(len(d.rows)-1, d.focus+1)
	case "pgup":
		d.focus = max(0, d.focus-max(1, d.height/2))
	case "pgdown":
		d.focus = min(len(d.rows)-1, d.focus+max(1, d.height/2))
	case "home":
		d.focus = 0
	case "end":
		d.focus = len(d.rows) - 1
	case "d", "delete":
		if r.kind != infoSetting {
			d.values[d.focus] = ""
		}
	case "left", "h":
		d.cycle(-1)
	case "right", "l", " ":
		d.cycle(1)
	case "enter":
		switch r.kind {
		case choiceSetting:
			d.cycle(1)
		case textSetting:
			ti := textinput.New()
			ti.Prompt = ""
			ti.CharLimit = 4096
			ti.SetValue(d.values[d.focus])
			ti.Focus()
			d.edit = &ti
		}
	}
	return false, nil
}

// textOf is a saved value as the dialog shows it.
func textOf(v any) string {
	switch v := v.(type) {
	case []string:
		return strings.Join(v, " ")
	case nil:
		return ""
	}
	return fmt.Sprint(v)
}

func (d *settingsDialog) cycle(step int) {
	r := d.rows[d.focus]
	if r.kind != choiceSetting {
		return
	}
	cur := max(0, slices.Index(r.choices, d.value(d.focus)))
	next := r.choices[(cur+step+len(r.choices))%len(r.choices)]
	if next == r.choices[0] {
		next = ""
	}
	d.values[d.focus] = next
}

// value is what row i is set to, its default for a choice left unset.
func (d *settingsDialog) value(i int) string {
	if v := d.values[i]; v != "" || d.rows[i].kind != choiceSetting {
		return v
	}
	return d.rows[i].choices[0]
}

// saveAll writes what changed and applies the TUI's own settings.
func (d *settingsDialog) saveAll(m *model) (bool, tea.Cmd) {
	ch := d.changed()
	if len(ch) == 0 {
		return true, nil
	}
	var set []config.Setting
	for _, i := range ch {
		r := d.rows[i]
		var v any
		if d.values[i] != "" {
			var err error
			if v, err = r.save(d.values[i]); err != nil {
				d.focus, d.err = i, r.label+": "+err.Error()
				return false, nil
			}
		}
		set = append(set, config.Setting{Key: r.key, Value: v})
	}
	if err := config.SaveSettings(d.path, set); err != nil {
		d.err = "not saved: " + err.Error()
		return false, nil
	}
	later := 0
	for _, i := range ch {
		if !d.rows[i].now {
			later++
		}
	}
	msg := "settings saved in " + homeShort(d.path)
	if later > 0 {
		msg += fmt.Sprintf("; %d of them apply when %s starts again", later, product.Name)
	}
	m.setStatus(msg, false)
	return true, m.applyPrefs(d)
}

// applyPrefs takes the TUI's own settings from the dialog, as saved;
// one a flag sets for this run stays as the flag says.
func (m *model) applyPrefs(d *settingsDialog) tea.Cmd {
	get := func(key string) string {
		for i, r := range d.rows {
			if r.key == key {
				return d.value(i)
			}
		}
		return ""
	}
	p := &m.prefs
	var cmd tea.Cmd
	if c := get("tui.colors"); p.overridden["tui.colors"] == "" && c != p.colors {
		if setColors(c) == nil {
			p.colors, m.st = c, newStyles()
		}
	}
	if on := get("tui.mouse") == "on"; p.overridden["tui.mouse"] == "" && on != p.mouse {
		p.mouse = on
		cmd = tea.DisableMouse
		if on {
			cmd = tea.EnableMouseCellMotion
		}
	}
	p.ifExists = max(0, slices.Index(conflictPolicies, api.ConflictPolicy(get("tui.if_exists"))))
	p.compress = get("tui.compress") == "on"
	if ex := get("tui.exclude"); ex != p.exclude {
		if m.lastExclude == p.exclude {
			m.lastExclude = ex
		}
		p.exclude = ex
	}
	return cmd
}

func homeShort(p string) string {
	if h, err := os.UserHomeDir(); err == nil && h != "/" {
		if rest, ok := strings.CutPrefix(p, h+"/"); ok {
			return "~/" + rest
		}
	}
	return p
}

// labelWidth is the label column.
const labelWidth = 27

// lines lays out the rows: section titles, a gap before each, and the
// line of each row (rowAt maps a line back to its row, or -1).
func (d *settingsDialog) lines(st *styles, w int) (out []string, rowAt []int) {
	for i, r := range d.rows {
		if r.section != "" {
			if i > 0 {
				out, rowAt = append(out, ""), append(rowAt, -1)
			}
			out, rowAt = append(out, st.dialogTitle.Render(r.section)), append(rowAt, -1)
		}
		mark := "  "
		if d.values[i] != d.loaded[i] {
			mark = "* "
		}
		val := d.shown(i)
		if d.edit != nil && i == d.focus {
			d.edit.Width = max(4, w-labelWidth-4)
			st.styleInputs(0, d.edit)
			line := st.inputLabel.Render(mark+fit(r.label, labelWidth)) + d.edit.View()
			out, rowAt = append(out, line), append(rowAt, i)
			continue
		}
		line := mark + fit(r.label, labelWidth) + clip(safetext.Clean(val), w-labelWidth-2)
		if i == d.focus {
			line = st.highlight(st.inputCursor, fit(line, w))
		}
		out, rowAt = append(out, line), append(rowAt, i)
	}
	return out, rowAt
}

// shown is row i's value as the dialog shows it: a default in words,
// and a flag that wins this run.
func (d *settingsDialog) shown(i int) string {
	r := d.rows[i]
	v := d.value(i)
	switch {
	case r.kind == choiceSetting && d.values[i] == "":
		v += "  (default)"
	case r.kind == textSetting && v == "":
		v = r.def + "  (default)"
	}
	if f := d.over[r.key]; f != "" {
		v += "  · " + f + " wins this run"
	}
	return v
}

func (d *settingsDialog) view(st *styles, width, height int) string {
	w := min(width-6, 96)
	lines, rowAt := d.lines(st, w)
	where := homeShort(d.path)
	if _, err := os.Stat(d.path); err != nil && d.path != "" {
		where += " (made when you save)"
	}
	title := st.dialogTitle.Render(fit("Settings · kept in "+where, w))
	r := d.rows[d.focus]
	when := "applies when the node starts again"
	if r.now {
		when = "applies at once"
	}
	if r.kind == infoSetting {
		when = "shown only"
	}
	help := flowText(r.help+" ("+when+")", w)
	foot := []string{}
	switch {
	case d.asking:
		foot = append(foot, "Save the changes? y = save · n = throw them away · Esc = back")
	case d.err != "":
		foot = append(foot, st.dialogErr.Render(fit("! "+d.err, w)))
	default:
		foot = append(foot, "")
	}
	hint := "↑↓ move · Enter change · d default · ^S save · Esc close"
	if d.edit != nil {
		hint = "Enter keep · Esc cancel · empty: the default"
	}
	foot = append(foot, hint)
	// What fits between the title and the help: scroll to the focus.
	fixed := 2 + len(strings.Split(help, "\n")) + 1 + len(foot)
	d.height = max(3, height-6-fixed)
	at := slices.Index(rowAt, d.focus)
	if at < d.top+1 { // keep the section title above a first row in view
		d.top = max(0, at-2)
	}
	if at >= d.top+d.height {
		d.top = at - d.height + 1
	}
	d.top = max(0, min(d.top, len(lines)-d.height))
	end := min(len(lines), d.top+d.height)
	shown := lines[d.top:end]
	bar := st.bar(len(shown), len(lines), d.top)
	for i, l := range shown {
		if gap := w - 1 - lipgloss.Width(l); gap > 0 {
			l += st.inputLabel.Render(strings.Repeat(" ", gap))
		} else if lipgloss.Width(l) > w-1 {
			l = ansi.Truncate(l, w-1, "")
		}
		shown[i] = l + bar[i]
	}
	return title + "\n\n" + strings.Join(shown, "\n") + "\n\n" + help + "\n" + strings.Join(foot, "\n")
}

// focusLine puts the cursor on the row shown on line (a click); a second
// click on a choice changes it.
func (d *settingsDialog) focusLine(line string) bool {
	plain := strings.TrimSpace(ansi.Strip(line))
	plain = strings.TrimPrefix(plain, "* ")
	for i, r := range d.rows {
		if strings.HasPrefix(plain, r.label) {
			if i == d.focus && d.edit == nil {
				d.cycle(1)
			}
			if d.edit == nil {
				d.focus = i
			}
			return true
		}
	}
	return false
}
