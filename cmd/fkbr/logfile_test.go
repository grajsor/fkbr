package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLogFileRotates(t *testing.T) {
	p := filepath.Join(t.TempDir(), "node.log")
	lf, err := openLogFile(p, 100)
	if err != nil {
		t.Fatal(err)
	}
	line := strings.Repeat("x", 39) + "\n" // 40 bytes
	for i := 0; i < 6; i++ {
		if _, err := lf.Write([]byte(line)); err != nil {
			t.Fatal(err)
		}
	}
	lf.Close()
	cur, _ := os.ReadFile(p)
	old, err := os.ReadFile(p + ".1")
	if err != nil {
		t.Fatalf("no rotated file: %v", err)
	}
	if len(cur) > 100 || len(old) > 100 || len(cur)+len(old) < 160 {
		t.Errorf("sizes after rotation: current %d, old %d", len(cur), len(old))
	}

	// Reopening appends, and rotates at once if already over the limit.
	os.WriteFile(p, []byte(strings.Repeat("y", 150)), 0o600)
	lf, err = openLogFile(p, 100)
	if err != nil {
		t.Fatal(err)
	}
	lf.Write([]byte("new\n"))
	lf.Close()
	if cur, _ := os.ReadFile(p); string(cur) != "new\n" {
		t.Errorf("oversized log not rotated on open: %q", cur)
	}
}
