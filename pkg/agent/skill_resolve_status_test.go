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

package agent

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
)

// The runtime broker stores a fixed reason phrase for skill resolver errors,
// plus the upstream HTTP status when one is known. These tests check that
// the resolvers record that status on ResolveError.Status.

func TestGitHubSkillResolver_ClientErrorRecordsStatus(t *testing.T) {
	server, mux := newTestGitHubServer(t)
	mux.HandleFunc("/repos/owner/repo/commits/main", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnprocessableEntity)
		_, _ = w.Write([]byte("arbitrary upstream body"))
	})
	resolver := newTestGitHubResolver(server)

	result, err := resolver.Resolve(context.Background(), []api.SkillReference{
		{URI: "gh://owner/repo/my-skill@main"},
	}, ResolveOpts{})
	if err != nil {
		t.Fatalf("Resolve failed: %v", err)
	}
	if len(result.Errors) != 1 {
		t.Fatalf("expected 1 error, got %d", len(result.Errors))
	}
	re := result.Errors[0]
	if re.Status != http.StatusUnprocessableEntity {
		t.Errorf("Status = %d, want %d", re.Status, http.StatusUnprocessableEntity)
	}
	if re.Code != SkillErrCodeResolveFailed {
		t.Errorf("Code = %q, want %q (classification unchanged)", re.Code, SkillErrCodeResolveFailed)
	}
}

func TestGitHubSkillResolver_NotFoundRecordsStatus(t *testing.T) {
	server, mux := newTestGitHubServer(t)
	mux.HandleFunc("/repos/owner/repo/commits/main", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	})
	resolver := newTestGitHubResolver(server)

	result, err := resolver.Resolve(context.Background(), []api.SkillReference{
		{URI: "gh://owner/repo/my-skill@main"},
	}, ResolveOpts{})
	if err != nil {
		t.Fatalf("Resolve failed: %v", err)
	}
	if len(result.Errors) != 1 {
		t.Fatalf("expected 1 error, got %d", len(result.Errors))
	}
	if got := result.Errors[0]; got.Code != SkillErrCodeNotFound || got.Status != http.StatusNotFound {
		t.Errorf("got Code %q Status %d, want %q %d", got.Code, got.Status, SkillErrCodeNotFound, http.StatusNotFound)
	}
}

func TestGCPSkillResolver_ErrorRecordsStatus(t *testing.T) {
	for _, status := range []int{http.StatusNotFound, http.StatusForbidden, http.StatusTeapot} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(status)
				_, _ = w.Write([]byte("arbitrary upstream body"))
			}))
			defer server.Close()

			resolver := &GCPSkillResolver{
				registryLookup: func(_ context.Context, _ string) (*RegistryLookupResult, error) {
					return &RegistryLookupResult{
						Name: "reg", Endpoint: server.URL + "/skills", Type: "gcp", Status: "active",
					}, nil
				},
				httpClient:  server.Client(),
				tokenSource: func(context.Context) (string, error) { return "tok", nil },
			}
			result, err := resolver.Resolve(context.Background(), []api.SkillReference{
				{URI: "gcp-skill://reg/some-skill"},
			}, ResolveOpts{})
			if err != nil {
				t.Fatalf("Resolve() hard error: %v", err)
			}
			if len(result.Errors) != 1 {
				t.Fatalf("got %d errors, want 1", len(result.Errors))
			}
			if got := result.Errors[0].Status; got != status {
				t.Errorf("Status = %d, want %d", got, status)
			}
		})
	}
}

func TestUpstreamStatus(t *testing.T) {
	if got := upstreamStatus(nil); got != 0 {
		t.Errorf("upstreamStatus(nil) = %d, want 0", got)
	}
	if got := upstreamStatus(fmt.Errorf("plain")); got != 0 {
		t.Errorf("upstreamStatus(plain) = %d, want 0", got)
	}
	wrapped := fmt.Errorf("outer: %w", &upstreamStatusError{status: 418, msg: "x"})
	if got := upstreamStatus(wrapped); got != 418 {
		t.Errorf("upstreamStatus(wrapped) = %d, want 418", got)
	}
	gh := fmt.Errorf("outer: %w", &githubResolveError{code: SkillErrCodeUpstreamUnavailable, status: 503})
	if got := upstreamStatus(gh); got != 503 {
		t.Errorf("upstreamStatus(githubResolveError) = %d, want 503", got)
	}
}
