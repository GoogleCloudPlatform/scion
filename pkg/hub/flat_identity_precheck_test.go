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
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// TestDispatch_FlatProfileFreeAssignSkipsHubPrecheck: a flat Runtime Broker
// sends no per-profile ServiceAccount report and its agents carry no
// profile, so the Hub's per-profile identity precheck has nothing to judge:
// a profile-free GCP assign dispatch to a flat row is not refused by the
// Hub and reaches the broker, which makes the mapping decision
// (ptone/scion#3274 amendment r6).
func TestDispatch_FlatProfileFreeAssignSkipsHubPrecheck(t *testing.T) {
	ctx := context.Background()
	for _, op := range precheckOps(ctx) {
		t.Run(op.name, func(t *testing.T) {
			d, m, a, s := precheckDispatcher(t, precheckGSA, false, func(a *store.Agent) {
				a.AppliedConfig.Profile = ""
			})
			// The flat row is created with its runtime target; the
			// target is fixed at creation and never updated.
			// It also brings a complete, fresh Kubernetes profile report
			// (and default profile) that omits the GSA: a profile dispatch
			// naming it would be refused, a profile-free one is not.
			broker := &store.RuntimeBroker{
				ID: tid("flat-k8s"), Name: "flat-k8s", Slug: "flat-k8s", Endpoint: "http://localhost:9801",
				Status:         store.BrokerStatusOnline,
				Profiles:       []store.BrokerProfile{precheckProfile(time.Now())},
				DefaultProfile: "gke",
				RuntimeTarget:  &api.RuntimeTargetDescriptor{ID: "flat-k8s-target", Type: "kubernetes"},
			}
			require.NotNil(t, kubernetesIdentityNotMapped(broker, "gke", a.AppliedConfig.GCPIdentity, time.Now()),
				"control: the report refuses a dispatch naming its profile")
			require.Nil(t, kubernetesIdentityNotMapped(broker, "", a.AppliedConfig.GCPIdentity, time.Now()),
				"a profile-free dispatch is not judged against the report, nor the default profile")
			require.NoError(t, s.CreateRuntimeBroker(ctx, broker))
			stored, err := s.GetRuntimeBroker(ctx, broker.ID)
			require.NoError(t, err)
			require.Empty(t, stored.Profiles, "a flat row stores no profiles")
			a.RuntimeBrokerID = broker.ID
			// A valid pin: pinned to the Runtime Broker the agent is assigned to.
			a.PinnedRuntimeBrokerID = broker.ID
			a.PinnedRuntimeTargetID, a.PinnedRuntimeTargetType = broker.RuntimeTarget.ID, broker.RuntimeTarget.Type
			require.True(t, a.PinValid())

			err = op.run(d, a)
			_, isIdentity := identityMappingDispatchError(err)
			assert.False(t, isIdentity, "no Hub identity refusal: %v", err)
			assert.True(t, op.called(m), "the broker is called and decides")
		})
	}
}
