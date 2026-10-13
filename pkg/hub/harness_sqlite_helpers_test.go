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
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/hub/imagecheck"
	"github.com/GoogleCloudPlatform/scion/pkg/storage"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/resources"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// makeHarnessConfigDir creates a temp harness-configs directory with a single
// config subdirectory containing config.yaml and optional extra files.
// Returns the parent harness-configs directory.
func makeHarnessConfigDir(t *testing.T, configName string, files map[string]string) string {
	t.Helper()
	parentDir := t.TempDir()
	configDir := filepath.Join(parentDir, configName)
	for relPath, content := range files {
		full := filepath.Join(configDir, relPath)
		if err := os.MkdirAll(filepath.Dir(full), 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(content), 0644); err != nil {
			t.Fatal(err)
		}
	}
	return parentDir
}

// failingUploadStorage wraps mockStorage and fails every upload, to force a
// content sync of an existing row to fail.
type failingUploadStorage struct {
	*mockStorage
}

func testHarnessConfigFileServer(t *testing.T) (*Server, store.Store, *contentMockStorage) {
	t.Helper()
	s, err := newTestStore(t, ":memory:")
	if err != nil {
		if strings.Contains(err.Error(), "sqlite driver not registered") {
			t.Skip("Skipping: sqlite driver not registered")
		}
		t.Fatalf("failed to create test store: %v", err)
	}

	cfg := DefaultServerConfig()
	cfg.DevAuthToken = testDevToken
	srv, err := newTestHubServer(t, cfg, s)
	if err != nil {
		t.Fatalf("New() failed: %v", err)
	}

	stor := newContentMockStorage("test-bucket")
	srv.SetStorage(stor)

	return srv, s, stor
}

func createTestHarnessConfigWithFiles(t *testing.T, s store.Store, stor *contentMockStorage, files map[string]string) *store.HarnessConfig {
	t.Helper()
	ctx := context.Background()

	hc := &store.HarnessConfig{
		ID:            tid("hc-file-test-1"),
		Name:          "test-hc",
		Slug:          "test-hc",
		Harness:       "claude",
		Scope:         store.HarnessConfigScopeGlobal,
		Status:        store.HarnessConfigStatusActive,
		StoragePath:   "harness-configs/global/test-hc",
		StorageBucket: "test-bucket",
		Updated:       time.Date(2026, 7, 15, 12, 0, 0, 0, time.UTC),
	}

	hcFiles := make([]store.TemplateFile, 0, len(files))
	for path, content := range files {
		objectPath := hc.StoragePath + "/" + path
		stor.content[objectPath] = []byte(content)
		stor.objects[objectPath] = &storage.Object{
			Name: objectPath,
			Size: int64(len(content)),
		}
		hcFiles = append(hcFiles, store.TemplateFile{
			Path: path,
			Size: int64(len(content)),
			Hash: "sha256:placeholder",
		})
	}
	hc.Files = hcFiles
	hc.ContentHash = computeContentHash(hcFiles)

	if err := s.CreateHarnessConfig(ctx, hc); err != nil {
		t.Fatalf("failed to create test harness config: %v", err)
	}
	return hc
}

// harnessConfigMultipartRequest builds a multipart form upload request for
// harness-config file upload tests, mirroring templateMultipartRequest.
func harnessConfigMultipartRequest(t *testing.T, hcID string, files map[string][]byte) *http.Request {
	t.Helper()
	var buf bytes.Buffer
	writer := multipart.NewWriter(&buf)

	for fieldName, content := range files {
		part, err := writer.CreateFormFile(fieldName, fieldName)
		if err != nil {
			t.Fatalf("failed to create form file: %v", err)
		}
		if _, err := part.Write(content); err != nil {
			t.Fatalf("failed to write form file: %v", err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("failed to close multipart writer: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/api/v1/harness-configs/"+hcID+"/files", &buf)
	req.Header.Set("Content-Type", writer.FormDataContentType())
	req.Header.Set("Authorization", "Bearer "+testDevToken)
	return req
}

func setupImageStatusTest(t *testing.T) (*Server, store.Store) {
	t.Helper()
	// CO1: use testServer to get a fully-initialized server with migrated
	// store, role definitions, and dev user role bindings. The image status
	// admin user also needs a super-admin role binding for broker dispatch.
	srv, db := testServer(t)
	srv.imageChecker = imagecheck.NewChecker()
	createTestUserWithRole(t, db, tid("image-status-admin"), "admin@example.com", "admin", store.SystemRoleSuperAdmin)
	return srv, db
}

// imageStatusRequest builds a request carrying an authenticated admin identity.
//
// These handlers filter the broker list through canDispatchToBroker, which now
// denies a caller with no identity at all where it previously allowed one
// (#591). These tests invoke the handler directly, bypassing the auth
// middleware, so before this helper they ran with an empty context — and every
// broker was filtered out, leaving the aggregation under test nothing to
// aggregate.
//
// That they broke is not a reason to soften the deny: it is evidence that the
// fail-open was load-bearing at this call site, which is the finding, not a side
// effect. Nor is it a production behaviour change — in production the auth
// middleware guarantees an identity is present by the time these handlers run.
// Supplying the identity the middleware would have supplied is the repair; the
// alternative, relaxing canDispatchToBroker so unauthenticated callers keep
// seeing brokers, is the bug.
func imageStatusRequest(method, path string) *http.Request {
	req := httptest.NewRequest(method, path, nil)
	return req.WithContext(contextWithIdentity(req.Context(),
		NewAuthenticatedUser(tid("image-status-admin"), "admin@example.com", "Admin", "admin", "cli")))
}

func createTestBroker(t *testing.T, db store.Store, id, name, endpoint string, profiles []store.BrokerProfile, labels map[string]string) {
	t.Helper()
	broker := &store.RuntimeBroker{
		ID:       tid(id),
		Name:     name,
		Slug:     name,
		Endpoint: endpoint,
		Status:   store.BrokerStatusOnline,
		Profiles: profiles,
		Labels:   labels,
		Created:  time.Now(),
		Updated:  time.Now(),
	}
	if err := db.CreateRuntimeBroker(context.Background(), broker); err != nil {
		t.Fatalf("failed to create broker %s: %v", name, err)
	}
}

func createTestHarnessConfig(t *testing.T, db store.Store, id, image string) *store.HarnessConfig {
	t.Helper()
	hc := &store.HarnessConfig{
		ID:      tid(id),
		Name:    "test-config",
		Slug:    "test-config",
		Harness: "test",
		Scope:   store.HarnessConfigScopeGlobal,
		Status:  store.HarnessConfigStatusActive,
		Config: &store.HarnessConfigData{
			Image: image,
		},
	}
	if err := db.CreateHarnessConfig(context.Background(), hc); err != nil {
		t.Fatalf("failed to create harness config: %v", err)
	}
	return hc
}

type fakeImageManager struct {
	exists map[string]bool
}

// localStorageHarnessConfig sets up a server backed by real local storage
// (rooted at a bucket directory below root) and a harness-config record whose
// file list is recordPaths. Every path in storedPaths is written to storage
// below the config's storage path. Local storage resolves object paths with
// filepath.Join, so it shows what a path really points at on disk.
func localStorageHarnessConfig(t *testing.T, recordPaths, storedPaths []string) (srv *Server, s store.Store, hc *store.HarnessConfig, root string) {
	t.Helper()
	srv, s, _ = testHarnessConfigFileServer(t)
	root = t.TempDir()
	stor, err := storage.NewLocal(storage.Config{Provider: storage.ProviderLocal, Bucket: "b", LocalPath: root})
	if err != nil {
		t.Fatalf("NewLocal: %v", err)
	}
	srv.SetStorage(stor)

	hc = &store.HarnessConfig{
		ID:            tid("hc-local-paths"),
		Name:          "test-hc",
		Slug:          "test-hc",
		Harness:       "claude",
		Scope:         store.HarnessConfigScopeGlobal,
		Status:        store.HarnessConfigStatusActive,
		StoragePath:   "harness-configs/global/test-hc",
		StorageBucket: "b",
	}
	for _, p := range storedPaths {
		if _, err := stor.Upload(context.Background(), hc.StoragePath+"/"+p, strings.NewReader("x\n"), storage.UploadOptions{}); err != nil {
			t.Fatalf("upload %s: %v", p, err)
		}
	}
	for _, p := range recordPaths {
		hc.Files = append(hc.Files, store.TemplateFile{Path: p, Size: 2, Hash: "sha256:placeholder"})
	}
	hc.ContentHash = computeContentHash(hc.Files)
	if err := s.CreateHarnessConfig(context.Background(), hc); err != nil {
		t.Fatalf("CreateHarnessConfig: %v", err)
	}
	return srv, s, hc, root
}

// outsidePath is four levels up from harness-configs/global/test-hc in bucket
// "b", i.e. the storage root's parent directory, outside the bucket.
const outsidePath = "../../../../outside.txt"

func writeOutsideFile(t *testing.T, root string) string {
	t.Helper()
	outside := filepath.Join(root, "outside.txt")
	if err := os.WriteFile(outside, []byte("keep\n"), 0644); err != nil {
		t.Fatal(err)
	}
	return outside
}

const (
	installCanonicalClaudeURL = "https://github.com/GoogleCloudPlatform/scion/harnesses/claude"
	installPinnedClaudeURL    = "https://github.com/GoogleCloudPlatform/scion/tree/v0.9.0/harnesses/claude"
)

// embeddedClaudeFiles returns the files of the bundled claude harness config,
// standing in for what the CLI fetched from the URL.
func embeddedClaudeFiles(t *testing.T) []installFile {
	t.Helper()
	for _, r := range resources.BuiltinHarnessConfigs() {
		if r.Name != "claude" {
			continue
		}
		var files []installFile
		require.NoError(t, fs.WalkDir(r.FS, ".", func(p string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() {
				return err
			}
			data, err := fs.ReadFile(r.FS, p)
			if err != nil {
				return err
			}
			files = append(files, installFile{p, data})
			return nil
		}))
		require.NotEmpty(t, files)
		return files
	}
	t.Fatal("claude not in the bundled catalog")
	return nil
}

// globalClaude returns the global claude row, or nil.
func globalClaude(t *testing.T, s store.Store) *store.HarnessConfig {
	t.Helper()
	hc, err := s.GetHarnessConfigBySlug(context.Background(), "claude", store.HarnessConfigScopeGlobal, "")
	if err == store.ErrNotFound {
		return nil
	}
	require.NoError(t, err)
	require.NotNil(t, hc)
	return hc
}

// installClaudeViaHub mirrors syncHarnessConfigToHub for a global install:
// create the row if it does not exist, upload the files to its storage path,
// then finalize with the manifest and sourceURL ("" sends no sourceUrl).
func installClaudeViaHub(t *testing.T, srv *Server, s store.Store, sourceURL string) *store.HarnessConfig {
	t.Helper()
	files := embeddedClaudeFiles(t)

	hc := globalClaude(t, s)
	if hc == nil {
		reqFiles := make([]map[string]interface{}, 0, len(files))
		for _, f := range files {
			reqFiles = append(reqFiles, map[string]interface{}{"path": f.path, "size": len(f.data)})
		}
		rec := doRequest(t, srv, http.MethodPost, "/api/v1/harness-configs", map[string]interface{}{
			"name": "claude", "harness": "claude", "scope": "global", "files": reqFiles,
		})
		require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
		hc = globalClaude(t, s)
		require.NotNil(t, hc)
	}

	stor := srv.GetStorage()
	manifestFiles := make([]map[string]interface{}, 0, len(files))
	for _, f := range files {
		_, err := stor.Upload(context.Background(), hc.StoragePath+"/"+f.path, bytes.NewReader(f.data), storage.UploadOptions{})
		require.NoError(t, err)
		sum := sha256.Sum256(f.data)
		manifestFiles = append(manifestFiles, map[string]interface{}{
			"path": f.path, "size": len(f.data), "hash": "sha256:" + hex.EncodeToString(sum[:]),
		})
	}
	body := map[string]interface{}{"manifest": map[string]interface{}{"version": "1.0", "harness": "claude", "files": manifestFiles}}
	if sourceURL != "" {
		body["sourceUrl"] = sourceURL
	}
	rec := doRequest(t, srv, http.MethodPost, "/api/v1/harness-configs/"+hc.ID+"/finalize", body)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	after := globalClaude(t, s)
	require.NotNil(t, after)
	return after
}

// testInstallSourceServer is a dev-auth hub with storage after one hosted
// bootstrap, with the bundled claude row deleted (the ledger lists it, so
// startup does not re-create it).
func testInstallSourceServer(t *testing.T) (*Server, store.Store) {
	t.Helper()
	srv, s := testServer(t)
	srv.SetStorage(newMockStorage("test-bucket"))
	hostedInstallBootstrap(t, srv)
	hc := globalClaude(t, s)
	require.NotNil(t, hc)
	require.NoError(t, s.DeleteHarnessConfig(context.Background(), hc.ID))
	return srv, s
}

// setProjectHarnessConfigAnnotation stamps the project-level
// default-harness-config annotation, exactly as PUT /settings would.
func setProjectHarnessConfigAnnotation(t *testing.T, s store.Store, project *store.Project, value string) {
	t.Helper()
	if project.Annotations == nil {
		project.Annotations = map[string]string{}
	}
	project.Annotations[projectSettingDefaultHarnessConfig] = value
	require.NoError(t, s.UpdateProject(context.Background(), project))
}

// setProjectAnnotations merges the given annotations into the project, exactly
// as PUT /settings would. Used where a test needs more than one setting.
func setProjectAnnotations(t *testing.T, s store.Store, project *store.Project, annotations map[string]string) {
	t.Helper()
	if project.Annotations == nil {
		project.Annotations = map[string]string{}
	}
	for k, v := range annotations {
		project.Annotations[k] = v
	}
	require.NoError(t, s.UpdateProject(context.Background(), project))
}

// createHarnessTemplate creates a global template whose DefaultHarnessConfig is
// set. ContentHash is populated so the agent-create handler's "template has no
// files" guard does not reject it.
func createHarnessTemplate(t *testing.T, s store.Store, slug, defaultHarnessConfig string) *store.Template {
	t.Helper()
	tmpl := &store.Template{
		ID:                   tid("template-" + slug + "-" + t.Name()),
		Name:                 slug,
		Slug:                 slug,
		Harness:              "claude",
		DefaultHarnessConfig: defaultHarnessConfig,
		ContentHash:          "d00dfeed",
		Scope:                store.TemplateScopeGlobal,
		Status:               "active",
	}
	require.NoError(t, s.CreateTemplate(context.Background(), tmpl))
	return tmpl
}

// levelCapturingHandler records the level and message of every log record that
// passes the level filter, so a test can assert on how loudly something was
// logged rather than only whether it happened.
type levelCapturingHandler struct {
	mu      sync.Mutex
	records []slog.Record
}

// captureHarnessLogs swaps the server's agent-lifecycle logger for a capturing
// one and returns the handler.
func captureHarnessLogs(srv *Server) *levelCapturingHandler {
	h := &levelCapturingHandler{}
	srv.agentLifecycleLog = slog.New(h)
	return h
}

// runDispatchAgentEvent fires a dispatch_agent scheduled event for the project
// and returns the created agent.
func runDispatchAgentEvent(t *testing.T, srv *Server, s store.Store, projectID, agentName, template string) *store.Agent {
	t.Helper()
	ctx := context.Background()
	if _, err := s.GetUser(ctx, DevUserID); err != nil {
		require.ErrorIs(t, err, store.ErrNotFound)
		require.NoError(t, s.CreateUser(ctx, &store.User{
			ID:          DevUserID,
			Email:       "dev@localhost",
			DisplayName: "Dev User",
			Role:        store.UserRoleAdmin,
		}))
	}

	payload, err := json.Marshal(DispatchAgentEventPayload{
		AgentName: agentName,
		Template:  template,
		Task:      "do the thing",
	})
	require.NoError(t, err)

	handler := srv.dispatchAgentEventHandler()
	require.NoError(t, handler(ctx, withSessionRevision(store.ScheduledEvent{
		ID:        tid("sched-" + agentName + "-" + t.Name()),
		ProjectID: projectID,
		EventType: "dispatch_agent",
		Payload:   string(payload),
		CreatedBy: DevUserID,
	}, DevUserID)))

	agent, err := s.GetAgentBySlug(ctx, projectID, agentName)
	require.NoError(t, err)
	require.NotNil(t, agent)
	require.NotNil(t, agent.AppliedConfig)
	return agent
}

// assertBrokerRefusalRelayed asserts rec carries the broker's status, code
// and message, with no details.
func assertBrokerRefusalRelayed(t *testing.T, rec *httptest.ResponseRecorder, status int, code, message string) {
	t.Helper()
	require.Equal(t, status, rec.Code, rec.Body.String())
	var resp ErrorResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.Equal(t, code, resp.Error.Code)
	assert.Equal(t, message, resp.Error.Message)
	assert.Empty(t, resp.Error.Details, "broker start markers are not relayed")
}

// submitEnvWithFinalizeErr submits env for a provisioning agent whose
// finalize-env dispatch fails with err.
func submitEnvWithFinalizeErr(t *testing.T, name string, err error) *httptest.ResponseRecorder {
	t.Helper()
	rec, _ := submitEnvWithFinalizeErrAgent(t, name, err)
	return rec
}

// submitEnvWithFinalizeErrAgent is submitEnvWithFinalizeErr that also
// returns the agent row as stored after the request.
func submitEnvWithFinalizeErrAgent(t *testing.T, name string, err error) (*httptest.ResponseRecorder, *store.Agent) {
	t.Helper()
	srv, s, project := setupCreateAgentServer(t, &finalizeEnvErrDispatcher{err: err})
	agent := &store.Agent{
		ID:              tid("agent-" + name),
		Name:            name,
		Slug:            name,
		ProjectID:       project.ID,
		RuntimeBrokerID: project.DefaultRuntimeBrokerID,
		Phase:           string(state.PhaseProvisioning),
	}
	require.NoError(t, s.CreateAgent(context.Background(), agent))
	rec := doRequest(t, srv, http.MethodPost, "/api/v1/projects/"+project.ID+"/agents/"+name+"/env",
		SubmitEnvRequest{Env: map[string]string{"API_KEY": "v"}})
	got, gerr := s.GetAgent(context.Background(), agent.ID)
	require.NoError(t, gerr)
	return rec, got
}

func truncateForLog(s string) string {
	if len(s) > 512 {
		return s[:512] + "..."
	}
	return s
}

const repairStaleHash = "stale-db-hash"

// setupHarnessConfigScopeTest creates a test server with alice (hub member,
// project owner) and carol (hub member, neither owner nor project member —
// the principal the pre-fix bug affected).
func setupHarnessConfigScopeTest(t *testing.T) (srv *Server, s store.Store, alice, carol *store.User, project *store.Project) {
	t.Helper()
	srv, s = testServer(t)
	ctx := context.Background()

	alice = &store.User{
		ID: tid("hcscope-alice"), Email: "hcscope-alice@test.com", DisplayName: "Alice",
		Role: store.UserRoleMember, Status: "active", Created: time.Now(),
	}
	require.NoError(t, s.CreateUser(ctx, alice))
	ensureHubMembership(ctx, s, alice.ID)

	carol = createNamedTestUser(t, s, "hcscope-carol", store.UserRoleMember)
	ensureHubMembership(ctx, s, carol.ID)

	project = &store.Project{
		ID: tid("hcscope-project"), Name: "Harness Config Project", Slug: "hcscope-project",
		OwnerID: alice.ID, CreatedBy: alice.ID, Created: time.Now(), Updated: time.Now(),
	}
	require.NoError(t, s.CreateProject(ctx, project))
	srv.seedProjectCreatorMembership(ctx, project)

	return srv, s, alice, carol, project
}

// createAuthzTestHarnessConfig inserts a scope-parameterized harness config
// directly into the store, mirroring createAuthzTestTemplate.
func createAuthzTestHarnessConfig(t *testing.T, s store.Store, name, scope, scopeID, ownerID string) *store.HarnessConfig {
	t.Helper()
	hc := &store.HarnessConfig{
		ID:          api.NewUUID(),
		Name:        name,
		Slug:        api.Slugify(name),
		Harness:     "claude",
		Scope:       scope,
		ScopeID:     scopeID,
		OwnerID:     ownerID,
		Status:      store.HarnessConfigStatusActive,
		StoragePath: fmt.Sprintf("harness-configs/%s/%s", scope, api.Slugify(name)),
		Config:      &store.HarnessConfigData{Image: "example/claude:latest"},
		Created:     time.Now(),
		Updated:     time.Now(),
	}
	require.NoError(t, s.CreateHarnessConfig(context.Background(), hc))
	return hc
}

func (f *failingUploadStorage) Upload(context.Context, string, io.Reader, storage.UploadOptions) (*storage.Object, error) {
	return nil, errors.New("upload failed (test)")
}

func (f *fakeImageManager) ImageExists(_ context.Context, image string) (bool, error) {
	return f.exists[image], nil
}
func (f *fakeImageManager) PullImage(context.Context, string) error   { return nil }
func (f *fakeImageManager) RemoveImage(context.Context, string) error { return nil }
func (f *fakeImageManager) Name() string                              { return "Podman" }

type installFile struct {
	path string
	data []byte
}

func (h *levelCapturingHandler) Enabled(context.Context, slog.Level) bool { return true }

func (h *levelCapturingHandler) Handle(_ context.Context, r slog.Record) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.records = append(h.records, r.Clone())
	return nil
}

func (h *levelCapturingHandler) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h *levelCapturingHandler) WithGroup(string) slog.Handler      { return h }

// harnessNotFoundRecords returns the captured records for the
// harness-config-not-found message.
func (h *levelCapturingHandler) harnessNotFoundRecords() []slog.Record {
	h.mu.Lock()
	defer h.mu.Unlock()
	var out []slog.Record
	for _, r := range h.records {
		if strings.Contains(r.Message, "harness config not found") {
			out = append(out, r)
		}
	}
	return out
}

// recordsContaining returns captured log records whose message contains sub.
func (h *levelCapturingHandler) recordsContaining(sub string) []slog.Record {
	h.mu.Lock()
	defer h.mu.Unlock()
	var out []slog.Record
	for _, r := range h.records {
		if strings.Contains(r.Message, sub) {
			out = append(out, r)
		}
	}
	return out
}

// hubDefaultTemplateWarnings returns the records emitted by
// warnHubDefaultTemplateUnusable.
func (h *levelCapturingHandler) hubDefaultTemplateWarnings() []slog.Record {
	return h.recordsContaining("hub operational default_template is unusable")
}

// hostedInstallBootstrap runs the hosted startup bootstrap with the options
// cmd/server_foreground.go uses.
func hostedInstallBootstrap(t *testing.T, srv *Server) {
	t.Helper()
	require.NoError(t, srv.BootstrapBundledResources(context.Background(), BootstrapOptions{
		RepairStorage:   true,
		OverwritePolicy: OverwriteBuiltinManaged,
	}))
}

// finalizeEnvErrDispatcher answers finalize-env with err.
type finalizeEnvErrDispatcher struct {
	createAgentDispatcher
	err error
}

func (d *finalizeEnvErrDispatcher) DispatchFinalizeEnv(context.Context, *store.Agent, map[string]string) (*CreateDispatchResult, error) {
	return nil, d.err
}
