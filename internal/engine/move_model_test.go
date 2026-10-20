package engine

import (
	"bytes"
	"context"
	"fmt"
	"math/rand"
	"os"
	"path"
	"path/filepath"
	"strings"
	"testing"

	"fkbr/internal/frontend/cli"
)

// The move model test: random trees moved with the real `mv` to random
// places, the dangerous ones included (onto or into the sources, through
// a symlink into them), with random conflict policies and destinations
// that already hold some of the files. The rule: no file's content is
// ever lost. It is at its source or at its destination (or both, when the
// move refused or kept the sources); when mv reports success, everything
// arrived and the sources are gone.
func TestMoveModel(t *testing.T) {
	share := t.TempDir()
	a, aroot := node(t, share, "a")
	b, broot := node(t, share, "b")
	runWorker(t, a)
	runWorker(t, b)
	ctx := context.Background()
	roots := map[string]string{"a": aroot, "b": broot}
	policies := []string{"skip", "overwrite", "overwrite-if-older"}
	for round := range 30 {
		rng := rand.New(rand.NewSource(int64(round*104729 + 3)))
		top := fmt.Sprintf("r%d", round)
		src := mTree{}
		mGen(rng, src, "", 0)
		if len(src) == 0 {
			continue
		}
		mWrite(t, filepath.Join(aroot, top, "src"), src)
		os.Symlink("src", filepath.Join(aroot, top, "link-to-src"))
		// What gets moved: the whole folder, or some entries in it.
		var items []string
		if rng.Intn(2) == 0 {
			items = []string{top + "/src"}
		} else {
			for _, p := range mSorted(src) {
				if !strings.Contains(p, "/") && rng.Intn(2) == 0 {
					items = append(items, top+"/src/"+p)
				}
			}
			if len(items) == 0 {
				items = []string{top + "/src/" + mSorted(src)[0]}
			}
		}
		dstNode := "b"
		dstDir := top + "/in"
		switch rng.Intn(6) {
		case 1:
			dstNode, dstDir = "a", top+"/elsewhere"
		case 2:
			dstNode, dstDir = "a", top+"/src" // onto/into itself
		case 3:
			dstNode, dstDir = "a", top+"/src/sub" // inside itself
		case 4:
			dstNode, dstDir = "a", top // the parent: onto itself for the folder
		case 5:
			dstNode, dstDir = "a", top+"/link-to-src" // into itself through a symlink
		}
		// Some of it already at the destination.
		if rng.Intn(2) == 0 {
			pre := mTree{}
			for _, p := range mSorted(src) {
				if !src[p].dir && rng.Intn(3) == 0 && mParentIsDir(pre, p) {
					pre[p] = mEntry{content: "old", mtime: mT0}
				} else if src[p].dir && mParentIsDir(pre, p) {
					pre[p] = mEntry{dir: true}
				}
			}
			if dstNode == "b" {
				mWrite(t, filepath.Join(broot, dstDir, "src"), pre)
			}
		}
		policy := policies[rng.Intn(len(policies))]
		what := fmt.Sprintf("round %d: mv %v to %s:%s (-conflict %s)", round, items, dstNode, dstDir, policy)

		// Every file's content before, by its path in the root.
		before := map[string]string{}
		for p, e := range src {
			if !e.dir {
				before[path.Join(top, "src", p)] = e.content
			}
		}
		args := []string{"-conflict", policy}
		for _, it := range items {
			args = append(args, "/"+it)
		}
		args = append(args, dstNode+":/"+dstDir)
		var out bytes.Buffer
		err := cli.Run(ctx, a, "mv", args, &out)

		// Where each moved file can be now: its source, or the destination.
		for p, content := range before {
			var item string
			for _, it := range items {
				if p == it || strings.HasPrefix(p, it+"/") {
					item = it
				}
			}
			if item == "" {
				if got, _ := os.ReadFile(filepath.Join(aroot, p)); string(got) != content {
					t.Errorf("%s: %s was not moved but changed: %q", what, p, got)
				}
				continue
			}
			land := path.Join(dstDir, path.Base(item), strings.TrimPrefix(strings.TrimPrefix(p, item), "/"))
			atSrc, _ := os.ReadFile(filepath.Join(aroot, p))
			atDst, _ := os.ReadFile(filepath.Join(roots[dstNode], land))
			if string(atSrc) != content && string(atDst) != content {
				t.Errorf("%s: %s LOST (source %q, destination %s %q, mv: %v)", what, p, atSrc, land, atDst, err)
			}
			if err == nil && (string(atDst) != content || exists(aroot, p)) {
				t.Errorf("%s: mv succeeded but %s: destination %q, source still there %v", what, p, atDst, exists(aroot, p))
			}
		}
	}
}
