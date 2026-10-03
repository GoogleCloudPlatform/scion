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

package api

// IsWorkspaceMarker reports whether name is a top-level workspace entry
// that does not make a workspace populated: the provisioning marker
// directories and the mount points of shared directories (.scion,
// .scion-volumes, .agents). The clone step in sciontool init and the
// clone-per-agent provisioning step both decide whether a workspace is
// populated with this rule.
func IsWorkspaceMarker(name string) bool {
	switch name {
	case ".scion", ".scion-volumes", ".agents":
		return true
	}
	return false
}
