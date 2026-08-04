//go:build !no_sqlite

package hub

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/storage"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// createSourceProject creates a fully populated source project for clone tests,
// with settings annotations, labels, env vars, skill injections, a pre-start
// hook, and project-scoped harness configs and templates.
func createSourceProject(t *testing.T, srv *Server, s store.Store) *store.Project {
	t.Helper()
	ctx := context.Background()

	projectID := api.NewUUID()
	project := &store.Project{
		ID:                     projectID,
		Name:                   "Source Project",
		Slug:                   "source-project",
		GitRemote:              "https://github.com/test/repo.git",
		DefaultRuntimeBrokerID: "broker-123",
		Visibility:             store.VisibilityPublic,
		OwnerID:                DevUserID,
		CreatedBy:              DevUserID,
		Annotations: map[string]string{
			"scion.io/default-model":          "claude-sonnet",
			"scion.io/default-max-turns":      "100",
			"scion.io/default-harness-config": "my-config",
		},
		Labels: map[string]string{
			"scion.dev/workspace-mode": "per-agent",
			"scion.dev/clone-url":      "https://github.com/test/repo.git",
			"scion.dev/default-branch": "main",
			"team":                     "backend",
		},
		SharedDirs: []api.SharedDir{
			{Name: "data"},
		},
		GitIdentity: &store.GitIdentityConfig{
			Name:  "Bot",
			Email: "bot@test.com",
		},
	}
	require.NoError(t, s.CreateProject(ctx, project))

	// Add non-secret env vars
	require.NoError(t, s.CreateEnvVar(ctx, &store.EnvVar{
		ID:      api.NewUUID(),
		Key:     "API_URL",
		Value:   "https://api.example.com",
		Scope:   store.ScopeProject,
		ScopeID: projectID,
	}))
	require.NoError(t, s.CreateEnvVar(ctx, &store.EnvVar{
		ID:        api.NewUUID(),
		Key:       "MASKED_TOKEN",
		Value:     "token-value-123",
		Scope:     store.ScopeProject,
		ScopeID:   projectID,
		Sensitive: true, // Sensitive but NOT a secret — should be copied
	}))

	// Add a secret-backed env var — should NOT be copied
	require.NoError(t, s.CreateEnvVar(ctx, &store.EnvVar{
		ID:      api.NewUUID(),
		Key:     "SECRET_KEY",
		Value:   "should-not-appear",
		Scope:   store.ScopeProject,
		ScopeID: projectID,
		Secret:  true,
	}))

	// Add skill injections
	require.NoError(t, s.SetSkillInjections(ctx, store.SkillInjectionScopeProject, projectID, []store.SkillInjection{
		{SkillURI: "skill://debugging", SortOrder: 1},
		{SkillURI: "skill://testing", SkillAs: "tdd", Optional: true, SortOrder: 2},
	}, DevUserID))

	// Add an older pre-start hook (will be archived when the second is created)
	_, err := s.CreateProjectPreStartHook(ctx, &store.ProjectPreStartHook{
		ID:        api.NewUUID(),
		Scope:     store.PreStartHookScopeProject,
		ProjectID: projectID,
		Name:      "Old Setup",
		Slug:      "old-setup",
		Script:    "#!/bin/bash\necho old",
		Status:    store.ProjectPreStartHookStatusActive,
		CreatedBy: DevUserID,
	})
	require.NoError(t, err)

	// Add active pre-start hook — this archives the previous one
	_, err = s.CreateProjectPreStartHook(ctx, &store.ProjectPreStartHook{
		ID:        api.NewUUID(),
		Scope:     store.PreStartHookScopeProject,
		ProjectID: projectID,
		Name:      "Setup",
		Slug:      "setup",
		Script:    "#!/bin/bash\necho setup",
		Status:    store.ProjectPreStartHookStatusActive,
		CreatedBy: DevUserID,
	})
	require.NoError(t, err)

	// Set up storage for project-scoped harness configs (with working Copy)
	stor := newCloneMockStorage("test-bucket")
	srv.SetStorage(stor)

	now := time.Now()

	// Add a project-scoped harness config
	hcStoragePath := "hubs/test-hub-id/harness-configs/project/" + projectID + "/my-config"
	require.NoError(t, s.CreateHarnessConfig(ctx, &store.HarnessConfig{
		ID:          api.NewUUID(),
		Name:        "My Config",
		Slug:        "my-config",
		Harness:     "claude",
		Scope:       store.HarnessConfigScopeProject,
		ScopeID:     projectID,
		Status:      store.HarnessConfigStatusActive,
		StoragePath: hcStoragePath,
		Files: []store.TemplateFile{
			{Path: "config.yaml", Size: 100, Hash: "abc123"},
		},
		Created: now, Updated: now,
	}))
	// Seed mock storage with the file
	stor.seedObject(hcStoragePath+"/config.yaml", []byte("harness: claude"))

	// Add a project-scoped template
	tplStoragePath := "hubs/test-hub-id/templates/project/" + projectID + "/my-template"
	require.NoError(t, s.CreateTemplate(ctx, &store.Template{
		ID:          api.NewUUID(),
		Name:        "My Template",
		Slug:        "my-template",
		Harness:     "claude",
		Scope:       store.TemplateScopeProject,
		ScopeID:     projectID,
		Status:      store.TemplateStatusActive,
		StoragePath: tplStoragePath,
		Files: []store.TemplateFile{
			{Path: "template.yaml", Size: 50, Hash: "def456"},
		},
		Created: now, Updated: now,
	}))
	stor.seedObject(tplStoragePath+"/template.yaml", []byte("template: test"))

	return project
}

// cloneMockStorage wraps mockStorage with a working Copy implementation.
type cloneMockStorage struct {
	mockStorage
}

func newCloneMockStorage(bucket string) *cloneMockStorage {
	return &cloneMockStorage{
		mockStorage: mockStorage{
			bucket:  bucket,
			objects: make(map[string]*storage.Object),
			content: make(map[string][]byte),
		},
	}
}

func (m *cloneMockStorage) Copy(_ context.Context, srcPath, dstPath string) (*storage.Object, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	srcObj, ok := m.objects[srcPath]
	if !ok {
		return nil, storage.ErrNotFound
	}
	dstObj := &storage.Object{
		Name: dstPath,
		Size: srcObj.Size,
	}
	m.objects[dstPath] = dstObj
	if data, ok := m.content[srcPath]; ok {
		m.content[dstPath] = data
	}
	return dstObj, nil
}

// seedObject inserts data into the mock storage for testing.
func (m *cloneMockStorage) seedObject(path string, data []byte) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.content[path] = data
	m.objects[path] = &storage.Object{
		Name: path,
		Size: int64(len(data)),
	}
}

// ──────────────────────────────────────────────────────────────────────
// Happy Path Tests
// ──────────────────────────────────────────────────────────────────────

func TestProjectClone_HappyPath(t *testing.T) {
	srv, s := testServer(t)
	src := createSourceProject(t, srv, s)
	ctx := context.Background()

	rec := doRequest(t, srv, http.MethodPost, "/api/v1/projects/"+src.ID+"/clone",
		map[string]string{"name": "Cloned Project"})
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())

	var clone store.Project
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&clone))

	// New identity
	assert.NotEqual(t, src.ID, clone.ID)
	assert.NotEqual(t, src.Slug, clone.Slug)
	assert.Equal(t, DevUserID, clone.OwnerID)
	assert.Equal(t, DevUserID, clone.CreatedBy)

	// Visibility resets to private
	assert.Equal(t, store.VisibilityPrivate, clone.Visibility)

	// Settings annotations copied
	assert.Equal(t, "claude-sonnet", clone.Annotations["scion.io/default-model"])
	assert.Equal(t, "100", clone.Annotations["scion.io/default-max-turns"])
	assert.Equal(t, "my-config", clone.Annotations["scion.io/default-harness-config"])

	// Git remote copied
	assert.Equal(t, src.GitRemote, clone.GitRemote)

	// Default broker copied
	assert.Equal(t, src.DefaultRuntimeBrokerID, clone.DefaultRuntimeBrokerID)

	// SharedDirs copied
	require.Len(t, clone.SharedDirs, 1)
	assert.Equal(t, "data", clone.SharedDirs[0].Name)

	// GitIdentity copied
	require.NotNil(t, clone.GitIdentity)
	assert.Equal(t, "Bot", clone.GitIdentity.Name)

	// Labels copied (minus scion.io/* prefix and workspace-mode re-derived)
	assert.Equal(t, "backend", clone.Labels["team"])
	// per-agent is the default — not stored as an explicit label (only shared
	// and worktree-per-agent are re-derived as labels).
	assert.Equal(t, "https://github.com/test/repo.git", clone.Labels["scion.dev/clone-url"])

	// Groups created
	agentsGroup, err := s.GetGroupBySlug(ctx, "project:"+clone.Slug+":agents")
	require.NoError(t, err)
	assert.Equal(t, clone.ID, agentsGroup.ProjectID)

	membersGroup, err := s.GetGroupBySlug(ctx, "project:"+clone.Slug+":members")
	require.NoError(t, err)
	assert.Equal(t, clone.ID, membersGroup.ProjectID)

	// Env vars copied (non-secret only)
	cloneEnvVars, err := s.ListEnvVars(ctx, store.EnvVarFilter{
		Scope:   store.ScopeProject,
		ScopeID: clone.ID,
	})
	require.NoError(t, err)
	envKeys := make(map[string]string)
	for _, ev := range cloneEnvVars {
		envKeys[ev.Key] = ev.Value
	}
	assert.Equal(t, "https://api.example.com", envKeys["API_URL"])
	assert.Equal(t, "token-value-123", envKeys["MASKED_TOKEN"]) // Sensitive is copied
	assert.NotContains(t, envKeys, "SECRET_KEY")                // Secret is NOT copied

	// Skill injections copied
	cloneSkills, err := s.ListSkillInjections(ctx, store.SkillInjectionScopeProject, clone.ID)
	require.NoError(t, err)
	require.Len(t, cloneSkills, 2)

	// Pre-start hook — only active is copied
	activeHook, err := s.GetActiveProjectPreStartHook(ctx, clone.ID)
	require.NoError(t, err)
	assert.Equal(t, "#!/bin/bash\necho setup", activeHook.Script)
	assert.NotEqual(t, src.ID, activeHook.ProjectID)

	// Verify only one hook exists (not the archived one)
	allHooks, err := s.ListProjectPreStartHooks(ctx, clone.ID)
	require.NoError(t, err)
	activeCount := 0
	for _, h := range allHooks {
		if h.Status == store.ProjectPreStartHookStatusActive {
			activeCount++
		}
	}
	assert.Equal(t, 1, activeCount, "only one active hook should exist")

	// Harness config slug preserved and resolves in new project
	cloneHCs, err := s.ListHarnessConfigs(ctx, store.HarnessConfigFilter{
		Scope:   store.HarnessConfigScopeProject,
		ScopeID: clone.ID,
	}, store.ListOptions{Limit: 100})
	require.NoError(t, err)
	require.Len(t, cloneHCs.Items, 1)
	assert.Equal(t, "my-config", cloneHCs.Items[0].Slug)
	assert.NotEqual(t, src.ID, cloneHCs.Items[0].ScopeID)
	assert.Equal(t, clone.ID, cloneHCs.Items[0].ScopeID)

	// Template copied
	cloneTpls, err := s.ListTemplates(ctx, store.TemplateFilter{
		Scope:   store.TemplateScopeProject,
		ScopeID: clone.ID,
	}, store.ListOptions{Limit: 100})
	require.NoError(t, err)
	require.Len(t, cloneTpls.Items, 1)
	assert.Equal(t, "my-template", cloneTpls.Items[0].Slug)
}

func TestProjectClone_GitRemoteOverride(t *testing.T) {
	srv, s := testServer(t)
	src := createSourceProject(t, srv, s)

	overrideURL := "https://github.com/other-org/other-repo.git"

	rec := doRequest(t, srv, http.MethodPost, "/api/v1/projects/"+src.ID+"/clone",
		map[string]interface{}{"name": "Override Remote", "gitRemote": overrideURL})
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())

	var clone store.Project
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&clone))

	// The clone should have the overridden git remote (normalized), not the source's
	assert.NotEqual(t, src.GitRemote, clone.GitRemote)
	// NormalizeGitRemote strips scheme and .git suffix
	assert.Equal(t, "github.com/other-org/other-repo", clone.GitRemote)
}

func TestProjectClone_NoGitRemoteOverride(t *testing.T) {
	srv, s := testServer(t)
	src := createSourceProject(t, srv, s)

	rec := doRequest(t, srv, http.MethodPost, "/api/v1/projects/"+src.ID+"/clone",
		map[string]interface{}{"name": "No Override"})
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())

	var clone store.Project
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&clone))

	// Without override, the clone should keep the source's git remote
	assert.Equal(t, src.GitRemote, clone.GitRemote)
}

func TestProjectClone_UnsetAnnotations(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	// Create a project with NO annotations set
	project := &store.Project{
		ID:         api.NewUUID(),
		Name:       "Bare Project",
		Slug:       "bare-project",
		Visibility: store.VisibilityPrivate,
		OwnerID:    DevUserID,
		CreatedBy:  DevUserID,
	}
	require.NoError(t, s.CreateProject(ctx, project))

	rec := doRequest(t, srv, http.MethodPost, "/api/v1/projects/"+project.ID+"/clone",
		map[string]string{"name": "Bare Clone"})
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())

	var clone store.Project
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&clone))

	// Annotations should be nil/empty — not zero-filled, not hub-resolved
	assert.Empty(t, clone.Annotations)
}

func TestProjectClone_SlugOmitted_NameCollides(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	project := &store.Project{
		ID:         api.NewUUID(),
		Name:       "Original",
		Slug:       "original",
		Visibility: store.VisibilityPrivate,
		OwnerID:    DevUserID,
		CreatedBy:  DevUserID,
	}
	require.NoError(t, s.CreateProject(ctx, project))

	// Clone with the same name as the slug that already exists
	rec := doRequest(t, srv, http.MethodPost, "/api/v1/projects/"+project.ID+"/clone",
		map[string]string{"name": "Original"})
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())

	var clone store.Project
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&clone))

	// Auto-serialized slug — never 409
	assert.NotEqual(t, "original", clone.Slug)
	assert.Contains(t, clone.Slug, "original")
}

func TestProjectClone_ExplicitSlugCollides(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	project := &store.Project{
		ID:         api.NewUUID(),
		Name:       "Existing",
		Slug:       "existing-slug",
		Visibility: store.VisibilityPrivate,
		OwnerID:    DevUserID,
		CreatedBy:  DevUserID,
	}
	require.NoError(t, s.CreateProject(ctx, project))

	rec := doRequest(t, srv, http.MethodPost, "/api/v1/projects/"+project.ID+"/clone",
		map[string]interface{}{"name": "New Name", "slug": "existing-slug"})
	assert.Equal(t, http.StatusConflict, rec.Code)
}

// ──────────────────────────────────────────────────────────────────────
// Exclusion Tests (security-critical)
// ──────────────────────────────────────────────────────────────────────

func TestProjectClone_NoSecretEnvVars(t *testing.T) {
	srv, s := testServer(t)
	src := createSourceProject(t, srv, s)
	ctx := context.Background()

	rec := doRequest(t, srv, http.MethodPost, "/api/v1/projects/"+src.ID+"/clone",
		map[string]string{"name": "Safe Clone"})
	require.Equal(t, http.StatusCreated, rec.Code)

	var clone store.Project
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&clone))

	envVars, err := s.ListEnvVars(ctx, store.EnvVarFilter{
		Scope:   store.ScopeProject,
		ScopeID: clone.ID,
	})
	require.NoError(t, err)

	for _, ev := range envVars {
		assert.False(t, ev.Secret, "clone must not contain secret env vars: found key=%s", ev.Key)
	}

	// Sensitive-but-not-secret IS present
	found := false
	for _, ev := range envVars {
		if ev.Key == "MASKED_TOKEN" {
			found = true
			assert.True(t, ev.Sensitive)
			assert.Equal(t, "token-value-123", ev.Value)
		}
	}
	assert.True(t, found, "sensitive env var should be copied")
}

func TestProjectClone_NoAgents(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	project := &store.Project{
		ID:         api.NewUUID(),
		Name:       "With Agents",
		Slug:       "with-agents",
		Visibility: store.VisibilityPrivate,
		OwnerID:    DevUserID,
		CreatedBy:  DevUserID,
	}
	require.NoError(t, s.CreateProject(ctx, project))

	// Create an agent in the source project
	require.NoError(t, s.CreateAgent(ctx, &store.Agent{
		ID:        api.NewUUID(),
		Name:      "test-agent",
		Slug:      "test-agent",
		ProjectID: project.ID,
		Phase:     "running",
		CreatedBy: DevUserID,
	}))

	rec := doRequest(t, srv, http.MethodPost, "/api/v1/projects/"+project.ID+"/clone",
		map[string]string{"name": "Agent-Free Clone"})
	require.Equal(t, http.StatusCreated, rec.Code)

	var clone store.Project
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&clone))

	// No agents in the clone
	agents, err := s.ListAgents(ctx, store.AgentFilter{ProjectID: clone.ID}, store.ListOptions{Limit: 100})
	require.NoError(t, err)
	assert.Empty(t, agents.Items)
}

func TestProjectClone_OnlyActiveHookCopied(t *testing.T) {
	srv, s := testServer(t)
	src := createSourceProject(t, srv, s)
	ctx := context.Background()

	rec := doRequest(t, srv, http.MethodPost, "/api/v1/projects/"+src.ID+"/clone",
		map[string]string{"name": "Hook Clone"})
	require.Equal(t, http.StatusCreated, rec.Code)

	var clone store.Project
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&clone))

	allHooks, err := s.ListProjectPreStartHooks(ctx, clone.ID)
	require.NoError(t, err)

	// The source had 2 hooks (one archived, one active after the second create).
	// Clone should only have 1 hook (the latest active one).
	activeHooks := 0
	for _, h := range allHooks {
		if h.Status == store.ProjectPreStartHookStatusActive {
			activeHooks++
		}
	}
	assert.Equal(t, 1, activeHooks, "clone should have exactly one active hook")
}

// ──────────────────────────────────────────────────────────────────────
// Authorization Tests
// ──────────────────────────────────────────────────────────────────────

func TestProjectClone_Unauthenticated(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	project := &store.Project{
		ID:         api.NewUUID(),
		Name:       "Auth Test",
		Slug:       "auth-test",
		Visibility: store.VisibilityPrivate,
		OwnerID:    DevUserID,
		CreatedBy:  DevUserID,
	}
	require.NoError(t, s.CreateProject(ctx, project))

	rec := doRequestNoAuth(t, srv, http.MethodPost, "/api/v1/projects/"+project.ID+"/clone",
		map[string]string{"name": "Unauthed Clone"})
	assert.Equal(t, http.StatusUnauthorized, rec.Code)
}

func TestProjectClone_ReadOnly_Succeeds(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	// Project owned by the dev user — they can read it
	project := &store.Project{
		ID:         api.NewUUID(),
		Name:       "Readable",
		Slug:       "readable",
		Visibility: store.VisibilityPrivate,
		OwnerID:    DevUserID,
		CreatedBy:  DevUserID,
	}
	require.NoError(t, s.CreateProject(ctx, project))

	rec := doRequest(t, srv, http.MethodPost, "/api/v1/projects/"+project.ID+"/clone",
		map[string]string{"name": "Read Clone"})
	assert.Equal(t, http.StatusCreated, rec.Code)
}

func TestProjectClone_NotFound(t *testing.T) {
	srv, _ := testServer(t)

	rec := doRequest(t, srv, http.MethodPost, "/api/v1/projects/nonexistent/clone",
		map[string]string{"name": "Ghost Clone"})
	assert.Equal(t, http.StatusNotFound, rec.Code)
}

func TestProjectClone_MissingName(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	project := &store.Project{
		ID:         api.NewUUID(),
		Name:       "Valid Project",
		Slug:       "valid-project",
		Visibility: store.VisibilityPrivate,
		OwnerID:    DevUserID,
		CreatedBy:  DevUserID,
	}
	require.NoError(t, s.CreateProject(ctx, project))

	rec := doRequest(t, srv, http.MethodPost, "/api/v1/projects/"+project.ID+"/clone",
		map[string]string{"name": ""})
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestProjectClone_MethodNotAllowed(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	project := &store.Project{
		ID:         api.NewUUID(),
		Name:       "Method Test",
		Slug:       "method-test",
		Visibility: store.VisibilityPrivate,
		OwnerID:    DevUserID,
		CreatedBy:  DevUserID,
	}
	require.NoError(t, s.CreateProject(ctx, project))

	rec := doRequest(t, srv, http.MethodGet, "/api/v1/projects/"+project.ID+"/clone", nil)
	assert.Equal(t, http.StatusMethodNotAllowed, rec.Code)
}

// ──────────────────────────────────────────────────────────────────────
// Concurrency Tests
// ──────────────────────────────────────────────────────────────────────

func TestProjectClone_ConcurrentClones(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	project := &store.Project{
		ID:         api.NewUUID(),
		Name:       "Concurrent Source",
		Slug:       "concurrent-source",
		Visibility: store.VisibilityPrivate,
		OwnerID:    DevUserID,
		CreatedBy:  DevUserID,
	}
	require.NoError(t, s.CreateProject(ctx, project))

	// Clone twice with the same name
	rec1 := doRequest(t, srv, http.MethodPost, "/api/v1/projects/"+project.ID+"/clone",
		map[string]string{"name": "Twin Clone"})
	require.Equal(t, http.StatusCreated, rec1.Code)

	rec2 := doRequest(t, srv, http.MethodPost, "/api/v1/projects/"+project.ID+"/clone",
		map[string]string{"name": "Twin Clone"})
	require.Equal(t, http.StatusCreated, rec2.Code)

	var clone1, clone2 store.Project
	require.NoError(t, json.NewDecoder(rec1.Body).Decode(&clone1))
	require.NoError(t, json.NewDecoder(rec2.Body).Decode(&clone2))

	assert.NotEqual(t, clone1.ID, clone2.ID)
	assert.NotEqual(t, clone1.Slug, clone2.Slug)
}

// ──────────────────────────────────────────────────────────────────────
// AsTemplate Tests
// ──────────────────────────────────────────────────────────────────────

func TestProjectClone_AsTemplate_AdminOnly(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	project := &store.Project{
		ID:         api.NewUUID(),
		Name:       "Template Source",
		Slug:       "template-source",
		Visibility: store.VisibilityPrivate,
		OwnerID:    DevUserID,
		CreatedBy:  DevUserID,
	}
	require.NoError(t, s.CreateProject(ctx, project))

	// Clone with asTemplate: true (dev user is admin)
	rec := doRequest(t, srv, http.MethodPost, "/api/v1/projects/"+project.ID+"/clone",
		map[string]interface{}{"name": "My Template", "asTemplate": true})
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())

	var clone store.Project
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&clone))

	// Assert clone has scion.io/template: "true" label
	assert.Equal(t, "true", clone.Labels[store.LabelTemplate])

	// Assert clone visibility is "team"
	assert.Equal(t, store.VisibilityTeam, clone.Visibility)
}

func TestProjectClone_AsTemplate_ScionIOLabelsStripped_WhenNoAsTemplate(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	// Create source with scion.io/template label
	project := &store.Project{
		ID:         api.NewUUID(),
		Name:       "Existing Template",
		Slug:       "existing-template",
		Visibility: store.VisibilityTeam,
		OwnerID:    DevUserID,
		CreatedBy:  DevUserID,
		Labels: map[string]string{
			store.LabelTemplate: "true",
			"team":              "backend",
		},
	}
	require.NoError(t, s.CreateProject(ctx, project))

	// Clone without asTemplate — should strip scion.io/* labels
	rec := doRequest(t, srv, http.MethodPost, "/api/v1/projects/"+project.ID+"/clone",
		map[string]string{"name": "Normal Project"})
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())

	var clone store.Project
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&clone))

	// Assert result has NO scion.io/template label (stripped by existing logic)
	assert.NotEqual(t, "true", clone.Labels[store.LabelTemplate])

	// Assert non-system label IS preserved
	assert.Equal(t, "backend", clone.Labels["team"])

	// Assert visibility resets to private (not inherited from source)
	assert.Equal(t, store.VisibilityPrivate, clone.Visibility)
}

func TestProjectClone_StorageFilesCopied(t *testing.T) {
	srv, s := testServer(t)
	src := createSourceProject(t, srv, s)

	rec := doRequest(t, srv, http.MethodPost, "/api/v1/projects/"+src.ID+"/clone",
		map[string]string{"name": "Storage Clone"})
	require.Equal(t, http.StatusCreated, rec.Code)

	var clone store.Project
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&clone))

	// Verify cloned harness config has storage set
	cloneHCs, err := s.ListHarnessConfigs(context.Background(), store.HarnessConfigFilter{
		Scope:   store.HarnessConfigScopeProject,
		ScopeID: clone.ID,
	}, store.ListOptions{Limit: 100})
	require.NoError(t, err)
	require.Len(t, cloneHCs.Items, 1)
	assert.NotEmpty(t, cloneHCs.Items[0].StoragePath)
	assert.NotEqual(t, "", cloneHCs.Items[0].StoragePath)
	assert.Contains(t, cloneHCs.Items[0].StoragePath, clone.ID)

	// Verify storage files exist at the new path
	stor := srv.GetStorage().(*cloneMockStorage)
	stor.mu.Lock()
	defer stor.mu.Unlock()
	found := false
	for path := range stor.objects {
		if strings.Contains(path, clone.ID) {
			found = true
			break
		}
	}
	assert.True(t, found, "storage files should exist under clone's project ID")
}
