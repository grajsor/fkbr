package cli

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"fkbr/internal/api"
)

// filesClient serves one node "a" with root "files" holding a few
// entries, and records what the commands ask for.
type filesClient struct {
	api.Client
	mkdirs  []api.MkdirRequest
	removes []api.RemoveRequest
	submits []api.TransferRequest
	job     api.Job
}

func (c *filesClient) Self() string { return "a" }
func (c *filesClient) Nodes(context.Context) ([]api.Node, error) {
	r := []api.Root{{ID: "files", Writable: true}}
	return []api.Node{{NodeInfo: api.NodeInfo{ID: "a", Roots: r}, Online: true}, {NodeInfo: api.NodeInfo{ID: "b", Roots: r}, Online: true}}, nil
}
func (c *filesClient) List(_ context.Context, r api.ListRequest) (*api.ListResult, error) {
	all := map[string][]api.Entry{
		".":   {{Name: "dir", Type: api.TypeDir}, {Name: "f.txt", Type: api.TypeFile}},
		"dir": {{Name: "x", Type: api.TypeFile}},
	}
	es := all[r.Path]
	return &api.ListResult{Path: r.Path, Entries: es, Total: len(es)}, nil
}
func (c *filesClient) Mkdir(_ context.Context, r api.MkdirRequest) error {
	c.mkdirs = append(c.mkdirs, r)
	return nil
}
func (c *filesClient) Remove(_ context.Context, r api.RemoveRequest) (*api.RemoveResult, error) {
	c.removes = append(c.removes, r)
	return &api.RemoveResult{Removed: len(r.Paths)}, nil
}
func (c *filesClient) Submit(_ context.Context, r api.TransferRequest) (string, error) {
	c.submits = append(c.submits, r)
	return "job1", nil
}
func (c *filesClient) Job(context.Context, string) (*api.Job, error) { j := c.job; return &j, nil }

func run(t *testing.T, c api.Client, args ...string) (string, error) {
	t.Helper()
	var out bytes.Buffer
	err := Run(context.Background(), c, args[0], args[1:], &out)
	return out.String(), err
}

func TestMkdirRm(t *testing.T) {
	c := &filesClient{}
	if _, err := run(t, c, "mkdir", "/new/deep", "b:/other"); err != nil {
		t.Fatal(err)
	}
	if len(c.mkdirs) != 2 || c.mkdirs[0].Path != "new/deep" || c.mkdirs[1].Node != "b" {
		t.Errorf("mkdir: %+v", c.mkdirs)
	}
	if _, err := run(t, c, "rm", "/dir"); err == nil || !strings.Contains(err.Error(), "-r") {
		t.Errorf("rm of a folder without -r: %v", err)
	}
	if _, err := run(t, c, "rm", "/nope"); err == nil || !strings.Contains(err.Error(), "nope") {
		t.Errorf("rm of a missing path: %v", err)
	}
	if len(c.removes) != 0 {
		t.Fatalf("refused rm removed: %+v", c.removes)
	}
	out, err := run(t, c, "rm", "-r", "/dir", "/f.txt", "/dir/x")
	if err != nil || len(c.removes) != 1 || strings.Join(c.removes[0].Paths, " ") != "dir f.txt dir/x" {
		t.Errorf("rm -r: %v %+v", err, c.removes)
	}
	if !strings.Contains(out, "removed 3") {
		t.Errorf("rm output: %q", out)
	}
}

func TestMv(t *testing.T) {
	c := &filesClient{job: api.Job{ID: "job1", State: api.JobDone, Participants: []api.JobStatus{{Node: "b", Role: api.RoleTarget, State: api.JobDone}}}}
	if _, err := run(t, c, "mv", "/f.txt", "b:/in"); err != nil {
		t.Fatal(err)
	}
	if len(c.submits) != 1 || len(c.removes) != 1 || c.removes[0].Paths[0] != "f.txt" {
		t.Fatalf("mv: submits %+v removes %+v", c.submits, c.removes)
	}
	if !c.submits[0].Move {
		t.Error("mv does not mark its transfer as a move")
	}
	if _, err := run(t, &filesClient{}, "mv", "/", "b:/x"); err == nil {
		t.Error("mv of a whole root accepted")
	}
	// Anything skipped keeps the sources.
	c = &filesClient{job: api.Job{ID: "job1", State: api.JobDone, Participants: []api.JobStatus{{Node: "b", Role: api.RoleTarget, State: api.JobDone, Skipped: 1}}}}
	if _, err := run(t, c, "mv", "/f.txt", "b:/in"); err == nil || len(c.removes) != 0 {
		t.Errorf("mv with a skip: %v, removes %+v", err, c.removes)
	}
	c = &filesClient{}
	if _, err := run(t, c, "mv", "-exclude", "*.tmp", "/dir", "b:/in"); err == nil || len(c.submits) != 0 {
		t.Errorf("mv with an exclude: %v", err)
	}
}

// "cp /x @all" (no colon) copied into a folder "@all" here; so would
// "cp /x b" with b a node. Refused, with the right form; "/b" is a folder.
func TestDestinationNamesNodes(t *testing.T) {
	for _, dst := range []string{"@all", "@web", "b", "a,b"} {
		for _, cmd := range []string{"cp", "mv", "sync"} {
			c := &filesClient{job: api.Job{ID: "job1", State: api.JobDone}} // ends, if wrongly submitted
			_, err := run(t, c, cmd, "/f.txt", dst)
			if err == nil || !strings.Contains(err.Error(), dst+":/dir") || len(c.submits) != 0 {
				t.Errorf("%s /f.txt %s: %v, submits %d", cmd, dst, err, len(c.submits))
			}
		}
	}
	c := &filesClient{job: api.Job{ID: "job1", State: api.JobDone}}
	for _, dst := range []string{"/b", "inbox", "/@all"} {
		if _, err := run(t, c, "cp", "/f.txt", dst); err != nil {
			t.Errorf("cp /f.txt %s: %v", dst, err)
		}
	}
	if len(c.submits) != 3 || c.submits[0].DstDir != "b" || c.submits[0].DstNodes[0] != "a" {
		t.Errorf("folders here: %+v", c.submits)
	}
}
