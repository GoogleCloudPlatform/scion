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
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Clients send a provider's local path either as the project's .scion
// directory or as the project root. On the embedded broker the hub
// initializes that .scion directory itself, never a .scion nested inside it.

func TestLinkedProjectScionDir(t *testing.T) {
	cases := map[string]string{
		"/work/repo":             "/work/repo/.scion",
		"/work/repo/":            "/work/repo/.scion",
		"/work/repo/.scion":      "/work/repo/.scion",
		"/work/repo/.scion/":     "/work/repo/.scion",
		"/work/repo/x/../.scion": "/work/repo/.scion",
		"/work/repo/.scionx":     "/work/repo/.scionx/.scion",
		"/work/scion":            "/work/scion/.scion",
	}
	for in, want := range cases {
		assert.Equal(t, want, linkedProjectScionDir(in), "input %q", in)
	}
}

func TestProviderLocalPath_EmbeddedAddProviderScionDirPath(t *testing.T) {
	f := brokerAssocSetup(t, "localpath-scion-dir")
	f.srv.SetEmbeddedBrokerID(f.ownBroker.ID)
	dirs := stubLinkedProjectInit(t)

	root := filepath.Join(t.TempDir(), "repo")
	scionDir := filepath.Join(root, ".scion")
	require.NoError(t, os.MkdirAll(scionDir, 0o755))

	rec := doRequestAsUser(t, f.srv, f.projectOwner, http.MethodPost, f.providersPath(),
		AddProviderRequest{BrokerID: f.ownBroker.ID, LocalPath: scionDir})
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	assert.Equal(t, []string{scionDir}, *dirs, "a path ending in .scion is initialized as is")

	rec = doRequestAsUser(t, f.srv, f.projectOwner, http.MethodPost, f.providersPath(),
		AddProviderRequest{BrokerID: f.ownBroker.ID, LocalPath: root})
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	assert.Equal(t, []string{scionDir, scionDir}, *dirs, "a project root gets .scion appended")
}

func TestProviderLocalPath_EmbeddedRegisterScionDirPath(t *testing.T) {
	f := brokerAssocSetup(t, "localpath-register-scion-dir")
	f.srv.SetEmbeddedBrokerID(f.ownBroker.ID)
	dirs := stubLinkedProjectInit(t)

	root := filepath.Join(t.TempDir(), "repo")
	scionDir := filepath.Join(root, ".scion")
	require.NoError(t, os.MkdirAll(scionDir, 0o755))

	rec := doRequestAsUser(t, f.srv, f.projectOwner, http.MethodPost, "/api/v1/projects/register", RegisterProjectRequest{
		ID:       f.project.ID,
		Name:     f.project.Name,
		BrokerID: f.ownBroker.ID,
		Path:     scionDir,
	})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.Equal(t, []string{scionDir}, *dirs, "a path ending in .scion is initialized as is")
}

func TestProviderLocalPath_EmbeddedRegisterProjectRootPath(t *testing.T) {
	f := brokerAssocSetup(t, "localpath-register-root")
	f.srv.SetEmbeddedBrokerID(f.ownBroker.ID)
	dirs := stubLinkedProjectInit(t)

	root := filepath.Join(t.TempDir(), "repo")
	require.NoError(t, os.MkdirAll(root, 0o755))

	rec := doRequestAsUser(t, f.srv, f.projectOwner, http.MethodPost, "/api/v1/projects/register", RegisterProjectRequest{
		ID:       f.project.ID,
		Name:     f.project.Name,
		BrokerID: f.ownBroker.ID,
		Path:     root,
	})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.Equal(t, []string{filepath.Join(root, ".scion")}, *dirs, "a project root gets .scion appended")
}

// With the real project init, a provider path ending in .scion initializes
// that directory: no .scion level inside it and no project config named
// after the .scion directory.
func TestProviderLocalPath_EmbeddedScionDirPathNoNestedProject(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Cleanup(config.OverrideIsGitRepo(func() bool { return true }))

	f := brokerAssocSetup(t, "localpath-scion-dir-real")
	f.srv.SetEmbeddedBrokerID(f.ownBroker.ID)

	root := filepath.Join(t.TempDir(), "repo")
	scionDir := filepath.Join(root, ".scion")
	require.NoError(t, os.MkdirAll(scionDir, 0o755))

	rec := doRequestAsUser(t, f.srv, f.projectOwner, http.MethodPost, f.providersPath(),
		AddProviderRequest{BrokerID: f.ownBroker.ID, LocalPath: scionDir})
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())

	_, err := os.Lstat(filepath.Join(scionDir, ".scion"))
	assert.True(t, os.IsNotExist(err), "no .scion inside the provider's .scion directory, got %v", err)
	info, err := os.Stat(filepath.Join(scionDir, "templates"))
	require.NoError(t, err, "the provider's .scion directory is initialized")
	assert.True(t, info.IsDir())

	entries, err := os.ReadDir(filepath.Join(home, ".scion", "project-configs"))
	require.NoError(t, err)
	require.NotEmpty(t, entries)
	for _, e := range entries {
		assert.False(t, strings.HasPrefix(e.Name(), "scion__"),
			"no project config named after the .scion directory, got %s", e.Name())
	}
}
