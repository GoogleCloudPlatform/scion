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

// Package shareddirs holds the confined-resolution and symlink-safe
// filesystem primitives for the NFS-backed shared-dir layout (design
// deploy-config-explore §3.2.2):
//
//	<host base>/<subpath_root>/<projectID>/shared-dirs/<name>
//
// Phase 2 item 1 (pure move, hy-dev): this package was extracted out of
// pkg/agent so that pkg/hub's shared-dir file browser (§3.2.5) can reuse the
// exact same confinement and symlink-safety guarantees without importing all
// of pkg/agent's Docker/Kubernetes provisioning machinery just to get a
// handful of path-safety primitives. pkg/agent's resolveSharedDirs (the
// production caller for both container runtimes) now calls into this
// package instead of holding private copies of ValidProjectID, ConfineLeaf,
// and the openat(O_NOFOLLOW) component walk. This move changes no
// behavior: every function here is a rename of an existing pkg/agent
// unexported function, with the same logic, the same error text, and the
// same tests (moved alongside, assertions unchanged).
package shareddirs

import (
	"fmt"
	"path/filepath"
	"regexp"
)

// projectIDPattern is an allow-list for hub project IDs used as an NFS path
// segment (round 3 security review finding S-N2, PR #1779). Hub IDs are
// UUIDs in practice; identifiers from the project system's earlier naming
// era are slugs. Requiring the first character to be alphanumeric rejects
// "." and ".." (and any run of dots) along with path separators, so this
// subsumes the earlier deny-list (empty / "." / ".." / "/" / "\\"). It also
// rejects control characters, spaces and unbounded length, none of which
// are valid in either ID scheme — those wouldn't escape the export, but
// they would produce confusing errors or odd subPaths.
var projectIDPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)

// ValidProjectID reports whether projectID is safe to use as a path segment
// when resolving shared_dir_storage=nfs paths (round 2/3 security review
// findings F1/F4/S-N2, PR #1779).
func ValidProjectID(projectID string) bool {
	return projectIDPattern.MatchString(projectID)
}

// ConfineLeaf returns an error unless hostPath's parent directory is exactly
// <hostBase>/<subPathRoot>/<projectID>/shared-dirs. This is defense in depth
// alongside name/project-ID validation and the NFS backend's own path
// resolution (round 3 review finding N1/T1 part 1, PR #1779): every resolved
// shared dir must sit directly under the project's own subtree — never
// anywhere else in the export, even if a name/ID somehow validated but the
// resolver computed something unexpected (round 2 review finding F1).
func ConfineLeaf(hostPath, hostBase, subPathRoot, projectID, name string) error {
	wantParent := filepath.Join(hostBase, subPathRoot, projectID, "shared-dirs")
	if filepath.Dir(hostPath) != wantParent {
		return fmt.Errorf("shared dir %q resolved outside its project subtree", name)
	}
	return nil
}
