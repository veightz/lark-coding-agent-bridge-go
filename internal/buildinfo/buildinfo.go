// Package buildinfo exposes version identity for the running binary:
// module version, VCS commit/time/dirty flag. Values come from Go's
// embedded build info when built from a git checkout, with optional
// ldflags overrides for release builds:
//
//	-ldflags "-X lark-coding-agent-bridge-go/internal/buildinfo.Version=0.2.0"
package buildinfo

import (
	"runtime/debug"
	"strings"
)

// Version is the human-facing release version (ldflags-overridable).
var Version = "0.2.0"

// Info describes the running binary.
type Info struct {
	Version    string `json:"version"`
	Commit     string `json:"commit,omitempty"`
	CommitTime string `json:"commitTime,omitempty"`
	Dirty      bool   `json:"dirty,omitempty"`
	GoVersion  string `json:"goVersion,omitempty"`
}

// Short returns a compact label like "0.2.0 (a1b2c3d, dirty)".
func (i Info) Short() string {
	s := i.Version
	if i.Commit != "" {
		short := i.Commit
		if len(short) > 7 {
			short = short[:7]
		}
		s += " (" + short
		if i.Dirty {
			s += ", dirty"
		}
		s += ")"
	}
	return s
}

// Current resolves the build info of the running binary.
func Current() Info {
	info := Info{Version: Version}
	bi, ok := debug.ReadBuildInfo()
	if !ok {
		return info
	}
	info.GoVersion = bi.GoVersion
	for _, s := range bi.Settings {
		switch s.Key {
		case "vcs.revision":
			info.Commit = s.Value
		case "vcs.time":
			info.CommitTime = s.Value
		case "vcs.modified":
			info.Dirty = s.Value == "true"
		}
	}
	// Strip the "+incompatible"-style suffixes if present.
	info.Commit = strings.TrimSpace(info.Commit)
	return info
}
