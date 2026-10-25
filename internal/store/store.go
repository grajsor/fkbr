package store

// Job and data layout:
//
//	jobs/<id>/job.json            api.Job as submitted (initiator writes once)
//	jobs/<id>/status/<node>.<role>.json  api.JobStatus, replaced by that node
//	jobs/<id>/COMPLETE            total chunk count (source writes once)
//	jobs/<id>/CANCEL              present = cancellation requested
//	jobs/<id>/detached/<node>.json  api.Detached: target dropped from the job
//	data/<src>/<id>/<seq>.chunk   staged bytes
//	data/<src>/<id>/<seq>.json    chunk meta, written after the chunk = published
//	data/<src>/<id>/acks/<node>   chunks consumed by node, as a count

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"

	"fkbr/internal/api"
	"fkbr/internal/config"
	"fkbr/internal/transport"
)

var _ transport.Store = (*Dir)(nil)

func validJobID(id string) error {
	if !config.ValidID(id) {
		return fmt.Errorf("%w: %q", api.ErrUnknownJob, id)
	}
	return nil
}

// validChunkRef checks the names of a chunk path (data/<src>/<id>). Every
// name from a caller is checked before it becomes a path: over ssh the
// caller is another host (docs/ssh.md).
func validChunkRef(src, id string) error {
	if !config.ValidID(src) {
		return fmt.Errorf("%w: %q", api.ErrUnknownNode, src)
	}
	return validJobID(id)
}

// checkJobNames refuses a job record whose source or target names could
// not be node IDs: they become paths on the share (data/<src>/...,
// acks/<target>), so a forged "../" would reach outside it (security
// audit 2026-10-22).
func checkJobNames(id string, j *api.Job) error {
	if j.ID != id {
		return fmt.Errorf("job %s: %w: it says it is %q", id, errBadRecord, j.ID)
	}
	for _, n := range append([]string{j.Request.SrcNode}, j.Request.DstNodes...) {
		if !config.ValidID(n) {
			return fmt.Errorf("job %s: %w: invalid node name %q", id, errBadRecord, n)
		}
	}
	return nil
}

// errBadRecord: a job record no node could have written. Nobody can run
// such a job, so pruning removes it once it is old.
var errBadRecord = errors.New("bad job record")

func chunkName(seq int, ext string) string { return fmt.Sprintf("%08d%s", seq, ext) }

func (m *Dir) CreateJob(_ context.Context, job *api.Job) error {
	if err := validJobID(job.ID); err != nil {
		return err
	}
	dir := m.path("jobs", job.ID)
	if err := m.mkdir(filepath.Join(dir, "status")); err != nil {
		return err
	}
	for _, d := range job.Detached {
		if err := m.Detach(context.Background(), job.ID, d); err != nil {
			return err
		}
	}
	rec := *job
	rec.Participants, rec.State, rec.TotalChunks, rec.Cancelled, rec.Detached = nil, "", 0, false, nil
	return m.writeJSON(filepath.Join(dir, "job.json"), &rec)
}

func (m *Dir) Detach(_ context.Context, id string, d api.Detached) error {
	if err := validJobID(id); err != nil {
		return err
	}
	if !config.ValidID(d.Node) {
		return fmt.Errorf("%w: %q", api.ErrUnknownNode, d.Node)
	}
	dir := m.path("jobs", id, "detached")
	if err := m.mkdir(dir); err != nil {
		return err
	}
	return m.writeJSON(filepath.Join(dir, d.Node+".json"), &d)
}

func (m *Dir) GetJob(_ context.Context, id string) (*api.Job, error) {
	if err := validJobID(id); err != nil {
		return nil, err
	}
	dir := m.path("jobs", id)
	m.fresh(dir)
	m.fresh(filepath.Join(dir, "status"))
	m.fresh(filepath.Join(dir, "detached"))
	data, err := m.readFile(filepath.Join(dir, "job.json"))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, fmt.Errorf("%w: %s", api.ErrUnknownJob, id)
	}
	if err != nil {
		return nil, err
	}
	var job api.Job
	if err := json.Unmarshal(data, &job); err != nil {
		return nil, fmt.Errorf("job %s: %w: %w", id, errBadRecord, err)
	}
	if err := checkJobNames(id, &job); err != nil {
		return nil, err
	}
	// Markers appear later, so look for them by listing (see listed.go).
	if b, err := m.readListed(dir, "COMPLETE"); err == nil {
		job.TotalChunks, _ = strconv.Atoi(strings.TrimSpace(string(b)))
	}
	if ok, _ := m.listed(dir, "CANCEL"); ok {
		job.Cancelled = true
	}
	job.Detached = nil
	if names, err := m.listJSON(filepath.Join(dir, "detached")); err == nil {
		for _, n := range names {
			var d api.Detached
			if b, err := m.readFile(filepath.Join(dir, "detached", n)); err == nil && json.Unmarshal(b, &d) == nil {
				job.Detached = append(job.Detached, d)
			}
		}
	}
	names, err := m.listJSON(filepath.Join(dir, "status"))
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return nil, err
	}
	for _, n := range names {
		b, err := m.readFile(filepath.Join(dir, "status", n))
		if err != nil {
			continue
		}
		var st api.JobStatus
		if json.Unmarshal(b, &st) == nil {
			job.Participants = append(job.Participants, st)
		}
	}
	return &job, nil
}

func (m *Dir) ListJobs(ctx context.Context) ([]api.Job, error) {
	m.fresh(m.path("jobs"))
	des, err := m.fs.ReadDir(m.path("jobs"))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []api.Job
	for i := len(des) - 1; i >= 0; i-- { // IDs sort by time; newest first
		if !des[i].IsDir() {
			continue
		}
		j, err := m.GetJob(ctx, des[i].Name())
		if err != nil {
			continue // half-created or being pruned
		}
		out = append(out, *j)
	}
	return out, nil
}

func (m *Dir) PutStatus(_ context.Context, id string, st *api.JobStatus) error {
	if err := validJobID(id); err != nil {
		return err
	}
	if st.Node != m.self {
		return fmt.Errorf("store: status for %q written by %q", st.Node, m.self)
	}
	if st.Role != api.RoleSource && st.Role != api.RoleTarget { // part of the file name
		return fmt.Errorf("store: status with role %q", st.Role)
	}
	return m.writeJSON(m.path("jobs", id, "status", m.self+"."+string(st.Role)+".json"), st)
}

func (m *Dir) RequestCancel(_ context.Context, id string) error {
	if err := validJobID(id); err != nil {
		return err
	}
	m.fresh(m.path("jobs", id))
	if _, err := m.fs.Stat(m.path("jobs", id, "job.json")); err != nil {
		return fmt.Errorf("%w: %s", api.ErrUnknownJob, id)
	}
	return m.write(m.path("jobs", id, "CANCEL"), []byte(m.self+"\n"))
}

func (m *Dir) SetComplete(_ context.Context, id string, chunks int) error {
	if err := validJobID(id); err != nil {
		return err
	}
	return m.write(m.path("jobs", id, "COMPLETE"), []byte(strconv.Itoa(chunks)+"\n"))
}

type chunkWriter struct {
	m    *Dir
	f    *syncedFile
	dir  string
	seq  int
	done bool
}

func (m *Dir) CreateChunk(_ context.Context, id string, seq int) (transport.ChunkWriter, error) {
	if err := validJobID(id); err != nil {
		return nil, err
	}
	dir := m.path("data", m.self, id)
	if err := m.mkdir(filepath.Join(dir, "acks")); err != nil {
		return nil, err
	}
	// A source killed while writing this chunk left its temp file; the
	// restarted one writes the same chunk again (cloud run 2026-10-24).
	m.removeTemps(id, chunkName(seq, ".chunk"))
	f, err := m.fs.CreateTemp(m.path("tmp"), "."+id+"-"+chunkName(seq, ".chunk")+".*")
	if err != nil {
		return nil, err
	}
	return &chunkWriter{m: m, f: m.newSyncedFile(f), dir: dir, seq: seq}, nil
}

func (w *chunkWriter) Write(p []byte) (int, error) { return w.f.Write(p) }

func (w *chunkWriter) Commit(meta []byte) error {
	defer w.Abort()
	if err := w.f.Chmod(w.m.fileMode()); err != nil { // CreateTemp made it 0600
		return err
	}
	if err := w.f.sync(); err != nil {
		return err
	}
	if err := w.f.Close(); err != nil {
		return err
	}
	if err := w.m.fs.Rename(w.f.Name(), filepath.Join(w.dir, chunkName(w.seq, ".chunk"))); err != nil {
		return err
	}
	w.done = true
	return w.m.write(filepath.Join(w.dir, chunkName(w.seq, ".json")), meta)
}

func (w *chunkWriter) Abort() {
	if !w.done {
		w.f.Close()
		w.m.fs.Remove(w.f.Name())
		w.done = true
	}
}

func (m *Dir) ChunkMeta(_ context.Context, src, id string, seq int) ([]byte, error) {
	if err := validChunkRef(src, id); err != nil {
		return nil, err
	}
	return m.readListed(m.path("data", src, id), chunkName(seq, ".json"))
}

func (m *Dir) OpenChunk(_ context.Context, src, id string, seq int) (io.ReadCloser, error) {
	if err := validChunkRef(src, id); err != nil {
		return nil, err
	}
	m.fresh(m.path("data", src, id))
	return m.fs.Open(m.path("data", src, id, chunkName(seq, ".chunk")))
}

func (m *Dir) DeleteChunk(_ context.Context, id string, seq int) error {
	if err := validJobID(id); err != nil {
		return err
	}
	dir := m.path("data", m.self, id)
	// Meta first, so a reader never finds meta without its chunk.
	err := m.fs.Remove(filepath.Join(dir, chunkName(seq, ".json")))
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	err = m.fs.Remove(filepath.Join(dir, chunkName(seq, ".chunk")))
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	return err
}

func (m *Dir) SetAcked(_ context.Context, src, id, node string, n int) error {
	if node != m.self {
		return fmt.Errorf("store: ack for %q written by %q", node, m.self)
	}
	if err := validChunkRef(src, id); err != nil {
		return err
	}
	dir := m.path("data", src, id, "acks")
	if err := m.mkdir(dir); err != nil {
		return err
	}
	return m.write(filepath.Join(dir, node), []byte(strconv.Itoa(n)+"\n"))
}

func (m *Dir) Acked(_ context.Context, src, id, node string) (int, error) {
	if err := validChunkRef(src, id); err != nil {
		return 0, err
	}
	if !config.ValidID(node) {
		return 0, fmt.Errorf("%w: %q", api.ErrUnknownNode, node)
	}
	b, err := m.readListed(m.path("data", src, id, "acks"), node)
	if errors.Is(err, fs.ErrNotExist) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	return strconv.Atoi(strings.TrimSpace(string(b)))
}

// FreeBytes is the space left on the filesystem of the share, for this
// user.
func (m *Dir) FreeBytes() (int64, error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(m.root, &st); err != nil {
		return 0, err
	}
	return int64(st.Bavail) * int64(st.Bsize), nil
}

func (m *Dir) StagedBytes(_ context.Context, id string) (job, total int64, err error) {
	base := m.path("data", m.self)
	jobs, err := m.fs.ReadDir(base)
	if errors.Is(err, fs.ErrNotExist) {
		return 0, 0, nil
	}
	if err != nil {
		return 0, 0, err
	}
	for _, j := range jobs {
		des, err := m.fs.ReadDir(filepath.Join(base, j.Name()))
		if err != nil {
			continue
		}
		for _, de := range des {
			if !strings.HasSuffix(de.Name(), ".chunk") {
				continue
			}
			if fi, err := de.Info(); err == nil {
				total += fi.Size()
				if j.Name() == id {
					job += fi.Size()
				}
			}
		}
	}
	return job, total, nil
}

func (m *Dir) RemoveJobData(_ context.Context, id string) error {
	if err := validJobID(id); err != nil {
		return err
	}
	m.removeTemps(id, "") // a chunk a killed source was writing
	return m.fs.RemoveAll(m.path("data", m.self, id))
}

// removeTemps removes the temp files of job id's chunks (of chunk name, or
// all), which only this node writes. Others are pruned after StaleTmp.
func (m *Dir) removeTemps(id, name string) {
	pre := "." + id + "-" + name
	des, err := m.fs.ReadDir(m.path("tmp"))
	if err != nil {
		return
	}
	for _, de := range des {
		if strings.HasPrefix(de.Name(), pre) {
			m.fs.Remove(m.path("tmp", de.Name()))
		}
	}
}

// unstartedJobAge is when a job record that no node ever started is pruned.
const unstartedJobAge = 24 * time.Hour

// Floors under any keep (prune -keep 0 included): a job that just ended
// stays a minute, so the CLI that follows it reads how it ended, and
// plans, scans and job folders without a record stay ten minutes, so a
// sync being planned or applied keeps them.
var (
	minFinishedAge = time.Minute
	minBlobAge     = 10 * time.Minute
)

// jobFinished reports whether nothing will change in j any more: every
// participant ended, or is a detached target or a forgotten node.
func jobFinished(j *api.Job, registered []string) bool {
	if len(j.Participants) == 0 {
		return false
	}
	for _, p := range j.Participants {
		stale := (p.Role == api.RoleTarget && j.IsDetached(p.Node)) || !slices.Contains(registered, p.Node)
		if !p.State.Terminal() && !stale {
			return false
		}
	}
	return true
}

// newestEntry is the latest mtime of dir's entries. Not of dir itself: a
// refresh (Options.Fresh) bumps a directory's mtime on every read. The
// NFS refresh's probe files (".r-...") do not count either.
func (m *Dir) newestEntry(dir string) time.Time {
	var newest time.Time
	des, _ := m.fs.ReadDir(dir)
	for _, de := range des {
		if strings.HasPrefix(de.Name(), ".r-") {
			continue
		}
		if fi, err := de.Info(); err == nil && fi.ModTime().After(newest) {
			newest = fi.ModTime()
		}
	}
	return newest
}

// removeRacing is os.RemoveAll for a share directory others may still
// write into (a status landing as a job is pruned): it tries again, and
// what is left the next prune removes (see the orphan rule in PruneJobs).
func (m *Dir) removeRacing(dir string) error {
	err := m.fs.RemoveAll(dir)
	if errors.Is(err, syscall.ENOTEMPTY) || errors.Is(err, syscall.EEXIST) {
		time.Sleep(100 * time.Millisecond)
		if err = m.fs.RemoveAll(dir); errors.Is(err, syscall.ENOTEMPTY) || errors.Is(err, syscall.EEXIST) {
			return nil
		}
	}
	return err
}

func (m *Dir) PruneJobs(ctx context.Context, maxAge time.Duration) error {
	des, err := m.fs.ReadDir(m.path("jobs"))
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	now, err := m.serverNow() // job.json's mtime is server time; see Prune
	if err != nil {
		return fmt.Errorf("store: probe: %w", err)
	}
	// Statuses of detached targets and of nodes that were forgotten never
	// change again, whatever they say; they must not keep a job forever.
	registered, err := m.listNames(m.path("nodes"))
	if err != nil {
		return err
	}
	var errs []error
	for _, de := range des {
		dir := m.path("jobs", de.Name())
		fi, err := m.fs.Stat(filepath.Join(dir, "job.json"))
		if errors.Is(err, fs.ErrNotExist) {
			// A folder without its record: half pruned (a status written
			// as it went), or recreated by a late writer. Nobody reads it.
			// One with nothing in it yet may be a job CreateJob is making:
			// its own mtime tells.
			newest := m.newestEntry(dir)
			if fi, err := m.fs.Stat(dir); err == nil && newest.IsZero() {
				newest = fi.ModTime()
			}
			if validJobID(de.Name()) == nil && now.Sub(newest) > max(maxAge, minBlobAge) {
				errs = append(errs, m.removeRacing(dir))
			}
			continue
		}
		if err != nil {
			continue
		}
		age := now.Sub(fi.ModTime())
		if age < maxAge {
			continue
		}
		j, err := m.GetJob(ctx, de.Name())
		if errors.Is(err, errBadRecord) {
			errs = append(errs, m.removeRacing(dir))
			continue
		}
		if err != nil {
			continue
		}
		// A job nobody ever picked up has no statuses; give it a day.
		if len(j.Participants) == 0 {
			if age > unstartedJobAge {
				errs = append(errs, m.removeRacing(dir))
			}
			continue
		}
		if jobFinished(j, registered) && now.Sub(m.newestEntry(filepath.Join(dir, "status"))) >= minFinishedAge {
			errs = append(errs, m.removeRacing(dir))
		}
	}
	return errors.Join(errs...)
}
