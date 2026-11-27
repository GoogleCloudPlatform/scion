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

package agydecode

import (
	"regexp"
	"strings"
)

// VerifiedVersions lists the Antigravity (agy) CLI versions whose SQLite schema
// and protobuf wire layout have been verified against real conversation databases
// and pinned by CI golden fixtures.
//
// Antigravity conversation databases do not embed the agy binary version in SQLite
// metadata, so callers supply the installed agy version via Options.AgyVersion
// (for example, by running `agy --version` once through pkg/sciontool/procreap
// in the live tailer or via `--agy-version` on `sciontool internal decode-agy-db`).
// Keeping subprocess execution out of agydecode keeps this package pure and safe
// to call from PID-1 processes.
//
// Adding a version here requires verifying the schema/wire layout against that
// agy release and updating testdata fixtures if anything changed. See
// docs-site/src/content/docs/contributing/harness-cli-version-bump.md.
var VerifiedVersions = []string{
	"1.5.18",
}

// semverRe matches a canonical 3-part semver (optional leading 'v' and optional
// prerelease suffix) bounded by start/whitespace/open-paren/slash on the left
// and end/whitespace/close-paren/comma on the right. Anchoring rejects 4-part
// versions such as "agy 1.5.18.1" so unpinned builds cannot pass as pinned.
var semverRe = regexp.MustCompile(`(?:^|[\s(/])v?(\d+\.\d+\.\d+(?:-[0-9A-Za-z.-]+)?)(?:$|[\s),])`)

// NormalizeVersion extracts a canonical semver string from raw `agy --version`
// output or a caller-supplied version string (e.g. "v1.5.18\n" -> "1.5.18",
// "agy 1.5.18" -> "1.5.18"). Returns "" if no valid 3-part semver can be parsed
// (for example, "agy 1.5.18.1" returns "" and is treated as unverified).
func NormalizeVersion(raw string) string {
	s := strings.TrimSpace(raw)
	if s == "" {
		return ""
	}
	if m := semverRe.FindStringSubmatch(s); len(m) == 2 {
		return m[1]
	}
	return ""
}

// IsVerifiedVersion reports whether version matches a pinned entry in VerifiedVersions.
func IsVerifiedVersion(version string) bool {
	norm := NormalizeVersion(version)
	if norm == "" {
		return false
	}
	for _, v := range VerifiedVersions {
		if norm == v {
			return true
		}
	}
	return false
}
