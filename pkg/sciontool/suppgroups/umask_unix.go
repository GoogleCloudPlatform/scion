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

//go:build unix

package suppgroups

import "syscall"

// umask is syscall.Umask, replaceable in tests so they never change the
// test process's own umask.
var umask = syscall.Umask

// ShouldApplyUmask reports whether the runtime granted any nfs shared-dir
// groups, using exactly FromEnv's validation (env list ∩ this process's
// supplementary groups, never 0).
func ShouldApplyUmask() bool {
	return len(FromEnv()) > 0
}

// ApplySharedDirUmask clears the group bits of this process's umask when
// ShouldApplyUmask (022 becomes 002, 077 becomes 007, 002 stays 002), so
// every child (harness, services, lifecycle hooks, the provision wrapper,
// substrate exec) creates group-writable files while a stricter image
// umask keeps its other-bits. It returns whether it applied, and the
// previous and new umask. When no group was granted it changes nothing.
func ApplySharedDirUmask() (applied bool, previous, current int) {
	if !ShouldApplyUmask() {
		return false, 0, 0
	}
	// Read the current mask by setting the strictest one (0777), never 0:
	// under substrate-serve the exec endpoint can fork while this runs, and
	// a child forked between the two calls keeps that mask for life, so
	// the moment must be stricter, not looser.
	previous = umask(0o777)
	current = previous &^ sharedDirUmaskGroupBits
	umask(current)
	return true, previous, current
}
