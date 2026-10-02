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
	"strings"
	"syscall"

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
// absolute path, e.g. /var/run/secrets/sa.json) is created
// component-by-component via dirfd.EnsureDirTrustedAncestorFollow and never
// chowned to the workload — matching the ownership boundary the leaf write
// itself enforces via the uid/gid it is given. That walk follows a symlink
// at a component only when both the symlink and its containing directory
// are root-owned and free of the group/other-write bits — a system alias
// like "/var/run" -> "/run" that no workload can plant or redirect — and
// refuses, fatally, any other symlink along the way, so an operator path
// through ordinary system aliases still works while a workload-plantable
// redirection still fails closed.
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

		// Reject a Target with no real leaf name — a trailing path
		// separator, or a leaf that is "." or ".." — outright, before
		// anything below treats filepath.Dir/filepath.Base's result as the
		// directory/leaf to create and write. filepath.Base silently
		// strips a trailing separator (Base("a/b/") == "b"), so without
		// this check ".../secrets/sa.json/" would silently create
		// "sa.json" as a DIRECTORY and write a file "sa.json" inside it,
		// instead of failing the way base's os.WriteFile (and this
		// package's own OpenParentNoFollow, before this function existed)
		// always did for a malformed Target. filepath.Clean is applied
		// only AFTER this check — Clean alone would just as silently turn
		// the trailing slash into a different, also-wrong outcome (a
		// cleaned path with the leaf as a plain file) rather than refusing
		// it.
		if strings.HasSuffix(fs.Target, "/") {
			return fmt.Errorf("secret %s: target %q ends in a path separator, not a file name", fs.Name, fs.Target)
		}
		switch filepath.Base(fs.Target) {
		case ".", "..":
			return fmt.Errorf("secret %s: target %q has no usable file name", fs.Name, fs.Target)
		}
		target := filepath.Clean(fs.Target)

		dir := filepath.Dir(target)
		dirFd, underHome, derr := dirfd.EnsureDirNoFollowUnderRoot(homeDir, dir, 0755, uid, gid)
		if derr != nil {
			return fmt.Errorf("failed to create directory for secret %s: %w", fs.Name, derr)
		}
		fileUID, fileGID := uid, gid
		if !underHome {
			// dir does not resolve under homeDir at all: this is an
			// operator-configured target outside the agent home (e.g.
			// /var/run/secrets/sa.json, /etc/ssl/private/key.pem), never
			// chowned to the workload — only a target under homeDir is
			// eligible for that, and containment is decided by the walk
			// above, not by a string prefix check. dir's own chain is
			// resolved via EnsureDirTrustedAncestorFollow, which replaces
			// the plain os.MkdirAll this used at base: missing components
			// are still created (0755, root-owned) exactly like MkdirAll
			// did, but a symlink at any component is followed only when it
			// and its containing directory are both root-owned and free of
			// the group/other-write bits (a system alias like "/var/run"
			// -> "/run", which no workload can plant or redirect) — never
			// an ordinary EvalSymlinks, which would also follow a
			// WORKLOAD-plantable ancestor and let root write into or
			// truncate a directory the workload chose instead of the
			// operator.
			var ferr error
			dirFd, ferr = dirfd.EnsureDirTrustedAncestorFollow(dir)
			if ferr != nil {
				return fmt.Errorf("failed to create directory for secret %s: %w", fs.Name, ferr)
			}
			fileUID, fileGID = 0, 0
		}
		// The leaf write is anchored at dirFd — the fd EnsureDirNoFollowUnderRoot
		// or EnsureDirTrustedAncestorFollow just resolved above — never by
		// re-resolving target as a string: either walk may have followed
		// a trusted symlink (homeDir's own ancestor, or an outside-home
		// system alias like "/var/run" -> "/run") that a fresh path-based
		// walk would refuse the second time around. A file-secret target
		// may also be a bind-mounted path (e.g. gcloud's
		// application_default_credentials.json): renaming a freshly
		// created file over a bind-mounted regular file's directory entry
		// fails EBUSY, so this leaf is written in place when it already
		// exists as a regular file rather than replaced via create+rename.
		werr := dirfd.WriteAtNoFollowWithChown(dirFd, filepath.Base(target), target, data, 0600, fileUID, fileGID, dirfd.TruncateInPlaceOrCreate, syscall.Fchown)
		_ = syscall.Close(dirFd)
		if werr != nil {
			return fmt.Errorf("failed to write secret file %s: %w", fs.Name, werr)
		}
	}

	if len(staged.VariableSecrets) > 0 {
		scionDir := filepath.Join(homeDir, ".scion")
		scionDirFd, _, err := dirfd.EnsureDirNoFollowUnderRoot(homeDir, scionDir, 0700, uid, gid)
		if err != nil {
			return fmt.Errorf("failed to create .scion directory: %w", err)
		}
		data, err := json.Marshal(staged.VariableSecrets)
		if err != nil {
			_ = syscall.Close(scionDirFd)
			return fmt.Errorf("failed to marshal secrets.json: %w", err)
		}
		// Anchored at scionDirFd (which may have resolved homeDir through a
		// trusted ancestor symlink), not by re-resolving secretsPath as a
		// string — see the file-secret leaf write above for why.
		werr := dirfd.WriteAtNoFollowWithChown(scionDirFd, "secrets.json", filepath.Join(scionDir, "secrets.json"), data, 0600, uid, gid, dirfd.RefuseSymlink, syscall.Fchown)
		_ = syscall.Close(scionDirFd)
		if werr != nil {
			return fmt.Errorf("failed to write secrets.json: %w", werr)
		}
	}

	return nil
}
