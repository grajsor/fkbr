package inventory

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"fkbr/internal/config"
	"fkbr/internal/localfs"

	"fkbr/internal/api"
)

const t0 = int64(1_700_000_000_000_000_000)

func snap(t *testing.T, entries ...Entry) *Reader {
	t.Helper()
	return snapH(t, Header{Node: "n"}, entries...)
}

func snapH(t *testing.T, h Header, entries ...Entry) *Reader {
	t.Helper()
	var buf bytes.Buffer
	w, err := NewWriter(&buf, h)
	if err != nil {
		t.Fatal(err)
	}
	for i := range entries {
		if err := w.Write(&entries[i]); err != nil {
			t.Fatal(err)
		}
	}
	w.Close()
	r, err := NewReader(&buf)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func dir(p string) Entry           { return Entry{P: p, T: TypeDir, M: t0, Mo: 0o755} }
func file(p string, s int64) Entry { return Entry{P: p, T: TypeFile, S: s, M: t0, Mo: 0o644} }

type recSink struct {
	copies  map[string]uint64
	deletes map[int][]string
}

func newSink() *recSink { return &recSink{copies: map[string]uint64{}, deletes: map[int][]string{}} }

func (s *recSink) Copy(mask uint64, e *Entry) error {
	s.copies[e.P] |= mask
	return nil
}
func (s *recSink) Delete(i int, p string) error {
	s.deletes[i] = append(s.deletes[i], p)
	return nil
}

func (s *recSink) copied(mask uint64) string {
	var out []string
	for p, m := range s.copies {
		if m&mask != 0 {
			out = append(out, p)
		}
	}
	sort.Slice(out, func(i, j int) bool { return ComparePaths(out[i], out[j]) < 0 })
	return strings.Join(out, ",")
}

func TestComparePaths(t *testing.T) {
	ordered := []string{".", "a", "a/x", "a/x/y", "a-b", "a.c", "b", "b/a"}
	for i := range ordered {
		for j := range ordered {
			got := ComparePaths(ordered[i], ordered[j])
			if (i < j && got >= 0) || (i > j && got <= 0) || (i == j && got != 0) {
				t.Errorf("ComparePaths(%q,%q) = %d", ordered[i], ordered[j], got)
			}
		}
	}
}

func TestPlanIncremental(t *testing.T) {
	src := snap(t, dir("."), dir("a"), file("a/new", 5), file("a/same", 3), file("changed", 7), file("keep", 1))
	changed := file("changed", 6)
	dst := snap(t, dir("."), dir("a"), file("a/same", 3), changed, file("extra", 9), file("keep", 1))
	s := newSink()
	st, err := Plan(src, []Target{{Node: "t", Snap: dst}}, PlanOptions{}, s)
	if err != nil {
		t.Fatal(err)
	}
	// "a" is re-sent because a new entry lands in it (its mtime changes),
	// "." because "changed" is replaced in it.
	if got := s.copied(1); got != ".,a,a/new,changed" {
		t.Errorf("copies = %s", got)
	}
	if len(s.deletes[0]) != 0 {
		t.Errorf("incremental must not delete: %v", s.deletes)
	}
	ts := st.Targets[0]
	if ts.Unchanged != 4 || ts.CopyBytes != 12 {
		t.Errorf("stats %+v", ts)
	}
}

func TestPlanMirrorCollapsesDeletes(t *testing.T) {
	src := snap(t, dir("."), file("b", 1))
	dst := snap(t, dir("."), dir("a"), dir("a/deep"), file("a/deep/x", 1), file("a/y", 1), file("b", 1), file("c", 1))
	s := newSink()
	st, err := Plan(src, []Target{{Node: "t", Snap: dst}}, PlanOptions{Mirror: true}, s)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(s.deletes[0], ","); got != "a,c" {
		t.Errorf("deletes = %s (want top-most only)", got)
	}
	if got := s.copied(1); got != "." {
		t.Errorf("copies = %s (only the parent's mtime)", got)
	}
	if st.Targets[0].Delete != 2 {
		t.Errorf("stats %+v", st.Targets[0])
	}
}

func TestPlanTypeChanges(t *testing.T) {
	// x: dir on target, file in source; y: file on target, dir in source.
	src := snap(t, dir("."), file("x", 1), dir("y"), file("y/in", 1))
	dst := snap(t, dir("."), dir("x"), file("x/old", 1), file("y", 1))
	s := newSink()
	st, err := Plan(src, []Target{{Node: "t", Snap: dst}}, PlanOptions{}, s)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(s.deletes[0], ","); got != "x,y" {
		t.Errorf("deletes = %s", got)
	}
	if got := s.copied(1); got != ".,x,y,y/in" {
		t.Errorf("copies = %s", got)
	}
	if st.Targets[0].Replace != 2 {
		t.Errorf("stats %+v", st.Targets[0])
	}
}

func TestPlanUnreadableSourceNeverDeletes(t *testing.T) {
	unreadable := dir("locked")
	src := snap(t, dir("."), unreadable, Entry{P: "locked", T: TypeError, E: "permission denied"}, Entry{P: "lost", T: TypeError, E: "stat failed"})
	dst := snap(t, dir("."), dir("locked"), file("locked/precious", 1), file("lost", 1))
	s := newSink()
	st, err := Plan(src, []Target{{Node: "t", Snap: dst}}, PlanOptions{Mirror: true}, s)
	if err != nil {
		t.Fatal(err)
	}
	if len(s.deletes[0]) != 0 {
		t.Fatalf("deleted under an unreadable source: %v", s.deletes[0])
	}
	if len(st.Problems) != 2 {
		t.Errorf("problems: %v", st.Problems)
	}
}

func TestPlanFanOutGroups(t *testing.T) {
	src := snap(t, dir("."), file("a", 1), file("b", 1), file("c", 1))
	t1 := snap(t, dir("."), file("a", 1))               // needs b, c
	t2 := snap(t, dir("."), file("a", 1), file("b", 1)) // needs c
	t3 := snap(t)                                       // missing: needs all
	s := newSink()
	if _, err := Plan(src, []Target{{Node: "1", Snap: t1}, {Node: "2", Snap: t2}, {Node: "3", Snap: t3}}, PlanOptions{}, s); err != nil {
		t.Fatal(err)
	}
	want := map[string]uint64{".": 0b111, "a": 0b100, "b": 0b101, "c": 0b111}
	for p, m := range want {
		if s.copies[p] != m {
			t.Errorf("%s: mask %03b want %03b", p, s.copies[p], m)
		}
	}
}

func TestPlanOwnershipOnlyWhereFixable(t *testing.T) {
	sf := file("f", 1)
	sf.U = 1234
	src := func() *Reader { return snap(t, dir("."), sf) }
	dst := func() *Reader { return snap(t, dir("."), file("f", 1)) } // owned by uid 0
	s := newSink()
	Plan(src(), []Target{{Node: "plain", Snap: dst()}}, PlanOptions{}, s)
	if len(s.copies) != 0 {
		t.Errorf("target without chown re-copies forever: %v", s.copies)
	}
	s = newSink()
	Plan(src(), []Target{{Node: "cap", Snap: dst(), Chown: true}}, PlanOptions{}, s)
	if s.copies["f"] == 0 {
		t.Error("ownership difference ignored on a chown-capable target")
	}
}

func TestPlanChecksum(t *testing.T) {
	a, b := file("f", 1), file("f", 1)
	a.H, b.H = "aa", "bb"
	s := newSink()
	Plan(snap(t, dir("."), a), []Target{{Node: "t", Snap: snap(t, dir("."), b)}}, PlanOptions{}, s)
	if len(s.copies) != 0 {
		t.Error("hashes compared without checksum mode")
	}
	s = newSink()
	Plan(snap(t, dir("."), a), []Target{{Node: "t", Snap: snap(t, dir("."), b)}}, PlanOptions{Checksum: true}, s)
	if s.copies["f"] == 0 {
		t.Error("checksum mode missed a content difference")
	}
}

func TestReaderRejectsDisorder(t *testing.T) {
	r := snap(t, dir("."), file("b", 1), file("a", 1))
	var err error
	for err == nil {
		_, err = r.Next()
	}
	if !strings.Contains(err.Error(), "out of order") {
		t.Fatalf("got %v", err)
	}
}

func TestScanRealTree(t *testing.T) {
	root := t.TempDir()
	mk := func(p, data string) {
		os.MkdirAll(filepath.Dir(filepath.Join(root, p)), 0o755)
		os.WriteFile(filepath.Join(root, p), []byte(data), 0o644)
	}
	mk("base/a-b", "1")
	mk("base/a/x", "22")
	mk("base/z", "333")
	mk(".fkbr-tmp/job/tree/f", "x")
	os.Symlink("z", filepath.Join(root, "base/link"))
	os.Mkdir(filepath.Join(root, "base/locked"), 0o755)
	mk("base/locked/secret", "s")
	os.Chmod(filepath.Join(root, "base/locked"), 0)
	defer os.Chmod(filepath.Join(root, "base/locked"), 0o755)

	fsys, err := localfs.Open([]config.Root{{ID: "r", Path: root}})
	if err != nil {
		t.Fatal(err)
	}
	defer fsys.Close()
	var buf bytes.Buffer
	st, err := Scan(fsys, "r", "base", Header{Node: "n"}, ScanOptions{Checksum: true, Exclude: ".fkbr-tmp"}, &buf)
	if err != nil {
		t.Fatal(err)
	}
	r, err := NewReader(&buf)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for {
		e, err := r.Next()
		if err != nil {
			break
		}
		s := e.P + ":" + e.T
		if e.Unreadable {
			s += "!"
		}
		if e.T == TypeFile && e.H == "" {
			s += "(nohash)"
		}
		got = append(got, s)
	}
	want := ".:d,a:d,a/x:f,a-b:f,link:l,locked:d!,z:f"
	if os.Geteuid() == 0 { // root can read the locked dir
		want = ".:d,a:d,a/x:f,a-b:f,link:l,locked:d,locked/secret:f,z:f"
	}
	if strings.Join(got, ",") != want {
		t.Errorf("scan:\n got %s\nwant %s", strings.Join(got, ","), want)
	}
	if st.Bytes != 6 || (os.Geteuid() != 0 && st.Errors != 1) {
		t.Errorf("stats %+v", st)
	}

	// Whole root: the staging area is excluded.
	buf.Reset()
	if _, err := Scan(fsys, "r", ".", Header{}, ScanOptions{Exclude: ".fkbr-tmp"}, &buf); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(fmt.Sprint(readSnap(t, buf.Bytes())), ".fkbr-tmp") {
		t.Error("staging area not excluded")
	}
	// Missing base.
	buf.Reset()
	st, err = Scan(fsys, "r", "nope", Header{}, ScanOptions{}, &buf)
	if err != nil || !st.Missing {
		t.Fatalf("missing: %+v %v", st, err)
	}
}

func readSnap(t *testing.T, b []byte) string {
	r, err := NewReader(bytes.NewReader(b))
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for {
		e, err := r.Next()
		if err != nil {
			return strings.Join(out, ",")
		}
		out = append(out, e.P)
	}
}

// A target that cannot store what the source has (FAT: mtimes in 2 s
// steps, no modes) is compared within those limits, or every sync would
// send everything again.
func TestPlanCoarseTarget(t *testing.T) {
	src := func() *Reader {
		f := file("f", 1)
		f.M = t0 + 1_930_000_000
		g := file("g", 1)
		g.M = t0 + 2_500_000_000
		return snap(t, dir("."), f, g)
	}
	dst := func(h Header) *Reader {
		f, g := file("f", 1), file("g", 1)
		f.Mo, g.Mo = 0o755, 0o755
		d := dir(".")
		d.Mo = 0o777
		return snapH(t, h, d, f, g)
	}
	s := newSink()
	Plan(src(), []Target{{Node: "fat", Snap: dst(Header{Node: "fat", MtimeStep: 2, NoModes: true})}}, PlanOptions{}, s)
	if s.copies["f"] != 0 || s.copies["g"] == 0 {
		t.Errorf("FAT target: copies %v, want only g (2.5 s newer)", s.copies)
	}
	s = newSink()
	f := file("f", 1)
	f.M = t0 + 1_930_000_000
	Plan(snap(t, dir("."), f), []Target{{Node: "fat", Snap: snapH(t, Header{Node: "fat", MtimeStep: 2, NoModes: true}, dir("."), file("f", 1))}}, PlanOptions{}, s)
	if len(s.copies) != 0 {
		t.Errorf("FAT target in sync: copies %v", s.copies)
	}
	s = newSink()
	Plan(src(), []Target{{Node: "ext4", Snap: dst(Header{Node: "ext4"})}}, PlanOptions{}, s)
	if s.copies["f"] == 0 {
		t.Errorf("exact target: f differs in mode and mtime but is not copied: %v", s.copies)
	}
}

// KeepNewer (rsync -u) keeps target files newer than the source's; a type
// change still applies, and an older target file is still replaced.
func TestPlanKeepNewer(t *testing.T) {
	at := func(e Entry, d int64) Entry { e.M = t0 + d*1e9; return e }
	src := func() *Reader {
		return snap(t, dir("."), at(file("newer-there", 1), 0), at(file("older-there", 1), 10), at(file("same", 1), 0), dir("was-file"))
	}
	dst := func() *Reader {
		return snap(t, dir("."), at(file("newer-there", 2), 10), at(file("older-there", 2), 0), at(file("same", 1), 0), at(file("was-file", 1), 99))
	}
	s := newSink()
	ps, err := Plan(src(), []Target{{Node: "t", Snap: dst()}}, PlanOptions{KeepNewer: true}, s)
	if err != nil {
		t.Fatal(err)
	}
	if s.copies["newer-there"] != 0 || s.copies["older-there"] == 0 || s.copies["was-file"] == 0 {
		t.Errorf("copies %v", s.copies)
	}
	if ts := ps.Targets[0]; ts.Kept != 1 || ts.Unchanged < 1 || ts.Replace != 1 {
		t.Errorf("stats %+v", ts)
	}
	s = newSink()
	Plan(src(), []Target{{Node: "t", Snap: dst()}}, PlanOptions{}, s)
	if s.copies["newer-there"] == 0 {
		t.Error("without KeepNewer a newer target file must be replaced")
	}
}

// A target that clamps mtimes beyond 2038 (XFS without bigtime) holds the
// clamped value: that is as good as it gets, not a reason to copy again.
func TestPlanClampedMtimes(t *testing.T) {
	const max32 = int64(1<<31-1) * 1e9
	late := file("late", 1)
	late.M = 4102444800 * 1e9 // 2100-01-01
	early := file("early", 1)
	early.M = -2208988800 * 1e9 // 1900-01-01
	clampedLate, clampedEarly := late, early
	clampedLate.M, clampedEarly.M = max32, -(1<<31)*1e9
	h := Header{Node: "xfs", MtimeMin: -(1 << 31) * 1e9, MtimeMax: max32}
	s := newSink()
	Plan(snap(t, dir("."), early, late), []Target{{Node: "xfs", Snap: snapH(t, h, dir("."), clampedEarly, clampedLate)}}, PlanOptions{}, s)
	if len(s.copies) != 0 {
		t.Errorf("clamped mtimes copied again: %v", s.copies)
	}
	s = newSink()
	Plan(snap(t, dir("."), early, late), []Target{{Node: "xfs", Snap: snapH(t, h, dir("."), clampedEarly, clampedLate)}}, PlanOptions{KeepNewer: true}, s)
	if len(s.copies) != 0 {
		t.Errorf("-u: clamped mtimes copied again: %v", s.copies)
	}
	s = newSink()
	Plan(snap(t, dir("."), early, late), []Target{{Node: "ext4", Snap: snapH(t, Header{Node: "ext4"}, dir("."), clampedEarly, clampedLate)}}, PlanOptions{}, s)
	if s.copied(1) != ".,early,late" {
		t.Errorf("a target that can store them must get them: %q", s.copied(1))
	}
}

// A FAT target cannot store some names, and takes README and readme for
// one name: the plan leaves those out (and says so) instead of planning
// them again on every run.
func TestPlanNamesTheTargetCannotStore(t *testing.T) {
	link := Entry{P: "link", T: TypeSymlink, L: "fine", M: t0}
	src := snap(t, dir("."), file("README", 1), dir("bad."), file("bad./in", 1), file("fine", 1), link,
		file("q?x", 1), file("readme", 1), file("tr ", 1), file("\xc3\xa9t\xc3\xa9", 1))
	h := Header{Node: "fat", MtimeStep: 2, NoModes: true, Names: &api.NameRules{FS: "vfat", Fold: true, ASCII: true, NoSymlinks: true}}
	s := newSink()
	st, err := Plan(src, []Target{{Node: "fat", Snap: snapH(t, h, dir("."))}}, PlanOptions{}, s)
	if err != nil {
		t.Fatal(err)
	}
	if got := s.copied(1); got != ".,README,fine" {
		t.Errorf("copied %q, want .,README,fine", got)
	}
	if st.Targets[0].Refused != 6 || len(st.Problems) == 0 || !strings.Contains(st.Problems[0], "6 entries cannot be stored on its vfat") {
		t.Errorf("refused %d, problems %q", st.Targets[0].Refused, st.Problems)
	}
	// The target already holds Readme: a source readme is the same name.
	s = newSink()
	Plan(snap(t, dir("."), file("Readme", 1), file("readme", 1)), []Target{{Node: "fat", Snap: snapH(t, h, dir("."), file("Readme", 1))}}, PlanOptions{}, s)
	if len(s.copies) != 0 {
		t.Errorf("readme next to the target's Readme: %v", s.copies)
	}
	// casefold ext4: é in NFC and NFD are one name, invalid UTF-8 is fine.
	cf := Header{Node: "ci", Names: &api.NameRules{FS: "casefold", Fold: true, Form: true}}
	s = newSink()
	st, _ = Plan(snap(t, dir("."), file("cafe\xcc\x81", 1), file("caf\xc3\xa9", 1), file("x\xff", 1)), []Target{{Node: "ci", Snap: snapH(t, cf, dir("."))}}, PlanOptions{}, s)
	if got := s.copied(1); got != ".,cafe\xcc\x81,x\xff" || st.Targets[0].Refused != 1 {
		t.Errorf("casefold: copied %q, refused %d", got, st.Targets[0].Refused)
	}
	// A mirror deletes the target's README first, so readme can come.
	s = newSink()
	Plan(snap(t, dir("."), file("readme", 1)), []Target{{Node: "fat", Snap: snapH(t, h, dir("."), file("README", 1))}}, PlanOptions{Mirror: true}, s)
	if s.copies["readme"] == 0 || len(s.deletes[0]) != 1 {
		t.Errorf("mirror over a case-only rename: copies %v deletes %v", s.copies, s.deletes)
	}
}
