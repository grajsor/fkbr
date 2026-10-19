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

// With autofs the trigger and the real mount share a mount point; the one
// listed later is mounted on top and is the one that counts.
func TestAutofsOvermount(t *testing.T) {
	const autofs = `22 1 8:2 / / rw,relatime shared:1 - ext4 /dev/sda2 rw
40 22 0:40 / /data rw,relatime shared:20 - autofs systemd-1 rw,fd=33,pgrp=1,timeout=0,minproto=5,maxproto=5,direct
98 40 0:60 / /data rw,relatime shared:50 - nfs nas:/vol/data rw,vers=3,acregmin=3,acregmax=60,acdirmin=30,acdirmax=60,hard
`
	m := For(Parse(strings.NewReader(autofs)), "/data/xfer/box")
	if m == nil || m.FSType != "nfs" {
		t.Fatalf("got %+v, want the nfs mount on top of autofs", m)
	}
	if !m.CachesAttributes() {
		t.Error("default NFS attribute caching (acdirmin=30) not detected")
	}
}
