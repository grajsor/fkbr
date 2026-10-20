package engine

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"fkbr/internal/api"
)

// One worker per node ID. The worker lock only covers one state dir, so a
// second user on the same host (a TUI under sudo, say) or a -state flag
// would otherwise start a second worker for the same ID: both consume the
// one inbox, and each request lands in whichever process claims it, in
// that process's directory. So every worker run has an instance token,
// published in info.json (with its PID and the host's boot ID) and written
// into the heartbeat file:
//
//   - At start, a worker refuses an ID whose online entry belongs to another
//     host or to a process still running on this one. A crashed worker (dead
//     PID, or another boot) can be replaced at once.
//   - On every later heartbeat it reads the token there first (at start it
//     is the replaced worker's). A foreign token means another worker uses
//     the ID (both passed the start check at the same moment): the one that
//     started later stops, and the other re-registers its own info.

// liveInstances holds the tokens of workers running in this process; for
// them, "is the PID alive" says nothing.
var liveInstances sync.Map

// newToken returns a worker instance token: "w1 <start unix ns> <random>".
func newToken(started time.Time) string {
	var b [8]byte
	rand.Read(b[:])
	return fmt.Sprintf("w1 %d %s", started.UnixNano(), hex.EncodeToString(b[:]))
}

// tokenStart returns when the worker with token t started.
func tokenStart(t string) (int64, bool) {
	f := strings.Fields(t)
	if len(f) != 3 || f[0] != "w1" {
		return 0, false
	}
	ns, err := strconv.ParseInt(f[1], 10, 64)
	return ns, err == nil
}

// startedAfter reports whether the worker with token a started after the
// one with token b (ties broken by the token, so both sides agree).
func startedAfter(a, b string) bool {
	na, _ := tokenStart(a)
	nb, _ := tokenStart(b)
	if na != nb {
		return na > nb
	}
	return a > b
}

func bootID() string {
	b, _ := os.ReadFile("/proc/sys/kernel/random/boot_id")
	return strings.TrimSpace(string(b))
}

func processAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	err := syscall.Kill(pid, 0)
	return err == nil || err == syscall.EPERM
}

// runningHere reports whether registry entry n belongs to another worker
// that is still running on this host.
func (e *Engine) runningHere(n api.Node) bool {
	if n.Instance == "" || n.Instance == e.info.Instance || n.BootID == "" || n.BootID != e.info.BootID {
		return false
	}
	if n.PID == os.Getpid() {
		_, ok := liveInstances.Load(n.Instance)
		return ok
	}
	return processAlive(n.PID)
}

// heartbeatConflict handles a foreign token found in the heartbeat file.
// It returns an error if this worker must stop.
func (e *Engine) heartbeatConflict(other string) error {
	if startedAfter(e.info.Instance, other) {
		return fmt.Errorf("%w: another worker for %q is running (it started first); stopping this one", ErrNodeIDTaken, e.Self())
	}
	e.log.Warn("another worker uses this node ID; it should stop. Re-registering this one", "node", e.Self())
	return nil
}
