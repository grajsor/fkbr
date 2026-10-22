package frame

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"strings"
	"sync"
	"testing"
	"time"
)

func pair(t *testing.T, opts Options) (client, server *Session) {
	t.Helper()
	a, b := net.Pipe()
	errs := make(chan error, 1)
	go func() {
		var err error
		server, err = Server(b, opts)
		errs <- err
	}()
	client, err := Client(a, opts)
	if err != nil {
		t.Fatal(err)
	}
	if err := <-errs; err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { client.Close(); server.Close() })
	return client, server
}

// echo serves every stream by sending back what it reads.
func echo(s *Session) {
	for {
		st, err := s.Accept(context.Background())
		if err != nil {
			return
		}
		go func() {
			io.Copy(st, st)
			st.CloseWrite()
		}()
	}
}

func roundTrip(t *testing.T, c *Session, data []byte) {
	t.Helper()
	st, err := c.Open()
	if err != nil {
		t.Fatal(err)
	}
	got := make(chan []byte, 1)
	go func() { b, _ := io.ReadAll(st); got <- b }()
	if _, err := st.Write(data); err != nil {
		t.Fatal(err)
	}
	st.CloseWrite()
	if b := <-got; !bytes.Equal(b, data) {
		t.Fatalf("echoed %d bytes, sent %d", len(b), len(data))
	}
	st.Close()
}

func TestEchoBig(t *testing.T) {
	c, s := pair(t, Options{})
	go echo(s)
	data := make([]byte, 3*Window+12345) // several windows, odd tail
	rand.Read(data)
	roundTrip(t, c, data)
}

func TestManyStreamsAtOnce(t *testing.T) {
	c, s := pair(t, Options{})
	go echo(s)
	var wg sync.WaitGroup
	for i := range 40 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			data := bytes.Repeat([]byte{byte(i)}, 100<<10+i)
			roundTrip(t, c, data)
		}()
	}
	wg.Wait()
}

// A stream whose reader stalls fills its window; other streams go on.
func TestStalledStreamDoesNotBlockOthers(t *testing.T) {
	c, s := pair(t, Options{})
	stalled := make(chan *Stream, 1)
	go func() {
		first, _ := s.Accept(context.Background())
		stalled <- first // never read
		echo(s)
	}()
	big, _ := c.Open()
	wrote := make(chan error, 1)
	go func() { _, err := big.Write(make([]byte, 2*Window)); wrote <- err }()
	<-stalled
	time.Sleep(50 * time.Millisecond)
	select {
	case err := <-wrote:
		t.Fatalf("write past the window returned: %v", err)
	default:
	}
	done := make(chan struct{})
	go func() { roundTrip(t, c, []byte("small request")); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("small stream blocked behind a stalled one")
	}
}

func TestBannerSkipped(t *testing.T) {
	a, b := net.Pipe()
	go func() {
		io.WriteString(b, "Welcome to host\nLast login: yesterday\n")
		s, err := Server(b, Options{})
		if err == nil {
			echo(s)
		}
	}()
	c, err := Client(a, Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if string(c.Banner) != "Welcome to host\nLast login: yesterday\n" {
		t.Errorf("banner %q", c.Banner)
	}
	roundTrip(t, c, []byte("after the banner"))
}

func TestNoMagic(t *testing.T) {
	a, b := net.Pipe()
	go func() {
		io.ReadAll(io.LimitReader(b, int64(len(Magic))))
		io.WriteString(b, "bash: fkbr: command not found\n")
		b.Close()
	}()
	_, err := Client(a, Options{})
	if err == nil || !strings.Contains(err.Error(), "command not found") {
		t.Fatalf("err %v", err)
	}
}

func TestResetReachesThePeer(t *testing.T) {
	c, s := pair(t, Options{})
	go func() {
		st, _ := s.Accept(context.Background())
		buf := make([]byte, 5)
		io.ReadFull(st, buf)
		st.Reset(errors.New("disk full"))
	}()
	st, _ := c.Open()
	st.Write([]byte("hello"))
	_, err := io.ReadAll(st)
	if !errors.Is(err, ErrReset) || !strings.Contains(err.Error(), "disk full") {
		t.Fatalf("read after reset: %v", err)
	}
	select {
	case <-st.Context().Done():
	case <-time.After(time.Second):
		t.Fatal("stream context not done after a reset")
	}
}

func TestSessionEndFailsStreams(t *testing.T) {
	c, s := pair(t, Options{})
	go func() {
		st, _ := s.Accept(context.Background())
		io.ReadFull(st, make([]byte, 1))
		s.Close()
	}()
	st, _ := c.Open()
	st.Write([]byte("x"))
	if _, err := io.ReadAll(st); !errors.Is(err, ErrClosed) {
		t.Fatalf("read: %v", err)
	}
	select {
	case <-c.Done():
	case <-time.After(time.Second):
		t.Fatal("client session not done")
	}
	if _, err := c.Open(); !errors.Is(err, ErrClosed) {
		t.Errorf("open after end: %v", err)
	}
}

// rawPeer does the handshake and then hands the raw connection to the test.
func rawPeer(t *testing.T, opts Options) (*Session, net.Conn) {
	t.Helper()
	a, b := net.Pipe()
	go func() {
		io.ReadFull(b, make([]byte, len(Magic)))
	}()
	go io.WriteString(b, Magic)
	c, err := Client(a, opts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	go io.Copy(io.Discard, b) // pings and the like
	return c, b
}

func TestSilentPeerEndsSession(t *testing.T) {
	c, _ := rawPeer(t, Options{Ping: 20 * time.Millisecond, Dead: 80 * time.Millisecond})
	select {
	case <-c.Done():
		if !strings.Contains(c.Err().Error(), "nothing from the other side") {
			t.Errorf("err %v", c.Err())
		}
	case <-time.After(2 * time.Second):
		t.Fatal("session to a silent peer did not end")
	}
}

func frameBytes(typ byte, id uint32, payload []byte) []byte {
	b := make([]byte, headerLen+len(payload))
	b[0] = typ
	binary.BigEndian.PutUint32(b[1:], id)
	binary.BigEndian.PutUint32(b[5:], uint32(len(payload)))
	copy(b[headerLen:], payload)
	return b
}

func TestPeerPastItsWindow(t *testing.T) {
	c, raw := rawPeer(t, Options{})
	raw.Write(frameBytes(typeOpen, 2, nil))
	st, err := c.Accept(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	chunk := make([]byte, MaxPayload)
	for range Window/MaxPayload + 1 {
		if _, err := raw.Write(frameBytes(typeData, 2, chunk)); err != nil {
			break
		}
	}
	select {
	case <-c.Done():
		if !strings.Contains(c.Err().Error(), "past its window") {
			t.Errorf("err %v", c.Err())
		}
	case <-time.After(2 * time.Second):
		t.Fatal("session survived a peer that ignored the window")
	}
	_ = st
}

func TestBadFrames(t *testing.T) {
	for name, f := range map[string][]byte{
		"oversized data": frameBytes(typeData, 2, make([]byte, MaxPayload+1)),
		"unknown type":   frameBytes(99, 2, nil),
		"own parity":     frameBytes(typeOpen, 1, nil),
		"window size":    frameBytes(typeWindow, 2, []byte{1}),
	} {
		t.Run(name, func(t *testing.T) {
			c, raw := rawPeer(t, Options{})
			go raw.Write(f)
			select {
			case <-c.Done():
			case <-time.After(2 * time.Second):
				t.Fatal("session survived a bad frame")
			}
		})
	}
}

type fuzzConn struct {
	io.Reader
}

func (fuzzConn) Write(p []byte) (int, error) { return len(p), nil }
func (fuzzConn) Close() error                { return nil }

// FuzzSession feeds arbitrary bytes from the peer: no panic, no hang, no
// memory beyond the windows.
func FuzzSession(f *testing.F) {
	f.Add(frameBytes(typeOpen, 2, nil))
	f.Add(append(frameBytes(typeOpen, 2, nil), frameBytes(typeData, 2, []byte("abc"))...))
	f.Add(append(append(frameBytes(typeOpen, 4, nil), frameBytes(typeWindow, 4, []byte{0, 0, 1, 0})...), frameBytes(typeReset, 4, []byte("x"))...))
	f.Add(frameBytes(typePing, 0, nil))
	f.Fuzz(func(t *testing.T, in []byte) {
		s, err := Client(fuzzConn{io.MultiReader(strings.NewReader(Magic), bytes.NewReader(in))}, Options{})
		if err != nil {
			return
		}
		go func() {
			for {
				st, err := s.Accept(context.Background())
				if err != nil {
					return
				}
				go io.Copy(io.Discard, st)
			}
		}()
		select {
		case <-s.Done():
		case <-time.After(5 * time.Second):
			t.Fatal("session did not end at the end of its input")
		}
	})
}
