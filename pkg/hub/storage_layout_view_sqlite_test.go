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
	"fmt"
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
	adminUser := newSuperAdminUser(t, s, "redact-records-admin")

	for _, path := range []string{"/api/v1/runtime-brokers/" + tid("redact-broker"), "/api/v1/runtime-brokers"} {
		rec := doRequestAsIdentity(t, srv, member, http.MethodGet, path, nil)
		require.Equal(t, http.StatusOK, rec.Code, "%s: %s", path, rec.Body.String())
		body := rec.Body.String()
		require.Contains(t, body, tid("redact-broker"), "%s: the broker is listed", path)
		require.Contains(t, body, `"backend":"nfs"`, "%s: the backend is kept", path)
		for _, v := range storageFixtureValues {
			require.NotContains(t, body, v, "%s: a non-admin sees the storage value %q", path, v)
		}
		// The value fields are omitted, not empty, and the descriptor says
		// it was redacted.
		require.Contains(t, body, `"redacted":true`, "%s: the descriptor is marked redacted", path)
		for _, key := range []string{`"server":`, `"export":`, `"subPathRoot":`, `"exportId":`} {
			require.NotContains(t, body, key, "%s: a redacted value field is present", path)
		}

		admin := doRequestAsUser(t, srv, adminUser, http.MethodGet, path, nil)
		require.Equal(t, http.StatusOK, admin.Code, "%s: %s", path, admin.Body.String())
		adminBody := admin.Body.String()
		for _, v := range storageFixtureValues {
			require.True(t, strings.Contains(adminBody, v), "%s: an admin sees the value %q", path, v)
		}
		require.NotContains(t, adminBody, `"redacted":true`, "%s: the admin record is not redacted", path)
	}
}

// TestRegisterProject_BrokerStorageValuesForAdminsOnly: the project
// registration response returns the linked broker's storage descriptor
// without values (redacted:true) to a non-admin broker owner, and with the
// values to a hub admin.
func TestRegisterProject_BrokerStorageValuesForAdminsOnly(t *testing.T) {
	f := brokerAssocSetup(t, "redact-register")
	ctx := context.Background()
	b, err := f.store.GetRuntimeBroker(ctx, f.otherBroker.ID)
	require.NoError(t, err)
	b.WorkspaceStorage = &api.BrokerWorkspaceStorage{Backend: "nfs", Homes: "local", NFS: &api.BrokerNFSWorkspaceStorage{
		Server: "10.9.8.7", Export: "/fixture-export", SubPathRoot: "fixture-root", Healthy: true, ExportID: "fixture-export-id"}}
	require.NoError(t, f.store.UpdateRuntimeBroker(ctx, b))

	rec := doRequestAsUser(t, f.srv, f.brokerOwner, http.MethodPost, "/api/v1/projects/register", RegisterProjectRequest{
		Name: "Redact Register Owner", BrokerID: f.otherBroker.ID,
	})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	body := rec.Body.String()
	require.Contains(t, body, `"redacted":true`, "the owner's response marks the descriptor redacted")
	for _, v := range storageFixtureValues {
		require.NotContains(t, body, v, "a non-admin broker owner sees the storage value %q", v)
	}

	admin := newSuperAdminUser(t, f.store, "redact-register-admin")
	rec = doRequestAsUser(t, f.srv, admin, http.MethodPost, "/api/v1/projects/register", RegisterProjectRequest{
		Name: "Redact Register Admin", BrokerID: f.otherBroker.ID,
	})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	for _, v := range storageFixtureValues {
		require.Contains(t, rec.Body.String(), v, "an admin sees the storage value %q", v)
	}
	require.NotContains(t, rec.Body.String(), `"redacted":true`)
}

// storageAdminCtx returns a context whose identity is a new super-admin
// user (store-backed, so the authorization pipeline grants hub.config.read).
func storageAdminCtx(t *testing.T, s store.Store, name string) context.Context {
	t.Helper()
	u := newSuperAdminUser(t, s, name)
	return contextWithIdentity(context.Background(), NewAuthenticatedUser(u.ID, u.Email, u.DisplayName, u.Role, "web"))
}

// TestStorageValuesAllowed: only a caller holding hub.config.read (a hub
// super-admin here) may see storage layout values; a member with
// broker.read and an anonymous caller may not.
func TestStorageValuesAllowed(t *testing.T) {
	srv, st := testServer(t)
	ctx := context.Background()
	admin := storageAdminCtx(t, st, "storage-values-admin")
	member := contextWithIdentity(ctx, setupNonAdminUser(t, st, []string{"broker.read"}))
	if !srv.storageValuesAllowed(admin) {
		t.Error("an admin may not see values")
	}
	if srv.storageValuesAllowed(member) || srv.storageValuesAllowed(ctx) {
		t.Error("a member or an anonymous caller may see values")
	}

	b := store.RuntimeBroker{ID: "b", WorkspaceStorage: &api.BrokerWorkspaceStorage{Backend: "nfs", Homes: "nfs",
		NFS:    &api.BrokerNFSWorkspaceStorage{Server: "10.9.8.7", Export: "/fixture-export", SubPathRoot: "fixture-root", Healthy: true, ExportID: "fixture-export-id"},
		Volume: &api.BrokerVolumeWorkspaceStorage{ClaimName: "fixture-claim", SubPathRoot: "fixture-root"}}}
	for name, c := range map[string]context.Context{"member": member, "anonymous": ctx} {
		got := srv.brokerStorageForViewer(c, b, false)
		if v := fmt.Sprintf("%+v %+v", *got.WorkspaceStorage.NFS, *got.WorkspaceStorage.Volume); strings.Contains(v, "fixture") || strings.Contains(v, "10.9.8.7") {
			t.Errorf("%s: values shown: %s", name, v)
		}
		if got.WorkspaceStorage.Backend != "nfs" || got.WorkspaceStorage.Homes != "nfs" || !got.WorkspaceStorage.NFS.Healthy {
			t.Errorf("%s: backend, homes or health dropped: %+v", name, got.WorkspaceStorage)
		}
		if !got.WorkspaceStorage.Redacted {
			t.Errorf("%s: the redacted descriptor is not marked Redacted", name)
		}
		if b.WorkspaceStorage.NFS.Server != "10.9.8.7" || b.WorkspaceStorage.Redacted {
			t.Fatal("the stored record was modified")
		}
	}
	if got := srv.brokerStorageForViewer(admin, b, false); got.WorkspaceStorage.NFS.Server != "10.9.8.7" || got.WorkspaceStorage.Redacted {
		t.Error("an admin does not see the values")
	}
	if got := srv.brokerStorageForViewer(member, b, true); got.WorkspaceStorage.NFS.Export != "/fixture-export" {
		t.Error("the broker reading its own record does not see the values")
	}
}
