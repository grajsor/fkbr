package nfsmailbox

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
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"fkbr/internal/api"
	"fkbr/internal/config"
	"fkbr/internal/transport"
)

var _ transport.Store = (*Mailbox)(nil)

func validJobID(id string) error {
	if !config.ValidID(id) {
		return fmt.Errorf("%w: %q", api.ErrUnknownJob, id)
	}
	return nil
}

func chunkName(seq int, ext string) string { return fmt.Sprintf("%08d%s", seq, ext) }

func (m *Mailbox) CreateJob(_ context.Context, job *api.Job) error {
	if err := validJobID(job.ID); err != nil {
		return err
	}
	dir := m.path("jobs", job.ID)
	if err := mkdir(filepath.Join(dir, "status")); err != nil {
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

func (m *Mailbox) Detach(_ context.Context, id string, d api.Detached) error {
	if err := validJobID(id); err != nil {
		return err
	}
	if !config.ValidID(d.Node) {
		return fmt.Errorf("%w: %q", api.ErrUnknownNode, d.Node)
	}
	dir := m.path("jobs", id, "detached")
	if err := mkdir(dir); err != nil {
		return err
	}
	return m.writeJSON(filepath.Join(dir, d.Node+".json"), &d)
}

func (m *Mailbox) GetJob(_ context.Context, id string) (*api.Job, error) {
	if err := validJobID(id); err != nil {
		return nil, err
	}
	dir := m.path("jobs", id)
	m.fresh(dir)
	m.fresh(filepath.Join(dir, "status"))
	m.fresh(filepath.Join(dir, "detached"))
	data, err := os.ReadFile(filepath.Join(dir, "job.json"))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, fmt.Errorf("%w: %s", api.ErrUnknownJob, id)
	}
	if err != nil {
		return nil, err
	}
	var job api.Job
	if err := json.Unmarshal(data, &job); err != nil {
		return nil, fmt.Errorf("job %s: %w", id, err)
	}
	// Markers appear later, so look for them by listing (see listed.go).
	if b, err := m.readListed(dir, "COMPLETE"); err == nil {
		job.TotalChunks, _ = strconv.Atoi(strings.TrimSpace(string(b)))
	}
	if ok, _ := m.listed(dir, "CANCEL"); ok {
		job.Cancelled = true
	}
	job.Detached = nil
	if names, err := listJSON(filepath.Join(dir, "detached")); err == nil {
		for _, n := range names {
			var d api.Detached
			if b, err := os.ReadFile(filepath.Join(dir, "detached", n)); err == nil && json.Unmarshal(b, &d) == nil {
				job.Detached = append(job.Detached, d)
			}
		}
	}
	names, err := listJSON(filepath.Join(dir, "status"))
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return nil, err
	}
	for _, n := range names {
		b, err := os.ReadFile(filepath.Join(dir, "status", n))
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

func (m *Mailbox) ListJobs(ctx context.Context) ([]api.Job, error) {
	m.fresh(m.path("jobs"))
	des, err := os.ReadDir(m.path("jobs"))
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

func (m *Mailbox) PutStatus(_ context.Context, id string, st *api.JobStatus) error {
	if err := validJobID(id); err != nil {
		return err
	}
	if st.Node != m.self {
		return fmt.Errorf("nfsmailbox: status for %q written by %q", st.Node, m.self)
	}
	return m.writeJSON(m.path("jobs", id, "status", m.self+"."+string(st.Role)+".json"), st)
}

func (m *Mailbox) RequestCancel(_ context.Context, id string) error {
	if err := validJobID(id); err != nil {
		return err
	}
	m.fresh(m.path("jobs", id))
	if _, err := os.Stat(m.path("jobs", id, "job.json")); err != nil {
		return fmt.Errorf("%w: %s", api.ErrUnknownJob, id)
	}
	return m.write(m.path("jobs", id, "CANCEL"), []byte(m.self+"\n"))
}

func (m *Mailbox) SetComplete(_ context.Context, id string, chunks int) error {
	return m.write(m.path("jobs", id, "COMPLETE"), []byte(strconv.Itoa(chunks)+"\n"))
}

type chunkWriter struct {
	m    *Mailbox
	f    *os.File
	dir  string
	seq  int
	done bool
}

func (m *Mailbox) CreateChunk(_ context.Context, id string, seq int) (transport.ChunkWriter, error) {
	if err := validJobID(id); err != nil {
		return nil, err
	}
	dir := m.path("data", m.self, id)
	if err := mkdir(filepath.Join(dir, "acks")); err != nil {
		return nil, err
	}
	f, err := os.CreateTemp(m.path("tmp"), "."+id+"-"+chunkName(seq, ".chunk")+".*")
	if err != nil {
		return nil, err
	}
	return &chunkWriter{m: m, f: f, dir: dir, seq: seq}, nil
}

func (w *chunkWriter) Write(p []byte) (int, error) { return w.f.Write(p) }

func (w *chunkWriter) Commit(meta []byte) error {
	defer w.Abort()
	if err := w.f.Chmod(shareFileMode); err != nil { // CreateTemp made it 0600
		return err
	}
	if err := w.f.Sync(); err != nil {
		return err
	}
	if err := w.f.Close(); err != nil {
		return err
	}
	if err := os.Rename(w.f.Name(), filepath.Join(w.dir, chunkName(w.seq, ".chunk"))); err != nil {
		return err
	}
	w.done = true
	return w.m.write(filepath.Join(w.dir, chunkName(w.seq, ".json")), meta)
}

func (w *chunkWriter) Abort() {
	if !w.done {
		w.f.Close()
		os.Remove(w.f.Name())
		w.done = true
	}
}

func (m *Mailbox) ChunkMeta(_ context.Context, src, id string, seq int) ([]byte, error) {
	return m.readListed(m.path("data", src, id), chunkName(seq, ".json"))
}

func (m *Mailbox) OpenChunk(_ context.Context, src, id string, seq int) (io.ReadCloser, error) {
	m.fresh(m.path("data", src, id))
	return os.Open(m.path("data", src, id, chunkName(seq, ".chunk")))
}

func (m *Mailbox) DeleteChunk(_ context.Context, id string, seq int) error {
	dir := m.path("data", m.self, id)
	// Meta first, so a reader never finds meta without its chunk.
	err := os.Remove(filepath.Join(dir, chunkName(seq, ".json")))
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	err = os.Remove(filepath.Join(dir, chunkName(seq, ".chunk")))
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	return err
}

func (m *Mailbox) SetAcked(_ context.Context, src, id, node string, n int) error {
	if node != m.self {
		return fmt.Errorf("nfsmailbox: ack for %q written by %q", node, m.self)
	}
	dir := m.path("data", src, id, "acks")
	if err := mkdir(dir); err != nil {
		return err
	}
	return m.write(filepath.Join(dir, node), []byte(strconv.Itoa(n)+"\n"))
}

func (m *Mailbox) Acked(_ context.Context, src, id, node string) (int, error) {
	b, err := m.readListed(m.path("data", src, id, "acks"), node)
	if errors.Is(err, fs.ErrNotExist) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	return strconv.Atoi(strings.TrimSpace(string(b)))
}

func (m *Mailbox) StagedBytes(_ context.Context, id string) (job, total int64, err error) {
	base := m.path("data", m.self)
	jobs, err := os.ReadDir(base)
	if errors.Is(err, fs.ErrNotExist) {
		return 0, 0, nil
	}
	if err != nil {
		return 0, 0, err
	}
	for _, j := range jobs {
		des, err := os.ReadDir(filepath.Join(base, j.Name()))
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

func (m *Mailbox) RemoveJobData(_ context.Context, id string) error {
	if err := validJobID(id); err != nil {
		return err
	}
	return os.RemoveAll(m.path("data", m.self, id))
}

// unstartedJobAge is when a job record that no node ever started is pruned.
const unstartedJobAge = 24 * time.Hour

func (m *Mailbox) PruneJobs(ctx context.Context, maxAge time.Duration) error {
	des, err := os.ReadDir(m.path("jobs"))
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	cutoff := time.Now().Add(-maxAge)
	var errs []error
	for _, de := range des {
		j, err := m.GetJob(ctx, de.Name())
		if err != nil || j.Created.After(cutoff) {
			continue
		}
		// A job nobody ever picked up has no statuses; give it a day.
		if len(j.Participants) == 0 {
			if time.Since(j.Created) > unstartedJobAge {
				errs = append(errs, os.RemoveAll(m.path("jobs", de.Name())))
			}
			continue
		}
		finished := true
		for _, p := range j.Participants {
			finished = finished && p.State.Terminal()
		}
		if finished {
			errs = append(errs, os.RemoveAll(m.path("jobs", de.Name())))
		}
	}
	return errors.Join(errs...)
}
