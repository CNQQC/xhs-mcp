package main

import (
	"io/fs"
	"os"
	"strconv"
	"strings"
)

const contentBatchHeadroom = 640 << 20

func contentBatchConcurrency() int {
	return contentBatchWidth(os.DirFS("/"))
}

func contentBatchWidth(system fs.FS) int {
	const cgroup = "sys/fs/cgroup/"
	cpu, _ := fs.ReadFile(system, cgroup+"cpu.max")
	parts := strings.Fields(string(cpu))
	if len(parts) != 2 {
		return feedBatchConcurrency
	}
	quota, errQuota := strconv.ParseInt(parts[0], 10, 64)
	period, errPeriod := strconv.ParseInt(parts[1], 10, 64)
	memory, _ := fs.ReadFile(system, cgroup+"memory.max")
	limit, errMemory := strconv.ParseInt(strings.TrimSpace(string(memory)), 10, 64)
	if errQuota != nil || errPeriod != nil || errMemory != nil || quota <= 0 || period <= 0 || limit <= 0 ||
		float64(quota)/float64(period) > 1.5 || limit > 1200<<20 {
		return feedBatchConcurrency
	}
	usage, _ := fs.ReadFile(system, cgroup+"memory.current")
	if current, err := strconv.ParseInt(strings.TrimSpace(string(usage)), 10, 64); err == nil && current >= 0 && limit-current < contentBatchHeadroom {
		return 1
	}
	info, _ := fs.ReadFile(system, "proc/meminfo")
	for _, line := range strings.Split(string(info), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 3 && fields[0] == "MemAvailable:" && fields[2] == "kB" {
			if available, err := strconv.ParseInt(fields[1], 10, 64); err == nil && available >= 0 && available < contentBatchHeadroom/1024 {
				return 1
			}
			break
		}
	}
	return feedBatchConcurrency
}
