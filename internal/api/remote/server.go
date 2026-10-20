// Package remote exposes an api.Service over HTTP+JSON on a Unix socket
// and provides the matching api.Client. The daemon serves; CLI and TUI
// connect. A future web UI can mount Handler as well.
//
//	GET  /v1/self                 {"id": node, "share": dir, "roots": []api.Root}
//	GET  /v1/nodes                []api.Node
//	POST /v1/nodes/{id}/forget
//	POST /v1/prune                ?keep=DURATION (default 0)
//	POST /v1/list                 api.ListRequest -> api.ListResult
//	POST /v1/mkdir                api.MkdirRequest
//	POST /v1/remove               api.RemoveRequest -> api.RemoveResult
//	POST /v1/read                 api.ReadRequest -> api.ReadResult
//	GET  /v1/jobs                 []api.Job
//	POST /v1/jobs                 api.TransferRequest -> {"id": job}
//	GET  /v1/jobs/{id}            api.Job
//	POST /v1/jobs/{id}/cancel
//	POST /v1/jobs/{id}/catchup    -> {"id": new job}
//	POST /v1/sync                 api.SyncRequest -> api.SyncPlan (dry run)
//	GET  /v1/sync/{id}            api.SyncPlan
//	POST /v1/sync/{id}/apply      ?force=1 -> api.SyncPlan
//	GET  /v1/sync/{id}/entries    ?limit=N -> api.SyncEntries
//	GET  /v1/events               text/event-stream of api.Event
//
// Errors are {"error": msg, "code": code} with a matching HTTP status.
package remote

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"os"
	"strconv"
	"syscall"
	"time"

	"fkbr/internal/api"
)

// Handler returns the HTTP API for svc.
func Handler(svc api.Service) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/self", func(w http.ResponseWriter, r *http.Request) {
		self := map[string]any{"id": svc.Self()}
		if s, ok := svc.(interface {
			Share() string
			LocalRoots() []api.Root
		}); ok {
			self["share"], self["roots"] = s.Share(), s.LocalRoots()
		}
		reply(w, self, nil)
	})
	mux.HandleFunc("GET /v1/nodes", func(w http.ResponseWriter, r *http.Request) {
		v, err := svc.Nodes(r.Context())
		reply(w, v, err)
	})
	mux.HandleFunc("POST /v1/nodes/{id}/forget", func(w http.ResponseWriter, r *http.Request) {
		reply(w, struct{}{}, svc.Forget(r.Context(), r.PathValue("id")))
	})
	mux.HandleFunc("POST /v1/prune", func(w http.ResponseWriter, r *http.Request) {
		var keep time.Duration
		if k := r.URL.Query().Get("keep"); k != "" {
			var err error
			if keep, err = time.ParseDuration(k); err != nil {
				reply(w, nil, fmt.Errorf("%w: keep %q", api.ErrBadPath, k))
				return
			}
		}
		reply(w, struct{}{}, svc.Prune(r.Context(), keep))
	})
	mux.HandleFunc("POST /v1/list", func(w http.ResponseWriter, r *http.Request) {
		var req api.ListRequest
		if !decode(w, r, &req) {
			return
		}
		v, err := svc.List(r.Context(), req)
		reply(w, v, err)
	})
	mux.HandleFunc("POST /v1/mkdir", func(w http.ResponseWriter, r *http.Request) {
		var req api.MkdirRequest
		if !decode(w, r, &req) {
			return
		}
		reply(w, struct{}{}, svc.Mkdir(r.Context(), req))
	})
	mux.HandleFunc("POST /v1/remove", func(w http.ResponseWriter, r *http.Request) {
		var req api.RemoveRequest
		if !decode(w, r, &req) {
			return
		}
		v, err := svc.Remove(r.Context(), req)
		reply(w, v, err)
	})
	mux.HandleFunc("POST /v1/read", func(w http.ResponseWriter, r *http.Request) {
		var req api.ReadRequest
		if !decode(w, r, &req) {
			return
		}
		v, err := svc.Read(r.Context(), req)
		reply(w, v, err)
	})
	mux.HandleFunc("GET /v1/jobs", func(w http.ResponseWriter, r *http.Request) {
		v, err := svc.Jobs(r.Context())
		reply(w, v, err)
	})
	mux.HandleFunc("POST /v1/jobs", func(w http.ResponseWriter, r *http.Request) {
		var req api.TransferRequest
		if !decode(w, r, &req) {
			return
		}
		id, err := svc.Submit(r.Context(), req)
		reply(w, map[string]string{"id": id}, err)
	})
	mux.HandleFunc("GET /v1/jobs/{id}", func(w http.ResponseWriter, r *http.Request) {
		v, err := svc.Job(r.Context(), r.PathValue("id"))
		reply(w, v, err)
	})
	mux.HandleFunc("POST /v1/jobs/{id}/cancel", func(w http.ResponseWriter, r *http.Request) {
		reply(w, struct{}{}, svc.Cancel(r.Context(), r.PathValue("id")))
	})
	mux.HandleFunc("POST /v1/jobs/{id}/catchup", func(w http.ResponseWriter, r *http.Request) {
		id, err := svc.CatchUp(r.Context(), r.PathValue("id"))
		reply(w, map[string]string{"id": id}, err)
	})
	mux.HandleFunc("POST /v1/sync", func(w http.ResponseWriter, r *http.Request) {
		var req api.SyncRequest
		if !decode(w, r, &req) {
			return
		}
		v, err := svc.PlanSync(r.Context(), req)
		reply(w, v, err)
	})
	mux.HandleFunc("GET /v1/sync/{id}", func(w http.ResponseWriter, r *http.Request) {
		v, err := svc.SyncPlan(r.Context(), r.PathValue("id"))
		reply(w, v, err)
	})
	mux.HandleFunc("GET /v1/sync/{id}/entries", func(w http.ResponseWriter, r *http.Request) {
		limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
		v, err := svc.SyncPlanEntries(r.Context(), r.PathValue("id"), limit)
		reply(w, v, err)
	})
	mux.HandleFunc("POST /v1/sync/{id}/apply", func(w http.ResponseWriter, r *http.Request) {
		v, err := svc.ApplySync(r.Context(), r.PathValue("id"), r.URL.Query().Get("force") == "1")
		reply(w, v, err)
	})
	mux.HandleFunc("GET /v1/events", func(w http.ResponseWriter, r *http.Request) {
		events(w, r, svc)
	})
	return mux
}

func decode(w http.ResponseWriter, r *http.Request, v any) bool {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(wireError{Error: "bad request: " + err.Error()})
		return false
	}
	return true
}

func reply(w http.ResponseWriter, v any, err error) {
	w.Header().Set("Content-Type", "application/json")
	if err != nil {
		we, status := encodeError(err)
		w.WriteHeader(status)
		json.NewEncoder(w).Encode(we)
		return
	}
	json.NewEncoder(w).Encode(v)
}

// events streams Server-Sent Events, with a comment line every 15s so dead
// connections are noticed.
func events(w http.ResponseWriter, r *http.Request, svc api.Service) {
	fl, ok := w.(http.Flusher)
	if !ok {
		reply(w, nil, errors.New("streaming unsupported"))
		return
	}
	ch, err := svc.Subscribe(r.Context())
	if err != nil {
		reply(w, nil, err)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)
	fl.Flush()
	keepalive := time.NewTicker(15 * time.Second)
	defer keepalive.Stop()
	for {
		select {
		case ev, ok := <-ch:
			if !ok {
				return
			}
			b, _ := json.Marshal(ev)
			fmt.Fprintf(w, "event: %s\ndata: %s\n\n", ev.Type, b)
			fl.Flush()
		case <-keepalive.C:
			fmt.Fprint(w, ": keepalive\n\n")
			fl.Flush()
		case <-r.Context().Done():
			return
		}
	}
}

// Listen creates the daemon socket with the given permissions. A leftover
// socket from a crashed daemon is replaced; a live one is an error.
func Listen(path string, mode os.FileMode) (net.Listener, error) {
	if _, err := os.Lstat(path); err == nil {
		if c, err := net.DialTimeout("unix", path, time.Second); err == nil {
			c.Close()
			return nil, fmt.Errorf("a daemon is already listening on %s", path)
		}
		if err := os.Remove(path); err != nil {
			return nil, err
		}
	} else if !errors.Is(err, fs.ErrNotExist) {
		return nil, err
	}
	l, err := net.Listen("unix", path)
	if err != nil {
		return nil, err
	}
	if err := os.Chmod(path, mode); err != nil {
		l.Close()
		return nil, err
	}
	return l, nil
}

// Socket upkeep intervals (see ServeSocket).
var (
	SocketCheckInterval = 30 * time.Second
	SocketTouchInterval = 10 * time.Minute
)

// Serve runs the API on l until ctx ends, then shuts down gracefully.
func Serve(ctx context.Context, l net.Listener, svc api.Service, log *slog.Logger) error {
	return ServeSocket(ctx, l, "", 0, svc, log)
}

// ServeSocket is Serve for a listener created by Listen(path, mode). It
// also keeps the socket file's timestamps fresh (tmpfiles cleans /tmp by
// age) and, if the file is removed anyway, listens on path again so
// clients can still connect.
func ServeSocket(ctx context.Context, l net.Listener, path string, mode os.FileMode, svc api.Service, log *slog.Logger) error {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	srv := &http.Server{
		Handler:           Handler(svc),
		ReadHeaderTimeout: 10 * time.Second,
		BaseContext:       func(net.Listener) context.Context { return ctx },
	}
	errc := make(chan error, 1)
	serve := func(l net.Listener) {
		go func() {
			if err := srv.Serve(l); err != nil && !errors.Is(err, http.ErrServerClosed) {
				select {
				case errc <- err:
				default:
				}
			}
		}()
	}
	serve(l)

	var check <-chan time.Time
	ino := socketInode(path)
	if path != "" {
		t := time.NewTicker(SocketCheckInterval)
		defer t.Stop()
		check = t.C
	}
	lastTouch := time.Now()
	for {
		select {
		case err := <-errc:
			return err
		case <-check:
			if cur := socketInode(path); cur == 0 || cur != ino {
				nl, err := Listen(path, mode)
				if err != nil {
					log.Warn("api socket vanished and could not be recreated", "socket", path, "err", err)
					continue
				}
				log.Info("api socket recreated", "socket", path)
				ino, lastTouch = socketInode(path), time.Now()
				serve(nl)
			} else if time.Since(lastTouch) >= SocketTouchInterval {
				now := time.Now()
				os.Chtimes(path, now, now)
				lastTouch = now
			}
		case <-ctx.Done():
			sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if err := srv.Shutdown(sctx); err != nil {
				log.Warn("api shutdown", "err", err)
			}
			return nil
		}
	}
}

// socketInode returns the inode of path if it is a socket, else 0.
func socketInode(path string) uint64 {
	if path == "" {
		return 0
	}
	var st syscall.Stat_t
	if err := syscall.Lstat(path, &st); err != nil || st.Mode&syscall.S_IFMT != syscall.S_IFSOCK {
		return 0
	}
	return st.Ino
}
