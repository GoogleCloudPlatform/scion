//go:build !no_sqlite

package hub

import (
	"context"
	"encoding/json"
	"fmt"
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

	// raceBarrier, when set, makes Copy block the calling goroutine until a
	// fixed number of distinct concurrent requests have all reached their
	// first Copy call — see copyBarrier in clone_concurrency_test.go. Nil by
	// default, so every other test using cloneMockStorage is unaffected.
	raceBarrier *copyBarrier
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

func (m *cloneMockStorage) Copy(ctx context.Context, srcPath, dstPath string) (*storage.Object, error) {
	if m.raceBarrier != nil {
		// Must run before the mutex below: every racing goroutine needs to
		// reach the barrier on its own, so none of them can be holding m.mu
		// while waiting. Identified by the request's raceRequestID rather
		// than dstPath: see copyBarrier's doc comment in
		// clone_concurrency_test.go for why.
		if id, ok := raceRequestIDFromContext(ctx); ok {
			m.raceBarrier.arrive(id)
		}
	}
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

// DeletePrefix actually removes matching keys, unlike the embedded
// mockStorage's no-op, so tests can observe whether a handler deleted a
// prefix it should not have (ptone/scion#1916 follow-up: clone failure
// cleanup must never remove a prefix the request did not itself create).
func (m *cloneMockStorage) DeletePrefix(_ context.Context, prefix string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for path := range m.objects {
		if strings.HasPrefix(path, prefix) {
			delete(m.objects, path)
			delete(m.content, path)
		}
	}
	return nil
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

// TestProjectClone_GitRemoteOverride_RederivesSourceLabels guards the OQ-1 bug
// (ptone/scion#2702): the template's scion.dev/clone-url label takes precedence
// over GitRemote, so a copied clone-url made a gitRemote override ineffective
// — agents still cloned the template's repository.
func TestProjectClone_GitRemoteOverride_RederivesSourceLabels(t *testing.T) {
	srv, s := testServer(t)
	src := createSourceProject(t, srv, s)
	ctx := context.Background()

	// Make the template's source labels clearly template-specific.
	src.Labels[store.LabelSourceURL] = "git@github.com:test/repo.git"
	src.Labels[store.LabelDefaultBranch] = "develop"
	require.NoError(t, s.UpdateProject(ctx, src))

	overrideURL := "git@github.com:other-org/other-repo.git"
	rec := doRequest(t, srv, http.MethodPost, "/api/v1/projects/"+src.ID+"/clone",
		map[string]interface{}{"name": "Override Labels", "gitRemote": overrideURL})
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())

	var clone store.Project
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&clone))

	assert.Equal(t, "github.com/other-org/other-repo", clone.GitRemote)
	assert.Equal(t, "https://github.com/other-org/other-repo.git", clone.Labels[store.LabelCloneURL])
	assert.Equal(t, overrideURL, clone.Labels[store.LabelSourceURL])
	assert.Equal(t, "main", clone.Labels[store.LabelDefaultBranch])
	// Unrelated labels are still copied.
	assert.Equal(t, "backend", clone.Labels["team"])

	// The persisted row matches the response.
	stored, err := s.GetProject(ctx, clone.ID)
	require.NoError(t, err)
	assert.Equal(t, "https://github.com/other-org/other-repo.git", stored.Labels[store.LabelCloneURL])

	// Agent create resolves the overridden repository, not the template's.
	agent := &store.Agent{ID: api.NewUUID(), AppliedConfig: &store.AgentAppliedConfig{}}
	srv.populateAgentConfig(ctx, agent, stored, nil)
	require.NotNil(t, agent.AppliedConfig.GitClone)
	assert.Equal(t, "https://github.com/other-org/other-repo.git", agent.AppliedConfig.GitClone.URL)
	assert.Equal(t, "main", agent.AppliedConfig.GitClone.Branch)
}

// TestProjectClone_GitRemoteOverride_SameRemoteKeepsLabels checks that an
// "override" naming the template's own repository (in any URL form) is not
// treated as a change: the template's clone-url and branch are kept.
func TestProjectClone_GitRemoteOverride_SameRemoteKeepsLabels(t *testing.T) {
	for _, remote := range []string{
		"git@github.com:test/repo.git",
		// An explicit default port names the same repository (r4).
		"https://github.com:443/test/repo",
		"github.com:443/test/repo",
	} {
		t.Run(remote, func(t *testing.T) {
			srv, s := testServer(t)
			src := createSourceProject(t, srv, s)
			ctx := context.Background()

			src.Labels[store.LabelDefaultBranch] = "develop"
			require.NoError(t, s.UpdateProject(ctx, src))

			rec := doRequest(t, srv, http.MethodPost, "/api/v1/projects/"+src.ID+"/clone",
				map[string]interface{}{"name": "Same Remote", "gitRemote": remote})
			require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())

			var clone store.Project
			require.NoError(t, json.NewDecoder(rec.Body).Decode(&clone))

			assert.Equal(t, "github.com/test/repo", clone.GitRemote)
			assert.Equal(t, "https://github.com/test/repo.git", clone.Labels[store.LabelCloneURL])
			assert.Equal(t, "develop", clone.Labels[store.LabelDefaultBranch])
			_, hasSource := clone.Labels[store.LabelSourceURL]
			assert.False(t, hasSource, "source-url must not be invented when the remote is unchanged")
		})
	}
}

// TestProjectClone_GitRemoteOverride_StripsCredentials checks that a token
// embedded in the override never reaches GitRemote or the readable git
// source labels, in the response or the persisted row.
func TestProjectClone_GitRemoteOverride_StripsCredentials(t *testing.T) {
	srv, s := testServer(t)
	src := createSourceProject(t, srv, s)

	rec := doRequest(t, srv, http.MethodPost, "/api/v1/projects/"+src.ID+"/clone",
		map[string]interface{}{
			"name":      "Token Override",
			"gitRemote": "https://x-access-token:ghp_SECRET@github.com/other-org/other-repo.git",
		})
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	assert.NotContains(t, rec.Body.String(), "ghp_SECRET")
	assert.NotContains(t, rec.Body.String(), "x-access-token")

	var clone store.Project
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&clone))
	assert.Equal(t, "github.com/other-org/other-repo", clone.GitRemote)
	assert.Equal(t, "https://github.com/other-org/other-repo.git", clone.Labels[store.LabelCloneURL])
	assert.Equal(t, "https://github.com/other-org/other-repo.git", clone.Labels[store.LabelSourceURL])

	stored, err := s.GetProject(context.Background(), clone.ID)
	require.NoError(t, err)
	for k, v := range stored.Labels {
		assert.NotContains(t, v, "ghp_SECRET", "label %s", k)
	}
	assert.NotContains(t, stored.GitRemote, "ghp_SECRET")
}

// TestProjectClone_GitRemoteOverride_RejectsNonGitURL checks that an override
// that is not a remote git URL is a 400 and creates nothing.
func TestProjectClone_GitRemoteOverride_RejectsNonGitURL(t *testing.T) {
	srv, s := testServer(t)
	src := createSourceProject(t, srv, s)
	ctx := context.Background()

	before, err := s.ListProjects(ctx, store.ProjectFilter{}, store.ListOptions{Limit: 1000})
	require.NoError(t, err)

	for _, remote := range []string{
		"/home/user/code/repo",
		"./repo",
		"../repo",
		"~/code/repo",
		"repo",
		"org/repo",
		"github.com",
		"https://github.com",
		"C:\\code\\repo",
		"file:///home/user/repo",
		"git@github.com",
		"alice:pw@github.com:org/repo",
		"alice@github.com:repo",
		"alice@github.com:/abs/path",
		"github.com:notaport/org/repo",
		"github.com:8443/repo",
		"localhost:8080/org/repo",
		// SCP without a login is not supported (documented in §5.3).
		"github.com:org/repo",
		// Unencoded '/' in the password: net/url cannot parse a host.
		"https://u:SECRET_P/w@github.com/org/repo",
		"https://u:SECRET_P/w@x@github.com/org/repo.git",
		"ssh://git:SECRET_P/w@github.com/org/repo.git",
		// A port then '@' in the path, and passwords with '/' that look
		// like (or almost like) a port: all ambiguous, all rejected
		// without storing anything (GCP#2368 review).
		"https://git.example.com:8443/org/repo@v1",
		"https://github.com:443/org/repo@v1",
		"https://[::1]:8443/org/repo@v1",
		"https://u:SECRET_P@git.example.com:8443/org/repo@v1",
		"https://u:8443/SECRET_P@github.com/org/repo",
		"https://u:0123/SECRET_P@github.com/org/repo",
		"https://u:/SECRET_P@github.com/org/repo",
		// '@' in the path would make credential stripping change the host.
		"https://github.com/org/x@evil.example/repo",
		"https://bad_host/org/repo",
		"https://[::1/org/repo",
		// '@' in the path must not swap the repository (r4).
		"https://github.com/org/repo@github.com/x",
		"https://u:SECRET_P@github.com/org/x@github.com/repo",
		"https://a/b@github.com/x",
		"git@github.com:org/repo@github.com/x",
		"github.com/org/repo@github.com/x",
		// Whitespace and control characters, in every form.
		"github.com/org/repo\nX",
		"git@github.com:org/repo\nX",
		"https://github.com/org/my repo",
		"https://github.com/org/repo\tx",
		"ssh://git@github.com/org/re\x00po",
		"https://github.com/org/r\u00a0epo",
		// Loose ports and hosts.
		"https://u:SECRET_P@github.com:/org/repo",
		"https://github.com:0443/org/repo",
		"https://github.com:0/org/repo",
		"https://github.com:65536/org/repo",
		"git.example.com:0443/team/repo",
		"https://-x.com/org/repo",
		"https://x-.example.com/org/repo",
		"git@-gitserver:org/repo",
		"-x.example.com/org/repo",
		// Dot and empty path segments, escaped or not, in every form (r5):
		// git would clone a different repository than GitRemote names.
		"https://github.com/org/../evil/repo",
		"https://github.com/org/%2e%2e/evil/repo",
		"https://github.com/org/%2E%2E/evil/repo",
		"https://github.com/./org/repo",
		"https://github.com//org/repo",
		"https://github.com/org//repo",
		"https://github.com/org/repo//",
		"ssh://git@github.com/org/../evil/repo.git",
		"git://github.com/org/./repo",
		"github.com/org/../evil/repo",
		"github.com/org/%2e%2e/evil/repo",
		"github.com//org/repo",
		"github.com/org//repo",
		"git@github.com:org/../evil/repo",
		"git@github.com:org/%2e%2e/evil/repo",
		"git@github.com:org//repo",
		"git@github.com:./org/repo",
		// Printable ASCII only (r5): format characters, homoglyphs and
		// non-ASCII hosts (IDN hosts must be punycode); escaped controls.
		"https://github.com/org/\u202erepo",
		"github.com/org/\u202erepo",
		"git@github.com:org/\u202erepo",
		"https://github.com/\u043erg/repo",
		"https://b\u00fccher.example/org/repo",
		"https://github.com/org/re%0Apo",
		"https://github.com/org/re%00po",
		"github.com/org/re%7Fpo",
		"git@github.com:org/re%1Fpo",
		// Web parity (#2713 r4 F1): %40 in the path, malformed escapes,
		// invalid IPv6, userinfo characters net/url rejects.
		"https://github.com/org/%40evil/repo",
		"github.com/org/re%40po/x",
		"git@github.com:org/%40x/repo",
		"https://github.com/org/re%zzpo",
		"https://github.com/org/repo%",
		"github.com/org/re%zpo",
		"git@github.com:org/repo%",
		"https://u:SECRET_%zz@github.com/org/repo",
		// %2F inside a segment, in every form (r6 R1).
		"https://github.com/a/o%2Fr",
		"https://github.com/org/o%2fr",
		"github.com/a/o%2Fr",
		"git@github.com:a/o%2Fr",
		// Characters outside the RFC 3986 path set, in every form (r6 R2).
		"https://github.com/org/r\\x",
		"github.com/org/r\\x",
		"git@github.com:org/r\\x",
		"https://github.com/org/r%5Cx",
		"https://github.com/org/re\"po",
		"https://github.com/org/re|po",
		"https://github.com/org/re^po",
		"https://github.com/org/re`po",
		"https://github.com/org/[repo]",
		"git@github.com:org/re{po}",
		"github.com/org/re<po>",
		"https://[1:2]/org/repo",
		"https://[:::]/org/repo",
		"https://[v1.x]/org/repo",
		"https://[1:2:3:4:5:6:7::8]/org/repo",
		"https://us\"er@h.example/o/r",
		"https://h.com\\@evil.com/o/r",
		// Only ASCII whitespace is trimmed (#2713 r4 F2/F3).
		"https://github.com/org/repo\u0085",
		"\ufeffhttps://github.com/org/repo",
		"https://github.com/org/repo\ufeff",
		"\u00a0github.com/org/repo",
	} {
		t.Run(remote, func(t *testing.T) {
			rec := doRequest(t, srv, http.MethodPost, "/api/v1/projects/"+src.ID+"/clone",
				map[string]interface{}{"name": "Bad Remote", "gitRemote": remote})
			require.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
			assert.Contains(t, rec.Body.String(), "gitRemote")
			assert.NotContains(t, rec.Body.String(), "SECRET_")
		})
	}

	after, err := s.ListProjects(ctx, store.ProjectFilter{}, store.ListOptions{Limit: 1000})
	require.NoError(t, err)
	assert.Equal(t, len(before.Items), len(after.Items), "a rejected override must not create a project")
}

// TestProjectClone_GitRemoteOverride_AcceptedForms checks the URL forms an
// override may use, including the scheme-less form GitRemote is stored in.
func TestProjectClone_GitRemoteOverride_AcceptedForms(t *testing.T) {
	for i, remote := range []string{
		"https://github.com/other-org/other-repo.git",
		"git@github.com:other-org/other-repo.git",
		"ssh://git@github.com/other-org/other-repo.git",
		"github.com/other-org/other-repo",
	} {
		t.Run(remote, func(t *testing.T) {
			srv, s := testServer(t)
			src := createSourceProject(t, srv, s)
			rec := doRequest(t, srv, http.MethodPost, "/api/v1/projects/"+src.ID+"/clone",
				map[string]interface{}{"name": fmt.Sprintf("Form %d", i), "gitRemote": remote})
			require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())

			var clone store.Project
			require.NoError(t, json.NewDecoder(rec.Body).Decode(&clone))
			assert.Equal(t, "github.com/other-org/other-repo", clone.GitRemote)
			assert.Equal(t, "https://github.com/other-org/other-repo.git", clone.Labels[store.LabelCloneURL])
		})
	}
}

// TestProjectClone_GitRemoteOverride_RejectsSSHPort checks that ssh:// URLs
// with a port get a specific 400: NormalizeGitRemote/ToHTTPSCloneURL would turn
// the port into a path segment.
func TestProjectClone_GitRemoteOverride_RejectsSSHPort(t *testing.T) {
	srv, s := testServer(t)
	src := createSourceProject(t, srv, s)

	for _, remote := range []string{
		"ssh://git@git.example.com:2222/group/repo.git",
		"ssh://review.example.com:29418/project/repo",
		"SSH://git:pw@git.example.com:2222/group/repo.git",
	} {
		t.Run(remote, func(t *testing.T) {
			rec := doRequest(t, srv, http.MethodPost, "/api/v1/projects/"+src.ID+"/clone",
				map[string]interface{}{"name": "SSH Port", "gitRemote": remote})
			require.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
			assert.Contains(t, rec.Body.String(), "ssh URLs with a port are not supported yet; use the https URL")
			assert.NotContains(t, rec.Body.String(), "pw@")
		})
	}
}

// TestProjectClone_GitRemoteOverride_RejectsTLSPort checks that git:// with
// any port and http:// with a port other than 80 get a specific 400:
// ToHTTPSCloneURL keeps the port, so the clone-url would speak TLS to a
// plain-text port (r5).
func TestProjectClone_GitRemoteOverride_RejectsTLSPort(t *testing.T) {
	srv, s := testServer(t)
	src := createSourceProject(t, srv, s)

	for _, remote := range []string{
		"git://git.example.com:9418/group/repo.git",
		"GIT://git.example.com:9419/group/repo",
		"http://git.example.com:8080/group/repo",
		"http://u:SECRET_P@git.example.com:443/group/repo",
		// The scheme-less form's clone-url is https (r6 R3).
		"git.example.com:80/group/repo",
	} {
		t.Run(remote, func(t *testing.T) {
			rec := doRequest(t, srv, http.MethodPost, "/api/v1/projects/"+src.ID+"/clone",
				map[string]interface{}{"name": "TLS Port", "gitRemote": remote})
			require.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
			assert.Contains(t, rec.Body.String(), "git:// URLs with a port, http:// URLs with a port other than 80 and host:80/... remotes are not supported; use the https URL")
			assert.NotContains(t, rec.Body.String(), "SECRET_")
		})
	}
}

// TestProjectClone_GitRemoteOverride_DerivedForms checks GitRemote and the git
// source labels for override forms beyond the github https/scp basics.
func TestProjectClone_GitRemoteOverride_DerivedForms(t *testing.T) {
	tests := []struct {
		name, remote, gitRemote, cloneURL, sourceURL string
	}{
		{
			name:      "scp with non-git login",
			remote:    "alice@git.example.com:team/repo.git",
			gitRemote: "git.example.com/team/repo",
			cloneURL:  "https://git.example.com/team/repo.git",
			sourceURL: "alice@git.example.com:team/repo.git",
		},
		{
			name:      "scp with single-label host",
			remote:    "git@gitserver:org/repo",
			gitRemote: "gitserver/org/repo",
			cloneURL:  "https://gitserver/org/repo.git",
			sourceURL: "git@gitserver:org/repo",
		},
		{
			name:      "https with single-label host",
			remote:    "https://gitserver/org/repo.git",
			gitRemote: "gitserver/org/repo",
			cloneURL:  "https://gitserver/org/repo.git",
			sourceURL: "https://gitserver/org/repo.git",
		},
		{
			name:      "password containing @ is stripped",
			remote:    "https://u:SECRET_P@ss@github.com/acme/repo.git",
			gitRemote: "github.com/acme/repo",
			cloneURL:  "https://github.com/acme/repo.git",
			sourceURL: "https://github.com/acme/repo.git",
		},
		{
			name:      "https default port dropped",
			remote:    "https://github.com:443/acme/repo.git",
			gitRemote: "github.com/acme/repo",
			cloneURL:  "https://github.com/acme/repo.git",
			sourceURL: "https://github.com/acme/repo.git",
		},
		{
			name:      "http default port dropped",
			remote:    "http://u:SECRET_P@git.example.com:80/team/repo",
			gitRemote: "git.example.com/team/repo",
			cloneURL:  "https://git.example.com/team/repo.git",
			sourceURL: "http://git.example.com/team/repo",
		},
		{
			name:      "ssh login kept in source-url",
			remote:    "ssh://alice:SECRET_P@git.example.com/team/repo.git",
			gitRemote: "git.example.com/team/repo",
			cloneURL:  "https://git.example.com/team/repo.git",
			sourceURL: "ssh://alice@git.example.com/team/repo.git",
		},
		{
			name:      "scheme-less host:port",
			remote:    "git.example.com:8443/team/repo",
			gitRemote: "git.example.com:8443/team/repo",
			cloneURL:  "https://git.example.com:8443/team/repo.git",
			sourceURL: "git.example.com:8443/team/repo",
		},
		{
			name:      "https with port",
			remote:    "https://git.example.com:8443/team/repo.git",
			gitRemote: "git.example.com:8443/team/repo",
			cloneURL:  "https://git.example.com:8443/team/repo.git",
			sourceURL: "https://git.example.com:8443/team/repo.git",
		},
		{
			name:      "punycode host and dotted names are not dot segments",
			remote:    "https://xn--bcher-kva.example/org/.github",
			gitRemote: "xn--bcher-kva.example/org/.github",
			cloneURL:  "https://xn--bcher-kva.example/org/.github.git",
			sourceURL: "https://xn--bcher-kva.example/org/.github",
		},
		{
			name:      "dots inside a segment and a trailing slash",
			remote:    "git@git.example.com:team/my..repo/",
			gitRemote: "git.example.com/team/my..repo",
			cloneURL:  "https://git.example.com/team/my..repo.git",
			sourceURL: "git@git.example.com:team/my..repo/",
		},
		{
			name:      "ASCII whitespace around the remote is trimmed",
			remote:    " \t git.example.com/team/repo \r\n",
			gitRemote: "git.example.com/team/repo",
			cloneURL:  "https://git.example.com/team/repo.git",
			sourceURL: "git.example.com/team/repo",
		},
		{
			name:      "scheme-less form drops :443 like https",
			remote:    "git.example.com:443/team/repo",
			gitRemote: "git.example.com/team/repo",
			cloneURL:  "https://git.example.com/team/repo.git",
			sourceURL: "git.example.com/team/repo",
		},
		{
			name:      "pct-encoded space and sub-delims in the path",
			remote:    "https://dev.azure.com/org/My%20Project/_git/repo",
			gitRemote: "dev.azure.com/org/my%20project/_git/repo",
			cloneURL:  "https://dev.azure.com/org/My%20Project/_git/repo",
			sourceURL: "https://dev.azure.com/org/My%20Project/_git/repo",
		},
		{
			name:      "http with the default port",
			remote:    "http://git.example.com:80/team/repo",
			gitRemote: "git.example.com/team/repo",
			cloneURL:  "https://git.example.com/team/repo.git",
			sourceURL: "http://git.example.com/team/repo",
		},
		{
			name:      "query and fragment dropped",
			remote:    "https://github.com/acme/repo.git?access_token=SECRET_Q#SECRET_F",
			gitRemote: "github.com/acme/repo",
			cloneURL:  "https://github.com/acme/repo.git",
			sourceURL: "https://github.com/acme/repo.git",
		},
		{
			name:      "query, fragment and userinfo dropped",
			remote:    "https://u:SECRET_P@github.com/acme/repo?private_token=SECRET_Q",
			gitRemote: "github.com/acme/repo",
			cloneURL:  "https://github.com/acme/repo.git",
			sourceURL: "https://github.com/acme/repo",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv, s := testServer(t)
			src := createSourceProject(t, srv, s)
			rec := doRequest(t, srv, http.MethodPost, "/api/v1/projects/"+src.ID+"/clone",
				map[string]interface{}{"name": "Derived", "gitRemote": tt.remote})
			require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
			assert.NotContains(t, rec.Body.String(), "SECRET_")

			var clone store.Project
			require.NoError(t, json.NewDecoder(rec.Body).Decode(&clone))
			assert.Equal(t, tt.gitRemote, clone.GitRemote)
			assert.Equal(t, tt.cloneURL, clone.Labels[store.LabelCloneURL])
			assert.Equal(t, tt.sourceURL, clone.Labels[store.LabelSourceURL])

			stored, err := s.GetProject(context.Background(), clone.ID)
			require.NoError(t, err)
			for k, v := range stored.Labels {
				assert.NotContains(t, v, "SECRET_", "label %s", k)
			}
		})
	}
}

// TestProjectClone_GitRemoteOverride_NonGitTemplate checks that overriding the
// remote of a template with no git remote and no labels derives the git
// source labels from the override. (Workspace-mode derivation for this case
// is owned by #2703.)
func TestProjectClone_GitRemoteOverride_NonGitTemplate(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	src := &store.Project{
		ID:        api.NewUUID(),
		Name:      "Notebook Template",
		Slug:      "notebook-template",
		OwnerID:   DevUserID,
		CreatedBy: DevUserID,
	}
	require.NoError(t, s.CreateProject(ctx, src))
	require.Empty(t, src.GitRemote)
	require.Nil(t, src.Labels)

	overrideURL := "https://github.com/acme/notebooks.git"
	rec := doRequest(t, srv, http.MethodPost, "/api/v1/projects/"+src.ID+"/clone",
		map[string]interface{}{"name": "Notebooks", "gitRemote": overrideURL})
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())

	var clone store.Project
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&clone))
	assert.Equal(t, "github.com/acme/notebooks", clone.GitRemote)
	assert.Equal(t, map[string]string{
		store.LabelCloneURL:      "https://github.com/acme/notebooks.git",
		store.LabelSourceURL:     overrideURL,
		store.LabelDefaultBranch: "main",
	}, clone.Labels)

	stored, err := s.GetProject(ctx, clone.ID)
	require.NoError(t, err)
	assert.Equal(t, clone.Labels, stored.Labels)
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
		ID:        api.NewUUID(),
		Name:      "Bare Project",
		Slug:      "bare-project",
		OwnerID:   DevUserID,
		CreatedBy: DevUserID,
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
		ID:        api.NewUUID(),
		Name:      "Original",
		Slug:      "original",
		OwnerID:   DevUserID,
		CreatedBy: DevUserID,
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
		ID:        api.NewUUID(),
		Name:      "Existing",
		Slug:      "existing-slug",
		OwnerID:   DevUserID,
		CreatedBy: DevUserID,
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
		ID:        api.NewUUID(),
		Name:      "With Agents",
		Slug:      "with-agents",
		OwnerID:   DevUserID,
		CreatedBy: DevUserID,
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
		ID:        api.NewUUID(),
		Name:      "Auth Test",
		Slug:      "auth-test",
		OwnerID:   DevUserID,
		CreatedBy: DevUserID,
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
		ID:        api.NewUUID(),
		Name:      "Readable",
		Slug:      "readable",
		OwnerID:   DevUserID,
		CreatedBy: DevUserID,
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
		ID:        api.NewUUID(),
		Name:      "Valid Project",
		Slug:      "valid-project",
		OwnerID:   DevUserID,
		CreatedBy: DevUserID,
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
		ID:        api.NewUUID(),
		Name:      "Method Test",
		Slug:      "method-test",
		OwnerID:   DevUserID,
		CreatedBy: DevUserID,
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
		ID:        api.NewUUID(),
		Name:      "Concurrent Source",
		Slug:      "concurrent-source",
		OwnerID:   DevUserID,
		CreatedBy: DevUserID,
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
		ID:        api.NewUUID(),
		Name:      "Template Source",
		Slug:      "template-source",
		OwnerID:   DevUserID,
		CreatedBy: DevUserID,
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
}

func TestProjectClone_AsTemplate_ScionIOLabelsStripped_WhenNoAsTemplate(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	// Create source with scion.io/template label
	project := &store.Project{
		ID:        api.NewUUID(),
		Name:      "Existing Template",
		Slug:      "existing-template",
		OwnerID:   DevUserID,
		CreatedBy: DevUserID,
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

func TestProjectClone_CopiesMaxAgentRole(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	src := &store.Project{
		ID:        api.NewUUID(),
		Name:      "Source With MaxRole",
		Slug:      "source-maxrole",
		OwnerID:   DevUserID,
		CreatedBy: DevUserID,
		Annotations: map[string]string{
			projectSettingMaxAgentRole: "readonly",
		},
	}
	require.NoError(t, s.CreateProject(ctx, src))

	rec := doRequest(t, srv, http.MethodPost, "/api/v1/projects/"+src.ID+"/clone",
		map[string]string{"name": "Cloned MaxRole"})
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())

	var clone store.Project
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&clone))

	assert.Equal(t, "readonly", clone.Annotations[projectSettingMaxAgentRole],
		"clone should copy max_agent_role annotation from source")
}

func TestProjectClone_GCPServiceAccountVerifiedAndDefaultRemapped(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	// Create a source project with a default SA annotation.
	srcID := api.NewUUID()

	verifiedSA := &store.GCPServiceAccount{
		ID:        api.NewUUID(),
		Scope:     store.ScopeProject,
		ScopeID:   srcID,
		Email:     "verified@proj.iam.gserviceaccount.com",
		ProjectID: "gcp-proj",
		Verified:  true,
		CreatedBy: DevUserID,
	}
	unverifiedSA := &store.GCPServiceAccount{
		ID:        api.NewUUID(),
		Scope:     store.ScopeProject,
		ScopeID:   srcID,
		Email:     "unverified@proj.iam.gserviceaccount.com",
		ProjectID: "gcp-proj",
		Verified:  false,
		CreatedBy: DevUserID,
	}

	src := &store.Project{
		ID:        srcID,
		Name:      "Source SA Project",
		Slug:      "source-sa-proj",
		OwnerID:   DevUserID,
		CreatedBy: DevUserID,
		Annotations: map[string]string{
			// Default SA points to the verified SA in the source project.
			projectSettingDefaultGCPIdentitySAID: verifiedSA.ID,
		},
	}
	require.NoError(t, s.CreateProject(ctx, src))
	require.NoError(t, s.CreateGCPServiceAccount(ctx, verifiedSA))
	require.NoError(t, s.CreateGCPServiceAccount(ctx, unverifiedSA))

	// Clone the project.
	rec := doRequest(t, srv, http.MethodPost, "/api/v1/projects/"+src.ID+"/clone",
		map[string]string{"name": "Cloned SA Project"})
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())

	var clone store.Project
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&clone))

	// Fetch cloned SAs.
	cloneSAs, err := s.ListGCPServiceAccounts(ctx, store.GCPServiceAccountFilter{
		Scope:   store.ScopeProject,
		ScopeID: clone.ID,
	})
	require.NoError(t, err)
	require.Len(t, cloneSAs, 2, "both SAs should be cloned")

	// Build a lookup by email for assertions.
	saByEmail := make(map[string]store.GCPServiceAccount, len(cloneSAs))
	for _, sa := range cloneSAs {
		saByEmail[sa.Email] = sa
	}

	// Assert: verified state preserved.
	clonedVerified, ok := saByEmail["verified@proj.iam.gserviceaccount.com"]
	require.True(t, ok, "cloned verified SA should exist")
	assert.True(t, clonedVerified.Verified, "cloned SA should preserve verified=true")

	clonedUnverified, ok := saByEmail["unverified@proj.iam.gserviceaccount.com"]
	require.True(t, ok, "cloned unverified SA should exist")
	assert.False(t, clonedUnverified.Verified, "cloned SA should preserve verified=false")

	// Assert: default SA annotation remapped to the cloned SA's ID.
	// Re-read the clone from the store to get the persisted annotations.
	persistedClone, err := s.GetProject(ctx, clone.ID)
	require.NoError(t, err)

	remappedID := persistedClone.Annotations[projectSettingDefaultGCPIdentitySAID]
	assert.NotEmpty(t, remappedID, "default SA annotation should be set")
	assert.NotEqual(t, verifiedSA.ID, remappedID,
		"default SA annotation must not point to source SA ID")
	assert.Equal(t, clonedVerified.ID, remappedID,
		"default SA annotation should point to the cloned SA with the same email")
}
