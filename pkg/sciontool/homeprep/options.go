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

package homeprep

// PrepareOptions configures Prepare.
type PrepareOptions struct {
	// Home is the agent's home directory on the export, as mounted in the
	// init container.
	Home string
	// MemDir is the pod's memory directory, where the mode file and the
	// link result are written for the broker.
	MemDir string
	// AgentID is the hub agent ID the home belongs to.
	AgentID string
	// StartID identifies this start; LaunchID the hub launch, if any.
	StartID  string
	LaunchID string
	// Links are the links to place (from SCION_HOME_LINKS).
	Links []Link
	// SkeletonSource is the image's own home directory, copied into a new
	// home if its regular files total at most SkeletonMaxBytes. Empty
	// copies nothing.
	SkeletonSource   string
	SkeletonMaxBytes int64
	// Log receives warnings and progress lines. Nil discards them.
	Log func(format string, args ...any)
}

func (o *PrepareOptions) logf(format string, args ...any) {
	if o.Log != nil {
		o.Log(format, args...)
	}
}

// LeafOptions configures Leaf.
type LeafOptions struct {
	// AgentDir is the agent's directory on the export (agents/<slug>), as
	// mounted in the init container.
	AgentDir string
	// HomeName is the home directory's name in it (home-<agent id>).
	HomeName string
	// UID and GID own the home directory. GID is the export's group.
	UID int
	GID int
	Log func(format string, args ...any)
}

// HomeDirPrefix starts the name of an agent's home directory in its agent
// directory: home-<agent id>.
const HomeDirPrefix = "home-"

// ValidAgentID reports whether id is a hub agent ID in canonical
// (lower-case, hyphenated) UUID form, the only form a home directory is
// named with.
func ValidAgentID(id string) bool {
	if len(id) != 36 {
		return false
	}
	for i, c := range id {
		switch i {
		case 8, 13, 18, 23:
			if c != '-' {
				return false
			}
		default:
			if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
				return false
			}
		}
	}
	return true
}
