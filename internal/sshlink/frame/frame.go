// Package frame multiplexes streams over one byte stream, such as an ssh
// session's stdin and stdout: many requests and chunk transfers at once,
// each with its own flow control, so a big transfer never holds up a small
// request.
//
// Each side first writes Magic. What the other side reads before it (a
// shell's login banner, say) is kept as Session.Banner. Then frames:
//
//	type (1 byte) | stream ID (4 bytes, big-endian) | length (4) | payload
//
// Types: open, data, close (no more data from this side), reset (payload:
// the reason), window (payload: 4-byte increment), ping and pong (no
// payload). Streams opened by the side that dialed (Client) have odd IDs,
// the other side's even. A side may send up to Window bytes on a stream
// before the receiver gives window back as its reader consumes them.
package frame

import (
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"sync"
	"sync/atomic"
	"time"
)

// Magic starts the stream in both directions; the digit is the version.
const Magic = "fkbr-link 1\n"

const (
	typeOpen byte = iota + 1
	typeData
	typeClose
	typeReset
	typeWindow
	typePing
	typePong
)

const (
	headerLen = 9
	// MaxPayload bounds a frame; longer writes are split.
	MaxPayload = 64 << 10
	// Window is what a side may send on a stream before it is acked.
	Window = 4 << 20
	// maxBanner is how much may come before the peer's Magic.
	maxBanner = 64 << 10
	maxReason = 1024
	// maxPending is how many opened streams may wait for Accept.
	maxPending = 256
)

var (
	// ErrClosed: the session ended (EOF, a write error, Close, or no frame
	// from the peer for too long).
	ErrClosed = errors.New("link closed")
	// ErrReset: the other side reset the stream; Reset's reason follows.
	ErrReset = errors.New("stream reset")
)

// Options tune keepalive; zero values pick the defaults.
type Options struct {
	// Ping is how often an idle session sends a ping (default 15 s).
	Ping time.Duration
	// Dead ends the session after this long without any frame from the
	// peer (default 3 × Ping).
	Dead time.Duration
}

// Session is one end of a link.
type Session struct {
	conn   io.ReadWriteCloser
	r      *bufio.Reader
	wmu    sync.Mutex
	opts   Options
	Banner []byte // what came before the peer's Magic

	mu      sync.Mutex
	streams map[uint32]*Stream
	next    uint32
	parity  uint32 // of the IDs this side opens
	pending chan *Stream
	err     error
	done    chan struct{}

	lastRecv atomic.Int64 // unix ns
}

// Client starts the side that dialed (odd stream IDs).
func Client(conn io.ReadWriteCloser, opts Options) (*Session, error) {
	return start(conn, opts, 1)
}

// Server starts the side that was started (even stream IDs).
func Server(conn io.ReadWriteCloser, opts Options) (*Session, error) {
	return start(conn, opts, 2)
}

func start(conn io.ReadWriteCloser, opts Options, first uint32) (*Session, error) {
	if opts.Ping <= 0 {
		opts.Ping = 15 * time.Second
	}
	if opts.Dead <= 0 {
		opts.Dead = 3 * opts.Ping
	}
	s := &Session{
		conn: conn, r: bufio.NewReaderSize(conn, 256<<10), opts: opts,
		streams: map[uint32]*Stream{}, next: first, parity: first % 2,
		pending: make(chan *Stream, maxPending), done: make(chan struct{}),
	}
	werr := make(chan error, 1)
	go func() { _, err := io.WriteString(conn, Magic); werr <- err }()
	if err := s.readMagic(); err != nil {
		conn.Close()
		return nil, err
	}
	if err := <-werr; err != nil {
		conn.Close()
		return nil, fmt.Errorf("link: %w", err)
	}
	s.lastRecv.Store(time.Now().UnixNano())
	go s.readLoop()
	go s.keepalive()
	return s, nil
}

// readMagic skips what comes before Magic (at most maxBanner bytes).
func (s *Session) readMagic() error {
	var seen []byte
	for {
		b, err := s.r.ReadByte()
		if err != nil {
			if len(seen) > 0 {
				return fmt.Errorf("link: no %q from the other side, it said: %q", Magic[:len(Magic)-1], firstLine(seen))
			}
			return fmt.Errorf("link: %w", err)
		}
		seen = append(seen, b)
		if bytes.HasSuffix(seen, []byte(Magic)) {
			s.Banner = seen[:len(seen)-len(Magic)]
			return nil
		}
		if len(seen) > maxBanner+len(Magic) {
			return fmt.Errorf("link: no %q in the first %d bytes, they start: %q", Magic[:len(Magic)-1], maxBanner, firstLine(seen))
		}
	}
}

func firstLine(b []byte) string {
	if i := bytes.IndexByte(b, '\n'); i >= 0 {
		b = b[:i]
	}
	if len(b) > 200 {
		b = b[:200]
	}
	return string(b)
}

// Done is closed when the session has ended; Err then says why.
func (s *Session) Done() <-chan struct{} { return s.done }

func (s *Session) Err() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.err
}

// Close ends the session and every stream on it.
func (s *Session) Close() error {
	s.fail(ErrClosed)
	return nil
}

func (s *Session) fail(err error) {
	s.mu.Lock()
	if s.err != nil {
		s.mu.Unlock()
		return
	}
	s.err = err
	streams := s.streams
	s.streams = map[uint32]*Stream{}
	close(s.done)
	s.mu.Unlock()
	s.conn.Close()
	for _, st := range streams {
		st.ended(err)
	}
}

// Open starts a stream to the other side.
func (s *Session) Open() (*Stream, error) {
	s.mu.Lock()
	if s.err != nil {
		s.mu.Unlock()
		return nil, s.err
	}
	id := s.next
	s.next += 2
	st := newStream(s, id)
	s.streams[id] = st
	s.mu.Unlock()
	if err := s.write(typeOpen, id, nil); err != nil {
		return nil, err
	}
	return st, nil
}

// Accept returns the next stream the other side opened.
func (s *Session) Accept(ctx context.Context) (*Stream, error) {
	select {
	case st := <-s.pending:
		return st, nil
	case <-s.done:
		return nil, s.Err()
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (s *Session) write(typ byte, id uint32, payload []byte) error {
	buf := make([]byte, headerLen+len(payload))
	buf[0] = typ
	binary.BigEndian.PutUint32(buf[1:], id)
	binary.BigEndian.PutUint32(buf[5:], uint32(len(payload)))
	copy(buf[headerLen:], payload)
	s.wmu.Lock()
	defer s.wmu.Unlock()
	if err := s.Err(); err != nil {
		return err
	}
	if _, err := s.conn.Write(buf); err != nil {
		s.fail(fmt.Errorf("%w: %w", ErrClosed, err))
		return s.Err()
	}
	return nil
}

func (s *Session) keepalive() {
	t := time.NewTicker(s.opts.Ping)
	defer t.Stop()
	for {
		select {
		case <-s.done:
			return
		case <-t.C:
			if time.Since(time.Unix(0, s.lastRecv.Load())) > s.opts.Dead {
				s.fail(fmt.Errorf("%w: nothing from the other side for %s", ErrClosed, s.opts.Dead))
				return
			}
			s.write(typePing, 0, nil)
		}
	}
}

func (s *Session) readLoop() {
	var hdr [headerLen]byte
	for {
		if _, err := io.ReadFull(s.r, hdr[:]); err != nil {
			if errors.Is(err, io.EOF) {
				err = ErrClosed
			} else {
				err = fmt.Errorf("%w: %w", ErrClosed, err)
			}
			s.fail(err)
			return
		}
		typ, id, n := hdr[0], binary.BigEndian.Uint32(hdr[1:]), binary.BigEndian.Uint32(hdr[5:])
		if err := checkLen(typ, n); err != nil {
			s.fail(fmt.Errorf("%w: %w", ErrClosed, err))
			return
		}
		payload := make([]byte, n)
		if _, err := io.ReadFull(s.r, payload); err != nil {
			s.fail(fmt.Errorf("%w: %w", ErrClosed, err))
			return
		}
		s.lastRecv.Store(time.Now().UnixNano())
		if err := s.dispatch(typ, id, payload); err != nil {
			s.fail(fmt.Errorf("%w: %w", ErrClosed, err))
			return
		}
	}
}

func checkLen(typ byte, n uint32) error {
	switch typ {
	case typeData:
		if n > MaxPayload {
			return fmt.Errorf("data frame of %d bytes", n)
		}
	case typeReset:
		if n > maxReason {
			return fmt.Errorf("reset reason of %d bytes", n)
		}
	case typeWindow:
		if n != 4 {
			return fmt.Errorf("window frame of %d bytes", n)
		}
	case typeOpen, typeClose, typePing, typePong:
		if n != 0 {
			return fmt.Errorf("frame type %d with %d bytes", typ, n)
		}
	default:
		return fmt.Errorf("unknown frame type %d", typ)
	}
	return nil
}

func (s *Session) dispatch(typ byte, id uint32, payload []byte) error {
	switch typ {
	case typePing:
		go s.write(typePong, 0, nil) // never block the reader on a write
		return nil
	case typePong:
		return nil
	case typeOpen:
		if id == 0 || id%2 == s.parity {
			return fmt.Errorf("stream %d opened with this side's parity", id)
		}
		s.mu.Lock()
		if s.err != nil {
			s.mu.Unlock()
			return nil
		}
		if _, ok := s.streams[id]; ok {
			s.mu.Unlock()
			return fmt.Errorf("stream %d opened twice", id)
		}
		st := newStream(s, id)
		s.streams[id] = st
		s.mu.Unlock()
		select {
		case s.pending <- st:
		default:
			go st.Reset(errors.New("too many streams waiting"))
		}
		return nil
	}
	s.mu.Lock()
	st := s.streams[id]
	s.mu.Unlock()
	if st == nil {
		return nil // reset here already; the peer did not know yet
	}
	switch typ {
	case typeData:
		return st.received(payload)
	case typeClose:
		st.remoteClose()
	case typeReset:
		st.remoteReset(string(payload))
	case typeWindow:
		st.grant(int(binary.BigEndian.Uint32(payload)))
	}
	return nil
}

func (s *Session) forget(id uint32) {
	s.mu.Lock()
	delete(s.streams, id)
	s.mu.Unlock()
}

// Stream is one bidirectional stream. Read and Write may be used by one
// goroutine each, at the same time.
type Stream struct {
	s  *Session
	id uint32

	mu           sync.Mutex
	cond         *sync.Cond
	buf          bytes.Buffer
	consumed     int // read but not yet given back as window
	sendWin      int
	localClosed  bool // CloseWrite was called
	remoteClosed bool // the peer sent close
	err          error
	ctx          context.Context
	cancel       context.CancelFunc
}

func newStream(s *Session, id uint32) *Stream {
	st := &Stream{s: s, id: id, sendWin: Window}
	st.cond = sync.NewCond(&st.mu)
	st.ctx, st.cancel = context.WithCancel(context.Background())
	return st
}

// Context ends when the stream is reset (by either side) or the session
// ends: a handler serving the stream should stop then.
func (st *Stream) Context() context.Context { return st.ctx }

func (st *Stream) received(p []byte) error {
	st.mu.Lock()
	defer st.mu.Unlock()
	if st.err != nil {
		return nil // reset here; drop what was in flight
	}
	if st.buf.Len()+len(p) > Window {
		return fmt.Errorf("stream %d: peer sent past its window", st.id)
	}
	st.buf.Write(p)
	st.cond.Broadcast()
	return nil
}

func (st *Stream) remoteClose() {
	st.mu.Lock()
	st.remoteClosed = true
	both := st.localClosed
	st.cond.Broadcast()
	st.mu.Unlock()
	if both {
		st.s.forget(st.id)
	}
}

func (st *Stream) remoteReset(reason string) {
	st.ended(fmt.Errorf("%w: %s", ErrReset, reason))
	st.s.forget(st.id)
}

// ended marks the stream failed (reset, or the session gone).
func (st *Stream) ended(err error) {
	st.mu.Lock()
	if st.err == nil {
		st.err = err
	}
	st.cond.Broadcast()
	st.mu.Unlock()
	st.cancel()
}

func (st *Stream) grant(n int) {
	st.mu.Lock()
	st.sendWin += n
	st.cond.Broadcast()
	st.mu.Unlock()
}

// Read returns what the peer sent, then io.EOF once it closed its side.
// Data that arrived before a reset or close is read first.
func (st *Stream) Read(p []byte) (int, error) {
	st.mu.Lock()
	for st.buf.Len() == 0 && !st.remoteClosed && st.err == nil {
		st.cond.Wait()
	}
	if st.buf.Len() > 0 {
		n, _ := st.buf.Read(p)
		st.consumed += n
		give := 0
		if st.consumed >= Window/4 {
			give, st.consumed = st.consumed, 0
		}
		failed := st.err != nil
		st.mu.Unlock()
		if give > 0 && !failed {
			var b [4]byte
			binary.BigEndian.PutUint32(b[:], uint32(give))
			st.s.write(typeWindow, st.id, b[:])
		}
		return n, nil
	}
	defer st.mu.Unlock()
	if st.remoteClosed {
		return 0, io.EOF
	}
	return 0, st.err
}

// Write sends p, waiting for window as needed.
func (st *Stream) Write(p []byte) (int, error) {
	written := 0
	for len(p) > 0 {
		st.mu.Lock()
		for st.sendWin == 0 && st.err == nil && !st.localClosed {
			st.cond.Wait()
		}
		if err := st.err; err != nil {
			st.mu.Unlock()
			return written, err
		}
		if st.localClosed {
			st.mu.Unlock()
			return written, errors.New("write after CloseWrite")
		}
		n := min(len(p), st.sendWin, MaxPayload)
		st.sendWin -= n
		st.mu.Unlock()
		if err := st.s.write(typeData, st.id, p[:n]); err != nil {
			return written, err
		}
		written += n
		p = p[n:]
	}
	return written, nil
}

// CloseWrite tells the peer nothing more comes from this side.
func (st *Stream) CloseWrite() error {
	st.mu.Lock()
	if st.localClosed || st.err != nil {
		err := st.err
		st.mu.Unlock()
		return err
	}
	st.localClosed = true
	both := st.remoteClosed
	st.cond.Broadcast()
	st.mu.Unlock()
	err := st.s.write(typeClose, st.id, nil)
	if both {
		st.s.forget(st.id)
		st.cancel()
	}
	return err
}

// Reset aborts the stream in both directions, telling the peer why.
func (st *Stream) Reset(reason error) {
	st.mu.Lock()
	if st.err != nil || (st.localClosed && st.remoteClosed) {
		st.mu.Unlock()
		return
	}
	st.mu.Unlock()
	msg := "reset"
	if reason != nil {
		msg = reason.Error()
	}
	if len(msg) > maxReason {
		msg = msg[:maxReason]
	}
	st.ended(fmt.Errorf("%w: %s", ErrReset, msg))
	st.s.forget(st.id)
	st.s.write(typeReset, st.id, []byte(msg))
}

// Close ends this side: CloseWrite, and a reset if the peer has not
// finished (whatever it still sends is not wanted).
func (st *Stream) Close() error {
	st.CloseWrite()
	st.mu.Lock()
	done := st.remoteClosed || st.err != nil
	st.mu.Unlock()
	if !done {
		st.Reset(errors.New("closed"))
	}
	st.cancel()
	return nil
}
