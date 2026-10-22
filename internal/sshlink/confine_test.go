package sshlink

import (
	"context"
	"fmt"
	"io/fs"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"fkbr/internal/api"
	"fkbr/internal/sshlink/frame"
	"fkbr/internal/store"
	"fkbr/internal/transport"
)

// A linked node is another host: every name it sends becomes a path in
// the home node's private share only after it has been checked. Each call
// a linked node can make gets a hostile name in each of its fields in
// turn; nothing outside the share may change.
func TestRemoteCallsStayInTheShare(t *testing.T) {
	base := t.TempDir()
	dir := filepath.Join(base, "swarm")
	victim := filepath.Join(base, "victim")
	for _, d := range []string{dir, victim} {
		if err := os.Mkdir(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(victim, "keep"), []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	h, mb, cli := linked(t, dir)
	ctx := context.Background()
	// A real job, so calls on it get past "no such job" to their writes.
	if err := mb.CreateJob(ctx, &api.Job{ID: "j1", Request: api.TransferRequest{SrcNode: "web01", DstNodes: []string{"home"}}}); err != nil {
		t.Fatal(err)
	}
	_ = h

	msg := func(id, from, to string) transport.Message { return transport.Message{ID: id, From: from, To: to} }
	job := func(id, src, dst string) api.Job {
		return api.Job{ID: id, Request: api.TransferRequest{SrcNode: src, DstNodes: []string{dst}}}
	}
	calls := func(v string) map[string][]any {
		return map[string][]any{
			"register":     {api.NodeInfo{ID: v}},
			"heartbeat":    {map[string]string{"token": v}},
			"stale":        {map[string]string{"node": v}},
			"unregister":   {map[string]string{"node": v}},
			"send":         {msg(v, "web01", "home"), msg("r1", "web01", v), msg("r1", v, "home")},
			"await":        {map[string]string{"req_id": v}},
			"reply":        {replyArgs{Req: msg(v, "home", "web01")}, replyArgs{Req: msg("r1", v, "web01")}},
			"finish":       {finishArgs{Msg: msg(v, "home", "web01"), Failed: true}, finishArgs{Msg: msg(v, "home", "web01")}},
			"job.create":   {job(v, "web01", "home"), job("j1", v, "home"), job("j1", "web01", v)},
			"job.get":      {map[string]string{"id": v}},
			"job.status":   {statusArgs{ID: v, St: api.JobStatus{Node: "web01", Role: api.RoleSource}}, statusArgs{ID: "j1", St: api.JobStatus{Node: "web01", Role: api.Role(v)}}},
			"job.cancel":   {map[string]string{"id": v}},
			"job.complete": {completeArgs{ID: v, Chunks: 1}},
			"job.detach":   {detachArgs{ID: v, D: api.Detached{Node: "home"}}, detachArgs{ID: "j1", D: api.Detached{Node: v}}},
			"ack.set":      {map[string]any{"src": v, "id": "j1", "node": "web01", "n": 1}, map[string]any{"src": "home", "id": v, "node": "web01", "n": 1}},
			"ack.get":      {map[string]any{"src": v, "id": "j1", "node": "web01"}, map[string]any{"src": "home", "id": v, "node": "web01"}, map[string]any{"src": "home", "id": "j1", "node": v}},
			"data.remove":  {map[string]string{"id": v}},
			"blob.create":  {blobArgs{Name: v}, blobArgs{Name: "plans/" + v + "/x"}},
			"blob.open":    {blobArgs{Name: v}, blobArgs{Name: "plans/" + v + "/x"}},
			"chunk.meta":   {chunkRef{Src: v, ID: "j1"}, chunkRef{Src: "home", ID: v}},
			"chunk.open":   {chunkRef{Src: v, ID: "j1"}, chunkRef{Src: "home", ID: v}},
		}
	}
	deep := "../../../../../../../../../../.." + victim
	hostile := []string{"..", "../victim", "../../victim", "../victim/keep", "x/../../victim", "/" + victim, victim + "/keep", deep, deep + "/keep", deep + "/new"}
	for _, v := range hostile {
		for method, list := range calls(v) {
			for _, a := range list {
				cctx, cancel := context.WithTimeout(ctx, 300*time.Millisecond)
				do(cctx, cli, method, a, nil) // refused or harmless; what counts is the disk
				cancel()
			}
		}
	}

	if b, err := os.ReadFile(filepath.Join(victim, "keep")); err != nil || string(b) != "keep" {
		t.Errorf("victim/keep: %q, %v", b, err)
	}
	filepath.WalkDir(base, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			t.Errorf("walk: %v", err)
			return nil
		}
		switch rel, _ := filepath.Rel(base, path); {
		case rel == "." || rel == "swarm" || rel == "victim" || rel == filepath.Join("victim", "keep"):
		case filepath.Dir(rel) == "." || filepath.Dir(rel) == "victim":
			t.Errorf("%s: written outside the share", rel)
		default:
			return fs.SkipDir // inside the share
		}
		return nil
	})
}

// linked serves a home node on dir to a linked node web01 over an
// in-memory link; it returns web01's store and its end of the link.
func linked(t *testing.T, dir string) (*Home, *store.Dir, *frame.Session) {
	t.Helper()
	h := NewHome(dir, "home", store.Options{}, nil)
	if _, err := h.Open(); err != nil {
		t.Fatal(err)
	}
	mb, err := store.Open(dir, "web01", h.opts)
	if err != nil {
		t.Fatal(err)
	}
	if err := mb.Register(context.Background(), api.NodeInfo{ID: "web01"}); err != nil {
		t.Fatal(err)
	}
	p := &peer{node: "web01", host: "web01", mb: mb}
	c1, c2 := net.Pipe() // unbuffered: both ends start together
	var srv *frame.Session
	srvErr := make(chan error, 1)
	go func() {
		var err error
		srv, err = frame.Server(c2, frame.Options{})
		srvErr <- err
	}()
	cli, err := frame.Client(c1, frame.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if err := <-srvErr; err != nil {
		t.Fatal(err)
	}
	ctx, stop := context.WithCancel(context.Background())
	t.Cleanup(func() { stop(); cli.Close(); srv.Close() })
	go serve(ctx, srv, h.handlers(p), nil)
	return h, mb, cli
}

// A blob upload that fails halfway is not published, cut short.
func TestAbortedBlobNotCommitted(t *testing.T) {
	dir := t.TempDir()
	_, mb, cli := linked(t, dir)
	r := &Remote{sess: cli}
	w, err := r.CreateBlob(context.Background(), "plans/p1/copy-x")
	if err != nil {
		t.Fatal(err)
	}
	w.Write([]byte("half of it"))
	w.Abort()
	time.Sleep(200 * time.Millisecond) // the home node handles the end
	if rc, err := mb.OpenBlob(context.Background(), "plans/p1/copy-x"); err == nil {
		rc.Close()
		t.Error("an aborted blob was published")
	}
}

// Two hosts that both say they are web01, at the same moment: one links,
// the other is refused.
func TestSameNameLinksOnce(t *testing.T) {
	for round := range 20 {
		dir := t.TempDir()
		h := NewHome(dir, "home", store.Options{}, nil)
		if _, err := h.Open(); err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithCancel(context.Background())
		type result struct {
			r   *Remote
			err error
		}
		results := make(chan result, 2)
		for i := range 2 {
			a, b := net.Pipe()
			go h.ServeConn(ctx, b, fmt.Sprint("host", i), frame.Options{})
			go func() {
				r, err := Dial(ctx, a, "web01", "test", filepath.Join(t.TempDir(), "chunks"), frame.Options{}, nil)
				results <- result{r, err}
			}()
		}
		linked := 0
		for range 2 {
			if res := <-results; res.err == nil {
				linked++
				defer res.r.Close()
			}
		}
		cancel()
		if linked != 1 {
			t.Fatalf("round %d: %d links named web01, want 1", round, linked)
		}
	}
}
