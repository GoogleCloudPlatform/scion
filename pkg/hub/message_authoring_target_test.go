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
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/hub/hubfire"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestMessageAuthoringTargetRule_TriggerParity pins Stage 1 §16 criterion 17
// and rev 8.32 option (a) for the trigger caller:
//   - an unresolved target is refused with 404;
//   - an unreadable cross-project target answers byte-identically to a
//     nonexistent target (whether cross-project messaging is disabled or
//     enabled);
//   - a conflicting agentId + agentName returns the same 404;
//   - a readable cross-project target returns 403 when cross-project messaging
//     is disabled and succeeds when enabled.
func TestMessageAuthoringTargetRule_TriggerParity(t *testing.T) {
	f := setupTriggersTestFixture(t)
	ctx := context.Background()

	// Create an active webhook endpoint in f.projectID owned by f.owner.
	epRec := doRequestAsUser(t, f.srv, f.owner, http.MethodPost,
		"/api/v1/projects/"+f.projectID+"/webhook-endpoints",
		CreateWebhookEndpointRequest{Name: "parity-hook"})
	require.Equal(t, http.StatusCreated, epRec.Code, epRec.Body.String())
	var epResp struct {
		ID string `json:"id"`
	}
	require.NoError(t, json.Unmarshal(epRec.Body.Bytes(), &epResp))

	// Create an agent in f.otherProjID that f.member (who has no role in
	// f.otherProjID) cannot read and may not message.
	unreadableTarget := &store.Agent{
		ID:        api.NewUUID(),
		Slug:      "secret-cross-agent",
		Name:      "secret-cross-agent",
		ProjectID: f.otherProjID,
		Phase:     "running",
	}
	require.NoError(t, f.store.CreateAgent(ctx, unreadableTarget))

	nonexistentID := api.NewUUID()

	createTrigger := func(caller *store.User, name string, action TriggerActionSpec) (int, string) {
		t.Helper()
		rec := doRequestAsUser(t, f.srv, caller, http.MethodPost,
			"/api/v1/projects/"+f.projectID+"/triggers",
			CreateTriggerRequest{
				Name:   name,
				Source: TriggerSourceSpec{Kind: store.TriggerSourceKindWebhook, ID: epResp.ID},
				Match:  &TriggerMatchSpec{Types: []string{"webhook.received"}},
				Action: action,
			})
		return rec.Code, rec.Body.String()
	}

	for _, crossProjectEnabled := range []bool{false, true} {
		label := "cross_project_disabled"
		if crossProjectEnabled {
			label = "cross_project_enabled"
			enableCPM(t, f.srv, f.store)
		} else {
			f.srv.SetOperationalSettings(nil)
		}
		t.Run(label, func(t *testing.T) {
			codeMissing, bodyMissing := createTrigger(f.member, "tr-missing-"+label, TriggerActionSpec{
				Type:    store.TriggerActionMessage,
				AgentID: nonexistentID,
				Message: "ping",
			})
			codeUnreadable, bodyUnreadable := createTrigger(f.member, "tr-unreadable-"+label, TriggerActionSpec{
				Type:    store.TriggerActionMessage,
				AgentID: unreadableTarget.ID,
				Message: "ping",
			})
			assert.Equal(t, http.StatusNotFound, codeMissing)
			assert.Equal(t, http.StatusNotFound, codeUnreadable)
			assert.Equal(t, bodyMissing, bodyUnreadable,
				"unreadable cross-project target must answer byte-identically to nonexistent target")

			// Direct messageAction.Validate parity (HTTP-free layer).
			act := &messageAction{srv: f.srv}
			_, errMissing := act.Validate(ctx, f.memberAuth, f.projectID,
				`{"agentId":"`+nonexistentID+`","message":"ping"}`)
			_, errUnreadable := act.Validate(ctx, f.memberAuth, f.projectID,
				`{"agentId":"`+unreadableTarget.ID+`","message":"ping"}`)
			var aeMissing, aeUnreadable *messageAuthoringError
			require.ErrorAs(t, errMissing, &aeMissing)
			require.ErrorAs(t, errUnreadable, &aeUnreadable)
			assert.Equal(t, aeMissing.httpStatus, aeUnreadable.httpStatus)
			assert.Equal(t, aeMissing.notFoundResource, aeUnreadable.notFoundResource)
			assert.Equal(t, http.StatusNotFound, aeUnreadable.httpStatus)
			assert.Equal(t, "Agent", aeUnreadable.notFoundResource)
		})
	}

	t.Run("conflicting agentId and agentName returns 404 identical to nonexistent", func(t *testing.T) {
		codeMissing, bodyMissing := createTrigger(f.owner, "tr-missing-conflict", TriggerActionSpec{
			Type:    store.TriggerActionMessage,
			AgentID: nonexistentID,
			Message: "ping",
		})
		codeConflict, bodyConflict := createTrigger(f.owner, "tr-conflict", TriggerActionSpec{
			Type:      store.TriggerActionMessage,
			AgentID:   f.targetAgent.ID,
			AgentName: "wrong-slug",
			Message:   "ping",
		})
		assert.Equal(t, http.StatusNotFound, codeMissing)
		assert.Equal(t, http.StatusNotFound, codeConflict)
		assert.Equal(t, bodyMissing, bodyConflict)
	})

	t.Run("readable cross-project target obeys cross-project setting", func(t *testing.T) {
		// f.owner owns f.otherProjID so unreadableTarget is readable to f.owner.
		f.srv.SetOperationalSettings(nil)
		codeDisabled, bodyDisabled := createTrigger(f.owner, "tr-xproj-off", TriggerActionSpec{
			Type:    store.TriggerActionMessage,
			AgentID: unreadableTarget.ID,
			Message: "ping",
		})
		assert.Equal(t, http.StatusForbidden, codeDisabled, bodyDisabled)
		assert.Contains(t, bodyDisabled, "cross-project trigger messages require cross-project messaging to be enabled")

		enableCPM(t, f.srv, f.store)
		codeEnabled, bodyEnabled := createTrigger(f.owner, "tr-xproj-on", TriggerActionSpec{
			Type:    store.TriggerActionMessage,
			AgentID: unreadableTarget.ID,
			Message: "ping",
		})
		assert.Equal(t, http.StatusCreated, codeEnabled, bodyEnabled)
	})
}

// TestMessageAuthoringTargetRule_ScheduleParity pins Stage 1 §16 criterion 17
// and rev 8.32 option (a) for the schedule caller:
//   - an unresolved target is accepted at authoring (201) and refused at fire;
//   - an unreadable cross-project new target is accepted at authoring (201,
//     identical to nonexistent) and refused at fire;
//   - conflicting payload vs convenience targets return 400;
//   - an existing target on resume/update refuses unauthorized targets with 403.
func TestMessageAuthoringTargetRule_ScheduleParity(t *testing.T) {
	f := setupTriggersTestFixture(t)
	ctx := context.Background()

	// Create an agent in f.otherProjID that f.member cannot read and may not message.
	unreadableTarget := &store.Agent{
		ID:        api.NewUUID(),
		Slug:      "sched-secret-cross-agent",
		Name:      "sched-secret-cross-agent",
		ProjectID: f.otherProjID,
		Phase:     "running",
	}
	require.NoError(t, f.store.CreateAgent(ctx, unreadableTarget))
	nonexistentID := api.NewUUID()

	f.srv.SetOperationalSettings(nil)

	createSched := func(name, targetID string) (int, string, store.Schedule) {
		t.Helper()
		rec := doRequestAsUser(t, f.srv, f.member, http.MethodPost,
			"/api/v1/projects/"+f.projectID+"/schedules",
			CreateScheduleRequest{
				Name:      name,
				CronExpr:  "0 * * * *",
				EventType: "message",
				Payload:   `{"agentId":"` + targetID + `","message":"hello"}`,
			})
		var sc store.Schedule
		if rec.Code == http.StatusCreated {
			require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &sc))
		}
		return rec.Code, rec.Body.String(), sc
	}

	codeMissing, bodyMissing, schedMissing := createSched("sched-missing", nonexistentID)
	codeUnreadable, bodyUnreadable, schedUnreadable := createSched("sched-unreadable", unreadableTarget.ID)
	require.Equal(t, http.StatusCreated, codeMissing, bodyMissing)
	require.Equal(t, http.StatusCreated, codeUnreadable, bodyUnreadable)

	// Reload from store to get the recorded InitiatorAttribution & AuthorityCeiling.
	storedMissing, err := f.store.GetSchedule(ctx, schedMissing.ID)
	require.NoError(t, err)
	storedUnreadable, err := f.store.GetSchedule(ctx, schedUnreadable.ID)
	require.NoError(t, err)

	// Both must refuse at fire time with the same generic refusal.
	for name, sc := range map[string]*store.Schedule{
		"nonexistent": storedMissing,
		"unreadable":  storedUnreadable,
	} {
		out := f.srv.fireAction(ctx, hubfire.FireRequest{
			ProjectID: f.projectID,
			Authority: hubfire.RecordedAuthority{
				Attribution:    sc.InitiatorAttribution,
				Ceiling:        sc.AuthorityCeiling,
				DefinitionKind: hubfire.DefinitionKindSchedule,
				DefinitionID:   sc.ID,
				FireID:         api.NewUUID(),
			},
			ActionType: "message",
			Payload:    sc.Payload,
			FireAt:     time.Now().UTC(),
		})
		assert.Equal(t, hubfire.FireStatusRefused, out.Status, "schedule %s must refuse at fire time", name)
		assert.Equal(t, errScheduledMessageRefused.Error(), out.Error, "schedule %s refusal text", name)
	}

	// Conflicting payload vs convenience fields returns 400.
	recConflict := doRequestAsUser(t, f.srv, f.member, http.MethodPost,
		"/api/v1/projects/"+f.projectID+"/schedules",
		CreateScheduleRequest{
			Name:      "sched-conflict",
			CronExpr:  "0 * * * *",
			EventType: "message",
			AgentName: f.targetAgent.Slug,
			Payload:   `{"agentName":"other-slug","message":"hello"}`,
		})
	assert.Equal(t, http.StatusBadRequest, recConflict.Code, recConflict.Body.String())
	assert.Contains(t, recConflict.Body.String(), "conflicting target")

	// Existing target (re-authoring on resume): cross-project target when
	// cross-project messaging is disabled refuses with 403 rather than masking.
	pauseRec := doRequestAsUser(t, f.srv, f.owner, http.MethodPost,
		"/api/v1/projects/"+f.projectID+"/schedules/"+schedUnreadable.ID+"/pause", nil)
	require.Equal(t, http.StatusOK, pauseRec.Code, pauseRec.Body.String())

	resumeRec := doRequestAsUser(t, f.srv, f.owner, http.MethodPost,
		"/api/v1/projects/"+f.projectID+"/schedules/"+schedUnreadable.ID+"/resume", nil)
	assert.Equal(t, http.StatusForbidden, resumeRec.Code, resumeRec.Body.String())
	assert.Contains(t, resumeRec.Body.String(), "cross-project scheduled messages require cross-project messaging to be enabled")
}

// TestMessageAuthoringTargetRule_OutOfRangeCallerKindFailsClosed pins S2C-I1:
// any out-of-range messageAuthoringCaller value (zero, negative, or non-zero
// unknown) must fail closed to strict trigger mode and never fall into
// schedule-style acceptance for unresolved or unreadable targets.
func TestMessageAuthoringTargetRule_OutOfRangeCallerKindFailsClosed(t *testing.T) {
	f := setupTriggersTestFixture(t)
	ctx := context.Background()

	unreadableTarget := &store.Agent{
		ID:        api.NewUUID(),
		Slug:      "s2c-i1-secret-agent",
		Name:      "s2c-i1-secret-agent",
		ProjectID: f.otherProjID,
		Phase:     "running",
	}
	require.NoError(t, f.store.CreateAgent(ctx, unreadableTarget))

	nonexistentID := api.NewUUID()
	outOfRangeKinds := []messageAuthoringCaller{
		messageAuthoringCaller(0),
		messageAuthoringCaller(-1),
		messageAuthoringCaller(99),
	}

	for _, kind := range outOfRangeKinds {
		for _, tc := range []struct {
			name    string
			payload string
		}{
			{name: "unresolved", payload: `{"agentId":"` + nonexistentID + `","message":"ping"}`},
			{name: "unreadable_cross_project", payload: `{"agentId":"` + unreadableTarget.ID + `","message":"ping"}`},
		} {
			_, err := f.srv.validateMessageAuthoringTarget(ctx, f.memberAuth, f.projectID, kind, tc.payload, "", "")
			var authErr *messageAuthoringError
			require.ErrorAs(t, err, &authErr, "callerKind=%d case=%s must fail closed", kind, tc.name)
			assert.Equal(t, http.StatusNotFound, authErr.httpStatus, "callerKind=%d case=%s", kind, tc.name)
			assert.Equal(t, "Agent", authErr.notFoundResource, "callerKind=%d case=%s", kind, tc.name)
		}
	}
}
