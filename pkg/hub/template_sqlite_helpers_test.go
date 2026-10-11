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
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/storage"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/require"
)

// testTemplateBootstrapServer creates a hub Server backed by an in-memory
// SQLite store and a mock storage, suitable for template bootstrap tests.
func testTemplateBootstrapServer(t *testing.T) (*Server, store.Store, *mockStorage) {
	t.Helper()
	s, err := newTestStore(t, ":memory:")
	if err != nil {
		if strings.Contains(err.Error(), "sqlite driver not registered") {
			t.Skip("Skipping: sqlite driver not registered")
		}
		t.Fatalf("failed to create test store: %v", err)
	}

	cfg := DefaultServerConfig()
	srv, err := newTestHubServer(t, cfg, s)
	if err != nil {
		t.Fatalf("New() failed: %v", err)
	}

	stor := newMockStorage("test-bucket")
	srv.SetStorage(stor)

	return srv, s, stor
}

// makeTemplateDir creates a temp directory with template files and returns
// the parent templates directory. The template is created as a subdirectory
// named templateName.
func makeTemplateDir(t *testing.T, templateName string, files map[string]string) string {
	t.Helper()
	templatesDir := t.TempDir()
	templateDir := filepath.Join(templatesDir, templateName)
	for relPath, content := range files {
		full := filepath.Join(templateDir, relPath)
		if err := os.MkdirAll(filepath.Dir(full), 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(content), 0644); err != nil {
			t.Fatal(err)
		}
	}
	return templatesDir
}

// setupWorkspaceProject creates a server, store, project, and workspace temp dir
// linked via an embedded broker provider. Returns the server, store, project,
// and the workspace root path. Templates should be placed under the returned
// workspace root.
func setupWorkspaceProject(t *testing.T, projectName string) (*Server, store.Store, *store.Project, string) {
	t.Helper()
	srv, s, _ := testTemplateBootstrapServer(t)
	ctx := context.Background()

	workspaceRoot := t.TempDir()

	project := &store.Project{
		ID:        tid("project-ws-" + projectName),
		Name:      projectName,
		Slug:      projectName,
		GitRemote: "https://github.com/test/" + projectName,
	}
	if err := s.CreateProject(ctx, project); err != nil {
		t.Fatalf("failed to create project: %v", err)
	}

	brokerID := tid("broker-ws-" + projectName)
	broker := &store.RuntimeBroker{
		ID:       brokerID,
		Name:     "ws-broker",
		Slug:     "ws-broker",
		Endpoint: "http://localhost:9090",
		Status:   store.BrokerStatusOnline,
	}
	if err := s.CreateRuntimeBroker(ctx, broker); err != nil {
		t.Fatalf("failed to create broker: %v", err)
	}

	if err := s.AddProjectProvider(ctx, &store.ProjectProvider{
		ProjectID:  project.ID,
		BrokerID:   brokerID,
		BrokerName: broker.Name,
		LocalPath:  workspaceRoot,
		Status:     "online",
		LastSeen:   time.Now(),
	}); err != nil {
		t.Fatalf("failed to add project provider: %v", err)
	}

	srv.SetEmbeddedBrokerID(brokerID)

	return srv, s, project, workspaceRoot
}

type mockRoundTripper struct {
	roundTrip func(req *http.Request) (*http.Response, error)
}

// commitCfgBoth sets both harness-config keys. The broker's order (decision
// E5) resolves default_harness_config first, so the expected index is
// claude-web / claude. task and branch are per-agent fields the snapshot
// drops.
const (
	commitCfgOld  = "default_harness_config: gemini-web\n"
	commitCfgBoth = "harness_config: gemini-web\ndefault_harness_config: claude-web\nmodel: opus\ntask: per-agent task\nbranch: per-agent-branch\n"

	unusableHCConfig = "harness: claude\nprovisioner:\n  type: builtin\n"
)

// assertBothIndex checks the index derived from commitCfgBoth. The values
// are written out rather than computed with deriveTemplateIndex so a change
// to the derivation shows up here.
func assertBothIndex(t *testing.T, got *store.Template) {
	t.Helper()
	if got.DefaultHarnessConfig != "claude-web" {
		t.Errorf("DefaultHarnessConfig = %q, want %q (default_harness_config wins, E5)", got.DefaultHarnessConfig, "claude-web")
	}
	if got.Harness != "claude" {
		t.Errorf("Harness = %q, want %q", got.Harness, "claude")
	}
	if got.AgentConfig == nil {
		t.Fatal("AgentConfig = nil, want the parsed scion-agent.yaml")
	}
	if got.AgentConfig.Model != "opus" || got.AgentConfig.HarnessConfig != "gemini-web" || got.AgentConfig.DefaultHarnessConfig != "claude-web" {
		t.Errorf("AgentConfig = %+v, want model opus, harness_config gemini-web, default_harness_config claude-web", got.AgentConfig)
	}
	if got.AgentConfig.Task != "" || got.AgentConfig.Branch != "" {
		t.Errorf("AgentConfig kept per-agent fields: task %q, branch %q", got.AgentConfig.Task, got.AgentConfig.Branch)
	}
}

func newCommitTestStorage(t *testing.T) storage.Storage {
	t.Helper()
	stor, err := storage.NewLocal(storage.Config{Provider: storage.ProviderLocal, Bucket: "b", LocalPath: t.TempDir()})
	if err != nil {
		t.Fatalf("NewLocal: %v", err)
	}
	return stor
}

func newCommitTestServer(t *testing.T, stor storage.Storage) (*Server, store.Store) {
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
	srv.SetStorage(stor)
	return srv, s
}

// commitManifest builds a sorted manifest for files.
func commitManifest(files map[string]string) []store.TemplateFile {
	out := make([]store.TemplateFile, 0, len(files))
	for p, c := range files {
		out = append(out, store.TemplateFile{Path: p, Size: int64(len(c)), Hash: commitFileHash(c), Mode: "0644"})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out
}

func putObjects(t *testing.T, stor storage.Storage, storagePath string, files map[string]string) {
	t.Helper()
	for p, c := range files {
		if _, err := stor.Upload(context.Background(), storagePath+"/"+p, strings.NewReader(c), storage.UploadOptions{}); err != nil {
			t.Fatalf("upload %s: %v", p, err)
		}
	}
}

// stageObjects stages files the way a client's upload URLs do, under the
// template's content base, for a finalize to commit (ptone/scion#4221).
func stageObjects(t *testing.T, srv *Server, tmpl *store.Template, files map[string]string) {
	t.Helper()
	base := srv.templateContentBase(tmpl)
	for p, c := range files {
		if _, err := srv.GetStorage().Upload(context.Background(), templateStagedObjectPath(base, "test-upload", p), strings.NewReader(c), storage.UploadOptions{}); err != nil {
			t.Fatalf("stage %s: %v", p, err)
		}
	}
}

func objectExists(t *testing.T, stor storage.Storage, objectPath string) bool {
	t.Helper()
	ok, err := stor.Exists(context.Background(), objectPath)
	if err != nil {
		t.Fatalf("Exists(%s): %v", objectPath, err)
	}
	return ok
}

// seedCommittedTemplate creates a template with files through the commit path.
func seedCommittedTemplate(t *testing.T, srv *Server, name, scope, scopeID string, files map[string]string) *store.Template {
	t.Helper()
	slug := api.Slugify(name)
	tmpl := &store.Template{
		ID:          api.NewUUID(),
		Name:        name,
		Slug:        slug,
		Scope:       scope,
		ScopeID:     scopeID,
		ProjectID:   scopeID,
		Status:      store.TemplateStatusActive,
		StoragePath: storage.TemplateStoragePath(srv.HubID(), scope, scopeID, slug),
	}
	putObjects(t, srv.GetStorage(), tmpl.StoragePath, files)
	if err := srv.commitTemplateFiles(context.Background(), tmpl, commitManifest(files), commitOpts{create: true}); err != nil {
		t.Fatalf("seed commit: %v", err)
	}
	return tmpl
}

func writeTemplateDir(t *testing.T, parent, name string, files map[string]string) string {
	t.Helper()
	dir := filepath.Join(parent, name)
	for p, c := range files {
		fp := filepath.Join(dir, filepath.FromSlash(p))
		if err := os.MkdirAll(filepath.Dir(fp), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(fp, []byte(c), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func doTemplateRequest(t *testing.T, srv *Server, method, path, contentType string, body []byte) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, bytes.NewReader(body))
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	req.Header.Set("Authorization", "Bearer "+testDevToken)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	return w
}

func finalizeTemplate(t *testing.T, srv *Server, id string, files []store.TemplateFile) *httptest.ResponseRecorder {
	t.Helper()
	body, err := json.Marshal(FinalizeRequest{Manifest: &TemplateManifest{Version: "1.0", Harness: "ignored", Files: files}})
	if err != nil {
		t.Fatal(err)
	}
	return doTemplateRequest(t, srv, http.MethodPost, "/api/v1/templates/"+id+"/finalize", "application/json", body)
}

func mustStatus(t *testing.T, w *httptest.ResponseRecorder, want int) {
	t.Helper()
	if w.Code != want {
		t.Fatalf("status = %d, want %d: %s", w.Code, want, w.Body.String())
	}
}

// seedUncommittedTemplate creates a template row and its objects directly,
// bypassing the commit path, so a source the commit would refuse (a legacy
// row) can be cloned.
func seedUncommittedTemplate(t *testing.T, srv *Server, s store.Store, id, name, scope, scopeID string, files map[string]string) *store.Template {
	t.Helper()
	if id == "" {
		id = api.NewUUID()
	}
	slug := api.Slugify(name)
	tmpl := &store.Template{
		ID:          id,
		Name:        name,
		Slug:        slug,
		Harness:     "claude",
		Scope:       scope,
		ScopeID:     scopeID,
		ProjectID:   scopeID,
		Status:      store.TemplateStatusActive,
		StoragePath: storage.TemplateStoragePath(srv.HubID(), scope, scopeID, slug),
	}
	putObjects(t, srv.GetStorage(), tmpl.StoragePath, files)
	tmpl.Files = commitManifest(files)
	tmpl.ContentHash = computeContentHash(tmpl.Files)
	if err := s.CreateTemplate(context.Background(), tmpl); err != nil {
		t.Fatal(err)
	}
	return tmpl
}

// setTemplateContentForTest writes tmpl with its content columns, as only
// the commit path may in production (store.UpdateTemplate no longer writes
// them). Tests use it to plant drifted or stale content.
func setTemplateContentForTest(ctx context.Context, s store.Store, tmpl *store.Template) error {
	cur, err := s.GetTemplate(ctx, tmpl.ID)
	if err != nil {
		return err
	}
	return s.UpdateTemplateContent(ctx, tmpl, store.TemplateContentPrecondition{ContentHash: cur.ContentHash, Layout: cur.Layout})
}

// putBlobs stores files as blobs under the template's content base, as the
// file APIs do before they commit.
func putBlobs(t *testing.T, srv *Server, tmpl *store.Template, files map[string]string) {
	t.Helper()
	base := srv.templateContentBase(tmpl)
	for p, c := range files {
		hex, _ := templateBlobHex(commitHash(c))
		if _, err := srv.GetStorage().Upload(context.Background(), templateBlobPath(base, hex), strings.NewReader(c), storage.UploadOptions{}); err != nil {
			t.Fatalf("put blob %s: %v", p, err)
		}
	}
}

// contentMockStorage extends mockStorage to also store file content for
// Download support in template file handler tests.
type contentMockStorage struct {
	mockStorage
	content map[string][]byte
}

func newContentMockStorage(bucket string) *contentMockStorage {
	return &contentMockStorage{
		mockStorage: mockStorage{
			bucket:  bucket,
			objects: make(map[string]*storage.Object),
		},
		content: make(map[string][]byte),
	}
}

// testTemplateFileServer creates a Server with content-aware mock storage.
func testTemplateFileServer(t *testing.T) (*Server, store.Store, *contentMockStorage) {
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

// createTestTemplate creates a template in the store with the given files
// pre-populated in storage.
func createTestTemplate(t *testing.T, s store.Store, stor *contentMockStorage, files map[string]string) *store.Template {
	t.Helper()
	ctx := context.Background()

	tmpl := &store.Template{
		ID:            tid("tmpl-test-1"),
		Name:          "test-template",
		Slug:          "test-template",
		Harness:       "claude",
		Scope:         "global",
		Status:        store.TemplateStatusActive,
		StoragePath:   "templates/global/test-template",
		StorageBucket: "test-bucket",
		Updated:       time.Date(2026, 4, 3, 12, 0, 0, 0, time.UTC),
	}

	templateFiles := make([]store.TemplateFile, 0, len(files))
	for path, content := range files {
		objectPath := tmpl.StoragePath + "/" + path
		stor.content[objectPath] = []byte(content)
		stor.objects[objectPath] = &storage.Object{
			Name: objectPath,
			Size: int64(len(content)),
		}

		templateFiles = append(templateFiles, store.TemplateFile{
			Path: path,
			Size: int64(len(content)),
			Hash: "sha256:placeholder",
		})
	}
	tmpl.Files = templateFiles
	tmpl.ContentHash = computeContentHash(templateFiles)

	if err := s.CreateTemplate(ctx, tmpl); err != nil {
		t.Fatalf("failed to create test template: %v", err)
	}

	return tmpl
}

// templateMultipartRequest creates a multipart form request for template file upload tests.
func templateMultipartRequest(t *testing.T, templateID string, files map[string][]byte) *http.Request {
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

	req := httptest.NewRequest(http.MethodPost, "/api/v1/templates/"+templateID+"/files", &buf)
	req.Header.Set("Content-Type", writer.FormDataContentType())
	req.Header.Set("Authorization", "Bearer "+testDevToken)
	return req
}

// setupTemplateAuthzTest creates a test server with two users and a project.
// Alice is a hub member and project owner. Bob is NOT a hub member, so the
// seeded hub-member-read-all policy does not grant him read access.
func setupTemplateAuthzTest(t *testing.T) (srv *Server, s store.Store, alice, bob *store.User, project *store.Project) {
	t.Helper()

	srv, s = testServer(t)
	ctx := context.Background()

	alice = &store.User{
		ID:          tid("tpl-alice"),
		Email:       "tpl-alice@test.com",
		DisplayName: "Alice",
		Role:        store.UserRoleMember,
		Status:      "active",
		Created:     time.Now(),
	}
	require.NoError(t, s.CreateUser(ctx, alice))

	bob = &store.User{
		ID:          tid("tpl-bob"),
		Email:       "tpl-bob@test.com",
		DisplayName: "Bob",
		Role:        store.UserRoleMember,
		Status:      "active",
		Created:     time.Now(),
	}
	require.NoError(t, s.CreateUser(ctx, bob))

	ensureHubMembership(ctx, s, alice.ID)
	// Bob is intentionally NOT added to hub-members, so default-deny applies.

	project = &store.Project{
		ID:        tid("tpl-project"),
		Name:      "Template Project",
		Slug:      "template-project",
		OwnerID:   alice.ID,
		CreatedBy: alice.ID,
		Created:   time.Now(),
		Updated:   time.Now(),
	}
	require.NoError(t, s.CreateProject(ctx, project))
	srv.seedProjectCreatorMembership(ctx, project)

	return srv, s, alice, bob, project
}

// createAuthzTestTemplate inserts a template directly into the store.
func createAuthzTestTemplate(t *testing.T, s store.Store, name, scope, scopeID, ownerID string) *store.Template {
	t.Helper()
	tpl := &store.Template{
		ID:          api.NewUUID(),
		Name:        name,
		Slug:        api.Slugify(name),
		Scope:       scope,
		ScopeID:     scopeID,
		OwnerID:     ownerID,
		Status:      "active",
		StoragePath: fmt.Sprintf("templates/%s/%s", scope, api.Slugify(name)),
		Created:     time.Now(),
		Updated:     time.Now(),
	}
	require.NoError(t, s.CreateTemplate(context.Background(), tpl))
	return tpl
}

// setupTemplateScopeTest builds on setupTemplateAuthzTest, adding carol: a
// hub member who is neither the resource owner nor a member of alice's
// project. Carol is the principal the pre-fix bug affected — unlike bob (not
// a hub member at all), carol's denial can only come from the scope boundary
// itself, not from missing hub membership.
func setupTemplateScopeTest(t *testing.T) (srv *Server, s store.Store, alice, carol *store.User, project *store.Project) {
	t.Helper()
	srv, s, alice, _, project = setupTemplateAuthzTest(t)
	carol = createNamedTestUser(t, s, "tplscope-carol", store.UserRoleMember)
	ensureHubMembership(context.Background(), s, carol.ID)
	return srv, s, alice, carol, project
}

// createScopeSuperAdmin creates a user with an explicit super-admin role
// binding. Unlike hub-admin, super-admin's permission set (allPermissionIDs,
// seed.go) is not curated per resource type — it is the only built-in role
// guaranteed to include template.read/template.list and
// harness_config.read/harness_config.list today, so it is the elevated-role
// control for this suite (hub-admin's curated permission list, unlike
// skill.read, was never extended to templates or harness configs — a
// separate, pre-existing product decision, not something this fix changes).
func createScopeSuperAdmin(t *testing.T, s store.Store, namePrefix string) *store.User {
	t.Helper()
	id := tid(namePrefix)
	email := namePrefix + "@test.com"
	// createTestUserWithRole (authz_candelegate_test.go) creates the user
	// record itself and binds the role; it does not return the user.
	createTestUserWithRole(t, s, id, email, store.UserRoleAdmin, store.SystemRoleSuperAdmin)
	return &store.User{ID: id, Email: email, DisplayName: email, Role: store.UserRoleAdmin, Status: "active"}
}

func (m *mockRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	return m.roundTrip(req)
}

func commitFileHash(content string) string {
	h := sha256.Sum256([]byte(content))
	return "sha256:" + hex.EncodeToString(h[:])
}

func (m *contentMockStorage) Upload(_ context.Context, objectPath string, reader io.Reader, opts storage.UploadOptions) (*storage.Object, error) {
	data, err := io.ReadAll(reader)
	if err != nil {
		return nil, err
	}
	obj := &storage.Object{
		Name:     objectPath,
		Size:     int64(len(data)),
		Metadata: opts.Metadata,
	}
	m.objects[objectPath] = obj
	m.content[objectPath] = data
	return obj, nil
}

func (m *contentMockStorage) Download(_ context.Context, objectPath string) (io.ReadCloser, *storage.Object, error) {
	data, ok := m.content[objectPath]
	if !ok {
		return nil, nil, storage.ErrNotFound
	}
	obj := m.objects[objectPath]
	return io.NopCloser(bytes.NewReader(data)), obj, nil
}

func (m *contentMockStorage) Delete(_ context.Context, objectPath string) error {
	delete(m.objects, objectPath)
	delete(m.content, objectPath)
	return nil
}

func (m *contentMockStorage) Exists(_ context.Context, objectPath string) (bool, error) {
	_, ok := m.content[objectPath]
	return ok, nil
}
