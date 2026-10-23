package config

import (
	"bufio"
	"fmt"
	"os"
	"strconv"
	"strings"
)

// MemInfoPath is where MemInfo reads (tests point it elsewhere).
var MemInfoPath = "/proc/meminfo"

// MemInfo reads one field of /proc/meminfo ("MemTotal", "MemAvailable"),
// in bytes.
func MemInfo(field string) (int64, error) {
	f, err := os.Open(MemInfoPath)
	if err != nil {
		return 0, err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		k, v, ok := strings.Cut(sc.Text(), ":")
		if !ok || k != field {
			continue
		}
		fields := strings.Fields(v)
		if len(fields) == 0 {
			break
		}
		n, err := strconv.ParseInt(fields[0], 10, 64)
		if err != nil {
			return 0, fmt.Errorf("%s: %s: %w", MemInfoPath, field, err)
		}
		if len(fields) > 1 && fields[1] == "kB" {
			n <<= 10
		}
		return n, nil
	}
	if err := sc.Err(); err != nil {
		return 0, err
	}
	return 0, fmt.Errorf("%s: no %s", MemInfoPath, field)
}

// sshStagingQuota is staging_quota's default over ssh, where staged chunks
// are in memory: a quarter of the RAM, at least job (one job's window),
// at most 512 MiB.
func sshStagingQuota(job ByteSize) ByteSize {
	q := ByteSize(SSHStagingQuota)
	if total, err := MemInfo("MemTotal"); err == nil {
		q = min(q, ByteSize(total/4))
	}
	return max(q, job)
}
