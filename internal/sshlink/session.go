package sshlink

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"time"

	"fkbr/internal/sshlink/frame"
)

// Host is one remote host of an ssh swarm.
type Host struct {
	Dest string // as ssh takes it: host, user@host, an alias from ~/.ssh/config
	Path string // the directory to share there; "" is the login directory
}

func (h Host) String() string {
	if h.Path == "" {
		return h.Dest
	}
	return h.Dest + ":" + h.Path
}

// ParseHosts parses the hosts of an ssh: share, e.g.
// "web01:/srv/www,admin@db01,backup:/data".
func ParseHosts(s string) ([]Host, error) {
	var hs []Host
	for _, item := range strings.Split(s, ",") {
		item = strings.TrimSpace(item)
		if item == "" {
			continue
		}
		dest, path, _ := strings.Cut(item, ":")
		if dest == "" || strings.HasPrefix(dest, "-") || strings.ContainsAny(dest, " \t'\"") {
			return nil, fmt.Errorf("ssh host %q: not a host name", item)
		}
		if path != "" && !strings.HasPrefix(path, "/") {
			return nil, fmt.Errorf("ssh host %q: the path must be absolute", item)
		}
		hs = append(hs, Host{dest, path})
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
	cmd := "exec " + shellQuote(o.Bin)
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
	var wg sync.WaitGroup
	for _, host := range hosts {
		h.setState(host.String(), "", "connecting", "")
		wg.Add(1)
		go func() {
			defer wg.Done()
			h.keep(ctx, host, &o)
		}()
	}
	<-ctx.Done()
	h.Stop(o.StopWait)
	wg.Wait()
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
	cmd := exec.Command(o.SSH[0], sshArgs(o, host, remoteCommand(o, host, append(o.Args, "link")...))...)
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
		sc := bufio.NewScanner(stderr)
		sc.Buffer(make([]byte, 64<<10), 1<<20)
		for sc.Scan() {
			line := sc.Text()
			last.set(line)
			h.log.Info("ssh "+name, "says", line)
		}
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
