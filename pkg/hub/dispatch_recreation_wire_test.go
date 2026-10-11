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
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/secret"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// These tests drive DispatchAgentStart and DispatchAgentRestart through the
// real HTTP broker client against a recording broker, and read the request
// bodies the broker receives (ptone/scion#2157). They use only the
// dispatcher's public behaviour, so they also run unchanged against code
// that predates the change.

// recordingBroker records the JSON body of every start and restart request.
type recordingBroker struct {
	mu     sync.Mutex
	bodies map[string]map[string]interface{} // "start" / "restart" -> body
}

func newRecordingBroker(t *testing.T) (*recordingBroker, *httptest.Server) {
	t.Helper()
	rb := &recordingBroker{bodies: map[string]map[string]interface{}{}}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		data, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read body: %v", err)
		}
		body := map[string]interface{}{}
		if len(data) > 0 {
			if err := json.Unmarshal(data, &body); err != nil {
				t.Errorf("decode body: %v", err)
			}
		}
		op := r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:]
		rb.mu.Lock()
		rb.bodies[op] = body
		rb.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write([]byte(`{}`))
	}))
	t.Cleanup(srv.Close)
	return rb, srv
}

func (rb *recordingBroker) body(t *testing.T, op string) map[string]interface{} {
	t.Helper()
	rb.mu.Lock()
	defer rb.mu.Unlock()
	body, ok := rb.bodies[op]
	require.True(t, ok, "the broker received no %s request", op)
	return body
}

// newWireRecreationDispatcher builds a dispatcher whose broker is the
// recording broker, for an agent whose applied config carries a git clone,
// a branch, a Hub template and a Hub harness config. With localPath set the
// broker has a provider path for the project.
func newWireRecreationDispatcher(t *testing.T, localPath string) (*HTTPAgentDispatcher, *store.Agent, *recordingBroker) {
	t.Helper()
	ctx := context.Background()
	rb, srv := newRecordingBroker(t)
	memStore := createTestStore(t)

	project := &store.Project{
		ID:         tid("project-wire"),
		Name:       "wire-project",
		Slug:       "wire-project",
		GitRemote:  "https://github.com/example/repo.git",
		SharedDirs: []api.SharedDir{{Name: "cache"}},
		Labels: map[string]string{
			store.LabelWorkspaceMode: store.WorkspaceModeWorktreePerAgent,
		},
	}
	require.NoError(t, memStore.CreateProject(ctx, project))
	broker := &store.RuntimeBroker{
		ID:       tid("broker-wire"),
		Name:     "wire-broker",
		Slug:     "wire-broker",
		Endpoint: srv.URL,
		Status:   store.BrokerStatusOnline,
	}
	require.NoError(t, memStore.CreateRuntimeBroker(ctx, broker))
	if localPath != "" {
		require.NoError(t, memStore.AddProjectProvider(ctx, &store.ProjectProvider{
			ProjectID:  project.ID,
			BrokerID:   broker.ID,
			BrokerName: broker.Name,
			LocalPath:  localPath,
			Status:     store.BrokerStatusOnline,
		}))
	}

	d := NewHTTPAgentDispatcherWithClient(memStore, NewHTTPRuntimeBrokerClient(), false, slog.Default())
	agent := &store.Agent{
		ID:              tid("agent-wire"),
		Name:            "wire-agent",
		Slug:            "wire-agent",
		Template:        "web-dev",
		ProjectID:       project.ID,
		OwnerID:         "owner-wire",
		RuntimeBrokerID: broker.ID,
		AppliedConfig: &store.AgentAppliedConfig{
			GitClone:          &api.GitCloneConfig{URL: "https://github.com/example/repo.git"},
			Branch:            "feature-branch",
			TemplateID:        "template-id-1",
			TemplateHash:      "sha256:template-hash-1",
			HarnessConfig:     "claude",
			HarnessConfigID:   "hc-id-1",
			HarnessConfigHash: "sha256:hc-hash-1",
		},
	}
	return d, agent, rb
}

// TestDispatchAgentStart_WireCarriesTemplateIdentity: the start request
// carries the template ID and hash next to the template name.
func TestDispatchAgentStart_WireCarriesTemplateIdentity(t *testing.T) {
	d, agent, rb := newWireRecreationDispatcher(t, "")
	require.NoError(t, d.DispatchAgentStart(context.Background(), agent, "", false))

	body := rb.body(t, "start")
	assert.Equal(t, "web-dev", body["templateName"])
	assert.Equal(t, "template-id-1", body["templateId"])
	assert.Equal(t, "sha256:template-hash-1", body["templateHash"])
}

// TestDispatchAgentRestart_WireCarriesStartInputs: the restart request
// carries every input the start request carries for recreating the agent
// (project, harness config, shared dirs, git clone, branch, workspace mode,
// template identity), with the same values.
func TestDispatchAgentRestart_WireCarriesStartInputs(t *testing.T) {
	d, agent, rb := newWireRecreationDispatcher(t, "/home/user/projects/wire/.scion")
	require.NoError(t, d.DispatchAgentStart(context.Background(), agent, "", false))
	require.NoError(t, d.DispatchAgentRestart(context.Background(), agent))

	start := rb.body(t, "start")
	restart := rb.body(t, "restart")
	assert.Equal(t, "/home/user/projects/wire/.scion", restart["projectPath"])
	for _, key := range []string{
		"projectPath", "harnessConfig", "harnessConfigId", "harnessConfigHash",
		"sharedDirs", "gitClone", "branch", "workspaceMode",
		"templateName", "templateId", "templateHash",
	} {
		require.Contains(t, start, key, "the start request must carry %q", key)
		assert.Equal(t, start[key], restart[key], "restart must send %q as start does", key)
	}
	_, hasTask := restart["task"]
	assert.False(t, hasTask, "a restart sends no task")
	_, hasInline := restart["inlineConfig"]
	assert.False(t, hasInline, "a restart sends no inline config")
}

// TestDispatchAgentRestart_WireHubNativeProjectSendsSlug: with no provider
// path the restart names the project by slug, as start does.
func TestDispatchAgentRestart_WireHubNativeProjectSendsSlug(t *testing.T) {
	d, agent, rb := newWireRecreationDispatcher(t, "")
	require.NoError(t, d.DispatchAgentRestart(context.Background(), agent))

	restart := rb.body(t, "restart")
	assert.Equal(t, "wire-project", restart["projectSlug"])
	_, hasPath := restart["projectPath"]
	assert.False(t, hasPath)
}

// failingResolveBackend is a fake secret backend whose Resolve always fails.
type failingResolveBackend struct {
	mockSecretBackend
}

func (b *failingResolveBackend) Resolve(context.Context, string, string, string, *secret.ResolveOpts) ([]secret.SecretWithValue, error) {
	return nil, errors.New("fake secret backend unavailable")
}

// fakeFileSecret is a file-type secret with a fake value. File secrets do
// not travel in resolvedEnv, so before ptone/scion#2157 a restart dropped
// them.
var fakeFileSecret = secret.SecretWithValue{
	SecretMeta: secret.SecretMeta{Name: "FAKE_CREDS", SecretType: "file", Target: "~/.fake/creds.json", Scope: secret.ScopeUser},
	Value:      "fake-secret-value",
}

// TestDispatchAgentRestart_WireSendsResolvedSecretsLikeStart: a restart
// sends the resolved secrets a start sends for the same agent, resolved by
// the same buildStartEnv call at dispatch time.
func TestDispatchAgentRestart_WireSendsResolvedSecretsLikeStart(t *testing.T) {
	d, agent, rb := newWireRecreationDispatcher(t, "")
	d.SetSecretBackend(&mockSecretBackend{secrets: []secret.SecretWithValue{fakeFileSecret}})

	require.NoError(t, d.DispatchAgentStart(context.Background(), agent, "", false))
	require.NoError(t, d.DispatchAgentRestart(context.Background(), agent))

	start := rb.body(t, "start")
	restart := rb.body(t, "restart")
	require.Contains(t, start, "resolvedSecrets")
	require.Contains(t, restart, "resolvedSecrets", "a restart must send the resolved secrets")
	assert.Equal(t, start["resolvedSecrets"], restart["resolvedSecrets"], "restart must get exactly what start gets")
}

// TestDispatchAgentRestart_WireSecretResolutionErrorMatchesStart pins the
// behaviour on a secret resolution error: start and restart both proceed
// and send no resolved secrets (the error is logged, as on start).
func TestDispatchAgentRestart_WireSecretResolutionErrorMatchesStart(t *testing.T) {
	d, agent, rb := newWireRecreationDispatcher(t, "")
	d.SetSecretBackend(&failingResolveBackend{})

	startErr := d.DispatchAgentStart(context.Background(), agent, "", false)
	restartErr := d.DispatchAgentRestart(context.Background(), agent)
	assert.NoError(t, startErr)
	assert.NoError(t, restartErr, "restart must handle a resolution error as start does")

	for _, op := range []string{"start", "restart"} {
		_, has := rb.body(t, op)["resolvedSecrets"]
		assert.False(t, has, "%s must send no resolved secrets after a resolution error", op)
	}
}

// TestDispatchAgentRestart_WirePicksUpSecretChange: a restart resolves the
// secrets at dispatch time, so a secret changed between two restarts reaches
// the second restart with its new value (fake values only).
func TestDispatchAgentRestart_WirePicksUpSecretChange(t *testing.T) {
	d, agent, rb := newWireRecreationDispatcher(t, "")
	backend := &mockSecretBackend{secrets: []secret.SecretWithValue{fakeFileSecret}}
	d.SetSecretBackend(backend)

	secretValue := func() string {
		t.Helper()
		list, ok := rb.body(t, "restart")["resolvedSecrets"].([]interface{})
		require.True(t, ok, "the restart must send resolvedSecrets")
		require.Len(t, list, 1)
		entry, ok := list[0].(map[string]interface{})
		require.True(t, ok)
		value, _ := entry["value"].(string)
		return value
	}

	require.NoError(t, d.DispatchAgentRestart(context.Background(), agent))
	assert.Equal(t, "fake-secret-value", secretValue())

	changed := fakeFileSecret
	changed.Value = "fake-secret-value-rotated"
	backend.secrets = []secret.SecretWithValue{changed}

	require.NoError(t, d.DispatchAgentRestart(context.Background(), agent))
	assert.Equal(t, "fake-secret-value-rotated", secretValue(), "the second restart must carry the new value")
}
