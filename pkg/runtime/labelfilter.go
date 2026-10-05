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

package runtime

import "github.com/GoogleCloudPlatform/scion/pkg/projectkeys"

// LabelsMatchFilter reports whether labels satisfy every key/value pair in
// filter. It is the single matcher for List's label filter: the Docker,
// Podman and Apple runtimes apply it to each container's labels, and
// agent.AgentManager.List applies it to the label set a created (not yet
// started) agent's container would carry, so the two paths cannot drift.
//
// Each key is compared with projectkeys.LabelValuesMatch: the project path
// is compared as a resolved path, every other key exactly. A key that is
// absent from labels reads as "" and therefore never matches a non-empty
// wanted value. An empty or nil filter matches everything.
func LabelsMatchFilter(labels, filter map[string]string) bool {
	for k, want := range filter {
		if !projectkeys.LabelValuesMatch(k, labels[k], want) {
			return false
		}
	}
	return true
}
