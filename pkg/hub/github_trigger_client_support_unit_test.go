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
