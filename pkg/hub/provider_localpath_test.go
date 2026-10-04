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

package hub

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"path/filepath"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// brokerGlobalDir is a broker's global scion directory as provide sent it
// when run from the broker user's home directory. The hub never initializes
// it in these tests: it is either rejected or written straight to the store.
const brokerGlobalDir = "/home/brokeruser/.scion"

func TestIsBrokerGlobalDirPath(t *testing.T) {
	cases := map[string]bool{
		"/home/scion/.scion":          true,
		"/home/scion/.scion/":         true,
		"/root/.scion":                true,
		"/Users/alice/.scion":         true,
		"/home/scion":                 true, // project root form
		"/root":                       true,
		"/Users/alice/":               true,
		"/home/scion/src/repo/.scion": false,
		"/home/scion/src/repo":        false,
		"/home/scion/.scion/project-configs/x/.scion": false,
		"/home/scion/.scion/projects/web":             false,
		"/home/.scion":                                false,
		"/home":                                       false,
		"/srv/repo/.scion":                            false,
		"":                                            false,
		"relative/.scion":                             false,
	}
	for path, want := range cases {
		assert.Equal(t, want, isBrokerGlobalDirPath(path), "path %q", path)
	}
}

func TestValidateProviderLocalPath(t *testing.T) {
	assert.Error(t, validateProviderLocalPath("web-app", "web-app", brokerGlobalDir))
	assert.Error(t, validateProviderLocalPath("web-app", "web-app", "/home/brokeruser"))
	assert.NoError(t, validateProviderLocalPath("Global", "global", brokerGlobalDir))
	// Only the slug identifies the global project, not the name.
	assert.Error(t, validateProviderLocalPath("Global", "global-2", brokerGlobalDir))
	assert.NoError(t, validateProviderLocalPath("web-app", "web-app", "/home/brokeruser/src/web-app/.scion"))
	assert.NoError(t, validateProviderLocalPath("web-app", "web-app", ""))
}

func TestValidateProviderLocalPath_AcceptsLinkAndSyncPaths(t *testing.T) {
	for _, path := range []string{
		"/home/brokeruser/src/web-app/.scion",                              // in-repo project (hub link, hubsync)
		"/home/brokeruser/.scion/project-configs/web-app__1dfdd6c7/.scion", // external split-storage project
		"/home/brokeruser/.scion/projects/web-app",                         // hub-managed project directory
		"/home/brokeruser/src/web-app",                                     // project root (web linked create)
		"/srv/checkouts/web-app/.scion",
	} {
		assert.NoError(t, validateProviderLocalPath("web-app", "web-app", path), "path %q", path)
	}
}

func newLocalPathTestBroker(t *testing.T, s store.Store, name string) *store.RuntimeBroker {
	t.Helper()
	broker := &store.RuntimeBroker{
		ID:     tid(name),
		Name:   name,
		Slug:   name,
		Status: store.BrokerStatusOnline,
	}
	require.NoError(t, s.CreateRuntimeBroker(context.Background(), broker))
	return broker
}

func registerWithBroker(t *testing.T, srv *Server, name, brokerID, path string) (*RegisterProjectResponse, int, string) {
	t.Helper()
	body := map[string]interface{}{"name": name, "brokerId": brokerID}
	if path != "" {
		body["path"] = path
	}
	rec := doRequest(t, srv, http.MethodPost, "/api/v1/projects/register", body)
	if rec.Code != http.StatusOK {
		return nil, rec.Code, rec.Body.String()
	}
	var resp RegisterProjectResponse
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&resp))
	return &resp, rec.Code, rec.Body.String()
}

func TestProjectRegister_RejectsGlobalDirPathForNewProject(t *testing.T) {
	srv, s := testServer(t)
	broker := newLocalPathTestBroker(t, s, "gd-new-broker")

	_, code, body := registerWithBroker(t, srv, "gd-new-project", broker.ID, brokerGlobalDir)
	require.Equal(t, http.StatusBadRequest, code, "body: %s", body)
	assert.Contains(t, body, "global scion directory")

	_, err := s.GetProjectBySlug(context.Background(), "gd-new-project")
	assert.ErrorIs(t, err, store.ErrNotFound, "a rejected register must not create the project")
}

func TestProjectRegister_RejectsGlobalDirPathForExistingProject(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()
	broker := newLocalPathTestBroker(t, s, "gd-existing-broker")

	resp, code, body := registerWithBroker(t, srv, "gd-existing-project", broker.ID, "")
	require.Equal(t, http.StatusOK, code, "body: %s", body)

	_, code, body = registerWithBroker(t, srv, "gd-existing-project", broker.ID, brokerGlobalDir)
	require.Equal(t, http.StatusBadRequest, code, "body: %s", body)

	provider, err := s.GetProjectProvider(ctx, resp.Project.ID, broker.ID)
	require.NoError(t, err)
	assert.Empty(t, provider.LocalPath, "a rejected register must leave the provider unchanged")
}

func TestProjectRegister_AcceptsProjectPath(t *testing.T) {
	srv, s := testServer(t)
	broker := newLocalPathTestBroker(t, s, "gd-normal-broker")
	projectDir := filepath.Join(t.TempDir(), "web-app", ".scion")

	resp, code, body := registerWithBroker(t, srv, "gd-normal-project", broker.ID, projectDir)
	require.Equal(t, http.StatusOK, code, "body: %s", body)

	provider, err := s.GetProjectProvider(context.Background(), resp.Project.ID, broker.ID)
	require.NoError(t, err)
	assert.Equal(t, projectDir, provider.LocalPath)
}

// A provider stored with the global dir (before register rejected it) is
// repaired by re-running provide without a path, or with the right path.
func TestProjectRegister_RepairsStoredGlobalDirPath(t *testing.T) {
	for _, tc := range []struct {
		name, newPath, want string
	}{
		{name: "no path clears it"},
		{name: "project path replaces it", newPath: "/home/brokeruser/src/web-app/.scion", want: "/home/brokeruser/src/web-app/.scion"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, s := testServer(t)
			ctx := context.Background()
			broker := newLocalPathTestBroker(t, s, "gd-repair-broker")

			resp, code, body := registerWithBroker(t, srv, "gd-repair-project", broker.ID, "")
			require.Equal(t, http.StatusOK, code, "body: %s", body)
			projectID := resp.Project.ID

			require.NoError(t, s.AddProjectProvider(ctx, &store.ProjectProvider{
				ProjectID:  projectID,
				BrokerID:   broker.ID,
				BrokerName: broker.Name,
				LocalPath:  brokerGlobalDir,
				Status:     store.BrokerStatusOnline,
			}))

			newPath := tc.newPath
			if newPath != "" {
				// Keep the hub's own InitProject off real directories.
				newPath = filepath.Join(t.TempDir(), "web-app", ".scion")
				tc.want = newPath
			}
			_, code, body = registerWithBroker(t, srv, "gd-repair-project", broker.ID, newPath)
			require.Equal(t, http.StatusOK, code, "body: %s", body)

			provider, err := s.GetProjectProvider(ctx, projectID, broker.ID)
			require.NoError(t, err)
			assert.Equal(t, tc.want, provider.LocalPath)
		})
	}
}

// A re-register with no path (provide --project without --path) keeps a
// linked provider's path.
func TestProjectRegister_EmptyPathKeepsLinkedPath(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()
	broker := newLocalPathTestBroker(t, s, "gd-keep-broker")
	projectDir := filepath.Join(t.TempDir(), "web-app", ".scion")

	resp, code, body := registerWithBroker(t, srv, "gd-keep-project", broker.ID, projectDir)
	require.Equal(t, http.StatusOK, code, "body: %s", body)

	_, code, body = registerWithBroker(t, srv, "gd-keep-project", broker.ID, "")
	require.Equal(t, http.StatusOK, code, "body: %s", body)

	provider, err := s.GetProjectProvider(ctx, resp.Project.ID, broker.ID)
	require.NoError(t, err)
	assert.Equal(t, projectDir, provider.LocalPath)
}

func TestAddProvider_AcceptsLinkedProjectPath(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()
	broker := newLocalPathTestBroker(t, s, "gd-add-ok-broker")
	project := &store.Project{ID: tid("gd-add-ok-project"), Name: "gd-add-ok-project", Slug: "gd-add-ok-project"}
	require.NoError(t, s.CreateProject(ctx, project))
	projectRoot := t.TempDir()

	rec := doRequest(t, srv, http.MethodPost, "/api/v1/projects/"+project.ID+"/providers", map[string]interface{}{
		"brokerId":  broker.ID,
		"localPath": projectRoot,
	})
	require.Equal(t, http.StatusCreated, rec.Code, "body: %s", rec.Body.String())

	provider, err := s.GetProjectProvider(ctx, project.ID, broker.ID)
	require.NoError(t, err)
	assert.Equal(t, projectRoot, provider.LocalPath)
}

// The global project's provider path travels with the global slug, so the
// broker can tell it from another project pointed at its global directory.
// Other projects with a provider path send no slug, as before.
func TestResolveDispatchProjectInfo_GlobalSlugWithProviderPath(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name, slug, path, wantSlug string
	}{
		{name: "global with path", slug: "global", path: brokerGlobalDir, wantSlug: "global"},
		{name: "global without path", slug: "global", wantSlug: "global"},
		{name: "project with path", slug: "gd-web", path: "/home/brokeruser/src/web/.scion", wantSlug: ""},
		{name: "project without path", slug: "gd-web-native", wantSlug: "gd-web-native"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, s := testServer(t)
			broker := newLocalPathTestBroker(t, s, "gd-dispatch-broker")
			d := NewHTTPAgentDispatcherWithClient(s, nil, false, slog.Default())
			project := &store.Project{ID: tid("gd-dispatch-" + tc.slug), Name: tc.slug, Slug: tc.slug}
			require.NoError(t, s.CreateProject(ctx, project))
			require.NoError(t, s.AddProjectProvider(ctx, &store.ProjectProvider{
				ProjectID: project.ID, BrokerID: broker.ID, BrokerName: broker.Name, LocalPath: tc.path,
				Status: store.BrokerStatusOnline,
			}))
			info, err := d.resolveDispatchProjectInfo(ctx, &store.Agent{ProjectID: project.ID, RuntimeBrokerID: broker.ID})
			require.NoError(t, err)
			assert.Equal(t, tc.path, info.projectPath)
			assert.Equal(t, tc.wantSlug, info.projectSlug)
		})
	}
}

func TestAddProvider_RejectsGlobalDirPathForProject(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()
	broker := newLocalPathTestBroker(t, s, "gd-add-broker")
	project := &store.Project{ID: tid("gd-add-project"), Name: "gd-add-project", Slug: "gd-add-project"}
	require.NoError(t, s.CreateProject(ctx, project))

	rec := doRequest(t, srv, http.MethodPost, "/api/v1/projects/"+project.ID+"/providers", map[string]interface{}{
		"brokerId":  broker.ID,
		"localPath": brokerGlobalDir,
	})
	require.Equal(t, http.StatusBadRequest, rec.Code, "body: %s", rec.Body.String())
	assert.Contains(t, rec.Body.String(), "global scion directory")

	_, err := s.GetProjectProvider(ctx, project.ID, broker.ID)
	assert.ErrorIs(t, err, store.ErrNotFound)
}

// A client can set the scion.io/global label on an ordinary project (register
// without a broker, or a project update). The label does not make it the
// global project: register still rejects the global dir for it, and a stored
// global-dir path is still repaired by a register without a path.
func TestProjectRegister_GlobalLabelDoesNotExemptProject(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()
	broker := newLocalPathTestBroker(t, s, "gd-label-broker")

	rec := doRequest(t, srv, http.MethodPost, "/api/v1/projects/register", map[string]interface{}{
		"name":   "gd-label-project",
		"labels": map[string]string{"scion.io/global": "true"},
	})
	require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())
	var created RegisterProjectResponse
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&created))
	projectID := created.Project.ID
	stored, err := s.GetProject(ctx, projectID)
	require.NoError(t, err)
	require.Equal(t, "true", stored.Labels["scion.io/global"], "the label must be stored for this test to mean anything")

	_, code, body := registerWithBroker(t, srv, "gd-label-project", broker.ID, brokerGlobalDir)
	require.Equal(t, http.StatusBadRequest, code, "body: %s", body)
	assert.Contains(t, body, "global scion directory")

	require.NoError(t, s.AddProjectProvider(ctx, &store.ProjectProvider{
		ProjectID: projectID, BrokerID: broker.ID, BrokerName: broker.Name,
		LocalPath: brokerGlobalDir, Status: store.BrokerStatusOnline,
	}))
	_, code, body = registerWithBroker(t, srv, "gd-label-project", broker.ID, "")
	require.Equal(t, http.StatusOK, code, "body: %s", body)
	provider, err := s.GetProjectProvider(ctx, projectID, broker.ID)
	require.NoError(t, err)
	assert.Empty(t, provider.LocalPath, "the stored global-dir path must be cleared")
}

// A new project named "global" that comes with a git remote does not take
// the global slug's place, so the global dir is rejected for it.
func TestProjectRegister_NewGitProjectNamedGlobalRejectsGlobalDir(t *testing.T) {
	srv, s := testServer(t)
	broker := newLocalPathTestBroker(t, s, "gd-gitglobal-broker")

	rec := doRequest(t, srv, http.MethodPost, "/api/v1/projects/register", map[string]interface{}{
		"name":      "global",
		"gitRemote": "github.com/test/global",
		"brokerId":  broker.ID,
		"path":      brokerGlobalDir,
	})
	require.Equal(t, http.StatusBadRequest, rec.Code, "body: %s", rec.Body.String())
}
