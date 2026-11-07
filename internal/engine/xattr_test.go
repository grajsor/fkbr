package engine

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"fkbr/internal/api"
)

// setUserXattr sets a user.* attribute, or skips the test on a
// filesystem that has none (tmpfs before Linux 6.6, NFS).
func setUserXattr(t *testing.T, path string) {
	t.Helper()
	if err := unix.Setxattr(path, "user.fkbr.test", []byte("v"), 0); err != nil {
		t.Skipf("no user xattrs here: %v", err)
	}
}

func TestHasUncarried(t *testing.T) {
	dir := t.TempDir()
	plain, tagged := filepath.Join(dir, "plain"), filepath.Join(dir, "tagged")
	os.WriteFile(plain, []byte("x"), 0o644)
	os.WriteFile(tagged, []byte("x"), 0o644)
	setUserXattr(t, tagged)
	for path, want := range map[string]bool{plain: false, tagged: true} {
		f, err := os.Open(path)
		if err != nil {
			t.Fatal(err)
		}
		if got := hasUncarried(f); got != want {
			t.Errorf("%s: %v, want %v", filepath.Base(path), got, want)
		}
		f.Close()
	}
}

// A copy says how many files had attributes that were left behind, for
// small files (read ahead by the prefetch workers) and big ones alike.
func TestCopyNotesUncarriedXattrs(t *testing.T) {
	share := t.TempDir()
	a, aroot := node(t, share, "a")
	b, broot := node(t, share, "b")
	runWorker(t, a)
	runWorker(t, b)
	big := make([]byte, prefetchMaxFile+1)
	files := map[string][]byte{"plain": []byte("p"), "small": []byte("s"), "big": big}
	for name, data := range files {
		os.WriteFile(filepath.Join(aroot, name), data, 0o644)
	}
	setUserXattr(t, filepath.Join(aroot, "small"))
	setUserXattr(t, filepath.Join(aroot, "big"))
	id, err := a.Submit(context.Background(), api.TransferRequest{SrcRoot: "data", Paths: []string{"plain", "small", "big"}, DstNodes: []string{"b"}, DstRoot: "data", DstDir: "in"})
	if err != nil {
		t.Fatal(err)
	}
	j := waitJob(t, a, id, 30*time.Second)
	if j.State != api.JobDone {
		t.Fatalf("job: %s", j.State)
	}
	var notes string
	for _, p := range j.Participants {
		if p.Role == api.RoleSource {
			notes = strings.Join(p.Problems, "\n")
		}
	}
	if !strings.Contains(notes, "2 files have ACLs or extended attributes") {
		t.Errorf("source notes: %q", notes)
	}
	for name := range files {
		if _, err := os.Stat(filepath.Join(broot, "in", name)); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
	// a copy of plain files says nothing
	id, err = a.Submit(context.Background(), api.TransferRequest{SrcRoot: "data", Paths: []string{"plain"}, DstNodes: []string{"b"}, DstRoot: "data", DstDir: "in2"})
	if err != nil {
		t.Fatal(err)
	}
	j = waitJob(t, a, id, 30*time.Second)
	for _, p := range j.Participants {
		if strings.Contains(strings.Join(p.Problems, "\n"), "extended attributes") {
			t.Errorf("plain copy notes: %v", p.Problems)
		}
	}
}

// A produce that is retried (a transient chunk-write error) walks the
// source again; the note still counts each file once.
func TestUncarriedNoteCountsOnceAcrossRetry(t *testing.T) {
	shortTransient(t, 10*time.Second)
	share := t.TempDir()
	mcfg := nodeCfg(t, share, "master")
	master, fs := buildFaulty(t, mcfg)
	fs.eioWriteAt = 0 // one transient failure: produce runs twice
	s1, _ := node(t, share, "s1")
	tagged := filepath.Join(mcfg.Roots[0].Path, "tagged")
	if err := os.WriteFile(tagged, []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}
	setUserXattr(t, tagged)
	runWorker(t, master)
	runWorker(t, s1)
	id, err := master.Submit(context.Background(), api.TransferRequest{SrcRoot: "data", Paths: []string{"tagged"}, DstNodes: []string{"s1"}, DstRoot: "data", DstDir: "in"})
	if err != nil {
		t.Fatal(err)
	}
	j := waitJob(t, master, id, 30*time.Second)
	if j.State != api.JobDone {
		t.Fatalf("job: %s %+v", j.State, j.Participants)
	}
	if !fs.eioWriteHit {
		t.Fatal("fault never injected: no retry happened")
	}
	var notes string
	for _, p := range j.Participants {
		if p.Role == api.RoleSource {
			notes = strings.Join(p.Problems, "\n")
		}
	}
	if !strings.Contains(notes, "1 files have ACLs") {
		t.Errorf("source note across the retry: %q", notes)
	}
}
