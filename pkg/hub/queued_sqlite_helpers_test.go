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

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/require"
)

// queueStop puts a in the state an offline stop leaves: intent stopped,
// container status stop_queued with the notice, and a pending stop row
// carrying the intent time. It returns the intent time.
func queueStop(t *testing.T, f *reconcileFixture, a *store.Agent, supersedes string) time.Time {
	t.Helper()
	ctx := context.Background()
	at, err := f.s.SetRunIntent(ctx, a.ID, store.RunIntentStopped)
	require.NoError(t, err)
	require.NoError(t, f.s.UpdateAgentStatus(ctx, a.ID, store.AgentStatusUpdate{
		Phase: "stopped", ContainerStatus: containerStatusStopQueued, Message: offlineStopMessage,
	}))
	args, err := MarshalDispatchArgs(StopDispatchArgs{IntentAt: &at, SupersedesClaim: supersedes})
	require.NoError(t, err)
	require.NoError(t, f.s.InsertBrokerDispatch(ctx, &store.BrokerDispatch{
		ID: tid("qs-" + a.Slug), BrokerID: f.brokerID, AgentID: a.ID, AgentSlug: a.Slug, ProjectID: f.projectID, Op: "stop", Args: args,
	}))
	return at
}

// httpOnlyBroker gives the fixture's broker an endpoint and no control
// channel, as a broker reached over HTTP only.
func httpOnlyBroker(t *testing.T, f *reconcileFixture) {
	t.Helper()
	b, err := f.s.GetRuntimeBroker(context.Background(), f.brokerID)
	require.NoError(t, err)
	b.Endpoint = "http://broker.invalid:9800"
	require.NoError(t, f.s.UpdateRuntimeBroker(context.Background(), b))
}
