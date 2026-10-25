package api

import (
	"encoding/json"
	"testing"
)

func TestNamesSurviveJSON(t *testing.T) {
	names := []string{"plain", "ümlaut ✓", "caf\xe9", "\xff", "a/b\xfe/c", "", "\x00odd"}
	for _, n := range names {
		if got := DecodeName(EncodeName(n)); got != n {
			t.Errorf("%q: round trip gave %q", n, got)
		}
	}
	if EncodeName("ümlaut ✓") != "ümlaut ✓" {
		t.Error("valid UTF-8 must travel unchanged")
	}
	in := RemoveRequest{Root: "r", Paths: []string{"caf\xe9", "ok"}, Base: "b\xe9"}
	b, err := json.Marshal(in)
	if err != nil {
		t.Fatal(err)
	}
	var out RemoveRequest
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatal(err)
	}
	if out.Paths[0] != "caf\xe9" || out.Paths[1] != "ok" || out.Base != "b\xe9" || in.Paths[0] != "caf\xe9" {
		t.Fatalf("got %q / %q, input changed: %q", out.Paths, out.Base, in.Paths)
	}
	sp := ScanProgress{Node: "n", Dir: "a/caf\xe9"}
	b, _ = json.Marshal(sp)
	var sp2 ScanProgress
	if err := json.Unmarshal(b, &sp2); err != nil || sp2 != sp {
		t.Fatalf("scan progress: %+v %v", sp2, err)
	}
	e := Entry{Name: "x\xe9", LinkTarget: "y\xe9", Type: TypeSymlink}
	b, _ = json.Marshal(e)
	var e2 Entry
	if err := json.Unmarshal(b, &e2); err != nil || e2 != e {
		t.Fatalf("entry: %+v %v", e2, err)
	}
	w := WriteRequest{Root: "r", Path: "d/caf\xe9.sh", Data: []byte("x\n"), Size: 3}
	b, _ = json.Marshal(w)
	var w2 WriteRequest
	if err := json.Unmarshal(b, &w2); err != nil || w2.Path != w.Path || string(w2.Data) != "x\n" || w2.Size != 3 {
		t.Fatalf("write request: %+v %v", w2, err)
	}
}
