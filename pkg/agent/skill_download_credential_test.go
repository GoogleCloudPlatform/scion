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
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
	"github.com/GoogleCloudPlatform/scion/pkg/transfer"
)

// TestHubResolvedGHSkill_DownloadsWithResolvingCredential verifies that when
// the Hub resolves a gh:// skill with a project credential (DownloadToken),
// installResolvedSkills downloads the skill files using that credential rather
// than falling back to the creating user's GITHUB_TOKEN in context
// (ptone/scion#5028).
func TestHubResolvedGHSkill_DownloadsWithResolvingCredential(t *testing.T) {
	const (
		projectToken = "ghs_project_app_resolving_token"
		userToken    = "ghp_creating_user_different_token"
		skillBody    = "# Private project skill\n"
		uri          = "gh://acme/private-repo/my-skill@main"
	)

	blobHash := transfer.GitBlobHashBytes([]byte(skillBody))
	bundleHash := transfer.ComputeContentHashV1([]transfer.FileInfo{{Path: "SKILL.md", Hash: blobHash}})

	// Serve raw.githubusercontent.com: accept only the project's resolving
	// credential, reject the user's credential (404, as GitHub does).
	fake := installGitHubRawFake(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+projectToken {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		_, _ = w.Write([]byte(skillBody))
	})

	mock := &mockSkillService{
		resolveResp: &hubclient.ResolveSkillsResponse{
			Resolved: []hubclient.ResolvedSkill{{
				URI:             uri,
				Name:            "my-skill",
				ResolvedVersion: "0123456789ab",
				ContentHash:     bundleHash,
				DownloadToken:   projectToken,
				Files: []hubclient.DownloadURLInfo{{
					Path: "SKILL.md",
					URL:  fakeRawFileURL,
					Hash: blobHash,
					Size: int64(len(skillBody)),
					Mode: "0644",
				}},
			}},
		},
	}

	resolver := NewHubSkillResolver(mock)
	res, err := resolver.Resolve(context.Background(), []api.SkillReference{{URI: uri, Scope: "project"}}, ResolveOpts{ProjectID: "proj-1"})
	if err != nil {
		t.Fatalf("HubSkillResolver.Resolve: %v", err)
	}
	if len(res.Resolved) != 1 {
		t.Fatalf("got %d resolved skills, want 1", len(res.Resolved))
	}

	// Ensure marshaling ResolvedSkill never serializes the download token.
	marshaled, err := json.Marshal(res.Resolved[0])
	if err != nil {
		t.Fatalf("json.Marshal(ResolvedSkill): %v", err)
	}
	if strings.Contains(string(marshaled), projectToken) {
		t.Fatal("ResolvedSkill JSON must not contain DownloadToken")
	}

	// Context carries the creating user's GITHUB_TOKEN; the skill's own
	// resolving credential must win.
	ctx := ContextWithGitHubToken(context.Background(), userToken)
	agentHome := t.TempDir()
	skillsDest := filepath.Join(agentHome, "skills")

	record, skipped, err := installResolvedSkills(ctx, res.Resolved, skillsDest, agentHome)
	if err != nil {
		t.Fatalf("installResolvedSkills: %v", err)
	}
	if len(skipped) != 0 {
		t.Fatalf("unexpected skipped skills: %+v", skipped)
	}

	reqs := fake.requests()
	if len(reqs) != 1 {
		t.Fatalf("requests = %d, want 1", len(reqs))
	}
	if reqs[0].auth != "Bearer "+projectToken {
		t.Fatalf("Authorization = %q, want %q", reqs[0].auth, "Bearer "+projectToken)
	}

	got, err := os.ReadFile(filepath.Join(skillsDest, "my-skill", "SKILL.md"))
	if err != nil {
		t.Fatalf("read installed SKILL.md: %v", err)
	}
	if string(got) != skillBody {
		t.Fatalf("SKILL.md = %q, want %q", got, skillBody)
	}

	// Resolution record must never contain the download token.
	recordJSON, err := json.Marshal(record)
	if err != nil {
		t.Fatalf("json.Marshal(record): %v", err)
	}
	if strings.Contains(string(recordJSON), projectToken) {
		t.Fatal("SkillResolutionRecord must not contain DownloadToken")
	}
}

// TestPreResolvedGHSkill_DownloadsWithResolvingCredential verifies that a
// gh:// skill delivered via PreResolvedSkillResolver also downloads with its
// resolving credential rather than the context's default GITHUB_TOKEN.
func TestPreResolvedGHSkill_DownloadsWithResolvingCredential(t *testing.T) {
	const (
		projectToken = "ghs_preresolved_project_token"
		userToken    = "ghp_creating_user_token"
		skillBody    = "# Pre-resolved private skill\n"
		uri          = "gh://acme/private-repo/pre-skill@main"
	)

	blobHash := transfer.GitBlobHashBytes([]byte(skillBody))
	bundleHash := transfer.ComputeContentHashV1([]transfer.FileInfo{{Path: "SKILL.md", Hash: blobHash}})

	fake := installGitHubRawFake(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+projectToken {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		_, _ = w.Write([]byte(skillBody))
	})

	pre := &hubclient.ResolveSkillsResponse{
		Resolved: []hubclient.ResolvedSkill{{
			URI:             uri,
			Name:            "pre-skill",
			ResolvedVersion: "0123456789ab",
			ContentHash:     bundleHash,
			DownloadToken:   projectToken,
			Files: []hubclient.DownloadURLInfo{{
				Path: "SKILL.md",
				URL:  fakeRawFileURL,
				Hash: blobHash,
				Size: int64(len(skillBody)),
				Mode: "0644",
			}},
		}},
	}

	resolver := NewPreResolvedSkillResolver(pre, nil, "https://hub.example")
	res, err := resolver.Resolve(context.Background(), []api.SkillReference{{URI: uri}}, ResolveOpts{})
	if err != nil {
		t.Fatalf("PreResolvedSkillResolver.Resolve: %v", err)
	}

	ctx := ContextWithGitHubToken(context.Background(), userToken)
	agentHome := t.TempDir()
	skillsDest := filepath.Join(agentHome, "skills")

	if _, _, err := installResolvedSkills(ctx, res.Resolved, skillsDest, agentHome); err != nil {
		t.Fatalf("installResolvedSkills: %v", err)
	}

	reqs := fake.requests()
	if len(reqs) != 1 || reqs[0].auth != "Bearer "+projectToken {
		t.Fatalf("requests = %+v, want 1 request with Bearer %s", reqs, projectToken)
	}
}

// TestHubResolvedGHSkill_DownloadTokenNotSentToNonGitHubHost verifies the host
// guard still prevents a Hub-supplied DownloadToken from being sent to a
// non-GitHub origin.
func TestHubResolvedGHSkill_DownloadTokenNotSentToNonGitHubHost(t *testing.T) {
	const projectToken = "ghs_must_not_leak"
	const skillBody = "# Non-github host skill\n"

	var gotAuth string
	nonGH := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		_, _ = w.Write([]byte(skillBody))
	}))
	t.Cleanup(nonGH.Close)

	hash := transfer.HashBytes([]byte(skillBody))
	bundleHash := transfer.ComputeContentHashV1([]transfer.FileInfo{{Path: "SKILL.md", Hash: hash}})

	skill := ResolvedSkill{
		Name:          "non-gh",
		URI:           "gh://acme/repo/non-gh@main",
		Version:       "1.0.0",
		Hash:          bundleHash,
		downloadToken: projectToken,
		Files: []ResolvedFile{{
			Path: "SKILL.md",
			URL:  nonGH.URL + "/SKILL.md",
			Hash: hash,
			Size: int64(len(skillBody)),
		}},
	}

	if _, err := installOneSkill(context.Background(), skill, "non-gh", t.TempDir()); err != nil {
		t.Fatalf("installOneSkill: %v", err)
	}
	if gotAuth != "" {
		t.Fatalf("Authorization sent to non-GitHub host: %q", gotAuth)
	}
}

// TestHubResolvedGHSkill_RejectedHubCredentialReportsSourceWithoutToken
// verifies that if GitHub rejects the Hub's resolving credential, the error
// names the Hub credential source and never includes the token value.
func TestHubResolvedGHSkill_RejectedHubCredentialReportsSourceWithoutToken(t *testing.T) {
	const projectToken = "ghs_rejected_hub_token_secret"

	installGitHubRawFake(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	})

	skill := ResolvedSkill{
		Name:          "private-skill",
		URI:           "gh://acme/private-repo/private-skill@main",
		Version:       "0123456789ab",
		downloadToken: projectToken,
		Files: []ResolvedFile{{
			Path: "SKILL.md",
			URL:  fakeRawFileURL,
			Size: 10,
		}},
	}

	_, err := installOneSkill(context.Background(), skill, "private-skill", t.TempDir())
	if err == nil {
		t.Fatal("expected installOneSkill to fail")
	}
	var rej *gitHubCredentialRejectedError
	if !errors.As(err, &rej) {
		t.Fatalf("error = %T (%v), want *gitHubCredentialRejectedError", err, err)
	}
	if rej.Source != gitHubCredSourceHub {
		t.Errorf("Source = %q, want %q", rej.Source, gitHubCredSourceHub)
	}
	if strings.Contains(err.Error(), projectToken) {
		t.Fatal("error text leaked DownloadToken")
	}
}
