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

package transfer

import "strings"

// WorkspaceIdentityEntry is the name of a workspace's root project identity
// entry (config.DotScion): a marker file, or a directory holding the
// project identity and settings. pkg/config imports this package, so the
// name is written here as a literal; pkg/config's tests keep the two equal.
const WorkspaceIdentityEntry = ".scion"

// WorkspaceObjectExclusion names why ExcludeWorkspaceObject excluded a name.
type WorkspaceObjectExclusion string

const (
	// WorkspaceObjectKept means the name is materialized.
	WorkspaceObjectKept WorkspaceObjectExclusion = ""
	// WorkspaceObjectDirectory is a "directory" object (a name ending in /).
	WorkspaceObjectDirectory WorkspaceObjectExclusion = "directory"
	// WorkspaceObjectInvalidPath is a name that fails ValidateRelPath.
	WorkspaceObjectInvalidPath WorkspaceObjectExclusion = "invalid_path"
	// WorkspaceObjectIdentity is the root identity entry or a name under it.
	WorkspaceObjectIdentity WorkspaceObjectExclusion = "identity"
)

// ExcludeWorkspaceObject is the one rule table that decides which stored
// workspace object names are left out of a workspace download. name is the
// object name relative to the workspace's files/ prefix, slash-separated.
// It returns WorkspaceObjectKept for a name to materialize, else the reason
// it is excluded:
//
//   - a "directory" object: a name ending in "/";
//   - a name that is not a canonical relative path (ValidateRelPath), which
//     covers traversal, absolute names and non-canonical forms such as
//     "./.scion";
//   - the root identity entry and everything under it (".scion",
//     ".scion/x"). A nested identity-named entry such as "a/.scion" is
//     ordinary content and is kept, as the rclone sync's root-anchored
//     exclude rules keep it.
//
// The hub applies it when it lists the objects it signs URLs for, and the
// Runtime Broker applies it again to every name it is about to write, so
// the two sides cannot disagree about what a workspace contains.
func ExcludeWorkspaceObject(name string) WorkspaceObjectExclusion {
	if strings.HasSuffix(name, "/") {
		return WorkspaceObjectDirectory
	}
	if ValidateRelPath(name) != nil {
		return WorkspaceObjectInvalidPath
	}
	first, _, _ := strings.Cut(name, "/")
	if first == WorkspaceIdentityEntry {
		return WorkspaceObjectIdentity
	}
	return WorkspaceObjectKept
}
