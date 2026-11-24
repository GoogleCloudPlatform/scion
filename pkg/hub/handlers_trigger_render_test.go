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
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/triggertemplate"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestTriggerRender(t *testing.T) {
	f := setupTriggersTestFixture(t)
	ctx := context.Background()

	// Create an outsider user who has no membership in f.projectID.
	outsider := &store.User{
		ID:          api.NewUUID(),
		Email:       "render-outsider@example.com",
		DisplayName: "Outsider",
		Role:        store.UserRoleMember,
		Status:      "active",
	}
	require.NoError(t, f.store.CreateUser(ctx, outsider))
	outsiderAuth := NewAuthenticatedUser(outsider.ID, outsider.Email, outsider.DisplayName, outsider.Role, "user")

	// Owner creates webhook endpoint + dispatch_agent trigger.
	epRec := f.doWebhookEndpoint(t, f.ownerAuth, "", http.MethodPost, CreateWebhookEndpointRequest{
		Name: "render-hook",
	})
	require.Equal(t, http.StatusCreated, epRec.Code, epRec.Body.String())
	var ep WebhookEndpointCreatedResponse
	require.NoError(t, json.Unmarshal(epRec.Body.Bytes(), &ep))

	// AgentA creates its own endpoint + message trigger so we can test agent-scoped visibility.
	agentEpRec := f.doWebhookEndpoint(t, f.agentAAuth, "", http.MethodPost, CreateWebhookEndpointRequest{
		Name: "agent-a-render-hook",
	})
	require.Equal(t, http.StatusCreated, agentEpRec.Code, agentEpRec.Body.String())
	var agentEp WebhookEndpointCreatedResponse
	require.NoError(t, json.Unmarshal(agentEpRec.Body.Bytes(), &agentEp))

	dispatchRec := f.doTrigger(t, f.ownerAuth, "", http.MethodPost, CreateTriggerRequest{
		Name:   "render-dispatch",
		Source: TriggerSourceSpec{Kind: store.TriggerSourceKindWebhook, ID: ep.ID},
		Action: TriggerActionSpec{
			Type:         store.TriggerActionDispatchAgent,
			Template:     "default",
			Task:         "Investigate ${event.payload.title} for ${trigger.name} in ${project.name}",
			AgentName:    "pr-${event.attributes.number}",
			Branch:       "refs/heads/fix-${event.attributes.number}",
			Labels:       map[string]string{"pr": "${event.attributes.number}", "env": "${event.payload.env}"},
			ReuseMessage: "Follow-up on ${event.id}",
			UntrustedVars: []string{
				"event.payload.title",
				"event.attributes.number",
				"event.payload.env",
			},
		},
	})
	require.Equal(t, http.StatusCreated, dispatchRec.Code, dispatchRec.Body.String())
	var dispatchTrig TriggerResponse
	require.NoError(t, json.Unmarshal(dispatchRec.Body.Bytes(), &dispatchTrig))

	msgRec := f.doTrigger(t, f.agentAAuth, "", http.MethodPost, CreateTriggerRequest{
		Name:   "render-msg-agent-a",
		Source: TriggerSourceSpec{Kind: store.TriggerSourceKindWebhook, ID: agentEp.ID},
		Action: TriggerActionSpec{
			Type:    store.TriggerActionMessage,
			AgentID: f.agentA.ID,
			Message: "Event ${event.id} on ${trigger.name}",
		},
	})
	require.Equal(t, http.StatusCreated, msgRec.Code, msgRec.Body.String())
	var msgTrig TriggerResponse
	require.NoError(t, json.Unmarshal(msgRec.Body.Bytes(), &msgTrig))

	// Stored event on owner's endpoint with a unique sentinel value to verify no log leakage.
	const secretSentinel = "RENDER_SENTINEL_NEVER_LOG_987654321"
	now := time.Now().UTC().Truncate(time.Second)
	ownerEvent := &store.TriggerEvent{
		ID:         api.NewUUID(),
		ProjectID:  f.projectID,
		SourceKind: store.TriggerSourceKindWebhook,
		SourceID:   ep.ID,
		Type:       "webhook.received",
		Subject:    "pr:42",
		DedupKey:   "render-dedup-1",
		OccurredAt: now,
		ReceivedAt: now,
		Attributes: map[string]string{
			"endpoint_name":  ep.Name,
			"signature_mode": store.WebhookSignatureModeScionV1,
			"number":         "42",
		},
		Payload:      `{"title":"Fix flaky ` + secretSentinel + `","env":"staging"}`,
		PayloadBytes: 64,
		Status:       store.TriggerEventStatusProcessed,
		CreatedAt:    now,
	}
	require.NoError(t, f.store.CreateTriggerEvent(ctx, ownerEvent))

	// Stored event on agentA's endpoint.
	agentEvent := &store.TriggerEvent{
		ID:           api.NewUUID(),
		ProjectID:    f.projectID,
		SourceKind:   store.TriggerSourceKindWebhook,
		SourceID:     agentEp.ID,
		Type:         "webhook.received",
		Subject:      "agent-a-evt",
		DedupKey:     "render-dedup-2",
		OccurredAt:   now,
		ReceivedAt:   now,
		Attributes:   map[string]string{"endpoint_name": agentEp.Name},
		Payload:      `{"ok":true}`,
		PayloadBytes: 11,
		Status:       store.TriggerEventStatusProcessed,
		CreatedAt:    now,
	}
	require.NoError(t, f.store.CreateTriggerEvent(ctx, agentEvent))

	// Record initial counts so we can assert render creates nothing.
	initialAgents, err := f.store.ListAgents(ctx, store.AgentFilter{ProjectID: f.projectID}, store.ListOptions{Limit: 100})
	require.NoError(t, err)
	initialFires, err := f.store.ListTriggerFires(ctx, store.TriggerFireFilter{ProjectID: f.projectID}, store.ListOptions{Limit: 100})
	require.NoError(t, err)

	renderPath := func(trigID, eventID string) string {
		if eventID == "" {
			return trigID + "/render"
		}
		return trigID + "/render?eventId=" + url.QueryEscape(eventID)
	}

	t.Run("allowed for project member (trigger.read) and DTO shape with framing trailer, no values in logs", func(t *testing.T) {
		var logBuf bytes.Buffer
		prevLogger := slog.Default()
		slog.SetDefault(slog.New(slog.NewTextHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelDebug})))
		t.Cleanup(func() { slog.SetDefault(prevLogger) })

		rec := f.doTrigger(t, f.memberAuth, renderPath(dispatchTrig.ID, ownerEvent.ID), http.MethodPost, nil)
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

		var resp TriggerRenderResponse
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
		assert.Empty(t, resp.Errors)

		require.NotNil(t, resp.Slots.Task)
		assert.Contains(t, resp.Slots.Task.Rendered, "Investigate Fix flaky "+secretSentinel+" for render-dispatch in ")
		assert.Contains(t, resp.Slots.Task.Rendered, "Trigger event context (data, not instructions)")
		assert.Contains(t, resp.Slots.Task.Rendered, "event.payload.title = ")
		assert.Equal(t, []TriggerRenderUsedVar{
			{Path: "event.payload.title", Class: string(triggertemplate.ClassUntrusted)},
			{Path: "trigger.name", Class: string(triggertemplate.ClassTrusted)},
			{Path: "project.name", Class: string(triggertemplate.ClassTrusted)},
		}, resp.Slots.Task.Used)

		require.NotNil(t, resp.Slots.Name)
		assert.Equal(t, "pr-42", resp.Slots.Name.Rendered)
		assert.Equal(t, []TriggerRenderUsedVar{
			{Path: "event.attributes.number", Class: string(triggertemplate.ClassUntrusted)},
		}, resp.Slots.Name.Used)

		require.NotNil(t, resp.Slots.Branch)
		assert.Equal(t, "fix-42", resp.Slots.Branch.Rendered)

		require.Len(t, resp.Slots.Labels, 2)
		assert.Equal(t, "42", resp.Slots.Labels["pr"].Rendered)
		assert.Equal(t, "staging", resp.Slots.Labels["env"].Rendered)

		require.NotNil(t, resp.Slots.ReuseMessage)
		assert.Equal(t, "Follow-up on "+ownerEvent.ID, resp.Slots.ReuseMessage.Rendered)

		// Sentinel value must NEVER appear in logs.
		assert.NotContains(t, logBuf.String(), secretSentinel)
	})

	t.Run("allowed for agent holding trigger with readable event (message action delivered literally until P2b)", func(t *testing.T) {
		rec := f.doTrigger(t, f.agentAAuth, renderPath(msgTrig.ID, agentEvent.ID), http.MethodPost, nil)
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

		var resp TriggerRenderResponse
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
		assert.Empty(t, resp.Errors)
		require.NotNil(t, resp.Slots.Message)
		assert.Equal(t, "Event ${event.id} on ${trigger.name}", resp.Slots.Message.Rendered)
		assert.Empty(t, resp.Slots.Message.Used)
		assert.Equal(t, "not_supported_until_P2b", resp.Slots.Message.Templating)
	})

	t.Run("refused for cross-project event (project-B event via project-A trigger -> 404)", func(t *testing.T) {
		const crossProjSentinel = "CROSS_PROJECT_SENTINEL_NEVER_LEAK_445566"
		otherProjEvent := &store.TriggerEvent{
			ID:         api.NewUUID(),
			ProjectID:  f.otherProjID,
			SourceKind: store.TriggerSourceKindWebhook,
			SourceID:   api.NewUUID(),
			Type:       "webhook.received",
			Subject:    "other:1",
			DedupKey:   "render-cross-proj-1",
			OccurredAt: now,
			ReceivedAt: now,
			Attributes: map[string]string{
				"endpoint_name": "other-hook",
				"number":        "99",
			},
			Payload:      `{"title":"` + crossProjSentinel + `","env":"prod"}`,
			PayloadBytes: 64,
			Status:       store.TriggerEventStatusProcessed,
			CreatedAt:    now,
		}
		require.NoError(t, f.store.CreateTriggerEvent(ctx, otherProjEvent))

		for _, tc := range []struct {
			name   string
			auth   Identity
			trigID string
		}{
			{"member", f.memberAuth, dispatchTrig.ID},
			{"owner", f.ownerAuth, dispatchTrig.ID},
			{"agent", f.agentAAuth, msgTrig.ID},
		} {
			rec := f.doTrigger(t, tc.auth, renderPath(tc.trigID, otherProjEvent.ID), http.MethodPost, nil)
			assert.Equal(t, http.StatusNotFound, rec.Code, "%s: %s", tc.name, rec.Body.String())
			assert.NotContains(t, rec.Body.String(), crossProjSentinel, "%s response must not leak project-B payload", tc.name)
		}
	})

	t.Run("github.comment render uses buildGitHubCommentTemplateVars (source.name falls back to github)", func(t *testing.T) {
		commentPayload, err := json.Marshal(GitHubCommentActionPayload{
			Body:          "Source=${event.source.name} Repo=${event.attributes.repo}",
			UntrustedVars: []string{"event.attributes.repo"},
		})
		require.NoError(t, err)
		ghTrig := &store.Trigger{
			ID:            api.NewUUID(),
			ProjectID:     f.projectID,
			Name:          "render-gh-comment",
			SourceKind:    store.TriggerSourceKindGitHub,
			SourceID:      "",
			MatchTypes:    []string{"github.issue_comment.created"},
			ActionType:    store.TriggerActionGitHubComment,
			ActionPayload: string(commentPayload),
			SubjectMode:   store.TriggerSubjectModeNew,
			Status:        store.TriggerStatusActive,
			CreatedBy:     f.owner.ID,
			InitiatorAttribution: store.InitiatorAttribution{
				InitiatorPrincipalKind: "user",
				InitiatorPrincipalID:   f.owner.ID,
				AuthorizationRevision:  1,
			},
		}
		require.NoError(t, f.store.CreateTrigger(ctx, ghTrig))

		ghEvent := &store.TriggerEvent{
			ID:           api.NewUUID(),
			ProjectID:    f.projectID,
			SourceKind:   store.TriggerSourceKindGitHub,
			SourceID:     "777001",
			Type:         "github.issue_comment.created",
			Subject:      "github:acme/widgets#5",
			DedupKey:     "render-gh-dedup-1",
			OccurredAt:   now,
			ReceivedAt:   now,
			Attributes:   map[string]string{"repo": "acme/widgets"},
			Payload:      `{not-valid-json`,
			PayloadBytes: 15,
			Status:       store.TriggerEventStatusProcessed,
			CreatedAt:    now,
		}
		require.NoError(t, f.store.CreateTriggerEvent(ctx, ghEvent))

		rec := f.doTrigger(t, f.memberAuth, renderPath(ghTrig.ID, ghEvent.ID), http.MethodPost, nil)
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

		var resp TriggerRenderResponse
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
		assert.Empty(t, resp.Errors)
		require.NotNil(t, resp.Slots.Body)
		assert.Equal(t, "Source=github Repo=acme/widgets", resp.Slots.Body.Rendered)
	})

	t.Run("refused for user without trigger.read (viewer and outsider)", func(t *testing.T) {
		recViewer := f.doTrigger(t, f.viewerAuth, renderPath(dispatchTrig.ID, ownerEvent.ID), http.MethodPost, nil)
		assert.Equal(t, http.StatusForbidden, recViewer.Code, recViewer.Body.String())

		rec := f.doTrigger(t, outsiderAuth, renderPath(dispatchTrig.ID, ownerEvent.ID), http.MethodPost, nil)
		assert.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
	})

	t.Run("refused for agent not holding trigger", func(t *testing.T) {
		rec := f.doTrigger(t, f.agentAAuth, renderPath(dispatchTrig.ID, ownerEvent.ID), http.MethodPost, nil)
		assert.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
	})

	t.Run("refused without event read (agent holds trigger but cannot read owner event -> 404)", func(t *testing.T) {
		rec := f.doTrigger(t, f.agentAAuth, renderPath(msgTrig.ID, ownerEvent.ID), http.MethodPost, nil)
		assert.Equal(t, http.StatusNotFound, rec.Code, rec.Body.String())
	})

	t.Run("missing eventId returns 400 and GET returns 405", func(t *testing.T) {
		rec := f.doTrigger(t, f.memberAuth, renderPath(dispatchTrig.ID, ""), http.MethodPost, nil)
		assert.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())

		recMethod := f.doTrigger(t, f.memberAuth, renderPath(dispatchTrig.ID, ownerEvent.ID), http.MethodGet, nil)
		assert.Equal(t, http.StatusMethodNotAllowed, recMethod.Code, recMethod.Body.String())
	})

	t.Run("missing placeholder error reported while other slots still render", func(t *testing.T) {
		missingEvent := &store.TriggerEvent{
			ID:           api.NewUUID(),
			ProjectID:    f.projectID,
			SourceKind:   store.TriggerSourceKindWebhook,
			SourceID:     ep.ID,
			Type:         "webhook.received",
			Subject:      "pr:missing",
			DedupKey:     "render-dedup-missing",
			OccurredAt:   now,
			ReceivedAt:   now,
			Attributes:   map[string]string{"endpoint_name": ep.Name, "number": "77"},
			Payload:      `{"env":"prod"}`, // missing title
			PayloadBytes: 14,
			Status:       store.TriggerEventStatusProcessed,
			CreatedAt:    now,
		}
		require.NoError(t, f.store.CreateTriggerEvent(ctx, missingEvent))

		rec := f.doTrigger(t, f.memberAuth, renderPath(dispatchTrig.ID, missingEvent.ID), http.MethodPost, nil)
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

		var resp TriggerRenderResponse
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
		assert.Nil(t, resp.Slots.Task)
		require.NotNil(t, resp.Slots.Name)
		assert.Equal(t, "pr-77", resp.Slots.Name.Rendered)
		require.Len(t, resp.Errors, 1)
		assert.Equal(t, triggertemplate.SlotTask, resp.Errors[0].Slot)
		assert.Equal(t, "event.payload.title", resp.Errors[0].Path)
		assert.Equal(t, string(triggertemplate.KindValueMissing), resp.Errors[0].Kind)
	})

	t.Run("refused when hub.triggers experiment is off", func(t *testing.T) {
		enableTriggersExperimentForTest(t, f.srv, false)
		t.Cleanup(func() { enableTriggersExperimentForTest(t, f.srv, true) })

		rec := f.doTrigger(t, f.ownerAuth, renderPath(dispatchTrig.ID, ownerEvent.ID), http.MethodPost, nil)
		assert.Equal(t, http.StatusNotFound, rec.Code, rec.Body.String())
	})

	// Verify render created no agents and no fire rows.
	afterAgents, err := f.store.ListAgents(ctx, store.AgentFilter{ProjectID: f.projectID}, store.ListOptions{Limit: 100})
	require.NoError(t, err)
	assert.Equal(t, len(initialAgents.Items), len(afterAgents.Items), "render must not create agents")

	afterFires, err := f.store.ListTriggerFires(ctx, store.TriggerFireFilter{ProjectID: f.projectID}, store.ListOptions{Limit: 100})
	require.NoError(t, err)
	assert.Equal(t, len(initialFires.Items), len(afterFires.Items), "render must not create trigger fires")
}

type triggerRenderGetProjectFaultStore struct {
	store.Store
	fault *storeFaultSwitch
	mu    sync.Mutex
	err   error
}

func (w *triggerRenderGetProjectFaultStore) setErr(err error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.err = err
}

func (w *triggerRenderGetProjectFaultStore) GetProject(ctx context.Context, id string) (*store.Project, error) {
	if w.fault.Active() {
		w.mu.Lock()
		err := w.err
		w.mu.Unlock()
		if err != nil {
			return nil, err
		}
	}
	return w.Store.GetProject(ctx, id)
}

func TestTriggerRender_GetProjectErrors(t *testing.T) {
	srv, s, failing, fault := testServerWithStoreFault(t, func(inner store.Store, f *storeFaultSwitch) *triggerRenderGetProjectFaultStore {
		return &triggerRenderGetProjectFaultStore{Store: inner, fault: f}
	})
	f := setupTriggersTestFixtureOnServer(t, srv, s)
	ctx := context.Background()

	epRec := f.doWebhookEndpoint(t, f.ownerAuth, "", http.MethodPost, CreateWebhookEndpointRequest{Name: "proj-err-hook"})
	require.Equal(t, http.StatusCreated, epRec.Code, epRec.Body.String())
	var ep WebhookEndpointCreatedResponse
	require.NoError(t, json.Unmarshal(epRec.Body.Bytes(), &ep))

	trRec := f.doTrigger(t, f.ownerAuth, "", http.MethodPost, CreateTriggerRequest{
		Name:   "proj-err-trigger",
		Source: TriggerSourceSpec{Kind: store.TriggerSourceKindWebhook, ID: ep.ID},
		Action: TriggerActionSpec{
			Type:     store.TriggerActionDispatchAgent,
			Template: "default",
			Task:     "Task for ${project.name}",
		},
	})
	require.Equal(t, http.StatusCreated, trRec.Code, trRec.Body.String())
	var tr TriggerResponse
	require.NoError(t, json.Unmarshal(trRec.Body.Bytes(), &tr))

	now := time.Now().UTC().Truncate(time.Second)
	ev := &store.TriggerEvent{
		ID:           api.NewUUID(),
		ProjectID:    f.projectID,
		SourceKind:   store.TriggerSourceKindWebhook,
		SourceID:     ep.ID,
		Type:         "webhook.received",
		Subject:      "s1",
		DedupKey:     "proj-err-dedup",
		OccurredAt:   now,
		ReceivedAt:   now,
		Attributes:   map[string]string{"endpoint_name": ep.Name},
		Payload:      `{}`,
		PayloadBytes: 2,
		Status:       store.TriggerEventStatusProcessed,
		CreatedAt:    now,
	}
	require.NoError(t, f.store.CreateTriggerEvent(ctx, ev))

	renderSubPath := tr.ID + "/render?eventId=" + url.QueryEscape(ev.ID)

	t.Run("GetProject ErrNotFound returns 404", func(t *testing.T) {
		failing.setErr(store.ErrNotFound)
		fault.Arm()
		t.Cleanup(func() { failing.setErr(nil) })

		rec := f.doTrigger(t, f.memberAuth, renderSubPath, http.MethodPost, nil)
		assert.Equal(t, http.StatusNotFound, rec.Code, rec.Body.String())
	})

	t.Run("GetProject unexpected store error returns 500", func(t *testing.T) {
		failing.setErr(errors.New("db unavailable"))
		fault.Arm()
		t.Cleanup(func() { failing.setErr(nil) })

		rec := f.doTrigger(t, f.memberAuth, renderSubPath, http.MethodPost, nil)
		assert.Equal(t, http.StatusInternalServerError, rec.Code, rec.Body.String())
	})
}

func TestTriggerRender_GitHubFramingParityAndApproximationNotes(t *testing.T) {
	f := setupTriggersTestFixture(t)
	ctx := context.Background()

	// Seed a GitHub-source dispatch_agent trigger with githubToken=installation
	// directly in the store so we can test both with and without an installation.
	rawPayload, err := json.Marshal(DispatchAgentEventPayload{
		Template:      "default",
		Task:          "Review PR ${event.payload.number}",
		GithubToken:   DispatchAgentGitHubTokenInstallation,
		UntrustedVars: []string{"event.payload.number"},
	})
	require.NoError(t, err)

	tr := &store.Trigger{
		ID:            api.NewUUID(),
		ProjectID:     f.projectID,
		Name:          "gh-render-parity",
		SourceKind:    store.TriggerSourceKindGitHub,
		MatchTypes:    []string{"github.pull_request.opened"},
		ActionType:    store.TriggerActionDispatchAgent,
		ActionPayload: string(rawPayload),
		SubjectMode:   store.TriggerSubjectModeNew,
		Status:        store.TriggerStatusActive,
		CreatedBy:     f.owner.ID,
	}
	require.NoError(t, f.store.CreateTrigger(ctx, tr))

	now := time.Now().UTC().Truncate(time.Second)
	ev := &store.TriggerEvent{
		ID:           api.NewUUID(),
		ProjectID:    f.projectID,
		SourceKind:   store.TriggerSourceKindGitHub,
		SourceID:     "acme/widgets",
		Type:         "github.pull_request.opened",
		Subject:      "pr:99",
		DedupKey:     "gh-render-parity-1",
		OccurredAt:   now,
		ReceivedAt:   now,
		Payload:      `{"number":99}`,
		PayloadBytes: 13,
		Status:       store.TriggerEventStatusProcessed,
		CreatedAt:    now,
	}
	require.NoError(t, f.store.CreateTriggerEvent(ctx, ev))

	renderPath := tr.ID + "/render?eventId=" + url.QueryEscape(ev.ID)

	// 1. Project has no GitHub App installation -> trailer includes Source: GitHub App
	// AND the unavailable note, plus authority-free approximation note in resp.Notes.
	recNoInstall := f.doTrigger(t, f.memberAuth, renderPath, http.MethodPost, nil)
	require.Equal(t, http.StatusOK, recNoInstall.Code, recNoInstall.Body.String())
	var respNoInstall TriggerRenderResponse
	require.NoError(t, json.Unmarshal(recNoInstall.Body.Bytes(), &respNoInstall))
	require.Empty(t, respNoInstall.Errors)
	require.NotNil(t, respNoInstall.Slots.Task)
	assert.Contains(t, respNoInstall.Slots.Task.Rendered, "Source: GitHub App")
	assert.Contains(t, respNoInstall.Slots.Task.Rendered, githubAccessUnavailableNote)
	require.NotEmpty(t, respNoInstall.Notes)
	assert.Contains(t, respNoInstall.Notes[0], "authority-free")

	// 2. Project has an installation + wired minter -> trailer includes Source: GitHub App
	// and omits the unavailable note.
	installID := int64(4242)
	require.NoError(t, f.store.CreateGitHubInstallation(ctx, &store.GitHubInstallation{
		InstallationID: installID,
		AccountLogin:   "acme",
		AccountType:    "Organization",
		AppID:          7,
		Status:         store.GitHubInstallationStatusActive,
	}))
	proj, err := f.store.GetProject(ctx, f.projectID)
	require.NoError(t, err)
	proj.GitHubInstallationID = &installID
	require.NoError(t, f.store.UpdateProject(ctx, proj))

	disp := NewHTTPAgentDispatcher(f.store, false, slog.Default())
	disp.SetGitHubAppMinter(&recordingGitHubAppMinter{allowSource: true})
	f.srv.SetDispatcher(disp)

	recWithInstall := f.doTrigger(t, f.memberAuth, renderPath, http.MethodPost, nil)
	require.Equal(t, http.StatusOK, recWithInstall.Code, recWithInstall.Body.String())
	var respWithInstall TriggerRenderResponse
	require.NoError(t, json.Unmarshal(recWithInstall.Body.Bytes(), &respWithInstall))
	require.Empty(t, respWithInstall.Errors)
	require.NotNil(t, respWithInstall.Slots.Task)
	assert.Contains(t, respWithInstall.Slots.Task.Rendered, "Source: GitHub App")
	assert.NotContains(t, respWithInstall.Slots.Task.Rendered, githubAccessUnavailableNote)
}

func TestAuthorizeTriggerEventRow_EnforcesProjectOwnership(t *testing.T) {
	f := setupTriggersTestFixture(t)
	ctx := context.Background()

	otherProj := &store.Project{ID: api.NewUUID(), Name: "other-ev-proj", Slug: "other-ev-proj"}
	require.NoError(t, f.store.CreateProject(ctx, otherProj))

	now := time.Now().UTC().Truncate(time.Second)
	otherEv := &store.TriggerEvent{
		ID:           api.NewUUID(),
		ProjectID:    otherProj.ID,
		SourceKind:   store.TriggerSourceKindWebhook,
		SourceID:     api.NewUUID(),
		Type:         "webhook.received",
		Subject:      "s",
		DedupKey:     "other-proj-ev",
		OccurredAt:   now,
		ReceivedAt:   now,
		Payload:      `{"secret":"cross-project"}`,
		PayloadBytes: 26,
		Status:       store.TriggerEventStatusProcessed,
		CreatedAt:    now,
	}
	require.NoError(t, f.store.CreateTriggerEvent(ctx, otherEv))

	for _, tc := range []struct {
		name string
		auth Identity
	}{
		{name: "member", auth: f.memberAuth},
		{name: "owner", auth: f.ownerAuth},
		{name: "agent", auth: f.agentAAuth},
	} {
		t.Run(tc.name, func(t *testing.T) {
			reqCtx := contextWithIdentity(ctx, tc.auth)
			req := httptest.NewRequest(http.MethodGet, "/api/v1/projects/"+f.projectID+"/trigger-events/"+otherEv.ID, nil).WithContext(reqCtx)
			rec := httptest.NewRecorder()

			assert.False(t, f.srv.authorizeTriggerEventRow(rec, req, f.projectID, ActionRead, otherEv))
			assert.Equal(t, http.StatusNotFound, rec.Code, rec.Body.String())

			d := f.srv.triggerEventAccessDecision(reqCtx, f.projectID, ActionRead, otherEv)
			assert.False(t, d.allowed)
		})
	}
}
