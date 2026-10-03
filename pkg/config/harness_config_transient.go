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

package config

import "path/filepath"

// HarnessConfigTransientPatterns are basename glob patterns (filepath.Match
// syntax) for local-only files that harness-config maintenance leaves inside a
// harness-config directory. They are not part of the config: they must not be
// uploaded by harness-config sync and must not count towards the revision.
//
//   - "*.bak.[0-9]*": backups written by backupFile as
//     <file>.bak.<UTC timestamp 20060102T150405Z>, e.g.
//     config.yaml.bak.20261003T193320Z or provision.py.bak.20261003T193320Z.
//     The leading digit keeps ordinary names such as notes.bak.md included.
//   - ".*.tmp-*": leftovers of an interrupted atomic write, which creates
//     os.CreateTemp(dir, "."+<file>+".tmp-*"), e.g. .provision.py.tmp-123456.
//
// The patterns are matched against the basename, so they apply at any depth.
// They are in the format transfer.CollectFiles accepts as exclude patterns, so
// file collection and IsHarnessConfigTransientFile share one definition.
var HarnessConfigTransientPatterns = []string{
	"*.bak.[0-9]*",
	".*.tmp-*",
}

// IsHarnessConfigTransientFile reports whether the file at path (only its
// basename is considered) is a backup or atomic-write temp file that must be
// excluded from harness-config sync and from ComputeHarnessConfigRevision.
func IsHarnessConfigTransientFile(path string) bool {
	base := filepath.Base(path)
	for _, pattern := range HarnessConfigTransientPatterns {
		if matched, _ := filepath.Match(pattern, base); matched {
			return true
		}
	}
	return false
}
