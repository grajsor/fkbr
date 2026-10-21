package nfsmailbox

import (
	"os"
	"sync/atomic"
	"time"
)

// Bulk writes to the share (chunks, snapshots, path lists) are synced now
// and then instead of only at the end. Unsynced data waits in the NFS
// client's queue, and every other call to the server (the heartbeat, a
// status, an ack) queues behind it: on a slow or lossy link a 128 MiB
// chunk held the heartbeat back for a minute, and peers took this node
// for offline while it worked. Syncing after about syncFor worth of data,
// at the share's measured write rate, bounds that wait.
const (
	syncMin = 4 << 20
	syncMax = 64 << 20
	syncFor = 2 * time.Second
)

// writeRate is bytes per second, shared by every writer of the process.
var writeRate atomic.Int64

func syncEvery() int64 {
	r := writeRate.Load() * int64(syncFor) / int64(time.Second)
	return min(max(r, syncMin), syncMax)
}

// syncedFile is an *os.File on the share written in bulk.
type syncedFile struct {
	*os.File
	unsynced int64
	since    time.Time
}

func newSyncedFile(f *os.File) *syncedFile { return &syncedFile{File: f, since: time.Now()} }

func (f *syncedFile) Write(p []byte) (int, error) {
	n, err := f.File.Write(p)
	f.unsynced += int64(n)
	if err == nil && f.unsynced >= syncEvery() {
		if err = f.File.Sync(); err == nil {
			// Writing plus syncing that much took this long: the rate the
			// share takes data at (page cache writes are near free).
			if d := time.Since(f.since); d > 0 {
				rate := f.unsynced * int64(time.Second) / int64(d)
				old := writeRate.Load()
				if old > 0 {
					rate = (old*3 + rate) / 4
				}
				writeRate.Store(rate)
			}
		}
		f.unsynced, f.since = 0, time.Now()
	}
	return n, err
}
