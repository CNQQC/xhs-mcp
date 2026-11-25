package main

import (
	"testing"
	"testing/fstest"
)

func TestContentBatchWidth(t *testing.T) {
	for _, tc := range []struct {
		name, cpu, limit, used, available string
		want                              int
	}{
		{"room for two", "150000 100000", "1258291200", "123456789", "900000", 2},
		{"host pressure", "150000 100000", "1258291200", "123456789", "600000", 1},
		{"container pressure", "150000 100000", "1258291200", "700000000", "900000", 1},
		{"already above limit", "150000 100000", "1258291200", "1300000000", "", 1},
		{"at threshold", "150000 100000", "1258291200", "587202560", "655360", 2},
		{"other cpu", "200000 100000", "1258291200", "700000000", "600000", 2},
		{"other memory", "150000 100000", "2147483648", "700000000", "600000", 2},
		{"cpu unlimited", "max 100000", "1258291200", "1", "1", 2},
		{"memory unlimited", "150000 100000", "max", "1", "1", 2},
		{"unknown pressure", "150000 100000", "1258291200", "bogus", "-1", 2},
		{"bad period", "1 0", "1", "1", "1", 2},
		{"no controller", "", "", "", "", 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			system := fstest.MapFS{
				"sys/fs/cgroup/cpu.max":        {Data: []byte(tc.cpu)},
				"sys/fs/cgroup/memory.max":     {Data: []byte(tc.limit)},
				"sys/fs/cgroup/memory.current": {Data: []byte(tc.used)},
				"proc/meminfo":                 {Data: []byte("MemFree: 0 kB\nMemAvailable: " + tc.available + " kB\n")},
			}
			if got := contentBatchWidth(system); got != tc.want {
				t.Errorf("got %d, want %d", got, tc.want)
			}
		})
	}
}
