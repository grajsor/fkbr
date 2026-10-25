package engine

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"fkbr/internal/api"
)

// The TUI's editor saves through Write: on another node, only over the
// file as it was read, keeping its mode and links.
func TestWriteFile(t *testing.T) {
	share := t.TempDir()
	a, _ := node(t, share, "a")
	b, broot := node(t, share, "b")
	f := filepath.Join(broot, "f.txt")
	if err := os.WriteFile(f, []byte("old\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	os.Chmod(f, 0o640) // whatever the umask
	runWorker(t, a)
	runWorker(t, b)
	ctx := context.Background()

	read, err := a.Read(ctx, api.ReadRequest{Node: "b", Root: "data", Path: "f.txt"})
	if err != nil || read.ModTime.IsZero() {
		t.Fatalf("read: %+v %v", read, err)
	}
	req := api.WriteRequest{Node: "b", Root: "data", Path: "f.txt", Data: []byte("new content\n"), Size: read.Size, ModTime: read.ModTime}
	res, err := a.Write(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(f); string(got) != "new content\n" || res.Size != 12 {
		t.Fatalf("content %q, result %+v", got, res)
	}
	if fi, _ := os.Stat(f); fi.Mode().Perm() != 0o640 {
		t.Errorf("mode %v, want 0640", fi.Mode().Perm())
	}
	if ents, _ := os.ReadDir(broot); len(ents) != 1 {
		t.Errorf("left beside it: %v", ents)
	}

	// The same request again: the file is not as it was read any more.
	if _, err := a.Write(ctx, req); !errors.Is(err, api.ErrChanged) {
		t.Fatalf("stale write: %v", err)
	}
	if got, _ := os.ReadFile(f); string(got) != "new content\n" {
		t.Fatalf("stale write changed the file: %q", got)
	}
	// With what the first write returned it goes through.
	req.Data, req.Size, req.ModTime = []byte("third\n"), res.Size, res.ModTime
	if _, err := a.Write(ctx, req); err != nil {
		t.Fatalf("write after write: %v", err)
	}

	for _, c := range []struct {
		name string
		req  api.WriteRequest
		want error
	}{
		{"root itself", api.WriteRequest{Path: "."}, api.ErrBadPath},
		{"staging", api.WriteRequest{Path: ".fkbr-tmp/x"}, api.ErrBadPath},
		{"escape", api.WriteRequest{Path: "../x"}, api.ErrBadPath},
		{"too big", api.WriteRequest{Path: "f.txt", Data: make([]byte, api.MaxWriteSize+1)}, api.ErrNotAllowed},
		{"unknown root", api.WriteRequest{Root: "nope", Path: "f.txt"}, api.ErrUnknownRoot},
	} {
		c.req.Node = "b"
		if c.req.Root == "" {
			c.req.Root = "data"
		}
		if _, err := a.Write(ctx, c.req); !errors.Is(err, c.want) {
			t.Errorf("%s: %v, want %v", c.name, err, c.want)
		}
	}
	// Write never creates a file.
	if _, err := a.Write(ctx, api.WriteRequest{Node: "b", Root: "data", Path: "new.txt", Data: []byte("x")}); err == nil {
		t.Error("a missing file was written")
	}
	if _, err := os.Lstat(filepath.Join(broot, "new.txt")); err == nil {
		t.Error("new.txt created")
	}
}

// A hard-linked file or one reached through a symlink is written in
// place: the other name sees the new content, the link stays a link.
func TestWriteFileInPlace(t *testing.T) {
	share := t.TempDir()
	a, root := node(t, share, "a")
	f := filepath.Join(root, "f")
	os.WriteFile(f, []byte("old"), 0o644)
	if err := os.Link(f, filepath.Join(root, "hard")); err != nil {
		t.Fatal(err)
	}
	os.Symlink("f", filepath.Join(root, "sym"))
	ino := func(p string) uint64 {
		fi, err := os.Stat(p)
		if err != nil {
			t.Fatal(err)
		}
		return fi.Sys().(*syscall.Stat_t).Ino
	}
	before := ino(f)
	ctx := context.Background()
	for i, p := range []string{"hard", "sym"} {
		fi, _ := os.Stat(f)
		data := strings.Repeat("new", i+2)
		if _, err := a.Write(ctx, api.WriteRequest{Root: "data", Path: p, Data: []byte(data), Size: fi.Size(), ModTime: fi.ModTime()}); err != nil {
			t.Fatalf("%s: %v", p, err)
		}
		if got, _ := os.ReadFile(f); string(got) != data {
			t.Fatalf("%s: f is %q", p, got)
		}
	}
	if ino(f) != before || ino(filepath.Join(root, "hard")) != before {
		t.Error("the file was replaced, not written in place")
	}
	if fi, _ := os.Lstat(filepath.Join(root, "sym")); fi.Mode()&os.ModeSymlink == 0 {
		t.Error("sym is no longer a symlink")
	}
}

func TestWriteFileRefused(t *testing.T) {
	share := t.TempDir()
	a, _ := node(t, share, "a")
	rc := nodeCfg(t, share, "ro")
	rc.Roots[0].AllowWrite = false
	ro := build(t, rc)
	pc := nodeCfg(t, share, "picky")
	pc.AcceptWritesFrom = []string{"someone-else"}
	picky := build(t, pc)
	for _, cfg := range []string{rc.Roots[0].Path, pc.Roots[0].Path} {
		os.WriteFile(filepath.Join(cfg, "f"), []byte("x"), 0o644)
	}
	for _, e := range []*Engine{a, ro, picky} {
		runWorker(t, e)
	}
	ctx := context.Background()
	req := api.WriteRequest{Root: "data", Path: "f", Data: []byte("y"), Size: 1}
	req.Node = "ro"
	if _, err := a.Write(ctx, req); !errors.Is(err, api.ErrReadOnly) {
		t.Errorf("read-only root: %v", err)
	}
	req.Node = "picky"
	if _, err := a.Write(ctx, req); !errors.Is(err, api.ErrNotAllowed) {
		t.Errorf("accept_writes_from: %v", err)
	}
	for _, cfg := range []string{rc.Roots[0].Path, pc.Roots[0].Path} {
		if got, _ := os.ReadFile(filepath.Join(cfg, "f")); string(got) != "x" {
			t.Errorf("%s written: %q", cfg, got)
		}
	}
}
