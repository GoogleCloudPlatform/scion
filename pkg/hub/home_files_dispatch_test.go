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
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/storage"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/transfer"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type homeFilesTokenCounter struct {
	authorize, sign int
}

func (g *homeFilesTokenCounter) AuthorizeAgentToken(context.Context, *store.Agent) (AgentTokenGrant, error) {
	g.authorize++
	return AgentTokenGrant{}, fmt.Errorf("unexpected token authorization")
}

func (g *homeFilesTokenCounter) SignAgentToken(AgentTokenGrant, string) (string, *store.AgentCredential, error) {
	g.sign++
	return "", nil, fmt.Errorf("unexpected token signature")
}

type prefixedMintingTokenGenerator struct {
	mu     sync.Mutex
	prefix string
	jtis   []string
}

func (g *prefixedMintingTokenGenerator) AuthorizeAgentToken(_ context.Context, agent *store.Agent) (AgentTokenGrant, error) {
	return AgentTokenGrant{AgentID: agent.ID, ProjectID: agent.ProjectID, Ancestry: agent.Ancestry}, nil
}

func (g *prefixedMintingTokenGenerator) SignAgentToken(grant AgentTokenGrant, runID string) (string, *store.AgentCredential, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	jti := fmt.Sprintf("test-jti-%s-%s-%d", g.prefix, grant.AgentID, len(g.jtis)+1)
	g.jtis = append(g.jtis, jti)
	now := time.Now()
	cred := &store.AgentCredential{
		AgentID:      grant.AgentID,
		ProjectID:    grant.ProjectID,
		TokenJTIHash: hashJTI(jti),
		RunID:        runID,
		IssuedAt:     now,
		ExpiresAt:    now.Add(10 * time.Hour),
	}
	return "fake-agent-jwt-" + jti, cred, nil
}

func (g *prefixedMintingTokenGenerator) lastJTI() string {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.jtis[len(g.jtis)-1]
}

func homeFilesTestDispatcher(t *testing.T, caps *store.BrokerCapabilities, asNeededVars ...store.EnvVar) (*HTTPAgentDispatcher, *mockRuntimeBrokerClient, *store.Agent, store.Store) {
	t.Helper()
	ctx := context.Background()
	s := createTestStore(t)

	brokerID := tid("hf-broker")
	projectID := tid("hf-project")
	agentID := tid("hf-agent")

	require.NoError(t, s.CreateRuntimeBroker(ctx, &store.RuntimeBroker{
		ID:           brokerID,
		Name:         "hf-broker",
		Slug:         "hf-broker",
		Endpoint:     "http://localhost:9800",
		Status:       store.BrokerStatusOnline,
		Capabilities: caps,
	}))
	require.NoError(t, s.CreateProject(ctx, &store.Project{
		ID:   projectID,
		Name: "hf-project",
		Slug: "hf-project",
	}))
	for i := range asNeededVars {
		v := asNeededVars[i]
		v.ID = api.NewUUID()
		v.Scope = store.ScopeProject
		v.ScopeID = projectID
		v.InjectionMode = store.InjectionModeAsNeeded
		require.NoError(t, s.CreateEnvVar(ctx, &v))
	}

	agent := &store.Agent{
		ID:              agentID,
		Name:            "hf-agent",
		Slug:            "hf-agent",
		ProjectID:       projectID,
		OwnerID:         tid("user-hf-1"),
		RuntimeBrokerID: brokerID,
		AppliedConfig:   &store.AgentAppliedConfig{HarnessConfig: "claude"},
	}
	require.NoError(t, s.CreateAgent(ctx, agent))

	m := &mockRuntimeBrokerClient{}
	d := NewHTTPAgentDispatcherWithClient(s, m, false, slog.Default())
	return d, m, agent, s
}

type homeFilesCreateOp struct {
	name string
	run  func(ctx context.Context, d *HTTPAgentDispatcher, a *store.Agent) error
}

// homeFilesCreateOps lists the top-level create entry points that accept
// WithCreateHomeFiles(ctx, files). Standalone DispatchFinalizeEnv is NOT an
// entry point: HomeFiles exist only within one dispatch's ctx (never persisted
// across a separate HTTP POST .../env submission), while same-ctx as_needed
// finalizeEnv and cross-node deferredFinalizeEnv are tested dedicatedly below.
func homeFilesCreateOps() []homeFilesCreateOp {
	return []homeFilesCreateOp{
		{
			name: "DispatchAgentCreate",
			run: func(ctx context.Context, d *HTTPAgentDispatcher, a *store.Agent) error {
				_, err := d.DispatchAgentCreate(ctx, a)
				return err
			},
		},
		{
			name: "DispatchAgentCreateWithGather",
			run: func(ctx context.Context, d *HTTPAgentDispatcher, a *store.Agent) error {
				_, err := d.DispatchAgentCreateWithGather(ctx, a)
				return err
			},
		},
		{
			name: "DispatchAgentProvision",
			run: func(ctx context.Context, d *HTTPAgentDispatcher, a *store.Agent) error {
				return d.DispatchAgentProvision(ctx, a)
			},
		},
	}
}

func TestHomeFilesDispatch_RefusesBrokerLackingCapability(t *testing.T) {
	sample := []api.HomeFile{
		{Path: ".scion/trigger-event.json", Mode: 0o600, Content: []byte(`{"id":"evt-1"}`)},
	}

	for capName, caps := range map[string]*store.BrokerCapabilities{
		"nil capabilities": nil,
		"homeFiles false":  {WebPTY: true, HomeFiles: false},
	} {
		for _, op := range homeFilesCreateOps() {
			t.Run(capName+"/"+op.name, func(t *testing.T) {
				d, m, a, s := homeFilesTestDispatcher(t, caps)
				tokens := &homeFilesTokenCounter{}
				d.SetTokenGenerator(tokens)

				ctx := WithCreateHomeFiles(context.Background(), sample)
				err := op.run(ctx, d, a)
				require.Error(t, err)
				assert.True(t, errors.Is(err, ErrBrokerLacksHomeFiles), "expected ErrBrokerLacksHomeFiles, got %v", err)
				assert.Contains(t, err.Error(), "broker hf-broker does not support home files")
				assert.True(t, isConfirmedStartNotActedOnError(err), "must be classified not-acted-on")
				assert.False(t, m.createCalled, "broker must not be contacted")
				assert.Zero(t, tokens.authorize, "no agent token must be authorized")
				assert.Zero(t, tokens.sign, "no agent token must be signed")

				stored, getErr := s.GetAgent(context.Background(), a.ID)
				require.NoError(t, getErr)
				assert.Empty(t, stored.RunID, "no run ID must be recorded")
				assert.Empty(t, a.RunID, "no run ID must be minted")
			})
		}
	}
}

func TestHomeFilesDispatch_RefusesInvalidHomeFilesBeforeDispatch(t *testing.T) {
	const secretCanary = "SUPER_SECRET_TRIGGER_PAYLOAD_CANARY"
	invalidCases := map[string][]api.HomeFile{
		"outside .scion": {
			{Path: ".ssh/authorized_keys", Mode: 0o600, Content: []byte(secretCanary)},
		},
		"invalid mode": {
			{Path: ".scion/trigger-event.json", Mode: 0o700, Content: []byte(secretCanary)},
		},
		"too many files": {
			{Path: ".scion/1.json", Mode: 0o600, Content: []byte("1")},
			{Path: ".scion/2.json", Mode: 0o600, Content: []byte("2")},
			{Path: ".scion/3.json", Mode: 0o600, Content: []byte("3")},
			{Path: ".scion/4.json", Mode: 0o600, Content: []byte("4")},
			{Path: ".scion/5.json", Mode: 0o600, Content: []byte(secretCanary)},
		},
		"oversize total content": {
			{
				Path:    api.TriggerEventHomeFilePath,
				Mode:    0o600,
				Content: append([]byte(secretCanary), bytes.Repeat([]byte("a"), api.MaxHomeFilesTotalBytes)...),
			},
		},
		"case variant of reserved secrets.json": {
			{Path: ".scion/Secrets.json", Mode: 0o600, Content: []byte(secretCanary)},
		},
		"case variant of allowed path": {
			{Path: ".scion/Trigger-Event.json", Mode: 0o600, Content: []byte(secretCanary)},
		},
		"hooks post-start path": {
			{Path: ".scion/hooks/post-start", Mode: 0o600, Content: []byte(secretCanary)},
		},
		"hooks pre-start.sh path": {
			{Path: ".scion/hooks/pre-start.sh", Mode: 0o600, Content: []byte(secretCanary)},
		},
		"unregistered path under .scion": {
			{Path: ".scion/other.json", Mode: 0o600, Content: []byte(secretCanary)},
		},
	}

	for caseName, files := range invalidCases {
		for _, op := range homeFilesCreateOps() {
			t.Run(caseName+"/"+op.name, func(t *testing.T) {
				d, m, a, _ := homeFilesTestDispatcher(t, &store.BrokerCapabilities{HomeFiles: true})
				tokens := &homeFilesTokenCounter{}
				d.SetTokenGenerator(tokens)

				ctx := WithCreateHomeFiles(context.Background(), files)
				err := op.run(ctx, d, a)
				require.Error(t, err)
				assert.NotContains(t, err.Error(), secretCanary, "validation errors must never echo content")
				assert.True(t, isConfirmedStartNotActedOnError(err), "must be classified not-acted-on")
				assert.False(t, m.createCalled, "broker must not be contacted")
				assert.Zero(t, tokens.authorize, "no agent token must be authorized")
				assert.Zero(t, tokens.sign, "no agent token must be signed")
			})
		}
	}
}

func TestHomeFilesDispatch_ProceedsWhenBrokerAdvertisesCapability(t *testing.T) {
	want := []api.HomeFile{
		{Path: api.TriggerEventHomeFilePath, Mode: 0o600, Content: []byte(`{"id":"evt-42"}`)},
	}

	for _, op := range homeFilesCreateOps() {
		t.Run(op.name, func(t *testing.T) {
			d, m, a, _ := homeFilesTestDispatcher(t, &store.BrokerCapabilities{HomeFiles: true})
			callerCopy := api.CloneHomeFiles(want)
			ctx := WithCreateHomeFiles(context.Background(), callerCopy)
			// Mutate caller slice after WithCreateHomeFiles to verify defensive cloning.
			callerCopy[0].Content[0] = 'X'

			require.NoError(t, op.run(ctx, d, a))
			require.True(t, m.createCalled)
			require.NotNil(t, m.lastCreateReq)
			assert.Equal(t, want, m.lastCreateReq.HomeFiles)
		})
	}

	t.Run("DispatchAgentCreateWithGather preserves HomeFiles across as_needed finalize replay", func(t *testing.T) {
		d, m, a, _ := homeFilesTestDispatcher(t, &store.BrokerCapabilities{HomeFiles: true}, store.EnvVar{
			Key:   "NEEDED_KEY",
			Value: "resolved-val",
		})
		var seen [][]api.HomeFile
		m.createWithGatherFunc = func(_ context.Context, _, _ string, req *RemoteCreateAgentRequest) (*RemoteAgentResponse, *RemoteEnvRequirementsResponse, error) {
			seen = append(seen, api.CloneHomeFiles(req.HomeFiles))
			if len(seen) == 1 {
				return nil, &RemoteEnvRequirementsResponse{Needs: []string{"NEEDED_KEY"}}, nil
			}
			return &RemoteAgentResponse{Agent: &RemoteAgentInfo{ID: req.ID, Phase: "running"}}, nil, nil
		}

		ctx := WithCreateHomeFiles(context.Background(), want)
		res, err := d.DispatchAgentCreateWithGather(ctx, a)
		require.NoError(t, err)
		assert.Nil(t, res)
		require.Len(t, seen, 2, "expected initial gather pass plus finalizeEnv replay")
		assert.Equal(t, want, seen[0])
		assert.Equal(t, want, seen[1])
	})
}

func TestHomeFilesDispatch_EmptyHomeFilesAllowsLegacyBroker(t *testing.T) {
	for _, op := range homeFilesCreateOps() {
		t.Run(op.name, func(t *testing.T) {
			d, m, a, _ := homeFilesTestDispatcher(t, nil)
			require.NoError(t, op.run(WithCreateHomeFiles(context.Background(), nil), d, a))
			require.True(t, m.createCalled)
			require.NotNil(t, m.lastCreateReq)
			assert.Empty(t, m.lastCreateReq.HomeFiles)
		})
	}
}

func TestHomeFilesDispatch_ReprovisionRefusesHomeFiles(t *testing.T) {
	d, m, a, _ := homeFilesTestDispatcher(t, &store.BrokerCapabilities{HomeFiles: true})
	sample := []api.HomeFile{
		{Path: ".scion/trigger-event.json", Mode: 0o600, Content: []byte(`{"id":"evt-99"}`)},
	}

	err := d.DispatchAgentReprovision(WithCreateHomeFiles(context.Background(), sample), a, ReprovisionOptions{})
	require.Error(t, err)
	assert.True(t, isConfirmedStartNotActedOnError(err))
	assert.False(t, m.createCalled, "reprovision with HomeFiles must not call broker")

	require.NoError(t, d.DispatchAgentReprovision(context.Background(), a, ReprovisionOptions{}))
	require.True(t, m.createCalled)
	assert.True(t, m.lastCreateReq.Reprovision)
	assert.Empty(t, m.lastCreateReq.HomeFiles)
}

func TestHomeFilesDispatch_CreateWithGatherFailsClosedOnUnresolvedEnvNeeds(t *testing.T) {
	want := []api.HomeFile{
		{Path: ".scion/trigger-event.json", Mode: 0o600, Content: []byte(`{"id":"evt-unresolved"}`)},
	}

	t.Run("no as_needed match fails closed and revokes credential", func(t *testing.T) {
		d, m, a, s := homeFilesTestDispatcher(t, &store.BrokerCapabilities{HomeFiles: true})
		gen := &fakeMintingTokenGenerator{store: s}
		d.SetTokenGenerator(gen)

		var seen []*RemoteCreateAgentRequest
		m.createWithGatherFunc = func(_ context.Context, _, _ string, req *RemoteCreateAgentRequest) (*RemoteAgentResponse, *RemoteEnvRequirementsResponse, error) {
			cp := *req
			cp.HomeFiles = api.CloneHomeFiles(req.HomeFiles)
			seen = append(seen, &cp)
			return nil, &RemoteEnvRequirementsResponse{Needs: []string{"CLI_ONLY_SECRET"}}, nil
		}

		ctx := withKeepCredentialOnUnknownOutcome(WithCreateHomeFiles(context.Background(), want))
		res, err := d.DispatchAgentCreateWithGather(ctx, a)
		require.Error(t, err)
		assert.Nil(t, res)
		assert.ErrorIs(t, err, ErrHomeFilesEnvGatherDeferred)
		assert.NotErrorIs(t, err, errStartRequestNotSent, "request was sent; must not wrap errStartRequestNotSent")
		assert.True(t, isConfirmedStartNotActedOnError(err), "must be classified confirmed not-acted-on")
		assert.False(t, isUnknownDispatchOutcome(err), "must be a definite failure so create rolls back")

		require.Len(t, seen, 1)
		assert.Equal(t, want, seen[0].HomeFiles, "every broker request sent during the attempt carried HomeFiles")

		require.Len(t, gen.jtis, 1)
		cred := getTestAgentCredential(t, s, gen.lastJTI())
		require.NotNil(t, cred.RevokedAt, "credential minted before 202 refusal must be revoked")
	})

	t.Run("partial as_needed match (ErrEnvStillMissing) fails closed and revokes all minted credentials", func(t *testing.T) {
		d, m, a, s := homeFilesTestDispatcher(t, &store.BrokerCapabilities{HomeFiles: true}, store.EnvVar{
			Key:   "AS_NEEDED_KEY",
			Value: "present",
		})
		gen := &fakeMintingTokenGenerator{store: s}
		d.SetTokenGenerator(gen)

		var seen [][]api.HomeFile
		m.createWithGatherFunc = func(_ context.Context, _, _ string, req *RemoteCreateAgentRequest) (*RemoteAgentResponse, *RemoteEnvRequirementsResponse, error) {
			seen = append(seen, api.CloneHomeFiles(req.HomeFiles))
			if len(seen) == 1 {
				return nil, &RemoteEnvRequirementsResponse{Needs: []string{"AS_NEEDED_KEY", "STILL_MISSING_KEY"}}, nil
			}
			return nil, &RemoteEnvRequirementsResponse{Needs: []string{"STILL_MISSING_KEY"}}, nil
		}

		ctx := withKeepCredentialOnUnknownOutcome(WithCreateHomeFiles(context.Background(), want))
		res, err := d.DispatchAgentCreateWithGather(ctx, a)
		require.Error(t, err)
		assert.Nil(t, res)
		assert.ErrorIs(t, err, ErrHomeFilesEnvGatherDeferred)
		assert.NotErrorIs(t, err, errStartRequestNotSent)
		assert.True(t, isConfirmedStartNotActedOnError(err))
		assert.False(t, isUnknownDispatchOutcome(err))

		// Pass 1 (initial gather), pass 2 (finalizeEnv first send), and no request ever sent empty HomeFiles.
		require.NotEmpty(t, seen)
		for i, hf := range seen {
			assert.Equal(t, want, hf, "broker request %d must carry HomeFiles", i)
		}
		require.NotEmpty(t, gen.jtis)
		for _, jti := range gen.jtis {
			assert.NotNil(t, getTestAgentCredential(t, s, jti).RevokedAt, "credential %s must be revoked", jti)
		}
	})

	t.Run("DispatchAgentProvision also fails closed when HomeFiles gather leaves unresolved needs", func(t *testing.T) {
		d, m, a, s := homeFilesTestDispatcher(t, &store.BrokerCapabilities{HomeFiles: true})
		gen := &fakeMintingTokenGenerator{store: s}
		d.SetTokenGenerator(gen)

		m.createWithGatherFunc = func(_ context.Context, _, _ string, _ *RemoteCreateAgentRequest) (*RemoteAgentResponse, *RemoteEnvRequirementsResponse, error) {
			return nil, &RemoteEnvRequirementsResponse{Needs: []string{"MISSING_ON_PROVISION"}}, nil
		}

		err := d.DispatchAgentProvision(WithCreateHomeFiles(context.Background(), want), a)
		require.Error(t, err)
		assert.ErrorIs(t, err, ErrHomeFilesEnvGatherDeferred)
		assert.NotErrorIs(t, err, errStartRequestNotSent)
		assert.True(t, isConfirmedStartNotActedOnError(err))
		require.Len(t, gen.jtis, 1)
		assert.NotNil(t, getTestAgentCredential(t, s, gen.lastJTI()).RevokedAt)
	})
}

func runPendingDispatchOnOwner(ctx context.Context, srv *Server, s store.Store, events *ChannelEventPublisher, brokerID string) error {
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		pending, err := s.ListPendingDispatch(ctx, brokerID)
		if err != nil {
			return err
		}
		if len(pending) == 0 {
			time.Sleep(5 * time.Millisecond)
			continue
		}
		row := pending[0]
		if _, err := s.ClaimBrokerDispatch(ctx, row.ID, "owner-node"); err != nil {
			return err
		}
		out, execErr := srv.executeDispatch(ctx, row)
		if execErr != nil {
			if ferr := s.FailBrokerDispatch(ctx, row.ID, execErr.Error(), dispatchFailureResult(execErr)); ferr != nil {
				return ferr
			}
			events.PublishDispatchDone(ctx, row.ID)
			return nil
		}
		if err := s.CompleteBrokerDispatch(ctx, row.ID, out); err != nil {
			return err
		}
		events.PublishDispatchDone(ctx, row.ID)
		return nil
	}
	return fmt.Errorf("timed out waiting for pending BrokerDispatch row on broker %s", brokerID)
}

func TestHomeFilesDispatch_CrossNodeDeferredCarriesHomeFiles(t *testing.T) {
	want := []api.HomeFile{
		{Path: api.TriggerEventHomeFilePath, Mode: 0o600, Content: []byte(`{"id":"evt-xnode"}`)},
	}

	setupCrossNode := func(t *testing.T, ownerClient *mockRuntimeBrokerClient, reqClient *mockRuntimeBrokerClient, asNeeded ...store.EnvVar) (*Server, *HTTPAgentDispatcher, *prefixedMintingTokenGenerator, *prefixedMintingTokenGenerator, *store.Agent, store.Store, *ChannelEventPublisher) {
		t.Helper()
		ctx := context.Background()
		srv, s := testServer(t)

		brokerID := tid("xnode-hf-broker")
		projectID := tid("xnode-hf-project")
		agentID := tid("xnode-hf-agent")

		require.NoError(t, s.CreateRuntimeBroker(ctx, &store.RuntimeBroker{
			ID:           brokerID,
			Name:         "xnode-hf-broker",
			Slug:         "xnode-hf-broker",
			Endpoint:     "http://localhost:9800",
			Status:       store.BrokerStatusOnline,
			Capabilities: &store.BrokerCapabilities{HomeFiles: true},
		}))
		require.NoError(t, s.CreateProject(ctx, &store.Project{
			ID:   projectID,
			Name: "xnode-hf-project",
			Slug: "xnode-hf-project",
		}))
		for i := range asNeeded {
			v := asNeeded[i]
			v.ID = api.NewUUID()
			v.Scope = store.ScopeProject
			v.ScopeID = projectID
			v.InjectionMode = store.InjectionModeAsNeeded
			require.NoError(t, s.CreateEnvVar(ctx, &v))
		}
		agent := &store.Agent{
			ID:              agentID,
			Name:            "xnode-hf-agent",
			Slug:            "xnode-hf-agent",
			ProjectID:       projectID,
			OwnerID:         tid("user-xnode-hf"),
			RuntimeBrokerID: brokerID,
			Phase:           string(state.PhaseProvisioning),
			AppliedConfig:   &store.AgentAppliedConfig{HarnessConfig: "claude"},
		}
		require.NoError(t, s.CreateAgent(ctx, agent))

		ownerDisp := NewHTTPAgentDispatcherWithClient(s, ownerClient, false, slog.Default())
		ownerGen := &prefixedMintingTokenGenerator{prefix: "owner"}
		ownerDisp.SetTokenGenerator(ownerGen)
		srv.SetDispatcher(ownerDisp)

		events := NewChannelEventPublisher()
		t.Cleanup(func() { events.Close() })

		reqDisp := NewHTTPAgentDispatcherWithClient(s, reqClient, false, slog.Default())
		reqGen := &prefixedMintingTokenGenerator{prefix: "req"}
		reqDisp.SetTokenGenerator(reqGen)
		reqDisp.SetCrossNodeDeps(events, NoopCommandBus{})

		return srv, reqDisp, reqGen, ownerGen, agent, s, events
	}

	t.Run("deferredCreateWithGather -> execDispatchCreate forwards HomeFiles to owner broker", func(t *testing.T) {
		var ownerReceived *RemoteCreateAgentRequest
		ownerClient := &mockRuntimeBrokerClient{
			createWithGatherFunc: func(_ context.Context, _, _ string, req *RemoteCreateAgentRequest) (*RemoteAgentResponse, *RemoteEnvRequirementsResponse, error) {
				cp := *req
				cp.HomeFiles = api.CloneHomeFiles(req.HomeFiles)
				ownerReceived = &cp
				return &RemoteAgentResponse{Agent: &RemoteAgentInfo{ID: req.ID, Phase: string(state.PhaseRunning)}}, nil, nil
			},
		}
		reqClient := &mockRuntimeBrokerClient{
			createWithGatherFunc: func(context.Context, string, string, *RemoteCreateAgentRequest) (*RemoteAgentResponse, *RemoteEnvRequirementsResponse, error) {
				return nil, nil, ErrLifecycleDeferred
			},
		}
		srv, reqDisp, _, _, agent, s, events := setupCrossNode(t, ownerClient, reqClient)

		ownerDone := make(chan error, 1)
		go func() {
			ownerDone <- runPendingDispatchOnOwner(context.Background(), srv, s, events, agent.RuntimeBrokerID)
		}()

		res, err := reqDisp.DispatchAgentCreateWithGather(WithCreateHomeFiles(context.Background(), want), agent)
		require.NoError(t, err)
		assert.Nil(t, res)
		require.NoError(t, <-ownerDone)

		require.NotNil(t, ownerReceived, "owner broker must receive the re-dispatched create")
		assert.Equal(t, want, ownerReceived.HomeFiles, "owner execDispatchCreate must attach HomeFiles from BrokerDispatch.Args")
	})

	t.Run("deferredFinalizeEnv -> execDispatchFinalizeEnv forwards HomeFiles and as_needed env to owner broker", func(t *testing.T) {
		var ownerReceived *RemoteCreateAgentRequest
		ownerClient := &mockRuntimeBrokerClient{
			createWithGatherFunc: func(_ context.Context, _, _ string, req *RemoteCreateAgentRequest) (*RemoteAgentResponse, *RemoteEnvRequirementsResponse, error) {
				cp := *req
				cp.HomeFiles = api.CloneHomeFiles(req.HomeFiles)
				ownerReceived = &cp
				return &RemoteAgentResponse{Agent: &RemoteAgentInfo{ID: req.ID, Phase: string(state.PhaseRunning)}}, nil, nil
			},
		}
		pass := 0
		reqClient := &mockRuntimeBrokerClient{
			createWithGatherFunc: func(context.Context, string, string, *RemoteCreateAgentRequest) (*RemoteAgentResponse, *RemoteEnvRequirementsResponse, error) {
				pass++
				if pass == 1 {
					return nil, &RemoteEnvRequirementsResponse{Needs: []string{"XNODE_AS_NEEDED"}}, nil
				}
				return nil, nil, ErrLifecycleDeferred
			},
		}
		srv, reqDisp, _, _, agent, s, events := setupCrossNode(t, ownerClient, reqClient, store.EnvVar{
			Key:   "XNODE_AS_NEEDED",
			Value: "xnode-secret-val",
		})

		ownerDone := make(chan error, 1)
		go func() {
			ownerDone <- runPendingDispatchOnOwner(context.Background(), srv, s, events, agent.RuntimeBrokerID)
		}()

		res, err := reqDisp.DispatchAgentCreateWithGather(WithCreateHomeFiles(context.Background(), want), agent)
		require.NoError(t, err)
		assert.Nil(t, res)
		require.NoError(t, <-ownerDone)

		require.NotNil(t, ownerReceived, "owner broker must receive the re-dispatched finalize_env create")
		assert.Equal(t, want, ownerReceived.HomeFiles, "owner execDispatchFinalizeEnv must attach HomeFiles from BrokerDispatch.Args")
		assert.Equal(t, "xnode-secret-val", ownerReceived.ResolvedEnv["XNODE_AS_NEEDED"])
	})

	t.Run("cross-node deferredCreateWithGather fails closed on unresolved owner env needs and revokes all credentials", func(t *testing.T) {
		ownerClient := &mockRuntimeBrokerClient{
			createWithGatherFunc: func(_ context.Context, _, _ string, _ *RemoteCreateAgentRequest) (*RemoteAgentResponse, *RemoteEnvRequirementsResponse, error) {
				return nil, &RemoteEnvRequirementsResponse{Needs: []string{"OWNER_MISSING_VAR"}}, nil
			},
		}
		reqClient := &mockRuntimeBrokerClient{
			createWithGatherFunc: func(context.Context, string, string, *RemoteCreateAgentRequest) (*RemoteAgentResponse, *RemoteEnvRequirementsResponse, error) {
				return nil, nil, ErrLifecycleDeferred
			},
		}
		srv, reqDisp, reqGen, ownerGen, agent, s, events := setupCrossNode(t, ownerClient, reqClient)

		ownerDone := make(chan error, 1)
		go func() {
			ownerDone <- runPendingDispatchOnOwner(context.Background(), srv, s, events, agent.RuntimeBrokerID)
		}()

		ctx := withKeepCredentialOnUnknownOutcome(WithCreateHomeFiles(context.Background(), want))
		res, err := reqDisp.DispatchAgentCreateWithGather(ctx, agent)
		require.Error(t, err)
		assert.Nil(t, res)
		require.NoError(t, <-ownerDone)

		assert.ErrorIs(t, err, ErrHomeFilesEnvGatherDeferred)
		assert.NotErrorIs(t, err, errStartRequestNotSent)
		assert.True(t, isConfirmedStartNotActedOnError(err))
		assert.False(t, isUnknownDispatchOutcome(err))
		require.Len(t, reqGen.jtis, 1)
		require.Len(t, ownerGen.jtis, 1)
		assert.NotNil(t, getTestAgentCredential(t, s, reqGen.lastJTI()).RevokedAt)
		assert.NotNil(t, getTestAgentCredential(t, s, ownerGen.lastJTI()).RevokedAt)
	})
}

type homeFilesFailSignStorage struct {
	*mockStorage
	signCalls int
}

func (f *homeFilesFailSignStorage) GenerateSignedURL(context.Context, string, storage.SignedURLOptions) (*storage.SignedURL, error) {
	f.signCalls++
	return nil, errors.New("unexpected signed URL call")
}

// Must not run in parallel: swaps the package-level managed-agent backend.
func TestHomeFilesCreateHandler_RefusesManagedAgentAndWorkspaceBootstrapUpload(t *testing.T) {
	want := []api.HomeFile{
		{Path: ".scion/trigger-event.json", Mode: 0o600, Content: []byte(`{"id":"evt-handler"}`)},
	}

	postCreateWithHomeFiles := func(t *testing.T, srv *Server, reqBody CreateAgentRequest) *httptest.ResponseRecorder {
		t.Helper()
		raw, err := json.Marshal(reqBody)
		require.NoError(t, err)
		r := httptest.NewRequest(http.MethodPost, "/api/v1/agents", bytes.NewReader(raw))
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("Authorization", "Bearer "+testDevToken)
		r = r.WithContext(WithCreateHomeFiles(r.Context(), want))
		rec := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rec, r)
		return rec
	}

	t.Run("managed agent refused with 422 before any store write or backend call", func(t *testing.T) {
		for _, tc := range []struct {
			name string
			req  CreateAgentRequest
		}{
			{"managedAgent flag", CreateAgentRequest{Name: "mgd-hf-flag", ManagedAgent: true, Task: "do work"}},
			{"legacy managed-agents profile", CreateAgentRequest{Name: "mgd-hf-prof", Profile: ManagedAgentsProfile, Task: "do work"}},
		} {
			t.Run(tc.name, func(t *testing.T) {
				disp := &createAgentDispatcher{}
				srv, s, project := setupCreateAgentServer(t, disp)
				managedCalled := false
				useManagedBackend(t, &recordingManagedBackend{
					s:         s,
					projectID: project.ID,
					hook:      func(string) { managedCalled = true },
				})

				reqBody := tc.req
				reqBody.ProjectID = project.ID
				rec := postCreateWithHomeFiles(t, srv, reqBody)

				require.Equal(t, http.StatusUnprocessableEntity, rec.Code, rec.Body.String())
				var errResp ErrorResponse
				require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &errResp))
				assert.Equal(t, ErrCodeValidationError, errResp.Error.Code)
				assert.Contains(t, errResp.Error.Message, "homeFiles")

				assert.False(t, managedCalled, "managed backend must not be invoked")
				assert.Nil(t, disp.capturedAgent, "dispatcher must not be invoked")
				assert.False(t, disp.deleteCalled, "runtime delete must not be invoked")

				agents, err := s.ListAgents(context.Background(), store.AgentFilter{ProjectID: project.ID}, store.ListOptions{})
				require.NoError(t, err)
				assert.Empty(t, agents.Items, "no agent row may be persisted")
				assert.Empty(t, activeReservationResources(t, s, "max_agents_per_project", "project", project.ID),
					"no quota reservation may remain active")
			})
		}
	})

	t.Run("remote workspace bootstrap upload refused with 422, neutral sentinel, and no side effects", func(t *testing.T) {
		disp := &createAgentDispatcher{}
		srv, s, project := setupCreateAgentServer(t, disp)
		clearCreateAgentProviderPath(t, s, project)
		stor := &homeFilesFailSignStorage{mockStorage: newMockStorage("test-bucket")}
		srv.SetStorage(stor)

		rec := postCreateWithHomeFiles(t, srv, CreateAgentRequest{
			Name:           "ws-upload-hf",
			ProjectID:      project.ID,
			Task:           "do work",
			WorkspaceFiles: []transfer.FileInfo{{Path: "main.go", Size: 64, Hash: "sha256:abc123"}},
		})

		require.Equal(t, http.StatusUnprocessableEntity, rec.Code, rec.Body.String())
		var errResp ErrorResponse
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &errResp))
		assert.Equal(t, ErrCodeValidationError, errResp.Error.Code)
		assert.Contains(t, errResp.Error.Message, "workspace bootstrap upload")

		assert.Equal(t, 0, stor.signCalls, "upload signed URLs must not be generated")
		assert.Nil(t, disp.capturedAgent, "dispatcher must not be invoked")
		assert.False(t, disp.deleteCalled, "runtime delete must not be invoked")

		agents, err := s.ListAgents(context.Background(), store.AgentFilter{ProjectID: project.ID}, store.ListOptions{})
		require.NoError(t, err)
		assert.Empty(t, agents.Items, "created agent row must be rolled back")
		assert.Empty(t, activeReservationResources(t, s, "max_agents_per_project", "project", project.ID),
			"project agent quota reservation must be released on rollback")

		failed, _, err := s.ListMutationAudits(context.Background(), store.MutationAuditFilter{
			TargetType:   "agent",
			MutationType: mutationTypeAgentCreateDispatchFailed,
		})
		require.NoError(t, err)
		require.Len(t, failed, 1)
		sum := assertCompensated(t, s, failed[0].TargetID)
		assert.Equal(t, createStageStorage, sum.Stage)
		assert.Equal(t, ErrHomeFilesCreateNotDeferrable.Error(), sum.Error,
			"rollback audit must record neutral ErrHomeFilesCreateNotDeferrable, not env-gather sentinel")
	})
}
