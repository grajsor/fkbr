package remote

import (
	"context"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// Each time the socket file is removed and listened on again, the old
// listener is closed (without unlinking the new socket): descriptors do
// not pile up, and the new socket still answers.
func TestSocketRecreationClosesOldListener(t *testing.T) {
	oldCheck := SocketCheckInterval
	t.Cleanup(func() { SocketCheckInterval = oldCheck })
	SocketCheckInterval = 10 * time.Millisecond
	share := t.TempDir()
	master, _ := newEngine(t, share, "master")
	dir, _ := os.MkdirTemp("", "fkbr-sock")
	defer os.RemoveAll(dir)
	sock := filepath.Join(dir, "api.sock")
	l, err := Listen(sock, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- ServeSocket(ctx, l, sock, 0o600, master, nil) }()
	defer func() { cancel(); <-done }()

	fds := func() int {
		ents, err := os.ReadDir("/proc/self/fd")
		if err != nil {
			t.Skip("no /proc")
		}
		return len(ents)
	}
	before := fds()
	const rounds = 12
	for i := 0; i < rounds; i++ {
		old := socketInode(sock)
		os.Remove(sock)
		deadline := time.Now().Add(3 * time.Second)
		for {
			if cur := socketInode(sock); cur != 0 && cur != old {
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("round %d: the socket never came back", i)
			}
			time.Sleep(5 * time.Millisecond)
		}
	}
	time.Sleep(100 * time.Millisecond) // let the checks settle: no further recreation
	if grown := fds() - before; grown > 3 {
		t.Errorf("%d recreations left %d more descriptors open", rounds, grown)
	}
	c, err := Dial(context.Background(), sock)
	if err != nil {
		t.Fatalf("the socket answers no more: %v", err)
	}
	c.Close()
	select {
	case err := <-done:
		t.Fatalf("ServeSocket ended: %v", err)
	default:
	}
}

// A refusal with no usable body still says something.
func TestSubscribeErrorHasAMessage(t *testing.T) {
	dir, _ := os.MkdirTemp("", "fkbr-sock")
	defer os.RemoveAll(dir)
	sock := filepath.Join(dir, "s")
	l, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	})}
	go srv.Serve(l)
	defer srv.Close()
	tr := &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		var d net.Dialer
		return d.DialContext(ctx, "unix", sock)
	}}
	c := &Client{http: &http.Client{Transport: tr}}
	_, err = c.Subscribe(context.Background())
	if err == nil || err.Error() == "" {
		t.Fatalf("Subscribe: %v", err)
	}
}
