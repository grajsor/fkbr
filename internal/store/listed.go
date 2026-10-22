package store

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
)

// Waiting for a file that another node will create.
//
// Never look such a name up before it exists. The failed lookup leaves a
// cached "missing" entry, and a Linux NFS client can keep it after the
// other node creates the file: it tells other clients' changes to a
// directory from its own by the directory's timestamps around its own
// operations, and when the other change lands in the same coarse server
// timestamp as one of this client's own (the refresh in fresh), it takes
// the change for its own and never looks the name up again. On the lab,
// 12 of 40 tight polls by name never saw a file another host renamed into
// place (6 of 30 for a new directory); listing the directory saw all of
// them. A listing also replaces stale entries for the names it returns.
//
// So wait by listing the directory, and open a name only once a listing
// has shown it.

// listNames lists dir afresh. If dir looks missing, which may be a stale
// entry in its parent, the parent is listed first and dir tried again.
func (m *Dir) listNames(dir string) ([]string, error) {
	m.fresh(dir)
	names, err := readNames(dir)
	if errors.Is(err, fs.ErrNotExist) {
		parent := filepath.Dir(dir)
		m.fresh(parent)
		if pn, perr := readNames(parent); perr == nil && slices.Contains(pn, filepath.Base(dir)) {
			names, err = readNames(dir)
		}
	}
	return names, err
}

func readNames(dir string) ([]string, error) {
	f, err := os.Open(dir)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return f.Readdirnames(-1)
}

// readListed reads dir/name once a fresh listing of dir shows it, and
// returns an error wrapping fs.ErrNotExist until then.
func (m *Dir) readListed(dir, name string) ([]byte, error) {
	ok, err := m.listed(dir, name)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, &fs.PathError{Op: "open", Path: filepath.Join(dir, name), Err: fs.ErrNotExist}
	}
	return readFile(filepath.Join(dir, name))
}

// listed reports whether a fresh listing of dir shows name; a missing dir
// counts as not listed.
func (m *Dir) listed(dir, name string) (bool, error) {
	names, err := m.listNames(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return slices.Contains(names, name), nil
}
