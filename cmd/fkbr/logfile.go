package main

import (
	"os"
	"sync"
)

// logFile is an append-only log that keeps at most two files of limit
// bytes: when the current one would grow past limit it becomes path.1.
// A TUI that runs the node logs here, since log lines would draw over the
// screen; the file stays after exit so problems can be looked at later.
type logFile struct {
	mu    sync.Mutex
	path  string
	limit int64
	f     *os.File
	size  int64
}

func openLogFile(path string, limit int64) (*logFile, error) {
	l := &logFile{path: path, limit: limit}
	if fi, err := os.Stat(path); err == nil && fi.Size() >= limit {
		os.Rename(path, path+".1")
	}
	if err := l.open(); err != nil {
		return nil, err
	}
	return l, nil
}

func (l *logFile) open() error {
	f, err := os.OpenFile(l.path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	fi, err := f.Stat()
	if err != nil {
		f.Close()
		return err
	}
	l.f, l.size = f, fi.Size()
	return nil
}

func (l *logFile) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.size > 0 && l.size+int64(len(p)) > l.limit {
		l.f.Close()
		os.Rename(l.path, l.path+".1")
		if err := l.open(); err != nil {
			return 0, err
		}
	}
	n, err := l.f.Write(p)
	l.size += int64(n)
	return n, err
}

func (l *logFile) Close() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.f.Close()
}
