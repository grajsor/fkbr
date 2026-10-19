package mountinfo

import (
	"strings"
	"testing"
)

const sample = `22 1 8:2 / / rw,relatime shared:1 - ext4 /dev/sda2 rw
95 22 0:50 / /shared rw,noatime - nfs 172.20.16.222:/volume1/shared rw,vers=3,acregmin=1800,acregmax=1800,acdirmin=1800,acdirmax=1800,hard,nolock
96 22 0:51 / /mnt/good\040dir rw - nfs4 srv:/x rw,vers=4.1,hard,acregmin=1,acdirmin=1,lookupcache=pos
97 22 0:52 / /mnt/noac rw - nfs srv:/y rw,vers=3,noac
`

func TestMounts(t *testing.T) {
	ms := Parse(strings.NewReader(sample))
	cases := map[string]bool{"/shared/mailbox": true, "/mnt/good dir/x": false, "/mnt/noac/z": false, "/home/x": false}
	for p, want := range cases {
		m := For(ms, p)
		if m == nil {
			t.Fatalf("%s: no mount", p)
		}
		if got := m.CachesAttributes(); got != want {
			t.Errorf("%s (%s %s): CachesAttributes=%v want %v", p, m.Point, m.FSType, got, want)
		}
	}
	if m := For(ms, "/mnt/good dir/x"); m.Point != "/mnt/good dir" {
		t.Errorf("escaped mount point: %q", m.Point)
	}
}
