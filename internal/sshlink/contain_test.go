package sshlink

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"io/fs"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"runtime/debug"
	"strings"
	"testing"
	"time"

	"fkbr/internal/api"
	"fkbr/internal/sshlink/frame"
	"fkbr/internal/store"
)

// A handler that panics (a bug on a name or frame off the wire) fails its
// own call, with the stack in the log, and nothing else: the link and the
// calls after it carry on (review 2026-11-05: a panic in a goroutine of
// serve ends the whole process).
func TestHandlerPanicFailsOnlyItsCall(t *testing.T) {
	for _, c := range []struct {
		name    string
		answerd bool // the handler had started its answer
	}{{"before the answer", false}, {"after the answer", true}} {
		t.Run(c.name, func(t *testing.T) {
			var logged bytes.Buffer
			log := slog.New(slog.NewTextHandler(&logged, nil))
			ctx := context.Background()
			cli, srv := sessionPair(t)
			go serve(ctx, srv, map[string]handler{
				"boom": func(_ context.Context, _ json.RawMessage, _ io.Reader, w *answerWriter) (any, error) {
					if c.answerd {
						w.Header(nil)
						w.Write([]byte("half"))
					}
					var m map[string]int
					m["x"] = 1 // nil map: panics
					return nil, nil
				},
				"ok": func(context.Context, json.RawMessage, io.Reader, *answerWriter) (any, error) { return "fine", nil },
			}, log)

			call, err := open(ctx, cli, "boom", nil, nil)
			if err == nil {
				defer call.Close()
				if err = call.answer(ctx, nil); err == nil {
					_, err = io.ReadAll(call)
				}
			}
			if err == nil {
				t.Fatal("a call whose handler panicked read as complete")
			}
			var got string
			if err := do(ctx, cli, "ok", nil, &got); err != nil || got != "fine" {
				t.Fatalf("the call after the panic: %q, %v", got, err)
			}
			for _, want := range []string{"panic", "boom", "nil map", "contain_test.go"} {
				if !strings.Contains(logged.String(), want) {
					t.Errorf("the log lacks %q:\n%s", want, logged.String())
				}
			}
		})
	}
}

func openFDs(t *testing.T) int {
	t.Helper()
	ents, err := os.ReadDir("/proc/self/fd")
	if err != nil {
		t.Skip("no /proc")
	}
	return len(ents)
}

// Links that come and go, and hellos refused for a name already linked,
// leave no descriptors behind: the home node closes the store it opened
// for each of them, rather than leaving it to the garbage collector
// (review 2026-11-05).
func TestLinkChurnLeavesNoDescriptors(t *testing.T) {
	defer debug.SetGCPercent(debug.SetGCPercent(-1)) // no finalizer may do it for us
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	h := NewHome(t.TempDir(), "home", store.Options{}, nil)
	if _, err := h.Open(); err != nil {
		t.Fatal(err)
	}
	dial := func(node string) (*Remote, chan struct{}, error) {
		a, b := net.Pipe()
		served := make(chan struct{})
		go func() { h.ServeConn(ctx, b, node+"-host", frame.Options{}); close(served) }()
		r, err := Dial(ctx, a, node, "test", filepath.Join(t.TempDir(), "chunks"), frame.Options{}, nil)
		return r, served, err
	}
	// One round first: the share's folders, the temp dirs and the like.
	r, served, err := dial("web01")
	if err != nil {
		t.Fatal(err)
	}
	r.Close()
	<-served
	before := openFDs(t)
	const rounds = 15
	for range rounds {
		r, served, err := dial("web01")
		if err != nil {
			t.Fatal(err)
		}
		r2, served2, err := dial("web01") // refused: linked already
		if err == nil {
			r2.Close()
			t.Fatal("a second web01 linked")
		}
		<-served2
		r.Close()
		<-served
	}
	if grown := openFDs(t) - before; grown > 4 {
		t.Errorf("%d rounds of links left %d more descriptors open", rounds, grown)
	}
}

// A linked host that asks the home node to prune by a negative age is
// refused, and the finished jobs stay (review 2026-11-05).
func TestRemotePruneNegativeAge(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	_, mb, cli := linked(t, dir)
	j := &api.Job{ID: "000000000000000000000012", Request: api.TransferRequest{SrcNode: "web01", DstNodes: []string{"web01"}}}
	if err := mb.CreateJob(ctx, j); err != nil {
		t.Fatal(err)
	}
	mb.PutStatus(ctx, j.ID, &api.JobStatus{Node: "web01", Role: api.RoleSource, State: api.JobDone})
	mb.PutStatus(ctx, j.ID, &api.JobStatus{Node: "web01", Role: api.RoleTarget, State: api.JobDone})
	// Old enough to go, were the age taken at its word.
	old := time.Now().Add(-2 * time.Minute)
	filepath.WalkDir(filepath.Join(dir, "jobs", j.ID), func(p string, _ fs.DirEntry, _ error) error {
		return os.Chtimes(p, old, old)
	})
	for _, call := range []string{"prune", "job.prune", "blob.prune"} {
		if err := do(ctx, cli, call, map[string]time.Duration{"max_age": -time.Hour}, nil); err == nil {
			t.Errorf("%s accepted a negative age", call)
		}
	}
	if _, err := mb.GetJob(ctx, j.ID); err != nil {
		t.Fatalf("a finished job went with a negative age: %v", err)
	}
}

// A blob writer whose context ends before anything is written is freed:
// its call to the home node waited in io.Copy on the pipe, which resetting
// the stream does not wake (review 2026-11-05).
func TestBlobWriterFreedWhenContextEnds(t *testing.T) {
	_, _, cli := linked(t, t.TempDir())
	r := &Remote{sess: cli}
	ctx, cancel := context.WithCancel(context.Background())
	w, err := r.CreateBlob(ctx, "plans/p1/copy-x")
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(100 * time.Millisecond) // the call is on its way, waiting for a body
	cancel()
	select {
	case <-w.(*blobWriter).done:
	case <-time.After(3 * time.Second):
		t.Fatal("the writer's call is still waiting after its context ended")
	}
}
