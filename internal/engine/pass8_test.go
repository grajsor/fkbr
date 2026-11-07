package engine

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"fkbr/internal/api"
)

// A source whose job succeeded but whose done status could not be stored
// (over ssh, the link down; a soft mount's EIO) used to have its chunks and
// acks removed already and made one try: the job read as running for good.
// Now the end is recorded first, retried, and the data goes after it.
func TestSourceDoneEndLostRetries(t *testing.T) {
	share := t.TempDir()
	mcfg := nodeCfg(t, share, "master")
	master, fs := buildFaulty(t, mcfg)
	fs.endsLost = true
	s1, sroot := node(t, share, "s1")
	makeTree(t, mcfg.Roots[0].Path, 5, 300<<10)
	runWorker(t, master)
	runWorker(t, s1)
	id, err := master.Submit(context.Background(), api.TransferRequest{SrcRoot: "data", Paths: []string{"tree"}, DstNodes: []string{"s1"}, DstRoot: "data"})
	if err != nil {
		t.Fatal(err)
	}
	waitUntil(t, func() bool { return fs.refused() > 0 }, "the source's done refused")
	fs.recordEnds()
	if j := waitJob(t, master, id, 30*time.Second); j.State != api.JobDone {
		t.Fatalf("state %s: %+v", j.State, j.Participants)
	}
	waitUntil(t, func() bool {
		_, err := fs.Dir.ChunkMeta(context.Background(), "master", id, 0)
		return err != nil
	}, "chunks removed once the end is recorded")
	sameTree(t, filepath.Join(mcfg.Roots[0].Path, "tree"), filepath.Join(sroot, "tree"))
}

// A job that fails while its end cannot be recorded for the whole budget
// keeps its staged data "for the next run". The request must stay claimed
// for that run: parked in failed/ (or removed) nothing would ever come back
// for the chunks or the job.
func TestEndLostKeepsTheRequest(t *testing.T) {
	shortTransient(t, time.Second)
	share := t.TempDir()
	mcfg := nodeCfg(t, share, "master")
	master, fs := buildFaulty(t, mcfg)
	fs.failWriteAt, fs.endsLost = 2, true
	s1, sfs := buildFaulty(t, nodeCfg(t, share, "s1"))
	sfs.openDelay = 3 * time.Second
	makeTree(t, mcfg.Roots[0].Path, 10, 600<<10)
	runWorker(t, master)
	runWorker(t, s1)
	_, err := master.Submit(context.Background(), api.TransferRequest{SrcRoot: "data", Paths: []string{"tree"}, DstNodes: []string{"s1"}, DstRoot: "data"})
	if err != nil {
		t.Fatal(err)
	}
	waitUntil(t, func() bool { return fs.refused() > 1 }, "the end refused again after a retry")
	time.Sleep(4 * time.Second) // the budget (1 s) is spent, the request would be finished by now
	cur, _ := filepath.Glob(filepath.Join(share, "inbox", "master", "cur", "*.json"))
	failed, _ := filepath.Glob(filepath.Join(share, "inbox", "master", "failed", "*.json"))
	if len(failed) != 0 {
		t.Fatalf("the request was parked in failed/ though the job's end was never recorded: %v", failed)
	}
	if len(cur) != 1 {
		t.Fatalf("the request is not left claimed in cur/: %v", cur)
	}
}

// A job that fails in its setup checks (here: a source path that is not
// there) while its end cannot be recorded must leave the request claimed
// too, like any other lost end: parked in failed/ it would read as running
// for good, with nothing to claim it.
func TestSetupFailureWithLostEndKeepsTheRequest(t *testing.T) {
	shortTransient(t, time.Second)
	share := t.TempDir()
	mcfg := nodeCfg(t, share, "master")
	master, fs := buildFaulty(t, mcfg)
	fs.endsLost = true
	other, _ := node(t, share, "other")
	runWorker(t, master)
	runWorker(t, other)
	if _, err := other.Submit(context.Background(), api.TransferRequest{SrcNode: "master", SrcRoot: "data", Paths: []string{"gone"}, DstNodes: []string{"other"}, DstRoot: "data"}); err != nil {
		t.Fatal(err)
	}
	waitUntil(t, func() bool { return fs.refused() > 1 }, "the end refused again after a retry")
	time.Sleep(4 * time.Second) // the budget (1 s) is spent
	cur, _ := filepath.Glob(filepath.Join(share, "inbox", "master", "cur", "*.json"))
	failed, _ := filepath.Glob(filepath.Join(share, "inbox", "master", "failed", "*.json"))
	if len(failed) != 0 || len(cur) != 1 {
		t.Fatalf("request: cur %v, failed %v; want it left claimed", cur, failed)
	}
}
