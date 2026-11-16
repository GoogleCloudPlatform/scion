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
	"errors"
	"fmt"
	"log/slog"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
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
		{
			name: "DispatchFinalizeEnv",
			run: func(ctx context.Context, d *HTTPAgentDispatcher, a *store.Agent) error {
				_, err := d.DispatchFinalizeEnv(ctx, a, map[string]string{"EXTRA": "1"})
				return err
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
				Path:    ".scion/big.json",
				Mode:    0o600,
				Content: append([]byte(secretCanary), bytes.Repeat([]byte("a"), api.MaxHomeFilesTotalBytes)...),
			},
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
		{Path: ".scion/trigger-event.json", Mode: 0o600, Content: []byte(`{"id":"evt-42"}`)},
		{Path: ".scion/meta/readme.txt", Mode: 0o644, Content: []byte("context")},
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
