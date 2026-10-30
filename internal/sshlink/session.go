package sshlink

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"slices"
	"strings"
	"sync"
	"syscall"
	"time"

	"fkbr/internal/config"
	"fkbr/internal/sshlink/frame"
)

// Host is one remote host of an ssh swarm.
type Host struct {
	Dest string // as ssh takes it: host, user@host, an alias from ~/.ssh/config
	Path string // the directory to share there; "" is the login directory
	// Node is the node's name ("" is the host's short name). Two
	// directories on one host, or two hosts of the same short name, need
	// one each.
	Node string
}

func (h Host) String() string {
	s := h.Dest
	if h.Node != "" {
		s = h.Node + "=" + s
	}
	if h.Path == "" {
		return s
	}
	return s + ":" + h.Path
}

// ParseHosts parses the hosts of an ssh: share, e.g.
// "web01:/srv/www,admin@db01,bak=web01:/var/backups".
func ParseHosts(s string) ([]Host, error) {
	var hs []Host
	for _, item := range strings.Split(s, ",") {
		item = strings.TrimSpace(item)
		if item == "" {
			continue
		}
		rest, node := item, ""
		if i := strings.IndexByte(item, '='); i >= 0 && !strings.Contains(item[:i], ":") {
			node, rest = item[:i], item[i+1:]
			if !config.ValidID(node) {
				return nil, fmt.Errorf("ssh host %q: %q is not a node name (a-z, 0-9, _ . -)", item, node)
			}
		}
		dest, path, _ := strings.Cut(rest, ":")
		if dest == "" || strings.HasPrefix(dest, "-") || strings.ContainsAny(dest, " \t'\"=") {
			return nil, fmt.Errorf("ssh host %q: not a host name", item)
		}
		if path != "" && !strings.HasPrefix(path, "/") {
			return nil, fmt.Errorf("ssh host %q: the path must be absolute", item)
		}
		hs = append(hs, Host{Dest: dest, Path: path, Node: node})
	}
	if len(hs) == 0 {
		return nil, errors.New("ssh: names no hosts, e.g. ssh:web01:/srv/www,db01")
	}
	return hs, nil
}

// SessionOptions say how sessions are started.
type SessionOptions struct {
	// SSH is the ssh command (default "ssh"); tests use a stand-in.
	SSH []string
	// Bin is fkbr on the remote hosts (default "fkbr").
	Bin string
	// Args are passed to `fkbr link` there (e.g. -v).
	Args  []string
	Frame frame.Options
	// StopWait is how long remote nodes get to leave when the home node
	// stops (default 15 s).
	StopWait time.Duration
}

func (o *SessionOptions) defaults() {
	if len(o.SSH) == 0 {
		o.SSH = []string{"ssh"}
	}
	if o.Bin == "" {
		o.Bin = "fkbr"
	}
	if o.StopWait <= 0 {
		o.StopWait = 15 * time.Second
	}
}

// sshArgs is the ssh command line for host. BatchMode: no password or
// host key prompts over the TUI; Compression off: chunks are zstd already.
func sshArgs(o *SessionOptions, h Host, remote string) []string {
	args := append([]string{}, o.SSH[1:]...)
	args = append(args, "-T", "-o", "BatchMode=yes", "-o", "Compression=no", "--", h.Dest, remote)
	return args
}

// remoteCommand is what the remote shell runs.
func remoteCommand(o *SessionOptions, h Host, words ...string) string {
	bin := shellQuote(o.Bin)
	if rest, ok := strings.CutPrefix(o.Bin, "~/"); ok { // the remote home, also from a config file
		bin = `"$HOME"/` + shellQuote(rest)
	}
	cmd := "exec " + bin
	for _, w := range words {
		cmd += " " + shellQuote(w)
	}
	if h.Path != "" {
		cmd = "cd " + shellQuote(h.Path) + " && " + cmd
	}
	return cmd
}

func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// sshGrace is how long ssh's output is read after it exited or was told to.
const sshGrace = 2 * time.Second

// Backoff between attempts to start a host's session.
var (
	backoffMin = time.Second
	backoffMax = time.Minute
)

// Run keeps a session to every host until ctx ends; then it asks the
// remote nodes to stop, so each leaves while its link is up, and ends the
// sessions.
func (h *Home) Run(ctx context.Context, hosts []Host, o SessionOptions) {
	o.defaults()
	h.mu.Lock()
	h.runCtx, h.runOpts, h.keepers = ctx, &o, map[string]context.CancelFunc{}
	h.mu.Unlock()
	close(h.started)
	for _, host := range hosts {
		if err := h.Add(ctx, host); err != nil {
			h.log.Warn("ssh host not linked", "host", host.String(), "err", err)
		}
	}
	<-ctx.Done()
	h.Stop(o.StopWait)
	h.keeping.Wait()
}

// ErrNotRunning: hosts are added while the home node runs (Run).
// ErrLinked: Add of a host linked already.
var (
	ErrNotRunning = errors.New("the home node is not running")
	ErrLinked     = errors.New("linked already")
)

// Add links host while Run runs. Run starts once the node is ready; an
// Add that comes first (F7 at once) waits for it, a while.
func (h *Home) Add(ctx context.Context, host Host) error {
	select {
	case <-h.started:
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(time.Minute):
		return ErrNotRunning
	}
	key := host.String()
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.runCtx == nil || h.runCtx.Err() != nil {
		return ErrNotRunning
	}
	if _, ok := h.keepers[key]; ok {
		return fmt.Errorf("%s: %w", key, ErrLinked)
	}
	ctx, cancel := context.WithCancel(h.runCtx)
	h.keepers[key] = cancel
	hs := h.hosts[key]
	if hs == nil {
		hs = &hostState{}
		h.hosts[key] = hs
	}
	hs.state, hs.err = "connecting", ""
	h.keeping.Add(1)
	go func() {
		defer h.keeping.Done()
		h.keep(ctx, host, h.runOpts)
	}()
	return nil
}

// Remove stops linking host: its node is asked to stop (it leaves the
// swarm), then its session ends.
func (h *Home) Remove(host string) error {
	h.mu.Lock()
	cancel := h.keepers[host]
	delete(h.keepers, host)
	delete(h.hosts, host)
	var p *peer
	for _, q := range h.peers {
		if q.host == host {
			p = q
		}
	}
	wait := time.Second
	if h.runOpts != nil {
		wait = h.runOpts.StopWait
	}
	h.mu.Unlock()
	if cancel == nil {
		return fmt.Errorf("%s is not linked", host)
	}
	if p != nil {
		p.stopped.Store(true)
		ctx, done := context.WithTimeout(context.Background(), wait)
		do(ctx, p.sess, "stop", nil, nil)
		select {
		case <-p.sess.Done():
		case <-ctx.Done():
		}
		done()
	}
	cancel()
	return nil
}

func (h *Home) keep(ctx context.Context, host Host, o *SessionOptions) {
	wait := backoffMin
	for ctx.Err() == nil {
		start := time.Now()
		err := h.session(ctx, host, o)
		if ctx.Err() != nil {
			return
		}
		if time.Since(start) > backoffMax {
			wait = backoffMin // it ran a while: try again soon
		}
		msg := "ended"
		if err != nil {
			msg = err.Error()
		}
		if ctx.Err() != nil {
			return
		}
		h.setState(host.String(), "", "retrying in "+wait.String(), msg)
		h.log.Warn("ssh session ended; starting it again", "host", host.String(), "err", msg, "in", wait)
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
		wait = min(2*wait, backoffMax)
	}
}

// session runs one ssh session to host until it ends.
func (h *Home) session(ctx context.Context, host Host, o *SessionOptions) error {
	name := host.String()
	h.setState(name, "", "connecting", "")
	words := append(slices.Clone(o.Args), "link")
	if host.Node != "" {
		words = append([]string{"-node", host.Node}, words...)
	}
	cmd := exec.Command(o.SSH[0], sshArgs(o, host, remoteCommand(o, host, words...))...)
	// Its own process group: Ctrl+C in the TUI's terminal must not kill ssh
	// before the remote node could leave.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	// Our own pipes, not Std*Pipe: once ssh has exited, what it wrote is
	// read for sshGrace, then the read ends are closed. Something ssh left
	// running (a ControlPersist master) may hold its stdout and stderr open
	// for good; ssh's exit, not their EOF, ends the session.
	stdout, outW, err := os.Pipe()
	if err != nil {
		return err
	}
	inR, stdin, err := os.Pipe()
	if err != nil {
		stdout.Close()
		outW.Close()
		return err
	}
	stderr, errW, err := os.Pipe()
	if err != nil {
		stdout.Close()
		outW.Close()
		inR.Close()
		stdin.Close()
		return err
	}
	cmd.Stdin, cmd.Stdout, cmd.Stderr = inR, outW, errW
	err = cmd.Start()
	inR.Close() // ssh has its own copies now
	outW.Close()
	errW.Close()
	if err != nil {
		stdout.Close()
		stdin.Close()
		stderr.Close()
		return fmt.Errorf("starting %s: %w", o.SSH[0], err)
	}
	exited := make(chan error, 1)
	go func() {
		err := cmd.Wait()
		time.AfterFunc(sshGrace, func() {
			stdout.Close()
			stderr.Close()
		})
		exited <- err
	}()
	last := &lastLine{}
	logged := make(chan struct{})
	go func() {
		defer close(logged)
		eachLine(stderr, func(line string) {
			// The remote node's own log says how it ran, not why the
			// session ended: that is ssh's last word, or the link's.
			if !strings.HasPrefix(line, "time=") {
				last.set(line)
			}
			h.log.Info("ssh "+name, "says", line)
		})
	}()
	// Once the home node stops, the remote node gets StopWait to leave
	// (see Run); then ssh is ended, which ends the session in any state.
	served := make(chan struct{})
	go func() {
		select {
		case <-served:
		case <-ctx.Done():
			select {
			case <-served:
			case <-time.After(o.StopWait + time.Second):
				cmd.Process.Kill()
			}
		}
	}()
	conn := &pipeConn{r: stdout, w: stdin}
	err = h.ServeConn(context.WithoutCancel(ctx), conn, name, o.Frame)
	close(served)
	// The session has ended (or never started): let ssh go, and read its
	// last words on stderr.
	stdin.Close()
	select {
	case <-exited:
	case <-time.After(5 * time.Second):
		cmd.Process.Kill()
		<-exited
	}
	<-logged // ends at stderr's EOF, or sshGrace after ssh exited
	if l := last.get(); l != "" && !errors.Is(err, frame.ErrClosed) {
		return fmt.Errorf("%v (ssh: %s)", err, l)
	} else if l != "" {
		return errors.New(l)
	}
	return err
}

// stderrLine is the most of one stderr line that is logged.
const stderrLine = 64 << 10

// eachLine calls each for every line of r until it ends, a line longer
// than stderrLine cut short. It reads r to the end whatever comes: a
// reader that stopped would leave ssh's stderr full and the remote node
// blocked on its next log line.
func eachLine(r io.Reader, each func(string)) {
	br := bufio.NewReaderSize(r, stderrLine)
	skipping := false // the rest of a line already cut short
	for {
		b, err := br.ReadSlice('\n')
		if !skipping && len(b) > 0 {
			line := strings.TrimRight(string(b), "\r\n")
			if errors.Is(err, bufio.ErrBufferFull) {
				line += " …"
			}
			each(line)
		}
		switch {
		case errors.Is(err, bufio.ErrBufferFull):
			skipping = true
		case err != nil:
			return
		default:
			skipping = false
		}
	}
}

type lastLine struct {
	mu   sync.Mutex
	line string
}

func (l *lastLine) set(s string) {
	if s = strings.TrimSpace(s); s != "" {
		l.mu.Lock()
		l.line = s
		l.mu.Unlock()
	}
}

func (l *lastLine) get() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.line
}

// pipeConn is a session's stdout and stdin as one connection.
type pipeConn struct {
	r io.ReadCloser
	w io.WriteCloser
}

func (c *pipeConn) Read(p []byte) (int, error)  { return c.r.Read(p) }
func (c *pipeConn) Write(p []byte) (int, error) { return c.w.Write(p) }
func (c *pipeConn) Close() error {
	c.w.Close()
	return c.r.Close()
}

// Probe runs `fkbr version` on host over ssh, for `check`.
func Probe(ctx context.Context, host Host, o SessionOptions) (string, error) {
	o.defaults()
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, o.SSH[0], sshArgs(&o, host, remoteCommand(&o, host, "version"))...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.WaitDelay = sshGrace // output held open after ssh exited (see session)
	out, err := cmd.CombinedOutput()
	if errors.Is(err, exec.ErrWaitDelay) {
		err = nil // ssh succeeded; only its output stayed open
	}
	text := strings.TrimSpace(string(out))
	if err != nil {
		if i := strings.LastIndexByte(text, '\n'); i >= 0 {
			text = text[i+1:]
		}
		if text == "" {
			text = err.Error()
		}
		return "", errors.New(text)
	}
	if i := strings.LastIndexByte(text, '\n'); i >= 0 {
		text = text[i+1:] // after a login banner
	}
	return text, nil
}
