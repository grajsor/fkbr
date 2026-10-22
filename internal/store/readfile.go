package store

import (
	"errors"
	"fmt"
	"io"
	"os"
)

// maxShareFile caps what is read whole from the share (requests, replies,
// job records, statuses, node entries). The largest real ones are about
// 2 MiB; a huge file put there must not make a node run out of memory
// (security audit 2026-10-22).
var maxShareFile int64 = 64 << 20

var errTooBig = errors.New("file on the share is too big")

// readFile is os.ReadFile for files on the share, refusing ones over
// maxShareFile.
func readFile(p string) ([]byte, error) {
	f, err := os.Open(p)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, maxShareFile+1))
	if err != nil {
		return nil, err
	}
	if int64(len(b)) > maxShareFile {
		return nil, fmt.Errorf("%s: %w (over %d MiB)", p, errTooBig, maxShareFile>>20)
	}
	return b, nil
}
