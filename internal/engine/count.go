package engine

import (
	"context"
	"strings"
	"sync"

	"fkbr/internal/api"
	"fkbr/internal/localfs"
	"fkbr/internal/receive"
)

// totals is what a source counts alongside sending, so readers can show
// progress and an ETA without the transfer waiting for a full count.
type totals struct {
	mu           sync.Mutex
	files, bytes int64
	done, exact  bool
}

func (t *totals) add(ent api.Entry) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.exact {
		return
	}
	switch ent.Type {
	case api.TypeFile:
		t.files++
		t.bytes += ent.Size
	case api.TypeDir, api.TypeSymlink:
		t.files++
	}
}

// finish marks the count complete; exact totals (from the finished
// production) win over a count still running.
func (t *totals) finish(files, bytes int64, exact bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.exact {
		return
	}
	if exact {
		t.files, t.bytes, t.exact = files, bytes, true
	}
	t.done = true
}

// apply copies the totals into a status before it is published.
func (t *totals) apply(st *api.JobStatus) {
	t.mu.Lock()
	defer t.mu.Unlock()
	st.TotalFiles, st.TotalBytes, st.Counted = t.files, t.bytes, t.done
}

// count walks what the job sends, as produce does, without reading data.
func (s *sender) count(ctx context.Context) {
	e, req := s.e, s.job.Request
	visit := func(p string) error {
		if !req.NoRecurse {
			return e.fs.Walk(req.SrcRoot, p, func(rel string, ent api.Entry, err error) error {
				if err := ctx.Err(); err != nil {
					return err
				}
				if rel == receive.StagingDir || strings.HasPrefix(rel, receive.StagingDir+"/") {
					return localfs.SkipDir
				}
				if err == nil {
					s.totals.add(ent)
				}
				return nil
			})
		}
		if ent, err := e.fs.Stat(req.SrcRoot, p); err == nil {
			s.totals.add(ent)
		}
		return ctx.Err()
	}
	if req.PathsBlob != "" {
		rc, err := e.st.OpenBlob(ctx, req.PathsBlob)
		if err != nil {
			return
		}
		err = readPathList(rc, visit)
		rc.Close()
		if err != nil {
			return
		}
	} else {
		for _, p := range req.Paths {
			if visit(p) != nil {
				return
			}
		}
	}
	s.totals.finish(0, 0, false)
}
