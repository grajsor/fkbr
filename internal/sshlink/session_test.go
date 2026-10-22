package sshlink

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"fkbr/internal/store"
)

func TestParseHosts(t *testing.T) {
	hs, err := ParseHosts("web01:/srv/www, admin@db01 ,backup:/data/it's,")
	if err != nil {
		t.Fatal(err)
	}
	want := []Host{{"web01", "/srv/www"}, {"admin@db01", ""}, {"backup", "/data/it's"}}
	if len(hs) != len(want) {
		t.Fatalf("%+v", hs)
	}
	for i := range want {
		if hs[i] != want[i] {
			t.Errorf("host %d: %+v, want %+v", i, hs[i], want[i])
		}
	}
	for _, bad := range []string{"", ",", "web01:relative", "-oProxyCommand=x", "a b:/x", ":/x"} {
		if _, err := ParseHosts(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}

// What the remote shell runs keeps every word whole, quotes included.
func TestRemoteCommandQuoting(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "it's a dir")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	run := func(bin string, words ...string) string {
		o := &SessionOptions{Bin: bin}
		o.defaults()
		out, err := exec.Command("sh", "-c", remoteCommand(o, Host{"h", dir}, words...)).Output()
		if err != nil {
			t.Fatal(err)
		}
		return string(out)
	}
	if got := run("printf", "%s|%s", "$(id)", "a'b"); got != "$(id)|a'b" {
		t.Errorf("words: %q", got)
	}
	if got := run("pwd"); got != dir+"\n" {
		t.Errorf("directory: %q", got)
	}
}

// fakeSSH runs the remote command here, after ssh's options, and prints a
// banner first, as a login shell might.
func fakeSSH(t *testing.T) string {
	p := filepath.Join(t.TempDir(), "ssh")
	os.WriteFile(p, []byte(`#!/bin/sh
while [ $# -gt 0 ]; do
	case "$1" in
	--) shift; break ;;
	-o) shift 2 ;;
	*) shift ;;
	esac
done
echo "Welcome to $1"
exec sh -c "$2"
`), 0o755)
	return p
}

func TestProbe(t *testing.T) {
	ssh := fakeSSH(t)
	v, err := Probe(context.Background(), Host{"h", t.TempDir()}, SessionOptions{SSH: []string{ssh}, Bin: "echo"})
	if err != nil || v != "version" {
		t.Errorf("probe: %q, %v", v, err)
	}
	_, err = Probe(context.Background(), Host{"h", ""}, SessionOptions{SSH: []string{ssh}, Bin: "fkbr-not-installed"})
	if err == nil || !strings.Contains(err.Error(), "not found") {
		t.Errorf("missing binary: %v", err)
	}
}

// lingeringSSH is fakeSSH that leaves a process behind holding its stdout
// and stderr open, as a ControlPersist master forked by ssh does.
func lingeringSSH(t *testing.T) string {
	p := filepath.Join(t.TempDir(), "ssh")
	os.WriteFile(p, []byte(`#!/bin/sh
while [ $# -gt 0 ]; do
	case "$1" in
	--) shift; break ;;
	-o) shift 2 ;;
	*) shift ;;
	esac
done
sleep 30 &
exec sh -c "$2"
`), 0o755)
	return p
}

// A session or probe ends when ssh does, even if something it left behind
// keeps its output open.
func TestLingeringSSHDoesNotHang(t *testing.T) {
	ssh := lingeringSSH(t)
	done := make(chan struct{})
	go func() {
		defer close(done)
		if v, err := Probe(context.Background(), Host{"h", ""}, SessionOptions{SSH: []string{ssh}, Bin: "echo"}); err != nil || v != "version" {
			t.Errorf("probe: %q, %v", v, err)
		}
		h := NewHome(t.TempDir(), "home", store.Options{}, nil)
		o := SessionOptions{SSH: []string{ssh}, Bin: "fkbr-not-installed"}
		o.defaults()
		if err := h.session(context.Background(), Host{"h", ""}, &o); err == nil {
			t.Error("session to a host without fkbr: no error")
		}
	}()
	select {
	case <-done:
	case <-time.After(20 * time.Second):
		t.Fatal("hangs on ssh's output held open after it exited")
	}
}
