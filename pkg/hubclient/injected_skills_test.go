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

package hubclient_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	testProjID = "proj-1111-2222"
	testEntry1 = "entry-aaaa-1111"
	testEntry2 = "entry-bbbb-2222"
)

func newInjectedSkillsServer(t *testing.T) *httptest.Server {
	t.Helper()

	projEntries := []api.SkillInjectionEntry{
		{ID: testEntry1, SkillURI: "scion://skill-a", SkillAs: "alias-a", Optional: false, SortOrder: 0},
		{ID: testEntry2, SkillURI: "scion://skill-b", Optional: true, SortOrder: 1},
	}
	userEntries := []api.SkillInjectionEntry{
		{ID: "user-entry-1", SkillURI: "scion://user-skill", Optional: false},
	}

	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")

		switch {
		// ----- Project injected-skills -----
		case r.URL.Path == "/api/v1/projects/"+testProjID+"/injected-skills" && r.Method == http.MethodGet:
			_ = json.NewEncoder(w).Encode(api.SkillInjectionList{Entries: projEntries})

		case r.URL.Path == "/api/v1/projects/"+testProjID+"/injected-skills" && r.Method == http.MethodPost:
			var req hubclient.AddInjectedSkillRequest
			require.NoError(t, json.NewDecoder(r.Body).Decode(&req))
			entry := api.SkillInjectionEntry{
				ID:       "new-proj-entry",
				SkillURI: req.SkillURI,
				SkillAs:  req.SkillAs,
				Optional: req.Optional,
			}
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(entry)

		case r.URL.Path == "/api/v1/projects/"+testProjID+"/injected-skills" && r.Method == http.MethodPut:
			var req struct {
				Entries []api.SkillInjectionEntry `json:"entries"`
			}
			require.NoError(t, json.NewDecoder(r.Body).Decode(&req))
			_ = json.NewEncoder(w).Encode(api.SkillInjectionList{Entries: req.Entries})

		case r.URL.Path == "/api/v1/projects/"+testProjID+"/injected-skills/"+testEntry1 && r.Method == http.MethodDelete:
			w.WriteHeader(http.StatusNoContent)

		case r.URL.Path == "/api/v1/projects/"+testProjID+"/injected-skills/"+testEntry2 && r.Method == http.MethodDelete:
			w.WriteHeader(http.StatusNoContent)

		// ----- User injected-skills -----
		case r.URL.Path == "/api/v1/users/me/injected-skills" && r.Method == http.MethodGet:
			_ = json.NewEncoder(w).Encode(api.SkillInjectionList{Entries: userEntries})

		case r.URL.Path == "/api/v1/users/me/injected-skills" && r.Method == http.MethodPost:
			var req hubclient.AddInjectedSkillRequest
			require.NoError(t, json.NewDecoder(r.Body).Decode(&req))
			entry := api.SkillInjectionEntry{
				ID:       "new-user-entry",
				SkillURI: req.SkillURI,
				Optional: req.Optional,
			}
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(entry)

		case r.URL.Path == "/api/v1/users/me/injected-skills" && r.Method == http.MethodPut:
			var req struct {
				Entries []api.SkillInjectionEntry `json:"entries"`
			}
			require.NoError(t, json.NewDecoder(r.Body).Decode(&req))
			_ = json.NewEncoder(w).Encode(api.SkillInjectionList{Entries: req.Entries})

		case r.URL.Path == "/api/v1/users/me/injected-skills/user-entry-1" && r.Method == http.MethodDelete:
			w.WriteHeader(http.StatusNoContent)

		default:
			w.WriteHeader(http.StatusNotFound)
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"code":    "not_found",
				"message": "not found",
			})
		}
	}))
}

func TestProjectInjectedSkills_List(t *testing.T) {
	server := newInjectedSkillsServer(t)
	defer server.Close()

	c, err := hubclient.New(server.URL)
	require.NoError(t, err)

	svc := c.ProjectInjectedSkills(testProjID)
	list, err := svc.List(context.Background())
	require.NoError(t, err)
	assert.Len(t, list.Entries, 2)
	assert.Equal(t, testEntry1, list.Entries[0].ID)
	assert.Equal(t, "scion://skill-a", list.Entries[0].SkillURI)
	assert.Equal(t, "alias-a", list.Entries[0].SkillAs)
	assert.False(t, list.Entries[0].Optional)
	assert.Equal(t, testEntry2, list.Entries[1].ID)
	assert.True(t, list.Entries[1].Optional)
}

func TestProjectInjectedSkills_Add(t *testing.T) {
	server := newInjectedSkillsServer(t)
	defer server.Close()

	c, err := hubclient.New(server.URL)
	require.NoError(t, err)

	svc := c.ProjectInjectedSkills(testProjID)
	entry, err := svc.Add(context.Background(), &hubclient.AddInjectedSkillRequest{
		SkillURI: "scion://new-skill",
		SkillAs:  "alias",
		Optional: true,
	})
	require.NoError(t, err)
	assert.Equal(t, "new-proj-entry", entry.ID)
	assert.Equal(t, "scion://new-skill", entry.SkillURI)
	assert.Equal(t, "alias", entry.SkillAs)
	assert.True(t, entry.Optional)
}

func TestProjectInjectedSkills_Set(t *testing.T) {
	server := newInjectedSkillsServer(t)
	defer server.Close()

	c, err := hubclient.New(server.URL)
	require.NoError(t, err)

	svc := c.ProjectInjectedSkills(testProjID)
	newEntries := []api.SkillInjectionEntry{
		{SkillURI: "scion://skill-x"},
		{SkillURI: "scion://skill-y", Optional: true},
	}
	list, err := svc.Set(context.Background(), newEntries)
	require.NoError(t, err)
	assert.Len(t, list.Entries, 2)
	assert.Equal(t, "scion://skill-x", list.Entries[0].SkillURI)
}

func TestProjectInjectedSkills_Remove(t *testing.T) {
	server := newInjectedSkillsServer(t)
	defer server.Close()

	c, err := hubclient.New(server.URL)
	require.NoError(t, err)

	svc := c.ProjectInjectedSkills(testProjID)
	err = svc.Remove(context.Background(), testEntry1)
	require.NoError(t, err)
}

func TestProjectInjectedSkills_RemoveNotFound(t *testing.T) {
	server := newInjectedSkillsServer(t)
	defer server.Close()

	c, err := hubclient.New(server.URL)
	require.NoError(t, err)

	svc := c.ProjectInjectedSkills(testProjID)
	err = svc.Remove(context.Background(), "nonexistent-id")
	assert.Error(t, err)
}

func TestUserInjectedSkills_List(t *testing.T) {
	server := newInjectedSkillsServer(t)
	defer server.Close()

	c, err := hubclient.New(server.URL)
	require.NoError(t, err)

	svc := c.UserInjectedSkills()
	list, err := svc.List(context.Background())
	require.NoError(t, err)
	assert.Len(t, list.Entries, 1)
	assert.Equal(t, "scion://user-skill", list.Entries[0].SkillURI)
}

func TestUserInjectedSkills_Add(t *testing.T) {
	server := newInjectedSkillsServer(t)
	defer server.Close()

	c, err := hubclient.New(server.URL)
	require.NoError(t, err)

	svc := c.UserInjectedSkills()
	entry, err := svc.Add(context.Background(), &hubclient.AddInjectedSkillRequest{
		SkillURI: "scion://my-user-skill",
		Optional: false,
	})
	require.NoError(t, err)
	assert.Equal(t, "new-user-entry", entry.ID)
	assert.Equal(t, "scion://my-user-skill", entry.SkillURI)
}

func TestUserInjectedSkills_Set(t *testing.T) {
	server := newInjectedSkillsServer(t)
	defer server.Close()

	c, err := hubclient.New(server.URL)
	require.NoError(t, err)

	svc := c.UserInjectedSkills()
	newEntries := []api.SkillInjectionEntry{
		{SkillURI: "scion://skill-z", Optional: true},
	}
	list, err := svc.Set(context.Background(), newEntries)
	require.NoError(t, err)
	assert.Len(t, list.Entries, 1)
	assert.Equal(t, "scion://skill-z", list.Entries[0].SkillURI)
}

func TestUserInjectedSkills_Remove(t *testing.T) {
	server := newInjectedSkillsServer(t)
	defer server.Close()

	c, err := hubclient.New(server.URL)
	require.NoError(t, err)

	svc := c.UserInjectedSkills()
	err = svc.Remove(context.Background(), "user-entry-1")
	require.NoError(t, err)
}

func TestUserInjectedSkills_RemoveNotFound(t *testing.T) {
	server := newInjectedSkillsServer(t)
	defer server.Close()

	c, err := hubclient.New(server.URL)
	require.NoError(t, err)

	svc := c.UserInjectedSkills()
	err = svc.Remove(context.Background(), "nonexistent-user-entry")
	assert.Error(t, err)
}
