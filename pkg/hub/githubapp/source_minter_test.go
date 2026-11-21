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
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/hub/trigger/source"
)

func TestInstallationTokenMinter_Mint(t *testing.T) {
	keyPEM, _ := generateTestKey(t)
	exp := time.Date(2026, 11, 21, 13, 0, 0, 0, time.UTC)

	var gotPath string
	var gotBody struct {
		Repositories []string          `json:"repositories"`
		Permissions  map[string]string `json:"permissions"`
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		if err := json.NewDecoder(r.Body).Decode(&gotBody); err != nil {
			t.Errorf("decode request: %v", err)
		}
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"token":       "ghs_from_seam",
			"expires_at":  exp.Format(time.RFC3339),
			"permissions": map[string]string{"issues": "write", "metadata": "read"},
		})
	}))
	defer srv.Close()

	client, err := NewClient(Config{AppID: 123, APIBaseURL: srv.URL}, keyPEM)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}

	var minter source.IdentityMinter = &InstallationTokenMinter{Client: client}
	cred, err := minter.Mint(context.Background(), source.Target{
		ProjectID:      "proj-1",
		InstallationID: "456",
		Resource:       "acme/widgets",
	}, source.CredentialScope{
		Permissions: map[string]string{"issues": "write", "metadata": "read"},
	})
	if err != nil {
		t.Fatalf("Mint failed: %v", err)
	}
	if gotPath != "/app/installations/456/access_tokens" {
		t.Errorf("path = %q, want /app/installations/456/access_tokens", gotPath)
	}
	if len(gotBody.Repositories) != 1 || gotBody.Repositories[0] != "widgets" {
		t.Errorf("repositories = %v, want [widgets]", gotBody.Repositories)
	}
	if cred.Token != "ghs_from_seam" || !cred.ExpiresAt.Equal(exp) {
		t.Errorf("unexpected credential: %+v", cred)
	}
	if cred.Scope["issues"] != "write" {
		t.Errorf("unexpected scope: %+v", cred.Scope)
	}

	// Reject unknown permission.
	if _, err := minter.Mint(context.Background(), source.Target{
		InstallationID: "456",
		Resource:       "acme/widgets",
	}, source.CredentialScope{
		Permissions: map[string]string{"bogus": "read"},
	}); err == nil {
		t.Error("expected error for unknown permission")
	}
}
