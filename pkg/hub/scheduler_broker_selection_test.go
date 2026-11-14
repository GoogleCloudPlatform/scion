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
	"encoding/json"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/config/opsettings"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ptone/scion#7868: a scheduled dispatch_agent fire selects its runtime
// broker exactly as an interactive create with no explicit broker does
// (resolveRuntimeBroker cases 2-4, selectProjectRuntimeBroker), under the
// revision principal. Each case fires the schedule and runs the interactive
// resolver for the same project and identity, and requires both to agree.

// addSelectionBroker registers a broker as a provider of f.proj.
func addSelectionBroker(t *testing.T, f *bypassAgentsFixture, name string, status string, autoProvide bool, createdBy string) *store.RuntimeBroker {
	t.Helper()
	ctx := context.Background()
	b := &store.RuntimeBroker{
		ID:          uuid.New().String(),
		Name:        name,
		Slug:        name,
		Status:      status,
		AutoProvide: autoProvide,
		CreatedBy:   createdBy,
		Created:     time.Now(),
		Updated:     time.Now(),
	}
	require.NoError(t, f.store.CreateRuntimeBroker(ctx, b))
	require.NoError(t, f.store.AddProjectProvider(ctx, &store.ProjectProvider{
		ProjectID:  f.proj.ID,
		BrokerID:   b.ID,
		BrokerName: b.Name,
		LocalPath:  fixtureProviderCheckout(t),
		Status:     status,
	}))
	return b
}

func setSelectionProjectDefault(t *testing.T, f *bypassAgentsFixture, brokerID string) {
	t.Helper()
	ctx := context.Background()
	proj, err := f.store.GetProject(ctx, f.proj.ID)
	require.NoError(t, err)
	proj.DefaultRuntimeBrokerID = brokerID
	require.NoError(t, f.store.UpdateProject(ctx, proj))
	got, err := f.store.GetProject(ctx, f.proj.ID)
	require.NoError(t, err)
	require.Equal(t, brokerID, got.DefaultRuntimeBrokerID)
}

// interactiveSelection runs the interactive create resolver with no explicit
// broker, under the identity the scheduled fire runs as. It returns the
// selected broker ID, or the refusal message.
func interactiveSelection(t *testing.T, f *bypassAgentsFixture, evt store.ScheduledEvent) (string, string) {
	t.Helper()
	ctx := context.Background()
	creator, err := f.srv.authorizeScheduledAgentCreate(ctx, evt)
	require.NoError(t, err)
	proj, err := f.store.GetProject(ctx, f.proj.ID)
	require.NoError(t, err)
	rec := httptest.NewRecorder()
	id, err := f.srv.resolveRuntimeBroker(contextWithIdentity(ctx, creator.Identity), rec, "", proj)
	if err == nil {
		return id, ""
	}
	var body struct {
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body), rec.Body.String())
	require.NotEmpty(t, body.Error.Message, rec.Body.String())
	return "", body.Error.Message
}

func TestScheduledDispatch_RuntimeBrokerSelectionMatchesInteractiveCreate(t *testing.T) {
	t.Parallel()

	type outcome struct {
		brokerID string // expected selection, or "" for a refusal
		refusal  string // expected refusal message
	}

	cases := []struct {
		name  string
		setup func(t *testing.T, f *bypassAgentsFixture) outcome
	}{
		{
			// The project default wins over whichever provider is listed
			// first; before ptone/scion#7868 the fire took providers[0].
			name: "project default honoured over first provider",
			setup: func(t *testing.T, f *bypassAgentsFixture) outcome {
				addSelectionBroker(t, f, "sel-second", store.BrokerStatusOnline, true, "")
				providers, err := f.store.GetProjectProviders(context.Background(), f.proj.ID)
				require.NoError(t, err)
				require.Len(t, providers, 2)
				def := providers[len(providers)-1].BrokerID
				require.NotEqual(t, providers[0].BrokerID, def)
				setSelectionProjectDefault(t, f, def)
				return outcome{brokerID: def}
			},
		},
		{
			// An offline default is not replaced by another provider.
			name: "offline project default refuses without fallback",
			setup: func(t *testing.T, f *bypassAgentsFixture) outcome {
				off := addSelectionBroker(t, f, "sel-offline", store.BrokerStatusOffline, true, "")
				setSelectionProjectDefault(t, f, off.ID)
				return outcome{refusal: "Default runtime broker is unavailable; specify an alternative"}
			},
		},
		{
			// A default the revision principal may not dispatch to (not
			// auto-provide, no owner consent on the provider row, no
			// broker.dispatch) is refused, as on interactive create.
			name: "project default the principal may not use refuses",
			setup: func(t *testing.T, f *bypassAgentsFixture) outcome {
				other := &store.User{
					ID: uuid.New().String(), Email: "sel-other@example.com", DisplayName: "Other",
					Role: store.UserRoleMember, Status: "active", Created: time.Now(),
				}
				require.NoError(t, f.store.CreateUser(context.Background(), other))
				priv := addSelectionBroker(t, f, "sel-private", store.BrokerStatusOnline, false, other.ID)
				setSelectionProjectDefault(t, f, priv.ID)
				return outcome{refusal: "Default runtime broker is unavailable; specify an alternative"}
			},
		},
		{
			name: "no project default uses the hub default",
			setup: func(t *testing.T, f *bypassAgentsFixture) outcome {
				hubDef := addSelectionBroker(t, f, "sel-hub-default", store.BrokerStatusOnline, true, "")
				setSelectionProjectDefault(t, f, "")
				setHubAgentDefaults(f.srv, opsettings.AgentDefaultsSettings{DefaultRuntimeBroker: hubDef.Slug})
				return outcome{brokerID: hubDef.ID}
			},
		},
		{
			name: "no default and a single provider uses it",
			setup: func(t *testing.T, f *bypassAgentsFixture) outcome {
				setSelectionProjectDefault(t, f, "")
				return outcome{brokerID: f.broker.ID}
			},
		},
		{
			// Several usable providers and no default: the interactive path
			// asks for an explicit choice; a schedule cannot make one.
			name: "no default and several usable providers refuses",
			setup: func(t *testing.T, f *bypassAgentsFixture) outcome {
				addSelectionBroker(t, f, "sel-another", store.BrokerStatusOnline, true, "")
				setSelectionProjectDefault(t, f, "")
				return outcome{refusal: "Multiple runtime brokers available for this project; specify runtimeBrokerId to select one"}
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := bypassAgentsSetup(t)
			want := tc.setup(t, f)

			const agentName = "sched-broker-select"
			err := fireScheduledDispatchAsOwner(t, f, agentName)
			evt := withSessionRevision(store.ScheduledEvent{
				ID: "evt-" + agentName, ProjectID: f.proj.ID, EventType: "dispatch_agent",
				CreatedBy: f.owner.ID,
			}, f.owner.ID)
			gotInteractive, interactiveRefusal := interactiveSelection(t, f, evt)

			agent, getErr := f.store.GetAgentBySlug(context.Background(), f.proj.ID, agentName)
			if want.refusal != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), want.refusal)
				assert.Equal(t, want.refusal, interactiveRefusal, "interactive create must refuse the same way")
				assert.Empty(t, gotInteractive)
				assert.ErrorIs(t, getErr, store.ErrNotFound, "a refused fire must not leave an agent row")
				return
			}
			require.NoError(t, err)
			require.NoError(t, getErr)
			assert.Equal(t, want.brokerID, agent.RuntimeBrokerID, "scheduled dispatch broker")
			assert.Equal(t, want.brokerID, gotInteractive, "interactive create must pick the same broker")
			assert.Empty(t, interactiveRefusal)
		})
	}
}
