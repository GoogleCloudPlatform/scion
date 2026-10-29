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
	"fmt"
	"log/slog"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/runtime"
	"github.com/GoogleCloudPlatform/scion/pkg/runtimebroker"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// End-to-end check, in process, that starting, resuming or restarting an
// existing agent hands the agent the credential the hub issued for that
// run: the hub dispatcher (real credential issue and run record) talks over
// HTTP to a real runtimebroker.Server, whose start and restart handlers
// decide which credential reaches the agent manager's Start
// (StartOptions.HubIssuedToken). The agent manager is a stand-in that
// records its StartOptions; pkg/agent's refusal of a start without a
// hub-issued credential is covered there (ErrNoHubToken).
//
// The dispatcher here uses the HTTP transport. The control-channel
// transport tunnels the same body (applyStartExtras) to the same start and
// restart handlers; its payload is covered by
// TestControlChannelBrokerClient_StartAndRestartSendEnvClassifications.

// credentialStartManager is runLabelManager with a Start that records its
// options and runs the entry with the run ID it was given, as pkg/agent's
// Start labels the container.
type credentialStartManager struct {
	runLabelManager
	startMu sync.Mutex
	starts  []api.StartOptions
}

func (m *credentialStartManager) Start(_ context.Context, opts api.StartOptions) (*api.AgentInfo, error) {
	m.startMu.Lock()
	m.starts = append(m.starts, opts)
	n := len(m.starts)
	m.startMu.Unlock()
	cid := fmt.Sprintf("cid-start-%d", n)
	m.run(opts.Name, cid, opts.HubProjectID, opts.ProjectPath, opts.RunID)
	return &api.AgentInfo{Name: opts.Name, ContainerID: cid, Phase: "running", RunID: opts.RunID}, nil
}

func (m *credentialStartManager) lastStart(t *testing.T) api.StartOptions {
	t.Helper()
	m.startMu.Lock()
	defer m.startMu.Unlock()
	require.NotEmpty(t, m.starts, "the broker never called the agent manager's Start")
	return m.starts[len(m.starts)-1]
}

func TestStartCredential_E2E_HubDispatchReachesAgentStart(t *testing.T) {
	for _, tc := range []struct {
		name     string
		running  bool // the agent's container is up (restart stops it first)
		resume   bool
		dispatch func(ctx context.Context, d *HTTPAgentDispatcher, a *store.Agent) error
	}{
		{
			name: "start",
			dispatch: func(ctx context.Context, d *HTTPAgentDispatcher, a *store.Agent) error {
				return d.DispatchAgentStart(ctx, a, "", false)
			},
		},
		{
			name:   "resume",
			resume: true,
			dispatch: func(ctx context.Context, d *HTTPAgentDispatcher, a *store.Agent) error {
				return d.DispatchAgentStart(ctx, a, "", true)
			},
		},
		{
			name:    "restart",
			running: true,
			dispatch: func(ctx context.Context, d *HTTPAgentDispatcher, a *store.Agent) error {
				return d.DispatchAgentRestart(ctx, a)
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			home := t.TempDir()
			t.Setenv("HOME", home)
			workDir := t.TempDir()
			t.Chdir(workDir)

			// A linked project on the broker whose settings select the
			// mock runtime, so the broker resolves the stand-in manager.
			const slug, name = "e2e-cred", "dev"
			projectID := tid("project-e2e-cred")
			scionDir := filepath.Join(workDir, ".scion")
			require.NoError(t, os.MkdirAll(scionDir, 0o755))
			require.NoError(t, os.WriteFile(filepath.Join(scionDir, "settings.yaml"), []byte(`schema_version: "1"
active_profile: local
profiles:
    local:
        runtime: mock
runtimes:
    mock:
        type: mock
`), 0o644))
			require.NoError(t, config.WriteProjectID(scionDir, projectID))

			mgr := &credentialStartManager{}
			cfg := runtimebroker.DefaultServerConfig()
			cfg.BrokerAuthEnabled = false // no hub keys here: serve the broker API without auth
			cfg.BrokerID = "e2e-cred-broker"
			cfg.BrokerName = "e2e-cred-broker"
			cfg.ForceRuntime = "mock"
			brokerSrv := runtimebroker.New(cfg, mgr, &runtime.MockRuntime{NameFunc: func() string { return "mock" }}, config.RuntimeBuildRecord{})
			httpSrv := httptest.NewServer(brokerSrv.Handler())
			defer httpSrv.Close()

			s := createTestStore(t)
			require.NoError(t, s.CreateProject(ctx, &store.Project{ID: projectID, Name: slug, Slug: slug}))
			brokerID := tid("broker-e2e-cred")
			require.NoError(t, s.CreateRuntimeBroker(ctx, &store.RuntimeBroker{
				ID: brokerID, Name: "e2e-cred-broker", Slug: "e2e-cred-broker", Endpoint: httpSrv.URL, Status: store.BrokerStatusOnline,
			}))
			require.NoError(t, s.AddProjectProvider(ctx, &store.ProjectProvider{
				ProjectID: projectID, BrokerID: brokerID, BrokerName: "e2e-cred-broker",
				LocalPath: scionDir, Status: store.BrokerStatusOnline,
			}))
			a := &store.Agent{
				ID: tid("agent-e2e-cred"), Name: name, Slug: name, ProjectID: projectID, RuntimeBrokerID: brokerID,
				Phase:         "stopped",
				AppliedConfig: &store.AgentAppliedConfig{HarnessConfig: "claude"},
			}
			if tc.running {
				a.Phase = "running"
			}
			require.NoError(t, s.CreateAgent(ctx, a))
			_, err := s.SetAgentRunID(ctx, a.ID, "run-before", nil)
			require.NoError(t, err)
			a, err = s.GetAgent(ctx, a.ID)
			require.NoError(t, err)
			if tc.running {
				mgr.run(name, "cid-before", projectID, scionDir, "run-before")
			}

			svc, err := NewAgentTokenService(AgentTokenConfig{})
			require.NoError(t, err)
			d := NewHTTPAgentDispatcherWithClient(s, NewHTTPRuntimeBrokerClient(), false, slog.Default())
			d.SetTokenGenerator(runTokenGenerator{svc: svc})

			require.NoError(t, tc.dispatch(ctx, d, a))

			opts := mgr.lastStart(t)
			require.NotEmpty(t, opts.HubIssuedToken, "the agent received no hub-issued credential")
			claims, err := svc.ValidateAgentToken(opts.HubIssuedToken)
			require.NoError(t, err, "the agent's credential is not one this hub issued")
			assert.Equal(t, a.ID, claims.Subject, "credential agent")
			assert.NotEqual(t, "run-before", opts.RunID)
			assert.Equal(t, opts.RunID, claims.RunID, "the credential is the one issued for this run")
			got, err := s.GetAgent(ctx, a.ID)
			require.NoError(t, err)
			assert.Equal(t, opts.RunID, got.RunID, "the hub recorded the run the agent started with")
			assert.Equal(t, tc.resume, opts.Resume, "resume flag")
			_, inEnv := opts.Env[startCredentialEnvKey]
			assert.False(t, inEnv, "the credential must reach the agent only as the hub-issued credential, not as a plain env value")
		})
	}
}
