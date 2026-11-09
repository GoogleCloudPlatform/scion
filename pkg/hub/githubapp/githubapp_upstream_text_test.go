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
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// githubMarker stands for text GitHub sends; it must never reach a
// TokenMintError's Message or a listing error (ptone/scion#7498).
const githubMarker = "GITHUB-UPSTREAM-MARKER-7498"

// A GitHub error response picks the error code; Message is fixed text for
// that code, and GitHub's message is kept only for UpstreamDetail.
func TestClassifyGitHubError_MessageIsFixed(t *testing.T) {
	tests := []struct {
		name     string
		status   int
		ghMsg    string
		wantCode string
	}{
		{"401 app not found", http.StatusUnauthorized, "Integration not found " + githubMarker, ErrCodeAppNotFound},
		{"401 bad credentials", http.StatusUnauthorized, "Bad credentials " + githubMarker, ErrCodePrivateKeyInvalid},
		{"403 suspended", http.StatusForbidden, "This installation has been suspended " + githubMarker, ErrCodeInstallationSuspended},
		{"403 other", http.StatusForbidden, "Resource not accessible " + githubMarker, ErrCodePermissionDenied},
		{"404", http.StatusNotFound, "Not Found " + githubMarker, ErrCodeInstallationRevoked},
		{"422 repository", http.StatusUnprocessableEntity, "Repository not found " + githubMarker, ErrCodeRepoNotAccessible},
		{"422 other", http.StatusUnprocessableEntity, "Invalid permissions " + githubMarker, ErrCodePermissionDenied},
		{"500", http.StatusInternalServerError, "Server Error " + githubMarker, ErrCodeTokenMintFailed},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			body, err := json.Marshal(githubErrorResponse{Message: tc.ghMsg})
			if err != nil {
				t.Fatal(err)
			}
			got := classifyGitHubError(tc.status, body)
			if got.ErrorCode != tc.wantCode {
				t.Errorf("ErrorCode = %q, want %q", got.ErrorCode, tc.wantCode)
			}
			if got.StatusCode != tc.status {
				t.Errorf("StatusCode = %d, want %d", got.StatusCode, tc.status)
			}
			if got.Message == "" {
				t.Error("Message is empty")
			}
			if strings.Contains(got.Message, githubMarker) || strings.Contains(got.Error(), githubMarker) {
				t.Errorf("GitHub text reached Message or Error(): %q", got.Error())
			}
			if got.UpstreamDetail() != tc.ghMsg {
				t.Errorf("UpstreamDetail() = %q, want GitHub's message %q", got.UpstreamDetail(), tc.ghMsg)
			}
		})
	}
}

// A non-JSON error body (a proxy page) is kept as the detail, never in Message.
func TestClassifyGitHubError_NonJSONBody(t *testing.T) {
	body := []byte("<html>" + githubMarker + "</html>")
	got := classifyGitHubError(http.StatusBadGateway, body)
	if got.ErrorCode != ErrCodeTokenMintFailed {
		t.Errorf("ErrorCode = %q, want %q", got.ErrorCode, ErrCodeTokenMintFailed)
	}
	if strings.Contains(got.Error(), githubMarker) {
		t.Errorf("GitHub text reached Error(): %q", got.Error())
	}
	if got.UpstreamDetail() != string(body) {
		t.Errorf("UpstreamDetail() = %q, want the body", got.UpstreamDetail())
	}
}

// A GitHub error body is read up to upstreamBodyReadLimit bytes only.
func TestMintInstallationToken_ErrorBodyReadIsBounded(t *testing.T) {
	pemData, _ := generateTestKey(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(strings.Repeat("A", upstreamBodyReadLimit) + githubMarker))
	}))
	defer server.Close()

	client, err := NewClient(Config{AppID: 42, APIBaseURL: server.URL}, pemData)
	if err != nil {
		t.Fatalf("NewClient failed: %v", err)
	}
	_, err = client.MintInstallationToken(context.Background(), 12345, nil, DefaultTokenPermissions())
	mintErr, ok := err.(*TokenMintError)
	if !ok {
		t.Fatalf("expected TokenMintError, got %T (%v)", err, err)
	}
	if n := len(mintErr.UpstreamDetail()); n != upstreamBodyReadLimit {
		t.Errorf("detail is %d bytes, want exactly the %d-byte read limit", n, upstreamBodyReadLimit)
	}
	if strings.Contains(mintErr.UpstreamDetail(), githubMarker) {
		t.Error("bytes past the read limit were read")
	}
}

// The repository listing error carries the status only, never GitHub's body.
func TestListInstallationRepos_ErrorHasNoBody(t *testing.T) {
	pemData, _ := generateTestKey(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/access_tokens") {
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(accessTokenResponse{Token: "ghs_test"})
			return
		}
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"message":"` + githubMarker + `"}`))
	}))
	defer server.Close()

	client, err := NewClient(Config{AppID: 42, APIBaseURL: server.URL}, pemData)
	if err != nil {
		t.Fatalf("NewClient failed: %v", err)
	}
	_, err = client.ListInstallationRepos(context.Background(), 12345)
	if err == nil {
		t.Fatal("expected an error")
	}
	if strings.Contains(err.Error(), githubMarker) {
		t.Errorf("GitHub body reached the listing error: %q", err.Error())
	}
	if !strings.Contains(err.Error(), "status 500") {
		t.Errorf("listing error %q does not name the status", err.Error())
	}
}
