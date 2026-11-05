package main

import (
	"syscall"
	"testing"
	"time"
)

// A home node cut off by a power cut reads nothing more: its sshd stops
// reading the link's stdout, and no reset ever comes back. Closing the
// link's stdout must end a write stuck on that full pipe, or the link
// never stops and keeps its lock (powercut.py, 2026-11-04: 22 min).
func TestLinkStdoutCloseEndsStuckWrite(t *testing.T) {
	var p [2]int
	if err := syscall.Pipe2(p[:], syscall.O_CLOEXEC); err != nil { // blocking, as sshd's
		t.Fatal(err)
	}
	defer syscall.Close(p[0])
	out, err := pollable(p[1], "stdout")
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		_, err := out.Write(make([]byte, 1<<20)) // more than a pipe holds
		done <- err
	}()
	time.Sleep(200 * time.Millisecond)
	select {
	case err := <-done:
		t.Fatalf("write returned before the close: %v", err)
	default:
	}
	out.Close()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("write into a closed link succeeded")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("write still stuck 5 s after the close")
	}
}

// What the link's fd pointed at is replaced, so nothing else writing to
// that fd number can reach the home node or a file opened later.
func TestLinkStdioLeavesDevNull(t *testing.T) {
	var p [2]int
	if err := syscall.Pipe2(p[:], syscall.O_CLOEXEC); err != nil {
		t.Fatal(err)
	}
	defer syscall.Close(p[0])
	defer syscall.Close(p[1])
	out, err := pollable(p[1], "stdout")
	if err != nil {
		t.Fatal(err)
	}
	defer out.Close()
	if _, err := syscall.Write(p[1], []byte("stray")); err != nil {
		t.Fatal(err)
	}
	if _, err := out.Write([]byte("frame")); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 16)
	n, err := syscall.Read(p[0], buf)
	if err != nil {
		t.Fatal(err)
	}
	if got := string(buf[:n]); got != "frame" {
		t.Fatalf("the pipe got %q, want only the frame", got)
	}
}

// A link whose home is gone gives up a stop that hangs: stuck in stopping
// it keeps its lock, and the host stays out of the swarm until killed.
func TestStopWithinGivesUp(t *testing.T) {
	never := make(chan struct{})
	defer close(never)
	res := make(chan error, 1)
	go func() { res <- stopWithin(func() error { <-never; return nil }, 100*time.Millisecond) }()
	select {
	case err := <-res:
		if err == nil {
			t.Fatal("a stop that hangs reported success")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("stopWithin waited for a stop that hangs")
	}
	if err := stopWithin(func() error { return nil }, time.Second); err != nil {
		t.Fatalf("a quick stop: %v", err)
	}
}
