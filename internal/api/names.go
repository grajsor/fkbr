package api

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"
	"unicode/utf8"

	"golang.org/x/text/unicode/norm"
)

// File names are bytes, not text: Linux allows any byte but NUL and '/'.
// encoding/json writes invalid UTF-8 as U+FFFD, so a name like "caf\xe9"
// (Latin-1) would arrive as another name. Names and paths that are not
// valid UTF-8 therefore travel as namePrefix + base64 of their bytes. No
// real path starts with NUL, so the form is unambiguous, and every valid
// name stays exactly as it was (older builds read those unchanged).
const namePrefix = "\x00"

// EncodeName makes s safe to put in JSON.
func EncodeName(s string) string {
	if utf8.ValidString(s) && !strings.HasPrefix(s, namePrefix) {
		return s
	}
	return namePrefix + base64.RawStdEncoding.EncodeToString([]byte(s))
}

// DecodeName undoes EncodeName.
func DecodeName(s string) string {
	if !strings.HasPrefix(s, namePrefix) {
		return s
	}
	b, err := base64.RawStdEncoding.DecodeString(s[len(namePrefix):])
	if err != nil {
		return s
	}
	return string(b)
}

func encodeNames(ss []string) []string {
	if ss == nil {
		return nil
	}
	out := make([]string, len(ss))
	for i, s := range ss {
		out[i] = EncodeName(s)
	}
	return out
}

func decodeNames(ss []string) {
	for i, s := range ss {
		ss[i] = DecodeName(s)
	}
}

// The JSON methods below apply EncodeName/DecodeName to every field that
// holds a file name or path.

func (e Entry) MarshalJSON() ([]byte, error) {
	type plain Entry
	p := plain(e)
	p.Name, p.LinkTarget = EncodeName(e.Name), EncodeName(e.LinkTarget)
	return json.Marshal(p)
}

func (e *Entry) UnmarshalJSON(b []byte) error {
	type plain Entry
	var p plain
	if err := json.Unmarshal(b, &p); err != nil {
		return err
	}
	p.Name, p.LinkTarget = DecodeName(p.Name), DecodeName(p.LinkTarget)
	*e = Entry(p)
	return nil
}

func (r ListRequest) MarshalJSON() ([]byte, error) {
	type plain ListRequest
	p := plain(r)
	p.Path = EncodeName(r.Path)
	return json.Marshal(p)
}

func (r *ListRequest) UnmarshalJSON(b []byte) error {
	type plain ListRequest
	var p plain
	if err := json.Unmarshal(b, &p); err != nil {
		return err
	}
	p.Path = DecodeName(p.Path)
	*r = ListRequest(p)
	return nil
}

func (r ListResult) MarshalJSON() ([]byte, error) {
	type plain ListResult
	p := plain(r)
	p.Path = EncodeName(r.Path)
	return json.Marshal(p)
}

func (r *ListResult) UnmarshalJSON(b []byte) error {
	type plain ListResult
	var p plain
	if err := json.Unmarshal(b, &p); err != nil {
		return err
	}
	p.Path = DecodeName(p.Path)
	*r = ListResult(p)
	return nil
}

func (r TransferRequest) MarshalJSON() ([]byte, error) {
	type plain TransferRequest
	p := plain(r)
	p.Paths, p.DstDir, p.BaseDir = encodeNames(r.Paths), EncodeName(r.DstDir), EncodeName(r.BaseDir)
	return json.Marshal(p)
}

func (r *TransferRequest) UnmarshalJSON(b []byte) error {
	type plain TransferRequest
	var p plain
	if err := json.Unmarshal(b, &p); err != nil {
		return err
	}
	decodeNames(p.Paths)
	p.DstDir, p.BaseDir = DecodeName(p.DstDir), DecodeName(p.BaseDir)
	*r = TransferRequest(p)
	return nil
}

func (r MkdirRequest) MarshalJSON() ([]byte, error) {
	type plain MkdirRequest
	p := plain(r)
	p.Path = EncodeName(r.Path)
	return json.Marshal(p)
}

func (r *MkdirRequest) UnmarshalJSON(b []byte) error {
	type plain MkdirRequest
	var p plain
	if err := json.Unmarshal(b, &p); err != nil {
		return err
	}
	p.Path = DecodeName(p.Path)
	*r = MkdirRequest(p)
	return nil
}

func (r RemoveRequest) MarshalJSON() ([]byte, error) {
	type plain RemoveRequest
	p := plain(r)
	p.Paths, p.Base = encodeNames(r.Paths), EncodeName(r.Base)
	return json.Marshal(p)
}

func (r *RemoveRequest) UnmarshalJSON(b []byte) error {
	type plain RemoveRequest
	var p plain
	if err := json.Unmarshal(b, &p); err != nil {
		return err
	}
	decodeNames(p.Paths)
	p.Base = DecodeName(p.Base)
	*r = RemoveRequest(p)
	return nil
}

func (r ReadRequest) MarshalJSON() ([]byte, error) {
	type plain ReadRequest
	p := plain(r)
	p.Path = EncodeName(r.Path)
	return json.Marshal(p)
}

func (r *ReadRequest) UnmarshalJSON(b []byte) error {
	type plain ReadRequest
	var p plain
	if err := json.Unmarshal(b, &p); err != nil {
		return err
	}
	p.Path = DecodeName(p.Path)
	*r = ReadRequest(p)
	return nil
}

func (r WriteRequest) MarshalJSON() ([]byte, error) {
	type plain WriteRequest
	p := plain(r)
	p.Path = EncodeName(r.Path)
	return json.Marshal(p)
}

func (r *WriteRequest) UnmarshalJSON(b []byte) error {
	type plain WriteRequest
	var p plain
	if err := json.Unmarshal(b, &p); err != nil {
		return err
	}
	p.Path = DecodeName(p.Path)
	*r = WriteRequest(p)
	return nil
}

func (r SyncRequest) MarshalJSON() ([]byte, error) {
	type plain SyncRequest
	p := plain(r)
	p.SrcPath, p.DstPath = EncodeName(r.SrcPath), EncodeName(r.DstPath)
	return json.Marshal(p)
}

func (r *SyncRequest) UnmarshalJSON(b []byte) error {
	type plain SyncRequest
	var p plain
	if err := json.Unmarshal(b, &p); err != nil {
		return err
	}
	p.SrcPath, p.DstPath = DecodeName(p.SrcPath), DecodeName(p.DstPath)
	*r = SyncRequest(p)
	return nil
}

func (l SyncEntryList) MarshalJSON() ([]byte, error) {
	type plain SyncEntryList
	p := plain(l)
	p.Paths = encodeNames(l.Paths)
	return json.Marshal(p)
}

func (l *SyncEntryList) UnmarshalJSON(b []byte) error {
	type plain SyncEntryList
	var p plain
	if err := json.Unmarshal(b, &p); err != nil {
		return err
	}
	decodeNames(p.Paths)
	*l = SyncEntryList(p)
	return nil
}

// NameRules says which names a destination filesystem refuses or folds
// together, so a sync plan leaves out what cannot be stored there instead
// of planning it again on every run (the receiver refuses it anyway).
type NameRules struct {
	// FS is where the rules come from: "vfat", "exfat" or "casefold"
	// (an ext4 folder with chattr +F).
	FS string `json:"fs,omitempty"`
	// Fold: names that differ only in case are the same name; Form: also
	// names that differ only in Unicode normalization (NFC/NFD).
	Fold bool `json:"fold,omitempty"`
	Form bool `json:"form,omitempty"`
	// ASCII: only ASCII names (vfat mounted with iocharset=ascii); UTF8:
	// names must be valid UTF-8 (exFAT, vfat with utf8).
	ASCII bool `json:"ascii,omitempty"`
	UTF8  bool `json:"utf8,omitempty"`
	// NoSymlinks: symlinks cannot be stored (FAT, exFAT).
	NoSymlinks bool `json:"no_symlinks,omitempty"`
}

// fatForbidden are the characters FAT and exFAT refuse in names.
const fatForbidden = "\"*/:<>?\\|"

// Refuses says why name (one path component) cannot be stored, or "".
func (n NameRules) Refuses(name string) string {
	fat := n.FS == "vfat" || n.FS == "exfat"
	if fat {
		for i := 0; i < len(name); i++ {
			if c := name[i]; c < 0x20 || strings.IndexByte(fatForbidden, c) >= 0 {
				return fmt.Sprintf("character %q not allowed on %s", c, n.FS)
			}
		}
		if strings.HasSuffix(name, ".") || n.FS == "vfat" && strings.HasSuffix(name, " ") {
			return "a trailing dot or space is dropped on " + n.FS
		}
	}
	if n.ASCII {
		for i := 0; i < len(name); i++ {
			if name[i] >= 0x80 {
				return "only ASCII names on this " + n.FS + " mount (no utf8)"
			}
		}
	}
	if n.UTF8 && !utf8.ValidString(name) {
		return "not valid UTF-8, which " + n.FS + " needs"
	}
	return ""
}

// Key is what the filesystem compares names by: names with the same key
// are one name there.
func (n NameRules) Key(name string) string {
	if n.Form {
		name = norm.NFC.String(name)
	}
	if n.Fold {
		name = strings.ToLower(name)
	}
	return name
}

func (s ScanProgress) MarshalJSON() ([]byte, error) {
	type plain ScanProgress
	p := plain(s)
	p.Dir = EncodeName(s.Dir)
	return json.Marshal(p)
}

func (s *ScanProgress) UnmarshalJSON(b []byte) error {
	type plain ScanProgress
	var p plain
	if err := json.Unmarshal(b, &p); err != nil {
		return err
	}
	p.Dir = DecodeName(p.Dir)
	*s = ScanProgress(p)
	return nil
}
