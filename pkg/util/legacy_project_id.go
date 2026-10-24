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

package util

import (
	"strings"

	"github.com/google/uuid"
)

// legacyProjectIDNamespace is the UUID v5 namespace that project IDs of
// git-backed projects were derived in before project IDs became random
// (2026-03-29). It is pinned here, apart from the namespace HashProjectID
// uses for cache keys, so that a change there cannot move the legacy ID.
var legacyProjectIDNamespace = uuid.MustParse("a1b8e4f0-7c3d-4a1e-9f2b-6d5c8e7a0b1f")

// maxLegacyGitRemoteLen bounds the remotes LegacyGitProjectID accepts.
const maxLegacyGitRemoteLen = 255

// LegacyGitProjectID returns the project ID that a git-backed project on
// remote had before project IDs became random: UUID v5 of the normalized
// remote, as the Hub and the CLI derived it then. It exists only as a
// targeted fix for resuming an agent after its project was re-linked and
// got a new ID (ptone/scion#4476); nothing else should use it.
//
// remote must be a stored, already normalized git remote (see
// NormalizeGitRemote) of the form host[:port]/segment/segment[/...], with a
// lowercase host of [a-z0-9.-] that starts and ends with a letter or digit,
// an optional numeric port, and at least two path segments of [a-z0-9._~-],
// none of them "." or "..". Anything else is refused (ok is false): an empty
// or overlong remote, a remote that normalizes to something else, or one
// with userinfo, brackets, whitespace, escapes, a query, empty segments or a
// trailing '/'. A legacy project whose raw remote had a trailing '/' got an
// ID from that unnormalized form, which this does not reproduce.
//
// Trust assumption: this follows the legacy rule that one normalized remote
// is one project, and the Hub sends the result only while no live project
// holds the ID and no other live project shares the remote. It cannot see a
// deleted project or who owned it. If a legacy project was deleted and its
// project config directory survived on a broker, a later project on the
// same remote with the same slug can carry that directory over on resume,
// including the previous agent's home (and any credentials the harness left
// there), when the workspace's project-id names the legacy ID. That holds
// when the old workspace survived, or when a workload in the shared
// workspace writes the project-id file. The agent name is no barrier, as
// the creator picks it. Nothing on the broker records a trustworthy owner
// to match on; SCION_CREATOR was considered and rejected, as it is a
// display name that is not unique, not authenticated and set in the
// container environment.
func LegacyGitProjectID(remote string) (string, bool) {
	if remote == "" || len(remote) > maxLegacyGitRemoteLen || NormalizeGitRemote(remote) != remote {
		return "", false
	}
	parts := strings.Split(remote, "/")
	if len(parts) < 3 || !legacyRemoteHostOK(parts[0]) {
		return "", false
	}
	for _, seg := range parts[1:] {
		if !legacyRemoteSegmentOK(seg) {
			return "", false
		}
	}
	return uuid.NewSHA1(legacyProjectIDNamespace, []byte(remote)).String(), true
}

// legacyRemoteHostOK reports whether h is host or host:port for
// LegacyGitProjectID.
func legacyRemoteHostOK(h string) bool {
	host, port, hasPort := strings.Cut(h, ":")
	if hasPort {
		if port == "" {
			return false
		}
		for _, c := range port {
			if c < '0' || c > '9' {
				return false
			}
		}
	}
	if host == "" || !isLowerAlnum(rune(host[0])) || !isLowerAlnum(rune(host[len(host)-1])) {
		return false
	}
	for _, c := range host {
		if !isLowerAlnum(c) && c != '.' && c != '-' {
			return false
		}
	}
	return true
}

// legacyRemoteSegmentOK reports whether s is an accepted path segment for
// LegacyGitProjectID.
func legacyRemoteSegmentOK(s string) bool {
	if s == "" || s == "." || s == ".." {
		return false
	}
	for _, c := range s {
		if !isLowerAlnum(c) && !strings.ContainsRune("._~-", c) {
			return false
		}
	}
	return true
}

func isLowerAlnum(c rune) bool {
	return (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9')
}
