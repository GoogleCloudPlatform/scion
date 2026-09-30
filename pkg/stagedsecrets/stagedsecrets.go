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

package stagedsecrets

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/dirfd"
	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/rootexec"
)

// EnvVar is the environment variable used to pass serialized
// file and variable secrets from the broker to the container. The value is
// a base64-encoded JSON blob decoded by sciontool init.
const EnvVar = "SCION_STAGED_SECRETS"

// FileSecret describes a single file-type secret in the staged blob.
type FileSecret struct {
	Name   string `json:"name"`
	Target string `json:"target"` // container-side path (tilde already expanded)
	Value  string `json:"value"`  // base64-encoded file content
}

// Staged is the top-level structure serialized into SCION_STAGED_SECRETS.
type Staged struct {
	FileSecrets     []FileSecret      `json:"file_secrets,omitempty"`
	VariableSecrets map[string]string `json:"variable_secrets,omitempty"`
}

// Decode decodes the SCION_STAGED_SECRETS env var value
// (base64 → JSON) and returns the structured secrets. This is called
// inside the container by sciontool init.
func Decode(encoded string) (*Staged, error) {
	jsonData, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return nil, fmt.Errorf("failed to base64-decode staged secrets: %w", err)
	}
	var staged Staged
	if err := json.Unmarshal(jsonData, &staged); err != nil {
		return nil, fmt.Errorf("failed to unmarshal staged secrets JSON: %w", err)
	}
	return &staged, nil
}

// Write writes decoded staged secrets to the filesystem inside
// the container. File secrets are written to their target paths with 0600
// permissions. Variable secrets are written to <homeDir>/.scion/secrets.json.
//
// Every leaf this writes — a file secret's own Target, and secrets.json —
// goes through dirfd.WriteFileNoFollow: a workload-plantable symlink at
// either path (e.g. left over from a previous run on a persisted home, or
// planted ahead of a restart) is refused outright rather than written or
// chowned through, which this function otherwise does as root.
// WriteFileNoFollow's own parent-directory walk (OpenParentNoFollow) is
// symlink-safe component-by-component. This applies in both enforced and
// non-enforced runs: a restart exposes the same planted symlink regardless
// of which mode created the home directory in the first place.
//
// The parent directory each leaf is about to be written into is never
// created or chowned by a path-based os.MkdirAll/os.Chown, both of which
// silently follow a symlink swapped into any component: a workload that
// plants, say, homeDir/.scion -> /etc ahead of a restart could otherwise
// have root chown /etc to the workload's own uid before the leaf write's own
// guard ever runs. Instead, a target under homeDir is created and chowned
// component-by-component via dirfd.EnsureDirNoFollowUnderRoot, which never
// follows a symlink at any level and only chowns a component it creates
// itself. A target outside homeDir (a legitimate operator-configured
// absolute path) is created the ordinary way and never chowned to the
// workload — matching the ownership boundary the leaf write itself
// enforces via the uid/gid it is given.
func Write(homeDir string, staged *Staged) error {
	var uid, gid int
	if os.Getuid() == 0 {
		if uidStr := os.Getenv("SCION_HOST_UID"); uidStr != "" {
			if id, err := rootexec.ValidWorkloadID(uidStr, true); err == nil {
				uid = int(id)
			}
		}
		if gidStr := os.Getenv("SCION_HOST_GID"); gidStr != "" {
			if id, err := rootexec.ValidWorkloadID(gidStr, true); err == nil {
				gid = int(id)
			}
		}
	}
	return writeAs(homeDir, staged, uid, gid)
}

// writeAs is Write with its resolved workload uid/gid as parameters, so a
// test can exercise the chown-vs-no-chown behavior directly (chowning to the
// test process's own current uid/gid, which an unprivileged process can
// always do) without needing to run the test itself as uid 0 — the only way
// Write's own os.Getuid() == 0 gate would otherwise ever pass uid/gid
// through at all.
func writeAs(homeDir string, staged *Staged, uid, gid int) error {
	for _, fs := range staged.FileSecrets {
		data, err := base64.StdEncoding.DecodeString(fs.Value)
		if err != nil {
			return fmt.Errorf("failed to base64-decode secret %s: %w", fs.Name, err)
		}

		dir := filepath.Dir(fs.Target)
		underHome, derr := dirfd.EnsureDirNoFollowUnderRoot(homeDir, dir, 0755, uid, gid)
		if derr != nil {
			return fmt.Errorf("failed to create directory for secret %s: %w", fs.Name, derr)
		}
		fileUID, fileGID := uid, gid
		if !underHome {
			// dir does not resolve under homeDir at all: this is an
			// operator-configured target outside the agent home (e.g.
			// /etc/ssl/private/key.pem), created as at base, and never
			// chowned to the workload — only a target under homeDir is
			// eligible for that, and containment is decided by the walk
			// above, not by a string prefix check.
			if err := os.MkdirAll(dir, 0755); err != nil {
				return fmt.Errorf("failed to create directory for secret %s: %w", fs.Name, err)
			}
			fileUID, fileGID = 0, 0
		}
		// A file-secret target may be a bind-mounted path (e.g. gcloud's
		// application_default_credentials.json): renaming a freshly created
		// file over a bind-mounted regular file's directory entry fails
		// EBUSY, so this leaf is written in place when it already exists as
		// a regular file rather than replaced via create+rename.
		if err := dirfd.WriteFileNoFollow(fs.Target, data, 0600, fileUID, fileGID, dirfd.TruncateInPlaceOrCreate); err != nil {
			return fmt.Errorf("failed to write secret file %s: %w", fs.Name, err)
		}
	}

	if len(staged.VariableSecrets) > 0 {
		scionDir := filepath.Join(homeDir, ".scion")
		if _, err := dirfd.EnsureDirNoFollowUnderRoot(homeDir, scionDir, 0700, uid, gid); err != nil {
			return fmt.Errorf("failed to create .scion directory: %w", err)
		}
		data, err := json.Marshal(staged.VariableSecrets)
		if err != nil {
			return fmt.Errorf("failed to marshal secrets.json: %w", err)
		}
		secretsPath := filepath.Join(scionDir, "secrets.json")
		if err := dirfd.WriteFileNoFollow(secretsPath, data, 0600, uid, gid, dirfd.RefuseSymlink); err != nil {
			return fmt.Errorf("failed to write secrets.json: %w", err)
		}
	}

	return nil
}
