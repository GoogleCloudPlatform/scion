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
	"github.com/GoogleCloudPlatform/scion/pkg/experiments"
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
	outputEventID  string // one-shot exec event, finished with captured output canary
	activeSchedule string // exec schedule, active
	pausedSchedule string // exec schedule, paused
}

// seedGuardRows stores exec rows carrying sentinel in argv directly, plus a
// finished exec event with captured output carrying outputCanary.
func seedGuardRows(t *testing.T, f *bypassAgentsFixture, sentinel, outputCanary string) guardRows {
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

	// A finished one-shot exec event with captured JSON output containing outputCanary.
	pCap := scheduledExecPayload{
		AgentID: f.sibling.ID, AgentName: f.sibling.Slug,
		Command: []string{"sh", "-c", "echo " + sentinel}, CaptureOutput: agentcontrol.ExecCaptureJSON,
	}
	rawCap, err := pCap.encode()
	require.NoError(t, err)
	firedAt := time.Now().Add(-time.Minute)
	outEvt := withSessionRevision(store.ScheduledEvent{
		ID: api.NewUUID(), ProjectID: f.proj.ID, EventType: scheduledEventTypeExec,
		FireAt: firedAt, Payload: rawCap, Status: store.ScheduledEventPending, CreatedBy: f.owner.ID,
	}, f.owner.ID)
	require.NoError(t, f.store.CreateScheduledEvent(ctx, &outEvt))
	require.NoError(t, f.store.UpdateScheduledEventStatus(ctx, outEvt.ID, store.ScheduledEventFired, &firedAt, ""))
	run := store.ScheduledExecRun{
		EventID:        outEvt.ID,
		ProjectID:      f.proj.ID,
		TargetAgentID:  f.sibling.ID,
		CommandSHA256:  agentcontrol.ExecArgvDigest(pCap.Command),
		Argc:           len(pCap.Command),
		TimeoutSeconds: 60,
		StartedAt:      firedAt,
		NotAfter:       firedAt.Add(time.Minute),
		LeaseUntil:     firedAt.Add(3 * time.Minute),
		Outcome:        store.ScheduledExecOutcomeStarting,
		CaptureMode:    agentcontrol.ExecCaptureJSON,
		LaunchID:       "launch-guard",
	}
	require.NoError(t, f.store.CreateScheduledExecRun(ctx, &run))
	exitZero := 0
	dur := int64(5)
	outBytes := int64(len(outputCanary) + 12)
	won, err := f.store.FinishScheduledExecRun(ctx, store.ScheduledExecRunFinish{
		EventID:      outEvt.ID,
		FinishedAt:   time.Now(),
		Outcome:      store.ScheduledExecOutcomeOK,
		ExitCode:     &exitZero,
		DurationMS:   &dur,
		OutputBytes:  &outBytes,
		OutputStatus: agentcontrol.ExecOutputCaptured,
		Output:       []byte(`{"canary":"` + outputCanary + `"}`),
	})
	require.NoError(t, err)
	require.True(t, won)

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
	return guardRows{eventID: evt.ID, outputEventID: outEvt.ID, activeSchedule: mkSchedule(store.ScheduleStatusActive), pausedSchedule: mkSchedule(store.ScheduleStatusPaused)}
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
			path: func(f *bypassAgentsFixture, r guardRows) string {
				return base(f) + "/scheduled-events/" + r.outputEventID
			},
		},
		"GET /api/v1/projects/{projectId}/scheduled-events/{id}/output": {
			path: func(f *bypassAgentsFixture, r guardRows) string {
				return base(f) + "/scheduled-events/" + r.outputEventID + "/output"
			},
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
	"GET /api/v1/projects/{projectId}/scheduled-events/{id}/output",
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
		{"project-admin without attach", func(t *testing.T, f *bypassAgentsFixture) Identity {
			id := tid("guard-proj-admin")
			createTestUserWithProjectRole(t, f.store, id, "guard-proj-admin@test", f.proj.ID, store.ProjectRoleAdmin)
			ensureHubMembership(context.Background(), f.store, id)
			return NewAuthenticatedUser(id, "guard-proj-admin@test", "PA", store.UserRoleMember, "api")
		}},
		{"creator with a UAT without agent:attach", func(t *testing.T, f *bypassAgentsFixture) Identity {
			return NewScopedUserIdentity(authUser(f.owner), f.proj.ID, []string{
				"scheduled_event:read", "scheduled_event:list", "scheduled_event:create",
				"scheduled_event:update", "scheduled_event:delete", store.UATScopeAgentManage, store.UATScopeAgentRead, store.UATScopeProjectRead,
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
			outputCanary := "EXECOUTPUTCANARY-" + uuid.NewString()
			rows := seedGuardRows(t, f, sentinel, outputCanary)
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
				assert.NotContains(t, rec.Body.String(), outputCanary, "%s answered %d with captured output canary", key, rec.Code)
				if key == "GET /api/v1/projects/{projectId}/scheduled-events/{id}/output" {
					assert.Equal(t, http.StatusForbidden, rec.Code, "non-attach caller %s must get 403 on /output", c.name)
				}
				if rec.Code < 300 {
					successes++
				}
				t.Logf("%s -> %d", key, rec.Code)
			}
			assert.Positive(t, successes, "the caller must reach at least some routes for the guard to mean anything")

			// Also verify an attach holder NEVER sees the output canary on any
			// catalogued route OTHER than /output (A10).
			owner := authUser(f.owner)
			for _, key := range guardOrder {
				if key == "GET /api/v1/projects/{projectId}/scheduled-events/{id}/output" {
					rec := requestAsIdentity(t, f.srv, owner, http.MethodGet, cases[key].path(f, rows), nil)
					assert.Equal(t, http.StatusOK, rec.Code, "attach holder must get 200 on /output: %s", rec.Body.String())
					assert.Contains(t, rec.Body.String(), outputCanary, "positive control: attach holder receives output canary")
					assert.Equal(t, "no-store", rec.Header().Get("Cache-Control"))
					continue
				}
				method, _, _ := strings.Cut(key, " ")
				if method != http.MethodGet {
					continue
				}
				rec := requestAsIdentity(t, f.srv, owner, method, cases[key].path(f, rows), nil)
				assert.NotContains(t, rec.Body.String(), outputCanary, "catalogued route %s leaked output canary even to attach holder", key)
			}

			// R1 transition & reclassification guard cases: reclassifying or
			// re-targeting an exec row (including to an agent the caller DOES
			// own) must never leak argv or carry it forward, and stored exec
			// content must redact fail-closed regardless of claimed eventType.
			ctx := context.Background()
			ownAgent := &store.Agent{
				ID: tid("guard-own-" + uuid.NewString()[:8]), Slug: "guard-own-" + uuid.NewString()[:8],
				Name: "guard-own", ProjectID: f.proj.ID, Phase: "running",
				CreatedBy: identity.ID(), OwnerID: identity.ID(), Ancestry: []string{identity.ID()}, Runtime: "docker",
			}
			require.NoError(t, f.store.CreateAgent(ctx, ownAgent))

			transitions := []struct {
				name string
				body map[string]any
			}{
				{"switch-to-message-no-payload", map[string]any{"eventType": "message"}},
				{"switch-to-dispatch-no-payload", map[string]any{"eventType": "dispatch_agent"}},
				{"switch-to-message-with-payload", map[string]any{"eventType": "message", "payload": `{"agentId":"` + f.sibling.ID + `","message":"hi"}`}},
				{"retarget-own-agent-no-command", map[string]any{"agentId": ownAgent.ID}},
				{"retarget-own-agent-with-command", map[string]any{"agentId": ownAgent.ID, "command": []string{"true"}}},
				{"retarget-own-agent-via-payload", map[string]any{"payload": `{"agentId":"` + ownAgent.ID + `","command":["true"]}`}},
				{"patch-timeout", map[string]any{"timeoutSeconds": 120}},
			}
			basePath := "/api/v1/projects/" + f.proj.ID + "/schedules"
			for _, tr := range transitions {
				f.srv.scheduledExecLimiter()
				f.srv.scheduledExecAuthoringLimiter = newKeysRateLimiter(scheduledExecAuthoringRate, scheduledExecAuthoringBurst)
				trRows := seedGuardRows(t, f, sentinel, "TRCANARY-"+uuid.NewString())
				schedURL := basePath + "/" + trRows.activeSchedule
				rec := requestAsIdentity(t, f.srv, identity, http.MethodPatch, schedURL, tr.body)
				assert.GreaterOrEqual(t, rec.Code, http.StatusBadRequest, "transition %s must be refused (%d: %s)", tr.name, rec.Code, rec.Body.String())
				assert.NotContains(t, rec.Body.String(), sentinel, "PATCH %s leaked exec argv", tr.name)
				recGet := requestAsIdentity(t, f.srv, identity, http.MethodGet, schedURL, nil)
				assert.NotContains(t, recGet.Body.String(), sentinel, "GET after %s leaked exec argv", tr.name)
				stored, err := f.store.GetSchedule(ctx, trRows.activeSchedule)
				require.NoError(t, err)
				assert.Equal(t, scheduledEventTypeExec, stored.EventType, "transition %s changed stored eventType", tr.name)
				assert.Equal(t, 1, stored.AuthorizationRevision, "transition %s bumped revision", tr.name)
			}

			// Fail-closed serializer check: even if a stored row carries
			// EventType != "exec" while its stored payload has exec content,
			// every read/write/admin route still redacts argv.
			execRaw, err := (&scheduledExecPayload{
				AgentID: f.sibling.ID, AgentName: f.sibling.Slug, Command: []string{"sh", "-c", "curl -H " + sentinel},
			}).encode()
			require.NoError(t, err)
			next := time.Now().Add(time.Hour).UTC()
			mislabeled := &store.Schedule{
				ID: api.NewUUID(), ProjectID: f.proj.ID, Name: "exec-mislabeled-" + uuid.NewString()[:8],
				CronExpr: "0 * * * *", EventType: "message", Payload: execRaw, Status: store.ScheduleStatusActive,
				NextRunAt: &next, CreatedBy: f.owner.ID,
				InitiatorAttribution: store.InitiatorAttribution{
					InitiatorPrincipalKind: store.DelegationPrincipalUser, InitiatorPrincipalID: f.owner.ID,
					InitiatorCredentialKind: store.InitiatorCredentialKindSession, AttributionVersion: 1, AuthorizationRevision: 1,
				},
				AuthorityCeiling: store.EffectCeiling{Kind: store.EffectCeilingPrincipal},
			}
			require.NoError(t, f.store.CreateSchedule(ctx, mislabeled))
			for _, p := range []string{basePath + "/" + mislabeled.ID, basePath, "/api/v1/admin/scheduler"} {
				rec := requestAsIdentity(t, f.srv, identity, http.MethodGet, p, nil)
				assert.NotContains(t, rec.Body.String(), sentinel, "GET %s leaked mislabeled exec payload", p)
			}
			recRename := requestAsIdentity(t, f.srv, identity, http.MethodPatch, basePath+"/"+mislabeled.ID, map[string]any{"name": "mislabeled-renamed-" + uuid.NewString()[:8]})
			assert.NotContains(t, recRename.Body.String(), sentinel, "PATCH rename leaked mislabeled exec payload")
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
	rows := seedGuardRows(t, f, sentinel, "SHAPECANARY-"+uuid.NewString())
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
// a stored exec payload that does not decode is shown, and stored exec
// payload content is redacted fail-closed regardless of claimed eventType.
func TestScheduledResponses_UndecodableExecPayloadRedactsToMarker(t *testing.T) {
	assert.Equal(t, `{"commandRedacted":true}`, redactExecPayload(`not json EXECSENTINEL`))
	rev := &execRevealer{never: true}
	assert.Equal(t, `{"commandRedacted":true}`, rev.payloadFor(scheduledEventTypeExec, `{"command":`))
	assert.Equal(t, `{"commandRedacted":true}`, rev.payloadFor("message", `{"command":["EXECSENTINEL"`), "truncated exec payload under non-exec type still redacts")
	assert.NotContains(t, rev.payloadFor("message", `{"agentId":"a","command":["sh","-c","EXECSENTINEL"]}`), "EXECSENTINEL")
	assert.NotContains(t, rev.payloadFor("dispatch_agent", `{"agentId":"a","Command":["sh","-c","EXECSENTINEL"]}`), "EXECSENTINEL")
	assert.NotContains(t, rev.payloadFor("message", `{"agentId":"a","timeoutSeconds":30}`), `"timeoutSeconds":0`)
	assert.Contains(t, rev.payloadFor("message", `{"agentId":"a","timeoutSeconds":30}`), `"commandRedacted":true`)
	assert.Equal(t, `{"agentId":"m","message":"run command later"}`, rev.payloadFor("message", `{"agentId":"m","message":"run command later"}`), "non-exec payloads without exec keys are unchanged")
}

// TestScheduledResponses_RecurringAuthoringAndCanonicalDigest (AC7, AC16, Nit N1):
// real HTTP recurring /schedules authoring reveals argv only to live attach
// holders, redacts for non-attach readers (including the creator after attach
// loss or via UAT without agent:attach), and emits canonical unescaped UTF-8
// JSON commandSha256 for commands containing &, <, and >.
func TestScheduledResponses_RecurringAuthoringAndCanonicalDigest(t *testing.T) {
	f := execAuthoringFixture(t)
	sentinel := "EXECRECURRING-" + uuid.NewString()
	argv := []string{"sh", "-c", "echo a&b <in >out " + sentinel}
	schedulesPath := "/api/v1/projects/" + f.proj.ID + "/schedules"

	rec := requestAsIdentity(t, f.srv, authUser(f.owner), http.MethodPost, schedulesPath, map[string]any{
		"name":           "canonical-digest-guard",
		"cronExpr":       "0 * * * *",
		"eventType":      "exec",
		"agentId":        f.sibling.ID,
		"command":        argv,
		"timeoutSeconds": 45,
	})
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	assert.Contains(t, rec.Body.String(), sentinel, "owner holds live attach on create response")

	var created struct {
		ID      string `json:"id"`
		Payload string `json:"payload"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &created))
	require.NotEmpty(t, created.ID)

	reader := setupNonAdminUser(t, f.store, []string{
		"scheduled_event.read", "scheduled_event.list", "scheduled_event.update", "hub.scheduler.read",
	})
	schedURL := schedulesPath + "/" + created.ID

	// GET /schedules/{id} as non-attach reader -> redacted with canonical unescaped sha256.
	rec = requestAsIdentity(t, f.srv, reader, http.MethodGet, schedURL, nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	require.NotContains(t, rec.Body.String(), sentinel)
	var got struct {
		Payload string `json:"payload"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got))
	var red redactedExecPayload
	require.NoError(t, json.Unmarshal([]byte(got.Payload), &red))
	assert.True(t, red.CommandRedacted)
	assert.Equal(t, 3, red.Argc)
	assert.Equal(t, agentcontrol.ExecArgvDigest(argv), red.CommandSHA256)

	// Verify pause and metadata-only PATCH responses are also redacted for non-attach callers.
	rec = requestAsIdentity(t, f.srv, reader, http.MethodPatch, schedURL, map[string]any{"name": "canonical-digest-guard-2"})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.NotContains(t, rec.Body.String(), sentinel)

	rec = requestAsIdentity(t, f.srv, reader, http.MethodPost, schedURL+"/pause", nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.NotContains(t, rec.Body.String(), sentinel)

	// SEC-P5: creator using a UAT without agent:attach does NOT get argv revealed.
	uatNoAttach := NewScopedUserIdentity(authUser(f.owner), f.proj.ID, []string{
		"scheduled_event:read", "scheduled_event:list", store.UATScopeAgentRead, store.UATScopeProjectRead,
	})
	rec = requestAsIdentity(t, f.srv, uatNoAttach, http.MethodGet, schedURL, nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.NotContains(t, rec.Body.String(), sentinel)
}

// TestScheduledExecR1_EventTypeSwitchAndRetargetNeverLeakArgv reproduces the
// Phase 2 Pass 1 R1 probe (exec-schedules-2-rev) and its siblings:
//   - PATCH switching eventType away from exec (to message or dispatch_agent,
//     with or without a replacement payload) is refused and never leaks argv;
//   - PATCH switching eventType from message/dispatch_agent to exec is refused;
//   - PATCH retargeting an exec schedule to an agent the caller DOES own (so
//     the caller holds agent.attach on the new target, but NOT on the stored
//     target) is refused and never carries argv forward or reveals it.
func TestScheduledExecR1_EventTypeSwitchAndRetargetNeverLeakArgv(t *testing.T) {
	ctx := context.Background()
	seedExec := func(t *testing.T, f *bypassAgentsFixture, sentinel string) *store.Schedule {
		t.Helper()
		p := scheduledExecPayload{AgentID: f.sibling.ID, AgentName: f.sibling.Slug, Command: []string{"sh", "-c", "curl -H " + sentinel}}
		raw, err := p.encode()
		require.NoError(t, err)
		next := time.Now().Add(time.Hour).UTC()
		sc := &store.Schedule{
			ID: api.NewUUID(), ProjectID: f.proj.ID, Name: "r1-" + uuid.NewString()[:8],
			CronExpr: "0 * * * *", EventType: scheduledEventTypeExec, Payload: raw, Status: store.ScheduleStatusActive,
			NextRunAt: &next, CreatedBy: f.owner.ID,
			InitiatorAttribution: store.InitiatorAttribution{
				InitiatorPrincipalKind: store.DelegationPrincipalUser, InitiatorPrincipalID: f.owner.ID,
				InitiatorCredentialKind: store.InitiatorCredentialKindSession, AttributionVersion: 1, AuthorizationRevision: 1,
			},
			AuthorityCeiling: store.EffectCeiling{Kind: store.EffectCeilingPrincipal},
		}
		require.NoError(t, f.store.CreateSchedule(ctx, sc))
		return sc
	}

	callers := []struct {
		name  string
		setup func(t *testing.T, f *bypassAgentsFixture) Identity
	}{
		{"project-admin", func(t *testing.T, f *bypassAgentsFixture) Identity {
			id := tid("r1-proj-admin")
			createTestUserWithProjectRole(t, f.store, id, "r1-proj-admin@test", f.proj.ID, store.ProjectRoleAdmin)
			ensureHubMembership(ctx, f.store, id)
			return NewAuthenticatedUser(id, "r1-proj-admin@test", "PA", store.UserRoleMember, "api")
		}},
		{"owner UAT agent:manage (no agent:attach)", func(t *testing.T, f *bypassAgentsFixture) Identity {
			return NewScopedUserIdentity(authUser(f.owner), f.proj.ID,
				append([]string{store.UATScopeAgentManage, store.UATScopeAgentRead}, scheduledEventUATScopes...))
		}},
	}

	for _, newType := range []string{"message", "dispatch_agent"} {
		for _, c := range callers {
			t.Run("switch/"+newType+"/"+c.name, func(t *testing.T) {
				f := execAuthoringFixture(t)
				sentinel := "REVSENTINEL-" + uuid.NewString()
				sc := seedExec(t, f, sentinel)
				id := c.setup(t, f)
				target, err := f.store.GetAgent(ctx, f.sibling.ID)
				require.NoError(t, err)
				require.NotNil(t, f.srv.authorizeAgentTargetAction(ctx, id, target, ActionAttach),
					"precondition: caller must not hold agent.attach on stored target")

				path := "/api/v1/projects/" + f.proj.ID + "/schedules/" + sc.ID
				recBefore := requestAsIdentity(t, f.srv, id, http.MethodGet, path, nil)
				require.NotContains(t, recBefore.Body.String(), sentinel)

				rec := requestAsIdentity(t, f.srv, id, http.MethodPatch, path, map[string]any{"eventType": newType})
				require.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
				require.NotContains(t, rec.Body.String(), sentinel, "PATCH leaked exec argv to a caller without attach")

				recGet := requestAsIdentity(t, f.srv, id, http.MethodGet, path, nil)
				require.NotContains(t, recGet.Body.String(), sentinel, "GET leaked exec argv after eventType switch attempt")

				after, err := f.store.GetSchedule(ctx, sc.ID)
				require.NoError(t, err)
				require.Equal(t, scheduledEventTypeExec, after.EventType)
				require.Equal(t, 1, after.AuthorizationRevision)
			})
		}
	}

	t.Run("switch non-exec schedule to exec is refused", func(t *testing.T) {
		f := execAuthoringFixture(t)
		next := time.Now().Add(time.Hour).UTC()
		msgSc := &store.Schedule{
			ID: api.NewUUID(), ProjectID: f.proj.ID, Name: "msg-to-exec",
			CronExpr: "0 * * * *", EventType: "message",
			Payload: `{"agentId":"` + f.sibling.ID + `","message":"hello"}`,
			Status:  store.ScheduleStatusActive, NextRunAt: &next, CreatedBy: f.owner.ID,
			InitiatorAttribution: store.InitiatorAttribution{
				InitiatorPrincipalKind: store.DelegationPrincipalUser, InitiatorPrincipalID: f.owner.ID,
				InitiatorCredentialKind: store.InitiatorCredentialKindSession, AttributionVersion: 1, AuthorizationRevision: 1,
			},
			AuthorityCeiling: store.EffectCeiling{Kind: store.EffectCeilingPrincipal},
		}
		require.NoError(t, f.store.CreateSchedule(ctx, msgSc))
		path := "/api/v1/projects/" + f.proj.ID + "/schedules/" + msgSc.ID
		rec := requestAsIdentity(t, f.srv, authUser(f.owner), http.MethodPatch, path, map[string]any{
			"eventType": "exec", "agentId": f.sibling.ID, "command": []string{"true"},
		})
		require.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
		after, err := f.store.GetSchedule(ctx, msgSc.ID)
		require.NoError(t, err)
		require.Equal(t, "message", after.EventType)
	})

	t.Run("retarget to caller-owned agent without attach on stored target is refused", func(t *testing.T) {
		f := execAuthoringFixture(t)
		sentinel := "RETARGETSENTINEL-" + uuid.NewString()
		sc := seedExec(t, f, sentinel)

		paID := tid("r1-retarget-pa")
		createTestUserWithProjectRole(t, f.store, paID, "r1-retarget-pa@test", f.proj.ID, store.ProjectRoleAdmin)
		ensureHubMembership(ctx, f.store, paID)
		pa := NewAuthenticatedUser(paID, "r1-retarget-pa@test", "PA", store.UserRoleMember, "api")

		paAgent := &store.Agent{
			ID: tid("r1-pa-own-agent"), Slug: "r1-pa-own-agent", Name: "r1-pa-own-agent", ProjectID: f.proj.ID,
			Phase: "running", CreatedBy: paID, OwnerID: paID, Ancestry: []string{paID}, Runtime: "docker",
		}
		require.NoError(t, f.store.CreateAgent(ctx, paAgent))
		require.Nil(t, f.srv.authorizeAgentTargetAction(ctx, pa, paAgent, ActionAttach),
			"precondition: project-admin holds live attach on their own agent")

		path := "/api/v1/projects/" + f.proj.ID + "/schedules/" + sc.ID
		for _, body := range []map[string]any{
			{"agentId": paAgent.ID},
			{"agentName": paAgent.Slug},
			{"agentId": paAgent.ID, "command": []string{"true"}},
			{"payload": `{"agentId":"` + paAgent.ID + `","command":["true"]}`},
		} {
			rec := requestAsIdentity(t, f.srv, pa, http.MethodPatch, path, body)
			require.Contains(t, []int{http.StatusForbidden, http.StatusNotFound}, rec.Code, rec.Body.String())
			require.NotContains(t, rec.Body.String(), sentinel)
			after, err := f.store.GetSchedule(ctx, sc.ID)
			require.NoError(t, err)
			p, err := decodeScheduledExecPayload(after.Payload)
			require.NoError(t, err)
			require.Equal(t, f.sibling.ID, p.AgentID)
		}
	})
}

// TestRev2Probe_MessageScheduleWithStrayExecKey reproduces Phase 2 Pass 2 P2-R1
// (reviewer exec-schedules-2-rev-2): a message schedule whose raw payload
// carries a stray top-level timeoutSeconds/command key must remain editable
// whether hub.exec_schedules is enabled or disabled.
func TestRev2Probe_MessageScheduleWithStrayExecKey(t *testing.T) {
	for _, execOn := range []bool{true, false} {
		name := "exec flag on"
		if !execOn {
			name = "exec flag off"
		}
		t.Run(name, func(t *testing.T) {
			f := execAuthoringFixture(t)
			if !execOn {
				setHubExperiments(t, f.srv, map[string]bool{conduitExperiment: true, experiments.ExecSchedules: false})
			}
			base := "/api/v1/projects/" + f.proj.ID + "/schedules"
			rec := requestAsIdentity(t, f.srv, authUser(f.owner), http.MethodPost, base, map[string]any{
				"name": "msg-" + uuid.NewString()[:8], "cronExpr": "0 * * * *", "eventType": "message",
				"payload": `{"agentName":"` + f.sibling.Slug + `","message":"hello","timeoutSeconds":5}`,
			})
			require.Equal(t, http.StatusCreated, rec.Code, "message schedule with a stray key is accepted at create: %s", rec.Body.String())
			id, _ := decodeEventResponse(t, rec)["id"].(string)
			rename := requestAsIdentity(t, f.srv, authUser(f.owner), http.MethodPatch, base+"/"+id, map[string]any{"name": "renamed-" + uuid.NewString()[:8]})
			cron := requestAsIdentity(t, f.srv, authUser(f.owner), http.MethodPatch, base+"/"+id, map[string]any{"cronExpr": "5 * * * *"})
			assert.Equal(t, http.StatusOK, rename.Code, "metadata-only PATCH of a message schedule: %s", rename.Body.String())
			assert.Equal(t, http.StatusOK, cron.Code, "cron PATCH of a message schedule: %s", cron.Body.String())
		})
	}
}

// TestScheduledExecP2R1_StrayExecKeyNonExecSchedulesMatrix verifies P2-R1 / FV1-O1
// across both message and dispatch_agent schedules carrying stray exec keys
// ("command" and "timeoutSeconds"), with hub.exec_schedules both off and on:
//   - rename, cron change, status pause/active, pause/resume, message<->dispatch_agent
//     switching, and delete all succeed;
//   - switching such a row TO exec remains refused (400);
//   - every response returned to a caller without live agent.attach (project-admin)
//     redacts the stored payload fail-closed and never exposes the sentinel.
func TestScheduledExecP2R1_StrayExecKeyNonExecSchedulesMatrix(t *testing.T) {
	ctx := context.Background()
	for _, execOn := range []bool{false, true} {
		flagLabel := "flag_off"
		if execOn {
			flagLabel = "flag_on"
		}
		for _, startType := range []string{"message", "dispatch_agent"} {
			t.Run(flagLabel+"/"+startType, func(t *testing.T) {
				f := execAuthoringFixture(t)
				if !execOn {
					setHubExperiments(t, f.srv, map[string]bool{conduitExperiment: true, experiments.ExecSchedules: false})
				}

				paID := tid("p2r1-pa-" + flagLabel + "-" + startType)
				createTestUserWithProjectRole(t, f.store, paID, paID+"@test", f.proj.ID, store.ProjectRoleAdmin)
				ensureHubMembership(ctx, f.store, paID)
				bindPermsToUser(t, f.store, paID, []string{"scheduled_event.delete", "hub.scheduler.read"})
				pa := NewAuthenticatedUser(paID, paID+"@test", "PA", store.UserRoleMember, "api")

				target, err := f.store.GetAgent(ctx, f.sibling.ID)
				require.NoError(t, err)
				require.NotNil(t, f.srv.authorizeAgentTargetAction(ctx, pa, target, ActionAttach),
					"precondition: project-admin must not hold agent.attach on sibling target")

				sentinel := "P2R1SENTINEL-" + uuid.NewString()
				rawPayload := `{"agentId":"` + f.sibling.ID + `","agentName":"` + f.sibling.Slug + `","message":"hello","prompt":"hello","command":["sh","-c","echo ` + sentinel + `"],"timeoutSeconds":5}`

				base := "/api/v1/projects/" + f.proj.ID + "/schedules"
				recCreate := requestAsIdentity(t, f.srv, pa, http.MethodPost, base, map[string]any{
					"name":      "stray-" + uuid.NewString()[:8],
					"cronExpr":  "0 * * * *",
					"eventType": startType,
					"payload":   rawPayload,
				})
				require.Equal(t, http.StatusCreated, recCreate.Code, recCreate.Body.String())
				assert.NotContains(t, recCreate.Body.String(), sentinel, "POST create leaked sentinel to non-attach caller")
				id, _ := decodeEventResponse(t, recCreate)["id"].(string)
				schedPath := base + "/" + id

				assertRedactedScheduleResp := func(step string, code int, body string) {
					t.Helper()
					require.Equal(t, http.StatusOK, code, "%s failed: %s", step, body)
					assert.NotContains(t, body, sentinel, "%s leaked sentinel to non-attach caller", step)
					var outer map[string]any
					require.NoError(t, json.Unmarshal([]byte(body), &outer), step)
					payloadStr, _ := outer["payload"].(string)
					var inner map[string]any
					require.NoError(t, json.Unmarshal([]byte(payloadStr), &inner), "%s inner payload: %s", step, payloadStr)
					assert.Equal(t, true, inner["commandRedacted"], "%s did not fail-closed redact payload: %s", step, payloadStr)
				}

				// 1. GET as project-admin -> 200, redacted.
				recGet := requestAsIdentity(t, f.srv, pa, http.MethodGet, schedPath, nil)
				assertRedactedScheduleResp("GET", recGet.Code, recGet.Body.String())

				// 2. Rename as project-admin -> 200, redacted.
				recRename := requestAsIdentity(t, f.srv, pa, http.MethodPatch, schedPath, map[string]any{
					"name": "renamed-" + uuid.NewString()[:8],
				})
				assertRedactedScheduleResp("PATCH rename", recRename.Code, recRename.Body.String())

				// 3. Cron change as project-admin -> 200, redacted.
				recCron := requestAsIdentity(t, f.srv, pa, http.MethodPatch, schedPath, map[string]any{
					"cronExpr": "15 * * * *",
				})
				assertRedactedScheduleResp("PATCH cronExpr", recCron.Code, recCron.Body.String())

				// 4. Switch message <-> dispatch_agent (and back) -> 200, redacted.
				otherType := "dispatch_agent"
				if startType == "dispatch_agent" {
					otherType = "message"
				}
				recSwitch := requestAsIdentity(t, f.srv, pa, http.MethodPatch, schedPath, map[string]any{
					"eventType": otherType,
				})
				assertRedactedScheduleResp("PATCH switch "+startType+"->"+otherType, recSwitch.Code, recSwitch.Body.String())

				recSwitchBack := requestAsIdentity(t, f.srv, pa, http.MethodPatch, schedPath, map[string]any{
					"eventType": startType,
				})
				assertRedactedScheduleResp("PATCH switch back "+otherType+"->"+startType, recSwitchBack.Code, recSwitchBack.Body.String())

				// 5. Switching such a row TO exec remains refused (400), both as pa and as owner.
				for _, caller := range []Identity{pa, authUser(f.owner)} {
					recToExec := requestAsIdentity(t, f.srv, caller, http.MethodPatch, schedPath, map[string]any{
						"eventType": "exec",
					})
					require.Equal(t, http.StatusBadRequest, recToExec.Code, recToExec.Body.String())
					assert.NotContains(t, recToExec.Body.String(), sentinel)
					stored, gerr := f.store.GetSchedule(ctx, id)
					require.NoError(t, gerr)
					require.Equal(t, startType, stored.EventType)
				}

				// 6. Sibling paths: PATCH status pause/active, POST pause, POST resume, admin scheduler, DELETE.
				recStatusPause := requestAsIdentity(t, f.srv, pa, http.MethodPatch, schedPath, map[string]any{"status": store.ScheduleStatusPaused})
				assertRedactedScheduleResp("PATCH status=paused", recStatusPause.Code, recStatusPause.Body.String())

				recStatusActive := requestAsIdentity(t, f.srv, pa, http.MethodPatch, schedPath, map[string]any{"status": store.ScheduleStatusActive})
				assertRedactedScheduleResp("PATCH status=active", recStatusActive.Code, recStatusActive.Body.String())

				recPause := requestAsIdentity(t, f.srv, pa, http.MethodPost, schedPath+"/pause", nil)
				assertRedactedScheduleResp("POST pause", recPause.Code, recPause.Body.String())

				recResume := requestAsIdentity(t, f.srv, pa, http.MethodPost, schedPath+"/resume", nil)
				assertRedactedScheduleResp("POST resume", recResume.Code, recResume.Body.String())

				recAdmin := requestAsIdentity(t, f.srv, pa, http.MethodGet, "/api/v1/admin/scheduler", nil)
				require.Equal(t, http.StatusOK, recAdmin.Code, recAdmin.Body.String())
				assert.NotContains(t, recAdmin.Body.String(), sentinel, "GET /api/v1/admin/scheduler leaked sentinel")

				recDel := requestAsIdentity(t, f.srv, pa, http.MethodDelete, schedPath, nil)
				require.Equal(t, http.StatusNoContent, recDel.Code, recDel.Body.String())
			})
		}
	}
}
