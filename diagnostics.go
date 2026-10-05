package main

import (
	"runtime"
	"runtime/debug"
	"strings"
	"time"
)

var processStartedAt = time.Now()

// BuildIdentity 来自实际二进制，main.version 的发布注入保持兼容。
type BuildIdentity struct {
	Version     string `json:"version"`
	Revision    string `json:"revision,omitempty"`
	VCSModified bool   `json:"vcs_modified"`
	VCSTime     string `json:"vcs_time,omitempty"`
	GoVersion   string `json:"go_version"`
	OS          string `json:"os"`
	Arch        string `json:"arch"`
}

func currentBuildIdentity() BuildIdentity {
	build := BuildIdentity{Version: strings.TrimSpace(version), GoVersion: runtime.Version(), OS: runtime.GOOS, Arch: runtime.GOARCH}
	if build.Version == "" {
		build.Version = "dev"
	}
	if info, ok := debug.ReadBuildInfo(); ok {
		for _, setting := range info.Settings {
			switch setting.Key {
			case "vcs.revision":
				build.Revision = setting.Value
			case "vcs.modified":
				build.VCSModified = setting.Value == "true"
			case "vcs.time":
				build.VCSTime = setting.Value
			}
		}
	}
	if build.Version == "dev" && build.Revision != "" {
		revision := build.Revision
		if len(revision) > 12 {
			revision = revision[:12]
		}
		build.Version = "dev+" + revision
		if build.VCSModified {
			build.Version += ".dirty"
		}
	}
	return build
}

// diagnosticsSnapshot 只检查进程存活，不启动浏览器、不读取 cookies、不探测登录。
func diagnosticsSnapshot() map[string]any {
	return map[string]any{
		"status":         "alive",
		"scope":          "process_liveness",
		"service":        "xiaohongshu-mcp",
		"version":        currentBuildIdentity().Version,
		"build":          currentBuildIdentity(),
		"timestamp":      time.Now().UTC().Format(time.RFC3339Nano),
		"uptime_seconds": int64(time.Since(processStartedAt).Seconds()),
		"browser_status": "not_checked",
		"login_status":   "not_checked",
		"limits":         map[string]any{"batch_max": feedBatchMax, "batch_concurrency": feedBatchConcurrency, "ref_ttl_seconds": int64(refTTL.Seconds())},
	}
}
