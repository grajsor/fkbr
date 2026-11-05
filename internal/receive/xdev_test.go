package receive

import (
	"bytes"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"fkbr/internal/api"
)

// treeState is every entry of a tree with what a copy must keep: type,
// mode, size, mtime, content or link target.
func treeState(t *testing.T, dir string) map[string]string {
	t.Helper()
	out := map[string]string{}
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(dir, p)
		fi, err := os.Lstat(p)
		if err != nil {
			return err
		}
		s := fi.Mode().String() + " " + fi.ModTime().UTC().Format("20060102T150405.000000000")
		switch {
		case fi.Mode()&fs.ModeSymlink != 0:
			l, _ := os.Readlink(p)
			s += " -> " + l
		case fi.Mode().IsRegular():
			b, _ := os.ReadFile(p)
			s += " " + string(rune(len(b)%97+33)) + " " + strings.ToValidUTF8(string(b[:min(len(b), 12)]), "?") + " " + string(rune(len(b)))
		}
		out[rel] = s
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// The destination is on another filesystem than the staging (a drive
// mounted below the root): rename says EXDEV. Files and links are copied
// over into the destination's filesystem and installed there, and the
// tree ends exactly as it does with a plain rename (review 2026-11-05: the
// job ended done with an error for every file).
func TestInstallAcrossFilesystems(t *testing.T) {
	for _, existing := range []bool{false, true} {
		name := "new folder"
		if existing {
			name = "existing folder"
		}
		t.Run(name, func(t *testing.T) {
			_, _, src, _ := setup(t)
			s := bundleTree(t, src, "tree", 64<<10)
			run := func(across bool) (string, *Result) {
				dstDir := t.TempDir()
				dst, err := os.OpenRoot(dstDir)
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { dst.Close() })
				if existing {
					os.MkdirAll(filepath.Join(dstDir, "tree/sub"), 0o755)
					os.WriteFile(filepath.Join(dstDir, "tree/a.txt"), []byte("old"), 0o644)
				}
				if across {
					old := renameEntry
					renameEntry = func(r *os.Root, from, to string) error {
						if strings.HasPrefix(from, StagingDir+"/") {
							return &os.LinkError{Op: "rename", Old: from, New: to, Err: syscall.EXDEV}
						}
						return old(r, from, to)
					}
					t.Cleanup(func() { renameEntry = old })
				}
				r, err := New(dst, "job1", ".", Options{Conflict: api.ConflictOverwrite})
				if err != nil {
					t.Fatal(err)
				}
				apply(t, r, s, 0)
				res, err := r.Commit()
				if err != nil {
					t.Fatalf("Commit: %v", err)
				}
				return dstDir, res
			}
			plainDir, plain := run(false)
			acrossDir, across := run(true)
			if plain.Errors != 0 || across.Errors != 0 {
				t.Fatalf("errors: plain %d, across %d: %v", plain.Errors, across.Errors, across.Problems)
			}
			want, got := treeState(t, plainDir), treeState(t, acrossDir)
			delete(want, ".") // the destination's own mtime is whenever
			delete(got, ".")
			for p, w := range want {
				if g, ok := got[p]; !ok {
					t.Errorf("%s did not arrive", p)
				} else if g != w {
					t.Errorf("%s: %q, want %q", p, g, w)
				}
			}
			for p := range got {
				if _, ok := want[p]; !ok {
					t.Errorf("%s: left over in the destination", p)
				}
			}
			if across.Files != plain.Files || across.Symlinks != plain.Symlinks || across.Bytes != plain.Bytes {
				t.Errorf("counted %+v, want %+v", across, plain)
			}
		})
	}
}

// A copy across keeps a sparse file sparse and a file's data exact.
func TestCopyAcrossKeepsHoles(t *testing.T) {
	base := t.TempDir()
	root, err := os.OpenRoot(base)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	data := make([]byte, 8<<20)
	copy(data[4<<20:], "middle")
	os.MkdirAll(filepath.Join(base, StagingDir, "j", "tree"), 0o700)
	os.MkdirAll(filepath.Join(base, "d"), 0o755)
	srcP := filepath.Join(StagingDir, "j", "tree", "f")
	f, _ := root.Create(srcP)
	writeSparse(f, bytes.NewReader(data), 0)
	f.Truncate(int64(len(data)))
	f.Close()
	r := &Receiver{root: root, opts: Options{}, base: filepath.Join(StagingDir, "j")}
	sfi, _ := root.Lstat(srcP)
	if err := r.copyAcross(srcP, "d/f", sfi); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(filepath.Join(base, "d", "f"))
	if !bytes.Equal(got, data) {
		t.Fatal("content differs")
	}
	var st syscall.Stat_t
	syscall.Stat(filepath.Join(base, "d", "f"), &st)
	if used := st.Blocks * 512; used > 1<<20 {
		t.Errorf("8 MiB with 6 bytes of data uses %d bytes on disk", used)
	}
	if _, err := os.Stat(srcP); err == nil {
		if _, err := root.Lstat(srcP); err == nil {
			t.Error("the staged file is still there")
		}
	}
	ents, _ := os.ReadDir(filepath.Join(base, "d"))
	if len(ents) != 1 {
		t.Errorf("%d entries in the destination, want 1 (no temp file)", len(ents))
	}
}
