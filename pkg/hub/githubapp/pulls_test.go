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

package githubapp

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestGetPullRequestJSON_HappyPath(t *testing.T) {
	pemData, _ := generateTestKey(t)
	var gotAuth, gotAccept, gotPath string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		gotAccept = r.Header.Get("Accept")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"number":42,"state":"open","draft":false,"merged":false,"head":{"ref":"feat","sha":"abc"},"base":{"ref":"main","sha":"def"}}`))
	}))
	defer server.Close()

	client, err := NewClient(Config{AppID: 42, APIBaseURL: server.URL}, pemData)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	raw, err := client.GetPullRequestJSON(context.Background(), "ghs_test_token", "octo-org", "octo-repo", 42)
	if err != nil {
		t.Fatalf("GetPullRequestJSON: %v", err)
	}
	if gotPath != "/repos/octo-org/octo-repo/pulls/42" {
		t.Errorf("path = %q, want /repos/octo-org/octo-repo/pulls/42", gotPath)
	}
	if gotAuth != "Bearer ghs_test_token" {
		t.Errorf("Authorization = %q", gotAuth)
	}
	if gotAccept != "application/vnd.github+json" {
		t.Errorf("Accept = %q", gotAccept)
	}
	if !strings.Contains(string(raw), `"number":42`) {
		t.Errorf("unexpected response body: %s", string(raw))
	}
}

func TestGetPullRequestJSON_Validation(t *testing.T) {
	pemData, _ := generateTestKey(t)
	client, err := NewClient(Config{AppID: 42, APIBaseURL: "http://127.0.0.1:1"}, pemData)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	cases := []struct {
		name  string
		token string
		owner string
		repo  string
		num   int
	}{
		{"empty token", "", "octo", "repo", 1},
		{"bad owner", "tok", "../bad", "repo", 1},
		{"bad repo", "tok", "octo", "a/b", 1},
		{"zero number", "tok", "octo", "repo", 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := client.GetPullRequestJSON(context.Background(), tc.token, tc.owner, tc.repo, tc.num)
			if err == nil {
				t.Fatal("expected error")
			}
			var apiErr *PullRequestAPIError
			if !errors.As(err, &apiErr) {
				t.Fatalf("expected PullRequestAPIError, got %T", err)
			}
		})
	}
}

func TestGetPullRequestJSON_ContentFreeErrors(t *testing.T) {
	pemData, _ := generateTestKey(t)
	const secretMarker = "GITHUB-PR-ERROR-MARKER-8088"

	cases := []struct {
		name     string
		status   int
		headers  map[string]string
		body     string
		wantCode string
		wantRate bool
	}{
		{"401", http.StatusUnauthorized, nil, `{"message":"Bad credentials ` + secretMarker + `"}`, ErrCodePullRequestUnauthorized, false},
		{"403 refused", http.StatusForbidden, nil, `{"message":"Resource not accessible ` + secretMarker + `"}`, ErrCodePullRequestRefused, false},
		{"403 rate limited", http.StatusForbidden, map[string]string{"X-RateLimit-Remaining": "0"}, `{"message":"API rate limit ` + secretMarker + `"}`, ErrCodePullRequestRateLimited, true},
		{"404", http.StatusNotFound, nil, `{"message":"Not Found ` + secretMarker + `"}`, ErrCodePullRequestNotFound, false},
		{"429", http.StatusTooManyRequests, nil, `{"message":"Too many requests ` + secretMarker + `"}`, ErrCodePullRequestRateLimited, true},
		{"500", http.StatusInternalServerError, nil, `<html>` + secretMarker + `</html>`, ErrCodePullRequestFailed, false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				for k, v := range tc.headers {
					w.Header().Set(k, v)
				}
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			}))
			defer server.Close()

			client, err := NewClient(Config{AppID: 42, APIBaseURL: server.URL}, pemData)
			if err != nil {
				t.Fatalf("NewClient: %v", err)
			}
			_, err = client.GetPullRequestJSON(context.Background(), "ghs_secret_tok", "octo", "repo", 42)
			if err == nil {
				t.Fatal("expected error")
			}
			var apiErr *PullRequestAPIError
			if !errors.As(err, &apiErr) {
				t.Fatalf("expected PullRequestAPIError, got %T", err)
			}
			if apiErr.ErrorCode != tc.wantCode {
				t.Errorf("ErrorCode = %q, want %q", apiErr.ErrorCode, tc.wantCode)
			}
			if strings.Contains(apiErr.Error(), secretMarker) || strings.Contains(apiErr.Message, secretMarker) {
				t.Errorf("upstream text leaked into Error()/Message: %q", apiErr.Error())
			}
			if strings.Contains(apiErr.Error(), "ghs_secret_tok") {
				t.Errorf("token leaked into Error(): %q", apiErr.Error())
			}
			if !strings.Contains(apiErr.UpstreamDetail(), secretMarker) {
				t.Errorf("UpstreamDetail() = %q, want marker", apiErr.UpstreamDetail())
			}
			if tc.wantRate && !errors.Is(err, ErrRateLimited) {
				t.Errorf("expected errors.Is(err, ErrRateLimited)")
			}
		})
	}
}

func TestGetPullRequestJSON_TimeoutAndOversize(t *testing.T) {
	pemData, _ := generateTestKey(t)

	t.Run("context timeout", func(t *testing.T) {
		done := make(chan struct{})
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			select {
			case <-done:
			case <-r.Context().Done():
			}
		}))
		defer func() {
			close(done)
			server.Close()
		}()

		client, err := NewClient(Config{AppID: 42, APIBaseURL: server.URL}, pemData)
		if err != nil {
			t.Fatalf("NewClient: %v", err)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
		defer cancel()
		_, err = client.GetPullRequestJSON(ctx, "ghs_tok", "octo", "repo", 42)
		if err == nil {
			t.Fatal("expected timeout error")
		}
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Errorf("expected DeadlineExceeded, got %v", err)
		}
	})

	t.Run("oversized body rejected", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"body":"`))
			_, _ = w.Write([]byte(strings.Repeat("a", pullRequestResponseBodyLimit+64)))
			_, _ = w.Write([]byte(`"}`))
		}))
		defer server.Close()

		client, err := NewClient(Config{AppID: 42, APIBaseURL: server.URL}, pemData)
		if err != nil {
			t.Fatalf("NewClient: %v", err)
		}
		_, err = client.GetPullRequestJSON(context.Background(), "ghs_tok", "octo", "repo", 42)
		if err == nil {
			t.Fatal("expected size limit error")
		}
		var apiErr *PullRequestAPIError
		if !errors.As(err, &apiErr) {
			t.Fatalf("expected PullRequestAPIError, got %T", err)
		}
		if !strings.Contains(apiErr.Message, "size limit") {
			t.Errorf("unexpected error message: %q", apiErr.Message)
		}
	})

	// Keep encoding/json referenced in case of edits.
	_ = json.Valid
}
