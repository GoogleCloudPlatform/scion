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
	"io"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/messages"
	"github.com/GoogleCloudPlatform/scion/pkg/storage"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// mockStorage implements storage.Storage for testing.
type mockStorage struct {
	bucket string
	// provider is what Provider reports; "" means storage.ProviderLocal.
	provider storage.Provider
	// mu guards objects and content: the Phase-4 import path uploads files and
	// resources concurrently, so real backends (GCS / local FS) are exercised
	// concurrently and the mock must be safe for concurrent access too (and
	// race-clean under `go test -race`).
	mu      sync.Mutex
	objects map[string]*storage.Object // objectPath -> Object
	content map[string][]byte          // objectPath -> file data
}

func newMockStorage(bucket string) *mockStorage {
	return &mockStorage{
		bucket:  bucket,
		objects: make(map[string]*storage.Object),
	}
}

// mockDispatcher implements AgentDispatcher for testing bootstrap dispatch.
type mockDispatcher struct {
	dispatchedAgents []*store.Agent
	startedAgents    []*store.Agent
	returnErr        error
}

// testBootstrapServer creates a test server with storage and dispatcher configured.
func testBootstrapServer(t *testing.T) (*Server, store.Store, *mockStorage, *mockDispatcher) {
	t.Helper()
	s, err := newTestStore(t, ":memory:")
	if err != nil {
		t.Fatalf("failed to create test store: %v", err)
	}

	cfg := DefaultServerConfig()
	cfg.DevAuthToken = testBootstrapDevToken
	srv, err := newTestHubServer(t, cfg, s)
	if err != nil {
		t.Fatalf("New() failed: %v", err)
	}

	stor := newMockStorage("test-bucket")
	srv.SetStorage(stor)

	disp := &mockDispatcher{}
	srv.SetDispatcher(disp)

	return srv, s, stor, disp
}

// doBootstrapRequest performs an authenticated HTTP request for bootstrap tests.
func doBootstrapRequest(t *testing.T, srv *Server, method, path string, body interface{}) *httptest.ResponseRecorder {
	t.Helper()
	var bodyBytes []byte
	if body != nil {
		var err error
		bodyBytes, err = json.Marshal(body)
		if err != nil {
			t.Fatalf("failed to marshal body: %v", err)
		}
	}

	req := httptest.NewRequest(method, path, bytes.NewReader(bodyBytes))
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Authorization", "Bearer "+testBootstrapDevToken)

	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	return rec
}

// setupProjectAndBroker creates a project and broker for agent creation tests.
func setupProjectAndBroker(t *testing.T, s store.Store) (string, string) {
	t.Helper()
	ctx := context.Background()

	broker := &store.RuntimeBroker{
		ID:     tid("broker_bootstrap_test"),
		Slug:   "bootstrap-host",
		Name:   "Bootstrap Host",
		Status: store.BrokerStatusOnline,
	}
	if err := s.CreateRuntimeBroker(ctx, broker); err != nil {
		t.Fatalf("failed to create runtime broker: %v", err)
	}

	project := &store.Project{
		ID:                     tid("project_bootstrap_test"),
		Slug:                   "bootstrap-project",
		Name:                   "Bootstrap Project",
		GitRemote:              "https://github.com/test/bootstrap",
		DefaultRuntimeBrokerID: broker.ID,
		Created:                time.Now(),
		Updated:                time.Now(),
	}
	if err := s.CreateProject(ctx, project); err != nil {
		t.Fatalf("failed to create project: %v", err)
	}

	provider := &store.ProjectProvider{
		ProjectID:  project.ID,
		BrokerID:   broker.ID,
		BrokerName: broker.Name,
		Status:     store.BrokerStatusOnline,
	}
	if err := s.AddProjectProvider(ctx, provider); err != nil {
		t.Fatalf("failed to add project provider: %v", err)
	}

	return project.ID, broker.ID
}

func (m *mockStorage) Bucket() string { return m.bucket }
func (m *mockStorage) Provider() storage.Provider {
	if m.provider == "" {
		return storage.ProviderLocal
	}
	return m.provider
}
func (m *mockStorage) Close() error { return nil }

func (m *mockStorage) GenerateSignedURL(_ context.Context, objectPath string, opts storage.SignedURLOptions) (*storage.SignedURL, error) {
	return &storage.SignedURL{
		URL:     fmt.Sprintf("https://storage.example.com/%s/%s?signed=true", m.bucket, objectPath),
		Method:  opts.Method,
		Expires: time.Now().Add(opts.Expires),
	}, nil
}

func (m *mockStorage) GetObject(_ context.Context, objectPath string) (*storage.Object, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	obj, ok := m.objects[objectPath]
	if !ok {
		return nil, storage.ErrNotFound
	}
	return obj, nil
}

func (m *mockStorage) Exists(_ context.Context, objectPath string) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	_, ok := m.objects[objectPath]
	return ok, nil
}

func (m *mockStorage) Upload(_ context.Context, objectPath string, r io.Reader, opts storage.UploadOptions) (*storage.Object, error) {
	var data []byte
	if r != nil {
		var err error
		data, err = io.ReadAll(r)
		if err != nil {
			return nil, err
		}
	}
	obj := &storage.Object{
		Name:     objectPath,
		Size:     int64(len(data)),
		Metadata: opts.Metadata,
	}
	m.mu.Lock()
	m.objects[objectPath] = obj
	if data != nil {
		if m.content == nil {
			m.content = make(map[string][]byte)
		}
		m.content[objectPath] = data
	}
	m.mu.Unlock()
	return obj, nil
}

func (m *mockStorage) Download(_ context.Context, objectPath string) (io.ReadCloser, *storage.Object, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	obj, ok := m.objects[objectPath]
	if !ok {
		return nil, nil, storage.ErrNotFound
	}
	data := m.content[objectPath]
	return io.NopCloser(bytes.NewReader(data)), obj, nil
}

func (m *mockStorage) Delete(_ context.Context, objectPath string) error {
	m.mu.Lock()
	delete(m.objects, objectPath)
	m.mu.Unlock()
	return nil
}

func (m *mockStorage) DeletePrefix(_ context.Context, _ string) error { return nil }
func (m *mockStorage) List(_ context.Context, opts storage.ListOptions) (*storage.ListResult, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	res := &storage.ListResult{}
	for path, obj := range m.objects {
		if opts.Prefix != "" && !strings.HasPrefix(path, opts.Prefix) {
			continue
		}
		res.Objects = append(res.Objects, *obj)
	}
	return res, nil
}

func (m *mockStorage) Copy(_ context.Context, _, _ string) (*storage.Object, error) {
	return nil, fmt.Errorf("not implemented")
}

func (d *mockDispatcher) DispatchAgentCreate(_ context.Context, agent *store.Agent) (*CreateDispatchResult, error) {
	if d.returnErr != nil {
		return nil, d.returnErr
	}
	d.dispatchedAgents = append(d.dispatchedAgents, agent)
	agent.Phase = string(state.PhaseProvisioning)
	return nil, nil
}

func (d *mockDispatcher) DispatchAgentProvision(_ context.Context, agent *store.Agent) error {
	if d.returnErr != nil {
		return d.returnErr
	}
	d.dispatchedAgents = append(d.dispatchedAgents, agent)
	agent.Phase = string(state.PhaseCreated)
	return nil
}

func (d *mockDispatcher) DispatchAgentReprovision(_ context.Context, agent *store.Agent) error {
	if d.returnErr != nil {
		return d.returnErr
	}
	d.dispatchedAgents = append(d.dispatchedAgents, agent)
	agent.Phase = string(state.PhaseCreated)
	return nil
}
func (d *mockDispatcher) DispatchAgentStart(_ context.Context, agent *store.Agent, _ string, _ bool) error {
	d.startedAgents = append(d.startedAgents, agent)
	return nil
}
func (d *mockDispatcher) DispatchAgentStop(_ context.Context, _ *store.Agent) error { return nil }
func (d *mockDispatcher) DispatchAgentRestart(_ context.Context, _ *store.Agent) error {
	return nil
}
func (d *mockDispatcher) DispatchAgentResetAuth(_ context.Context, _ *store.Agent) error {
	return nil
}
func (d *mockDispatcher) DispatchAgentDelete(_ context.Context, _ *store.Agent, _, _, _ bool, _ time.Time) error {
	return nil
}
func (d *mockDispatcher) DispatchAgentMessage(_ context.Context, _ *store.Agent, _ string, _ bool, _ *messages.StructuredMessage) error {
	return nil
}
func (d *mockDispatcher) DispatchCheckAgentPrompt(_ context.Context, _ *store.Agent) (bool, error) {
	return false, nil
}
func (d *mockDispatcher) DispatchAgentCreateWithGather(_ context.Context, agent *store.Agent) (*CreateDispatchResult, error) {
	return d.DispatchAgentCreate(context.Background(), agent)
}
func (d *mockDispatcher) DispatchAgentLogs(_ context.Context, _ *store.Agent, _ int) (string, error) {
	return "", nil
}
func (d *mockDispatcher) DispatchAgentExec(_ context.Context, _ *store.Agent, _ []string, _ int) (string, int, error) {
	return "", 0, nil
}
func (d *mockDispatcher) DispatchFinalizeEnv(_ context.Context, _ *store.Agent, _ map[string]string) (*CreateDispatchResult, error) {
	return nil, nil
}

// testBootstrapDevToken is the development token used for bootstrap testing.
const testBootstrapDevToken = "scion_dev_bootstrap_test_token_1234567890"
