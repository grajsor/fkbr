package engine

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"fkbr/internal/api"
	"fkbr/internal/product"
)

// writeLocal replaces an existing file's content, like an editor saving
// it (api.Write; the TUI's Ctrl+E).
//
//   - The file is opened once, following symlinks inside the root, and the
//     checks run on where it really is: a link into the staging area or
//     the share is refused like the name itself.
//   - It must be as it was read: same size and, through that open file,
//     the same content (Sum); without a Sum, the same mtime.
//   - setuid/setgid are removed unless allow_setid, as a copy does.
//   - A file with one link that we own (or may give back, as root) gets a
//     new file beside it, renamed over it once written and synced: a full
//     disk or a crash leaves the old content. Others (hard links, someone
//     else's file, a folder we may not write) are written in place, which
//     a crash can leave half old, half new; the result says so.
func (e *Engine) writeLocal(req api.WriteRequest) (*api.WriteResult, error) {
	p, err := e.writablePath(req.Root, req.Path)
	if err != nil {
		return nil, err
	}
	root, err := e.fs.WritableRoot(req.Root)
	if err != nil {
		return nil, err
	}
	f, err := root.OpenFile(p, os.O_RDWR, 0)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !fi.Mode().IsRegular() {
		return nil, fmt.Errorf("%w: %s is not a regular file", api.ErrBadPath, p)
	}
	real, err := e.realRel(req.Root, f)
	if err != nil {
		return nil, err
	}
	if real != p {
		if _, err := e.writablePath(req.Root, real); err != nil {
			return nil, fmt.Errorf("%s leads to %s: %w", p, real, err)
		}
	}
	if err := unchanged(f, fi, req); err != nil {
		return nil, fmt.Errorf("%w: %s", err, p)
	}
	keep := fs.ModePerm | fs.ModeSticky
	var notes []string
	if e.cfg.Transfer.AllowSetID {
		keep |= fs.ModeSetuid | fs.ModeSetgid
	} else if fi.Mode()&(fs.ModeSetuid|fs.ModeSetgid) != 0 {
		notes = append(notes, "setuid/setgid removed (allow_setid is off)")
	}

	why := inPlaceReason(fi)
	if why == "" {
		err := e.replaceFile(req.Root, root, real, fi, fi.Mode()&keep, req.Data)
		switch {
		case err == nil:
			res, err := written(root, real)
			if res != nil {
				res.Notes = notes
			}
			return res, err
		case errors.Is(err, fs.ErrPermission):
			why = "no new file may be made in its folder"
		case errors.Is(err, syscall.ENAMETOOLONG):
			why = "its name is too long for a new file beside it"
		default:
			return nil, err
		}
	}
	if n := int64(len(req.Data)); n > fi.Size() {
		if err := reserve(f, n); err != nil {
			return nil, err
		}
	}
	if _, err := f.WriteAt(req.Data, 0); err != nil {
		return nil, err
	}
	if err := f.Truncate(int64(len(req.Data))); err != nil {
		return nil, err
	}
	// The kernel clears setuid/setgid on a write only for a writer without
	// CAP_FSETID; as root they would stay.
	if now, err := f.Stat(); err == nil && now.Mode()&^keep&(fs.ModeSetuid|fs.ModeSetgid) != 0 {
		if err := f.Chmod(now.Mode() & keep); err != nil {
			return nil, err
		}
	}
	if err := f.Sync(); err != nil {
		return nil, err
	}
	if err := f.Close(); err != nil {
		return nil, err
	}
	res, err := written(root, real)
	if res != nil {
		res.Notes = append(notes, "written in place ("+why+"): a crash while saving could have left it half old, half new")
	}
	return res, err
}

// realRel is where the open file f really is, relative to root rootID:
// its path with every symlink followed.
func (e *Engine) realRel(rootID string, f *os.File) (string, error) {
	target, err := os.Readlink(fmt.Sprintf("/proc/self/fd/%d", f.Fd()))
	if err != nil {
		return "", fmt.Errorf("cannot tell where the file is: %w", err)
	}
	for _, r := range e.fs.Roots() {
		if r.ID != rootID {
			continue
		}
		base, err := filepath.EvalSymlinks(r.Path)
		if err != nil {
			return "", err
		}
		rel, err := filepath.Rel(base, target)
		if err != nil || rel == ".." || strings.HasPrefix(rel, "../") {
			return "", fmt.Errorf("%w: %s is outside the root", api.ErrBadPath, target)
		}
		return filepath.ToSlash(rel), nil
	}
	return "", fmt.Errorf("%w: %q", api.ErrUnknownRoot, rootID)
}

// unchanged checks that the open file f is as req says it was read.
func unchanged(f *os.File, fi fs.FileInfo, req api.WriteRequest) error {
	if fi.Size() != req.Size {
		return fmt.Errorf("%w (now %d bytes, was %d)", api.ErrChanged, fi.Size(), req.Size)
	}
	if req.Sum == "" {
		if !req.ModTime.IsZero() && !fi.ModTime().Equal(req.ModTime) {
			return fmt.Errorf("%w (modified %s)", api.ErrChanged, fi.ModTime().Format(time.DateTime))
		}
		return nil
	}
	if fi.Size() > api.MaxReadLimit {
		return fmt.Errorf("%w: %d bytes is too big to edit", api.ErrNotAllowed, fi.Size())
	}
	buf := make([]byte, fi.Size())
	if _, err := f.ReadAt(buf, 0); err != nil && err != io.EOF {
		return err
	}
	if api.ContentSum(buf) != req.Sum {
		return fmt.Errorf("%w (same size, other content)", api.ErrChanged)
	}
	return nil
}

// inPlaceReason says why a new file may not take fi's place without
// anyone noticing, or "": it has one link and is ours (or we can give it
// back, as root).
func inPlaceReason(fi fs.FileInfo) string {
	st, ok := fi.Sys().(*syscall.Stat_t)
	switch {
	case !ok:
		return "not a plain file here"
	case st.Nlink != 1:
		return "it has other hard links"
	case os.Geteuid() != 0 && st.Uid != uint32(os.Geteuid()):
		return "it belongs to another user"
	}
	return ""
}

// editRecord is a new file being written beside the one it replaces,
// noted in the state dir first: one a crash left is removed when the
// node starts again (sweepEdits).
type editRecord struct {
	Root string `json:"root"`
	Path string `json:"path"`
}

func (e *Engine) editsDir() string { return filepath.Join(e.cfg.StateDir, "edits") }

// replaceFile writes data to a new file beside p with mode (and, as root,
// fi's owner), renames it over p and syncs the folder.
func (e *Engine) replaceFile(rootID string, root *os.Root, p string, fi fs.FileInfo, mode fs.FileMode, data []byte) error {
	rnd := make([]byte, 8)
	rand.Read(rnd)
	tag := hex.EncodeToString(rnd)
	tmp := path.Join(path.Dir(p), fmt.Sprintf(".%s.%s-edit-%s", path.Base(p), product.Name, tag))
	if err := os.MkdirAll(e.editsDir(), 0o700); err != nil {
		return err
	}
	rec := filepath.Join(e.editsDir(), tag)
	b, _ := json.Marshal(editRecord{rootID, tmp})
	if err := os.WriteFile(rec, b, 0o600); err != nil {
		return err
	}
	defer os.Remove(rec)
	f, err := root.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	ok := false
	defer func() {
		if !ok {
			f.Close()
			root.Remove(tmp)
		}
	}()
	if _, err := f.Write(data); err != nil {
		return err
	}
	if st, isUnix := fi.Sys().(*syscall.Stat_t); isUnix && os.Geteuid() == 0 {
		if err := f.Chown(int(st.Uid), int(st.Gid)); err != nil {
			return err
		}
	}
	if err := f.Chmod(mode); err != nil {
		return err
	}
	if err := f.Sync(); err != nil {
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := root.Rename(tmp, p); err != nil {
		return err
	}
	ok = true
	if d, err := root.Open(path.Dir(p)); err == nil {
		d.Sync() // the rename itself
		d.Close()
	}
	return nil
}

// sweepEdits removes the new files of saves a crash interrupted.
func (e *Engine) sweepEdits() {
	ents, err := os.ReadDir(e.editsDir())
	if err != nil {
		return
	}
	for _, ent := range ents {
		rec := filepath.Join(e.editsDir(), ent.Name())
		var r editRecord
		if b, err := os.ReadFile(rec); err == nil && json.Unmarshal(b, &r) == nil && strings.Contains(path.Base(r.Path), "-edit-") {
			if root, err := e.fs.WritableRoot(r.Root); err == nil {
				if err := root.Remove(r.Path); err == nil {
					e.log.Info("removed the new file of a save that was interrupted", "root", r.Root, "path", r.Path)
				}
			}
		}
		os.Remove(rec)
	}
}

// reserve allocates n bytes for f where the filesystem can (not NFS v3),
// so a full disk fails before anything is overwritten.
func reserve(f *os.File, n int64) error {
	err := syscall.Fallocate(int(f.Fd()), 0, 0, n)
	if errors.Is(err, syscall.EOPNOTSUPP) || errors.Is(err, syscall.ENOSYS) {
		return nil
	}
	return err
}

func written(root *os.Root, p string) (*api.WriteResult, error) {
	fi, err := root.Stat(p)
	if err != nil {
		return nil, err
	}
	return &api.WriteResult{Size: fi.Size(), ModTime: fi.ModTime()}, nil
}
