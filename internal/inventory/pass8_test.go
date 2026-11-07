package inventory

import (
	"bytes"
	"io"
	"math"
	"strings"
	"testing"

	"fkbr/internal/bundle"
)

// Eighth review pass (2026-11-07, testplan.md 26).

func zstdLines(t *testing.T, lines ...string) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw, err := bundle.NewEncoder(&buf)
	if err != nil {
		t.Fatal(err)
	}
	for _, l := range lines {
		zw.Write([]byte(l + "\n"))
	}
	zw.Close()
	return buf.Bytes()
}

// The planner multiplies MtimeStep by 1e9 and clamps mtimes to MtimeMin/Max;
// a header no scan writes (a damaged blob) must not reach it.
func TestReaderRefusesBadHeaderNumbers(t *testing.T) {
	for name, h := range map[string]Header{
		"huge step":     {Node: "n", MtimeStep: math.MaxInt64},
		"negative step": {Node: "n", MtimeStep: -2},
		"min above max": {Node: "n", MtimeMin: 100, MtimeMax: 50},
	} {
		var buf bytes.Buffer
		w, err := NewWriter(&buf, h)
		if err != nil {
			t.Fatal(err)
		}
		w.Close()
		if _, err := NewReader(&buf); err == nil {
			t.Errorf("%s: header accepted", name)
		}
	}
	// A FAT target and a plain one are fine.
	for _, h := range []Header{{Node: "n"}, {Node: "n", MtimeStep: 2, MtimeMin: 1, MtimeMax: 2}} {
		var buf bytes.Buffer
		w, _ := NewWriter(&buf, h)
		w.Close()
		if _, err := NewReader(&buf); err != nil {
			t.Errorf("header %+v refused: %v", h, err)
		}
	}
}

// Every way a snapshot can fail to open ends in an error, not a panic or a
// zeroed header that plans as an exact target.
func TestNewReaderErrors(t *testing.T) {
	long := `{"p":"` + strings.Repeat("x", 2<<20) + `","t":"f"}`
	for name, c := range map[string]struct {
		in   []byte
		want string
	}{
		"empty":           {nil, "snapshot"},
		"not zstd":        {[]byte("hello, this is not a snapshot"), "snapshot"},
		"zstd, no lines":  {zstdLines(t), "missing header"},
		"header not json": {zstdLines(t, "not json"), "header"},
		"version 1":       {zstdLines(t, `{"snapshot":1,"node":"n"}`), "version 1"},
		"version 99":      {zstdLines(t, `{"snapshot":99,"node":"n"}`), "version 99"},
	} {
		r, err := NewReader(bytes.NewReader(c.in))
		if err == nil {
			r.Close()
			t.Errorf("%s: accepted", name)
			continue
		}
		if !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: error %q lacks %q", name, err, c.want)
		}
	}
	// An entry line over the 1 MiB cap is an error from Next, not a silent end.
	r, err := NewReader(bytes.NewReader(zstdLines(t, `{"snapshot":2,"node":"n"}`, long)))
	if err != nil {
		t.Fatal(err)
	}
	if e, err := r.Next(); err == nil || err == io.EOF {
		t.Errorf("an entry line over the cap gave %+v, %v", e, err)
	}
	r.Close()
	r.Close() // twice is harmless
	// The control: a valid header reads.
	r, err = NewReader(bytes.NewReader(zstdLines(t, `{"snapshot":2,"node":"n"}`, `{"p":".","t":"d"}`)))
	if err != nil {
		t.Fatal(err)
	}
	if e, err := r.Next(); err != nil || e.P != "." {
		t.Errorf("control: %+v, %v", e, err)
	}
	r.Close()
}
