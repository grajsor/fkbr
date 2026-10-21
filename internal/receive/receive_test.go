package receive

import (
	"bytes"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"fkbr/internal/api"
	"fkbr/internal/bundle"
	"fkbr/internal/config"
	"fkbr/internal/localfs"
)

type memSink struct {
	chunks [][]byte
	metas  []bundle.ChunkMeta
	cur    *bytes.Buffer
}

type nopCloser struct{ *bytes.Buffer }

func (nopCloser) Close() error { return nil }

func (s *memSink) Create(int) (io.WriteCloser, error) {
	s.cur = &bytes.Buffer{}
	return nopCloser{s.cur}, nil
}
func (s *memSink) Commit(m bundle.ChunkMeta) error {
	s.chunks, s.metas = append(s.chunks, s.cur.Bytes()), append(s.metas, m)
	return nil
}

// bundleTree bundles everything under src (a localfs root) with paths
// prefixed by the base name of rel, as the engine's sender does.
func bundleTree(t *testing.T, src *localfs.Local, rel string, chunk int64) *memSink {
	t.Helper()
	s := &memSink{}
	w := bundle.NewWriter(s, chunk, true)
	err := src.Walk("src", rel, func(p string, e api.Entry, err error) error {
		if err != nil {
			return err
		}
		name := filepath.Join(filepath.Base(rel), strings.TrimPrefix(strings.TrimPrefix(p, rel), "/"))
		return w.Add(bundle.Item{Name: name, Entry: e, Open: func() (bundle.File, error) { return src.Open("src", p) }})
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return s
}

func apply(t *testing.T, r *Receiver, s *memSink, from int) {
	t.Helper()
	for i := from; i < len(s.chunks); i++ {
		if err := bundle.Read(bytes.NewReader(s.chunks[i]), s.metas[i].Compressed, r.Apply); err != nil {
			t.Fatal(err)
		}
	}
}

func setup(t *testing.T) (srcDir, dstDir string, src *localfs.Local, dst *os.Root) {
	t.Helper()
	base := t.TempDir()
	srcDir, dstDir = filepath.Join(base, "src"), filepath.Join(base, "dst")
	mk := func(p string, data string, mode os.FileMode) {
		full := filepath.Join(srcDir, p)
		os.MkdirAll(filepath.Dir(full), 0o755)
		if err := os.WriteFile(full, []byte(data), mode); err != nil {
			t.Fatal(err)
		}
	}
	mk("tree/a.txt", "alpha", 0o640)
	mk("tree/sub/big.bin", strings.Repeat("x", 300<<10), 0o600)
	mk("tree/sub/run.sh", "#!/bin/sh\n", 0o4755)
	os.Symlink("../a.txt", filepath.Join(srcDir, "tree/sub/link"))
	os.Chmod(filepath.Join(srcDir, "tree/sub"), 0o750)
	old := time.Unix(1600000000, 5)
	os.Chtimes(filepath.Join(srcDir, "tree/a.txt"), old, old)
	os.Chtimes(filepath.Join(srcDir, "tree/sub"), old, old)
	os.Mkdir(dstDir, 0o755)
	var err error
	if src, err = localfs.Open([]config.Root{{ID: "src", Path: srcDir}}); err != nil {
		t.Fatal(err)
	}
	if dst, err = os.OpenRoot(dstDir); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { src.Close(); dst.Close() })
	return
}

func TestRoundTripMetadata(t *testing.T) {
	srcDir, dstDir, src, dst := setup(t)
	s := bundleTree(t, src, "tree", 64<<10)
	r, err := New(dst, "job1", "in/here", Options{Conflict: api.ConflictSkip})
	if err != nil {
		t.Fatal(err)
	}
	apply(t, r, s, 0)
	res, err := r.Commit()
	if err != nil {
		t.Fatal(err)
	}
	if res.Files != 3 || res.Symlinks != 1 || res.Dirs != 2 || res.Errors != 0 {
		t.Fatalf("result: %+v", res)
	}
	got := filepath.Join(dstDir, "in/here/tree")
	b, _ := os.ReadFile(filepath.Join(got, "sub/big.bin"))
	if len(b) != 300<<10 {
		t.Fatalf("big.bin: %d bytes", len(b))
	}
	fi, _ := os.Stat(filepath.Join(got, "a.txt"))
	sfi, _ := os.Stat(filepath.Join(srcDir, "tree/a.txt"))
	if fi.Mode().Perm() != 0o640 || !fi.ModTime().Equal(sfi.ModTime()) {
		t.Errorf("a.txt: mode %v mtime %v want %v", fi.Mode(), fi.ModTime(), sfi.ModTime())
	}
	fi, _ = os.Stat(filepath.Join(got, "sub"))
	if fi.Mode().Perm() != 0o750 || fi.ModTime().Unix() != 1600000000 {
		t.Errorf("sub dir: mode %v mtime %v", fi.Mode(), fi.ModTime())
	}
	fi, _ = os.Stat(filepath.Join(got, "sub/run.sh"))
	if fi.Mode()&fs.ModeSetuid != 0 || fi.Mode().Perm() != 0o755 {
		t.Errorf("setuid must be stripped by default: %v", fi.Mode())
	}
	if l, _ := os.Readlink(filepath.Join(got, "sub/link")); l != "../a.txt" {
		t.Errorf("link -> %q", l)
	}
	if _, err := os.Stat(filepath.Join(dstDir, StagingDir)); !os.IsNotExist(err) {
		t.Errorf("staging not cleaned up: %v", err)
	}
}

func TestConflictPolicies(t *testing.T) {
	_, dstDir, src, dst := setup(t)
	target := filepath.Join(dstDir, "tree/a.txt")
	os.MkdirAll(filepath.Dir(target), 0o755)
	cases := []struct {
		policy  api.ConflictPolicy
		destAge time.Time
		want    string
	}{
		{api.ConflictSkip, time.Unix(1, 0), "existing"},
		{api.ConflictOverwriteOlder, time.Unix(1, 0), "alpha"},             // dest older -> replaced
		{api.ConflictOverwriteOlder, time.Unix(2000000000, 0), "existing"}, // dest newer -> kept
		{api.ConflictOverwrite, time.Unix(2000000000, 0), "alpha"},
	}
	for i, c := range cases {
		os.WriteFile(target, []byte("existing"), 0o644)
		os.Chtimes(target, c.destAge, c.destAge)
		s := bundleTree(t, src, "tree", 1<<20)
		r, err := New(dst, "job"+string(rune('a'+i)), ".", Options{Conflict: c.policy})
		if err != nil {
			t.Fatal(err)
		}
		apply(t, r, s, 0)
		if _, err := r.Commit(); err != nil {
			t.Fatal(err)
		}
		if b, _ := os.ReadFile(target); string(b) != c.want {
			t.Errorf("%s (dest mtime %d): got %q want %q", c.policy, c.destAge.Unix(), b, c.want)
		}
	}
}

func TestResumeAndBad(t *testing.T) {
	_, dstDir, src, dst := setup(t)
	s := bundleTree(t, src, "tree", 64<<10)
	r, _ := New(dst, "job", ".", Options{Conflict: api.ConflictSkip})
	apply(t, r, s, 0)
	// Crash before commit: a new receiver re-applies the last chunks.
	r2, err := New(dst, "job", ".", Options{Conflict: api.ConflictSkip})
	if err != nil {
		t.Fatal(err)
	}
	if err := r2.MarkBad([]string{"tree/sub/big.bin"}); err != nil {
		t.Fatal(err)
	}
	apply(t, r2, s, len(s.chunks)-2)
	res, err := r2.Commit()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dstDir, "tree/sub/big.bin")); !os.IsNotExist(err) {
		t.Error("bad file must not be installed")
	}
	if res.Files != 2 {
		t.Errorf("files = %d", res.Files)
	}
}

func TestRejectsStagingAsDest(t *testing.T) {
	_, _, _, dst := setup(t)
	if _, err := New(dst, "j", StagingDir+"/x", Options{Conflict: api.ConflictSkip}); err == nil {
		t.Fatal("expected error")
	}
	if _, err := New(dst, "j", "../up", Options{Conflict: api.ConflictSkip}); err == nil {
		t.Fatal("expected error")
	}
}

// An entry that cannot be installed (here: every rename into sub/ fails)
// is an error for that entry; the rest is installed and the staging area
// is removed.
func TestInstallContinuesPastFailedEntries(t *testing.T) {
	_, dstDir, src, dst := setup(t)
	os.MkdirAll(filepath.Join(dstDir, "tree"), 0o755) // so entries install one by one
	orig := renameEntry
	renameEntry = func(r *os.Root, from, to string) error {
		if to == "tree/sub" || strings.HasPrefix(to, "tree/sub/") {
			return &os.LinkError{Op: "renameat", Old: from, New: to, Err: syscall.EIO}
		}
		return orig(r, from, to)
	}
	t.Cleanup(func() { renameEntry = orig })
	s := bundleTree(t, src, "tree", 64<<10)
	r, err := New(dst, "job1", ".", Options{Conflict: api.ConflictSkip})
	if err != nil {
		t.Fatal(err)
	}
	apply(t, r, s, 0)
	res, err := r.Commit()
	if err != nil {
		t.Fatalf("Commit: %v", err)
	}
	if res.Files != 1 || res.Errors != 3 {
		t.Fatalf("want a.txt installed and 3 errors in sub/: %+v", res)
	}
	if b, _ := os.ReadFile(filepath.Join(dstDir, "tree/a.txt")); string(b) != "alpha" {
		t.Errorf("a.txt: %q", b)
	}
	if _, err := os.Stat(filepath.Join(dstDir, StagingDir)); !os.IsNotExist(err) {
		t.Errorf("staging not cleaned up: %v", err)
	}
}

// A directory that arrived read-only (as on the source) does not stop a
// later copy into it: it is made writable for the moment, then restored.
func TestInstallIntoReadOnlyDir(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permissions")
	}
	_, dstDir, src, dst := setup(t)
	ro := filepath.Join(dstDir, "tree/sub")
	os.MkdirAll(ro, 0o755)
	os.WriteFile(filepath.Join(ro, "big.bin"), []byte("old"), 0o644)
	os.Chmod(ro, 0o555)
	t.Cleanup(func() { os.Chmod(ro, 0o755) })
	s := bundleTree(t, src, "tree", 64<<10)
	r, err := New(dst, "job1", ".", Options{Conflict: api.ConflictOverwrite})
	if err != nil {
		t.Fatal(err)
	}
	apply(t, r, s, 0)
	res, err := r.Commit()
	if err != nil || res.Errors != 0 {
		t.Fatalf("Commit: %+v %v", res, err)
	}
	if b, _ := os.ReadFile(filepath.Join(ro, "big.bin")); len(b) != 300<<10 {
		t.Errorf("big.bin not replaced: %d bytes", len(b))
	}
	// overwrite also applies the source's directory modes (0750 here).
	if fi, _ := os.Stat(ro); fi.Mode().Perm() != 0o750 {
		t.Errorf("mode: %v", fi.Mode())
	}
}

// A sparse file stays sparse, and a replayed segment over existing data
// still writes its zeros.
func TestSparseFiles(t *testing.T) {
	f, err := os.Create(filepath.Join(t.TempDir(), "s"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	data := make([]byte, 8<<20)
	copy(data[4<<20:], "middle")
	if n, err := writeSparse(f, bytes.NewReader(data), 0); err != nil || n != int64(len(data)) {
		t.Fatalf("writeSparse: %d %v", n, err)
	}
	f.Truncate(int64(len(data)))
	var st syscall.Stat_t
	syscall.Fstat(int(f.Fd()), &st)
	if used := st.Blocks * 512; used > 1<<20 {
		t.Errorf("8 MiB with 6 bytes of data uses %d bytes on disk", used)
	}
	got, _ := os.ReadFile(f.Name())
	if !bytes.Equal(got, data) {
		t.Fatal("content differs")
	}
	// Replay over a file that has data where the chunk has zeros.
	f.Seek(0, io.SeekStart)
	f.Write(bytes.Repeat([]byte{'x'}, 1<<20))
	f.Seek(0, io.SeekStart)
	writeSparse(f, bytes.NewReader(data), 0)
	if got, _ := os.ReadFile(f.Name()); !bytes.Equal(got, data) {
		t.Fatal("replay left old data in a zero range")
	}
}

// A directory that does not exist at the destination moves in with one
// rename; the result still counts what it holds.
func TestNewDirMovesWhole(t *testing.T) {
	_, dstDir, src, dst := setup(t)
	s := bundleTree(t, src, "tree", 64<<10)
	r, err := New(dst, "job1", "new", Options{Conflict: api.ConflictSkip})
	if err != nil {
		t.Fatal(err)
	}
	apply(t, r, s, 0)
	var renames int
	orig := renameEntry
	renameEntry = func(root *os.Root, from, to string) error { renames++; return orig(root, from, to) }
	t.Cleanup(func() { renameEntry = orig })
	res, err := r.Commit()
	if err != nil {
		t.Fatal(err)
	}
	if renames != 1 || res.Files != 3 || res.Dirs != 2 || res.Symlinks != 1 || res.Bytes != 5+(300<<10)+10 {
		t.Fatalf("renames %d, result %+v", renames, res)
	}
	if fi, err := os.Stat(filepath.Join(dstDir, "new/tree/sub")); err != nil || fi.Mode().Perm() != 0o750 {
		t.Fatalf("sub: %v %v", fi, err)
	}
}

// Nothing is written into the share when it sits inside the destination
// root (Hide): those entries are errors, the rest installs.
func TestHiddenShareNotWritten(t *testing.T) {
	_, dstDir, src, dst := setup(t)
	if err := os.MkdirAll(filepath.Join(dstDir, "tree", "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := New(dst, "j0", "tree/sub/in", Options{Conflict: api.ConflictSkip, Hide: "tree/sub"}); err == nil {
		t.Error("receive into the share accepted")
	}
	s := bundleTree(t, src, "tree", 64<<10)
	r, err := New(dst, "job1", ".", Options{Conflict: api.ConflictOverwrite, Hide: "tree/sub"})
	if err != nil {
		t.Fatal(err)
	}
	apply(t, r, s, 0)
	res, err := r.Commit()
	if err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(filepath.Join(dstDir, "tree/a.txt")); string(b) != "alpha" {
		t.Errorf("a.txt: %q", b)
	}
	if des, _ := os.ReadDir(filepath.Join(dstDir, "tree/sub")); len(des) != 0 || res.Errors == 0 {
		t.Errorf("share written: %d entries, %d errors", len(des), res.Errors)
	}
}

// A name the destination filesystem cannot hold (FAT refuses ':' with
// EINVAL) is an error for that entry, its later segments included; the
// rest arrives.
func TestUnplaceableNameRefused(t *testing.T) {
	_, dstDir, src, dst := setup(t)
	orig := openStaged
	openStaged = func(d *os.Root, name string, flag int, perm fs.FileMode) (*os.File, error) {
		if name == "big.bin" {
			return nil, &fs.PathError{Op: "openat", Path: name, Err: syscall.EINVAL}
		}
		return orig(d, name, flag, perm)
	}
	t.Cleanup(func() { openStaged = orig })
	s := bundleTree(t, src, "tree", 64<<10) // big.bin spans several chunks
	r, err := New(dst, "job1", ".", Options{Conflict: api.ConflictSkip})
	if err != nil {
		t.Fatal(err)
	}
	apply(t, r, s, 0)
	res, err := r.Commit()
	if err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(filepath.Join(dstDir, "tree/a.txt")); string(b) != "alpha" {
		t.Errorf("a.txt: %q", b)
	}
	if _, err := os.Stat(filepath.Join(dstDir, "tree/sub/big.bin")); !os.IsNotExist(err) {
		t.Errorf("big.bin: %v", err)
	}
	if res.Errors != 1 || len(res.Problems) != 1 || !strings.Contains(res.Problems[0], "big.bin") {
		t.Errorf("want one error for big.bin: %+v", res)
	}
}

// An mtime the filesystem cannot store (here 1900: ext4 stops at
// 1901-12-13, XFS without bigtime at 1901 and 2038) is clamped by the
// kernel without an error; the commit says so.
func TestMtimeNotStoredNoted(t *testing.T) {
	_, dstDir, _, dst := setup(t)
	if lo, hi := localfs.MtimeRange(dstDir); lo == 0 && hi == 0 {
		t.Skip("this filesystem stores 1900 and 2100")
	}
	r, err := New(dst, "job1", ".", Options{Conflict: api.ConflictSkip})
	if err != nil {
		t.Fatal(err)
	}
	old := time.Date(1900, 1, 1, 0, 0, 0, 0, time.UTC).UnixNano()
	now := time.Now().UnixNano()
	for name, m := range map[string]int64{"old": old, "new": now} {
		h := &bundle.Header{Name: name, Entry: api.Entry{Type: api.TypeFile, Size: 1, Mode: 0o644, MtimeNs: m, UID: uint32(os.Getuid()), GID: uint32(os.Getgid())}, Length: 1}
		if err := r.Apply(h, strings.NewReader("x")); err != nil {
			t.Fatal(err)
		}
	}
	res, err := r.Commit()
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Problems) != 1 || !strings.Contains(res.Problems[0], "mtime not stored on 1 files") || !strings.Contains(res.Problems[0], "old: 1900-01-01") {
		t.Errorf("want one note about old: %q", res.Problems)
	}
}

// FAT and exFAT store "dir." as "dir" (and vfat "f." as "f") without an
// error: such entries are refused, not merged with a real "dir".
func TestTrailingDotStrippedRefused(t *testing.T) {
	strip := func(name string) string { return strings.TrimRight(name, ". ") }
	origMk, origOpen := mkdirStaged, openStaged
	mkdirStaged = func(r *os.Root, name string, perm fs.FileMode) error {
		return origMk(r, path.Join(path.Dir(name), strip(path.Base(name))), perm)
	}
	openStaged = func(d *os.Root, name string, flag int, perm fs.FileMode) (*os.File, error) {
		return origOpen(d, strip(name), flag, perm)
	}
	t.Cleanup(func() { mkdirStaged, openStaged = origMk, origOpen })
	_, dstDir, _, dst := setup(t)
	r, err := New(dst, "job1", ".", Options{Conflict: api.ConflictSkip})
	if err != nil {
		t.Fatal(err)
	}
	me := func(typ api.EntryType, size int64) api.Entry {
		return api.Entry{Type: typ, Size: size, Mode: 0o755, MtimeNs: 1e18, UID: uint32(os.Getuid()), GID: uint32(os.Getgid())}
	}
	for _, it := range []struct {
		name string
		typ  api.EntryType
	}{{"dir.", api.TypeDir}, {"dir./in", api.TypeFile}, {"f.", api.TypeFile}, {"ok", api.TypeFile}} {
		h := &bundle.Header{Name: it.name, Entry: me(it.typ, 0)}
		if it.typ == api.TypeFile {
			h.Entry.Size, h.Length = 1, 1
		}
		if err := r.Apply(h, strings.NewReader("x")); err != nil {
			t.Fatalf("%s: %v", it.name, err)
		}
	}
	res, err := r.Commit()
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{"dir", "f"} {
		if _, err := os.Lstat(filepath.Join(dstDir, p)); !os.IsNotExist(err) {
			t.Errorf("%s: installed under the altered name (%v)", p, err)
		}
	}
	if _, err := os.Stat(filepath.Join(dstDir, "ok")); err != nil {
		t.Errorf("ok: %v", err)
	}
	if res.Errors != 2 {
		t.Errorf("want dir. and f. refused: %+v", res)
	}
}

// exFAT answers EPERM when asked to remove a name it cannot hold (one that
// is not valid UTF-8): refusing it must not fail the whole job.
func TestRefusedNameRemovalFails(t *testing.T) {
	origOpen, origRm := openStaged, removeStaged
	openStaged = func(d *os.Root, name string, flag int, perm fs.FileMode) (*os.File, error) {
		if strings.HasPrefix(name, "bad") { // what exFAT really answers
			return nil, &fs.PathError{Op: "openat", Path: name, Err: syscall.EPERM}
		}
		return origOpen(d, name, flag, perm)
	}
	removeStaged = func(r *os.Root, name string) error {
		if strings.Contains(name, "bad") {
			return &fs.PathError{Op: "removeat", Path: name, Err: syscall.EPERM}
		}
		return origRm(r, name)
	}
	t.Cleanup(func() { openStaged, removeStaged = origOpen, origRm })
	_, dstDir, _, dst := setup(t)
	r, err := New(dst, "job1", ".", Options{Conflict: api.ConflictSkip})
	if err != nil {
		t.Fatal(err)
	}
	for _, n := range []string{"good1", "bad\xe9", "good2"} {
		h := &bundle.Header{Name: n, Entry: api.Entry{Type: api.TypeFile, Size: 1, Mode: 0o644, MtimeNs: 1e18}, Length: 1}
		if err := r.Apply(h, strings.NewReader("x")); err != nil {
			t.Fatalf("%q: %v", n, err)
		}
	}
	res, err := r.Commit()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dstDir, "good2")); err != nil || res.Errors != 1 {
		t.Errorf("good2: %v; %+v", err, res)
	}
}
