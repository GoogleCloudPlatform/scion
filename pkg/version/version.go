// Copyright 2026 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package version

import (
	"fmt"
	"os"
	"runtime/debug"
)

var (
	// Version is the current version of the application.
	// It should be set via ldflags -X.
	Version string

	// Commit is the git commit hash of the build.
	// It should be set via ldflags -X.
	Commit string

	// BuildTime is the timestamp of the build.
	// It should be set via ldflags -X.
	BuildTime string
)

// Get returns the version string based on the current build information.
func Get() string {
	// If Version is set, we assume it's a semantic version tag injected at build time.
	if Version != "" {
		return Version
	}

	// Fallback to commit and build time.
	commit := Commit
	buildTime := BuildTime

	// If variables are empty (e.g. go run or simple go build), try to read from debug info.
	if commit == "" {
		if info, ok := debug.ReadBuildInfo(); ok {
			for _, setting := range info.Settings {
				if setting.Key == "vcs.revision" {
					commit = setting.Value
				}
				// Note: vcs.time is commit time, not build time.
			}
		}
	}

	// Shorten commit hash if it's long
	if len(commit) > 7 {
		commit = commit[:7]
	}

	if commit == "" {
		commit = "unknown"
	}

	// If buildTime is not set via ldflags, try to get the binary's modification time.
	if buildTime == "" {
		if exe, err := os.Executable(); err == nil {
			if info, err := os.Stat(exe); err == nil {
				buildTime = info.ModTime().Format("2006-01-02 15:04:05")
			}
		}
	}

	if buildTime == "" {
		buildTime = "unknown"
	}

	return fmt.Sprintf("Commit: %s\nBuild Time: %s", commit, buildTime)
}

// Short returns a short version string (either Version or short Commit hash).
func Short() string {
	if Version != "" {
		return Version
	}

	commit := Commit
	if commit == "" {
		if info, ok := debug.ReadBuildInfo(); ok {
			for _, setting := range info.Settings {
				if setting.Key == "vcs.revision" {
					commit = setting.Value
				}
			}
		}
	}

	if len(commit) > 7 {
		commit = commit[:7]
	}

	if commit == "" {
		return "unknown"
	}

	return commit
}
