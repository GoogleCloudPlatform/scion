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

// Get returns the version string showing both version tag and commit hash.
func Get() string {
	ver := Version
	if ver == "" {
		ver = "dev"
	}

	commit := GetCommit()
	if commit == "" {
		commit = "unknown"
	} else if len(commit) > 8 {
		commit = commit[:8]
	}

	return fmt.Sprintf("scion %s (commit %s)", ver, commit)
}

// GetBuildTime returns the build time, applying fallbacks if the ldflags
// variable was not set. Tries: ldflags value, then binary modification time.
func GetBuildTime() string {
	if BuildTime != "" {
		return BuildTime
	}
	if exe, err := os.Executable(); err == nil {
		if info, err := os.Stat(exe); err == nil {
			return info.ModTime().UTC().Format("2006-01-02T15:04:05Z")
		}
	}
	return "unknown"
}

// GetCommit returns the commit hash, applying fallbacks if the ldflags
// variable was not set. Tries: ldflags value, then Go debug build info.
func GetCommit() string {
	if Commit != "" {
		return Commit
	}
	if info, ok := debug.ReadBuildInfo(); ok {
		for _, setting := range info.Settings {
			if setting.Key == "vcs.revision" {
				return setting.Value
			}
		}
	}
	return "unknown"
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

	if len(commit) > 8 {
		commit = commit[:8]
	}

	if commit == "" {
		return "unknown"
	}

	return commit
}
