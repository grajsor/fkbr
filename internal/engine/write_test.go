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

// A hard-linked file is written in place: the other name sees the new
// content, and the save says it was in place. A file reached through a
// symlink is replaced like any other (the link stays a link).
func TestWriteFileInPlace(t *testing.T) {
	share := t.TempDir()
	a, root := node(t, share, "a")
	f := filepath.Join(root, "f")
	os.WriteFile(f, []byte("old"), 0o644)
	if err := os.Link(f, filepath.Join(root, "hard")); err != nil {
		t.Fatal(err)
	}
	g := filepath.Join(root, "g")
	os.WriteFile(g, []byte("old"), 0o644)
	os.Symlink("g", filepath.Join(root, "sym"))
	before := ino(t, f)
	ctx := context.Background()
	res, err := a.Write(ctx, readForWrite(t, root, "hard", "new data"))
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(f); string(got) != "new data" || ino(t, f) != before {
		t.Fatalf("f is %q, inode changed: %v", got, ino(t, f) != before)
	}
	if len(res.Notes) != 1 || !strings.Contains(res.Notes[0], "in place (it has other hard links)") {
		t.Errorf("notes %q", res.Notes)
	}
	gIno := ino(t, g)
	if res, err = a.Write(ctx, readForWrite(t, root, "sym", "via the link")); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(g); string(got) != "via the link" || ino(t, g) == gIno || len(res.Notes) != 0 {
		t.Errorf("g is %q, replaced %v, notes %q", got, ino(t, g) != gIno, res.Notes)
	}
	if fi, _ := os.Lstat(filepath.Join(root, "sym")); fi.Mode()&os.ModeSymlink == 0 {
		t.Error("sym is no longer a symlink")
	}
}

// readForWrite is a WriteRequest for rel as it is now, with new content.
func readForWrite(t *testing.T, root, rel, data string) api.WriteRequest {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(root, rel))
	if err != nil {
		t.Fatal(err)
	}
	return api.WriteRequest{Root: "data", Path: rel, Data: []byte(data), Size: int64(len(b)), Sum: api.ContentSum(b)}
}

func ino(t *testing.T, p string) uint64 {
	t.Helper()
	fi, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	return fi.Sys().(*syscall.Stat_t).Ino
}

// A save leaves setuid/setgid off unless allow_setid, as a copy does,
// whether the file is replaced or written in place (review 2026-10-25:
// an edit kept them, so it could do more than a copy).
func TestWriteDropsSetid(t *testing.T) {
	share := t.TempDir()
	cfg := nodeCfg(t, share, "a")
	a := build(t, cfg)
	root := cfg.Roots[0].Path
	for _, name := range []string{"suid", "linked"} {
		p := filepath.Join(root, name)
		os.WriteFile(p, []byte("#!/bin/sh\n"), 0o755)
		if err := os.Chmod(p, 0o755|os.ModeSetuid|os.ModeSetgid); err != nil {
			t.Fatal(err)
		}
	}
	os.Link(filepath.Join(root, "linked"), filepath.Join(root, "other-name"))
	ctx := context.Background()
	for _, name := range []string{"suid", "linked"} {
		res, err := a.Write(ctx, readForWrite(t, root, name, "#!/bin/sh\necho\n"))
		if err != nil {
			t.Fatal(err)
		}
		fi, _ := os.Stat(filepath.Join(root, name))
		if fi.Mode()&(os.ModeSetuid|os.ModeSetgid) != 0 || fi.Mode().Perm() != 0o755 {
			t.Errorf("%s: mode %v after a save", name, fi.Mode())
		}
		if len(res.Notes) == 0 || !strings.Contains(res.Notes[0], "setuid/setgid removed") {
			t.Errorf("%s: notes %q", name, res.Notes)
		}
	}
	// With allow_setid the bits stay.
	cfg.Transfer.AllowSetID = true
	p := filepath.Join(root, "kept")
	os.WriteFile(p, []byte("x"), 0o755)
	os.Chmod(p, 0o755|os.ModeSetuid)
	if _, err := a.Write(ctx, readForWrite(t, root, "kept", "y")); err != nil {
		t.Fatal(err)
	}
	if fi, _ := os.Stat(p); fi.Mode()&os.ModeSetuid == 0 {
		t.Errorf("allow_setid: mode %v", fi.Mode())
	}
}

// The checks run on where a symlink leads: a link into the staging area
// is refused like the name itself, and its target stays as it was
// (review 2026-10-25).
func TestWriteThroughSymlinkChecked(t *testing.T) {
	share := t.TempDir()
	a, root := node(t, share, "a")
	staged := filepath.Join(root, ".fkbr-tmp", "job", "x")
	os.MkdirAll(filepath.Dir(staged), 0o700)
	os.WriteFile(staged, []byte("staged"), 0o600)
	os.Symlink(".fkbr-tmp/job/x", filepath.Join(root, "notes"))
	if _, err := a.Write(context.Background(), readForWrite(t, root, "notes", "changed")); !errors.Is(err, api.ErrBadPath) {
		t.Fatalf("write through a link into staging: %v", err)
	}
	if got, _ := os.ReadFile(staged); string(got) != "staged" {
		t.Fatalf("staged file is %q", got)
	}
}

// The file must still hold what was read: the same size and mtime with
// other content is caught by the sum (review 2026-10-25: on an NFS root a
// stale stat let another host's change be overwritten).
func TestWriteChecksContent(t *testing.T) {
	share := t.TempDir()
	a, root := node(t, share, "a")
	p := filepath.Join(root, "f")
	os.WriteFile(p, []byte("aaaa"), 0o644)
	req := readForWrite(t, root, "f", "mine")
	fi, _ := os.Stat(p)
	os.WriteFile(p, []byte("bbbb"), 0o644)    // another edit, same length,
	os.Chtimes(p, fi.ModTime(), fi.ModTime()) // in the same mtime tick
	req.ModTime = fi.ModTime()
	if _, err := a.Write(context.Background(), req); !errors.Is(err, api.ErrChanged) {
		t.Fatalf("other content, same size and mtime: %v", err)
	}
	if got, _ := os.ReadFile(p); string(got) != "bbbb" {
		t.Fatalf("overwritten: %q", got)
	}
}

// A save a crash interrupted leaves its new file beside the old one; the
// node removes it when it starts again. Two saves never share a name.
func TestWriteLeftoverSwept(t *testing.T) {
	share := t.TempDir()
	cfg := nodeCfg(t, share, "a")
	a := build(t, cfg)
	root := cfg.Roots[0].Path
	left := filepath.Join(root, ".f.fkbr-edit-0123456789abcdef")
	os.WriteFile(left, []byte("half"), 0o600)
	os.MkdirAll(a.editsDir(), 0o700)
	os.WriteFile(filepath.Join(a.editsDir(), "0123456789abcdef"), []byte(`{"root":"data","path":".f.fkbr-edit-0123456789abcdef"}`), 0o600)
	runWorker(t, a)
	if _, err := os.Lstat(left); !os.IsNotExist(err) {
		t.Fatalf("leftover still there: %v", err)
	}
	if ents, _ := os.ReadDir(a.editsDir()); len(ents) != 0 {
		t.Errorf("records left: %v", ents)
	}
	os.WriteFile(filepath.Join(root, "f"), []byte("x"), 0o644)
	if _, err := a.Write(context.Background(), readForWrite(t, root, "f", "y")); err != nil {
		t.Fatal(err)
	}
	if ents, _ := os.ReadDir(a.editsDir()); len(ents) != 0 {
		t.Errorf("record of a finished save left: %v", ents)
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
