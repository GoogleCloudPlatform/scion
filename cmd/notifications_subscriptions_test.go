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

package cmd

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/spf13/pflag"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	testCheckoutLocalID = "c7c7775e-e3a0-43de-9d26-274688d467d0"
	testHubProjectID    = "1dfdd6c7-ca85-4cb5-87fb-7ee9a8ebfa7f"
	testOtherProjectID  = "99999999-1111-4222-8333-444444444444"
)

type subscriptionsMockHub struct {
	mu      sync.Mutex
	queries []string
	subs    []hubclient.Subscription
}

func (m *subscriptionsMockHub) setSubscriptions(subs []hubclient.Subscription) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.subs = append([]hubclient.Subscription(nil), subs...)
}

func (m *subscriptionsMockHub) resetQueries() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.queries = nil
}

func (m *subscriptionsMockHub) recordedQueries() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]string(nil), m.queries...)
}

func newSubscriptionsMockServer(t *testing.T, initialSubs []hubclient.Subscription) (*subscriptionsMockHub, *httptest.Server) {
	t.Helper()
	mock := &subscriptionsMockHub{subs: append([]hubclient.Subscription(nil), initialSubs...)}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/notifications/subscriptions":
			projectID := r.URL.Query().Get("projectId")
			mock.mu.Lock()
			mock.queries = append(mock.queries, projectID)
			all := append([]hubclient.Subscription(nil), mock.subs...)
			mock.mu.Unlock()

			filtered := make([]hubclient.Subscription, 0, len(all))
			for _, sub := range all {
				if projectID != "" && sub.ProjectID != projectID {
					continue
				}
				filtered = append(filtered, sub)
			}
			_ = json.NewEncoder(w).Encode(filtered)
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/projects/"+testHubProjectID:
			_ = json.NewEncoder(w).Encode(hubclient.Project{
				ID:   testHubProjectID,
				Name: "scion-fork",
				Slug: "scion-fork",
			})
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/projects":
			q := r.URL.Query()
			var projects []hubclient.Project
			if q.Get("slug") == "scion-fork" || q.Get("name") == "scion-fork" {
				projects = append(projects, hubclient.Project{
					ID:   testHubProjectID,
					Name: "scion-fork",
					Slug: "scion-fork",
				})
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"projects": projects})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return mock, srv
}

func isolateSubscriptionsCLIState(t *testing.T) string {
	t.Helper()
	setupNoProjectPreRun(t)
	restoreAllSilenceUsage(t)

	origHubEndpoint, origHubSource := hubEndpoint, hubSource
	origProjectSource, origContextSlug := projectSource, contextProjectSlug
	origSubsJSON := subscriptionsJSON
	t.Cleanup(func() {
		hubEndpoint, hubSource = origHubEndpoint, origHubSource
		projectSource, contextProjectSlug = origProjectSource, origContextSlug
		subscriptionsJSON = origSubsJSON
		if f := notificationsSubscriptionsCmd.Flags().Lookup("all"); f != nil {
			_ = f.Value.Set("false")
			f.Changed = false
		}
		if f := notificationsSubscriptionsCmd.Flags().Lookup("json"); f != nil {
			_ = f.Value.Set("false")
			f.Changed = false
		}
		rootCmd.PersistentFlags().VisitAll(func(f *pflag.Flag) {
			f.Changed = false
		})
		rootCmd.SetArgs(nil)
		rootCmd.SetOut(nil)
		rootCmd.SetErr(nil)
	})

	for _, k := range []string{
		"SCION_HUB_ENDPOINT", "SCION_HUB_URL", "SCION_PROJECT_ID", "SCION_HUB_PROJECT_ID",
		"SCION_PROJECT", "SCION_AUTH_TOKEN", "SCION_HUB_TOKEN", "SCION_DEV_TOKEN",
		"SCION_DEV_TOKEN_FILE", "SCION_CLI_MODE", "SCION_HOST_UID", "SCION_NETWORK_MODE",
		"SCION_HUB_PROFILE",
	} {
		unsetEnvForTest(t, k)
	}

	hubEndpoint, hubSource = "", ""
	projectPath, projectSource, contextProjectSlug = "", "", ""
	globalMode = false
	outputFormat = ""
	subscriptionsJSON = false

	cwd, err := filepath.EvalSymlinks(t.TempDir())
	require.NoError(t, err)
	t.Chdir(cwd)
	return cwd
}

func executeSubscriptionsCLI(t *testing.T, args ...string) (stdout, stderr string, err error) {
	t.Helper()
	if f := notificationsSubscriptionsCmd.Flags().Lookup("all"); f != nil {
		_ = f.Value.Set("false")
		f.Changed = false
	}
	if f := notificationsSubscriptionsCmd.Flags().Lookup("json"); f != nil {
		_ = f.Value.Set("false")
		f.Changed = false
	}
	rootCmd.PersistentFlags().VisitAll(func(f *pflag.Flag) {
		f.Changed = false
	})
	projectPath, projectSource, contextProjectSlug = "", "", ""
	globalMode = false
	hubEndpoint, hubSource = "", ""
	outputFormat = ""
	subscriptionsJSON = false

	var outBuf, errBuf bytes.Buffer
	rootCmd.SetOut(&outBuf)
	rootCmd.SetErr(&errBuf)
	rootCmd.SetArgs(append([]string{"notifications", "subscriptions"}, args...))
	err = rootCmd.Execute()
	return outBuf.String(), errBuf.String(), err
}

func sampleSubscription(id, projectID string) hubclient.Subscription {
	return hubclient.Subscription{
		ID:                id,
		Scope:             store.SubscriptionScopeProject,
		SubscriberType:    store.SubscriberTypeUser,
		SubscriberID:      "user-1",
		ProjectID:         projectID,
		TriggerActivities: []string{"COMPLETED"},
		CreatedAt:         time.Date(2026, 11, 15, 12, 0, 0, 0, time.UTC),
		CreatedBy:         "user-1",
	}
}

// TestNotificationsSubscriptions_AgentContainerCheckoutWithDifferentProjectID
// reproduces ptone/scion#8127 inside an agent container: the cloned repository
// tracks .scion/project-id from another project, while SCION_PROJECT_ID names
// the agent's actual Hub project.
func TestNotificationsSubscriptions_AgentContainerCheckoutWithDifferentProjectID(t *testing.T) {
	cwd := isolateSubscriptionsCLIState(t)
	dotScion := filepath.Join(cwd, ".scion")
	require.NoError(t, os.MkdirAll(dotScion, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(dotScion, "project-id"), []byte(testCheckoutLocalID+"\n"), 0o644))

	mock, srv := newSubscriptionsMockServer(t, []hubclient.Subscription{
		sampleSubscription("sub-fork-1", testHubProjectID),
	})

	t.Setenv("SCION_HUB_ENDPOINT", srv.URL)
	t.Setenv("SCION_PROJECT_ID", testHubProjectID)
	t.Setenv("SCION_PROJECT", "scion-fork")
	t.Setenv("SCION_AUTH_TOKEN", "agent-token")
	t.Setenv("SCION_CLI_MODE", "agent")
	t.Setenv("SCION_HOST_UID", "1000")
	t.Setenv("SCION_NETWORK_MODE", "host")

	stdout, _, err := executeSubscriptionsCLI(t)
	require.NoError(t, err)
	assert.Equal(t, []string{testHubProjectID}, mock.recordedQueries(),
		"agent container must query SCION_PROJECT_ID rather than the checkout's .scion/project-id")
	assert.Contains(t, stdout, "sub-fork-1")
}

// TestNotificationsSubscriptions_UnlinkedCheckoutDoesNotSilentlyFilterByLocalProjectID
// reproduces ptone/scion#8127 on a workstation inside an unlinked git checkout
// that tracks .scion/project-id: the command must not send the unlinked local
// project-id as a Hub project filter and hide Hub subscriptions.
func TestNotificationsSubscriptions_UnlinkedCheckoutDoesNotSilentlyFilterByLocalProjectID(t *testing.T) {
	cwd := isolateSubscriptionsCLIState(t)
	repoDir := filepath.Join(cwd, "scion-checkout")
	dotScion := filepath.Join(repoDir, ".scion")
	require.NoError(t, os.MkdirAll(dotScion, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(dotScion, "project-id"), []byte(testCheckoutLocalID+"\n"), 0o644))
	t.Chdir(repoDir)

	mock, srv := newSubscriptionsMockServer(t, []hubclient.Subscription{
		sampleSubscription("sub-fork-1", testHubProjectID),
	})
	saveTestContext(t, srv.URL, testHubProjectID)
	t.Setenv("SCION_HUB_TOKEN", "user-token")

	stdout, stderr, err := executeSubscriptionsCLI(t)
	require.NoError(t, err)
	assert.Equal(t, []string{""}, mock.recordedQueries(),
		"unlinked checkout must not filter by its local .scion/project-id")
	assert.Contains(t, stdout, "sub-fork-1")
	assert.Contains(t, stderr, "not linked to a hub")

	mock.resetQueries()
	jsonOut, jsonErr, err := executeSubscriptionsCLI(t, "--json")
	require.NoError(t, err)
	assert.Equal(t, []string{""}, mock.recordedQueries())
	assert.Empty(t, jsonErr)
	var got []hubclient.Subscription
	require.NoError(t, json.Unmarshal([]byte(jsonOut), &got))
	require.Len(t, got, 1)
	assert.Equal(t, "sub-fork-1", got[0].ID)
}

// TestNotificationsSubscriptions_LinkedCheckoutUsesHubProjectIDAndAllFlag
// verifies that a linked checkout filters by hub.project_id (even when
// .scion/project-id differs), explains the project filter and --all when empty,
// and lists across all projects with --all.
func TestNotificationsSubscriptions_LinkedCheckoutUsesHubProjectIDAndAllFlag(t *testing.T) {
	cwd := isolateSubscriptionsCLIState(t)
	repoDir := filepath.Join(cwd, "scion-fork")
	dotScion := filepath.Join(repoDir, ".scion")
	require.NoError(t, os.MkdirAll(dotScion, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(dotScion, "project-id"), []byte(testCheckoutLocalID+"\n"), 0o644))

	extDir := config.GetProjectConfigDir(dotScion)
	require.NoError(t, os.MkdirAll(extDir, 0o755))
	settingsYAML := "schema_version: \"1\"\nhub:\n  linked: true\n  project_id: " + testHubProjectID + "\n"
	require.NoError(t, os.WriteFile(filepath.Join(extDir, "settings.yaml"), []byte(settingsYAML), 0o644))
	t.Chdir(repoDir)

	mock, srv := newSubscriptionsMockServer(t, []hubclient.Subscription{
		sampleSubscription("sub-fork-1", testHubProjectID),
		sampleSubscription("sub-other-2", testOtherProjectID),
	})
	saveTestContext(t, srv.URL, "")
	t.Setenv("SCION_HUB_TOKEN", "user-token")

	// 1. Default inside linked checkout queries hub.project_id, not .scion/project-id.
	stdout, _, err := executeSubscriptionsCLI(t)
	require.NoError(t, err)
	assert.Equal(t, []string{testHubProjectID}, mock.recordedQueries())
	assert.Contains(t, stdout, "sub-fork-1")
	assert.NotContains(t, stdout, "sub-other-2")

	// 2. When the filtered project has no subscriptions, output names the project and --all.
	mock.resetQueries()
	mock.setSubscriptions([]hubclient.Subscription{
		sampleSubscription("sub-other-2", testOtherProjectID),
	})
	stdout, _, err = executeSubscriptionsCLI(t)
	require.NoError(t, err)
	assert.Equal(t, []string{testHubProjectID}, mock.recordedQueries())
	assert.Contains(t, stdout, "scion-fork")
	assert.Contains(t, stdout, testHubProjectID)
	assert.Contains(t, stdout, "--all")

	// 3. --all lists across all projects.
	mock.resetQueries()
	stdout, _, err = executeSubscriptionsCLI(t, "--all")
	require.NoError(t, err)
	assert.Equal(t, []string{""}, mock.recordedQueries())
	assert.Contains(t, stdout, "sub-other-2")
}

// TestNotificationsSubscriptions_ProjectFlagSlugAndContextOutsideLocalProject
// verifies resolution outside any local .scion directory: 'scion use' context,
// --project/-g with a Hub slug, --all without a context project, and rejecting
// --all combined with --project.
func TestNotificationsSubscriptions_ProjectFlagSlugAndContextOutsideLocalProject(t *testing.T) {
	_ = isolateSubscriptionsCLIState(t)

	assertNoLocalProjectFlag(t, notificationsSubscriptionsCmd)

	mock, srv := newSubscriptionsMockServer(t, []hubclient.Subscription{
		sampleSubscription("sub-other-2", testOtherProjectID),
	})
	saveTestContext(t, srv.URL, testHubProjectID) // slug "foo" in saveTestContext
	t.Setenv("SCION_HUB_TOKEN", "user-token")

	// 1. Outside a local project, 'scion use' context project filters and is named when empty.
	stdout, _, err := executeSubscriptionsCLI(t)
	require.NoError(t, err)
	assert.Equal(t, []string{testHubProjectID}, mock.recordedQueries())
	assert.Contains(t, stdout, "foo")
	assert.Contains(t, stdout, testHubProjectID)
	assert.Contains(t, stdout, "--all")

	// 2. --project <slug> and -g <slug> resolve via Hub API to the Hub UUID.
	mock.setSubscriptions([]hubclient.Subscription{
		sampleSubscription("sub-fork-1", testHubProjectID),
		sampleSubscription("sub-other-2", testOtherProjectID),
	})
	for _, flagArgs := range [][]string{{"--project", "scion-fork"}, {"-g", "scion-fork"}} {
		mock.resetQueries()
		stdout, _, err = executeSubscriptionsCLI(t, flagArgs...)
		require.NoError(t, err)
		assert.Equal(t, []string{testHubProjectID}, mock.recordedQueries())
		assert.Contains(t, stdout, "sub-fork-1")
		assert.NotContains(t, stdout, "sub-other-2")
	}

	// 3. --all overrides ambient 'scion use' context and lists all projects.
	mock.resetQueries()
	stdout, _, err = executeSubscriptionsCLI(t, "--all")
	require.NoError(t, err)
	assert.Equal(t, []string{""}, mock.recordedQueries())
	assert.Contains(t, stdout, "sub-fork-1")
	assert.Contains(t, stdout, "sub-other-2")

	// 4. --all combined with explicit --project or --global is a usage error.
	_, _, err = executeSubscriptionsCLI(t, "--all", "--project", "scion-fork")
	require.Error(t, err)
	assert.True(t, isUsageError(err))

	_, _, err = executeSubscriptionsCLI(t, "--all", "--global")
	require.Error(t, err)
	assert.True(t, isUsageError(err))

	// 5. --project pointing to an unlinked local directory returns a clear error.
	unlinkedDir := filepath.Join(t.TempDir(), "unlinked-repo")
	require.NoError(t, os.MkdirAll(filepath.Join(unlinkedDir, ".scion"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(unlinkedDir, ".scion", "project-id"), []byte(testCheckoutLocalID+"\n"), 0o644))
	_, _, err = executeSubscriptionsCLI(t, "--project", unlinkedDir)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not linked to a hub")
}

func TestNotificationsSubscriptions_JSONIncludesAgentSlug(t *testing.T) {
	_ = isolateSubscriptionsCLIState(t)

	agentSub := hubclient.Subscription{
		ID:                "a1b2c3d4-5678-4000-8000-000000000001",
		Scope:             store.SubscriptionScopeAgent,
		AgentID:           "99999999-aaaa-4000-8000-000000000001",
		AgentSlug:         "worker-alpha",
		SubscriberType:    store.SubscriberTypeUser,
		SubscriberID:      "user-1",
		ProjectID:         testHubProjectID,
		TriggerActivities: []string{"COMPLETED", "ERROR"},
		CreatedAt:         time.Date(2026, 11, 15, 12, 0, 0, 0, time.UTC),
		CreatedBy:         "user-1",
	}

	_, srv := newSubscriptionsMockServer(t, []hubclient.Subscription{agentSub})
	saveTestContext(t, srv.URL, testHubProjectID)
	t.Setenv("SCION_HUB_TOKEN", "user-token")

	jsonOut, _, err := executeSubscriptionsCLI(t, "--json")
	require.NoError(t, err)
	var raw []map[string]any
	require.NoError(t, json.Unmarshal([]byte(jsonOut), &raw))
	require.Len(t, raw, 1)
	assert.Equal(t, "worker-alpha", raw[0]["agentSlug"])

	tableOut, _, err := executeSubscriptionsCLI(t)
	require.NoError(t, err)
	assert.Contains(t, tableOut, "worker-alpha")
}
