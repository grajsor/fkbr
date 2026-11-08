package remote

import (
	"context"
	"fmt"
	"testing"

	"fkbr/internal/api"
)

type countSubmit struct {
	api.Service
	paths int
}

func (c *countSubmit) Self() string { return "a" }

func (c *countSubmit) Submit(_ context.Context, req api.TransferRequest) (string, error) {
	c.paths = len(req.Paths)
	return "job1", nil
}

// A command line can name many paths (a shell glob of a big folder, up to
// the length of an argument list); the whole list is one request, and the
// socket takes it.
func TestSubmitTakesALongPathList(t *testing.T) {
	svc := &countSubmit{}
	c := serve(t, svc)
	paths := make([]string, 120000)
	for i := range paths {
		paths[i] = fmt.Sprintf("some/folder/file-%06d.dat", i) // about 4 MiB as JSON
	}
	if _, err := c.Submit(context.Background(), api.TransferRequest{SrcNode: "a", SrcRoot: "data", Paths: paths, DstNodes: []string{"b"}, DstRoot: "data"}); err != nil {
		t.Fatalf("a list of %d paths: %v", len(paths), err)
	}
	if svc.paths != len(paths) {
		t.Errorf("the service got %d paths, want %d", svc.paths, len(paths))
	}
}
