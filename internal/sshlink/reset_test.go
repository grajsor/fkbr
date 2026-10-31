package sshlink

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"syscall"
	"testing"
	"time"

	"fkbr/internal/sshlink/frame"
)

// sessionPair is two ends of a link, the caller's and the server's.
func sessionPair(t *testing.T) (cli, srv *frame.Session) {
	t.Helper()
	c1, c2 := net.Pipe()
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
	t.Cleanup(func() { cli.Close(); srv.Close() })
	return cli, srv
}

// A call that fails after its answer started reaches the caller as the
// error it was: a chunk the home node relays from a node whose link then
// dies reads as a link down (EIO), which transfers ride out, not as a
// reset that fails the job (lab ssh test, the source's side cut). Other
// failures stay failures, and a cut-short body never reads as complete.
func TestFailureAfterAnswerKeepsItsCode(t *testing.T) {
	for _, c := range []struct {
		name string
		err  error
		eio  bool
	}{
		{"source link down", linkDown(errors.New("nothing from the other side for 45s")), true},
		{"other failure", errors.New("disk on fire"), false},
	} {
		t.Run(c.name, func(t *testing.T) {
			ctx := context.Background()
			cli, srv := sessionPair(t)
			go serve(ctx, srv, map[string]handler{"chunk.open": func(_ context.Context, _ json.RawMessage, _ io.Reader, w *answerWriter) (any, error) {
				if err := w.Header(nil); err != nil {
					return nil, err
				}
				w.Write([]byte("half a chunk"))
				return nil, c.err
			}}, nil) // no logger: the reset must not depend on one
			// The reset can come before open or answer is done (the
			// handler is quick; under load it was there first): wherever
			// it shows, it must keep its code.
			call, err := open(ctx, cli, "chunk.open", nil, nil)
			if err == nil {
				defer call.Close()
				if err = call.answer(ctx, nil); err == nil {
					if _, err = io.ReadAll(call); err == nil {
						t.Fatal("a cut-short body read as complete")
					}
				}
			}
			checkCodedReset(t, err, c.eio)
		})
	}
}

// A handler that fails while the caller is still sending its body: the
// reset reaches open's write, and keeps its code there too.
func TestFailureWhileSendingKeepsItsCode(t *testing.T) {
	for _, c := range []struct {
		name string
		err  error
		eio  bool
	}{
		{"source link down", linkDown(errors.New("nothing from the other side for 45s")), true},
		{"other failure", errors.New("disk on fire"), false},
	} {
		t.Run(c.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			cli, srv := sessionPair(t)
			failed := make(chan struct{})
			go serve(ctx, srv, map[string]handler{"chunk.put": func(_ context.Context, _ json.RawMessage, _ io.Reader, w *answerWriter) (any, error) {
				defer close(failed)
				if err := w.Header(nil); err != nil {
					return nil, err
				}
				return nil, c.err
			}}, nil)
			_, err := open(ctx, cli, "chunk.put", nil, endlessAfter(failed))
			if err == nil {
				t.Fatal("open sent an endless body to a failed handler")
			}
			checkCodedReset(t, err, c.eio)
		})
	}
}

// endlessAfter is a body that starts once ready is closed and never ends.
type endlessAfter chan struct{}

func (r endlessAfter) Read(p []byte) (int, error) {
	<-r
	for i := range p {
		p[i] = 'x'
	}
	return len(p), nil
}

func checkCodedReset(t *testing.T, err error, eio bool) {
	t.Helper()
	if err == nil {
		t.Fatal("no error")
	}
	if got := errors.Is(err, syscall.EIO); got != eio {
		t.Errorf("EIO = %v, want %v: %v", got, eio, err)
	}
	if !errors.Is(err, frame.ErrReset) {
		t.Errorf("not a reset: %v", err)
	}
}

// serve returns only once the calls in progress have: a home node's
// handler still wrote a record after its session ended, into a test's
// share being removed ("directory not empty", 1 full ssh run in 25).
func TestServeWaitsForCalls(t *testing.T) {
	cli, srv := sessionPair(t)
	started, release := make(chan struct{}), make(chan struct{})
	served := make(chan struct{})
	go func() {
		serve(context.Background(), srv, map[string]handler{"status.put": func(context.Context, json.RawMessage, io.Reader, *answerWriter) (any, error) {
			close(started)
			<-release // a write that goes on after the caller is gone
			return nil, nil
		}}, nil)
		close(served)
	}()
	go do(context.Background(), cli, "status.put", nil, nil)
	<-started
	cli.Close()
	<-srv.Done()
	select {
	case <-served:
		t.Fatal("serve returned while a call was still running")
	case <-time.After(100 * time.Millisecond):
	}
	close(release)
	select {
	case <-served:
	case <-time.After(5 * time.Second):
		t.Fatal("serve did not return after its last call")
	}
}

// A record call the home node does not answer within recordTimeout is a
// link down (EIO), which transfers ride out; the keepalive takes longer to
// call the link dead. A bare deadline failed an ssh copy whose source link
// was cut for 32 s (torture v2, lab).
func TestUnansweredRecordCallIsLinkDown(t *testing.T) {
	defer func(d time.Duration) { recordTimeout = d }(recordTimeout)
	recordTimeout = 100 * time.Millisecond
	cli, srv := sessionPair(t)
	release := make(chan struct{})
	defer close(release)
	go serve(context.Background(), srv, map[string]handler{"status.put": func(context.Context, json.RawMessage, io.Reader, *answerWriter) (any, error) {
		<-release // the link is cut: no answer
		return nil, nil
	}}, nil)
	r := &Remote{sess: cli}
	err := r.rec(context.Background(), "status.put", nil, nil)
	if !errors.Is(err, syscall.EIO) || !errors.Is(err, ErrLinkDown) {
		t.Fatalf("unanswered record call: %v, want a link down (EIO)", err)
	}
	// The caller's own end is not a link down.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := do(ctx, cli, "status.put", nil, nil); errors.Is(err, syscall.EIO) {
		t.Fatalf("cancelled call: %v, want no EIO", err)
	}
}
