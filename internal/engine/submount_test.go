package engine

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"fkbr/internal/api"
)

// A copy into a folder on another filesystem than the root's (a drive
// mounted below it) arrives, whole and without errors: the staging is at
// the root's top, and rename across filesystems is EXDEV (review
// 2026-11-05: the job ended done with an error for every file). Mounts a
// tmpfs, so it runs only with FKBR_TEST_MOUNT=1 and sudo (or root).
func TestCopyToAnotherFilesystem(t *testing.T) {
	if os.Getenv("FKBR_TEST_MOUNT") == "" {
		t.Skip("set FKBR_TEST_MOUNT=1 (mounts a tmpfs with sudo)")
	}
	share := t.TempDir()
	master, mroot := node(t, share, "master")
	s1, sroot := node(t, share, "s1")
	mnt := filepath.Join(sroot, "mnt")
	os.MkdirAll(mnt, 0o755)
	cmd := []string{"mount", "-t", "tmpfs", "-o", "size=64m,mode=1777", "tmpfs", mnt}
	if os.Geteuid() != 0 {
		cmd = append([]string{"sudo", "-n"}, cmd...)
	}
	if out, err := exec.Command(cmd[0], cmd[1:]...).CombinedOutput(); err != nil {
		t.Skip("cannot mount:", string(out))
	}
	t.Cleanup(func() {
		um := []string{"umount", "-l", mnt}
		if os.Geteuid() != 0 {
			um = append([]string{"sudo", "-n"}, um...)
		}
		exec.Command(um[0], um[1:]...).Run()
	})
	makeTree(t, mroot, 5, 100<<10)
	runWorker(t, master)
	runWorker(t, s1)
	id, err := master.Submit(context.Background(), api.TransferRequest{SrcRoot: "data", Paths: []string{"tree"}, DstNodes: []string{"s1"}, DstRoot: "data", DstDir: "mnt/in"})
	if err != nil {
		t.Fatal(err)
	}
	j := waitJob(t, master, id, 30*time.Second)
	for _, p := range j.Participants {
		if p.State != api.JobDone || p.Errors != 0 {
			t.Fatalf("%s %s with %d errors: %v", p.Node, p.State, p.Errors, p.Problems)
		}
	}
	sameTree(t, filepath.Join(mroot, "tree"), filepath.Join(mnt, "in", "tree"))
	if left, _ := filepath.Glob(filepath.Join(mnt, "in", "tree", "*", ".fkbr-xdev-*")); len(left) != 0 {
		t.Errorf("temporary copies left: %v", left)
	}
}
