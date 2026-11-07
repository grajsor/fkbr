package id

import (
	"regexp"
	"sync"
	"testing"
	"time"
)

var shape = regexp.MustCompile(`^[0-9a-f]{24}$`)

// IDs are file names and the only order of requests and jobs: 24 lowercase
// hex digits, the share's time first, so sorting names sorts by time.
func TestAtShapeAndOrder(t *testing.T) {
	base := time.Date(2026, 11, 7, 12, 0, 0, 0, time.UTC)
	times := []time.Time{time.Unix(0, 1), time.Unix(1, 0), base, base.Add(time.Nanosecond), base.Add(time.Hour), base.AddDate(100, 0, 0)}
	prev := ""
	for _, tm := range times {
		got := At(tm)
		if !shape.MatchString(got) {
			t.Fatalf("At(%v) = %q", tm, got)
		}
		if prev != "" && got[:16] <= prev[:16] {
			t.Errorf("At(%v) = %q does not sort after %q", tm, got, prev)
		}
		prev = got
	}
	// the time part is the one passed in, not the clock's
	if a, b := At(base)[:16], At(base)[:16]; a != b {
		t.Errorf("same time, different prefix: %q %q", a, b)
	}
}

func TestNewIsUnique(t *testing.T) {
	const n = 2000
	var mu sync.Mutex
	seen := map[string]bool{}
	var wg sync.WaitGroup
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < n; i++ {
				s := New()
				mu.Lock()
				if seen[s] {
					t.Errorf("duplicate %q", s)
				}
				seen[s] = true
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
}
