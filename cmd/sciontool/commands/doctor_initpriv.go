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

package commands

import (
	"strconv"
	"strings"
)

// initPrivilege is doctor's reading of whether the init process (PID 1)
// runs as root. Init publishes its credential state only as root (see
// hub.PublishCredentialState), so without a published state this tells
// doctor whether the missing state is expected.
type initPrivilege int

const (
	// initPrivUnknown: not established; doctor keeps its generic WARN.
	initPrivUnknown initPrivilege = iota
	// initPrivRoot: the effective uid of PID 1 is 0. Init publishes in
	// exactly that case, so a missing state points at a failure.
	initPrivRoot
	// initPrivNonRoot: PID 1 is this sciontool binary running with no
	// uid 0, seen from the initial user namespace through procfs files
	// on the /proc mount itself (see readInitPrivilege).
	initPrivNonRoot
)

// readInitPrivilegeFn is the init privilege doctor uses, with the reason
// when it is unknown; a test seam. Unknown fails closed to the WARN.
var readInitPrivilegeFn = readInitPrivilege

// parseInitStatusUIDs returns the verdict from the Uid line (real,
// effective, saved, filesystem) of a /proc/<pid>/status read: root if the
// effective uid is 0, as init's own publish rule decides; non-root if all
// four are non-zero; unknown otherwise (another uid is 0, so the process
// could regain root) or if the line is missing, repeated or not four
// decimal fields.
func parseInitStatusUIDs(data []byte) initPrivilege {
	verdict := initPrivUnknown
	seen := false
	for _, line := range strings.Split(string(data), "\n") {
		rest, ok := strings.CutPrefix(line, "Uid:")
		if !ok {
			continue
		}
		if seen {
			return initPrivUnknown
		}
		seen = true
		fields := strings.Fields(rest)
		if len(fields) != 4 {
			return initPrivUnknown
		}
		var uids [4]uint64
		for i, f := range fields {
			uid, err := strconv.ParseUint(f, 10, 32)
			if err != nil {
				return initPrivUnknown
			}
			uids[i] = uid
		}
		switch {
		case uids[1] == 0:
			verdict = initPrivRoot
		case uids[0] != 0 && uids[2] != 0 && uids[3] != 0:
			verdict = initPrivNonRoot
		default:
			verdict = initPrivUnknown
		}
	}
	return verdict
}

// uidMapIsIdentity reports whether a /proc/<pid>/uid_map read is the
// initial user namespace's map: one line "0 0 4294967295".
func uidMapIsIdentity(data []byte) bool {
	lines := strings.Split(strings.TrimRight(string(data), "\n"), "\n")
	if len(lines) != 1 {
		return false
	}
	f := strings.Fields(lines[0])
	return len(f) == 3 && f[0] == "0" && f[1] == "0" && f[2] == "4294967295"
}
