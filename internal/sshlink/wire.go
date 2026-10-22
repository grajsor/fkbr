package sshlink

// Calls over a link. Each call is one stream: a JSON header line naming
// the method, an optional raw body, then the answer: a JSON header line
// (an error, or the result) and an optional raw body.

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"syscall"

	"fkbr/internal/api"
	"fkbr/internal/sshlink/frame"
	"fkbr/internal/transport"
)

// maxHeader bounds a header line, as share reads are bounded (64 MiB).
const maxHeader = 64 << 20

// ErrLinkDown: the session to the other node is gone. It matches
// syscall.EIO too, so transfers ride it out while the link comes back,
// as they ride out a soft NFS mount.
var ErrLinkDown = errors.New("ssh link down")

func linkDown(err error) error {
	return fmt.Errorf("%w: %w (%w)", ErrLinkDown, syscall.EIO, err)
}

type request struct {
	M string          `json:"m"`
	A json.RawMessage `json:"a,omitempty"`
}

type answer struct {
	E string          `json:"e,omitempty"`
	C string          `json:"c,omitempty"`
	R json.RawMessage `json:"r,omitempty"`
}

// Codes beyond api's, for errors the engine tells apart.
var extraCodes = []struct {
	err  error
	code string
}{
	{fs.ErrNotExist, "not_exist"},
	{transport.ErrNotRegistered, "not_registered"},
	{syscall.EIO, "eio"},
	{syscall.ETIMEDOUT, "etimedout"},
	{syscall.ESTALE, "estale"},
	{context.Canceled, "canceled"},
	{context.DeadlineExceeded, "deadline"},
}

func errorCode(err error) string {
	if c := api.ErrorCode(err); c != "" {
		return c
	}
	for _, c := range extraCodes {
		if errors.Is(err, c.err) {
			return c.code
		}
	}
	return ""
}

// remoteError is an error from the other node; it unwraps to the sentinel
// its code names, so errors.Is works across the link.
type remoteError struct {
	msg, code string
}

func (e *remoteError) Error() string { return e.msg }

func (e *remoteError) Unwrap() error {
	if err := api.ErrorForCode(e.code); err != nil {
		return err
	}
	for _, c := range extraCodes {
		if c.code == e.code {
			return c.err
		}
	}
	return nil
}

func decodeErr(a *answer) error {
	if a.E == "" {
		return nil
	}
	return &remoteError{a.E, a.C}
}

func readLine(r *bufio.Reader) ([]byte, error) {
	var line []byte
	for {
		part, err := r.ReadSlice('\n')
		line = append(line, part...)
		if err == nil {
			return line, nil
		}
		if !errors.Is(err, bufio.ErrBufferFull) {
			return nil, err
		}
		if len(line) > maxHeader {
			return nil, fmt.Errorf("header line over %d bytes", maxHeader)
		}
	}
}

func writeJSONLine(w io.Writer, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	_, err = w.Write(append(b, '\n'))
	return err
}

// call is one call in progress: Body reads the answer's raw body.
type call struct {
	st   *frame.Stream
	r    *bufio.Reader
	stop func() bool
}

func (c *call) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	if err != nil && !errors.Is(err, io.EOF) {
		err = linkErr(err)
	}
	return n, err
}

func (c *call) Close() error {
	c.stop()
	return c.st.Close()
}

// linkErr turns a session's end into ErrLinkDown; a reset stays as it is.
func linkErr(err error) error {
	if errors.Is(err, frame.ErrClosed) {
		return linkDown(err)
	}
	return err
}

// open starts a call: it sends method and args and, if body is not nil,
// streams body after them. The caller then calls finish.
func open(ctx context.Context, s *frame.Session, method string, args any, body io.Reader) (*call, error) {
	st, err := s.Open()
	if err != nil {
		return nil, linkErr(err)
	}
	stop := context.AfterFunc(ctx, func() { st.Reset(ctx.Err()) })
	c := &call{st: st, r: bufio.NewReaderSize(st, 64<<10), stop: stop}
	req := request{M: method}
	if args != nil {
		if req.A, err = json.Marshal(args); err != nil {
			c.Close()
			return nil, err
		}
	}
	if err := writeJSONLine(st, &req); err != nil {
		c.Close()
		return nil, c.ctxErr(ctx, err)
	}
	if body != nil {
		if _, err := io.Copy(st, body); err != nil {
			c.Close()
			return nil, c.ctxErr(ctx, err)
		}
	}
	if err := st.CloseWrite(); err != nil {
		c.Close()
		return nil, c.ctxErr(ctx, err)
	}
	return c, nil
}

func (c *call) ctxErr(ctx context.Context, err error) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	return linkErr(err)
}

// answer reads the answer header into out; the raw body follows in c.
func (c *call) answer(ctx context.Context, out any) error {
	line, err := readLine(c.r)
	if err != nil {
		return c.ctxErr(ctx, err)
	}
	var a answer
	if err := json.Unmarshal(line, &a); err != nil {
		return fmt.Errorf("ssh link: bad answer: %w", err)
	}
	if err := decodeErr(&a); err != nil {
		return err
	}
	if out != nil && len(a.R) > 0 {
		return json.Unmarshal(a.R, out)
	}
	return nil
}

// do is a call without bodies.
func do(ctx context.Context, s *frame.Session, method string, args, out any) error {
	c, err := open(ctx, s, method, args, nil)
	if err != nil {
		return err
	}
	defer c.Close()
	if err := c.answer(ctx, out); err != nil {
		return err
	}
	io.Copy(io.Discard, c) // to the end, so Close does not reset
	return nil
}

// handler serves one method. args is the request's JSON; body its raw
// body. It answers with res (any JSON value, or nil), or writes the
// answer itself through w and returns errAnswered.
type handler func(ctx context.Context, args json.RawMessage, body io.Reader, w *answerWriter) (res any, err error)

var errAnswered = errors.New("answered")

type answerWriter struct {
	st      *frame.Stream
	written bool
}

// Header sends the answer's header (res, or nil); write the body after.
func (w *answerWriter) Header(res any) error {
	w.written = true
	a := answer{}
	if res != nil {
		b, err := json.Marshal(res)
		if err != nil {
			return err
		}
		a.R = b
	}
	return writeJSONLine(w.st, &a)
}

func (w *answerWriter) Write(p []byte) (int, error) { return w.st.Write(p) }

func (w *answerWriter) fail(err error) {
	if !w.written {
		w.written = true
		writeJSONLine(w.st, &answer{E: err.Error(), C: errorCode(err)})
	}
}

// serve answers the calls the other side opens until the session ends.
func serve(ctx context.Context, s *frame.Session, handlers map[string]handler, log *slog.Logger) {
	for {
		st, err := s.Accept(ctx)
		if err != nil {
			return
		}
		go serveOne(ctx, st, handlers, log)
	}
}

func serveOne(ctx context.Context, st *frame.Stream, handlers map[string]handler, log *slog.Logger) {
	defer st.Close()
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	stop := context.AfterFunc(st.Context(), cancel)
	defer stop()
	r := bufio.NewReaderSize(st, 64<<10)
	w := &answerWriter{st: st}
	line, err := readLine(r)
	if err != nil {
		return
	}
	var req request
	if err := json.Unmarshal(line, &req); err != nil {
		w.fail(fmt.Errorf("ssh link: bad request: %w", err))
		st.CloseWrite()
		return
	}
	h := handlers[req.M]
	if h == nil {
		w.fail(fmt.Errorf("ssh link: unknown call %q", req.M))
		st.CloseWrite()
		return
	}
	res, err := h(ctx, req.A, r, w)
	switch {
	case errors.Is(err, errAnswered):
	case err != nil:
		if !w.written {
			w.fail(err)
		} else if log != nil {
			log.Debug("ssh link: call failed after its answer", "call", req.M, "err", err)
			st.Reset(err)
			return
		}
	case !w.written:
		w.Header(res)
	}
	st.CloseWrite()
	// Wait for the caller to finish reading, so Close does not reset.
	io.Copy(io.Discard, r)
}

// args decodes a request's arguments.
func args[T any](raw json.RawMessage) (*T, error) {
	var v T
	if len(raw) == 0 {
		return &v, nil
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	if err := d.Decode(&v); err != nil {
		return nil, fmt.Errorf("ssh link: %w", err)
	}
	return &v, nil
}
