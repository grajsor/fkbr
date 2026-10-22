package sshlink

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"syscall"
	"testing"

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
			call, err := open(ctx, cli, "chunk.open", nil, nil)
			if err != nil {
				t.Fatal(err)
			}
			defer call.Close()
			if err := call.answer(ctx, nil); err != nil {
				t.Fatal(err)
			}
			_, err = io.ReadAll(call)
			if err == nil {
				t.Fatal("a cut-short body read as complete")
			}
			if got := errors.Is(err, syscall.EIO); got != c.eio {
				t.Errorf("EIO = %v, want %v: %v", got, c.eio, err)
			}
			if !errors.Is(err, frame.ErrReset) {
				t.Errorf("not a reset: %v", err)
			}
		})
	}
}
