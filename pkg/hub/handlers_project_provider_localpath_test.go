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
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A provider's LocalPath names a directory on the broker's host. The hub
// checks the directory and initializes .scion only for the embedded broker,
// which shares its filesystem; for any other broker the path is validated
// syntactically and stored.

func TestProviderLocalPath_OtherBrokerPathIsStoredWithoutHubFilesystemAccess(t *testing.T) {
	f := brokerAssocSetup(t, "localpath-other")
	missing := filepath.Join(t.TempDir(), "on-broker-host")

	rec := doRequestAsUser(t, f.srv, f.projectOwner, http.MethodPost, f.providersPath(),
		AddProviderRequest{BrokerID: f.ownBroker.ID, LocalPath: missing})

	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	provider, err := f.store.GetProjectProvider(context.Background(), f.project.ID, f.ownBroker.ID)
	require.NoError(t, err)
	assert.Equal(t, missing, provider.LocalPath)
	_, err = os.Stat(missing)
	assert.True(t, os.IsNotExist(err), "the hub must not create the directory, got %v", err)
}

func TestProviderLocalPath_OtherBrokerExistingDirIsNotInitialized(t *testing.T) {
	f := brokerAssocSetup(t, "localpath-other-dir")
	dir := t.TempDir()

	rec := doRequestAsUser(t, f.srv, f.projectOwner, http.MethodPost, f.providersPath(),
		AddProviderRequest{BrokerID: f.ownBroker.ID, LocalPath: dir})

	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	_, err := os.Stat(filepath.Join(dir, ".scion"))
	assert.True(t, os.IsNotExist(err), "the hub must not initialize .scion for another broker, got %v", err)
}

func TestProviderLocalPath_RestrictedPrefixRejected(t *testing.T) {
	f := brokerAssocSetup(t, "localpath-restricted")

	for _, path := range []string{"/etc/x", "relative/path"} {
		rec := doRequestAsUser(t, f.srv, f.projectOwner, http.MethodPost, f.providersPath(),
			AddProviderRequest{BrokerID: f.ownBroker.ID, LocalPath: path})
		assert.Equal(t, http.StatusBadRequest, rec.Code, "path %q: %s", path, rec.Body.String())
	}
	assertNoProvider(t, f.store, f.project.ID, f.ownBroker.ID)
}

func TestProviderLocalPath_EmbeddedBrokerChecksAndInitializes(t *testing.T) {
	f := brokerAssocSetup(t, "localpath-embedded")
	f.srv.SetEmbeddedBrokerID(f.ownBroker.ID)

	missing := filepath.Join(t.TempDir(), "missing")
	rec := doRequestAsUser(t, f.srv, f.projectOwner, http.MethodPost, f.providersPath(),
		AddProviderRequest{BrokerID: f.ownBroker.ID, LocalPath: missing})
	require.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
	assertNoProvider(t, f.store, f.project.ID, f.ownBroker.ID)

	dir := t.TempDir()
	rec = doRequestAsUser(t, f.srv, f.projectOwner, http.MethodPost, f.providersPath(),
		AddProviderRequest{BrokerID: f.ownBroker.ID, LocalPath: dir})
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	info, err := os.Stat(filepath.Join(dir, ".scion"))
	require.NoError(t, err, "the embedded broker's .scion directory is initialized")
	assert.True(t, info.IsDir())
}

func TestProviderLocalPath_RegisterOtherBrokerPathIsNotInitialized(t *testing.T) {
	f := brokerAssocSetup(t, "localpath-register")
	dir := t.TempDir()

	rec := doRequestAsUser(t, f.srv, f.projectOwner, http.MethodPost, "/api/v1/projects/register", RegisterProjectRequest{
		ID:       f.project.ID,
		Name:     f.project.Name,
		BrokerID: f.ownBroker.ID,
		Path:     dir,
	})

	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	_, err := os.Stat(filepath.Join(dir, ".scion"))
	assert.True(t, os.IsNotExist(err), "the hub must not initialize .scion for another broker, got %v", err)
}
