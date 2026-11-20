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
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/hub/githubapp"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

func TestAllowGitHubComment_BurstRefillAndBoundedMap(t *testing.T) {
	now := time.Date(2026, 11, 17, 0, 0, 0, 0, time.UTC)
	srv := &Server{}
	srv.githubCommentLimiter.now = func() time.Time { return now }
	srv.githubCommentLimiter.maxBuckets = 8

	// Burst of 5 succeeds for project-a; 6th is immediately refused without GitHub.
	for i := 1; i <= 5; i++ {
		if err := srv.AllowGitHubComment("project-a"); err != nil {
			t.Fatalf("attempt %d unexpectedly refused: %v", i, err)
		}
	}
	if err := srv.AllowGitHubComment("project-a"); !errors.Is(err, ErrGitHubCommentRateLimited) {
		t.Fatalf("6th attempt = %v, want ErrGitHubCommentRateLimited", err)
	}

	// Independent per project: project-b still has its own burst.
	if err := srv.AllowGitHubComment("project-b"); err != nil {
		t.Fatalf("project-b unexpectedly refused: %v", err)
	}

	// Empty project ID is refused.
	if err := srv.AllowGitHubComment("   "); !errors.Is(err, ErrGitHubCommentRateLimited) {
		t.Fatalf("empty project ID = %v, want ErrGitHubCommentRateLimited", err)
	}

	// After 500ms (< 1s), project-a still has < 1 token; at +1s it refills 1 token.
	now = now.Add(500 * time.Millisecond)
	if err := srv.AllowGitHubComment("project-a"); !errors.Is(err, ErrGitHubCommentRateLimited) {
		t.Fatalf("at +500ms = %v, want ErrGitHubCommentRateLimited", err)
	}
	now = now.Add(500 * time.Millisecond)
	if err := srv.AllowGitHubComment("project-a"); err != nil {
		t.Fatalf("at +1s = %v, want allowed after 1/s refill", err)
	}
	if err := srv.AllowGitHubComment("project-a"); !errors.Is(err, ErrGitHubCommentRateLimited) {
		t.Fatalf("second post at +1s = %v, want ErrGitHubCommentRateLimited", err)
	}

	// Bounded map: touching many distinct project IDs never lets buckets grow past maxBuckets.
	for i := 0; i < 40; i++ {
		now = now.Add(10 * time.Millisecond)
		_ = srv.AllowGitHubComment(fmt.Sprintf("proj-%03d", i))
		srv.githubCommentLimiter.mu.Lock()
		n := len(srv.githubCommentLimiter.buckets)
		srv.githubCommentLimiter.mu.Unlock()
		if n > 8 {
			t.Fatalf("bucket count = %d after %d projects, exceeds cap 8", n, i+1)
		}
	}

	// Idle TTL sweep evicts stale buckets.
	now = now.Add(githubCommentBucketIdleTTL + time.Second)
	_ = srv.AllowGitHubComment("fresh-project")
	srv.githubCommentLimiter.mu.Lock()
	n := len(srv.githubCommentLimiter.buckets)
	srv.githubCommentLimiter.mu.Unlock()
	if n != 1 {
		t.Fatalf("bucket count after idle TTL sweep = %d, want 1", n)
	}
}

func TestCanonicalGitHubTokenPermKey(t *testing.T) {
	commentKey := canonicalGitHubTokenPermKey(githubapp.TokenPermissions{
		Issues:   "write",
		Metadata: "read",
	})
	enrichKey := canonicalGitHubTokenPermKey(githubapp.TokenPermissions{
		PullRequests: "read",
		Metadata:     "read",
	})
	defaultKey := canonicalGitHubTokenPermKey(githubapp.DefaultTokenPermissions())

	if commentKey != "issues=write;metadata=read" {
		t.Errorf("commentKey = %q, want issues=write;metadata=read", commentKey)
	}
	if enrichKey != "metadata=read;pull_requests=read" {
		t.Errorf("enrichKey = %q, want metadata=read;pull_requests=read", enrichKey)
	}
	if commentKey == enrichKey || commentKey == defaultKey || enrichKey == defaultKey {
		t.Errorf("permission keys collided: comment=%q enrich=%q default=%q", commentKey, enrichKey, defaultKey)
	}
}

func TestScopeRequestedGitHubTokenPermissions(t *testing.T) {
	readOnlyGrant := githubAssociationGrant{Owner: "acme", Repo: "widgets", WriteAllowed: false}
	writeGrant := githubAssociationGrant{Owner: "acme", Repo: "widgets", WriteAllowed: true}

	writeCases := []struct {
		name  string
		perms githubapp.TokenPermissions
	}{
		{"contents write", githubapp.TokenPermissions{Contents: "write"}},
		{"pull_requests write", githubapp.TokenPermissions{PullRequests: "write"}},
		{"issues write", githubapp.TokenPermissions{Issues: "write", Metadata: "read"}},
		{"metadata write", githubapp.TokenPermissions{Metadata: "write"}},
		{"checks write", githubapp.TokenPermissions{Checks: "write"}},
		{"actions write", githubapp.TokenPermissions{Actions: "write"}},
	}
	for _, tc := range writeCases {
		t.Run("read-only grant refuses "+tc.name, func(t *testing.T) {
			_, err := scopeRequestedGitHubTokenPermissions(tc.perms, nil, readOnlyGrant)
			if !errors.Is(err, ErrGitHubWriteNotPermitted) {
				t.Fatalf("scopeRequestedGitHubTokenPermissions(%+v, nil, readOnly) err = %v, want ErrGitHubWriteNotPermitted", tc.perms, err)
			}
		})
	}

	// Default project limit (stored == nil) allows DefaultTokenPermissions fields (contents, pull_requests, issues, metadata).
	defaultWriteAllowed := []struct {
		name  string
		perms githubapp.TokenPermissions
	}{
		{"contents write", githubapp.TokenPermissions{Contents: "write"}},
		{"pull_requests write", githubapp.TokenPermissions{PullRequests: "write"}},
		{"issues write", githubapp.TokenPermissions{Issues: "write", Metadata: "read"}},
		{"metadata read", githubapp.TokenPermissions{Metadata: "read"}},
	}
	for _, tc := range defaultWriteAllowed {
		t.Run("write grant default limit allows "+tc.name, func(t *testing.T) {
			got, err := scopeRequestedGitHubTokenPermissions(tc.perms, nil, writeGrant)
			if err != nil {
				t.Fatalf("scopeRequestedGitHubTokenPermissions(%+v, nil, write) err = %v", tc.perms, err)
			}
			if got != tc.perms {
				t.Fatalf("got %+v, want %+v", got, tc.perms)
			}
		})
	}

	// G3 S3: explicit project permission limit intersects and refuses requests that exceed it.
	storedReadOnlyIssues := &store.GitHubTokenPermissions{
		Contents: "read",
		Issues:   "read",
		Metadata: "read",
	}
	if _, err := scopeRequestedGitHubTokenPermissions(githubapp.TokenPermissions{Issues: "write", Metadata: "read"}, storedReadOnlyIssues, writeGrant); !errors.Is(err, ErrGitHubPermissionNotPermitted) {
		t.Fatalf("write request exceeding stored issues:read limit err = %v, want ErrGitHubPermissionNotPermitted", err)
	}
	if _, err := scopeRequestedGitHubTokenPermissions(githubapp.TokenPermissions{PullRequests: "read", Metadata: "read"}, storedReadOnlyIssues, writeGrant); !errors.Is(err, ErrGitHubPermissionNotPermitted) {
		t.Fatalf("read request for ungranted pull_requests err = %v, want ErrGitHubPermissionNotPermitted", err)
	}
	gotWithinLimit, err := scopeRequestedGitHubTokenPermissions(githubapp.TokenPermissions{Issues: "read", Metadata: "read"}, storedReadOnlyIssues, writeGrant)
	if err != nil {
		t.Fatalf("request within stored limit failed: %v", err)
	}
	if gotWithinLimit != (githubapp.TokenPermissions{Issues: "read", Metadata: "read"}) {
		t.Fatalf("got %+v, want issues:read, metadata:read", gotWithinLimit)
	}

	// metadata:read is always within a project's stored limit (GitHub grants it implicitly),
	// so {issues: write} without explicit metadata still allows comment fires, while metadata:write is refused.
	storedIssuesOnly := &store.GitHubTokenPermissions{Issues: "write"}
	gotIssuesWithMeta, err := scopeRequestedGitHubTokenPermissions(githubapp.TokenPermissions{Issues: "write", Metadata: "read"}, storedIssuesOnly, writeGrant)
	if err != nil {
		t.Fatalf("issues:write + metadata:read against stored {issues:write} failed: %v", err)
	}
	if gotIssuesWithMeta != (githubapp.TokenPermissions{Issues: "write", Metadata: "read"}) {
		t.Fatalf("got %+v, want issues:write, metadata:read", gotIssuesWithMeta)
	}
	if _, err := scopeRequestedGitHubTokenPermissions(githubapp.TokenPermissions{Metadata: "write"}, storedIssuesOnly, writeGrant); !errors.Is(err, ErrGitHubPermissionNotPermitted) {
		t.Fatalf("metadata:write against stored {issues:write} err = %v, want ErrGitHubPermissionNotPermitted", err)
	}

	// Stored custom limit with checks/actions write allows them under write grant.
	storedBroad := &store.GitHubTokenPermissions{
		Checks:  "write",
		Actions: "write",
	}
	for _, tc := range []githubapp.TokenPermissions{
		{Checks: "write"},
		{Actions: "write"},
	} {
		got, err := scopeRequestedGitHubTokenPermissions(tc, storedBroad, writeGrant)
		if err != nil {
			t.Fatalf("scopeRequestedGitHubTokenPermissions(%+v, broad, write) err = %v", tc, err)
		}
		if got != tc {
			t.Fatalf("got %+v, want %+v", got, tc)
		}
	}

	readPerms := githubapp.TokenPermissions{PullRequests: "read", Metadata: "read"}
	gotRead, err := scopeRequestedGitHubTokenPermissions(readPerms, nil, readOnlyGrant)
	if err != nil {
		t.Fatalf("read request under read-only grant failed: %v", err)
	}
	if gotRead != readPerms {
		t.Fatalf("got %+v, want %+v", gotRead, readPerms)
	}
}
