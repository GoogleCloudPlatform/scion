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
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/hub/githubapp"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

func TestComparePermissions(t *testing.T) {
	tests := []struct {
		name          string
		projectPerms  *store.GitHubTokenPermissions
		appPerms      map[string]string
		expectedCount int
		expectedFirst string // first missing perm substring (if any)
	}{
		{
			name: "all permissions satisfied",
			projectPerms: &store.GitHubTokenPermissions{
				Contents:     "write",
				PullRequests: "write",
				Metadata:     "read",
			},
			appPerms: map[string]string{
				"contents":      "write",
				"pull_requests": "write",
				"metadata":      "read",
			},
			expectedCount: 0,
		},
		{
			name: "app has write, project requests read — satisfied",
			projectPerms: &store.GitHubTokenPermissions{
				Contents: "read",
			},
			appPerms: map[string]string{
				"contents": "write",
			},
			expectedCount: 0,
		},
		{
			name: "app has read, project requests write — insufficient",
			projectPerms: &store.GitHubTokenPermissions{
				Contents: "write",
			},
			appPerms: map[string]string{
				"contents": "read",
			},
			expectedCount: 1,
			expectedFirst: "contents:write (app has read)",
		},
		{
			name: "app missing permission entirely",
			projectPerms: &store.GitHubTokenPermissions{
				Issues: "write",
			},
			appPerms: map[string]string{
				"contents": "write",
			},
			expectedCount: 1,
			expectedFirst: "issues:write",
		},
		{
			name: "multiple missing permissions",
			projectPerms: &store.GitHubTokenPermissions{
				Contents: "write",
				Issues:   "read",
				Checks:   "write",
			},
			appPerms: map[string]string{
				"contents": "read", // insufficient
				// issues: missing entirely
				// checks: missing entirely
			},
			expectedCount: 3,
		},
		{
			name:         "empty project permissions — no missing",
			projectPerms: &store.GitHubTokenPermissions{},
			appPerms: map[string]string{
				"contents": "write",
			},
			expectedCount: 0,
		},
		{
			name: "empty app permissions — all missing",
			projectPerms: &store.GitHubTokenPermissions{
				Contents: "read",
				Metadata: "read",
			},
			appPerms:      map[string]string{},
			expectedCount: 2,
		},
		{
			name:         "nil project permissions — no missing",
			projectPerms: nil,
			appPerms: map[string]string{
				"contents": "write",
			},
			expectedCount: 0,
		},
		{
			name: "app admin satisfies write and read",
			projectPerms: &store.GitHubTokenPermissions{
				Contents: "write",
				Metadata: "read",
			},
			appPerms: map[string]string{
				"contents": "admin",
				"metadata": "admin",
			},
			expectedCount: 0,
		},
		{
			name: "unknown or empty app level treated as missing without leaking raw level",
			projectPerms: &store.GitHubTokenPermissions{
				Contents: "write",
				Metadata: "read",
			},
			appPerms: map[string]string{
				"contents": "UPSTREAM_BOGUS_LEVEL",
				"metadata": "",
			},
			expectedCount: 2,
			expectedFirst: "contents:write",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			missing := comparePermissions(tc.projectPerms, tc.appPerms)
			if len(missing) != tc.expectedCount {
				t.Errorf("expected %d missing permissions, got %d: %v",
					tc.expectedCount, len(missing), missing)
			}
			if tc.expectedFirst != "" && len(missing) > 0 && missing[0] != tc.expectedFirst {
				t.Errorf("expected first missing = %q, got %q", tc.expectedFirst, missing[0])
			}
		})
	}
}

func TestHandleGitHubAppSyncPermissions_MethodNotAllowed(t *testing.T) {
	srv, _ := webhookTestServer(t)

	// GET should be rejected
	rec := doRequest(t, srv, "GET", "/api/v1/github-app/sync-permissions", nil)
	if rec.Code != 405 {
		t.Errorf("expected 405 for GET, got %d", rec.Code)
	}
}

func TestHandleGitHubAppSyncPermissions_NotConfigured(t *testing.T) {
	srv, _ := testServer(t)

	// App not configured (AppID = 0), should return 502
	rec := doRequest(t, srv, "POST", "/api/v1/github-app/sync-permissions", nil)
	if rec.Code != 502 {
		t.Errorf("expected 502 for unconfigured app, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestSyncAppPermissions_NoStoredPermissionsMarkedDegraded(t *testing.T) {
	ctx := context.Background()

	runSyncCase := func(t *testing.T, appPerms map[string]string, stored *store.GitHubTokenPermissions) (*store.Project, []map[string]interface{}) {
		t.Helper()
		mockGH := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != "/app" || r.Method != http.MethodGet {
				http.NotFound(w, r)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"id":          42,
				"slug":        "sync-test-app",
				"permissions": appPerms,
			})
		}))
		t.Cleanup(mockGH.Close)

		srv, st := testServer(t)
		srv.mu.Lock()
		srv.config.GitHubAppConfig = GitHubAppServerConfig{
			AppID:      42,
			PrivateKey: testGitHubAppPrivateKeyPEM(t),
			APIBaseURL: mockGH.URL,
		}
		srv.mu.Unlock()

		instID := int64(6001)
		if err := st.CreateGitHubInstallation(ctx, &store.GitHubInstallation{
			InstallationID: instID,
			AccountLogin:   "acme",
			AccountType:    "Organization",
			AppID:          42,
			Repositories:   []string{"acme/widgets"},
			Status:         store.GitHubInstallationStatusActive,
		}); err != nil {
			t.Fatalf("CreateGitHubInstallation: %v", err)
		}

		projID := tid("proj-sync-defaults")
		if err := st.CreateProject(ctx, &store.Project{
			ID:                   projID,
			Name:                 "Sync Defaults Project",
			Slug:                 "sync-defaults-project",
			GitRemote:            "https://github.com/acme/widgets.git",
			GitHubInstallationID: &instID,
			GitHubPermissions:    stored,
			GitHubAppStatus:      &store.GitHubAppProjectStatus{State: store.GitHubAppStateOK},
			Created:              time.Now(),
			Updated:              time.Now(),
		}); err != nil {
			t.Fatalf("CreateProject: %v", err)
		}

		_, affected, err := srv.syncAppPermissions(ctx)
		if err != nil {
			t.Fatalf("syncAppPermissions: %v", err)
		}
		got, err := st.GetProject(ctx, projID)
		if err != nil {
			t.Fatalf("GetProject: %v", err)
		}
		return got, affected
	}

	t.Run("read-only app keeps nil-permissions project ok", func(t *testing.T) {
		proj, affected := runSyncCase(t, map[string]string{
			"contents": "read",
			"metadata": "read",
		}, nil)
		if len(affected) != 0 {
			t.Fatalf("expected 0 affected projects, got %d: %v", len(affected), affected)
		}
		if proj.GitHubAppStatus == nil || proj.GitHubAppStatus.State != store.GitHubAppStateOK {
			t.Fatalf("expected ok state, got %+v", proj.GitHubAppStatus)
		}
	})

	t.Run("git-only app without issues keeps nil-permissions project ok", func(t *testing.T) {
		proj, affected := runSyncCase(t, map[string]string{
			"contents":      "write",
			"pull_requests": "write",
			"metadata":      "read",
		}, nil)
		if len(affected) != 0 {
			t.Fatalf("expected 0 affected projects, got %d: %v", len(affected), affected)
		}
		if proj.GitHubAppStatus == nil || proj.GitHubAppStatus.State != store.GitHubAppStateOK {
			t.Fatalf("expected ok state, got %+v", proj.GitHubAppStatus)
		}
	})

	t.Run("empty app grant marks nil-permissions project degraded", func(t *testing.T) {
		proj, affected := runSyncCase(t, map[string]string{}, nil)
		if len(affected) != 1 {
			t.Fatalf("expected 1 affected project, got %d: %v", len(affected), affected)
		}
		if proj.GitHubAppStatus == nil || proj.GitHubAppStatus.State != store.GitHubAppStateDegraded {
			t.Fatalf("expected degraded state, got %+v", proj.GitHubAppStatus)
		}
		if proj.GitHubAppStatus.ErrorCode != githubapp.ErrCodePermissionDenied {
			t.Errorf("expected error_code %q, got %q", githubapp.ErrCodePermissionDenied, proj.GitHubAppStatus.ErrorCode)
		}
	})

	t.Run("disjoint app grant marks nil-permissions project degraded", func(t *testing.T) {
		proj, affected := runSyncCase(t, map[string]string{
			"actions": "write",
		}, nil)
		if len(affected) != 1 {
			t.Fatalf("expected 1 affected project, got %d: %v", len(affected), affected)
		}
		if proj.GitHubAppStatus == nil || proj.GitHubAppStatus.State != store.GitHubAppStateDegraded {
			t.Fatalf("expected degraded state, got %+v", proj.GitHubAppStatus)
		}
		if proj.GitHubAppStatus.ErrorCode != githubapp.ErrCodePermissionDenied {
			t.Errorf("expected error_code %q, got %q", githubapp.ErrCodePermissionDenied, proj.GitHubAppStatus.ErrorCode)
		}
	})

	t.Run("full app grant keeps nil-permissions project ok", func(t *testing.T) {
		proj, affected := runSyncCase(t, map[string]string{
			"contents":      "write",
			"pull_requests": "write",
			"issues":        "write",
			"metadata":      "read",
		}, nil)
		if len(affected) != 0 {
			t.Fatalf("expected 0 affected projects, got %d: %v", len(affected), affected)
		}
		if proj.GitHubAppStatus == nil || proj.GitHubAppStatus.State != store.GitHubAppStateOK {
			t.Fatalf("expected ok state, got %+v", proj.GitHubAppStatus)
		}
	})

	t.Run("explicit read-only stored permissions on read-only app stays ok", func(t *testing.T) {
		proj, affected := runSyncCase(t, map[string]string{
			"contents": "read",
			"metadata": "read",
		}, &store.GitHubTokenPermissions{
			Contents: "read",
			Metadata: "read",
		})
		if len(affected) != 0 {
			t.Fatalf("expected 0 affected projects, got %d: %v", len(affected), affected)
		}
		if proj.GitHubAppStatus == nil || proj.GitHubAppStatus.State != store.GitHubAppStateOK {
			t.Fatalf("expected ok state, got %+v", proj.GitHubAppStatus)
		}
	})

	t.Run("explicit stored permissions exceeding git-only app marks degraded", func(t *testing.T) {
		proj, affected := runSyncCase(t, map[string]string{
			"contents":      "write",
			"pull_requests": "write",
			"metadata":      "read",
		}, &store.GitHubTokenPermissions{
			Contents: "write",
			Issues:   "write",
		})
		if len(affected) != 1 {
			t.Fatalf("expected 1 affected project, got %d: %v", len(affected), affected)
		}
		if proj.GitHubAppStatus == nil || proj.GitHubAppStatus.State != store.GitHubAppStateDegraded {
			t.Fatalf("expected degraded state, got %+v", proj.GitHubAppStatus)
		}
		if proj.GitHubAppStatus.ErrorCode != githubapp.ErrCodePermissionDenied {
			t.Errorf("expected error_code %q, got %q", githubapp.ErrCodePermissionDenied, proj.GitHubAppStatus.ErrorCode)
		}
		if !strings.Contains(proj.GitHubAppStatus.ErrorMessage, "issues:write") {
			t.Errorf("expected ErrorMessage to mention issues:write, got %q", proj.GitHubAppStatus.ErrorMessage)
		}
	})
}

func TestHandleGitHubAppSyncPermissions_ErrorDoesNotLeakUpstream(t *testing.T) {
	mockGH := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"message":"SECRET_UPSTREAM_SYNC_DETAIL"}`))
	}))
	t.Cleanup(mockGH.Close)

	srv, _ := testServer(t)
	srv.mu.Lock()
	srv.config.GitHubAppConfig = GitHubAppServerConfig{
		AppID:      42,
		PrivateKey: testGitHubAppPrivateKeyPEM(t),
		APIBaseURL: mockGH.URL,
	}
	srv.mu.Unlock()

	rec := doRequest(t, srv, http.MethodPost, "/api/v1/github-app/sync-permissions", nil)
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("expected 502, got %d: %s", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "SECRET_UPSTREAM_SYNC_DETAIL") {
		t.Fatalf("sync-permissions response leaked upstream detail: %s", rec.Body.String())
	}
}
