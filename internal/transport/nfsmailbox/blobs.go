package nfsmailbox

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

func blobPath(m *Mailbox, name string) (string, error) {
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
	f    *syncedFile
	dest string
	done bool
}

func (m *Mailbox) CreateBlob(_ context.Context, name string) (transport.ChunkWriter, error) {
	dest, err := blobPath(m, name)
	if err != nil {
		return nil, err
	}
	if err := mkdir(filepath.Dir(dest)); err != nil {
		return nil, err
	}
	f, err := os.CreateTemp(m.path("tmp"), ".blob-*")
	if err != nil {
		return nil, err
	}
	return &blobWriter{f: newSyncedFile(f), dest: dest}, nil
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
	if err := os.Chmod(w.f.Name(), shareFileMode); err != nil {
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

func (m *Mailbox) OpenBlob(_ context.Context, name string) (io.ReadCloser, error) {
	p, err := blobPath(m, name)
	if err != nil {
		return nil, err
	}
	m.freshFile(p)
	return os.Open(p)
}

func (m *Mailbox) PruneBlobs(ctx context.Context, maxAge time.Duration) error {
	kinds, err := os.ReadDir(m.path("blobs"))
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	now, err := m.serverNow() // see Prune
	if err != nil {
		return fmt.Errorf("nfsmailbox: probe: %w", err)
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
	var errs []error
	for _, k := range kinds {
		ids, err := os.ReadDir(m.path("blobs", k.Name()))
		if err != nil {
			continue
		}
		for _, id := range ids {
			dir := m.path("blobs", k.Name(), id.Name())
			if !inUse[id.Name()] && newestMtime(dir).Before(cutoff) {
				errs = append(errs, removeRacing(dir))
			}
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
