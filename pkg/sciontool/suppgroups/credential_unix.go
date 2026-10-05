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

// Credential returns the credential for a privilege drop to uid/gid that
// keeps the runtime-granted nfs shared-dir groups (FromEnv). Groups is
// never nil: an empty, non-nil slice states explicitly that every other
// supplementary group is cleared, which is what Go does for nil as well.
// Use it at every site where sciontool drops to the agent user, so a hook
// or service writing into a shared dir behaves like the harness.
func Credential(uid, gid uint32) *syscall.Credential {
	groups := FromEnv()
	if groups == nil {
		groups = []uint32{}
	}
	return &syscall.Credential{Uid: uid, Gid: gid, Groups: groups}
}
