package store

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"fkbr/internal/api"
	"fkbr/internal/transport"
)

// Blobs live under blobs/<kind>/<id>/<file>.

var blobPart = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)

func blobPath(m *Dir, name string) (string, error) {
	parts := strings.Split(name, "/")
	if len(parts) != 3 {
		return "", fmt.Errorf("%w: blob name %q must be kind/id/file", api.ErrBadPath, name)
	}
	for _, p := range parts {
		if !blobPart.MatchString(p) {
			return "", fmt.Errorf("%w: blob name %q", api.ErrBadPath, name)
		}
	}
	return m.path(append([]string{"blobs"}, parts...)...), nil
}

type blobWriter struct {
	m    *Dir
	f    *syncedFile
	dest string
	done bool
}

func (m *Dir) CreateBlob(_ context.Context, name string) (transport.ChunkWriter, error) {
	dest, err := blobPath(m, name)
	if err != nil {
		return nil, err
	}
	if err := m.mkdir(filepath.Dir(dest)); err != nil {
		return nil, err
	}
	f, err := os.CreateTemp(m.path("tmp"), ".blob-*")
	if err != nil {
		return nil, err
	}
	return &blobWriter{m: m, f: newSyncedFile(f), dest: dest}, nil
}

func (w *blobWriter) Write(p []byte) (int, error) { return w.f.Write(p) }

func (w *blobWriter) Commit([]byte) error {
	defer w.Abort()
	if err := w.f.Sync(); err != nil {
		return err
	}
	if err := w.f.Close(); err != nil {
		return err
	}
	if err := os.Chmod(w.f.Name(), w.m.fileMode()); err != nil {
		return err
	}
	if err := os.Rename(w.f.Name(), w.dest); err != nil {
		return err
	}
	w.done = true
	return nil
}

func (w *blobWriter) Abort() {
	if !w.done {
		w.f.Close()
		os.Remove(w.f.Name())
		w.done = true
	}
}

func (m *Dir) OpenBlob(_ context.Context, name string) (io.ReadCloser, error) {
	p, err := blobPath(m, name)
	if err != nil {
		return nil, err
	}
	m.freshFile(p)
	return os.Open(p)
}

func (m *Dir) PruneBlobs(ctx context.Context, maxAge time.Duration) error {
	kinds, err := os.ReadDir(m.path("blobs"))
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	now, err := m.serverNow() // see Prune
	if err != nil {
		return fmt.Errorf("store: probe: %w", err)
	}
	cutoff := now.Add(-max(maxAge, minBlobAge))
	// A sync's transfers read their path lists while they run, however
	// long that takes.
	inUse := map[string]bool{}
	if jobs, err := m.ListJobs(ctx); err == nil {
		registered, _ := m.listNames(m.path("nodes"))
		for i := range jobs {
			if p := jobs[i].Request.SyncPlan; p != "" && !jobFinished(&jobs[i], registered) {
				inUse[p] = true
			}
		}
	} else {
		return err // not knowing which are in use, delete nothing
	}
	// Blobs of one ID (a plan and its scans) go together, by the newest:
	// applying a plan long after its dry run refreshes the plan, and its
	// deletes still read the scans.
	newest := map[string]time.Time{}
	var dirs [][2]string // kind, id
	for _, k := range kinds {
		ids, err := os.ReadDir(m.path("blobs", k.Name()))
		if err != nil {
			continue
		}
		for _, id := range ids {
			if t := newestMtime(m.path("blobs", k.Name(), id.Name())); t.After(newest[id.Name()]) {
				newest[id.Name()] = t
			}
			dirs = append(dirs, [2]string{k.Name(), id.Name()})
		}
	}
	var errs []error
	for _, d := range dirs {
		if !inUse[d[1]] && newest[d[1]].Before(cutoff) {
			errs = append(errs, removeRacing(m.path("blobs", d[0], d[1])))
		}
	}
	return errors.Join(errs...)
}

// newestMtime is the latest mtime of dir and its direct entries.
func newestMtime(dir string) time.Time {
	var newest time.Time
	if fi, err := os.Stat(dir); err == nil {
		newest = fi.ModTime()
	}
	des, _ := os.ReadDir(dir)
	for _, de := range des {
		if fi, err := de.Info(); err == nil && fi.ModTime().After(newest) {
			newest = fi.ModTime()
		}
	}
	return newest
}
