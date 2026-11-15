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
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/agentcontrol"
	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/hub/authzop"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The fail-closed response serializer (ptone/scion#7883, D3, SEC-P1,
// SEC-P5, AC7, AC16): no /schedules, /scheduled-events or /admin/scheduler
// response carries exec argv for a caller without live agent.attach on the
// target, the creator included.

// guardRows are the exec rows one guard pass reads and changes.
type guardRows struct {
	eventID        string // one-shot exec event, pending
	activeSchedule string // exec schedule, active
	pausedSchedule string // exec schedule, paused
}

// seedGuardRows stores exec rows carrying sentinel in argv directly: Phase 1
// cannot author recurring exec, but the serializer must still redact it.
func seedGuardRows(t *testing.T, f *bypassAgentsFixture, sentinel string) guardRows {
	t.Helper()
	ctx := context.Background()
	p := scheduledExecPayload{AgentID: f.sibling.ID, AgentName: f.sibling.Slug, Command: []string{"sh", "-c", "curl -H " + sentinel}}
	raw, err := p.encode()
	require.NoError(t, err)

	evt := withSessionRevision(store.ScheduledEvent{
		ID: api.NewUUID(), ProjectID: f.proj.ID, EventType: scheduledEventTypeExec,
		FireAt: time.Now().Add(time.Hour), Payload: raw, Status: store.ScheduledEventPending, CreatedBy: f.owner.ID,
	}, f.owner.ID)
	require.NoError(t, f.store.CreateScheduledEvent(ctx, &evt))

	mkSchedule := func(status string) string {
		next := time.Now().Add(time.Hour).UTC()
		sc := &store.Schedule{
			ID: api.NewUUID(), ProjectID: f.proj.ID, Name: "exec-guard-" + uuid.NewString()[:8],
			CronExpr: "0 * * * *", EventType: scheduledEventTypeExec, Payload: raw, Status: status,
			NextRunAt: &next, CreatedBy: f.owner.ID,
			InitiatorAttribution: store.InitiatorAttribution{
				InitiatorPrincipalKind: store.DelegationPrincipalUser, InitiatorPrincipalID: f.owner.ID,
				InitiatorCredentialKind: store.InitiatorCredentialKindSession, AttributionVersion: 1, AuthorizationRevision: 1,
			},
			AuthorityCeiling: store.EffectCeiling{Kind: store.EffectCeilingPrincipal},
		}
		require.NoError(t, f.store.CreateSchedule(ctx, sc))
		// A fired event of the schedule, for /history.
		fired := time.Now()
		hist := withSessionRevision(store.ScheduledEvent{
			ID: api.NewUUID(), ProjectID: f.proj.ID, EventType: scheduledEventTypeExec, ScheduleID: sc.ID,
			FireAt: fired, Payload: raw, Status: store.ScheduledEventPending, CreatedBy: f.owner.ID,
		}, f.owner.ID)
		require.NoError(t, f.store.CreateScheduledEvent(ctx, &hist))
		require.NoError(t, f.store.UpdateScheduledEventStatus(ctx, hist.ID, store.ScheduledEventFired, &fired, ""))
		return sc.ID
	}
	return guardRows{eventID: evt.ID, activeSchedule: mkSchedule(store.ScheduleStatusActive), pausedSchedule: mkSchedule(store.ScheduleStatusPaused)}
}

// guardCase issues one catalogued route against rows.
type guardCase struct {
	path func(f *bypassAgentsFixture, r guardRows) string
	body func(f *bypassAgentsFixture) any
}

func guardCases() map[string]guardCase {
	base := func(f *bypassAgentsFixture) string { return "/api/v1/projects/" + f.proj.ID }
	return map[string]guardCase{
		"GET /api/v1/projects/{projectId}/scheduled-events": {
			path: func(f *bypassAgentsFixture, _ guardRows) string { return base(f) + "/scheduled-events" },
		},
		"POST /api/v1/projects/{projectId}/scheduled-events": {
			path: func(f *bypassAgentsFixture, _ guardRows) string { return base(f) + "/scheduled-events" },
			body: func(f *bypassAgentsFixture) any {
				return map[string]any{"eventType": "exec", "agentId": f.sibling.ID, "command": []string{"true"}, "fireIn": "1h"}
			},
		},
		"GET /api/v1/projects/{projectId}/scheduled-events/{id}": {
			path: func(f *bypassAgentsFixture, r guardRows) string { return base(f) + "/scheduled-events/" + r.eventID },
		},
		"DELETE /api/v1/projects/{projectId}/scheduled-events/{id}": {
			path: func(f *bypassAgentsFixture, r guardRows) string { return base(f) + "/scheduled-events/" + r.eventID },
		},
		"GET /api/v1/projects/{projectId}/schedules": {
			path: func(f *bypassAgentsFixture, _ guardRows) string { return base(f) + "/schedules" },
		},
		"POST /api/v1/projects/{projectId}/schedules": {
			path: func(f *bypassAgentsFixture, _ guardRows) string { return base(f) + "/schedules" },
			body: func(f *bypassAgentsFixture) any {
				return map[string]any{"name": "exec-guard-new", "cronExpr": "0 * * * *", "eventType": "exec",
					"payload": `{"agentId":"` + f.sibling.ID + `","command":["true"]}`}
			},
		},
		"GET /api/v1/projects/{projectId}/schedules/{id}": {
			path: func(f *bypassAgentsFixture, r guardRows) string { return base(f) + "/schedules/" + r.activeSchedule },
		},
		"PATCH /api/v1/projects/{projectId}/schedules/{id}": {
			path: func(f *bypassAgentsFixture, r guardRows) string { return base(f) + "/schedules/" + r.activeSchedule },
			body: func(*bypassAgentsFixture) any {
				return map[string]any{"name": "exec-guard-renamed-" + uuid.NewString()[:8]}
			},
		},
		"GET /api/v1/projects/{projectId}/schedules/{id}/history": {
			path: func(f *bypassAgentsFixture, r guardRows) string {
				return base(f) + "/schedules/" + r.activeSchedule + "/history"
			},
		},
		"POST /api/v1/projects/{projectId}/schedules/{id}/pause": {
			path: func(f *bypassAgentsFixture, r guardRows) string {
				return base(f) + "/schedules/" + r.activeSchedule + "/pause"
			},
		},
		"POST /api/v1/projects/{projectId}/schedules/{id}/resume": {
			path: func(f *bypassAgentsFixture, r guardRows) string {
				return base(f) + "/schedules/" + r.pausedSchedule + "/resume"
			},
		},
		"DELETE /api/v1/projects/{projectId}/schedules/{id}": {
			path: func(f *bypassAgentsFixture, r guardRows) string { return base(f) + "/schedules/" + r.pausedSchedule },
		},
		"GET /api/v1/admin/scheduler": {
			path: func(*bypassAgentsFixture, guardRows) string { return "/api/v1/admin/scheduler" },
		},
	}
}

// guardOrder runs reads before the writes that change or remove rows.
var guardOrder = []string{
	"GET /api/v1/projects/{projectId}/scheduled-events",
	"GET /api/v1/projects/{projectId}/scheduled-events/{id}",
	"GET /api/v1/projects/{projectId}/schedules",
	"GET /api/v1/projects/{projectId}/schedules/{id}",
	"GET /api/v1/projects/{projectId}/schedules/{id}/history",
	"GET /api/v1/admin/scheduler",
	"POST /api/v1/projects/{projectId}/scheduled-events",
	"POST /api/v1/projects/{projectId}/schedules",
	"PATCH /api/v1/projects/{projectId}/schedules/{id}",
	"POST /api/v1/projects/{projectId}/schedules/{id}/pause",
	"POST /api/v1/projects/{projectId}/schedules/{id}/resume",
	"DELETE /api/v1/projects/{projectId}/scheduled-events/{id}",
	"DELETE /api/v1/projects/{projectId}/schedules/{id}",
}

// scheduledCatalogRoutes returns every catalogued HTTP entry point of the
// schedule surfaces, as "METHOD pattern".
func scheduledCatalogRoutes() map[string]bool {
	out := map[string]bool{}
	for _, op := range authzop.Catalog {
		for _, ep := range op.EntryPoints {
			if ep.Kind != authzop.EntryPointHTTPRoute {
				continue
			}
			if strings.Contains(ep.Pattern, "/schedules") || strings.Contains(ep.Pattern, "/scheduled-events") ||
				ep.Pattern == "/api/v1/admin/scheduler" {
				out[ep.Method+" "+ep.Pattern] = true
			}
		}
	}
	return out
}

// TestScheduledResponses_RouteEnumerationGuard: every catalogued schedule
// route has a case here, and none returns the exec argv to a caller without
// live attach.
func TestScheduledResponses_RouteEnumerationGuard(t *testing.T) {
	cases := guardCases()
	catalogued := scheduledCatalogRoutes()
	require.NotEmpty(t, catalogued)
	for key := range catalogued {
		if _, ok := cases[key]; !ok {
			t.Errorf("catalogued schedule route %q has no guard case: add one so its response is proven redacted", key)
		}
	}
	for key := range cases {
		if !catalogued[key] {
			t.Errorf("guard case %q names no catalogued route", key)
		}
	}
	require.Len(t, guardOrder, len(cases))

	scheduledPerms := []string{
		"scheduled_event.read", "scheduled_event.list", "scheduled_event.create",
		"scheduled_event.update", "scheduled_event.delete", "hub.scheduler.read", "agent.read", "project.read",
	}
	callers := []struct {
		name  string
		setup func(t *testing.T, f *bypassAgentsFixture) Identity
	}{
		{"scheduler reader without attach", func(t *testing.T, f *bypassAgentsFixture) Identity {
			return setupNonAdminUser(t, f.store, scheduledPerms)
		}},
		{"creator with a UAT without agent:attach", func(t *testing.T, f *bypassAgentsFixture) Identity {
			return NewScopedUserIdentity(authUser(f.owner), f.proj.ID, []string{
				"scheduled_event:read", "scheduled_event:list", "scheduled_event:create",
				"scheduled_event:update", "scheduled_event:delete", store.UATScopeAgentRead, store.UATScopeProjectRead,
			})
		}},
		{"creator after attach is lost", func(t *testing.T, f *bypassAgentsFixture) Identity {
			// The creator keeps scheduled_event access through a
			// system-scope role binding, and loses its project access, on
			// which the owner relationship (so attach) depends.
			bindPermsToUser(t, f.store, f.owner.ID, scheduledPerms)
			ctx := context.Background()
			bindings, err := f.store.ListRoleBindingsForPrincipal(ctx, store.RoleBindingPrincipalUser, f.owner.ID)
			require.NoError(t, err)
			for _, b := range bindings {
				if b.ScopeType == store.RoleScopeProject && b.ScopeID == f.proj.ID {
					require.NoError(t, f.store.DeleteRoleBinding(ctx, b.ID))
				}
			}
			target, err := f.store.GetAgent(ctx, f.sibling.ID)
			require.NoError(t, err)
			owner := authUser(f.owner)
			require.NotNil(t, f.srv.authorizeAgentTargetAction(context.Background(), owner, target, ActionAttach),
				"precondition: the creator lost attach on the target")
			return owner
		}},
	}
	for _, c := range callers {
		t.Run(c.name, func(t *testing.T) {
			f := execAuthoringFixture(t)
			sentinel := "EXECSENTINEL-" + uuid.NewString()
			rows := seedGuardRows(t, f, sentinel)
			identity := c.setup(t, f)
			successes := 0
			for _, key := range guardOrder {
				gc := cases[key]
				method, _, _ := strings.Cut(key, " ")
				var body any
				if gc.body != nil {
					body = gc.body(f)
				}
				rec := requestAsIdentity(t, f.srv, identity, method, gc.path(f, rows), body)
				assert.NotContains(t, rec.Body.String(), sentinel, "%s answered %d with the exec argv", key, rec.Code)
				if rec.Code < 300 {
					successes++
				}
				t.Logf("%s -> %d", key, rec.Code)
			}
			assert.Positive(t, successes, "the caller must reach at least some routes for the guard to mean anything")
		})
	}
}

// bindPermsToUser binds a system-scope role holding perms to userID.
func bindPermsToUser(t *testing.T, st store.Store, userID string, perms []string) {
	t.Helper()
	ctx := context.Background()
	rd, err := st.CreateRoleDefinition(ctx, &store.RoleDefinition{
		Name: "exec-guard-" + uuid.NewString()[:8], ScopeType: store.RoleScopeSystem, Permissions: perms,
	})
	require.NoError(t, err)
	_, err = st.CreateRoleBinding(ctx, &store.RoleBinding{
		RoleDefinitionID: rd.ID, PrincipalType: store.RoleBindingPrincipalUser, PrincipalID: userID,
		ScopeType: store.RoleScopeSystem, CreatedBy: "test-setup",
	})
	require.NoError(t, err)
}

// TestScheduledResponses_RevealAndRedactShape: an attach holder sees argv;
// any other reader gets the redacted form with argc and the digest.
func TestScheduledResponses_RevealAndRedactShape(t *testing.T) {
	f := execAuthoringFixture(t)
	sentinel := "EXECSENTINEL-" + uuid.NewString()
	rows := seedGuardRows(t, f, sentinel)
	path := "/api/v1/projects/" + f.proj.ID + "/scheduled-events/" + rows.eventID

	rec := requestAsIdentity(t, f.srv, authUser(f.owner), http.MethodGet, path, nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.Contains(t, rec.Body.String(), sentinel, "positive control: the owner holds live attach")

	reader := setupNonAdminUser(t, f.store, []string{"scheduled_event.read", "scheduled_event.list"})
	rec = requestAsIdentity(t, f.srv, reader, http.MethodGet, path, nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	require.NotContains(t, rec.Body.String(), sentinel)
	var resp struct {
		Payload string `json:"payload"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	var red redactedExecPayload
	require.NoError(t, json.Unmarshal([]byte(resp.Payload), &red))
	assert.True(t, red.CommandRedacted)
	assert.Equal(t, 3, red.Argc)
	assert.Equal(t, agentcontrol.ExecArgvDigest([]string{"sh", "-c", "curl -H " + sentinel}), red.CommandSHA256)
	assert.Equal(t, f.sibling.ID, red.AgentID)

	// The admin view never reveals, even to an attach holder.
	rev := f.srv.noExecRevealer(context.Background())
	evt, err := f.store.GetScheduledEvent(context.Background(), rows.eventID)
	require.NoError(t, err)
	out, err := json.Marshal(f.srv.scheduledEventResponse(context.Background(), rev, *evt))
	require.NoError(t, err)
	assert.NotContains(t, string(out), sentinel)
}

// TestScheduledResponses_UndecodableExecPayloadRedactsToMarker: nothing of
// a stored exec payload that does not decode is shown.
func TestScheduledResponses_UndecodableExecPayloadRedactsToMarker(t *testing.T) {
	assert.Equal(t, `{"commandRedacted":true}`, redactExecPayload(`not json EXECSENTINEL`))
	rev := &execRevealer{never: true}
	assert.Equal(t, `{"commandRedacted":true}`, rev.payloadFor(scheduledEventTypeExec, `{"command":`))
	assert.Equal(t, `{"agentId":"m"}`, rev.payloadFor("message", `{"agentId":"m"}`), "non-exec payloads are unchanged")
}
