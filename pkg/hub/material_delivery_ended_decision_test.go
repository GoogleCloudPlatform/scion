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
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// TestDispatchDelivery_EndedRequestStillRefusesSuspendedRoot: a restart
// selection under an ended request runs its delivery decisions in full
// (ptone/scion#6012: the checks run on their merits; nothing is skipped or
// treated as allowed). The same item under the same ended request is first
// delivered through the legacy fallback, so no request-end refusal applies
// (a); once the root user is suspended it is refused (b). Together (a) and
// (b) pin the refusal to the suspension; (c) adds that it is not a tagged
// resolution or ceiling fault (Decision.IsIndeterminate).
func TestDispatchDelivery_EndedRequestStillRefusesSuspendedRoot(t *testing.T) {
	m := newLDFixture(t, "lg-ended-suspended", ldOptions{noEdge: true})
	v := m.env(t, "LG_ENDED_SUSPENDED_ENV", "v", store.ScopeProject, m.agent.ProjectID, false)
	ended := endedCtx{context.Background()}

	// (a)
	ctl := newDeliverySelector(m.d.authzService, m.agent, PurposeRestart)
	env, _, err := m.d.selectDeliveryEnv(ended, ctl, m.agent, nil)
	require.NoError(t, err)
	require.Equal(t, "v", env[v.Key], "before the suspension the ended run delivers the item: no request-end refusal")

	// (b)
	setUserStatus(t, m.store, m.projectOwnerID, store.UserStatusSuspended)

	sel := newDeliverySelector(m.d.authzService, m.agent, PurposeRestart)
	env, _, err = m.d.selectDeliveryEnv(ended, sel, m.agent, nil)
	require.NoError(t, err)
	assert.NotContains(t, env, v.Key, "a suspended root user is refused after the request ended")
	assert.Positive(t, sel.denied, "the item was decided and denied")

	// (c): the same decision under the selection's credential, on a context
	// the request's end does not reach, is refused and is not a tagged
	// resolution or ceiling fault. (a) and (b) above are what pin the refusal
	// to the suspension.
	require.NotNil(t, sel.ident, "the selection minted its credential")
	grant, ok := grantForDeliveryScope(store.ScopeProject)
	require.True(t, ok)
	res := materialDeliveryResource(MaterialKindEnvVar, materialRecordEnvVar, v.ID, v.Key, store.ScopeProject, m.agent.ProjectID, m.agent.ID, grant, PurposeRestart)
	d := m.d.authzService.Decide(context.WithoutCancel(ended), deliveryAuthzRequest(sel.ident, MaterialKindEnvVar, res))
	assert.False(t, d.Allowed, "the suspended root user is refused")
	assert.False(t, d.IsIndeterminate(), "not a tagged resolution or ceiling fault: deny_cause %q, reason %q", d.DenyCause, d.Reason)
}
