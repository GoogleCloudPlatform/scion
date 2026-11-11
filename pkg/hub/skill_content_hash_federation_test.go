//go:build !no_sqlite

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
	"encoding/json"
	"net/http"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const hashV2FedURI = "skill://ext-registry/core/hashv2-skill@1.0"

// federatedLegacyFiles is the file list of goldenLegacySkillV1/V2 as a
// registry serves it.
func federatedLegacyFiles() []DownloadURLInfo {
	return []DownloadURLInfo{
		{Path: "SKILL.md", URL: "https://registry.example/a", Size: 10, Hash: skillHashFileA, Mode: "0644"},
		{Path: "scripts/run.sh", URL: "https://registry.example/b", Size: 20, Hash: skillHashFileB, Mode: "0644"},
	}
}

type hashV2FedCase struct {
	trust     string
	pin       string // "" for no pin
	claimV1   string
	claimV2   string
	files     []DownloadURLInfo
	v2Enabled bool
}

// runHashV2Federation resolves hashV2FedURI from a mock registry that
// answers with c's claims and files.
func runHashV2Federation(t *testing.T, c hashV2FedCase) (*ResolvedSkillResponse, *ResolveSkillError) {
	t.Helper()
	mockResp := ResolveSkillsResponse{
		Resolved: []ResolvedSkillResponse{{
			URI:             hashV2FedURI,
			Name:            "hashv2-skill",
			ResolvedVersion: "1.0.0",
			ContentHash:     c.claimV1,
			ContentHashV2:   c.claimV2,
			Files:           c.files,
		}},
	}
	mock := newFederationMockRegistry(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(mockResp)
	})
	srv, s := newFederationTestServer(t, mock)
	setSkillContentHashV2Experiment(t, srv, c.v2Enabled)

	registry := &store.SkillRegistry{
		Name:        "ext-registry",
		Endpoint:    mock.URL,
		Type:        "hub",
		TrustLevel:  c.trust,
		ResolvePath: "/api/v1/skills/resolve",
		Status:      "active",
	}
	require.NoError(t, s.CreateSkillRegistry(t.Context(), registry))
	if c.pin != "" {
		require.NoError(t, s.PinSkillHash(t.Context(), registry.ID, hashV2FedURI, c.pin))
	}
	return srv.federateResolve(t.Context(), "ext-registry", ResolveSkillRef{URI: hashV2FedURI})
}

// With the experiment on, a pin matches either hash form, both recomputed
// from the registry's file list; a claim that disagrees with the files, a
// pin that matches neither form, and a file list the hub cannot recompute
// from all fail closed.
func TestFederateResolve_SkillHashV2_On(t *testing.T) {
	noHashes := federatedLegacyFiles()
	for i := range noHashes {
		noHashes[i].Hash = ""
	}
	noModes := federatedLegacyFiles()
	for i := range noModes {
		noModes[i].Mode = "" // a registry before ptone/scion#5652
	}
	renamed := federatedLegacyFiles()
	renamed[1].Path = "scripts/start.sh"
	chmodded := federatedLegacyFiles()
	chmodded[1].Mode = "0755"

	tests := []struct {
		name   string
		c      hashV2FedCase
		wantOK bool
		wantV2 string
	}{
		{"pinned v1 pin", hashV2FedCase{trust: "pinned", pin: goldenLegacySkillV1, claimV1: goldenLegacySkillV1, files: federatedLegacyFiles()}, true, goldenLegacySkillV2},
		{"pinned v2 pin", hashV2FedCase{trust: "pinned", pin: goldenLegacySkillV2, claimV1: goldenLegacySkillV1, files: federatedLegacyFiles()}, true, goldenLegacySkillV2},
		{"pinned v2 pin, registry also claims v2", hashV2FedCase{trust: "pinned", pin: goldenLegacySkillV2, claimV1: goldenLegacySkillV1, claimV2: goldenLegacySkillV2, files: federatedLegacyFiles()}, true, goldenLegacySkillV2},
		{"pinned v2 pin, registry without modes", hashV2FedCase{trust: "pinned", pin: goldenLegacySkillV2, claimV1: goldenLegacySkillV1, files: noModes}, true, goldenLegacySkillV2},
		{"pinned v2 pin, files renamed", hashV2FedCase{trust: "pinned", pin: goldenLegacySkillV2, claimV1: goldenLegacySkillV1, files: renamed}, false, ""},
		{"pinned v2 pin, chmod only", hashV2FedCase{trust: "pinned", pin: goldenLegacySkillV2, claimV1: goldenLegacySkillV1, files: chmodded}, false, ""},
		{"pinned, pin matches neither", hashV2FedCase{trust: "pinned", pin: goldenGitHubExecSkillV2, claimV1: goldenLegacySkillV1, files: federatedLegacyFiles()}, false, ""},
		{"pinned, claimed v1 disagrees with files", hashV2FedCase{trust: "pinned", pin: goldenGitHubExecSkillV1, claimV1: goldenGitHubExecSkillV1, files: federatedLegacyFiles()}, false, ""},
		{"pinned, claimed v2 disagrees with files", hashV2FedCase{trust: "pinned", pin: goldenLegacySkillV1, claimV1: goldenLegacySkillV1, claimV2: goldenGitHubExecSkillV2, files: federatedLegacyFiles()}, false, ""},
		{"pinned, no per-file hashes", hashV2FedCase{trust: "pinned", pin: goldenLegacySkillV1, claimV1: goldenLegacySkillV1, files: noHashes}, false, ""},
		{"pinned, no pin", hashV2FedCase{trust: "pinned", claimV1: goldenLegacySkillV1, files: federatedLegacyFiles()}, false, ""},
		{"trusted", hashV2FedCase{trust: "trusted", claimV1: goldenLegacySkillV1, files: federatedLegacyFiles()}, true, goldenLegacySkillV2},
		{"trusted, claimed v1 disagrees with files", hashV2FedCase{trust: "trusted", claimV1: goldenGitHubExecSkillV1, files: federatedLegacyFiles()}, false, ""},
		{"trusted, no per-file hashes", hashV2FedCase{trust: "trusted", claimV1: goldenLegacySkillV1, claimV2: goldenLegacySkillV2, files: noHashes}, true, ""},
		{"trusted, no content hash and no per-file hashes", hashV2FedCase{trust: "trusted", files: noHashes}, false, ""},
		{"trusted, no content hash and no files", hashV2FedCase{trust: "trusted"}, false, ""},
		{"trusted, no content hash but recomputable", hashV2FedCase{trust: "trusted", files: federatedLegacyFiles()}, false, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tt.c.v2Enabled = true
			resolved, resolveErr := runHashV2Federation(t, tt.c)
			if !tt.wantOK {
				require.NotNil(t, resolveErr, "want a trust violation")
				assert.Equal(t, "trust_violation", resolveErr.Code)
				assert.Nil(t, resolved)
				return
			}
			require.Nil(t, resolveErr)
			assert.Equal(t, tt.c.claimV1, resolved.ContentHash, "contentHash stays the registry's v1")
			assert.Equal(t, tt.wantV2, resolved.ContentHashV2, "contentHashV2 is the recomputed v2, never the registry's claim alone")
		})
	}
}

// With the experiment off, federation behaves as before ptone/scion#5935:
// a pin matches the claimed v1 only, a registry's contentHashV2 is not
// passed on, and a claimed v1 that disagrees with the files is logged, not
// rejected (the broker's own v1 check rejects that bundle at install).
func TestFederateResolve_SkillHashV2_Off(t *testing.T) {
	resolved, resolveErr := runHashV2Federation(t, hashV2FedCase{
		trust: "pinned", pin: goldenLegacySkillV1, claimV1: goldenLegacySkillV1, claimV2: goldenLegacySkillV2, files: federatedLegacyFiles(),
	})
	require.Nil(t, resolveErr)
	assert.Equal(t, goldenLegacySkillV1, resolved.ContentHash)
	assert.Empty(t, resolved.ContentHashV2, "off: the registry's contentHashV2 is dropped")

	_, resolveErr = runHashV2Federation(t, hashV2FedCase{
		trust: "pinned", pin: goldenLegacySkillV2, claimV1: goldenLegacySkillV1, files: federatedLegacyFiles(),
	})
	require.NotNil(t, resolveErr, "off: a v2 pin does not match")
	assert.Equal(t, "trust_violation", resolveErr.Code)

	resolved, resolveErr = runHashV2Federation(t, hashV2FedCase{
		trust: "pinned", pin: goldenGitHubExecSkillV1, claimV1: goldenGitHubExecSkillV1, files: federatedLegacyFiles(),
	})
	require.Nil(t, resolveErr, "off: a claim that disagrees with the files is only logged")
	assert.Equal(t, goldenGitHubExecSkillV1, resolved.ContentHash)

	_, resolveErr = runHashV2Federation(t, hashV2FedCase{
		trust: "pinned", pin: goldenLegacySkillV1, claimV1: goldenGitHubExecSkillV1, files: federatedLegacyFiles(),
	})
	require.NotNil(t, resolveErr, "off: a pin that differs from the claim still fails")
	assert.Equal(t, "trust_violation", resolveErr.Code)

	// A trusted registry that sends neither a content hash nor per-file
	// hashes is accepted with the experiment off, as before.
	noHashes := federatedLegacyFiles()
	for i := range noHashes {
		noHashes[i].Hash = ""
	}
	resolved, resolveErr = runHashV2Federation(t, hashV2FedCase{trust: "trusted", files: noHashes})
	require.Nil(t, resolveErr, "off: an unverifiable trusted skill is accepted, as before")
	assert.Empty(t, resolved.ContentHash)
	assert.Empty(t, resolved.ContentHashV2)
}
