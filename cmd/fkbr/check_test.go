package main

import (
	"bytes"
	"context"
	"io"
	"os"
	"strings"
	"testing"

	"fkbr/internal/config"
	"fkbr/internal/safetext"
)

// The check lines go to the writer given (stdout() cleans it on a
// terminal), never straight to os.Stdout: a share path with an escape in
// it must not reach a terminal raw.
func TestRunCheckWritesThroughItsWriter(t *testing.T) {
	cfg := &config.Config{NodeID: "n1", NFSRoot: "/tmp/x\x1b]0;pwned\x07", StateDir: t.TempDir(), NFSRefresh: "never"}
	_ = cfg.Finish()
	old := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout = w
	var buf bytes.Buffer
	_ = runCheck(context.Background(), cfg, safetext.Writer(&buf))
	w.Close()
	os.Stdout = old
	leaked, _ := io.ReadAll(r)
	if len(leaked) != 0 {
		t.Errorf("written to os.Stdout: %q", leaked)
	}
	if buf.Len() == 0 {
		t.Fatal("nothing written to the writer")
	}
	if strings.ContainsRune(buf.String(), '\x1b') {
		t.Errorf("escape in the output: %q", buf.String())
	}
}
