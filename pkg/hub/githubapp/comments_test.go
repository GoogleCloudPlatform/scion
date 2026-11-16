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
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"
)

func newCommentTestClient(t *testing.T, baseURL string) *Client {
	t.Helper()
	pemData, _ := generateTestKey(t)
	c, err := NewClient(Config{AppID: 99, APIBaseURL: baseURL}, pemData)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	return c
}

func TestAppendFireMarker_BoundsAndValidation(t *testing.T) {
	t.Run("appends newline and marker", func(t *testing.T) {
		got, err := AppendFireMarker("Thanks @alice", "fire-abc-123")
		if err != nil {
			t.Fatalf("AppendFireMarker: %v", err)
		}
		want := "Thanks @alice\n<!-- scion fire:fire-abc-123 -->"
		if got != want {
			t.Fatalf("got %q, want %q", got, want)
		}
		if !HasFireMarker(got, "fire-abc-123") {
			t.Fatal("HasFireMarker returned false for appended marker")
		}
		if HasFireMarker(got, "fire-abc") {
			t.Fatal("HasFireMarker matched prefix fire-abc against fire-abc-123")
		}
	})

	t.Run("idempotent when marker already present", func(t *testing.T) {
		first, err := AppendFireMarker("Hello\n", "fire-1")
		if err != nil {
			t.Fatalf("first AppendFireMarker: %v", err)
		}
		second, err := AppendFireMarker(first, "fire-1")
		if err != nil {
			t.Fatalf("second AppendFireMarker: %v", err)
		}
		if second != first {
			t.Fatalf("got %q, want unchanged %q", second, first)
		}
	})

	t.Run("truncates body on UTF-8 rune boundary within 65536 bytes", func(t *testing.T) {
		// 4-byte emoji repeated past 65,536 bytes so naive byte slicing would split a rune.
		huge := strings.Repeat("🎉", (MaxCommentBodyBytes/4)+100)
		got, err := AppendFireMarker(huge, "fire-utf8-bound")
		if err != nil {
			t.Fatalf("AppendFireMarker: %v", err)
		}
		if len(got) > MaxCommentBodyBytes {
			t.Fatalf("len(got) = %d, exceeds MaxCommentBodyBytes (%d)", len(got), MaxCommentBodyBytes)
		}
		if !utf8.ValidString(got) {
			t.Fatal("truncated comment is not valid UTF-8")
		}
		wantSuffix := "\n<!-- scion fire:fire-utf8-bound -->"
		if !strings.HasSuffix(got, wantSuffix) {
			t.Fatalf("truncated comment missing suffix %q", wantSuffix)
		}
	})

	t.Run("rejects invalid fire IDs and NUL bodies", func(t *testing.T) {
		badFireIDs := []string{
			"",
			"has space",
			"has\nnewline",
			"has--double-dash",
			"has>gt",
			"has<lt",
			"has\x00nul",
			strings.Repeat("x", maxFireIDBytes+1),
		}
		for _, id := range badFireIDs {
			if _, err := FireMarker(id); err == nil {
				t.Errorf("FireMarker(%q) succeeded, expected error", id)
			}
		}
		if _, err := AppendFireMarker("bad\x00body", "fire-ok"); err == nil {
			t.Error("AppendFireMarker accepted NUL byte in body")
		}
	})
}

type fakeGitHubCommentsServer struct {
	mu             sync.Mutex
	issueComments  []IssueComment
	reviewComments []IssueComment
	nextID         int64

	issueGetCalls   int
	issuePostCalls  int
	reviewGetCalls  int
	reviewPostCalls int
	lastQuery       string
	lastPath        string
	lastAuthHeader  string
}

func newFakeGitHubCommentsServer(t *testing.T) (*fakeGitHubCommentsServer, *httptest.Server) {
	t.Helper()
	fake := &fakeGitHubCommentsServer{nextID: 1000}
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fake.mu.Lock()
		defer fake.mu.Unlock()

		fake.lastPath = r.URL.Path
		fake.lastQuery = r.URL.RawQuery
		fake.lastAuthHeader = r.Header.Get("Authorization")

		if r.Header.Get("Accept") != "application/vnd.github+json" {
			http.Error(w, "missing accept header", http.StatusBadRequest)
			return
		}
		if r.Header.Get("X-GitHub-Api-Version") != "2022-11-28" {
			http.Error(w, "missing api version header", http.StatusBadRequest)
			return
		}

		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/repos/acme/widgets/issues/42/comments":
			fake.issueGetCalls++
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(fake.issueComments)

		case r.Method == http.MethodPost && r.URL.Path == "/repos/acme/widgets/issues/42/comments":
			fake.issuePostCalls++
			var req createCommentRequest
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				http.Error(w, "bad json", http.StatusBadRequest)
				return
			}
			fake.nextID++
			created := IssueComment{
				ID:        fake.nextID,
				Body:      req.Body,
				HTMLURL:   "https://github.com/acme/widgets/issues/42#issuecomment-created",
				CreatedAt: time.Date(2026, 11, 16, 12, 0, 0, 0, time.UTC),
				UpdatedAt: time.Date(2026, 11, 16, 12, 0, 0, 0, time.UTC),
			}
			fake.issueComments = append(fake.issueComments, created)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(created)

		case r.Method == http.MethodGet && r.URL.Path == "/repos/acme/widgets/pulls/42/comments":
			fake.reviewGetCalls++
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(fake.reviewComments)

		case r.Method == http.MethodPost && r.URL.Path == "/repos/acme/widgets/pulls/42/comments/777/replies":
			fake.reviewPostCalls++
			var req createCommentRequest
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				http.Error(w, "bad json", http.StatusBadRequest)
				return
			}
			fake.nextID++
			created := IssueComment{
				ID:        fake.nextID,
				Body:      req.Body,
				HTMLURL:   "https://github.com/acme/widgets/pull/42#discussion_r-created",
				CreatedAt: time.Date(2026, 11, 16, 12, 0, 0, 0, time.UTC),
				UpdatedAt: time.Date(2026, 11, 16, 12, 0, 0, 0, time.UTC),
			}
			fake.reviewComments = append(fake.reviewComments, created)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(created)

		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(ts.Close)
	return fake, ts
}

func TestPostCommentIdempotent_IssueAndReviewThread(t *testing.T) {
	const token = "ghs_test_install_token"
	since := time.Date(2026, 11, 16, 11, 55, 0, 0, time.UTC)

	t.Run("first attempt posts directly without listing", func(t *testing.T) {
		fake, ts := newFakeGitHubCommentsServer(t)
		client := newCommentTestClient(t, ts.URL)

		got, err := client.PostCommentIdempotent(context.Background(), token, PostCommentOptions{
			Owner:       "acme",
			Repo:        "widgets",
			IssueNumber: 42,
			Body:        "Started review agent",
			FireID:      "fire-1",
			Attempts:    1,
			Since:       since,
		})
		if err != nil {
			t.Fatalf("PostCommentIdempotent: %v", err)
		}
		if fake.issueGetCalls != 0 {
			t.Errorf("issueGetCalls = %d, want 0 on first attempt", fake.issueGetCalls)
		}
		if fake.issuePostCalls != 1 {
			t.Errorf("issuePostCalls = %d, want 1", fake.issuePostCalls)
		}
		if fake.lastAuthHeader != "Bearer "+token {
			t.Errorf("Authorization = %q, want Bearer token", fake.lastAuthHeader)
		}
		if !HasFireMarker(got.Body, "fire-1") {
			t.Errorf("posted body %q missing fire marker", got.Body)
		}
	})

	t.Run("retry with existing marker returns existing comment and does not post", func(t *testing.T) {
		fake, ts := newFakeGitHubCommentsServer(t)
		fake.issueComments = []IssueComment{
			{
				ID:      501,
				Body:    "Unrelated comment\n<!-- scion fire:fire-10 -->",
				HTMLURL: "https://github.com/acme/widgets/issues/42#issuecomment-501",
			},
			{
				ID:      502,
				Body:    "Started review agent\n<!-- scion fire:fire-1 -->",
				HTMLURL: "https://github.com/acme/widgets/issues/42#issuecomment-502",
			},
		}
		client := newCommentTestClient(t, ts.URL)

		got, err := client.PostCommentIdempotent(context.Background(), token, PostCommentOptions{
			Owner:       "acme",
			Repo:        "widgets",
			IssueNumber: 42,
			Body:        "Started review agent",
			FireID:      "fire-1",
			Attempts:    2,
			Since:       since,
		})
		if err != nil {
			t.Fatalf("PostCommentIdempotent: %v", err)
		}
		if fake.issueGetCalls != 1 {
			t.Errorf("issueGetCalls = %d, want 1", fake.issueGetCalls)
		}
		if fake.issuePostCalls != 0 {
			t.Errorf("issuePostCalls = %d, want 0 when marker already present", fake.issuePostCalls)
		}
		if !strings.Contains(fake.lastQuery, "per_page=100") || !strings.Contains(fake.lastQuery, "since=2026-11-16T11%3A55%3A00Z") {
			t.Errorf("unexpected list query: %q", fake.lastQuery)
		}
		if got.ID != 502 || got.HTMLURL != "https://github.com/acme/widgets/issues/42#issuecomment-502" {
			t.Errorf("got comment (%d, %q), want (502, issuecomment-502)", got.ID, got.HTMLURL)
		}
	})

	t.Run("retry without matching marker lists once then posts once", func(t *testing.T) {
		fake, ts := newFakeGitHubCommentsServer(t)
		fake.issueComments = []IssueComment{
			{
				ID:      501,
				Body:    "Prefix-similar fire\n<!-- scion fire:fire-10 -->",
				HTMLURL: "https://github.com/acme/widgets/issues/42#issuecomment-501",
			},
		}
		client := newCommentTestClient(t, ts.URL)

		got, err := client.PostCommentIdempotent(context.Background(), token, PostCommentOptions{
			Owner:       "acme",
			Repo:        "widgets",
			IssueNumber: 42,
			Body:        "Started review agent",
			FireID:      "fire-1",
			Attempts:    2,
			Since:       since,
		})
		if err != nil {
			t.Fatalf("PostCommentIdempotent: %v", err)
		}
		if fake.issueGetCalls != 1 {
			t.Errorf("issueGetCalls = %d, want 1", fake.issueGetCalls)
		}
		if fake.issuePostCalls != 1 {
			t.Errorf("issuePostCalls = %d, want 1", fake.issuePostCalls)
		}
		if got.ID != 1001 {
			t.Errorf("got created ID %d, want 1001", got.ID)
		}
	})

	t.Run("review thread reply posts and deduplicates on retry", func(t *testing.T) {
		fake, ts := newFakeGitHubCommentsServer(t)
		client := newCommentTestClient(t, ts.URL)

		first, err := client.PostCommentIdempotent(context.Background(), token, PostCommentOptions{
			Owner:           "acme",
			Repo:            "widgets",
			IssueNumber:     42,
			ReviewCommentID: 777,
			Body:            "Replying in review thread",
			FireID:          "fire-review-1",
			Attempts:        1,
			Since:           since,
		})
		if err != nil {
			t.Fatalf("first PostCommentIdempotent: %v", err)
		}
		if fake.reviewGetCalls != 0 || fake.reviewPostCalls != 1 {
			t.Fatalf("calls after attempt 1: get=%d post=%d, want 0/1", fake.reviewGetCalls, fake.reviewPostCalls)
		}

		second, err := client.PostCommentIdempotent(context.Background(), token, PostCommentOptions{
			Owner:           "acme",
			Repo:            "widgets",
			IssueNumber:     42,
			ReviewCommentID: 777,
			Body:            "Replying in review thread",
			FireID:          "fire-review-1",
			Attempts:        2,
			Since:           since,
		})
		if err != nil {
			t.Fatalf("second PostCommentIdempotent: %v", err)
		}
		if fake.reviewGetCalls != 1 || fake.reviewPostCalls != 1 {
			t.Fatalf("calls after attempt 2: get=%d post=%d, want 1/1", fake.reviewGetCalls, fake.reviewPostCalls)
		}
		if second.ID != first.ID || second.HTMLURL != first.HTMLURL {
			t.Fatalf("retry returned (%d, %q), want (%d, %q)", second.ID, second.HTMLURL, first.ID, first.HTMLURL)
		}
	})
}

func TestCommentMethods_ErrorMappingAndNoLeaks(t *testing.T) {
	const (
		secretToken = "ghs_SUPER_SECRET_INSTALL_TOKEN_999"
		secretBody  = "SENSITIVE-COMMENT-BODY-DO-NOT-LOG"
	)

	var logBuf bytes.Buffer
	origLogger := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(origLogger) })

	tests := []struct {
		name         string
		status       int
		headers      map[string]string
		ghMessage    string
		wantSentinel error
		wantCode     string
	}{
		{
			name:         "403 permission denied maps to ErrCommentRefused",
			status:       http.StatusForbidden,
			ghMessage:    "Resource not accessible by integration " + githubMarker,
			wantSentinel: ErrCommentRefused,
			wantCode:     ErrCodeCommentRefused,
		},
		{
			name:         "422 validation or unaccepted permission maps to ErrCommentRefused",
			status:       http.StatusUnprocessableEntity,
			ghMessage:    "Validation Failed " + githubMarker,
			wantSentinel: ErrCommentRefused,
			wantCode:     ErrCodeCommentRefused,
		},
		{
			name:         "429 maps to ErrRateLimited",
			status:       http.StatusTooManyRequests,
			ghMessage:    "API rate limit exceeded " + githubMarker,
			wantSentinel: ErrRateLimited,
			wantCode:     ErrCodeRateLimited,
		},
		{
			name:         "403 with X-RateLimit-Remaining 0 maps to ErrRateLimited",
			status:       http.StatusForbidden,
			headers:      map[string]string{"X-RateLimit-Remaining": "0"},
			ghMessage:    "API rate limit exceeded " + githubMarker,
			wantSentinel: ErrRateLimited,
			wantCode:     ErrCodeRateLimited,
		},
		{
			name:         "403 with Retry-After maps to ErrRateLimited",
			status:       http.StatusForbidden,
			headers:      map[string]string{"Retry-After": "60"},
			ghMessage:    "Please wait a few minutes " + githubMarker,
			wantSentinel: ErrRateLimited,
			wantCode:     ErrCodeRateLimited,
		},
		{
			name:         "403 secondary rate limit message maps to ErrRateLimited",
			status:       http.StatusForbidden,
			ghMessage:    "You have exceeded a secondary rate limit " + githubMarker,
			wantSentinel: ErrRateLimited,
			wantCode:     ErrCodeRateLimited,
		},
		{
			name:      "500 maps to ErrCodeCommentRequestFailed",
			status:    http.StatusInternalServerError,
			ghMessage: "Internal Error " + githubMarker,
			wantCode:  ErrCodeCommentRequestFailed,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				for k, v := range tc.headers {
					w.Header().Set(k, v)
				}
				w.WriteHeader(tc.status)
				_ = json.NewEncoder(w).Encode(githubErrorResponse{Message: tc.ghMessage})
			}))
			defer ts.Close()

			client := newCommentTestClient(t, ts.URL)
			_, err := client.CreateIssueComment(context.Background(), secretToken, "acme", "widgets", 42, secretBody)
			if err == nil {
				t.Fatal("expected error")
			}
			if tc.wantSentinel != nil && !errors.Is(err, tc.wantSentinel) {
				t.Errorf("errors.Is(%v, %v) = false, want true", err, tc.wantSentinel)
			}
			var apiErr *CommentAPIError
			if !errors.As(err, &apiErr) {
				t.Fatalf("expected *CommentAPIError, got %T (%v)", err, err)
			}
			if apiErr.ErrorCode != tc.wantCode {
				t.Errorf("ErrorCode = %q, want %q", apiErr.ErrorCode, tc.wantCode)
			}
			if apiErr.StatusCode != tc.status {
				t.Errorf("StatusCode = %d, want %d", apiErr.StatusCode, tc.status)
			}
			for _, forbidden := range []string{secretToken, secretBody, githubMarker} {
				if strings.Contains(err.Error(), forbidden) || strings.Contains(apiErr.Message, forbidden) {
					t.Errorf("error string %q leaked %q", err.Error(), forbidden)
				}
			}
			if apiErr.UpstreamDetail() != tc.ghMessage {
				t.Errorf("UpstreamDetail() = %q, want %q", apiErr.UpstreamDetail(), tc.ghMessage)
			}
		})
	}

	t.Run("network failure maps to CommentAPIError without leaking token or body", func(t *testing.T) {
		ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
		closedURL := ts.URL
		ts.Close()

		client := newCommentTestClient(t, closedURL)
		_, err := client.ReplyToReviewComment(context.Background(), secretToken, "acme", "widgets", 42, 777, secretBody)
		if err == nil {
			t.Fatal("expected transport error")
		}
		if errors.Is(err, ErrCommentRefused) || errors.Is(err, ErrRateLimited) {
			t.Errorf("transport error unexpectedly matched sentinel: %v", err)
		}
		var apiErr *CommentAPIError
		if !errors.As(err, &apiErr) {
			t.Fatalf("expected *CommentAPIError, got %T (%v)", err, err)
		}
		if apiErr.ErrorCode != ErrCodeCommentRequestFailed {
			t.Errorf("ErrorCode = %q, want %q", apiErr.ErrorCode, ErrCodeCommentRequestFailed)
		}
		for _, forbidden := range []string{secretToken, secretBody} {
			if strings.Contains(err.Error(), forbidden) {
				t.Errorf("transport error %q leaked %q", err.Error(), forbidden)
			}
		}
	})

	t.Run("error response body read is bounded at upstreamBodyReadLimit", func(t *testing.T) {
		ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte(strings.Repeat("B", upstreamBodyReadLimit) + githubMarker))
		}))
		defer ts.Close()

		client := newCommentTestClient(t, ts.URL)
		_, err := client.ListIssueComments(context.Background(), secretToken, "acme", "widgets", 42, time.Now())
		var apiErr *CommentAPIError
		if !errors.As(err, &apiErr) {
			t.Fatalf("expected *CommentAPIError, got %T", err)
		}
		if len(apiErr.UpstreamDetail()) != upstreamBodyReadLimit {
			t.Errorf("len(UpstreamDetail) = %d, want %d", len(apiErr.UpstreamDetail()), upstreamBodyReadLimit)
		}
		if strings.Contains(apiErr.UpstreamDetail(), githubMarker) {
			t.Error("bytes past upstreamBodyReadLimit were read")
		}
	})

	t.Run("rejects unsafe path segments", func(t *testing.T) {
		client := newCommentTestClient(t, "https://api.github.com")
		if _, err := client.CreateIssueComment(context.Background(), secretToken, "..", "widgets", 1, "hi"); err == nil {
			t.Error("accepted '..' owner")
		}
		if _, err := client.CreateIssueComment(context.Background(), secretToken, "acme", "wid/gets", 1, "hi"); err == nil {
			t.Error("accepted slash in repo")
		}
	})

	logged := logBuf.String()
	for _, forbidden := range []string{secretToken, secretBody, githubMarker} {
		if strings.Contains(logged, forbidden) {
			t.Fatalf("captured slog output leaked %q:\n%s", forbidden, logged)
		}
	}
}
