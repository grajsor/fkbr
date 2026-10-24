package engine

import (
	"bytes"
	"io"
	"io/fs"
	"os"
	"sync"
)

// Reading many small files one after another waits on the disk for each
// of them (a cold cache, a busy or network disk). So the sender's walk runs
// ahead of the bundle writer, and small files are read by a few workers in
// the meantime; the writer still takes the entries strictly in walk order,
// so the bundle is the same as without read-ahead (a resumed sender must
// regenerate identical chunks).
const (
	prefetchMaxFile = 1 << 20  // larger files are streamed when their turn comes
	prefetchBudget  = 64 << 20 // file bytes read ahead and not yet bundled
	prefetchWorkers = 8
)

// prefetcher bounds the read-ahead by bytes and by concurrent reads.
type prefetcher struct {
	mu      sync.Mutex
	cond    *sync.Cond
	held    int64
	workers chan struct{}
}

func newPrefetcher() *prefetcher {
	p := &prefetcher{workers: make(chan struct{}, prefetchWorkers)}
	p.cond = sync.NewCond(&p.mu)
	return p
}

// prefetched is one file being read ahead.
type prefetched struct {
	size int64
	done chan struct{}
	data []byte
	fi   fs.FileInfo
	open error // the file could not be opened
	read error // reading failed part-way: data is what was read
}

// start reads a file of size bytes in the background, waiting first until
// the budget allows it. stop aborts the wait (returns nil).
func (p *prefetcher) start(size int64, open func() (*os.File, error), stop <-chan struct{}) *prefetched {
	p.mu.Lock()
	for p.held > 0 && p.held+size > prefetchBudget {
		select {
		case <-stop:
			p.mu.Unlock()
			return nil
		default:
		}
		p.cond.Wait()
	}
	p.held += size
	p.mu.Unlock()
	f := &prefetched{size: size, done: make(chan struct{})}
	go func() {
		defer close(f.done)
		p.workers <- struct{}{}
		defer func() { <-p.workers }()
		fh, err := open()
		if err != nil {
			f.open = err
			return
		}
		defer fh.Close()
		buf := bytes.NewBuffer(make([]byte, 0, size))
		_, f.read = io.Copy(buf, fh)
		f.data = buf.Bytes()
		f.fi, _ = fh.Stat() // after reading, so a change while reading shows
	}()
	return f
}

// release returns a bundled file's bytes to the budget.
func (p *prefetcher) release(f *prefetched) {
	p.mu.Lock()
	p.held -= f.size
	p.mu.Unlock()
	p.cond.Broadcast()
}

// wake lets a walker blocked in start notice that it should stop.
func (p *prefetcher) wake() { p.cond.Broadcast() }

// file waits for the read and returns it as a bundle.File.
func (f *prefetched) file() (*memFile, error) {
	<-f.done
	if f.open != nil {
		return nil, f.open
	}
	return &memFile{r: bytes.NewReader(f.data), err: f.read, fi: f.fi}, nil
}

// memFile serves a file read ahead; a read error comes after its data,
// as it would from the file itself.
type memFile struct {
	r   *bytes.Reader
	err error
	fi  fs.FileInfo
}

func (m *memFile) Read(p []byte) (int, error) {
	n, err := m.r.Read(p)
	if err == io.EOF && m.err != nil {
		return n, m.err
	}
	return n, err
}

func (m *memFile) Close() error { return nil }

func (m *memFile) Stat() (fs.FileInfo, error) {
	if m.fi == nil {
		return nil, fs.ErrInvalid
	}
	return m.fi, nil
}

// readAhead reads r in a goroutine, up to readAheadBlocks blocks ahead of
// the caller, so a target reads the next part of a chunk from the share
// while it writes the files of the previous part to its disk, and a
// source reads the next part of a big file while it packs the last.
func readAhead(r io.Reader) io.ReadCloser {
	ra := &aheadReader{blocks: make(chan []byte, readAheadBlocks), stop: make(chan struct{})}
	go func() {
		defer close(ra.blocks)
		for {
			buf := getBlock()
			n, err := io.ReadFull(r, buf)
			if n == 0 {
				putBlock(buf)
			} else {
				select {
				case ra.blocks <- buf[:n]:
				case <-ra.stop:
					putBlock(buf)
					return
				}
			}
			if err != nil {
				if err != io.EOF && err != io.ErrUnexpectedEOF {
					ra.err = err
				}
				return
			}
		}
	}()
	return ra
}

const (
	readAheadBlock  = 1 << 20
	readAheadBlocks = 16
)

// Blocks are reused: a GiB read ahead is not a GiB of garbage.
var blocks = sync.Pool{New: func() any { b := make([]byte, readAheadBlock); return &b }}

func getBlock() []byte { return *blocks.Get().(*[]byte) }

func putBlock(b []byte) {
	b = b[:cap(b)]
	blocks.Put(&b)
}

type aheadReader struct {
	blocks chan []byte
	block  []byte // the block cur is in, given back once read
	cur    []byte
	err    error // set before blocks is closed
	stop   chan struct{}
	once   sync.Once
}

func (a *aheadReader) Read(p []byte) (int, error) {
	for len(a.cur) == 0 {
		if a.block != nil {
			putBlock(a.block)
			a.block = nil
		}
		b, ok := <-a.blocks
		if !ok {
			if a.err != nil {
				return 0, a.err
			}
			return 0, io.EOF
		}
		a.block, a.cur = b, b
	}
	n := copy(p, a.cur)
	a.cur = a.cur[n:]
	return n, nil
}

// Close stops the reader goroutine; it does not close the source. Read
// must not be called after it.
func (a *aheadReader) Close() error {
	a.once.Do(func() { close(a.stop) })
	for b := range a.blocks { // let the goroutine finish
		putBlock(b)
	}
	if a.block != nil {
		putBlock(a.block)
		a.block, a.cur = nil, nil
	}
	return nil
}

// aheadFile is a big source file read ahead (readAhead). Stat is the
// file's, so a change while the bundle read it still shows.
type aheadFile struct {
	io.ReadCloser
	f *os.File
}

func readFileAhead(f *os.File) *aheadFile { return &aheadFile{readAhead(f), f} }

func (a *aheadFile) Stat() (fs.FileInfo, error) { return a.f.Stat() }

func (a *aheadFile) Close() error {
	a.ReadCloser.Close()
	return a.f.Close()
}
