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
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

func TestComparePermissions(t *testing.T) {
	tests := []struct {
		name          string
		projectPerms    *store.GitHubTokenPermissions
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
			name:       "empty project permissions — no missing",
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
