// Copyright The EyeDebugger Authors
// SPDX-License-Identifier: Apache-2.0

// Package version reports build metadata for EyeDebugger binaries.
//
// Release builds inject values at link time:
//
//	-ldflags "-X github.com/eyedebugger/eyedebugger/internal/version.version=v1.2.3"
//
// Builds without ldflags (go build in a checkout, go install module@version)
// fall back to the metadata recorded by the Go toolchain.
package version

import (
	"runtime"
	"runtime/debug"
)

// Values injected with -ldflags "-X". They must stay uninitialized package-level
// string variables for -X to apply.
//
//nolint:gochecknoglobals // Link-time injection requires package-level variables.
var (
	version string
	commit  string
	date    string
)

const (
	devVersion = "dev"
	unknown    = "unknown"
)

// Info describes the build of the running binary.
type Info struct {
	Version   string `json:"version"`
	Commit    string `json:"commit"`
	Date      string `json:"date"`
	GoVersion string `json:"goVersion"`
	Platform  string `json:"platform"`
}

// Get returns the build information of the running binary.
func Get() Info {
	bi, _ := debug.ReadBuildInfo() // nil when the binary carries no build info

	info := resolve(version, commit, date, bi)
	info.GoVersion = runtime.Version()
	info.Platform = runtime.GOOS + "/" + runtime.GOARCH

	return info
}

// resolve merges link-time values with toolchain build metadata. Link-time
// values win; bi may be nil.
func resolve(ldVersion, ldCommit, ldDate string, bi *debug.BuildInfo) Info {
	info := Info{Version: ldVersion, Commit: ldCommit, Date: ldDate}

	if bi != nil {
		if info.Version == "" {
			info.Version = bi.Main.Version
		}

		for _, s := range bi.Settings {
			switch {
			case s.Key == "vcs.revision" && info.Commit == "":
				info.Commit = s.Value
			case s.Key == "vcs.time" && info.Date == "":
				info.Date = s.Value
			}
		}
	}

	if info.Version == "" || info.Version == "(devel)" {
		info.Version = devVersion
	}

	if info.Commit == "" {
		info.Commit = unknown
	}

	if info.Date == "" {
		info.Date = unknown
	}

	return info
}
