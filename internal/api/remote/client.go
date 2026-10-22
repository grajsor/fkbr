package remote

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"fkbr/internal/api"
	"fkbr/internal/product"
)

// Client talks to a daemon over its Unix socket.
type Client struct {
	http  *http.Client
	self  string
	share string
	roots []api.Root
}

var _ api.Client = (*Client)(nil)

// Dial connects to the daemon socket and fetches the node identity.
func Dial(ctx context.Context, socket string) (*Client, error) {
	tr := &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", socket)
		},
	}
	c := &Client{http: &http.Client{Transport: tr}}
	var self struct {
		ID    string
		Share string
		Roots []api.Root
	}
	dctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	if err := c.call(dctx, http.MethodGet, "/v1/self", nil, &self); err != nil {
		return nil, err
	}
	c.self, c.share, c.roots = self.ID, self.Share, self.Roots
	return c, nil
}

func (c *Client) call(ctx context.Context, method, path string, in, out any) error {
	var body io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, "http://"+product.Name+path, body)
	if err != nil {
		return err
	}
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		var we wireError
		if json.NewDecoder(resp.Body).Decode(&we) != nil || we.Error == "" {
			return fmt.Errorf("daemon: %s", resp.Status)
		}
		return decodeError(we)
	}
	if out == nil {
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

func (c *Client) Self() string { return c.self }

// Share is the share directory the node runs on ("" from older nodes).
func (c *Client) Share() string { return c.share }

// LocalRoots are the directories the node shares, with their paths.
func (c *Client) LocalRoots() []api.Root { return c.roots }

func (c *Client) Close() error {
	c.http.CloseIdleConnections()
	return nil
}

func (c *Client) Nodes(ctx context.Context) ([]api.Node, error) {
	var v []api.Node
	return v, c.call(ctx, http.MethodGet, "/v1/nodes", nil, &v)
}

var _ api.Linker = (*Client)(nil)

func (c *Client) Links(ctx context.Context) ([]api.Link, error) {
	var v []api.Link
	return v, c.call(ctx, http.MethodGet, "/v1/links", nil, &v)
}

func (c *Client) AddLink(ctx context.Context, host string, save bool) error {
	return c.call(ctx, http.MethodPost, "/v1/links", map[string]any{"host": host, "save": save}, nil)
}

func (c *Client) RemoveLink(ctx context.Context, host string) error {
	return c.call(ctx, http.MethodPost, "/v1/links/remove", map[string]any{"host": host}, nil)
}

func (c *Client) Forget(ctx context.Context, node string) error {
	return c.call(ctx, http.MethodPost, "/v1/nodes/"+url.PathEscape(node)+"/forget", nil, nil)
}

func (c *Client) Prune(ctx context.Context, keep time.Duration) error {
	return c.call(ctx, http.MethodPost, "/v1/prune?keep="+url.QueryEscape(keep.String()), nil, nil)
}

func (c *Client) List(ctx context.Context, req api.ListRequest) (*api.ListResult, error) {
	var v api.ListResult
	if err := c.call(ctx, http.MethodPost, "/v1/list", req, &v); err != nil {
		return nil, err
	}
	return &v, nil
}

func (c *Client) Mkdir(ctx context.Context, req api.MkdirRequest) error {
	return c.call(ctx, http.MethodPost, "/v1/mkdir", req, nil)
}

func (c *Client) Remove(ctx context.Context, req api.RemoveRequest) (*api.RemoveResult, error) {
	var v api.RemoveResult
	if err := c.call(ctx, http.MethodPost, "/v1/remove", req, &v); err != nil {
		return nil, err
	}
	return &v, nil
}

func (c *Client) Read(ctx context.Context, req api.ReadRequest) (*api.ReadResult, error) {
	var v api.ReadResult
	if err := c.call(ctx, http.MethodPost, "/v1/read", req, &v); err != nil {
		return nil, err
	}
	return &v, nil
}

func (c *Client) Submit(ctx context.Context, req api.TransferRequest) (string, error) {
	var v struct{ ID string }
	return v.ID, c.call(ctx, http.MethodPost, "/v1/jobs", req, &v)
}

func (c *Client) Job(ctx context.Context, id string) (*api.Job, error) {
	var v api.Job
	if err := c.call(ctx, http.MethodGet, "/v1/jobs/"+url.PathEscape(id), nil, &v); err != nil {
		return nil, err
	}
	return &v, nil
}

func (c *Client) Jobs(ctx context.Context) ([]api.Job, error) {
	var v []api.Job
	return v, c.call(ctx, http.MethodGet, "/v1/jobs", nil, &v)
}

func (c *Client) Cancel(ctx context.Context, id string) error {
	return c.call(ctx, http.MethodPost, "/v1/jobs/"+url.PathEscape(id)+"/cancel", nil, nil)
}

func (c *Client) CatchUp(ctx context.Context, id string) (string, error) {
	var v struct{ ID string }
	return v.ID, c.call(ctx, http.MethodPost, "/v1/jobs/"+url.PathEscape(id)+"/catchup", nil, &v)
}

func (c *Client) PlanSync(ctx context.Context, req api.SyncRequest) (*api.SyncPlan, error) {
	var v api.SyncPlan
	if err := c.call(ctx, http.MethodPost, "/v1/sync", req, &v); err != nil {
		return nil, err
	}
	return &v, nil
}

func (c *Client) SyncPlan(ctx context.Context, id string) (*api.SyncPlan, error) {
	var v api.SyncPlan
	if err := c.call(ctx, http.MethodGet, "/v1/sync/"+url.PathEscape(id), nil, &v); err != nil {
		return nil, err
	}
	return &v, nil
}

func (c *Client) SyncPlanEntries(ctx context.Context, id string, limit int) (*api.SyncEntries, error) {
	var v api.SyncEntries
	if err := c.call(ctx, http.MethodGet, fmt.Sprintf("/v1/sync/%s/entries?limit=%d", url.PathEscape(id), limit), nil, &v); err != nil {
		return nil, err
	}
	return &v, nil
}

func (c *Client) ApplySync(ctx context.Context, id string, force bool) (*api.SyncPlan, error) {
	q := ""
	if force {
		q = "?force=1"
	}
	var v api.SyncPlan
	if err := c.call(ctx, http.MethodPost, "/v1/sync/"+url.PathEscape(id)+"/apply"+q, nil, &v); err != nil {
		return nil, err
	}
	return &v, nil
}

// Subscribe reads the daemon's event stream until ctx ends or the
// connection drops; the channel is closed either way.
func (c *Client) Subscribe(ctx context.Context) (<-chan api.Event, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+product.Name+"/v1/events", nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		defer resp.Body.Close()
		var we wireError
		json.NewDecoder(resp.Body).Decode(&we)
		return nil, decodeError(we)
	}
	ch := make(chan api.Event, 256)
	go func() {
		defer close(ch)
		defer resp.Body.Close()
		sc := bufio.NewScanner(resp.Body)
		sc.Buffer(nil, 16<<20)
		for sc.Scan() {
			data, ok := strings.CutPrefix(sc.Text(), "data: ")
			if !ok {
				continue
			}
			var ev api.Event
			if json.Unmarshal([]byte(data), &ev) != nil {
				continue
			}
			select {
			case ch <- ev:
			case <-ctx.Done():
				return
			}
		}
	}()
	return ch, nil
}
