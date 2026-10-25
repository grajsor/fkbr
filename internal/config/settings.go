package config

import (
	"bytes"
	"errors"
	"fmt"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

// The TUI's settings dialog (F11) changes keys of the config file
// (WritePath) one by one: the rest of the file, its comments and its ssh
// list stay as they are.

// Setting is a key of the config file ("transfer.max_jobs") and its new
// value: a string, int, bool or []string; nil removes the key (its
// default applies again).
type Setting struct {
	Key   string
	Value any
}

// SaveSettings sets the keys in the config file at p, creating the file
// if need be, and refuses a result the node could not start with.
func SaveSettings(p string, set []Setting) error {
	if p == "" {
		return errors.New("no config file to keep settings in")
	}
	doc, err := readSaved(p)
	if err != nil {
		return err
	}
	if doc == nil {
		doc = newSaved(p)
	}
	for _, s := range set {
		if err := setKey(doc.Content[0], strings.Split(s.Key, "."), s.Value); err != nil {
			return fmt.Errorf("%s: %w", s.Key, err)
		}
	}
	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(doc); err != nil {
		return err
	}
	enc.Close()
	c, err := decode(buf.Bytes())
	if err != nil {
		return err
	}
	if err := CheckSettings(c); err != nil {
		return err
	}
	return writeSaved(p, doc)
}

// setKey sets (or, for nil, removes) the key at path in mapping m. A
// mapping left empty by a removal goes too.
func setKey(m *yaml.Node, path []string, v any) error {
	if m.Kind != yaml.MappingNode {
		return errors.New("not a mapping")
	}
	at := -1
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == path[0] {
			at = i
		}
	}
	if len(path) > 1 {
		if at < 0 {
			if v == nil {
				return nil
			}
			m.Content = append(m.Content, &yaml.Node{Kind: yaml.ScalarNode, Value: path[0]}, &yaml.Node{Kind: yaml.MappingNode})
			at = len(m.Content) - 2
		}
		sub := m.Content[at+1]
		if sub.Kind == yaml.ScalarNode && sub.Tag == "!!null" { // "transfer:" with nothing under it
			sub.Kind, sub.Tag, sub.Value = yaml.MappingNode, "", ""
		}
		if err := setKey(sub, path[1:], v); err != nil {
			return err
		}
		if len(sub.Content) == 0 && v == nil {
			m.Content = append(m.Content[:at], m.Content[at+2:]...)
		}
		return nil
	}
	if v == nil {
		if at >= 0 {
			m.Content = append(m.Content[:at], m.Content[at+2:]...)
		}
		return nil
	}
	var n yaml.Node
	if err := n.Encode(v); err != nil {
		return err
	}
	if n.Kind == yaml.SequenceNode {
		n.Style = yaml.FlowStyle
	}
	if at < 0 {
		m.Content = append(m.Content, &yaml.Node{Kind: yaml.ScalarNode, Value: path[0]}, &n)
		return nil
	}
	old := m.Content[at+1]
	n.HeadComment, n.LineComment, n.FootComment = old.HeadComment, old.LineComment, old.FootComment
	m.Content[at+1] = &n
	return nil
}

// CheckSettings validates what a config file says as a node would use
// it, with its defaults, without the parts only a start supplies (the
// node name defaults to the host's, the share to none).
func CheckSettings(c *Config) error {
	v := *c
	v.Transfer, v.Sync = c.Transfer, c.Sync
	if v.NodeID == "" {
		v.NodeID = "check"
	}
	if v.NFSRoot != "" && !filepath.IsAbs(v.NFSRoot) {
		return fmt.Errorf("nfs_root %q: must be an absolute path", v.NFSRoot)
	}
	v.Home = v.NFSRoot == ""
	if lr := v.LocalRoot; lr != "" && lr != "none" && !filepath.IsAbs(lr) {
		return fmt.Errorf("local_root %q: an absolute path, or none", lr)
	}
	v.applyDefaults()
	return v.Validate()
}
