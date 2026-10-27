//go:build !no_sqlite

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
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// storageFixtureValues are the configured storage values of the fixture
// broker; none may appear in a response a non-admin can read.
var storageFixtureValues = []string{"10.9.8.7", "/fixture-export", "fixture-root", "fixture-export-id"}

// TestBrokerRecords_StorageValuesForAdminsOnly: the broker detail and list
// responses carry the storage descriptor's values for a hub admin only; a
// caller that can read the broker but is not an admin gets the backend and
// health without the values.
func TestBrokerRecords_StorageValuesForAdminsOnly(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()
	require.NoError(t, s.CreateRuntimeBroker(ctx, &store.RuntimeBroker{
		ID: tid("redact-broker"), Name: "redact-broker", Slug: "redact-broker", Endpoint: "http://localhost:9811",
		Status: store.BrokerStatusOnline,
		WorkspaceStorage: &api.BrokerWorkspaceStorage{Backend: "nfs", Homes: "local", NFS: &api.BrokerNFSWorkspaceStorage{
			Server: "10.9.8.7", Export: "/fixture-export", SubPathRoot: "fixture-root", Healthy: true, ExportID: "fixture-export-id"}},
	}))
	member := setupNonAdminUser(t, s, []string{"broker.read"})

	for _, path := range []string{"/api/v1/runtime-brokers/" + tid("redact-broker"), "/api/v1/runtime-brokers"} {
		rec := doRequestAsIdentity(t, srv, member, http.MethodGet, path, nil)
		require.Equal(t, http.StatusOK, rec.Code, "%s: %s", path, rec.Body.String())
		body := rec.Body.String()
		require.Contains(t, body, tid("redact-broker"), "%s: the broker is listed", path)
		require.Contains(t, body, `"backend":"nfs"`, "%s: the backend is kept", path)
		for _, v := range storageFixtureValues {
			require.NotContains(t, body, v, "%s: a non-admin sees the storage value %q", path, v)
		}

		admin := doRequest(t, srv, http.MethodGet, path, nil)
		require.Equal(t, http.StatusOK, admin.Code, "%s: %s", path, admin.Body.String())
		require.True(t, strings.Contains(admin.Body.String(), "10.9.8.7"), "%s: an admin sees the values", path)
	}
}
