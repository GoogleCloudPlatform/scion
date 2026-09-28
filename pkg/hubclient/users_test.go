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

package hubclient

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/apiclient"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestUserService_List_SearchQueryEncoding verifies ListUsersOptions.Search
// is encoded as the "search" query param the Hub's listUsers handler expects
// (pkg/hub/handlers_users_core.go: store.UserFilter.Search). This is how the
// CLI's `scion list --owner <name-or-email>` resolves a name/email to a user
// ID (ptone/scion#2146).
func TestUserService_List_SearchQueryEncoding(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "alice@example.com", r.URL.Query().Get("search"))
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"users": []}`))
	}))
	defer server.Close()

	client, err := New(server.URL)
	require.NoError(t, err)

	_, err = client.Users().List(context.Background(), &ListUsersOptions{Search: "alice@example.com"})
	require.NoError(t, err)
}

// TestUserService_List_SearchOmittedWhenUnset verifies no "search" param is
// sent when Search is empty.
func TestUserService_List_SearchOmittedWhenUnset(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, has := r.URL.Query()["search"]
		assert.False(t, has)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"users": []}`))
	}))
	defer server.Close()

	client, err := New(server.URL)
	require.NoError(t, err)

	_, err = client.Users().List(context.Background(), &ListUsersOptions{})
	require.NoError(t, err)
}

// TestUserService_List_PageAndSearchCombine verifies Search combines with
// pagination options on the same request.
func TestUserService_List_PageAndSearchCombine(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		query := r.URL.Query()
		assert.Equal(t, "bob", query.Get("search"))
		assert.Equal(t, "10", query.Get("limit"))
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"users": []}`))
	}))
	defer server.Close()

	client, err := New(server.URL)
	require.NoError(t, err)

	_, err = client.Users().List(context.Background(), &ListUsersOptions{
		Search: "bob",
		Page:   apiclient.PageOptions{Limit: 10},
	})
	require.NoError(t, err)
}

// TestUserService_List_NilOptions verifies List tolerates a nil options
// pointer (mirrors AgentService.List's nil-safety).
func TestUserService_List_NilOptions(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"users": []}`))
	}))
	defer server.Close()

	client, err := New(server.URL)
	require.NoError(t, err)

	_, err = client.Users().List(context.Background(), nil)
	require.NoError(t, err)
}
