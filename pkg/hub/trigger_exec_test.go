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
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/agentcontrol"
	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/conduit"
	"github.com/GoogleCloudPlatform/scion/pkg/experiments"
	"github.com/GoogleCloudPlatform/scion/pkg/hub/hubfire"
	"github.com/GoogleCloudPlatform/scion/pkg/hub/trigger"
	"github.com/GoogleCloudPlatform/scion/pkg/secret"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	conduitv1 "github.com/GoogleCloudPlatform/scion/proto/conduit/v1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type triggerExecAuditCollector struct {
	*LogAuditLogger
	mu     sync.Mutex
	events []*ScheduledExecEvent
}

func (c *triggerExecAuditCollector) LogScheduledExecEvent(_ context.Context, e *ScheduledExecEvent) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	cp := *e
	if len(e.ContextAttributeKeys) > 0 {
		cp.ContextAttributeKeys = append([]string(nil), e.ContextAttributeKeys...)
	}
	c.events = append(c.events, &cp)
	return nil
}

func (c *triggerExecAuditCollector) snapshot() []*ScheduledExecEvent {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]*ScheduledExecEvent, len(c.events))
	copy(out, c.events)
	return out
}

func enableTriggerExecExperiments(t *testing.T, srv *Server) {
	t.Helper()
	setHubExperiments(t, srv, map[string]bool{
		experiments.Triggers:      true,
		conduitExperiment:         true,
		experiments.ExecSchedules: true,
	})
}

// TestTriggerExec_AuthoringRefusalsAndCaps verifies:
//   - authoring refusals: non-replay-protected source, subject mode reuse,
//     missing agent.attach, templating in argv (${...}), invalid contextAttributes,
//     managed runtime, and schedule refusal of contextAttributes;
//   - own per-trigger caps (separate from exec-schedule quotas).
func TestTriggerExec_AuthoringRefusalsAndCaps(t *testing.T) {
	f := setupTriggersTestFixture(t)
	enableTriggerExecExperiments(t, f.srv)

	// Create one scion_v1 (replay-protected) and one body_only (non-replay-protected) endpoint.
	scionEpRec := f.doWebhookEndpoint(t, f.ownerAuth, "", http.MethodPost, CreateWebhookEndpointRequest{
		Name:          "exec-scion-v1",
		SignatureMode: store.WebhookSignatureModeScionV1,
	})
	require.Equal(t, http.StatusCreated, scionEpRec.Code, scionEpRec.Body.String())
	var scionEp WebhookEndpointCreatedResponse
	require.NoError(t, json.Unmarshal(scionEpRec.Body.Bytes(), &scionEp))

	bodyOnlyEp := &store.WebhookEndpoint{
		ID:                  api.NewUUID(),
		ProjectID:           f.projectID,
		Name:                "exec-body-only",
		OwnerPrincipalKind:  store.DelegationPrincipalUser,
		OwnerPrincipalID:    f.owner.ID,
		OwnerCredentialKind: store.InitiatorCredentialKindSession,
		SignatureMode:       store.WebhookSignatureModeBodyOnly,
		SecretVersion:       1,
		Status:              store.WebhookEndpointStatusActive,
	}
	require.NoError(t, f.store.CreateWebhookEndpoint(context.Background(), bodyOnlyEp))

	// Refusal 1: non-replay-protected source -> 400 source_not_replay_protected.
	rec := f.doTrigger(t, f.ownerAuth, "", http.MethodPost, CreateTriggerRequest{
		Name:   "exec-bad-source",
		Source: TriggerSourceSpec{Kind: store.TriggerSourceKindWebhook, ID: bodyOnlyEp.ID},
		Action: TriggerActionSpec{Type: store.TriggerActionExec, AgentID: f.targetAgent.ID, Command: []string{"true"}},
	})
	require.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
	assert.Contains(t, rec.Body.String(), "source_not_replay_protected")

	// Refusal 2: subject mode reuse -> 400 at HTTP layer and exec-specific message
	// directly in authorizeTriggerExecAuthoring (pins the guard even after reuse
	// is enabled globally for other action types).
	rec = f.doTrigger(t, f.ownerAuth, "", http.MethodPost, CreateTriggerRequest{
		Name:    "exec-bad-reuse",
		Source:  TriggerSourceSpec{Kind: store.TriggerSourceKindWebhook, ID: scionEp.ID},
		Subject: &TriggerSubjectSpec{Mode: "reuse"},
		Action:  TriggerActionSpec{Type: store.TriggerActionExec, AgentID: f.targetAgent.ID, Command: []string{"true"}},
	})
	require.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())

	scionEpModel, err := f.store.GetWebhookEndpoint(context.Background(), scionEp.ID)
	require.NoError(t, err)
	directRec := httptest.NewRecorder()
	directReq := authoredRequest(t, f.ownerAuth, http.MethodPost, "/api/v1/projects/"+f.projectID+"/triggers", nil)
	_, _, directOK := f.srv.authorizeTriggerExecAuthoring(
		directRec,
		directReq,
		f.projectID,
		store.TriggerSourceKindWebhook,
		scionEpModel,
		"reuse",
		&TriggerActionSpec{Type: store.TriggerActionExec, AgentID: f.targetAgent.ID, Command: []string{"true"}},
		store.EffectCeiling{Kind: store.EffectCeilingPrincipal},
		store.AuthorityProvenance{SourcePrincipalKind: "user", SourcePrincipalID: f.owner.ID},
	)
	assert.False(t, directOK)
	require.Equal(t, http.StatusBadRequest, directRec.Code, directRec.Body.String())
	assert.Contains(t, directRec.Body.String(), "subject mode reuse is not supported for exec triggers")

	// Refusal 2b: non-exec field on TriggerActionSpec -> 400 (allow-list check).
	rec = f.doTrigger(t, f.ownerAuth, "", http.MethodPost, CreateTriggerRequest{
		Name:   "exec-bad-extra-field",
		Source: TriggerSourceSpec{Kind: store.TriggerSourceKindWebhook, ID: scionEp.ID},
		Action: TriggerActionSpec{
			Type:    store.TriggerActionExec,
			AgentID: f.targetAgent.ID,
			Command: []string{"true"},
			Body:    "disallowed",
		},
	})
	require.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
	assert.Contains(t, rec.Body.String(), "exec triggers take agentId or agentName")

	// Refusal 3: templating in argv -> 400 templating_not_allowed_in_argv.
	rec = f.doTrigger(t, f.ownerAuth, "", http.MethodPost, CreateTriggerRequest{
		Name:   "exec-bad-template-argv",
		Source: TriggerSourceSpec{Kind: store.TriggerSourceKindWebhook, ID: scionEp.ID},
		Action: TriggerActionSpec{
			Type:    store.TriggerActionExec,
			AgentID: f.targetAgent.ID,
			Command: []string{"echo", "${event.payload.msg}"},
		},
	})
	require.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
	assert.Contains(t, rec.Body.String(), "templating_not_allowed_in_argv")

	// Refusal 4: invalid contextAttributes key -> 400.
	rec = f.doTrigger(t, f.ownerAuth, "", http.MethodPost, CreateTriggerRequest{
		Name:   "exec-bad-ctx-attr",
		Source: TriggerSourceSpec{Kind: store.TriggerSourceKindWebhook, ID: scionEp.ID},
		Action: TriggerActionSpec{
			Type:              store.TriggerActionExec,
			AgentID:           f.targetAgent.ID,
			Command:           []string{"true"},
			ContextAttributes: []string{"bad key!"},
		},
	})
	require.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())

	// Refusal 5: missing agent.attach (viewer cannot author; member without attach on owner's agent gets 403).
	rec = f.doTrigger(t, f.viewerAuth, "", http.MethodPost, CreateTriggerRequest{
		Name:   "exec-viewer-denied",
		Source: TriggerSourceSpec{Kind: store.TriggerSourceKindWebhook, ID: scionEp.ID},
		Action: TriggerActionSpec{Type: store.TriggerActionExec, AgentID: f.targetAgent.ID, Command: []string{"true"}},
	})
	assert.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())

	// Refusal 6: member without agent.attach on owner's agent cannot author exec targeting it.
	rec = f.doTrigger(t, f.memberAuth, "", http.MethodPost, CreateTriggerRequest{
		Name:   "exec-member-no-attach",
		Source: TriggerSourceSpec{Kind: store.TriggerSourceKindWebhook, ID: scionEp.ID},
		Action: TriggerActionSpec{Type: store.TriggerActionExec, AgentID: f.targetAgent.ID, Command: []string{"true"}},
	})
	assert.Contains(t, []int{http.StatusForbidden, http.StatusNotFound}, rec.Code, rec.Body.String())

	// Refusal 7: schedule authoring refuses contextAttributes.
	schedReq := authoredRequest(t, f.ownerAuth, http.MethodPost, "/api/v1/projects/"+f.projectID+"/scheduled-events", CreateScheduledEventRequest{
		EventType: scheduledEventTypeExec,
		FireAt:    time.Now().UTC().Add(time.Hour).Format(time.RFC3339),
		Payload:   `{"agentId":"` + f.targetAgent.ID + `","command":["true"],"contextAttributes":["env"]}`,
	})
	schedRec := httptest.NewRecorder()
	f.srv.handleScheduledEvents(schedRec, schedReq, f.projectID, "")
	assert.Equal(t, http.StatusBadRequest, schedRec.Code, schedRec.Body.String())
	assert.Contains(t, schedRec.Body.String(), "contextAttributes")

	// Own per-trigger caps: creating triggerExecMaxActivePerTarget active exec
	// triggers succeeds (resetting authoring rate limiter bucket to test the DB cap),
	// and the next one returns 409 limit_exceeded without touching schedule quotas.
	f.srv.scheduledExecAuthoringLimiter = newKeysRateLimiter(1000, 1000)
	for i := 0; i < triggerExecMaxActivePerTarget; i++ {
		rec = f.doTrigger(t, f.ownerAuth, "", http.MethodPost, CreateTriggerRequest{
			Name:   fmt.Sprintf("exec-cap-%02d", i),
			Source: TriggerSourceSpec{Kind: store.TriggerSourceKindWebhook, ID: scionEp.ID},
			Action: TriggerActionSpec{
				Type:              store.TriggerActionExec,
				AgentID:           f.targetAgent.ID,
				Command:           []string{"true"},
				ContextAttributes: []string{"env"},
			},
		})
		require.Equal(t, http.StatusCreated, rec.Code, "trigger %d: %s", i, rec.Body.String())
	}
	overRec := f.doTrigger(t, f.ownerAuth, "", http.MethodPost, CreateTriggerRequest{
		Name:   "exec-cap-over",
		Source: TriggerSourceSpec{Kind: store.TriggerSourceKindWebhook, ID: scionEp.ID},
		Action: TriggerActionSpec{Type: store.TriggerActionExec, AgentID: f.targetAgent.ID, Command: []string{"true"}},
	})
	assert.Equal(t, http.StatusConflict, overRec.Code, overRec.Body.String())
	assert.Contains(t, overRec.Body.String(), ErrCodeLimitExceeded)
}

// TestTriggerExec_ArgvRedactionAcrossRoutes verifies fail-closed argv redaction
// on every trigger route: callers with live agent.attach see command; callers
// without live agent.attach (viewer, member without attach) see commandRedacted,
// argc, and commandSha256 only, never the sentinel argv.
func TestTriggerExec_ArgvRedactionAcrossRoutes(t *testing.T) {
	f := setupTriggersTestFixture(t)
	enableTriggerExecExperiments(t, f.srv)

	epRec := f.doWebhookEndpoint(t, f.ownerAuth, "", http.MethodPost, CreateWebhookEndpointRequest{
		Name:          "redact-hook",
		SignatureMode: store.WebhookSignatureModeScionV1,
	})
	require.Equal(t, http.StatusCreated, epRec.Code, epRec.Body.String())
	var ep WebhookEndpointCreatedResponse
	require.NoError(t, json.Unmarshal(epRec.Body.Bytes(), &ep))

	const sentinel = "TOP_SECRET_EXEC_ARGV_SENTINEL_9876"
	cmd := []string{"/bin/echo", sentinel}
	wantSHA := agentcontrol.ExecArgvDigest(cmd)

	createRec := f.doTrigger(t, f.ownerAuth, "", http.MethodPost, CreateTriggerRequest{
		Name:   "redact-exec-trig",
		Source: TriggerSourceSpec{Kind: store.TriggerSourceKindWebhook, ID: ep.ID},
		Action: TriggerActionSpec{
			Type:              store.TriggerActionExec,
			AgentID:           f.targetAgent.ID,
			Command:           cmd,
			TimeoutSeconds:    45,
			ContextAttributes: []string{"env"},
		},
	})
	require.Equal(t, http.StatusCreated, createRec.Code, createRec.Body.String())
	var created TriggerResponse
	require.NoError(t, json.Unmarshal(createRec.Body.Bytes(), &created))
	// Owner holds live agent.attach -> command is revealed.
	assert.Equal(t, cmd, created.Action.Command)
	assert.False(t, created.Action.CommandRedacted)

	// Member has trigger.read / trigger.list, NOT agent.attach on owner's agent -> redacted on GET and LIST.
	for _, tc := range []struct {
		name string
		path string
	}{
		{"GET /triggers/{id} (member)", created.ID},
		{"GET /triggers (member)", ""},
	} {
		rec := f.doTrigger(t, f.memberAuth, tc.path, http.MethodGet, nil)
		require.Equal(t, http.StatusOK, rec.Code, "%s: %s", tc.name, rec.Body.String())
		assert.NotContains(t, rec.Body.String(), sentinel, "%s must redact argv", tc.name)
		assert.Contains(t, rec.Body.String(), `"commandRedacted":true`, tc.name)
		assert.Contains(t, rec.Body.String(), wantSHA, tc.name)
		assert.Contains(t, rec.Body.String(), `"argc":2`, tc.name)
	}

	// Owner with live agent.attach sees command on GET.
	ownerGet := f.doTrigger(t, f.ownerAuth, created.ID, http.MethodGet, nil)
	require.Equal(t, http.StatusOK, ownerGet.Code, ownerGet.Body.String())
	assert.Contains(t, ownerGet.Body.String(), sentinel)
	assert.NotContains(t, ownerGet.Body.String(), `"commandRedacted":true`)

	// Fail-closed on malformed stored payload (nil revealer or corrupt JSON).
	malformed := buildTriggerResponse(nil, store.Trigger{
		ID:            "tr-bad",
		ProjectID:     f.projectID,
		Name:          "bad-exec",
		SourceKind:    store.TriggerSourceKindWebhook,
		SourceID:      ep.ID,
		MatchTypes:    []string{"webhook.received"},
		ActionType:    store.TriggerActionExec,
		ActionPayload: `{"command":["` + sentinel + `"`,
		Status:        store.TriggerStatusActive,
	})
	rawMalformed, err := json.Marshal(malformed)
	require.NoError(t, err)
	assert.NotContains(t, string(rawMalformed), sentinel)
	assert.True(t, malformed.Action.CommandRedacted)
}

// TestTriggerExec_AcceptanceFixture_WebhookStartedJobEndToEnd is the P8
// acceptance fixture: a webhook-started cron-like job on a generic scion_v1
// endpoint (no Stage 3 dependency), exercising ingress -> durablequeue ->
// trigger.Dispatcher -> execAction -> real in-process sciontool/control over
// Conduit -> tracker -> FinishScheduledExecRun -> GET /triggers/{id}/history
// with execSummary and trigger.fire.exec.* audit.
func TestTriggerExec_AcceptanceFixture_WebhookStartedJobEndToEnd(t *testing.T) {
	f := newExecRouteFixture(t)
	enableTriggerExecExperiments(t, f.srv)
	f.srv.SetHubID("test-hub-p8-e2e")
	f.srv.SetSecretBackend(secret.NewLocalBackendWithCipher(f.store, f.srv.hubID, f.srv.cipher))
	f.srv.config.HubEndpoint = "https://hub-public.example.com"

	auditor := &triggerExecAuditCollector{}
	f.srv.SetAuditLogger(auditor)
	f.startAgent(t)

	ctx := context.Background()
	projectID := f.launched.ProjectID

	doWebhookEp := func(method, subPath string, body any) *httptest.ResponseRecorder {
		req := authoredRequest(t, f.owner, method, "/api/v1/projects/"+projectID+"/webhook-endpoints"+subPath, body)
		rec := httptest.NewRecorder()
		f.srv.handleWebhookEndpoints(rec, req, projectID, strings.TrimPrefix(subPath, "/"))
		return rec
	}
	doTrig := func(method, subPath string, body any) *httptest.ResponseRecorder {
		req := authoredRequest(t, f.owner, method, "/api/v1/projects/"+projectID+"/triggers"+subPath, body)
		rec := httptest.NewRecorder()
		f.srv.handleTriggers(rec, req, projectID, strings.TrimPrefix(subPath, "/"))
		return rec
	}

	// 1. Create generic scion_v1 webhook endpoint.
	epRec := doWebhookEp(http.MethodPost, "", CreateWebhookEndpointRequest{
		Name:          "ci-sync-hook",
		SignatureMode: store.WebhookSignatureModeScionV1,
	})
	require.Equal(t, http.StatusCreated, epRec.Code, epRec.Body.String())
	var ep WebhookEndpointCreatedResponse
	require.NoError(t, json.Unmarshal(epRec.Body.Bytes(), &ep))
	require.NotEmpty(t, ep.Secret)

	// 2. Create exec trigger with literal command + allow-listed context attributes.
	// The shell prints SCION_TRIGGER_CONTEXT length and exits 0; argv is fixed.
	cmd := []string{"/bin/sh", "-c", `test -n "$SCION_TRIGGER_CONTEXT"`}
	trRec := doTrig(http.MethodPost, "", CreateTriggerRequest{
		Name:   "ci-sync-job",
		Source: TriggerSourceSpec{Kind: store.TriggerSourceKindWebhook, ID: ep.ID},
		Match:  &TriggerMatchSpec{Types: []string{"webhook.ci_done"}},
		Action: TriggerActionSpec{
			Type:              store.TriggerActionExec,
			AgentID:           f.launched.ID,
			Command:           cmd,
			TimeoutSeconds:    30,
			ContextAttributes: []string{"event", "endpoint_name", "subject"},
		},
	})
	require.Equal(t, http.StatusCreated, trRec.Code, trRec.Body.String())
	var tr TriggerResponse
	require.NoError(t, json.Unmarshal(trRec.Body.Bytes(), &tr))

	// 3. Sign and POST a hostile-payload webhook event to /api/v1/hooks/{id}.
	hostileBody := []byte(`{"cmd":"$(rm -rf /); --evil=1","tmpl":"${event.id}"}`)
	ts := time.Now().Unix()
	delivery := api.NewUUID()
	eventHeader := "ci_done"
	subject := "ci/nightly"
	sig := trigger.SignScionV1(ep.Secret, ts, delivery, eventHeader, subject, hostileBody)

	hookReq := httptest.NewRequest(http.MethodPost, "/api/v1/hooks/"+ep.ID, bytes.NewReader(hostileBody))
	hookReq.Header.Set("Content-Type", "application/json")
	hookReq.Header.Set(trigger.HeaderSignature, sig)
	hookReq.Header.Set(trigger.HeaderDelivery, delivery)
	hookReq.Header.Set(trigger.HeaderEvent, eventHeader)
	hookReq.Header.Set(trigger.HeaderSubject, subject)
	hookRec := httptest.NewRecorder()
	f.srv.Handler().ServeHTTP(hookRec, hookReq)
	require.Equal(t, http.StatusAccepted, hookRec.Code, hookRec.Body.String())

	// 4. Drain the queued event through the real trigger.Dispatcher.
	dqStore, ok := f.store.(store.DurableQueueStore)
	require.True(t, ok)
	q := f.srv.TriggerEventQueue()
	require.NotNil(t, q)
	drainer, err := f.srv.triggerDrainer(dqStore, q)
	require.NoError(t, err)
	_, err = drainer.DrainOnce(ctx)
	require.NoError(t, err)

	// Wait for the fire and its exec run to complete.
	var fire store.TriggerFire
	require.Eventually(t, func() bool {
		res, lErr := f.store.ListTriggerFires(ctx, store.TriggerFireFilter{
			ProjectID: projectID,
			TriggerID: tr.ID,
		}, store.ListOptions{Limit: 10})
		if lErr != nil || len(res.Items) == 0 {
			return false
		}
		fire = res.Items[0]
		return fire.FinishedAt != nil
	}, 5*time.Second, 25*time.Millisecond, "expected trigger exec fire to finish")

	// 5. Verify exec-start wire request: v=1 (capability-gated via exec_context.v1),
	// literal argv, bounded allow-listed trigger_context.
	f.capture.mu.Lock()
	startBodies := f.capture.bodies[execRoutePath(agentcontrol.RouteExecStart)]
	f.capture.mu.Unlock()
	require.NotEmpty(t, startBodies, "expected exec-start RPC over Conduit")
	var startReq agentcontrol.ExecStartRequest
	require.NoError(t, json.Unmarshal(startBodies[0], &startReq))
	assert.Equal(t, agentcontrol.ExecVersion, startReq.V)
	assert.Equal(t, fire.ID, startReq.RunID)
	assert.Equal(t, tr.ID, startReq.ScheduleID)
	assert.Equal(t, cmd, startReq.Argv, "argv must stay literal and never template from hostile payload")

	var gotCtx triggerExecContextPayload
	require.NoError(t, json.Unmarshal([]byte(startReq.TriggerContext), &gotCtx))
	assert.Equal(t, fire.EventID, gotCtx.EventID)
	assert.Equal(t, tr.ID, gotCtx.TriggerID)
	assert.Equal(t, fire.ID, gotCtx.FireID)
	assert.Equal(t, "webhook:"+ep.ID+"/"+subject, gotCtx.Subject)
	assert.Equal(t, "ci-sync-hook", gotCtx.Attributes["endpoint_name"])
	assert.Equal(t, eventHeader, gotCtx.Attributes["event"])
	// Non-allow-listed attributes (e.g. delivery, signature_mode) must NOT appear,
	// and "subject" maps to top-level subject rather than duplicating into attributes.
	assert.NotContains(t, gotCtx.Attributes, "subject")
	assert.NotContains(t, gotCtx.Attributes, "delivery")
	assert.NotContains(t, gotCtx.Attributes, "signature_mode")

	// 6. Verify scheduled_exec_runs row: event_id empty, fire_id owner, schedule_id empty, overlap_key set.
	run := f.getRun(t, fire.ID)
	assert.Empty(t, run.EventID)
	assert.Equal(t, fire.ID, run.FireID)
	assert.Empty(t, run.ScheduleID)
	assert.Equal(t, "trigger:"+tr.ID+":0", run.OverlapKey)
	assert.Equal(t, store.ScheduledExecOutcomeOK, run.Outcome)

	// 7. Verify GET /triggers/{id}/history carries execSummary.
	histRec := doTrig(http.MethodGet, "/"+tr.ID+"/history", nil)
	require.Equal(t, http.StatusOK, histRec.Code, histRec.Body.String())
	var hist ListTriggerFiresResponse
	require.NoError(t, json.Unmarshal(histRec.Body.Bytes(), &hist))
	require.Len(t, hist.Fires, 1)
	assert.Equal(t, store.TriggerFireStatusFired, hist.Fires[0].Status)
	assert.Equal(t, fire.ID, hist.Fires[0].ResultRef)
	require.NotNil(t, hist.Fires[0].ExecSummary)
	assert.Equal(t, store.ScheduledExecOutcomeOK, hist.Fires[0].ExecSummary.Outcome)
	require.NotNil(t, hist.Fires[0].ExecSummary.ExitCode)
	assert.Equal(t, 0, *hist.Fires[0].ExecSummary.ExitCode)

	// 8. Verify trigger.fire.exec.* audit records (admit + outcome, keys only, no secret/payload).
	audits := auditor.snapshot()
	var sawAdmit, sawOutcome bool
	for _, a := range audits {
		assert.True(t, a.IsTrigger, "audit record must be marked trigger_fire_exec")
		assert.Equal(t, tr.ID, a.TriggerID)
		assert.Equal(t, fire.ID, a.FireID)
		if a.Action == ScheduledExecAuditAdmit {
			sawAdmit = true
			assert.Equal(t, []string{"endpoint_name", "event", "subject"}, a.ContextAttributeKeys)
		}
		if a.Action == ScheduledExecAuditOutcome {
			sawOutcome = true
			assert.Equal(t, store.ScheduledExecOutcomeOK, a.Outcome)
		}
	}
	assert.True(t, sawAdmit, "expected trigger.fire.exec.admit audit")
	assert.True(t, sawOutcome, "expected trigger.fire.exec.outcome audit")
}

// TestTriggerExec_SkipHoldsClaimUntilRunEnds verifies that subject.mode=skip
// holds the trigger_subject_claims row until the exec run finishes, skipping
// concurrent events for the same subject with "subject busy", and admits a
// subsequent event once the run completes.
func TestTriggerExec_SkipHoldsClaimUntilRunEnds(t *testing.T) {
	f := newExecRouteFixture(t)
	enableTriggerExecExperiments(t, f.srv)
	f.srv.SetHubID("test-hub-p8-skip")
	f.srv.SetSecretBackend(secret.NewLocalBackendWithCipher(f.store, f.srv.hubID, f.srv.cipher))
	f.startAgent(t)

	ctx := context.Background()
	projectID := f.launched.ProjectID

	ep := &store.WebhookEndpoint{
		ID:                  api.NewUUID(),
		ProjectID:           projectID,
		Name:                "skip-hook",
		OwnerPrincipalKind:  store.DelegationPrincipalUser,
		OwnerPrincipalID:    f.owner.ID(),
		OwnerCredentialKind: store.InitiatorCredentialKindSession,
		SignatureMode:       store.WebhookSignatureModeScionV1,
		SecretVersion:       1,
		Status:              store.WebhookEndpointStatusActive,
	}
	require.NoError(t, f.store.CreateWebhookEndpoint(ctx, ep))

	payloadJSON, err := (&scheduledExecPayload{
		AgentID:        f.launched.ID,
		AgentName:      f.launched.Slug,
		Command:        []string{"/bin/sh", "-c", "sleep 0.25"},
		TimeoutSeconds: 30,
	}).encode()
	require.NoError(t, err)

	tr := &store.Trigger{
		ID:            api.NewUUID(),
		ProjectID:     projectID,
		Name:          "skip-exec-trig",
		SourceKind:    store.TriggerSourceKindWebhook,
		SourceID:      ep.ID,
		MatchTypes:    []string{"webhook.received"},
		ActionType:    store.TriggerActionExec,
		ActionPayload: payloadJSON,
		SubjectMode:   store.TriggerSubjectModeSkip,
		Status:        store.TriggerStatusActive,
		InitiatorAttribution: store.InitiatorAttribution{
			InitiatorPrincipalKind:  "user",
			InitiatorPrincipalID:    f.owner.ID(),
			InitiatorCredentialKind: "session",
			AttributionVersion:      1,
			AuthorizationRevision:   1,
		},
		AuthorityCeiling: store.EffectCeiling{
			Kind: store.EffectCeilingPrincipal,
		},
	}
	require.NoError(t, f.store.CreateTrigger(ctx, tr))

	fireOnce := func(subject string) (string, hubfire.FireOutcome) {
		now := time.Now().UTC()
		delivID := api.NewUUID()
		ev := &store.TriggerEvent{
			ID:         api.NewUUID(),
			ProjectID:  projectID,
			SourceKind: store.TriggerSourceKindWebhook,
			SourceID:   ep.ID,
			Type:       "webhook.received",
			Subject:    subject,
			DedupKey:   delivID,
			OccurredAt: now,
			ReceivedAt: now,
			Attributes: map[string]string{"delivery": delivID},
			Status:     store.TriggerEventStatusPending,
		}
		require.NoError(t, f.store.CreateTriggerEvent(ctx, ev))
		tf := &store.TriggerFire{
			ID:         api.NewUUID(),
			ProjectID:  projectID,
			TriggerID:  tr.ID,
			EventID:    ev.ID,
			Subject:    subject,
			ActionType: store.TriggerActionExec,
			Status:     store.TriggerFireStatusFiring,
			LeaseUntil: now.Add(10 * time.Minute),
			StartedAt:  now,
			CreatedAt:  now,
		}
		claimed, _, cErr := f.store.ClaimTriggerFire(ctx, tf)
		require.NoError(t, cErr)
		require.True(t, claimed)

		req := hubfire.FireRequest{
			ProjectID: projectID,
			Authority: hubfire.RecordedAuthority{
				Attribution:    tr.InitiatorAttribution,
				Ceiling:        tr.AuthorityCeiling,
				DefinitionKind: hubfire.DefinitionKindTrigger,
				DefinitionID:   tr.ID,
				FireID:         tf.ID,
			},
			ActionType: store.TriggerActionExec,
			Payload:    firstTriggerAction(*tr).Payload,
			Event: &hubfire.FireEvent{
				ID:         ev.ID,
				ProjectID:  projectID,
				SourceKind: ev.SourceKind,
				SourceID:   ev.SourceID,
				Type:       ev.Type,
				Subject:    subject,
				Attributes: ev.Attributes,
				Trigger: &hubfire.FireTriggerRef{
					ID:          tr.ID,
					Name:        tr.Name,
					SubjectMode: store.TriggerSubjectModeSkip,
					FireSubject: subject,
				},
			},
			Executor: hubfire.ExecutorContext{Kind: "trigger_drainer", ID: "trigger_event:" + ev.ID},
		}
		out := f.srv.fireAction(ctx, req)
		return tf.ID, out
	}

	// Fire 1 starts and stays running for 250ms.
	fire1ID, out1 := fireOnce("job-alpha")
	require.Equal(t, hubfire.FireStatusFired, out1.Status)
	require.True(t, out1.Detached)
	require.NotNil(t, out1.AfterRecord)

	// While run 1 is still active, fire 2 for the SAME subject is skipped ("subject busy").
	_, out2 := fireOnce("job-alpha")
	assert.Equal(t, hubfire.FireStatusSkipped, out2.Status)
	assert.Equal(t, execReasonSubjectBusy, out2.Error)

	// Start tracker for run 1 and wait for it to finish (which releases the subject claim).
	out1.AfterRecord(ctx)
	run1 := f.waitFinished(t, fire1ID)
	assert.Equal(t, store.ScheduledExecOutcomeOK, run1.Outcome)

	// Once run 1 has finished, fire 3 for the same subject is admitted and succeeds.
	fire3ID, out3 := fireOnce("job-alpha")
	require.Equal(t, hubfire.FireStatusFired, out3.Status)
	require.NotNil(t, out3.AfterRecord)
	out3.AfterRecord(ctx)
	run3 := f.waitFinished(t, fire3ID)
	assert.Equal(t, store.ScheduledExecOutcomeOK, run3.Outcome)
}

// TestTriggerExec_MissingCapabilityRefusalAndContextBounds verifies:
//   - a session whose Hello capabilities.features lacks exec_context.v1 fails
//     closed BEFORE sending exec-start (fire failed, reason
//     "sciontool lacks exec_context.v1", run outcome error, zero RPCs sent);
//   - an oversize allow-listed attribute refuses the fire before exec-start;
//   - sweeper recovery after a hub restart finishes a fire_id-owned run.
func TestTriggerExec_MissingCapabilityRefusalAndContextBounds(t *testing.T) {
	f := newExecRouteFixture(t)
	enableTriggerExecExperiments(t, f.srv)

	// Simulate an older sciontool that advertises exec-start in Rpc but lacks
	// exec_context.v1 in Hello.capabilities.features.
	f.controlFeatures = []string{conduit.FeatureStreamRenewal}
	f.startAgent(t)

	ctx := context.Background()
	projectID := f.launched.ProjectID
	ep := &store.WebhookEndpoint{
		ID:                  api.NewUUID(),
		ProjectID:           projectID,
		Name:                "old-tool-hook",
		OwnerPrincipalKind:  store.DelegationPrincipalUser,
		OwnerPrincipalID:    f.owner.ID(),
		OwnerCredentialKind: store.InitiatorCredentialKindSession,
		SignatureMode:       store.WebhookSignatureModeScionV1,
		SecretVersion:       1,
		Status:              store.WebhookEndpointStatusActive,
	}
	require.NoError(t, f.store.CreateWebhookEndpoint(ctx, ep))

	payloadJSON, err := (&scheduledExecPayload{
		AgentID:           f.launched.ID,
		AgentName:         f.launched.Slug,
		Command:           []string{"true"},
		TimeoutSeconds:    30,
		ContextAttributes: []string{"big", "subject"},
	}).encode()
	require.NoError(t, err)

	tr := &store.Trigger{
		ID:            api.NewUUID(),
		ProjectID:     projectID,
		Name:          "old-tool-trig",
		SourceKind:    store.TriggerSourceKindWebhook,
		SourceID:      ep.ID,
		MatchTypes:    []string{"webhook.received"},
		ActionType:    store.TriggerActionExec,
		ActionPayload: payloadJSON,
		SubjectMode:   store.TriggerSubjectModeNew,
		Status:        store.TriggerStatusActive,
		InitiatorAttribution: store.InitiatorAttribution{
			InitiatorPrincipalKind:  "user",
			InitiatorPrincipalID:    f.owner.ID(),
			InitiatorCredentialKind: "session",
			AttributionVersion:      1,
			AuthorizationRevision:   1,
		},
		AuthorityCeiling: store.EffectCeiling{Kind: store.EffectCeilingPrincipal},
	}
	require.NoError(t, f.store.CreateTrigger(ctx, tr))

	makeReq := func(attrs map[string]string) (string, hubfire.FireRequest) {
		now := time.Now().UTC()
		delivID := api.NewUUID()
		fullAttrs := map[string]string{"delivery": delivID}
		for k, v := range attrs {
			fullAttrs[k] = v
		}
		ev := &store.TriggerEvent{
			ID:         api.NewUUID(),
			ProjectID:  projectID,
			SourceKind: store.TriggerSourceKindWebhook,
			SourceID:   ep.ID,
			Type:       "webhook.received",
			Subject:    "s1",
			DedupKey:   delivID,
			OccurredAt: now,
			ReceivedAt: now,
			Attributes: fullAttrs,
			Status:     store.TriggerEventStatusPending,
		}
		require.NoError(t, f.store.CreateTriggerEvent(ctx, ev))
		tf := &store.TriggerFire{
			ID:         api.NewUUID(),
			ProjectID:  projectID,
			TriggerID:  tr.ID,
			EventID:    ev.ID,
			Subject:    "s1",
			ActionType: store.TriggerActionExec,
			Status:     store.TriggerFireStatusFiring,
			LeaseUntil: now.Add(10 * time.Minute),
			StartedAt:  now,
			CreatedAt:  now,
		}
		claimed, _, cErr := f.store.ClaimTriggerFire(ctx, tf)
		require.NoError(t, cErr)
		require.True(t, claimed)
		return tf.ID, hubfire.FireRequest{
			ProjectID: projectID,
			Authority: hubfire.RecordedAuthority{
				Attribution:    tr.InitiatorAttribution,
				Ceiling:        tr.AuthorityCeiling,
				DefinitionKind: hubfire.DefinitionKindTrigger,
				DefinitionID:   tr.ID,
				FireID:         tf.ID,
			},
			ActionType: store.TriggerActionExec,
			Payload:    firstTriggerAction(*tr).Payload,
			Event: &hubfire.FireEvent{
				ID:         ev.ID,
				ProjectID:  projectID,
				SourceKind: ev.SourceKind,
				SourceID:   ev.SourceID,
				Type:       ev.Type,
				Subject:    ev.Subject,
				Attributes: fullAttrs,
				Trigger:    &hubfire.FireTriggerRef{ID: tr.ID, Name: tr.Name, SubjectMode: store.TriggerSubjectModeNew},
			},
			Executor: hubfire.ExecutorContext{Kind: "trigger_drainer", ID: "trigger_event:" + ev.ID},
		}
	}

	// 1. Session lacking exec_context.v1 -> fails closed BEFORE sending exec-start.
	f1ID, req1 := makeReq(map[string]string{"big": "ok"})
	out1 := f.srv.fireAction(ctx, req1)
	assert.Equal(t, hubfire.FireStatusFailed, out1.Status)
	assert.Equal(t, execReasonContextUnsupported, out1.Error)
	assert.True(t, out1.Detached)
	assert.Equal(t, 0, f.capture.count(execRoutePath(agentcontrol.RouteExecStart)),
		"hub must refuse before sending exec-start when session lacks exec_context.v1")
	gotF1, err := f.store.GetTriggerFire(ctx, f1ID)
	require.NoError(t, err)
	assert.Equal(t, store.TriggerFireStatusFailed, gotF1.Status)
	assert.Equal(t, execReasonContextUnsupported, gotF1.Error)
	assert.Equal(t, store.ScheduledExecOutcomeError, f.getRun(t, f1ID).Outcome)

	// 2. Oversize allow-listed attribute (>4 KiB) -> refused before exec-start.
	startsBefore := f.capture.count(execRoutePath(agentcontrol.RouteExecStart))
	_, req2 := makeReq(map[string]string{"big": strings.Repeat("A", triggerExecMaxContextAttrValueBytes+1)})
	out2 := f.srv.fireAction(ctx, req2)
	assert.Equal(t, hubfire.FireStatusRefused, out2.Status)
	assert.Equal(t, execReasonContextInvalid, out2.Error)
	assert.Equal(t, startsBefore, f.capture.count(execRoutePath(agentcontrol.RouteExecStart)), "oversize context must not send exec-start")

	// 2b. NUL byte in context subject -> refused before exec-start (O4: refuse, never alter).
	_, req2b := makeReq(map[string]string{"big": "ok"})
	req2b.Event.Subject = "bad\x00subject"
	out2b := f.srv.fireAction(ctx, req2b)
	assert.Equal(t, hubfire.FireStatusRefused, out2b.Status)
	assert.Equal(t, execReasonContextInvalid, out2b.Error)
	assert.Equal(t, startsBefore, f.capture.count(execRoutePath(agentcontrol.RouteExecStart)), "NUL subject must not send exec-start")

	// 3. Sweeper recovery of a fire_id-owned run whose tracker was lost (restart mid-run).
	f.capture.answer = func(req *conduitv1.RpcRequest) *conduitv1.RpcResponse {
		if req.GetPath() == execRoutePath(agentcontrol.RouteExecStatus) {
			return &conduitv1.RpcResponse{
				Status: http.StatusOK,
				Body:   []byte(`{"state":"finished","outcome":"ok","exit_code":0,"duration_ms":42,"output_bytes":7}`),
			}
		}
		return nil
	}
	f3ID, _ := makeReq(nil)
	now := time.Now().UTC()
	require.NoError(t, f.store.CreateScheduledExecRun(ctx, &store.ScheduledExecRun{
		FireID:         f3ID,
		OverlapKey:     "trigger:" + tr.ID + ":0",
		ProjectID:      projectID,
		TargetAgentID:  f.launched.ID,
		CommandSHA256:  agentcontrol.ExecArgvDigest([]string{"true"}),
		Argc:           1,
		TimeoutSeconds: 30,
		StartedAt:      now,
		NotAfter:       now.Add(time.Minute),
		LeaseUntil:     now.Add(3 * time.Minute),
		Outcome:        store.ScheduledExecOutcomeRunning,
		Runtime:        f.launched.Runtime,
		LaunchID:       f.launched.RunID,
	}))
	f.srv.sweepScheduledExecRuns(ctx)
	recoveredRun := f.getRun(t, f3ID)
	require.NotNil(t, recoveredRun.FinishedAt)
	assert.Equal(t, store.ScheduledExecOutcomeOK, recoveredRun.Outcome)
	gotF3, err := f.store.GetTriggerFire(ctx, f3ID)
	require.NoError(t, err)
	assert.Equal(t, store.TriggerFireStatusFired, gotF3.Status)
	assert.NotNil(t, gotF3.FinishedAt)
}

// TestTriggerExec_FireTimeReplayAndDeliveryIDRefusals verifies:
//   - M1 replay rule: a non-hub_token webhook delivery without a delivery id is
//     refused at fire time with execReasonMissingDeliveryID, zero exec-start
//     RPCs, and a trigger.fire.exec.refused audit record;
//   - O1 fire-time replay re-check: when a scion_v1 endpoint is downgraded to
//     body_only after authoring, firing refuses with
//     execReasonSourceNotReplayProtected, zero exec-start RPCs, and a
//     trigger.fire.exec.refused audit record.
func TestTriggerExec_FireTimeReplayAndDeliveryIDRefusals(t *testing.T) {
	f := newExecRouteFixture(t)
	enableTriggerExecExperiments(t, f.srv)
	auditCol := &triggerExecAuditCollector{}
	f.srv.SetAuditLogger(auditCol)
	f.startAgent(t)

	ctx := context.Background()
	projectID := f.launched.ProjectID
	ep := &store.WebhookEndpoint{
		ID:                  api.NewUUID(),
		ProjectID:           projectID,
		Name:                "replay-recheck-hook",
		OwnerPrincipalKind:  store.DelegationPrincipalUser,
		OwnerPrincipalID:    f.owner.ID(),
		OwnerCredentialKind: store.InitiatorCredentialKindSession,
		SignatureMode:       store.WebhookSignatureModeScionV1,
		SecretVersion:       1,
		Status:              store.WebhookEndpointStatusActive,
	}
	require.NoError(t, f.store.CreateWebhookEndpoint(ctx, ep))
	bodyOnlyEp := &store.WebhookEndpoint{
		ID:                  api.NewUUID(),
		ProjectID:           projectID,
		Name:                "replay-recheck-body-only",
		OwnerPrincipalKind:  store.DelegationPrincipalUser,
		OwnerPrincipalID:    f.owner.ID(),
		OwnerCredentialKind: store.InitiatorCredentialKindSession,
		SignatureMode:       store.WebhookSignatureModeBodyOnly,
		SecretVersion:       1,
		Status:              store.WebhookEndpointStatusActive,
	}
	require.NoError(t, f.store.CreateWebhookEndpoint(ctx, bodyOnlyEp))

	payloadJSON, err := (&scheduledExecPayload{
		AgentID:        f.launched.ID,
		AgentName:      f.launched.Slug,
		Command:        []string{"true"},
		TimeoutSeconds: 30,
	}).encode()
	require.NoError(t, err)

	tr := &store.Trigger{
		ID:            api.NewUUID(),
		ProjectID:     projectID,
		Name:          "replay-recheck-trig",
		SourceKind:    store.TriggerSourceKindWebhook,
		SourceID:      ep.ID,
		MatchTypes:    []string{"webhook.received"},
		ActionType:    store.TriggerActionExec,
		ActionPayload: payloadJSON,
		SubjectMode:   store.TriggerSubjectModeNew,
		Status:        store.TriggerStatusActive,
		InitiatorAttribution: store.InitiatorAttribution{
			InitiatorPrincipalKind:  "user",
			InitiatorPrincipalID:    f.owner.ID(),
			InitiatorCredentialKind: "session",
			AttributionVersion:      1,
			AuthorizationRevision:   1,
		},
		AuthorityCeiling: store.EffectCeiling{Kind: store.EffectCeilingPrincipal},
	}
	require.NoError(t, f.store.CreateTrigger(ctx, tr))

	makeFire := func(sourceID string, attrs map[string]string) hubfire.FireRequest {
		now := time.Now().UTC()
		ev := &store.TriggerEvent{
			ID:         api.NewUUID(),
			ProjectID:  projectID,
			SourceKind: store.TriggerSourceKindWebhook,
			SourceID:   sourceID,
			Type:       "webhook.received",
			Subject:    "s1",
			DedupKey:   api.NewUUID(),
			OccurredAt: now,
			ReceivedAt: now,
			Attributes: attrs,
			Status:     store.TriggerEventStatusPending,
		}
		require.NoError(t, f.store.CreateTriggerEvent(ctx, ev))
		tf := &store.TriggerFire{
			ID:         api.NewUUID(),
			ProjectID:  projectID,
			TriggerID:  tr.ID,
			EventID:    ev.ID,
			Subject:    "s1",
			ActionType: store.TriggerActionExec,
			Status:     store.TriggerFireStatusFiring,
			LeaseUntil: now.Add(10 * time.Minute),
			StartedAt:  now,
			CreatedAt:  now,
		}
		claimed, _, cErr := f.store.ClaimTriggerFire(ctx, tf)
		require.NoError(t, cErr)
		require.True(t, claimed)
		return hubfire.FireRequest{
			ProjectID: projectID,
			Authority: hubfire.RecordedAuthority{
				Attribution:    tr.InitiatorAttribution,
				Ceiling:        tr.AuthorityCeiling,
				DefinitionKind: hubfire.DefinitionKindTrigger,
				DefinitionID:   tr.ID,
				FireID:         tf.ID,
			},
			ActionType: store.TriggerActionExec,
			Payload:    firstTriggerAction(*tr).Payload,
			Event: &hubfire.FireEvent{
				ID:         ev.ID,
				ProjectID:  projectID,
				SourceKind: ev.SourceKind,
				SourceID:   ev.SourceID,
				Type:       ev.Type,
				Subject:    ev.Subject,
				Attributes: attrs,
				Trigger:    &hubfire.FireTriggerRef{ID: tr.ID, Name: tr.Name, SubjectMode: store.TriggerSubjectModeNew},
			},
			Executor: hubfire.ExecutorContext{Kind: "trigger_drainer", ID: "trigger_event:" + ev.ID},
		}
	}

	// 1. M1: scion_v1 webhook delivery without delivery id -> refused before exec-start.
	outNoDeliv := f.srv.fireAction(ctx, makeFire(ep.ID, map[string]string{"event": "push"}))
	assert.Equal(t, hubfire.FireStatusRefused, outNoDeliv.Status)
	assert.Equal(t, execReasonMissingDeliveryID, outNoDeliv.Error)
	assert.Equal(t, 0, f.capture.count(execRoutePath(agentcontrol.RouteExecStart)),
		"missing delivery id must not send exec-start")

	// 2. O1: fire arriving from a non-replay-protected body_only endpoint -> fire-time
	// Preauthorize re-check refuses even when delivery id is present (kills mutant m5).
	outDowngraded := f.srv.fireAction(ctx, makeFire(bodyOnlyEp.ID, map[string]string{"delivery": api.NewUUID()}))
	assert.Equal(t, hubfire.FireStatusRefused, outDowngraded.Status)
	assert.Equal(t, execReasonSourceNotReplayProtected, outDowngraded.Error)
	assert.Equal(t, 0, f.capture.count(execRoutePath(agentcontrol.RouteExecStart)),
		"body_only endpoint must not send exec-start")

	// Verify both refusals emitted trigger.fire.exec.refused audit records.
	auditCol.mu.Lock()
	var refusedReasons []string
	for _, ev := range auditCol.events {
		if ev.Action == ScheduledExecAuditRefused && ev.TriggerID == tr.ID {
			refusedReasons = append(refusedReasons, ev.Reason)
		}
	}
	auditCol.mu.Unlock()
	assert.Contains(t, refusedReasons, execReasonMissingDeliveryID)
	assert.Contains(t, refusedReasons, execReasonSourceNotReplayProtected)
}

// TestTriggerExec_SubjectOptIn_ClaimReleaseOnOverlap_AndFireRateCap verifies:
//   - O-b: SCION_TRIGGER_CONTEXT omits top-level subject unless "subject" is in
//     the rule's contextAttributes allow-list, and includes it when allow-listed;
//   - O-c (kills mutant m11): when a skip-mode trigger acquires a subject claim
//     and then hits an overlap (previous run in progress) at run admission, the
//     unbound claim is released immediately;
//   - O-d (kills mutant m12): when a trigger exceeds triggerExecMaxFiresPerMinute
//     recent non-skipped fires, the next fire is skipped with execReasonRateLimit
//     and zero exec-start RPCs are sent.
func TestTriggerExec_SubjectOptIn_ClaimReleaseOnOverlap_AndFireRateCap(t *testing.T) {
	// 1. O-b: buildTriggerExecContext omits subject without opt-in and includes it with opt-in.
	baseReq := hubfire.FireRequest{
		ProjectID: "proj-ob",
		Authority: hubfire.RecordedAuthority{
			DefinitionKind: hubfire.DefinitionKindTrigger,
			DefinitionID:   "trg-ob",
			FireID:         "fire-ob",
		},
		Event: &hubfire.FireEvent{
			ID:         "evt-ob",
			Subject:    "webhook:ep/ci/nightly",
			Attributes: map[string]string{"event": "ci_done", "subject": "ci/nightly"},
		},
	}
	rawNoSubject, err := buildTriggerExecContext(baseReq, []string{"event"})
	require.NoError(t, err)
	var ctxNoSubject triggerExecContextPayload
	require.NoError(t, json.Unmarshal([]byte(rawNoSubject), &ctxNoSubject))
	assert.Empty(t, ctxNoSubject.Subject, "subject must be omitted unless allow-listed")
	assert.Equal(t, "ci_done", ctxNoSubject.Attributes["event"])

	rawWithSubject, err := buildTriggerExecContext(baseReq, []string{"event", "subject"})
	require.NoError(t, err)
	var ctxWithSubject triggerExecContextPayload
	require.NoError(t, json.Unmarshal([]byte(rawWithSubject), &ctxWithSubject))
	assert.Equal(t, "webhook:ep/ci/nightly", ctxWithSubject.Subject)
	assert.NotContains(t, ctxWithSubject.Attributes, "subject")

	// 2. O-c: skip-mode claim acquired, then overlap at run admission -> claim released.
	f := newExecRouteFixture(t)
	enableTriggerExecExperiments(t, f.srv)
	f.startAgent(t)

	ctx := context.Background()
	projectID := f.launched.ProjectID
	ep := &store.WebhookEndpoint{
		ID:                  api.NewUUID(),
		ProjectID:           projectID,
		Name:                "oc-od-hook",
		OwnerPrincipalKind:  store.DelegationPrincipalUser,
		OwnerPrincipalID:    f.owner.ID(),
		OwnerCredentialKind: store.InitiatorCredentialKindSession,
		SignatureMode:       store.WebhookSignatureModeScionV1,
		SecretVersion:       1,
		Status:              store.WebhookEndpointStatusActive,
	}
	require.NoError(t, f.store.CreateWebhookEndpoint(ctx, ep))

	payloadJSON, err := (&scheduledExecPayload{
		AgentID:        f.launched.ID,
		AgentName:      f.launched.Slug,
		Command:        []string{"true"},
		TimeoutSeconds: 30,
	}).encode()
	require.NoError(t, err)

	tr := &store.Trigger{
		ID:            api.NewUUID(),
		ProjectID:     projectID,
		Name:          "oc-od-trig",
		SourceKind:    store.TriggerSourceKindWebhook,
		SourceID:      ep.ID,
		MatchTypes:    []string{"webhook.received"},
		ActionType:    store.TriggerActionExec,
		ActionPayload: payloadJSON,
		SubjectMode:   store.TriggerSubjectModeSkip,
		Status:        store.TriggerStatusActive,
		InitiatorAttribution: store.InitiatorAttribution{
			InitiatorPrincipalKind:  "user",
			InitiatorPrincipalID:    f.owner.ID(),
			InitiatorCredentialKind: "session",
			AttributionVersion:      1,
			AuthorizationRevision:   1,
		},
		AuthorityCeiling: store.EffectCeiling{Kind: store.EffectCeilingPrincipal},
	}
	require.NoError(t, f.store.CreateTrigger(ctx, tr))

	makeFire := func(subject string) (string, hubfire.FireRequest) {
		now := time.Now().UTC()
		delivID := api.NewUUID()
		ev := &store.TriggerEvent{
			ID:         api.NewUUID(),
			ProjectID:  projectID,
			SourceKind: store.TriggerSourceKindWebhook,
			SourceID:   ep.ID,
			Type:       "webhook.received",
			Subject:    subject,
			DedupKey:   delivID,
			OccurredAt: now,
			ReceivedAt: now,
			Attributes: map[string]string{"delivery": delivID},
			Status:     store.TriggerEventStatusPending,
		}
		require.NoError(t, f.store.CreateTriggerEvent(ctx, ev))
		tf := &store.TriggerFire{
			ID:         api.NewUUID(),
			ProjectID:  projectID,
			TriggerID:  tr.ID,
			EventID:    ev.ID,
			Subject:    subject,
			ActionType: store.TriggerActionExec,
			Status:     store.TriggerFireStatusFiring,
			LeaseUntil: now.Add(10 * time.Minute),
			StartedAt:  now,
			CreatedAt:  now,
		}
		claimed, _, cErr := f.store.ClaimTriggerFire(ctx, tf)
		require.NoError(t, cErr)
		require.True(t, claimed)
		return tf.ID, hubfire.FireRequest{
			ProjectID: projectID,
			Authority: hubfire.RecordedAuthority{
				Attribution:    tr.InitiatorAttribution,
				Ceiling:        tr.AuthorityCeiling,
				DefinitionKind: hubfire.DefinitionKindTrigger,
				DefinitionID:   tr.ID,
				FireID:         tf.ID,
			},
			ActionType: store.TriggerActionExec,
			Payload:    firstTriggerAction(*tr).Payload,
			Event: &hubfire.FireEvent{
				ID:         ev.ID,
				ProjectID:  projectID,
				SourceKind: ev.SourceKind,
				SourceID:   ev.SourceID,
				Type:       ev.Type,
				Subject:    subject,
				Attributes: ev.Attributes,
				Trigger: &hubfire.FireTriggerRef{
					ID:          tr.ID,
					Name:        tr.Name,
					SubjectMode: store.TriggerSubjectModeSkip,
					FireSubject: subject,
				},
			},
			Executor: hubfire.ExecutorContext{Kind: "trigger_drainer", ID: "trigger_event:" + ev.ID},
		}
	}

	// Seed an active unfinished run holding overlap_key = "trigger:<tr.ID>" for another subject.
	blockerFireID, _ := makeFire("job-blocker")
	now := time.Now().UTC()
	require.NoError(t, f.store.CreateScheduledExecRun(ctx, &store.ScheduledExecRun{
		FireID:         blockerFireID,
		OverlapKey:     "trigger:" + tr.ID + ":0",
		ProjectID:      projectID,
		TargetAgentID:  f.launched.ID,
		CommandSHA256:  agentcontrol.ExecArgvDigest([]string{"true"}),
		Argc:           1,
		TimeoutSeconds: 30,
		StartedAt:      now,
		NotAfter:       now.Add(time.Minute),
		LeaseUntil:     now.Add(3 * time.Minute),
		Outcome:        store.ScheduledExecOutcomeRunning,
		Runtime:        f.launched.Runtime,
		LaunchID:       f.launched.RunID,
	}))

	// Fire for a new subject ("job-overlap"): claim succeeds, then DB overlap refuses.
	_, overlapReq := makeFire("job-overlap")
	overlapOut := f.srv.fireAction(ctx, overlapReq)
	assert.Equal(t, hubfire.FireStatusSkipped, overlapOut.Status)
	assert.Equal(t, execReasonOverlap, overlapOut.Error)
	// The unbound claim MUST have been released (kills mutant m11).
	_, claimErr := f.store.GetActiveTriggerSubjectClaim(ctx, tr.ID, 0, "job-overlap")
	assert.ErrorIs(t, claimErr, store.ErrNotFound, "unbound skip claim must be released on pre-admission overlap failure")

	// 3. O-d: per-trigger fire-rate cap (> triggerExecMaxFiresPerMinute recent non-skipped fires).
	// Create a fresh trigger without an active blocker run, seed triggerExecMaxFiresPerMinute
	// recent non-skipped fires, and verify the next fire is skipped with execReasonRateLimit.
	trCap := *tr
	trCap.ID = api.NewUUID()
	trCap.Name = "od-rate-cap-trig"
	trCap.SubjectMode = store.TriggerSubjectModeNew
	require.NoError(t, f.store.CreateTrigger(ctx, &trCap))

	for i := 0; i < triggerExecMaxFiresPerMinute; i++ {
		ts := time.Now().UTC()
		ev := &store.TriggerEvent{
			ID:         api.NewUUID(),
			ProjectID:  projectID,
			SourceKind: store.TriggerSourceKindWebhook,
			SourceID:   ep.ID,
			Type:       "webhook.received",
			Subject:    fmt.Sprintf("cap-%d", i),
			DedupKey:   api.NewUUID(),
			OccurredAt: ts,
			ReceivedAt: ts,
			Status:     store.TriggerEventStatusProcessed,
		}
		require.NoError(t, f.store.CreateTriggerEvent(ctx, ev))
		tf := &store.TriggerFire{
			ID:         api.NewUUID(),
			ProjectID:  projectID,
			TriggerID:  trCap.ID,
			EventID:    ev.ID,
			Subject:    ev.Subject,
			ActionType: store.TriggerActionExec,
			Status:     store.TriggerFireStatusFired,
			LeaseUntil: ts.Add(time.Minute),
			StartedAt:  ts,
			CreatedAt:  ts,
		}
		claimed, _, cErr := f.store.ClaimTriggerFire(ctx, tf)
		require.NoError(t, cErr)
		require.True(t, claimed)
	}

	startsBefore := f.capture.count(execRoutePath(agentcontrol.RouteExecStart))
	ts := time.Now().UTC()
	delivID := api.NewUUID()
	evOver := &store.TriggerEvent{
		ID:         api.NewUUID(),
		ProjectID:  projectID,
		SourceKind: store.TriggerSourceKindWebhook,
		SourceID:   ep.ID,
		Type:       "webhook.received",
		Subject:    "cap-over",
		DedupKey:   delivID,
		OccurredAt: ts,
		ReceivedAt: ts,
		Attributes: map[string]string{"delivery": delivID},
		Status:     store.TriggerEventStatusPending,
	}
	require.NoError(t, f.store.CreateTriggerEvent(ctx, evOver))
	tfOver := &store.TriggerFire{
		ID:         api.NewUUID(),
		ProjectID:  projectID,
		TriggerID:  trCap.ID,
		EventID:    evOver.ID,
		Subject:    evOver.Subject,
		ActionType: store.TriggerActionExec,
		Status:     store.TriggerFireStatusFiring,
		LeaseUntil: ts.Add(10 * time.Minute),
		StartedAt:  ts,
		CreatedAt:  ts,
	}
	claimed, _, cErr := f.store.ClaimTriggerFire(ctx, tfOver)
	require.NoError(t, cErr)
	require.True(t, claimed)

	overOut := f.srv.fireAction(ctx, hubfire.FireRequest{
		ProjectID: projectID,
		Authority: hubfire.RecordedAuthority{
			Attribution:    trCap.InitiatorAttribution,
			Ceiling:        trCap.AuthorityCeiling,
			DefinitionKind: hubfire.DefinitionKindTrigger,
			DefinitionID:   trCap.ID,
			FireID:         tfOver.ID,
		},
		ActionType: store.TriggerActionExec,
		Payload:    firstTriggerAction(trCap).Payload,
		Event: &hubfire.FireEvent{
			ID:         evOver.ID,
			ProjectID:  projectID,
			SourceKind: evOver.SourceKind,
			SourceID:   evOver.SourceID,
			Type:       evOver.Type,
			Subject:    evOver.Subject,
			Attributes: evOver.Attributes,
			Trigger:    &hubfire.FireTriggerRef{ID: trCap.ID, Name: trCap.Name, SubjectMode: store.TriggerSubjectModeNew},
		},
		Executor: hubfire.ExecutorContext{Kind: "trigger_drainer", ID: "trigger_event:" + evOver.ID},
	})
	assert.Equal(t, hubfire.FireStatusSkipped, overOut.Status)
	assert.Equal(t, execReasonRateLimit, overOut.Error)
	assert.Equal(t, startsBefore, f.capture.count(execRoutePath(agentcontrol.RouteExecStart)),
		"rate-limited fire must not send exec-start")
}
