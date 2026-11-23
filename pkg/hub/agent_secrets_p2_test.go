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
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/experiments"
	"github.com/GoogleCloudPlatform/scion/pkg/secret"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// failingSetSecretBackend wraps a SecretBackend and fails Set on the Nth call
// (1-indexed) when failOnSet > 0, or injects post-Set / Delete hooks.
type failingSetSecretBackend struct {
	secret.SecretBackend
	mu              sync.Mutex
	setCalls        int
	failOnSet       int
	failOnSetErr    error
	afterSetHook    func(ctx context.Context, input *secret.SetSecretInput)
	deleteFailCount map[string]int
	deleteCalls     map[string]int
	extNamePrefix   string
}

func (b *failingSetSecretBackend) Set(ctx context.Context, input *secret.SetSecretInput) (bool, *secret.SecretMeta, error) {
	b.mu.Lock()
	b.setCalls++
	callNum := b.setCalls
	failOn := b.failOnSet
	failErr := b.failOnSetErr
	hook := b.afterSetHook
	b.mu.Unlock()

	if failOn > 0 && callNum == failOn {
		if failErr == nil {
			failErr = errors.New("simulated secret backend failure")
		}
		return false, nil, failErr
	}
	created, meta, err := b.SecretBackend.Set(ctx, input)
	if err == nil && hook != nil {
		hook(ctx, input)
	}
	return created, meta, err
}

func (b *failingSetSecretBackend) Delete(ctx context.Context, name, scope, scopeID string) error {
	b.mu.Lock()
	if b.deleteCalls == nil {
		b.deleteCalls = make(map[string]int)
	}
	b.deleteCalls[name]++
	if b.deleteFailCount != nil && b.deleteFailCount[name] > 0 {
		b.deleteFailCount[name]--
		b.mu.Unlock()
		return errors.New("simulated gcp delete failure")
	}
	b.mu.Unlock()
	return b.SecretBackend.Delete(ctx, name, scope, scopeID)
}

func (b *failingSetSecretBackend) ExternalSecretName(name, scope, scopeID string) string {
	prefix := b.extNamePrefix
	if prefix == "" {
		prefix = "scion-hub-"
	}
	return fmt.Sprintf("%s%s-%s-%s", prefix, scope, scopeID, name)
}

// recordingSlogHandler captures slog records for canary-leak and error-log assertions.
type recordingSlogHandler struct {
	mu      sync.Mutex
	records []slog.Record
	buf     bytes.Buffer
	inner   slog.Handler
}

func newRecordingSlogHandler() *recordingSlogHandler {
	h := &recordingSlogHandler{}
	h.inner = slog.NewTextHandler(&h.buf, &slog.HandlerOptions{Level: slog.LevelDebug})
	return h
}

func (h *recordingSlogHandler) Enabled(_ context.Context, _ slog.Level) bool { return true }

func (h *recordingSlogHandler) Handle(ctx context.Context, r slog.Record) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.records = append(h.records, r.Clone())
	return h.inner.Handle(ctx, r)
}

func (h *recordingSlogHandler) WithAttrs(_ []slog.Attr) slog.Handler { return h }

func (h *recordingSlogHandler) WithGroup(_ string) slog.Handler { return h }

func (h *recordingSlogHandler) String() string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.buf.String()
}

func (h *recordingSlogHandler) HasLevelWithSubstring(level slog.Level, substr string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, r := range h.records {
		if r.Level == level {
			var b bytes.Buffer
			th := slog.NewTextHandler(&b, &slog.HandlerOptions{Level: slog.LevelDebug})
			_ = th.Handle(context.Background(), r)
			if strings.Contains(b.String(), substr) {
				return true
			}
		}
	}
	return false
}

// p2StartRecordingDispatcher implements AgentDispatcher minimally to record
// DispatchAgentStart calls during existing-agent start tests.
type p2StartRecordingDispatcher struct {
	AgentDispatcher
	mu      sync.Mutex
	started bool
}

func (d *p2StartRecordingDispatcher) DispatchAgentStart(_ context.Context, _ *store.Agent, _ string, _ bool) error {
	d.mu.Lock()
	d.started = true
	d.mu.Unlock()
	return nil
}

func (d *p2StartRecordingDispatcher) wasStarted() bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.started
}

func (d *p2StartRecordingDispatcher) reset() {
	d.mu.Lock()
	d.started = false
	d.mu.Unlock()
}

func attachOnlineBrokerForP2Test(t *testing.T, srv *Server, s store.Store, project *store.Project) *store.RuntimeBroker {
	t.Helper()
	ctx := context.Background()
	broker := &store.RuntimeBroker{
		ID:     api.NewUUID(),
		Name:   "p2-test-broker",
		Slug:   "p2-test-broker",
		Status: store.BrokerStatusOnline,
	}
	require.NoError(t, s.CreateRuntimeBroker(ctx, broker))
	require.NoError(t, s.AddProjectProvider(ctx, &store.ProjectProvider{
		ProjectID:  project.ID,
		BrokerID:   broker.ID,
		BrokerName: broker.Name,
		LocalPath:  "/tmp/p2-proj",
		Status:     store.BrokerStatusOnline,
	}))
	project.DefaultRuntimeBrokerID = broker.ID
	require.NoError(t, s.UpdateProject(ctx, project))
	srv.SetEmbeddedBrokerID(broker.ID)
	return broker
}

func TestAgentSecretsP2_CreateAgent_BothRoutesAndDelivery(t *testing.T) {
	srv, s, owner, _, project, agentX, _ := setupAgentSecretsTestFixture(t, true)
	attachOnlineBrokerForP2Test(t, srv, s, project)
	ctx := context.Background()

	// Route 1: POST /api/v1/agents (createAgent -> createAgentInProject)
	fileContent := "-----BEGIN KEY-----\nhello-file-secret\n-----END KEY-----\n"
	createReq1 := CreateAgentRequest{
		Name:          "p2-create-route1",
		ProjectID:     project.ID,
		ProvisionOnly: true,
		Secrets: []AgentSecretInput{
			{
				Key:           "ENV_SECRET",
				Value:         "env-val-1",
				Encoding:      "raw",
				InjectionMode: store.InjectionModeAlways,
				Description:   "Always-injected env secret",
			},
			{
				Key:      "FILE_SECRET",
				Value:    base64.StdEncoding.EncodeToString([]byte(fileContent)),
				Encoding: "base64",
				Type:     store.SecretTypeFile,
				Target:   "~/.ssh/deploy_key",
			},
			{
				Key:    "VAR_SECRET",
				Value:  `{"a":1}`,
				Type:   store.SecretTypeVariable,
				Target: "var_secret",
			},
		},
	}

	rec1 := doRequestAsUser(t, srv, owner, http.MethodPost, "/api/v1/agents", createReq1)
	require.Equal(t, http.StatusCreated, rec1.Code, "body=%s", rec1.Body.String())

	var resp1 CreateAgentResponse
	require.NoError(t, json.Unmarshal(rec1.Body.Bytes(), &resp1))
	require.NotNil(t, resp1.Agent)

	// Verify secrets stored in backend at agent scope with owner attribution.
	gotEnv, err := srv.GetSecretBackend().Get(ctx, "ENV_SECRET", secret.ScopeAgent, resp1.Agent.ID)
	require.NoError(t, err)
	assert.Equal(t, "env-val-1", gotEnv.Value)
	assert.Equal(t, owner.ID, gotEnv.CreatedBy)
	assert.Equal(t, store.InjectionModeAlways, gotEnv.InjectionMode)

	gotFile, err := srv.GetSecretBackend().Get(ctx, "FILE_SECRET", secret.ScopeAgent, resp1.Agent.ID)
	require.NoError(t, err)
	assert.Equal(t, fileContent, gotFile.Value)
	assert.Equal(t, store.SecretTypeFile, gotFile.SecretType)
	assert.Equal(t, "~/.ssh/deploy_key", gotFile.Target)

	// Verify start-time delivery picks up the created agent's secrets.
	storedAgent1, err := s.GetAgent(ctx, resp1.Agent.ID)
	require.NoError(t, err)
	d := NewHTTPAgentDispatcher(s, false, nil)
	d.SetSecretBackend(srv.GetSecretBackend())
	d.SetAuthzService(srv.authzService)
	d.SetAgentSecretsEnabledProvider(func() bool { return srv.experimentEnabled(experiments.AgentSecrets) })
	sel := newDeliverySelector(srv.authzService, storedAgent1, PurposeRestart)
	delivered, _, _, err := d.selectDeliverySecretsWith(ctx, sel, storedAgent1, nil)
	require.NoError(t, err)
	deliveredMap := make(map[string]string, len(delivered))
	for _, item := range delivered {
		deliveredMap[item.Name] = item.Value
	}
	assert.Equal(t, "env-val-1", deliveredMap["ENV_SECRET"])
	assert.Equal(t, fileContent, deliveredMap["FILE_SECRET"])
	assert.Equal(t, `{"a":1}`, deliveredMap["VAR_SECRET"])

	// Route 2: POST /api/v1/projects/{id}/agents (createProjectAgent -> createAgentInProject)
	createReq2 := CreateAgentRequest{
		Name:          "p2-create-route2",
		ProvisionOnly: true,
		Secrets: []AgentSecretInput{
			{Key: "PROJ_ROUTE_SECRET", Value: "route2-val"},
		},
	}
	rec2 := doRequestAsUser(t, srv, owner, http.MethodPost, "/api/v1/projects/"+project.ID+"/agents", createReq2)
	require.Equal(t, http.StatusCreated, rec2.Code, "body=%s", rec2.Body.String())

	var resp2 CreateAgentResponse
	require.NoError(t, json.Unmarshal(rec2.Body.Bytes(), &resp2))
	require.NotNil(t, resp2.Agent)

	got2, err := srv.GetSecretBackend().Get(ctx, "PROJ_ROUTE_SECRET", secret.ScopeAgent, resp2.Agent.ID)
	require.NoError(t, err)
	assert.Equal(t, "route2-val", got2.Value)

	// Agent creator: an agent with create authorization may pass Secrets to a sub-agent.
	agentX.AppliedConfig = &store.AgentAppliedConfig{AgentRole: string(AgentRoleFull)}
	require.NoError(t, s.UpdateAgent(ctx, agentX))
	require.NoError(t, s.CreateDelegationEdge(ctx, &store.DelegationEdge{
		DelegatorType:       store.DelegationPrincipalUser,
		DelegatorID:         owner.ID,
		DelegateType:        store.DelegationPrincipalAgent,
		DelegateID:          agentX.ID,
		ScopeType:           store.RoleScopeProject,
		ScopeID:             project.ID,
		Role:                string(AgentRoleFull),
		Active:              true,
		EffectCeiling:       store.EffectCeiling{Kind: store.EffectCeilingPrincipal},
		AuthorityProvenance: store.AuthorityProvenance{ProvenanceVersion: 1},
	}))

	subReq := CreateAgentRequest{
		Name:          "p2-sub-agent",
		ProjectID:     project.ID,
		ProvisionOnly: true,
		AgentRole:     string(AgentRoleNone),
		Secrets: []AgentSecretInput{
			{Key: "SUB_SECRET", Value: "handed-off-val"},
		},
	}
	rec3 := doAsAgentWithScopes(t, srv, agentX, []AgentTokenScope{ScopeAgentCreate}, http.MethodPost, "/api/v1/projects/"+project.ID+"/agents", subReq)
	require.Equal(t, http.StatusCreated, rec3.Code, "body=%s", rec3.Body.String())

	var resp3 CreateAgentResponse
	require.NoError(t, json.Unmarshal(rec3.Body.Bytes(), &resp3))
	require.NotNil(t, resp3.Agent)

	got3, err := srv.GetSecretBackend().Get(ctx, "SUB_SECRET", secret.ScopeAgent, resp3.Agent.ID)
	require.NoError(t, err)
	assert.Equal(t, "handed-off-val", got3.Value)
	assert.Equal(t, "agent:"+agentX.ID, got3.CreatedBy)
}

func TestAgentSecretsP2_CreateAgent_ExperimentOffAndValidation(t *testing.T) {
	t.Run("experiment off refuses Secrets with clear error", func(t *testing.T) {
		srv, s, owner, _, project, _, _ := setupAgentSecretsTestFixture(t, false)
		ctx := context.Background()

		rec := doRequestAsUser(t, srv, owner, http.MethodPost, "/api/v1/agents", CreateAgentRequest{
			Name:          "exp-off-agent",
			ProjectID:     project.ID,
			ProvisionOnly: true,
			Secrets:       []AgentSecretInput{{Key: "FOO", Value: "bar"}},
		})

		require.Equal(t, http.StatusBadRequest, rec.Code, "body=%s", rec.Body.String())
		assert.Contains(t, rec.Body.String(), "experiment not enabled")

		_, err := s.GetAgentBySlug(ctx, project.ID, "exp-off-agent")
		assert.ErrorIs(t, err, store.ErrNotFound)
	})

	t.Run("validation failures return 400 and write nothing", func(t *testing.T) {
		srv, s, owner, _, project, _, _ := setupAgentSecretsTestFixture(t, true)
		ctx := context.Background()

		tooMany := make([]AgentSecretInput, 51)
		for i := range tooMany {
			tooMany[i] = AgentSecretInput{Key: fmt.Sprintf("K_%d", i), Value: "v"}
		}

		cases := []struct {
			name    string
			secrets []AgentSecretInput
			wantMsg string
		}{
			{"empty key", []AgentSecretInput{{Key: "", Value: "v"}}, "secret key is required"},
			{"key with space", []AgentSecretInput{{Key: "BAD KEY", Value: "v"}}, "cannot contain whitespace"},
			{"duplicate key", []AgentSecretInput{{Key: "DUP", Value: "1"}, {Key: "DUP", Value: "2"}}, "duplicate secret key"},
			{"over 50 secrets", tooMany, "at most 50 secrets"},
			{"reserved SCION_ env target", []AgentSecretInput{{Key: "SCION_TOKEN", Value: "v"}}, "reserved"},
			{"reserved GCE_METADATA_ env target", []AgentSecretInput{{Key: "GCE_METADATA_HOST", Value: "v"}}, "reserved"},
			{"reserved systemone prefix", []AgentSecretInput{{Key: "systemone.instance.k", Value: "v"}}, "reserved"},
			{"reserved webhook prefix", []AgentSecretInput{{Key: "webhook_endpoint.k", Value: "v"}}, "reserved"},
			{"empty value", []AgentSecretInput{{Key: "EMPTY_V", Value: ""}}, "secret value is required"},
			{"bad encoding", []AgentSecretInput{{Key: "ENC", Value: "v", Encoding: "hex"}}, "encoding must be"},
			{"bad base64", []AgentSecretInput{{Key: "B64", Value: "!!!not-base64!!!", Encoding: "base64"}}, "valid base64"},
			{"value > 64KB", []AgentSecretInput{{Key: "BIG", Value: strings.Repeat("x", 64*1024+1)}}, "exceeds 64KB limit"},
			{"bad type", []AgentSecretInput{{Key: "T", Value: "v", Type: "bogus"}}, "type must be one of"},
			{"file target relative", []AgentSecretInput{{Key: "F", Value: "v", Type: "file", Target: "rel/path"}}, "absolute path"},
			{"file target traversal", []AgentSecretInput{{Key: "F", Value: "v", Type: "file", Target: "/tmp/../etc/passwd"}}, ".."},
			{"bad injectionMode", []AgentSecretInput{{Key: "M", Value: "v", InjectionMode: "never"}}, "injectionMode must be"},
		}

		for i, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				slug := fmt.Sprintf("val-fail-%d", i)
				rec := doRequestAsUser(t, srv, owner, http.MethodPost, "/api/v1/projects/"+project.ID+"/agents", CreateAgentRequest{
					Name:          slug,
					ProjectID:     project.ID,
					ProvisionOnly: true,
					Secrets:       tc.secrets,
				})

				require.Equal(t, http.StatusBadRequest, rec.Code, "case=%s body=%s", tc.name, rec.Body.String())
				assert.Contains(t, rec.Body.String(), tc.wantMsg)

				_, err := s.GetAgentBySlug(ctx, project.ID, slug)
				assert.ErrorIs(t, err, store.ErrNotFound, "agent row must not exist after validation error")
			})
		}
	})
}

func TestAgentSecretsP2_CreateAgent_BackendFailureCompensates(t *testing.T) {
	srv, s, owner, _, project, _, _ := setupAgentSecretsTestFixture(t, true)
	attachOnlineBrokerForP2Test(t, srv, s, project)
	ctx := context.Background()

	// Fail on the 2nd secret write so the 1st secret was written and must be rolled back.
	wrapped := &failingSetSecretBackend{
		SecretBackend: srv.GetSecretBackend(),
		failOnSet:     2,
	}
	srv.SetSecretBackend(wrapped)

	rec := doRequestAsUser(t, srv, owner, http.MethodPost, "/api/v1/projects/"+project.ID+"/agents", CreateAgentRequest{
		Name:          "fail-closed-agent",
		ProjectID:     project.ID,
		ProvisionOnly: true,
		Secrets: []AgentSecretInput{
			{Key: "FIRST_SECRET", Value: "val-1"},
			{Key: "SECOND_SECRET", Value: "val-2"},
		},
	})

	require.Equal(t, http.StatusServiceUnavailable, rec.Code, "body=%s", rec.Body.String())
	assert.Contains(t, rec.Body.String(), createAgentSecretsStoreFailedMessage)

	// Agent row must be hard-deleted.
	_, err := s.GetAgentBySlug(ctx, project.ID, "fail-closed-agent")
	assert.ErrorIs(t, err, store.ErrNotFound)

	// No secrets may remain from the failed create.
	allRows, err := s.ListSecrets(ctx, store.SecretFilter{Scope: store.ScopeAgent})
	require.NoError(t, err)
	for _, r := range allRows {
		assert.NotEqual(t, "FIRST_SECRET", r.Key, "partially written secret must be cleaned up")
		assert.NotEqual(t, "SECOND_SECRET", r.Key)
	}
}

func TestAgentSecretsP2_CreateAgent_CanaryNoLeak(t *testing.T) {
	srv, s, owner, _, project, _, _ := setupAgentSecretsTestFixture(t, true)
	attachOnlineBrokerForP2Test(t, srv, s, project)
	ctx := context.Background()

	canary := "CANARY_SECRET_VAL_7e4c2a9b1f0d"
	logCapture := newRecordingSlogHandler()
	origLogger := slog.Default()
	slog.SetDefault(slog.New(logCapture))
	t.Cleanup(func() { slog.SetDefault(origLogger) })

	createReq := CreateAgentRequest{
		Name:          "canary-agent",
		ProjectID:     project.ID,
		ProvisionOnly: true,
		Secrets: []AgentSecretInput{
			{Key: "CANARY_KEY", Value: canary, Description: "canary test secret"},
		},
	}

	// Explicitly exercise LogValue() redaction on both structs.
	slog.Info("test log create request", "req", createReq, "secret_input", createReq.Secrets[0])

	rec := doRequestAsUser(t, srv, owner, http.MethodPost, "/api/v1/projects/"+project.ID+"/agents", createReq)
	require.Equal(t, http.StatusCreated, rec.Code, "body=%s", rec.Body.String())

	var resp CreateAgentResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	require.NotNil(t, resp.Agent)

	// 1. Canary value IS stored in the secret backend.
	got, err := srv.GetSecretBackend().Get(ctx, "CANARY_KEY", secret.ScopeAgent, resp.Agent.ID)
	require.NoError(t, err)
	assert.Equal(t, canary, got.Value)

	// 2. Canary value is NOT anywhere in the stored agent row (applied_config, inline_config, create_inputs).
	storedAgent, err := s.GetAgent(ctx, resp.Agent.ID)
	require.NoError(t, err)
	agentJSON, err := json.Marshal(storedAgent)
	require.NoError(t, err)
	assert.NotContains(t, string(agentJSON), canary, "canary leaked into stored agent row")
	assert.NotContains(t, rec.Body.String(), canary, "canary leaked into create HTTP response")

	// 3. Canary value is NOT in any mutation audit record, while the key name IS recorded.
	audits, _, err := s.ListMutationAudits(ctx, store.MutationAuditFilter{TargetID: resp.Agent.ID})
	require.NoError(t, err)
	require.NotEmpty(t, audits)
	foundKeyInAudit := false
	for _, a := range audits {
		auditJSON, err := json.Marshal(a)
		require.NoError(t, err)
		assert.NotContains(t, string(auditJSON), canary, "canary leaked into mutation audit record")
		if strings.Contains(a.AfterSummary, "CANARY_KEY") {
			foundKeyInAudit = true
		}
	}
	assert.True(t, foundKeyInAudit, "expected create audit AfterSummary to record secret key name")

	// 4. Canary value is NOT in captured hub logs.
	assert.NotContains(t, logCapture.String(), canary, "canary leaked into hub logs")
	assert.Contains(t, logCapture.String(), "[REDACTED]")
}

func TestAgentSecretsP2_CreateAgent_Headless(t *testing.T) {
	srv, s, owner, _, project, _, _ := setupAgentSecretsTestFixture(t, true)
	attachOnlineBrokerForP2Test(t, srv, s, project)
	ctx := context.Background()

	newHC := func(name string, secrets []AgentSecretInput) HeadlessCreate {
		rr := headlessResolvedRole{preCapRole: AgentRoleFull}
		return HeadlessCreate{
			ProjectID: project.ID,
			Authority: FireAuthority{
				PrincipalKind: store.DelegationPrincipalUser,
				PrincipalID:   owner.ID,
			},
			Identity:    NewAuthenticatedUser(owner.ID, owner.Email, owner.DisplayName, owner.Role, "test"),
			CreatorName: owner.Email,
			EdgeCeiling: store.EffectCeiling{Kind: store.EffectCeilingPrincipal},
			EdgeProvenance: store.AuthorityProvenance{
				ProvenanceVersion:           1,
				SourcePrincipalKind:         store.DelegationPrincipalUser,
				SourcePrincipalID:           owner.ID,
				SourceCredentialKind:        store.SourceCredentialScheduler,
				SourceEventID:               api.NewUUID(),
				SourceScheduleID:            api.NewUUID(),
				SourceAuthorizationRevision: 1,
			},
			resolvedRole: &rr,
			Name:         name,
			Secrets:      secrets,
			Detached:     true,
		}
	}

	// Happy path (no dispatcher configured -> commits, stores secrets, returns agent).
	agent, err := srv.createAgentHeadless(ctx, newHC("headless-sec-ok", []AgentSecretInput{
		{Key: "HEADLESS_KEY", Value: "headless-secret-val"},
	}))
	require.NoError(t, err)
	require.NotNil(t, agent)

	got, err := srv.GetSecretBackend().Get(ctx, "HEADLESS_KEY", secret.ScopeAgent, agent.ID)
	require.NoError(t, err)
	assert.Equal(t, "headless-secret-val", got.Value)
	assert.Equal(t, owner.ID, got.CreatedBy)

	// Backend failure rolls back written secrets and hard-deletes the agent.
	srv.SetSecretBackend(&failingSetSecretBackend{
		SecretBackend: srv.GetSecretBackend(),
		failOnSet:     2,
	})
	_, err = srv.createAgentHeadless(ctx, newHC("headless-sec-fail", []AgentSecretInput{
		{Key: "HEADLESS_FIRST", Value: "v1"},
		{Key: "HEADLESS_SECOND", Value: "v2"},
	}))
	require.Error(t, err)
	assert.Contains(t, err.Error(), createAgentSecretsStoreFailedMessage)

	_, err = s.GetAgentBySlug(ctx, project.ID, "headless-sec-fail")
	assert.ErrorIs(t, err, store.ErrNotFound)

	allRows, err := s.ListSecrets(ctx, store.SecretFilter{Scope: store.ScopeAgent})
	require.NoError(t, err)
	for _, r := range allRows {
		assert.NotEqual(t, "HEADLESS_FIRST", r.Key)
	}
}

func TestAgentSecretsP2_StartExistingAgent_UpsertSecrets(t *testing.T) {
	srv, s, owner, viewer, project, _, _ := setupAgentSecretsTestFixture(t, true)
	ctx := context.Background()

	// Create a stopped existing agent with a mock dispatcher so start succeeds.
	broker := &store.RuntimeBroker{
		ID:     api.NewUUID(),
		Name:   "test-broker",
		Slug:   "test-broker",
		Status: store.BrokerStatusOnline,
	}
	require.NoError(t, s.CreateRuntimeBroker(ctx, broker))
	require.NoError(t, s.AddProjectProvider(ctx, &store.ProjectProvider{
		ProjectID:  project.ID,
		BrokerID:   broker.ID,
		BrokerName: broker.Name,
		Status:     store.BrokerStatusOnline,
	}))
	project.DefaultRuntimeBrokerID = broker.ID
	require.NoError(t, s.UpdateProject(ctx, project))

	existing := &store.Agent{
		ID:              api.NewUUID(),
		Slug:            "existing-start-agent",
		Name:            "existing-start-agent",
		ProjectID:       project.ID,
		RuntimeBrokerID: broker.ID,
		OwnerID:         owner.ID,
		CreatedBy:       owner.ID,
		Ancestry:        []string{owner.ID},
		Phase:           string(state.PhaseStopped),
		AppliedConfig:   &store.AgentAppliedConfig{AgentRole: string(AgentRoleFull)},
	}
	require.NoError(t, s.CreateAgent(ctx, existing))

	disp := &p2StartRecordingDispatcher{}
	srv.SetDispatcher(disp)

	// 1. Owner (has agent.update) upserts secrets on existing agent and starts it.
	rec := doRequestAsUser(t, srv, owner, http.MethodPost, "/api/v1/projects/"+project.ID+"/agents", CreateAgentRequest{
		Name:    existing.Slug,
		Resume:  true,
		Secrets: []AgentSecretInput{{Key: "UPSERT_KEY", Value: "upserted-v1"}},
	})
	require.Equal(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())
	assert.True(t, disp.wasStarted())

	got, err := srv.GetSecretBackend().Get(ctx, "UPSERT_KEY", secret.ScopeAgent, existing.ID)
	require.NoError(t, err)
	assert.Equal(t, "upserted-v1", got.Value)

	// 2. Backend failure during existing-agent upsert returns 503 and does NOT start or delete the agent.
	existing, err = s.GetAgent(ctx, existing.ID)
	require.NoError(t, err)
	existing.Phase = string(state.PhaseStopped)
	require.NoError(t, s.UpdateAgent(ctx, existing))
	disp.reset()
	origBackend := srv.GetSecretBackend()
	srv.SetSecretBackend(&failingSetSecretBackend{SecretBackend: origBackend, failOnSet: 1})

	rec2 := doRequestAsUser(t, srv, owner, http.MethodPost, "/api/v1/projects/"+project.ID+"/agents", CreateAgentRequest{
		Name:    existing.Slug,
		Resume:  true,
		Secrets: []AgentSecretInput{{Key: "FAIL_UPSERT", Value: "v2"}},
	})
	require.Equal(t, http.StatusServiceUnavailable, rec2.Code, "body=%s", rec2.Body.String())
	assert.False(t, disp.wasStarted(), "agent must not be started when secret upsert fails")
	_, err = s.GetAgent(ctx, existing.ID)
	require.NoError(t, err, "existing agent must not be deleted on upsert failure")

	// 3. Viewer (lacks agent.update) cannot upsert secrets onto existing agent.
	srv.SetSecretBackend(origBackend)
	rec3 := doRequestAsUser(t, srv, viewer, http.MethodPost, "/api/v1/projects/"+project.ID+"/agents", CreateAgentRequest{
		Name:    existing.Slug,
		Resume:  true,
		Secrets: []AgentSecretInput{{Key: "VIEWER_TRY", Value: "v3"}},
	})
	assert.Equal(t, http.StatusForbidden, rec3.Code)
}

func TestAgentSecretsP2_L1_SoftDeleteRaceAndRetryCleanup(t *testing.T) {
	t.Run("refuses writes on soft-deleted agent", func(t *testing.T) {
		srv, s, owner, _, _, agentX, _ := setupAgentSecretsTestFixture(t, true)
		ctx := context.Background()

		// Seed a secret before soft-delete to test DELETE too.
		_, _, err := srv.GetSecretBackend().Set(ctx, &secret.SetSecretInput{
			Name:       "EXISTING",
			Value:      "v1",
			SecretType: store.SecretTypeEnvironment,
			Target:     "EXISTING",
			Scope:      secret.ScopeAgent,
			ScopeID:    agentX.ID,
			ProjectID:  agentX.ProjectID,
		})
		require.NoError(t, err)

		// Soft-delete agentX.
		agentX.DeletedAt = time.Now().UTC()
		require.NoError(t, s.UpdateAgent(ctx, agentX))

		// PUT returns 404.
		rec := doRequestAsUser(t, srv, owner, http.MethodPut, "/api/v1/secrets/LATE", SetSecretRequest{
			Value:    "late",
			Encoding: "raw",
			Scope:    store.ScopeAgent,
			ScopeID:  agentX.ID,
		})
		assert.Equal(t, http.StatusNotFound, rec.Code)

		_, err = srv.GetSecretBackend().Get(ctx, "LATE", secret.ScopeAgent, agentX.ID)
		assert.ErrorIs(t, err, store.ErrNotFound)

		// DELETE returns 404.
		delRec := doRequestAsUser(t, srv, owner, http.MethodDelete, "/api/v1/secrets/EXISTING?scope=agent&scopeId="+agentX.ID, nil)
		assert.Equal(t, http.StatusNotFound, delRec.Code)
	})

	t.Run("compensates write racing hard-delete", func(t *testing.T) {
		srv, s, owner, _, _, agentX, _ := setupAgentSecretsTestFixture(t, true)
		ctx := context.Background()

		origBackend := srv.GetSecretBackend()
		srv.SetSecretBackend(&failingSetSecretBackend{
			SecretBackend: origBackend,
			afterSetHook: func(hctx context.Context, _ *secret.SetSecretInput) {
				// Simulate a hard delete completing right after Set succeeds.
				_ = s.DeleteAgent(hctx, agentX.ID)
			},
		})

		rec := doRequestAsUser(t, srv, owner, http.MethodPut, "/api/v1/secrets/RACED", SetSecretRequest{
			Value:    "raced-val",
			Encoding: "raw",
			Scope:    store.ScopeAgent,
			ScopeID:  agentX.ID,
		})

		assert.Equal(t, http.StatusNotFound, rec.Code)
		_, err := origBackend.Get(ctx, "RACED", secret.ScopeAgent, agentX.ID)
		assert.ErrorIs(t, err, store.ErrNotFound, "raced secret must be compensated")
	})

	t.Run("post-commit cleanup retries transient failure and logs permanent failure at Error with secret_name", func(t *testing.T) {
		srv, _, _, _, _, agentX, _ := setupAgentSecretsTestFixture(t, true)
		ctx := context.Background()

		origBackend := srv.GetSecretBackend()
		for _, k := range []string{"TRANSIENT_KEY", "PERM_FAIL_KEY"} {
			_, _, err := origBackend.Set(ctx, &secret.SetSecretInput{
				Name:       k,
				Value:      "val",
				SecretType: store.SecretTypeEnvironment,
				Target:     k,
				Scope:      secret.ScopeAgent,
				ScopeID:    agentX.ID,
				ProjectID:  agentX.ProjectID,
			})
			require.NoError(t, err)
		}

		// TRANSIENT_KEY fails once then succeeds on retry; PERM_FAIL_KEY fails both attempts.
		fb := &failingSetSecretBackend{
			SecretBackend: origBackend,
			deleteFailCount: map[string]int{
				"TRANSIENT_KEY": 1,
				"PERM_FAIL_KEY": 5,
			},
			extNamePrefix: "gcp-test-",
		}
		srv.SetSecretBackend(fb)

		logCapture := newRecordingSlogHandler()
		origLogger := slog.Default()
		slog.SetDefault(slog.New(logCapture))
		t.Cleanup(func() { slog.SetDefault(origLogger) })

		ok := srv.removeAgentScopedSecrets(ctx, agentX.ID, []string{"TRANSIENT_KEY", "PERM_FAIL_KEY"})
		assert.False(t, ok)

		// TRANSIENT_KEY was retried (2 calls) and deleted.
		assert.Equal(t, 2, fb.deleteCalls["TRANSIENT_KEY"])
		_, err := origBackend.Get(ctx, "TRANSIENT_KEY", secret.ScopeAgent, agentX.ID)
		assert.ErrorIs(t, err, store.ErrNotFound)

		// PERM_FAIL_KEY was tried twice and logged at Error level with external secret_name.
		assert.Equal(t, 2, fb.deleteCalls["PERM_FAIL_KEY"])
		expectedExtName := fb.ExternalSecretName("PERM_FAIL_KEY", secret.ScopeAgent, agentX.ID)
		assert.True(t, logCapture.HasLevelWithSubstring(slog.LevelError, expectedExtName),
			"expected Error-level log containing secret_name %q; logs:\n%s", expectedExtName, logCapture.String())
	})

	t.Run("withAgentSecretCleanup captures in-tx keys for external backend whose List reads DB", func(t *testing.T) {
		srv, s, owner, _, _, agentX, _ := setupAgentSecretsTestFixture(t, true)
		ctx := context.Background()

		// Simulate GCPBackend: values live outside the DB, while List reads DB
		// metadata rows (which DeleteSecretsByScope removes inside the tx).
		externalValues := map[string]string{}
		var deletedKeys []string
		extBackend := &fakeExternalSecretBackend{
			SecretBackend: srv.GetSecretBackend(),
			store:         s,
			values:        externalValues,
			onDelete: func(name string) {
				deletedKeys = append(deletedKeys, name)
			},
		}
		srv.SetSecretBackend(extBackend)

		// Initially no secrets exist before withAgentSecretCleanup takes its pre-tx snapshot.
		// Inside fn, a secret lands in the window before the hard-delete tx runs
		// agentHardDeleteWork (which lists inside tx and runs DeleteSecretsByScope).
		n, err := srv.withAgentSecretCleanup(ctx, agentX.ID, func(txCtx context.Context) (int, error) {
			_, _, serr := extBackend.Set(ctx, &secret.SetSecretInput{
				Name:       "WINDOW_KEY",
				Value:      "ext-secret-val",
				SecretType: store.SecretTypeEnvironment,
				Target:     "WINDOW_KEY",
				Scope:      secret.ScopeAgent,
				ScopeID:    agentX.ID,
				ProjectID:  agentX.ProjectID,
			})
			if serr != nil {
				return 0, serr
			}
			now := time.Now()
			err := s.WithTx(txCtx, func(tx store.Store) error {
				_, _, werr := srv.agentHardDeleteWork(txCtx, tx, agentX, AuditActor{PrincipalKind: "user", PrincipalID: owner.ID}, store.Deactivation{
					Cause: store.EdgeDeactivationAgentHardDelete,
					At:    &now,
					OpID:  api.NewUUID(),
				}, "test hard delete")
				return werr
			})
			return 1, err
		})
		require.NoError(t, err)
		assert.Equal(t, 1, n)

		// DB rows are gone (so extBackend.List returns empty, just like GCPBackend.List),
		// yet external Delete WAS called for WINDOW_KEY via the in-tx collector.
		remainingDB, err := extBackend.List(ctx, secret.Filter{Scope: secret.ScopeAgent, ScopeID: agentX.ID})
		require.NoError(t, err)
		assert.Empty(t, remainingDB)
		assert.Contains(t, deletedKeys, "WINDOW_KEY")
		assert.NotContains(t, externalValues, "WINDOW_KEY")
	})

	t.Run("create-time secret write racing agent delete compensates and answers 409", func(t *testing.T) {
		srv, s, owner, _, project, _, _ := setupAgentSecretsTestFixture(t, true)
		attachOnlineBrokerForP2Test(t, srv, s, project)
		ctx := context.Background()

		origBackend := srv.GetSecretBackend()
		var createdAgentID string
		srv.SetSecretBackend(&failingSetSecretBackend{
			SecretBackend: origBackend,
			afterSetHook: func(hctx context.Context, input *secret.SetSecretInput) {
				createdAgentID = input.ScopeID
				// Simulate a concurrent delete removing the newly created agent while Set is in flight.
				_ = s.DeleteAgent(hctx, input.ScopeID)
			},
		})

		rec := doRequestAsUser(t, srv, owner, http.MethodPost, "/api/v1/projects/"+project.ID+"/agents", CreateAgentRequest{
			Name:          "create-race-agent",
			ProjectID:     project.ID,
			ProvisionOnly: true,
			Secrets:       []AgentSecretInput{{Key: "CREATE_RACED", Value: "secret-val"}},
		})
		require.Equal(t, http.StatusConflict, rec.Code, "body=%s", rec.Body.String())
		assert.Contains(t, rec.Body.String(), "delete_in_progress")
		require.NotEmpty(t, createdAgentID)

		_, err := origBackend.Get(ctx, "CREATE_RACED", secret.ScopeAgent, createdAgentID)
		assert.ErrorIs(t, err, store.ErrNotFound, "raced create-time secret must be compensated")
	})

	t.Run("existing-agent upsert racing agent delete compensates and answers 404", func(t *testing.T) {
		srv, s, owner, _, project, _, _ := setupAgentSecretsTestFixture(t, true)
		broker := attachOnlineBrokerForP2Test(t, srv, s, project)
		ctx := context.Background()

		existing := &store.Agent{
			ID:              api.NewUUID(),
			Slug:            "upsert-race-agent",
			Name:            "upsert-race-agent",
			ProjectID:       project.ID,
			RuntimeBrokerID: broker.ID,
			OwnerID:         owner.ID,
			CreatedBy:       owner.ID,
			Ancestry:        []string{owner.ID},
			Phase:           string(state.PhaseStopped),
			AppliedConfig:   &store.AgentAppliedConfig{AgentRole: string(AgentRoleFull)},
		}
		require.NoError(t, s.CreateAgent(ctx, existing))

		disp := &p2StartRecordingDispatcher{}
		srv.SetDispatcher(disp)

		origBackend := srv.GetSecretBackend()
		srv.SetSecretBackend(&failingSetSecretBackend{
			SecretBackend: origBackend,
			afterSetHook: func(hctx context.Context, input *secret.SetSecretInput) {
				_ = s.DeleteAgent(hctx, input.ScopeID)
			},
		})

		rec := doRequestAsUser(t, srv, owner, http.MethodPost, "/api/v1/projects/"+project.ID+"/agents", CreateAgentRequest{
			Name:    existing.Slug,
			Resume:  true,
			Secrets: []AgentSecretInput{{Key: "UPSERT_RACED", Value: "secret-val"}},
		})
		require.Equal(t, http.StatusNotFound, rec.Code, "body=%s", rec.Body.String())
		assert.False(t, disp.wasStarted())

		_, err := origBackend.Get(ctx, "UPSERT_RACED", secret.ScopeAgent, existing.ID)
		assert.ErrorIs(t, err, store.ErrNotFound, "raced upsert secret must be compensated")
	})

	t.Run("compensation cleanup runs even when agent row is already absent (n==0)", func(t *testing.T) {
		srv, s, _, _, _, agentX, _ := setupAgentSecretsTestFixture(t, true)
		ctx := context.Background()

		_, _, err := srv.GetSecretBackend().Set(ctx, &secret.SetSecretInput{
			Name:       "ORPHAN_COMP_KEY",
			Value:      "val",
			SecretType: store.SecretTypeEnvironment,
			Target:     "ORPHAN_COMP_KEY",
			Scope:      secret.ScopeAgent,
			ScopeID:    agentX.ID,
			ProjectID:  agentX.ProjectID,
		})
		require.NoError(t, err)

		// Delete the agent row directly so FinalizeAgentDeletion would return n == 0.
		require.NoError(t, s.DeleteAgent(ctx, agentX.ID))

		n, err := srv.withAgentSecretCleanupOnCompensation(ctx, agentX.ID, func(_ context.Context) (int, error) {
			return 0, nil
		})
		require.NoError(t, err)
		assert.Equal(t, 0, n)

		_, err = srv.GetSecretBackend().Get(ctx, "ORPHAN_COMP_KEY", secret.ScopeAgent, agentX.ID)
		assert.ErrorIs(t, err, store.ErrNotFound, "compensation cleanup must remove secrets even when n==0 and row is absent")
	})

	t.Run("start-gate in-flight branch returns 409 when secrets are supplied", func(t *testing.T) {
		srv, s, owner, _, project, _, _ := setupAgentSecretsTestFixture(t, true)
		broker := attachOnlineBrokerForP2Test(t, srv, s, project)
		ctx := context.Background()

		inflight := &store.Agent{
			ID:              api.NewUUID(),
			Slug:            "inflight-sec-agent",
			Name:            "inflight-sec-agent",
			ProjectID:       project.ID,
			RuntimeBrokerID: broker.ID,
			OwnerID:         owner.ID,
			CreatedBy:       owner.ID,
			Ancestry:        []string{owner.ID},
			Phase:           string(state.PhaseStarting),
			AppliedConfig:   &store.AgentAppliedConfig{AgentRole: string(AgentRoleFull)},
		}
		require.NoError(t, s.CreateAgent(ctx, inflight))
		_, err := s.BeginLaunch(ctx, inflight.ID, store.LaunchKindCreate, 5*time.Minute)
		require.NoError(t, err)

		rec := doRequestAsUser(t, srv, owner, http.MethodPost, "/api/v1/projects/"+project.ID+"/agents", CreateAgentRequest{
			Name:    inflight.Slug,
			Resume:  true,
			Secrets: []AgentSecretInput{{Key: "INFLIGHT_KEY", Value: "val"}},
		})
		require.Equal(t, http.StatusConflict, rec.Code, "body=%s", rec.Body.String())
		assert.Contains(t, rec.Body.String(), "already in flight")

		_, err := srv.GetSecretBackend().Get(ctx, "INFLIGHT_KEY", secret.ScopeAgent, inflight.ID)
		assert.ErrorIs(t, err, store.ErrNotFound)
	})
}

// fakeExternalSecretBackend simulates GCPBackend's split storage model: values
// live in an external map outside the DB, while List queries the DB metadata
// table (which DeleteSecretsByScope deletes inside a transaction).
type fakeExternalSecretBackend struct {
	secret.SecretBackend
	store    store.Store
	mu       sync.Mutex
	values   map[string]string
	onDelete func(name string)
}

func (b *fakeExternalSecretBackend) Set(ctx context.Context, input *secret.SetSecretInput) (bool, *secret.SecretMeta, error) {
	created, meta, err := b.SecretBackend.Set(ctx, input)
	if err != nil {
		return false, nil, err
	}
	b.mu.Lock()
	b.values[input.Name] = input.Value
	b.mu.Unlock()
	return created, meta, nil
}

func (b *fakeExternalSecretBackend) List(ctx context.Context, filter secret.Filter) ([]secret.SecretMeta, error) {
	return b.SecretBackend.List(ctx, filter)
}

func (b *fakeExternalSecretBackend) Delete(ctx context.Context, name, scope, scopeID string) error {
	b.mu.Lock()
	delete(b.values, name)
	cb := b.onDelete
	b.mu.Unlock()
	if cb != nil {
		cb(name)
	}
	_ = b.SecretBackend.Delete(ctx, name, scope, scopeID)
	return nil
}
