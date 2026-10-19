package bundle

import (
	"io"
	"runtime"
	"sync"

	"github.com/klauspost/compress/zstd"
)

// Chunks and snapshots are compressed with zstd at its fastest level, which
// already outruns the share. A 1 MiB window costs about 1% of ratio and
// keeps an encoder near 9 MB; two threads are most of the speed-up a
// small CPU gets from more.
const zstdWindow = 1 << 20

// NewEncoder returns a zstd writer for chunks and snapshots. Reuse it for
// the next stream with Reset.
func NewEncoder(w io.Writer) (*zstd.Encoder, error) {
	return zstd.NewWriter(w,
		zstd.WithEncoderLevel(zstd.SpeedFastest),
		zstd.WithWindowSize(zstdWindow),
		zstd.WithEncoderConcurrency(min(runtime.GOMAXPROCS(0), 2)))
}

// Synchronous decoders hold no goroutines, so idle ones can sit in a pool.
// The window cap rejects frames our encoder never writes before they can
// allocate much.
var decoders = sync.Pool{New: func() any {
	d, _ := zstd.NewReader(nil,
		zstd.WithDecoderConcurrency(1),
		zstd.WithDecoderMaxWindow(32<<20))
	return d
}}

// Decoder reads one zstd stream. Close returns it to the pool.
type Decoder struct{ d *zstd.Decoder }

func NewDecoder(r io.Reader) (*Decoder, error) {
	d := decoders.Get().(*zstd.Decoder)
	if err := d.Reset(r); err != nil {
		d.Reset(nil)
		decoders.Put(d)
		return nil, err
	}
	return &Decoder{d}, nil
}

func (d *Decoder) Read(p []byte) (int, error) { return d.d.Read(p) }

// Close releases the decoder; it does not close the underlying reader.
func (d *Decoder) Close() {
	if d.d != nil {
		d.d.Reset(nil)
		decoders.Put(d.d)
		d.d = nil
	}
}
