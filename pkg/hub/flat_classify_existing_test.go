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

//go:build !no_sqlite

package hub

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
)

// TestClassifyExistingAgent_MatchesHandleExistingAgent pins the read-only
// classification the flat checks rely on against handleExistingAgent's real
// behaviour, observed through the dispatcher: a start dispatched means
// existingBranchStart, a delete dispatched means existingBranchRecreate, and
// neither means existingBranchNone. A change to handleExistingAgent's
// decision tree that classifyExistingAgent does not follow fails here, so
// the flat checks cannot silently stop covering a branch.
func TestClassifyExistingAgent_MatchesHandleExistingAgent(t *testing.T) {
	phases := []state.Phase{state.PhaseCreated, state.PhaseProvisioning, state.PhaseRunning,
		state.PhaseStopped, state.PhaseError, state.PhaseSuspended}
	type flags struct {
		name                                    string
		resume, force, gatherEnv, provisionOnly bool
	}
	variants := []flags{
		{name: "plain"},
		{name: "resume", resume: true},
		{name: "resume+force", resume: true, force: true},
		{name: "gatherEnv", gatherEnv: true},
		{name: "provisionOnly", provisionOnly: true},
	}
	f := newFlatHubFixture(t, flatHubOpts{experimentOn: true})
	for _, phase := range phases {
		for _, v := range variants {
			t.Run(fmt.Sprintf("%s/%s", phase, v.name), func(t *testing.T) {
				slug := fmt.Sprintf("cls-%s-%s", phase, v.name)
				slug = tidSlugSafe(slug)
				a := f.unpinnedAgentOn(t, slug, f.legacy.ID, string(phase))
				f.client.startCalled, f.client.deleteCalled, f.client.createCalled = false, false, false
				req := CreateAgentRequest{Name: slug, Resume: v.resume, ForceResume: v.force, GatherEnv: v.gatherEnv, ProvisionOnly: v.provisionOnly}
				want := classifyExistingAgent(a, req)
				body := map[string]interface{}{"name": slug, "task": "t", "runtimeBrokerId": f.legacy.ID,
					"resume": v.resume, "forceResume": v.force, "gatherEnv": v.gatherEnv, "provisionOnly": v.provisionOnly}
				_ = f.create(t, body)
				var got existingAgentBranch
				switch {
				case f.client.deleteCalled:
					got = existingBranchRecreate
				case f.client.startCalled:
					got = existingBranchStart
				default:
					got = existingBranchNone
				}
				assert.Equal(t, want, got, "classifyExistingAgent disagrees with handleExistingAgent (start=%v delete=%v create=%v)",
					f.client.startCalled, f.client.deleteCalled, f.client.createCalled)
			})
		}
	}
}
