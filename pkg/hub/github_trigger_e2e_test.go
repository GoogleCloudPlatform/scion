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
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/experiments"
	"github.com/GoogleCloudPlatform/scion/pkg/hub/githubapp"
	"github.com/GoogleCloudPlatform/scion/pkg/hub/trigger"
	"github.com/GoogleCloudPlatform/scion/pkg/messaging"
	"github.com/GoogleCloudPlatform/scion/pkg/secret"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/util"
)

func setTriggerExperimentsForG1bTest(t *testing.T, srv *Server, triggersOn, githubTriggersOn bool) {
	t.Helper()
	var active []experiments.Experiment
	for _, e := range srv.experimentRegistry().All() {
		switch e.Name {
		case experiments.Triggers:
			e.Default = triggersOn
		case experiments.GitHubTriggers:
			e.Default = githubTriggersOn
		}
		active = append(active, e)
	}
	reg, err := experiments.NewRegistry(active, srv.experimentRegistry().Retired())
	require.NoError(t, err)
	srv.mu.Lock()
	srv.experiments = reg
	srv.mu.Unlock()
	srv.reconcileTriggerDrainer(context.Background())
}

func decodeAPIErrorCode(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	var er ErrorResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &er), "body: %s", rec.Body.String())
	return er.Error.Code
}

func TestGitHubTriggerAuthoring_FlagSourceIDMatchTypesAssociationAndAgentRules(t *testing.T) {
	f := setupTriggersTestFixture(t)
	ctx := context.Background()

	// Install a fake GitHub API and fail the test if any authoring call hits GitHub.
	gh := newFakeGitHubForAssoc(t)
	f.srv.mu.Lock()
	f.srv.config.GitHubAppConfig.APIBaseURL = gh.srv.URL
	f.srv.config.GitHubAppConfig.AppID = 778899
	f.srv.config.GitHubAppConfig.AppSlug = "scion-bot"
	f.srv.config.GitHubAppConfig.WebhookSecret = "g1b-authoring-secret"
	f.srv.config.GitHubAppConfig.WebhooksEnabled = true
	f.srv.mu.Unlock()

	sysAdmin := createAssocAdmin(t, f.store, "g1b-sys-admin")
	const instID int64 = 6101
	createAssocInstallation(t, f.store, instID, "acme", "acme/widgets")

	// 1. hub.github_triggers off -> 400 experiment_disabled on create.
	setTriggerExperimentsForG1bTest(t, f.srv, true, false)
	rec := f.doTrigger(t, f.ownerAuth, "", http.MethodPost, CreateTriggerRequest{
		Name:   "gh-flag-off",
		Source: TriggerSourceSpec{Kind: store.TriggerSourceKindGitHub, ID: ""},
		Match:  &TriggerMatchSpec{Types: []string{"github.issue_comment.created"}},
		Action: TriggerActionSpec{Type: store.TriggerActionMessage, AgentID: f.targetAgent.ID, Message: "hi"},
	})
	require.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
	assert.Equal(t, ErrCodeExperimentDisabled, decodeAPIErrorCode(t, rec))

	// Enable both hub.triggers and hub.github_triggers for remaining authoring checks.
	setTriggerExperimentsForG1bTest(t, f.srv, true, true)

	// 2. Non-empty source.id on source.kind == "github" -> 400.
	rec = f.doTrigger(t, f.ownerAuth, "", http.MethodPost, CreateTriggerRequest{
		Name:   "gh-nonempty-source-id",
		Source: TriggerSourceSpec{Kind: store.TriggerSourceKindGitHub, ID: "6101"},
		Match:  &TriggerMatchSpec{Types: []string{"github.issue_comment.created"}},
		Action: TriggerActionSpec{Type: store.TriggerActionMessage, AgentID: f.targetAgent.ID, Message: "hi"},
	})
	require.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())

	// 3. Project has no GitHub installation -> 409 github_not_associated (0 GitHub calls).
	rec = f.doTrigger(t, f.ownerAuth, "", http.MethodPost, CreateTriggerRequest{
		Name:   "gh-unassociated",
		Source: TriggerSourceSpec{Kind: store.TriggerSourceKindGitHub, ID: ""},
		Match:  &TriggerMatchSpec{Types: []string{"github.issue_comment.created"}},
		Action: TriggerActionSpec{Type: store.TriggerActionMessage, AgentID: f.targetAgent.ID, Message: "hi"},
	})
	require.Equal(t, http.StatusConflict, rec.Code, rec.Body.String())
	assert.Equal(t, ErrCodeGitHubNotAssociated, decodeAPIErrorCode(t, rec))

	// Associate project with instID + matching remote, CreatedBy = owner.ID, but NO admin binding and NO creator GITHUB_TOKEN -> 409.
	remote := gh.remote("acme", "widgets")
	proj, err := f.store.GetProject(ctx, f.projectID)
	require.NoError(t, err)
	idCopy := instID
	proj.GitRemote = remote
	proj.GitHubInstallationID = &idCopy
	proj.CreatedBy = f.owner.ID
	require.NoError(t, f.store.UpdateProject(ctx, proj))
	f.srv.invalidateGitHubAssociation(proj.ID)

	rec = f.doTrigger(t, f.ownerAuth, "", http.MethodPost, CreateTriggerRequest{
		Name:   "gh-no-proof",
		Source: TriggerSourceSpec{Kind: store.TriggerSourceKindGitHub, ID: ""},
		Match:  &TriggerMatchSpec{Types: []string{"github.issue_comment.created"}},
		Action: TriggerActionSpec{Type: store.TriggerActionMessage, AgentID: f.targetAgent.ID, Message: "hi"},
	})
	require.Equal(t, http.StatusConflict, rec.Code, rec.Body.String())
	assert.Equal(t, ErrCodeGitHubNotAssociated, decodeAPIErrorCode(t, rec))

	// 4. Cold-cache creator-proof case on a project whose CreatedBy is persisted at creation time:
	// first without GITHUB_TOKEN -> 409, then with stored GITHUB_TOKEN + cold caches -> 201 with 0 GitHub HTTP calls.
	creatorInstCopy := instID
	creatorProj := createAssocProject(t, f.store, "g1b-creator-proj", remote, f.owner.ID, &creatorInstCopy)
	require.NoError(t, f.srv.createProjectOwnerRoleBinding(ctx, creatorProj.ID, f.owner.ID))
	creatorTarget := &store.Agent{
		ID:        tid("g1b-creator-target"),
		Slug:      "creator-target",
		Name:      "creator-target",
		ProjectID: creatorProj.ID,
		Phase:     string(state.PhaseRunning),
		CreatedBy: f.owner.ID,
		OwnerID:   f.owner.ID,
		Ancestry:  []string{f.owner.ID},
	}
	require.NoError(t, f.store.CreateAgent(ctx, creatorTarget))
	doCreatorTrigger := func(body any) *httptest.ResponseRecorder {
		path := "/api/v1/projects/" + creatorProj.ID + "/triggers"
		req := authoredRequest(t, f.ownerAuth, http.MethodPost, path, body)
		w := httptest.NewRecorder()
		f.srv.handleTriggers(w, req, creatorProj.ID, "")
		return w
	}

	// No stored GITHUB_TOKEN yet -> 409 github_not_associated.
	rec = doCreatorTrigger(CreateTriggerRequest{
		Name:   "gh-creator-no-token",
		Source: TriggerSourceSpec{Kind: store.TriggerSourceKindGitHub, ID: ""},
		Match:  &TriggerMatchSpec{Types: []string{"github.issue_comment.created"}},
		Action: TriggerActionSpec{Type: store.TriggerActionMessage, AgentID: creatorTarget.ID, Message: "hi"},
	})
	require.Equal(t, http.StatusConflict, rec.Code, rec.Body.String())
	assert.Equal(t, ErrCodeGitHubNotAssociated, decodeAPIErrorCode(t, rec))

	// Store user-scope GITHUB_TOKEN for f.owner.ID while caches stay cold.
	_, _, err = f.srv.secretBackend.Set(ctx, &secret.SetSecretInput{
		Name:       "GITHUB_TOKEN",
		Value:      "ghp_owner_token",
		SecretType: secret.TypeEnvironment,
		Scope:      secret.ScopeUser,
		ScopeID:    f.owner.ID,
	})
	require.NoError(t, err)

	// Empty match.types on source.kind == "github" -> 400 (no webhook.received default).
	rec = doCreatorTrigger(CreateTriggerRequest{
		Name:   "gh-missing-types",
		Source: TriggerSourceSpec{Kind: store.TriggerSourceKindGitHub, ID: ""},
		Action: TriggerActionSpec{Type: store.TriggerActionMessage, AgentID: creatorTarget.ID, Message: "hi"},
	})
	require.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())

	// Valid cold-cache creator-proof create -> 201 Created, zero GitHub HTTP calls.
	rec = doCreatorTrigger(CreateTriggerRequest{
		Name:   "gh-cold-creator-ok",
		Source: TriggerSourceSpec{Kind: store.TriggerSourceKindGitHub, ID: ""},
		Match:  &TriggerMatchSpec{Types: []string{"github.issue_comment.created"}},
		Action: TriggerActionSpec{Type: store.TriggerActionMessage, AgentID: creatorTarget.ID, Message: "Check PR comment"},
	})
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	var createdCold TriggerResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &createdCold))
	assert.Equal(t, store.TriggerSourceKindGitHub, createdCold.Source.Kind)
	assert.Empty(t, createdCold.Source.ID)
	assert.Equal(t, []string{"github.issue_comment.created"}, createdCold.Match.Types)

	repoCalls, mints := gh.counts()
	assert.Zero(t, repoCalls, "authoring must never call GitHub GET /repos")
	assert.Empty(t, mints, "authoring must never mint a GitHub token")

	// 5. If GitHubAppStatus shows association_unauthorized, cold-cache creator-proof refuses with 409.
	creatorProj, err = f.store.GetProject(ctx, creatorProj.ID)
	require.NoError(t, err)
	f.srv.updateProjectGitHubAppStatus(ctx, creatorProj, store.GitHubAppStateError, githubAppErrAssociationUnauthorized, "denied")
	rec = doCreatorTrigger(CreateTriggerRequest{
		Name:   "gh-status-unauthorized",
		Source: TriggerSourceSpec{Kind: store.TriggerSourceKindGitHub, ID: ""},
		Match:  &TriggerMatchSpec{Types: []string{"github.issue_comment.created"}},
		Action: TriggerActionSpec{Type: store.TriggerActionMessage, AgentID: creatorTarget.ID, Message: "hi"},
	})
	require.Equal(t, http.StatusConflict, rec.Code, rec.Body.String())
	assert.Equal(t, ErrCodeGitHubNotAssociated, decodeAPIErrorCode(t, rec))

	// 6. Warm association report cache answers directly (both false and true) with 0 GitHub calls.
	now := time.Now()
	f.srv.githubAssociationReportCache.mu.Lock()
	f.srv.githubAssociationReportCache.valid = true
	f.srv.githubAssociationReportCache.generatedAt = now
	f.srv.githubAssociationReportCache.dirty = map[string]uint64{}
	f.srv.githubAssociationReportCache.entries = map[string]githubAssociationReportEntry{
		f.projectID: {
			ProjectID:      f.projectID,
			ProjectName:    proj.Name,
			GitRemote:      proj.GitRemote,
			InstallationID: instID,
			Authorized:     false,
			ErrorCode:      githubAppErrAssociationUnauthorized,
		},
	}
	f.srv.githubAssociationReportCache.mu.Unlock()

	rec = f.doTrigger(t, f.ownerAuth, "", http.MethodPost, CreateTriggerRequest{
		Name:   "gh-report-cache-denied",
		Source: TriggerSourceSpec{Kind: store.TriggerSourceKindGitHub, ID: ""},
		Match:  &TriggerMatchSpec{Types: []string{"github.issue_comment.created"}},
		Action: TriggerActionSpec{Type: store.TriggerActionMessage, AgentID: f.targetAgent.ID, Message: "hi"},
	})
	require.Equal(t, http.StatusConflict, rec.Code, rec.Body.String())
	assert.Equal(t, ErrCodeGitHubNotAssociated, decodeAPIErrorCode(t, rec))

	f.srv.githubAssociationReportCache.mu.Lock()
	f.srv.githubAssociationReportCache.entries[f.projectID] = githubAssociationReportEntry{
		ProjectID:      f.projectID,
		ProjectName:    proj.Name,
		GitRemote:      proj.GitRemote,
		InstallationID: instID,
		Authorized:     true,
		Basis:          githubAssociationBasisAdminBinding,
		WriteAllowed:   true,
	}
	f.srv.githubAssociationReportCache.mu.Unlock()

	// Set real admin binding as well so subsequent invalidations also stay authorized.
	proj, err = f.store.GetProject(ctx, f.projectID)
	require.NoError(t, err)
	proj.GitHubAppStatus = &store.GitHubAppProjectStatus{State: store.GitHubAppStateOK, LastChecked: now}
	require.NoError(t, f.store.UpdateProject(ctx, proj))
	setAdminBinding(t, f.store, proj, instID, util.NormalizeGitRemote(remote), sysAdmin.ID)

	// 7. PATCH webhook trigger -> github trigger, pause, and resume checks.
	epRec := f.doWebhookEndpoint(t, f.ownerAuth, "", http.MethodPost, CreateWebhookEndpointRequest{Name: "patch-src-ep"})
	require.Equal(t, http.StatusCreated, epRec.Code)
	var ep WebhookEndpointCreatedResponse
	require.NoError(t, json.Unmarshal(epRec.Body.Bytes(), &ep))

	whTrRec := f.doTrigger(t, f.ownerAuth, "", http.MethodPost, CreateTriggerRequest{
		Name:   "wh-to-gh",
		Source: TriggerSourceSpec{Kind: store.TriggerSourceKindWebhook, ID: ep.ID},
		Match:  &TriggerMatchSpec{Types: []string{"github.pull_request.opened"}},
		Action: TriggerActionSpec{Type: store.TriggerActionMessage, AgentID: f.targetAgent.ID, Message: "PR opened"},
	})
	require.Equal(t, http.StatusCreated, whTrRec.Code)
	var whTr TriggerResponse
	require.NoError(t, json.Unmarshal(whTrRec.Body.Bytes(), &whTr))

	// Patch-to-github with flag off -> 400 experiment_disabled.
	setTriggerExperimentsForG1bTest(t, f.srv, true, false)
	patchTypes := []string{"github.pull_request.opened"}
	patchOff := f.doTrigger(t, f.ownerAuth, whTr.ID, http.MethodPatch, UpdateTriggerRequest{
		Source: &TriggerSourceSpec{Kind: store.TriggerSourceKindGitHub, ID: ""},
		Match:  &TriggerMatchPatchSpec{Types: &patchTypes},
	})
	require.Equal(t, http.StatusBadRequest, patchOff.Code)
	assert.Equal(t, ErrCodeExperimentDisabled, decodeAPIErrorCode(t, patchOff))

	// Patch-to-github with flag on WITHOUT match.types -> 400 Bad Request.
	setTriggerExperimentsForG1bTest(t, f.srv, true, true)
	patchMissingMatch := f.doTrigger(t, f.ownerAuth, whTr.ID, http.MethodPatch, UpdateTriggerRequest{
		Source: &TriggerSourceSpec{Kind: store.TriggerSourceKindGitHub, ID: ""},
	})
	require.Equal(t, http.StatusBadRequest, patchMissingMatch.Code, patchMissingMatch.Body.String())

	// Patch-to-github with flag on AND match.types -> 200 OK.
	patchOK := f.doTrigger(t, f.ownerAuth, whTr.ID, http.MethodPatch, UpdateTriggerRequest{
		Source: &TriggerSourceSpec{Kind: store.TriggerSourceKindGitHub, ID: ""},
		Match:  &TriggerMatchPatchSpec{Types: &patchTypes},
	})
	require.Equal(t, http.StatusOK, patchOK.Code, patchOK.Body.String())
	var patched TriggerResponse
	require.NoError(t, json.Unmarshal(patchOK.Body.Bytes(), &patched))
	assert.Equal(t, store.TriggerSourceKindGitHub, patched.Source.Kind)
	assert.Empty(t, patched.Source.ID)
	assert.Equal(t, patchTypes, patched.Match.Types)

	// Pause, then verify resume requires both hub.github_triggers and authorized association.
	pauseRec := f.doTrigger(t, f.ownerAuth, whTr.ID+"/pause", http.MethodPost, nil)
	require.Equal(t, http.StatusOK, pauseRec.Code)

	setTriggerExperimentsForG1bTest(t, f.srv, true, false)
	resOff := f.doTrigger(t, f.ownerAuth, whTr.ID+"/resume", http.MethodPost, nil)
	require.Equal(t, http.StatusBadRequest, resOff.Code)
	assert.Equal(t, ErrCodeExperimentDisabled, decodeAPIErrorCode(t, resOff))

	setTriggerExperimentsForG1bTest(t, f.srv, true, true)
	resOK := f.doTrigger(t, f.ownerAuth, whTr.ID+"/resume", http.MethodPost, nil)
	require.Equal(t, http.StatusOK, resOK.Code, resOK.Body.String())

	// 8. Agent callers (§8.1): Agent A may author a github trigger with a message action targeting itself,
	// cannot target another agent, and Agent B cannot see Agent A's github trigger.
	agentSelfRec := f.doTrigger(t, f.agentAAuth, "", http.MethodPost, CreateTriggerRequest{
		Name:   "agent-a-self-gh",
		Source: TriggerSourceSpec{Kind: store.TriggerSourceKindGitHub, ID: ""},
		Match:  &TriggerMatchSpec{Types: []string{"github.issue_comment.created"}},
		Action: TriggerActionSpec{Type: store.TriggerActionMessage, AgentID: f.agentA.ID, Message: "Self ping"},
	})
	require.Equal(t, http.StatusCreated, agentSelfRec.Code, agentSelfRec.Body.String())

	agentOtherRec := f.doTrigger(t, f.agentAAuth, "", http.MethodPost, CreateTriggerRequest{
		Name:   "agent-a-other-gh",
		Source: TriggerSourceSpec{Kind: store.TriggerSourceKindGitHub, ID: ""},
		Match:  &TriggerMatchSpec{Types: []string{"github.issue_comment.created"}},
		Action: TriggerActionSpec{Type: store.TriggerActionMessage, AgentID: f.agentB.ID, Message: "Cross-agent disallowed"},
	})
	assert.Equal(t, http.StatusForbidden, agentOtherRec.Code, agentOtherRec.Body.String())

	agentUnreadableRec := f.doTrigger(t, f.agentAAuth, "", http.MethodPost, CreateTriggerRequest{
		Name:   "agent-a-unreadable-gh",
		Source: TriggerSourceSpec{Kind: store.TriggerSourceKindGitHub, ID: ""},
		Match:  &TriggerMatchSpec{Types: []string{"github.issue_comment.created"}},
		Action: TriggerActionSpec{Type: store.TriggerActionMessage, AgentID: creatorTarget.ID, Message: "Unreadable target disallowed"},
	})
	assert.Equal(t, http.StatusNotFound, agentUnreadableRec.Code, agentUnreadableRec.Body.String())

	listBRec := f.doTrigger(t, f.agentBAuth, "?source=github", http.MethodGet, nil)
	require.Equal(t, http.StatusOK, listBRec.Code)
	var listB ListTriggersResponse
	require.NoError(t, json.Unmarshal(listBRec.Body.Bytes(), &listB))
	assert.Equal(t, "caller", listB.Scope)
	assert.Empty(t, listB.Triggers, "Agent B must not see Agent A's or owner's GitHub triggers")

	repoCalls, mints = gh.counts()
	assert.Zero(t, repoCalls, "entire authoring suite must make zero GitHub HTTP calls")
	assert.Empty(t, mints)
}

func postSignedGitHubDelivery(
	t *testing.T,
	srv *Server,
	secret string,
	appID int64,
	eventName, deliveryID string,
	body []byte,
) *httptest.ResponseRecorder {
	t.Helper()
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	sig := "sha256=" + hex.EncodeToString(mac.Sum(nil))

	req := httptest.NewRequest(http.MethodPost, "/api/v1/webhooks/github", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "GitHub-Hookshot/g1b-e2e")
	req.Header.Set("X-GitHub-Event", eventName)
	req.Header.Set("X-GitHub-Delivery", deliveryID)
	req.Header.Set("X-Hub-Signature-256", sig)
	req.Header.Set("X-GitHub-Hook-ID", "4242")
	req.Header.Set("X-GitHub-Hook-Installation-Target-Type", "integration")
	req.Header.Set("X-GitHub-Hook-Installation-Target-ID", strconv.FormatInt(appID, 10))

	rec := httptest.NewRecorder()
	srv.handleGitHubWebhook(rec, req)
	return rec
}

func TestGitHubTriggerE2E_SignedWebhookToFakeAgentHistoryRedeliveryAndRefusal(t *testing.T) {
	f := setupTriggersTestFixture(t)
	enableWriteDenySwitch(t, f.srv)
	ctx := context.Background()

	const (
		appID         int64  = 778899
		webhookSecret string = "g1b-e2e-webhook-secret"
		fixtureInstID int64  = 987654 // matches pkg/hub/trigger/github/testdata/issue_comment__created_pr.payload.json
	)

	fixtureBytes, err := os.ReadFile(filepath.Join("trigger", "github", "testdata", "issue_comment__created_pr.payload.json"))
	require.NoError(t, err)

	f.srv.mu.Lock()
	f.srv.config.GitHubAppConfig.AppID = appID
	f.srv.config.GitHubAppConfig.AppSlug = "octo-app"
	f.srv.config.GitHubAppConfig.WebhookSecret = webhookSecret
	f.srv.config.GitHubAppConfig.WebhooksEnabled = true
	f.srv.mu.Unlock()

	disp := &recordingDispatcher{}
	f.srv.SetDispatcher(disp)

	done := f.srv.startTriggerDrainer(ctx)
	require.NotNil(t, done)
	t.Cleanup(func() {
		stopCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		f.srv.stopTriggerDrainer(stopCtx)
	})

	sysAdmin := createAssocAdmin(t, f.store, "g1b-e2e-sys-admin")
	createAssocInstallation(t, f.store, fixtureInstID, "octo-org", "octo-org/octo-repo")

	proj, err := f.store.GetProject(ctx, f.projectID)
	require.NoError(t, err)
	instCopy := fixtureInstID
	proj.GitRemote = "https://github.com/octo-org/octo-repo.git"
	proj.GitHubInstallationID = &instCopy
	require.NoError(t, f.store.UpdateProject(ctx, proj))
	setAdminBinding(t, f.store, proj, fixtureInstID, "github.com/octo-org/octo-repo", sysAdmin.ID)
	f.srv.invalidateGitHubAssociation(proj.ID)

	// 1. Flag off (hub.github_triggers disabled) -> 200 ignored, zero events, zero fires, zero deliveries.
	setTriggerExperimentsForG1bTest(t, f.srv, true, false)
	offRec := postSignedGitHubDelivery(t, f.srv, webhookSecret, appID, "issue_comment", uuid.NewString(), fixtureBytes)
	require.Equal(t, http.StatusOK, offRec.Code, offRec.Body.String())
	evsOff, err := f.store.ListTriggerEvents(ctx, store.TriggerEventFilter{ProjectID: f.projectID}, store.ListOptions{})
	require.NoError(t, err)
	assert.Empty(t, evsOff.Items)
	assert.Empty(t, disp.getCalls())

	// 2. Enable hub.github_triggers and author a github trigger (Types-only, message action, authored by Agent A to itself).
	setTriggerExperimentsForG1bTest(t, f.srv, true, true)
	trRec := f.doTrigger(t, f.agentAAuth, "", http.MethodPost, CreateTriggerRequest{
		Name:   "on-pr-comment",
		Source: TriggerSourceSpec{Kind: store.TriggerSourceKindGitHub, ID: ""},
		Match:  &TriggerMatchSpec{Types: []string{"github.issue_comment.created"}},
		Action: TriggerActionSpec{
			Type:    store.TriggerActionMessage,
			AgentID: f.agentA.ID,
			Message: "Handle incoming GitHub PR comment",
		},
	})
	require.Equal(t, http.StatusCreated, trRec.Code, trRec.Body.String())
	var tr TriggerResponse
	require.NoError(t, json.Unmarshal(trRec.Body.Bytes(), &tr))

	// Post signed synthetic issue_comment.created delivery from testdata.
	delivID := "72d3162e-cc78-11e3-81ab-4c9367dc0958"
	ingRec := postSignedGitHubDelivery(t, f.srv, webhookSecret, appID, "issue_comment", delivID, fixtureBytes)
	require.Equal(t, http.StatusAccepted, ingRec.Code, ingRec.Body.String())
	var ingResp GitHubWebhookAcceptedResponse
	require.NoError(t, json.Unmarshal(ingRec.Body.Bytes(), &ingResp))
	assert.Equal(t, GitHubWebhookStatusQueued, ingResp.Status)
	require.Len(t, ingResp.Events, 1)
	eventID := ingResp.Events[0].EventID

	// Wait for drainer to process event and deliver envelope to fake agent.
	require.Eventually(t, func() bool {
		stored, gErr := f.store.GetTriggerEvent(ctx, eventID)
		return gErr == nil && stored != nil && stored.Status == store.TriggerEventStatusProcessed
	}, 5*time.Second, 20*time.Millisecond)

	calls := disp.getCalls()
	require.Len(t, calls, 1)
	assert.Equal(t, f.agentA.ID, calls[0].Agent.ID)
	assert.Equal(t, "Handle incoming GitHub PR comment", calls[0].Message)
	require.NotNil(t, calls[0].StructuredMessage)

	env := extractEnvelopeJSONFromDelivery(t, calls[0].StructuredMessage.DeliveryText)
	assert.Equal(t, "event", env.Type)
	assert.Equal(t, "Handle incoming GitHub PR comment", env.Msg)
	require.NotNil(t, env.Event)
	assert.Equal(t, messaging.EventTriggerFired, env.Event.Type)
	assert.Equal(t, "github.issue_comment.created", env.Event.EventType)
	assert.Equal(t, eventID, env.Event.EventID)
	assert.Equal(t, "github:octo-org/octo-repo#42", env.Event.Subject)
	require.NotNil(t, env.Event.Source)
	assert.Equal(t, store.TriggerSourceKindGitHub, env.Event.Source.Kind)
	assert.Equal(t, strconv.FormatInt(fixtureInstID, 10), env.Event.Source.ID)
	require.NotNil(t, env.Event.Trigger)
	assert.Equal(t, tr.ID, env.Event.Trigger.ID)
	assert.Equal(t, "on-pr-comment", env.Event.Trigger.Name)
	assert.Equal(t, "review", env.Event.Attributes["command"])
	assert.Equal(t, "--thorough", env.Event.Attributes["command_args"])
	assert.NotEmpty(t, env.Event.Lists["labels"])
	require.NotNil(t, env.Event.External)
	assert.Equal(t, messaging.ExternalVerifiedGitHubHMACSHA256Projected, env.Event.External.Verified)
	assert.False(t, env.Event.External.Truncated)

	// Fire history row shows status = "fired".
	histRec := f.doTrigger(t, f.agentAAuth, tr.ID+"/history", http.MethodGet, nil)
	require.Equal(t, http.StatusOK, histRec.Code, histRec.Body.String())
	var hist ListTriggerFiresResponse
	require.NoError(t, json.Unmarshal(histRec.Body.Bytes(), &hist))
	require.Len(t, hist.Fires, 1)
	assert.Equal(t, store.TriggerFireStatusFired, hist.Fires[0].Status)
	assert.Equal(t, eventID, hist.Fires[0].EventID)

	// Agent A can read GET /trigger-events/{id} (fired by its trigger); Agent B gets 404.
	evtARec := f.doTriggerEvents(t, f.agentAAuth, eventID, http.MethodGet, nil)
	require.Equal(t, http.StatusOK, evtARec.Code, evtARec.Body.String())
	var evtDetail TriggerEventDetailResponse
	require.NoError(t, json.Unmarshal(evtARec.Body.Bytes(), &evtDetail))
	assert.Equal(t, "review", evtDetail.Attributes["command"])
	assert.NotEmpty(t, evtDetail.Lists["labels"])
	assert.NotEmpty(t, evtDetail.Payload)

	evtBRec := f.doTriggerEvents(t, f.agentBAuth, eventID, http.MethodGet, nil)
	assert.Equal(t, http.StatusNotFound, evtBRec.Code)

	// 3. Redelivery (both identical GUID and fresh GUID with identical signed bytes) creates nothing.
	for _, replayGUID := range []string{delivID, uuid.NewString()} {
		dupRec := postSignedGitHubDelivery(t, f.srv, webhookSecret, appID, "issue_comment", replayGUID, fixtureBytes)
		require.Equal(t, http.StatusAccepted, dupRec.Code, dupRec.Body.String())
		var dupResp GitHubWebhookAcceptedResponse
		require.NoError(t, json.Unmarshal(dupRec.Body.Bytes(), &dupResp))
		assert.Equal(t, GitHubWebhookStatusDuplicate, dupResp.Status)
	}
	evsAfterDup, err := f.store.ListTriggerEvents(ctx, store.TriggerEventFilter{ProjectID: f.projectID}, store.ListOptions{})
	require.NoError(t, err)
	assert.Len(t, evsAfterDup.Items, 1)
	assert.Len(t, disp.getCalls(), 1)

	// 4. Unauthorized association at fire time: remove the admin binding and leave creator with no repo access ->
	// ingress queues the new delivery, drainer records refused "github association not authorized", updates
	// project.GitHubAppStatus just like a refused mint, and delivers nothing to the agent.
	proj, err = f.store.GetProject(ctx, f.projectID)
	require.NoError(t, err)
	delete(proj.Annotations, githubInstallationBindingAnnotation)
	proj.CreatedBy = f.owner.ID // owner has no user-scope GITHUB_TOKEN in this fixture
	proj.GitHubAppStatus = &store.GitHubAppProjectStatus{State: store.GitHubAppStateOK, LastChecked: time.Now()}
	require.NoError(t, f.store.UpdateProject(ctx, proj))
	f.srv.invalidateGitHubAssociation(proj.ID)

	secondFixtureBytes, err := os.ReadFile(filepath.Join("trigger", "github", "testdata", "issue_comment__created_issue.payload.json"))
	require.NoError(t, err)
	unauthDeliv := uuid.NewString()
	unauthRec := postSignedGitHubDelivery(t, f.srv, webhookSecret, appID, "issue_comment", unauthDeliv, secondFixtureBytes)
	require.Equal(t, http.StatusAccepted, unauthRec.Code, unauthRec.Body.String())
	var unauthResp GitHubWebhookAcceptedResponse
	require.NoError(t, json.Unmarshal(unauthRec.Body.Bytes(), &unauthResp))
	require.Len(t, unauthResp.Events, 1)
	unauthEventID := unauthResp.Events[0].EventID

	require.Eventually(t, func() bool {
		stored, gErr := f.store.GetTriggerEvent(ctx, unauthEventID)
		return gErr == nil && stored != nil && stored.Status == store.TriggerEventStatusProcessed
	}, 5*time.Second, 20*time.Millisecond)

	assert.Len(t, disp.getCalls(), 1, "unauthorized association must deliver nothing to the target agent")

	histRec2 := f.doTrigger(t, f.agentAAuth, tr.ID+"/history", http.MethodGet, nil)
	require.Equal(t, http.StatusOK, histRec2.Code, histRec2.Body.String())
	var hist2 ListTriggerFiresResponse
	require.NoError(t, json.Unmarshal(histRec2.Body.Bytes(), &hist2))
	require.Len(t, hist2.Fires, 2)
	assert.Equal(t, unauthEventID, hist2.Fires[0].EventID)
	assert.Equal(t, store.TriggerFireStatusRefused, hist2.Fires[0].Status)
	assert.Equal(t, trigger.ErrFireGitHubAssociationUnauthorized, hist2.Fires[0].Error)

	updatedProj, err := f.store.GetProject(ctx, f.projectID)
	require.NoError(t, err)
	require.NotNil(t, updatedProj.GitHubAppStatus)
	assert.Equal(t, store.GitHubAppStateError, updatedProj.GitHubAppStatus.State)
	assert.Equal(t, githubAppErrAssociationUnauthorized, updatedProj.GitHubAppStatus.ErrorCode)
}

func TestGitHubTriggerE2E_InconclusiveAssociationCheckRetriesUntilRecovered(t *testing.T) {
	f := setupTriggersTestFixture(t)
	enableWriteDenySwitch(t, f.srv)
	ctx := context.Background()

	const (
		appID         int64  = 778899
		webhookSecret string = "g1b-transient-webhook-secret"
		fixtureInstID int64  = 987654 // matches pkg/hub/trigger/github/testdata/issue_comment__created_pr.payload.json
	)

	fixtureBytes, err := os.ReadFile(filepath.Join("trigger", "github", "testdata", "issue_comment__created_pr.payload.json"))
	require.NoError(t, err)

	gh := newFakeGitHubForAssoc(t)
	gh.setRepoStatus(http.StatusInternalServerError) // Simulate transient GitHub 500 on creator-proof GET /repos

	f.srv.mu.Lock()
	f.srv.config.GitHubAppConfig.APIBaseURL = gh.srv.URL
	f.srv.config.GitHubAppConfig.AppID = appID
	f.srv.config.GitHubAppConfig.AppSlug = "octo-app"
	f.srv.config.GitHubAppConfig.WebhookSecret = webhookSecret
	f.srv.config.GitHubAppConfig.WebhooksEnabled = true
	f.srv.mu.Unlock()

	disp := &recordingDispatcher{}
	f.srv.SetDispatcher(disp)

	done := f.srv.startTriggerDrainer(ctx)
	require.NotNil(t, done)
	t.Cleanup(func() {
		stopCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		f.srv.stopTriggerDrainer(stopCtx)
	})

	createAssocInstallation(t, f.store, fixtureInstID, "octo-org", "octo-org/octo-repo")

	// Creator-proof project (CreatedBy persisted at CreateProject time, no admin binding).
	instCopy := fixtureInstID
	creatorProj := createAssocProject(t, f.store, "g1b-transient-proj", gh.remote("octo-org", "octo-repo"), f.owner.ID, &instCopy)
	require.NoError(t, f.srv.createProjectOwnerRoleBinding(ctx, creatorProj.ID, f.owner.ID))

	target := &store.Agent{
		ID:        tid("g1b-transient-target"),
		Slug:      "transient-target",
		Name:      "transient-target",
		ProjectID: creatorProj.ID,
		Phase:     string(state.PhaseRunning),
		CreatedBy: f.owner.ID,
		OwnerID:   f.owner.ID,
		Ancestry:  []string{f.owner.ID},
	}
	require.NoError(t, f.store.CreateAgent(ctx, target))

	_, _, err = f.srv.secretBackend.Set(ctx, &secret.SetSecretInput{
		Name:       "GITHUB_TOKEN",
		Value:      "ghp_creator_transient_token",
		SecretType: secret.TypeEnvironment,
		Scope:      secret.ScopeUser,
		ScopeID:    f.owner.ID,
	})
	require.NoError(t, err)

	setTriggerExperimentsForG1bTest(t, f.srv, true, true)

	// Authoring succeeds on cold cache with zero GitHub calls.
	createPath := "/api/v1/projects/" + creatorProj.ID + "/triggers"
	createReq := authoredRequest(t, f.ownerAuth, http.MethodPost, createPath, CreateTriggerRequest{
		Name:   "on-pr-comment-transient",
		Source: TriggerSourceSpec{Kind: store.TriggerSourceKindGitHub, ID: ""},
		Match:  &TriggerMatchSpec{Types: []string{"github.issue_comment.created"}},
		Action: TriggerActionSpec{Type: store.TriggerActionMessage, AgentID: target.ID, Message: "Recovered delivery"},
	})
	createRec := httptest.NewRecorder()
	f.srv.handleTriggers(createRec, createReq, creatorProj.ID, "")
	require.Equal(t, http.StatusCreated, createRec.Code, createRec.Body.String())

	// 1. Deliver signed event while GitHub returns 500 -> drainer sets GitHubAppStatus
	// association_check_failed, leaves event pending, and records NO refused fire.
	ingRec := postSignedGitHubDelivery(t, f.srv, webhookSecret, appID, "issue_comment", uuid.NewString(), fixtureBytes)
	require.Equal(t, http.StatusAccepted, ingRec.Code, ingRec.Body.String())
	var ingResp GitHubWebhookAcceptedResponse
	require.NoError(t, json.Unmarshal(ingRec.Body.Bytes(), &ingResp))
	require.Len(t, ingResp.Events, 1)
	eventID := ingResp.Events[0].EventID

	require.Eventually(t, func() bool {
		p, gErr := f.store.GetProject(ctx, creatorProj.ID)
		return gErr == nil && p != nil && p.GitHubAppStatus != nil &&
			p.GitHubAppStatus.ErrorCode == githubAppErrAssociationCheckFailed
	}, 5*time.Second, 20*time.Millisecond)

	pendingEv, err := f.store.GetTriggerEvent(ctx, eventID)
	require.NoError(t, err)
	assert.Equal(t, store.TriggerEventStatusPending, pendingEv.Status, "transient association failure must leave event pending for retry")

	firesWhileDown, err := f.store.ListTriggerFires(ctx, store.TriggerFireFilter{ProjectID: creatorProj.ID}, store.ListOptions{})
	require.NoError(t, err)
	assert.Empty(t, firesWhileDown.Items, "transient association failure must not record a refused fire")
	assert.Empty(t, disp.getCalls())

	// 2. Recover GitHub 500 -> 200 with pull access; durable queue backoff retries and fires!
	gh.grant("ghp_creator_transient_token", "octo-org/octo-repo", githubapp.RepoAccess{Pull: true})
	gh.setRepoStatus(0)
	if q := f.srv.TriggerEventQueue(); q != nil {
		q.Wake(trigger.EventQueueKind)
	}

	require.Eventually(t, func() bool {
		if q := f.srv.TriggerEventQueue(); q != nil {
			q.Wake(trigger.EventQueueKind)
		}
		stored, gErr := f.store.GetTriggerEvent(ctx, eventID)
		return gErr == nil && stored != nil && stored.Status == store.TriggerEventStatusProcessed
	}, 8*time.Second, 50*time.Millisecond)

	calls := disp.getCalls()
	require.Len(t, calls, 1)
	assert.Equal(t, target.ID, calls[0].Agent.ID)
	assert.Equal(t, "Recovered delivery", calls[0].Message)

	firesAfterRecovery, err := f.store.ListTriggerFires(ctx, store.TriggerFireFilter{ProjectID: creatorProj.ID}, store.ListOptions{})
	require.NoError(t, err)
	require.Len(t, firesAfterRecovery.Items, 1)
	assert.Equal(t, store.TriggerFireStatusFired, firesAfterRecovery.Items[0].Status)

	// Successful drainer association check resets association_check_failed -> ok.
	recoveredProj, err := f.store.GetProject(ctx, creatorProj.ID)
	require.NoError(t, err)
	require.NotNil(t, recoveredProj.GitHubAppStatus)
	assert.Equal(t, store.GitHubAppStateOK, recoveredProj.GitHubAppStatus.State)
	assert.Empty(t, recoveredProj.GitHubAppStatus.ErrorCode)

	// 3. Stale association_unauthorized status reset: if GitHubAppStatus carries a
	// stale association_unauthorized (which blocks cold-cache authoring with 409),
	// a successful drainer association check resets status to ok so subsequent
	// cold-cache authoring succeeds with 201 and zero GitHub HTTP calls.
	f.srv.updateProjectGitHubAppStatus(ctx, recoveredProj, store.GitHubAppStateError, githubAppErrAssociationUnauthorized, "stale denial")
	f.srv.invalidateGitHubAssociation(creatorProj.ID)

	blockedRec := httptest.NewRecorder()
	f.srv.handleTriggers(blockedRec, authoredRequest(t, f.ownerAuth, http.MethodPost, createPath, CreateTriggerRequest{
		Name:   "blocked-by-stale-status",
		Source: TriggerSourceSpec{Kind: store.TriggerSourceKindGitHub, ID: ""},
		Match:  &TriggerMatchSpec{Types: []string{"github.issue_comment.created"}},
		Action: TriggerActionSpec{Type: store.TriggerActionMessage, AgentID: target.ID, Message: "blocked"},
	}), creatorProj.ID, "")
	require.Equal(t, http.StatusConflict, blockedRec.Code, blockedRec.Body.String())
	assert.Equal(t, ErrCodeGitHubNotAssociated, decodeAPIErrorCode(t, blockedRec))

	secondFixtureBytes, err := os.ReadFile(filepath.Join("trigger", "github", "testdata", "issue_comment__created_issue.payload.json"))
	require.NoError(t, err)
	ingRec2 := postSignedGitHubDelivery(t, f.srv, webhookSecret, appID, "issue_comment", uuid.NewString(), secondFixtureBytes)
	require.Equal(t, http.StatusAccepted, ingRec2.Code, ingRec2.Body.String())
	var ingResp2 GitHubWebhookAcceptedResponse
	require.NoError(t, json.Unmarshal(ingRec2.Body.Bytes(), &ingResp2))
	require.Len(t, ingResp2.Events, 1)
	eventID2 := ingResp2.Events[0].EventID

	require.Eventually(t, func() bool {
		stored, gErr := f.store.GetTriggerEvent(ctx, eventID2)
		return gErr == nil && stored != nil && stored.Status == store.TriggerEventStatusProcessed
	}, 5*time.Second, 20*time.Millisecond)

	clearedProj, err := f.store.GetProject(ctx, creatorProj.ID)
	require.NoError(t, err)
	require.NotNil(t, clearedProj.GitHubAppStatus)
	assert.Equal(t, store.GitHubAppStateOK, clearedProj.GitHubAppStatus.State)
	assert.Empty(t, clearedProj.GitHubAppStatus.ErrorCode)

	// Invalidate in-memory caches so authoring hits the cold-cache creator-proof path,
	// and verify the cleared GitHubAppStatus now permits authoring with zero GitHub calls.
	f.srv.invalidateGitHubAssociation(creatorProj.ID)
	callsBefore, _ := gh.counts()

	unblockedRec := httptest.NewRecorder()
	f.srv.handleTriggers(unblockedRec, authoredRequest(t, f.ownerAuth, http.MethodPost, createPath, CreateTriggerRequest{
		Name:   "unblocked-after-drainer-ok",
		Source: TriggerSourceSpec{Kind: store.TriggerSourceKindGitHub, ID: ""},
		Match:  &TriggerMatchSpec{Types: []string{"github.issue_comment.created"}},
		Action: TriggerActionSpec{Type: store.TriggerActionMessage, AgentID: target.ID, Message: "unblocked"},
	}), creatorProj.ID, "")
	require.Equal(t, http.StatusCreated, unblockedRec.Code, unblockedRec.Body.String())

	callsAfter, _ := gh.counts()
	assert.Equal(t, callsBefore, callsAfter, "cold-cache authoring after status reset must make zero GitHub HTTP calls")
}
