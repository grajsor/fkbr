package sshlink

import (
	"context"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"fkbr/internal/api"
	"fkbr/internal/sshlink/frame"
	"fkbr/internal/store"
	"fkbr/internal/transport"
)

// queued puts requests for web01 in its inbox on the home node, bodies of
// size bytes.
func queued(t *testing.T, dir string, size int, ids ...string) {
	t.Helper()
	home, err := store.Open(dir, "home", store.Options{Private: true})
	if err != nil {
		t.Fatal(err)
	}
	var body []byte
	if size > 0 {
		body = []byte(`"` + strings.Repeat("a", size) + `"`)
	}
	for _, id := range ids {
		m := &transport.Message{Protocol: api.ProtocolVersion, ID: id, Type: "list", From: "home", To: "web01", Notify: true, Body: body}
		if err := home.Send(context.Background(), m); err != nil {
			t.Fatal(err)
		}
	}
}

func shortFetchWait(t *testing.T) {
	old := fetchWait
	fetchWait = 20 * time.Millisecond
	t.Cleanup(func() { fetchWait = old })
}

// fetchAck is a linked node's fetch that acks answer ack.
func fetchAck(t *testing.T, cli *frame.Session, ack uint64) *fetched {
	t.Helper()
	var f fetched
	if err := do(context.Background(), cli, "fetch", &fetchArgs{Ack: ack}, &f); err != nil {
		t.Fatal(err)
	}
	return &f
}

func ids(f *fetched) string {
	var s []string
	for _, m := range f.Msgs {
		s = append(s, m.ID)
	}
	return strings.Join(s, ",")
}

// A fetch answer that does not arrive (not acked) goes out again; once
// acked, its requests do not.
func TestLostFetchAnswerSentAgain(t *testing.T) {
	shortFetchWait(t)
	dir := t.TempDir()
	_, _, cli := linked(t, dir)
	queued(t, dir, 0, "r1", "r2")
	first := fetchAck(t, cli, 0)
	if ids(first) != "r1,r2" {
		t.Fatalf("first answer %q", ids(first))
	}
	// As if the first answer was lost: the node acks what it had before.
	again := fetchAck(t, cli, 0)
	if ids(again) != "r1" {
		t.Fatalf("after a lost answer got %q, want r1 alone", ids(again))
	}
	next := fetchAck(t, cli, again.Seq)
	if ids(next) != "r2" {
		t.Fatalf("after the ack got %q, want r2", ids(next))
	}
	if last := fetchAck(t, cli, next.Seq); ids(last) != "" {
		t.Fatalf("acked requests sent again: %q", ids(last))
	}
}

// A linked node's Remote acks each answer it reads with its next fetch.
func TestRemoteFetchAcks(t *testing.T) {
	shortFetchWait(t)
	dir := t.TempDir()
	_, _, cli := linked(t, dir)
	queued(t, dir, 0, "r1")
	r := &Remote{sess: cli}
	ctx := context.Background()
	got, err := r.Fetch(ctx)
	if err != nil || len(got) != 1 || got[0].ID != "r1" {
		t.Fatalf("Fetch = %v, %v", got, err)
	}
	if got, err := r.Fetch(ctx); err != nil || len(got) != 0 {
		t.Fatalf("second Fetch = %v, %v; want nothing", got, err)
	}
}

// An answer carries at most fetchBudget bytes of requests: a backlog
// goes out over several fetches, every request once.
func TestFetchAnswerBounded(t *testing.T) {
	shortFetchWait(t)
	dir := t.TempDir()
	_, _, cli := linked(t, dir)
	queued(t, dir, 6<<20, "r1", "r2", "r3")
	var seen []string
	var ack uint64
	for range 4 {
		f := fetchAck(t, cli, ack)
		if len(f.Msgs) > 2 {
			t.Fatalf("an answer with %d requests of 6 MiB", len(f.Msgs))
		}
		for _, m := range f.Msgs {
			seen = append(seen, m.ID)
		}
		ack = f.Seq
	}
	if strings.Join(seen, ",") != "r1,r2,r3" {
		t.Fatalf("requests arrived as %v", seen)
	}
}

// A request that never arrives is sent alone, then parked in failed/
// after maxSendTries; the ones behind it go on.
func TestRequestThatNeverArrivesParked(t *testing.T) {
	shortFetchWait(t)
	dir := t.TempDir()
	_, _, cli := linked(t, dir)
	queued(t, dir, 0, "r1", "r2")
	for i := 0; ; i++ {
		f := fetchAck(t, cli, 0) // never acks
		if i > 0 && strings.Contains(ids(f), "r1,") {
			t.Fatalf("try %d: r1 sent with others after a failure: %q", i, ids(f))
		}
		if ids(f) == "r2" {
			break
		}
		if i > maxSendTries+1 {
			t.Fatalf("r1 not parked after %d tries", i)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, "inbox", "web01", "failed", "r1.json")); err != nil {
		t.Errorf("r1 not in failed/: %v", err)
	}
}

// A linked node of a build before fetch acks (link 0) would get every
// request again and again: it is refused at hello.
func TestOlderLinkRefused(t *testing.T) {
	h := NewHome(t.TempDir(), "home", store.Options{}, nil)
	if _, err := h.Open(); err != nil {
		t.Fatal(err)
	}
	a, b := net.Pipe()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go h.ServeConn(ctx, b, "web01", frame.Options{})
	sess, err := frame.Server(a, frame.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()
	var now time.Time
	err = do(ctx, sess, "hello", &hello{Node: "web01", Protocol: api.ProtocolVersion, Version: "old"}, &now)
	if err == nil || !strings.Contains(err.Error(), "link 0") {
		t.Fatalf("hello without a link version: %v", err)
	}
}

// syncLog is a log sink for a Home serving several links.
type syncLog struct {
	mu sync.Mutex
	b  strings.Builder
}

func (l *syncLog) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *syncLog) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

// A link the home node ended by stopping its node is no link down; one
// that ends by itself still is (lab ssh test 2026-10-22).
func TestStopLogsNoLinkDown(t *testing.T) {
	var log syncLog
	h := NewHome(t.TempDir(), "home", store.Options{}, slog.New(slog.NewTextHandler(&log, nil)))
	if _, err := h.Open(); err != nil {
		t.Fatal(err)
	}
	dial := func(node string) (*Remote, chan struct{}) {
		a, b := net.Pipe()
		served := make(chan struct{})
		go func() { h.ServeConn(context.Background(), b, node+"-host", frame.Options{}); close(served) }()
		r, err := Dial(context.Background(), a, node, "test", filepath.Join(t.TempDir(), "chunks"), frame.Options{}, nil)
		if err != nil {
			t.Fatal(err)
		}
		return r, served
	}
	lost, lostDone := dial("web01")
	lost.Close() // ends by itself
	<-lostDone
	if !strings.Contains(log.String(), `msg="ssh link down"`) {
		t.Fatalf("a lost link not logged:\n%s", log.String())
	}
	r, done := dial("web02")
	go func() { <-r.Stopping(); r.Close() }() // as fkbr link does
	h.Stop(5 * time.Second)
	<-done
	if n := strings.Count(log.String(), `msg="ssh link down"`); n != 1 {
		t.Errorf("%d links down logged, want only the lost one:\n%s", n, log.String())
	}
}
