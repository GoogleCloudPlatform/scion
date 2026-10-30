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

package agent

import (
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
)

// The reprovision running-container guard (mayBeProjectsEntry) and target
// selection (matchAgentProject) must differ when an entry's project is
// unknown. The guard refuses when in doubt, so it counts such an entry as
// possibly the project's. Selection does not act when in doubt, so it does
// not match it. Both reject an entry positively identified as another
// project. Unifying the two predicates would make one of them unsafe.
func TestProjectPredicates_OppositeDefaultsWhenUnknown(t *testing.T) {
	const projectName = "proj-b"
	unknown := api.AgentInfo{Name: "dev", ContainerID: "cid-unknown"}
	other := api.AgentInfo{Name: "dev", ContainerID: "cid-other", ProjectID: identityProjA,
		Labels: map[string]string{"scion.project_id": identityProjA}}

	if !mayBeProjectsEntry(unknown, projectName, identityProjB) {
		t.Errorf("mayBeProjectsEntry(no project info) = false, want true: the guard must count an entry it cannot place")
	}
	if matchAgentProject(unknown, projectName, identityProjB) {
		t.Errorf("matchAgentProject(no project info) = true, want false: selection must not act on an entry it cannot place")
	}
	if mayBeProjectsEntry(other, projectName, identityProjB) {
		t.Errorf("mayBeProjectsEntry(entry of another project) = true, want false")
	}
	if matchAgentProject(other, projectName, identityProjB) {
		t.Errorf("matchAgentProject(entry of another project) = true, want false")
	}
}
