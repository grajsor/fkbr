package config

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"

	"gopkg.in/yaml.v3"

	"fkbr/internal/atomicfile"
	"fkbr/internal/product"
)

// The ssh hosts linked from the TUI are kept in the ssh list of the config
// file (WritePath): a node started without a share links them. Only that
// list is changed; other keys and comments stay as they are.

// SavedHosts reads the ssh list of the config file at p; none if there is
// no file.
func SavedHosts(p string) ([]string, error) {
	doc, err := readSaved(p)
	if err != nil || doc == nil {
		return nil, err
	}
	var v struct {
		SSH []string `yaml:"ssh"`
	}
	if err := doc.Decode(&v); err != nil {
		return nil, fmt.Errorf("%s: %w", p, err)
	}
	return v.SSH, nil
}

// SaveHost adds host to the ssh list of the config file at p. Not to a
// file that names a share: it could not be read with both.
func SaveHost(p, host string) error {
	doc, err := readSaved(p)
	if err != nil {
		return err
	}
	var v struct {
		NFSRoot string `yaml:"nfs_root"`
	}
	if doc != nil && doc.Decode(&v) == nil && v.NFSRoot != "" {
		return fmt.Errorf("%s sets nfs_root, and a config file has a share or an ssh list, not both", p)
	}
	return editSaved(p, func(hosts []string) []string {
		if slices.Contains(hosts, host) {
			return hosts
		}
		return append(hosts, host)
	})
}

// ForgetHost removes host from the ssh list of the config file at p.
func ForgetHost(p, host string) error {
	return editSaved(p, func(hosts []string) []string {
		return slices.DeleteFunc(hosts, func(h string) bool { return h == host })
	})
}

func readSaved(p string) (*yaml.Node, error) {
	b, err := os.ReadFile(p)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var doc yaml.Node
	if err := yaml.Unmarshal(b, &doc); err != nil {
		return nil, fmt.Errorf("%s: %w", p, err)
	}
	if len(doc.Content) == 0 {
		return nil, nil
	}
	if doc.Content[0].Kind != yaml.MappingNode {
		return nil, fmt.Errorf("%s: not a YAML mapping", p)
	}
	return &doc, nil
}

// editSaved rewrites the ssh list of the config file at p with edit,
// keeping the rest of the file and its mode. A new file gets mode 0600
// and a comment saying what it is.
func editSaved(p string, edit func([]string) []string) error {
	if p == "" {
		return errors.New("no config file to keep ssh hosts in")
	}
	doc, err := readSaved(p)
	if err != nil {
		return err
	}
	if doc == nil {
		doc = newSaved(p)
	}
	top := doc.Content[0]
	var seq *yaml.Node
	for i := 0; i+1 < len(top.Content); i += 2 {
		if top.Content[i].Value == "ssh" {
			seq = top.Content[i+1]
		}
	}
	if seq == nil {
		seq = &yaml.Node{Kind: yaml.SequenceNode}
		top.Content = append(top.Content, &yaml.Node{Kind: yaml.ScalarNode, Value: "ssh"}, seq)
	}
	var hosts []string
	if err := seq.Decode(&hosts); err != nil {
		return fmt.Errorf("%s: ssh: %w", p, err)
	}
	hosts = edit(hosts)
	seq.Kind, seq.Tag, seq.Content = yaml.SequenceNode, "", nil
	for _, h := range hosts {
		seq.Content = append(seq.Content, &yaml.Node{Kind: yaml.ScalarNode, Value: h})
	}
	return writeSaved(p, doc)
}

// newSaved is the document of a config file not there yet (or of
// comments only, which are kept).
func newSaved(p string) *yaml.Node {
	head := product.Expand("{name} config: F11 in the TUI changes settings here, F7 and F8 in a node list the ssh hosts")
	if b, err := os.ReadFile(p); err == nil && len(bytes.TrimSpace(b)) > 0 {
		head = string(bytes.TrimSpace(b))
	}
	return &yaml.Node{Kind: yaml.DocumentNode, Content: []*yaml.Node{{Kind: yaml.MappingNode, HeadComment: head}}}
}

// writeSaved writes doc to the config file at p, keeping its mode; a new
// file gets 0600. Not through a symlink planted in its place.
func writeSaved(p string, doc *yaml.Node) error {
	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(doc); err != nil {
		return err
	}
	enc.Close()
	mode := fs.FileMode(0o600)
	if fi, err := os.Lstat(p); err == nil {
		if fi.Mode()&fs.ModeSymlink != 0 {
			return fmt.Errorf("%s is a symlink; not writing through it", p)
		}
		mode = fi.Mode().Perm()
	}
	return atomicfile.Write(filepath.Dir(p), p, buf.Bytes(), mode)
}
