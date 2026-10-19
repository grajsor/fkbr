// Package atomicfile publishes files so that readers either see nothing or
// the complete content: write to a temp file, fsync, close, then rename.
//
// On NFS this relies on close-to-open consistency: the writer closes (which
// flushes) before the rename makes the name visible. tmpDir must be on the
// same filesystem as dest so the rename is atomic.
package atomicfile

import (
	"os"
	"path/filepath"
)

// Write atomically creates or replaces dest with data.
func Write(tmpDir, dest string, data []byte, perm os.FileMode) (err error) {
	f, err := os.CreateTemp(tmpDir, "."+filepath.Base(dest)+".*.tmp")
	if err != nil {
		return err
	}
	defer func() {
		if err != nil {
			f.Close()
			os.Remove(f.Name())
		}
	}()
	if _, err = f.Write(data); err != nil {
		return err
	}
	if err = f.Chmod(perm); err != nil {
		return err
	}
	if err = f.Sync(); err != nil {
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	return os.Rename(f.Name(), dest)
}
