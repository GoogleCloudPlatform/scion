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
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/agent"
	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/experiments"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/store/enttest"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Golden skill content hashes (ptone/scion#5935), computed independently
// of the Go implementation from the preimages documented on
// transfer.ComputeContentHashV1 and transfer.ComputeContentHashV2. The
// broker's verification tests in pkg/agent pin the same values, so the hub
// and the broker agree on them.
const (
	// skillHashFileA and skillHashFileB are sha256 file hashes of a
	// registry skill; skillBlobA and skillBlobB are git blob IDs of a gh://
	// skill.
	skillHashFileA = "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	skillHashFileB = "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	skillBlobA     = "1111111111111111111111111111111111111111"
	skillBlobB     = "2222222222222222222222222222222222222222"

	// SKILL.md (A) and scripts/run.sh (B), with no recorded modes (a
	// version published before ptone/scion#5652) or both "0644".
	goldenLegacySkillV1 = "sha256:cb6e5d6424eb09d95d48672779cf707c2500440d8775f27db011b948086bb9c2"
	goldenLegacySkillV2 = "sha256:12d49ca2a2a621ed728f8e4721fc7f7aed53fcccddc5686dd2598b2426d186bb"

	// gh:// skill: SKILL.md (blob A, 0644) and scripts/run.sh (blob B,
	// 0755 from the git tree).
	goldenGitHubExecSkillV1 = "sha256:6e2183b3004ab12e5ec4bbd37d13bdd13e1f2f22c042d9f9ac4489d673529489"
	goldenGitHubExecSkillV2 = "sha256:87792969fc4b497e5304b83b7688dc0e3ebe12c05d47989d4cc76c201761468a"
)

// setSkillContentHashV2Experiment turns hub.skill_content_hash_v2 on or off
// on srv by replacing its registry; every other experiment keeps its
// default.
func setSkillContentHashV2Experiment(t *testing.T, srv *Server, on bool) {
	t.Helper()
	var active []experiments.Experiment
	for _, e := range srv.experimentRegistry().All() {
		if e.Name == experiments.SkillContentHashV2 {
			e.Default = on
		}
		active = append(active, e)
	}
	reg, err := experiments.NewRegistry(active, nil)
	require.NoError(t, err)
	srv.experiments = reg
	require.Equal(t, on, srv.skillContentHashV2Enabled())
}

// legacySkillFiles is a skill version published before modes were recorded.
func legacySkillFiles() []store.TemplateFile {
	return []store.TemplateFile{
		{Path: "SKILL.md", Size: 10, Hash: skillHashFileA},
		{Path: "scripts/run.sh", Size: 20, Hash: skillHashFileB},
	}
}

func TestSkillContentHashV2Experiment_RegisteredDefaultOff(t *testing.T) {
	e, ok := experiments.Default().Lookup(experiments.SkillContentHashV2)
	require.True(t, ok, "experiment %q not registered", experiments.SkillContentHashV2)
	assert.False(t, e.Default)
	assert.True(t, e.HasLayer(experiments.LayerServer))
	assert.Equal(t, "ptone/scion#5935", e.Issue)
	assert.False(t, (&Server{}).skillContentHashV2Enabled())
}

// A rename and a chmod-only change change v2 but not v1; a content change
// changes both.
func TestComputeSkillContentHashV2_RenameAndChmod(t *testing.T) {
	base := []store.TemplateFile{
		{Path: "SKILL.md", Hash: skillHashFileA, Mode: "0644"},
		{Path: "scripts/run.sh", Hash: skillHashFileB, Mode: "0755"},
	}
	renamed := []store.TemplateFile{
		{Path: "SKILL.md", Hash: skillHashFileA, Mode: "0644"},
		{Path: "scripts/start.sh", Hash: skillHashFileB, Mode: "0755"},
	}
	chmodded := []store.TemplateFile{
		{Path: "SKILL.md", Hash: skillHashFileA, Mode: "0644"},
		{Path: "scripts/run.sh", Hash: skillHashFileB, Mode: "0644"},
	}
	edited := []store.TemplateFile{
		{Path: "SKILL.md", Hash: skillHashFileA, Mode: "0644"},
		{Path: "scripts/run.sh", Hash: skillHashFileA, Mode: "0755"},
	}

	v1, v2 := computeSkillContentHash(base), computeSkillContentHashV2(base)
	require.NotEqual(t, v1, v2)
	for name, files := range map[string][]store.TemplateFile{"rename": renamed, "chmod": chmodded} {
		assert.Equal(t, v1, computeSkillContentHash(files), "%s must not change v1", name)
		assert.NotEqual(t, v2, computeSkillContentHashV2(files), "%s must change v2", name)
	}
	assert.NotEqual(t, v1, computeSkillContentHash(edited))
	assert.NotEqual(t, v2, computeSkillContentHashV2(edited))
}

// A version published before modes were recorded gets the "0644" default
// on the hub, both when v2 is derived from the stored file list and when it
// is recomputed from the download metadata the broker installs from.
func TestComputeSkillContentHashV2_LegacyVersionWithoutModes(t *testing.T) {
	files := legacySkillFiles()
	assert.Equal(t, goldenLegacySkillV1, computeSkillContentHash(files))
	assert.Equal(t, goldenLegacySkillV2, computeSkillContentHashV2(files))

	// generateDownloadURLs serves the normalised mode, "0644" here.
	served := []DownloadURLInfo{
		{Path: "SKILL.md", Hash: skillHashFileA, Mode: "0644"},
		{Path: "scripts/run.sh", Hash: skillHashFileB, Mode: "0644"},
	}
	v1, v2, ok := resolvedSkillContentHashes(served)
	require.True(t, ok)
	assert.Equal(t, goldenLegacySkillV1, v1)
	assert.Equal(t, goldenLegacySkillV2, v2)

	// A peer before ptone/scion#5652 sends no mode at all.
	for i := range served {
		served[i].Mode = ""
	}
	_, v2, ok = resolvedSkillContentHashes(served)
	require.True(t, ok)
	assert.Equal(t, goldenLegacySkillV2, v2)
}

// A gh:// skill's v2 covers the git tree mode of an executable file (the
// mode the broker installs it with, ptone/scion#5781).
func TestResolvedSkillContentHashes_GitHubExecBit(t *testing.T) {
	files := []DownloadURLInfo{
		{Path: "scripts/run.sh", Hash: skillBlobB, Mode: "0755"},
		{Path: "SKILL.md", Hash: skillBlobA, Mode: "0644"},
	}
	v1, v2, ok := resolvedSkillContentHashes(files)
	require.True(t, ok)
	assert.Equal(t, goldenGitHubExecSkillV1, v1)
	assert.Equal(t, goldenGitHubExecSkillV2, v2)
	// v1 is what computeBundleHash records for the gh resolution cache.
	assert.Equal(t, goldenGitHubExecSkillV1, computeBundleHash([]GitHubFileEntry{
		{Path: "SKILL.md", Hash: skillBlobA, Mode: "0644"},
		{Path: "scripts/run.sh", Hash: skillBlobB, Mode: "0755"},
	}))

	files[0].Hash = ""
	_, _, ok = resolvedSkillContentHashes(files)
	assert.False(t, ok, "a file without a per-file hash cannot be recomputed")
	_, _, ok = resolvedSkillContentHashes(nil)
	assert.False(t, ok)
}

func TestWithSkillContentHashV2(t *testing.T) {
	srv := &Server{}
	published := func() *store.SkillVersion {
		return &store.SkillVersion{
			Status:        store.SkillVersionStatusPublished,
			ContentHash:   goldenLegacySkillV1,
			ContentHashV2: "sha256:stale",
			Files:         legacySkillFiles(),
		}
	}

	assert.Empty(t, srv.withSkillContentHashV2(published()).ContentHashV2, "off: never sent")

	setSkillContentHashV2Experiment(t, srv, true)
	assert.Equal(t, goldenLegacySkillV2, srv.withSkillContentHashV2(published()).ContentHashV2)
	deprecated := published()
	deprecated.Status = store.SkillVersionStatusDeprecated
	assert.Equal(t, goldenLegacySkillV2, srv.withSkillContentHashV2(deprecated).ContentHashV2)
	draft := published()
	draft.Status = store.SkillVersionStatusDraft
	assert.Empty(t, srv.withSkillContentHashV2(draft).ContentHashV2)
	assert.Nil(t, srv.withSkillContentHashV2(nil))
}

// legacyResolvedSkillResponse is ResolvedSkillResponse as it was before
// ptone/scion#5935, so a test can check that the experiment-off response
// is byte-identical.
type legacyResolvedSkillResponse struct {
	URI                string            `json:"uri"`
	Name               string            `json:"name"`
	ResolvedVersion    string            `json:"resolvedVersion"`
	ContentHash        string            `json:"contentHash"`
	Files              []DownloadURLInfo `json:"files"`
	Deprecated         bool              `json:"deprecated,omitempty"`
	DeprecationMessage string            `json:"deprecationMessage,omitempty"`
	ReplacementURI     string            `json:"replacementUri,omitempty"`
}

// legacySkillVersion is store.SkillVersion's JSON before ptone/scion#5935.
type legacySkillVersion struct {
	ID                 string               `json:"id"`
	SkillID            string               `json:"skillId"`
	Version            string               `json:"version"`
	Status             string               `json:"status"`
	ContentHash        string               `json:"contentHash,omitempty"`
	Files              []store.TemplateFile `json:"files,omitempty"`
	PublisherID        string               `json:"publisherId,omitempty"`
	DeprecationMessage string               `json:"deprecationMessage,omitempty"`
	ReplacementURI     string               `json:"replacementUri,omitempty"`
	DownloadCount      int64                `json:"downloadCount"`
	Created            time.Time            `json:"created"`
}

func skillHashJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	require.NoError(t, err)
	return string(b)
}

// publishSkillVersionWithFiles adds a published version with files to skill.
func publishSkillVersionWithFiles(t *testing.T, s store.Store, skill *store.Skill, version string, files []store.TemplateFile) *store.SkillVersion {
	t.Helper()
	sv := &store.SkillVersion{
		ID:          api.NewUUID(),
		SkillID:     skill.ID,
		Version:     version,
		ContentHash: computeSkillContentHash(files),
		Files:       files,
		Status:      store.SkillVersionStatusPublished,
	}
	require.NoError(t, s.CreateSkillVersion(context.Background(), sv))
	return sv
}

// With the experiment off, skill resolve output (dispatch pre-resolution,
// which shares resolveRegistrySkillRef with POST /skills/resolve) and skill
// version JSON are byte-identical to the API before ptone/scion#5935, and a
// v2 hash is not accepted as a version. With it on, contentHashV2 is added
// and contentHash stays v1.
func TestSkillContentHashV2_ResolveAndVersionJSON(t *testing.T) {
	srv, s, alice, _, project := setupSkillAuthzTest(t)
	skill := createTestSkill(t, s, "hashv2-skill", store.SkillScopeGlobal, "", alice.ID)
	sv := publishSkillVersionWithFiles(t, s, skill, "1.0.0", legacySkillFiles())
	require.Equal(t, goldenLegacySkillV1, sv.ContentHash)

	resolve := func(version string) *ResolveSkillsResponse {
		uri := "skill://scion/global/hashv2-skill@" + version
		return srv.preResolveAgentSkills(context.Background(), dispatchTestAgent(alice.ID, project.ID, uri))
	}

	// Off.
	resp := resolve("1.0.0")
	require.NotNil(t, resp)
	require.Len(t, resp.Resolved, 1)
	got := resp.Resolved[0]
	assert.Empty(t, got.ContentHashV2)
	assert.Equal(t, goldenLegacySkillV1, got.ContentHash)
	legacy := legacyResolvedSkillResponse{
		URI: got.URI, Name: got.Name, ResolvedVersion: got.ResolvedVersion, ContentHash: got.ContentHash,
		Files: got.Files, Deprecated: got.Deprecated, DeprecationMessage: got.DeprecationMessage, ReplacementURI: got.ReplacementURI,
	}
	assert.Equal(t, skillHashJSON(t, legacy), skillHashJSON(t, got))

	stored, err := s.GetSkillVersion(context.Background(), sv.ID)
	require.NoError(t, err)
	off := srv.withSkillContentHashV2(stored)
	assert.Equal(t, skillHashJSON(t, legacySkillVersion{
		ID: off.ID, SkillID: off.SkillID, Version: off.Version, Status: off.Status, ContentHash: off.ContentHash,
		Files: off.Files, PublisherID: off.PublisherID, DeprecationMessage: off.DeprecationMessage,
		ReplacementURI: off.ReplacementURI, DownloadCount: off.DownloadCount, Created: off.Created,
	}), skillHashJSON(t, off))

	resp = resolve(goldenLegacySkillV1)
	require.Len(t, resp.Resolved, 1, "a v1 hash resolves with the experiment off")
	resp = resolve(goldenLegacySkillV2)
	assert.Empty(t, resp.Resolved, "a v2 hash does not resolve with the experiment off")
	require.Len(t, resp.Errors, 1)
	assert.Equal(t, agent.SkillErrCodeNotFound, resp.Errors[0].Code)

	// On.
	setSkillContentHashV2Experiment(t, srv, true)
	for _, version := range []string{"1.0.0", goldenLegacySkillV1, goldenLegacySkillV2} {
		resp = resolve(version)
		require.Len(t, resp.Resolved, 1, version)
		assert.Equal(t, "1.0.0", resp.Resolved[0].ResolvedVersion, version)
		assert.Equal(t, goldenLegacySkillV1, resp.Resolved[0].ContentHash, "contentHash stays v1")
		assert.Equal(t, goldenLegacySkillV2, resp.Resolved[0].ContentHashV2)
	}
	resp = resolve("sha256:" + "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef")
	assert.Empty(t, resp.Resolved, "an unknown hash still fails")
}

// An "@sha256:" lookup tries v1 before v2, and when several versions share
// a v2 hash (the same files republished), the oldest one wins.
func TestResolveSkillVersion_V2TieBreakAndV1First(t *testing.T) {
	srv, s, alice, _, _ := setupSkillAuthzTest(t)
	setSkillContentHashV2Experiment(t, srv, true)
	ctx := context.Background()
	skill := createTestSkill(t, s, "hashv2-tiebreak", store.SkillScopeGlobal, "", alice.ID)

	files := []store.TemplateFile{
		{Path: "SKILL.md", Hash: skillHashFileA, Mode: "0644"},
		{Path: "scripts/run.sh", Hash: skillHashFileB, Mode: "0755"},
	}
	first := publishSkillVersionWithFiles(t, s, skill, "1.0.0", files)
	time.Sleep(5 * time.Millisecond) // distinct Created values
	second := publishSkillVersionWithFiles(t, s, skill, "1.1.0", files)
	require.Equal(t, first.ContentHash, second.ContentHash)

	got, err := srv.resolveSkillVersion(ctx, skill.ID, computeSkillContentHashV2(files))
	require.NoError(t, err)
	assert.Equal(t, first.ID, got.ID, "the oldest version with that v2 wins")

	// A draft with the same files is never matched.
	draft := &store.SkillVersion{ID: api.NewUUID(), SkillID: skill.ID, Version: "2.0.0", Files: files}
	require.NoError(t, s.CreateSkillVersion(ctx, draft))
	got, err = srv.resolveSkillVersion(ctx, skill.ID, computeSkillContentHashV2(files))
	require.NoError(t, err)
	assert.Equal(t, first.ID, got.ID)

	// v1 first: a version whose v1 equals another version's v2 is found by
	// its v1.
	other := createTestSkill(t, s, "hashv2-v1first", store.SkillScopeGlobal, "", alice.ID)
	v2Holder := publishSkillVersionWithFiles(t, s, other, "1.0.0", files)
	v1Holder := &store.SkillVersion{
		ID: api.NewUUID(), SkillID: other.ID, Version: "1.1.0",
		ContentHash: computeSkillContentHashV2(files), // collides with v2Holder's v2
		Files:       legacySkillFiles(),
		Status:      store.SkillVersionStatusPublished,
	}
	require.NoError(t, s.CreateSkillVersion(ctx, v1Holder))
	got, err = srv.resolveSkillVersion(ctx, other.ID, computeSkillContentHashV2(files))
	require.NoError(t, err)
	assert.Equal(t, v1Holder.ID, got.ID, "a v1 match wins over a v2 match")
	assert.NotEqual(t, v2Holder.ID, got.ID)
}

// Skill resolution by "@sha256:" (dispatch pre-resolution, which shares
// resolveRegistrySkillRef with POST /skills/resolve) considers only published
// and deprecated versions, with the v2 experiment off (v1 lookup only) and on
// (v1, then v2).
func TestPreResolveAgentSkills_ContentHashUsesStatusFilter(t *testing.T) {
	srv, s, alice, _, project := setupSkillAuthzTest(t)
	ctx := context.Background()
	skill := createTestSkill(t, s, "hash-status-skill", store.SkillScopeGlobal, "", alice.ID)

	type entry struct {
		status   string
		resolves bool
		version  string
		files    []store.TemplateFile
	}
	entries := []entry{
		{status: store.SkillVersionStatusPublished, resolves: true},
		{status: store.SkillVersionStatusDeprecated, resolves: true},
		{status: store.SkillVersionStatusDraft},
		{status: store.SkillVersionStatusArchived},
	}
	for i := range entries {
		e := &entries[i]
		// A distinct file content hash gives each version its own v1 and v2
		// hash (v1 covers file contents only, so a distinct path is not
		// enough).
		e.files = []store.TemplateFile{
			{Path: "SKILL.md", Size: 10, Hash: skillHashFileA, Mode: "0644"},
			{Path: "scripts/run.sh", Size: 20, Hash: fmt.Sprintf("sha256:%064x", i+1), Mode: "0755"},
		}
		e.version = fmt.Sprintf("1.%d.0", i)
		require.NoError(t, s.CreateSkillVersion(ctx, &store.SkillVersion{
			ID:          api.NewUUID(),
			SkillID:     skill.ID,
			Version:     e.version,
			Status:      e.status,
			ContentHash: computeSkillContentHash(e.files),
			Files:       e.files,
		}))
	}

	resolve := func(hash string) *ResolveSkillsResponse {
		uri := "skill://scion/global/hash-status-skill@" + hash
		resp := srv.preResolveAgentSkills(ctx, dispatchTestAgent(alice.ID, project.ID, uri))
		require.NotNil(t, resp)
		return resp
	}
	check := func(label, hash string, e entry) {
		t.Helper()
		resp := resolve(hash)
		if e.resolves {
			require.Len(t, resp.Resolved, 1, "%s %s", label, e.status)
			assert.Equal(t, e.version, resp.Resolved[0].ResolvedVersion, "%s %s", label, e.status)
			return
		}
		assert.Empty(t, resp.Resolved, "%s %s", label, e.status)
		require.Len(t, resp.Errors, 1, "%s %s", label, e.status)
		assert.Equal(t, agent.SkillErrCodeNotFound, resp.Errors[0].Code, "%s %s", label, e.status)
	}

	for _, e := range entries {
		check("v1, experiment off", computeSkillContentHash(e.files), e)
	}
	setSkillContentHashV2Experiment(t, srv, true)
	for _, e := range entries {
		check("v1, experiment on", computeSkillContentHash(e.files), e)
		check("v2, experiment on", computeSkillContentHashV2(e.files), e)
	}
}

// skillVersionHasContent must accept exactly the statuses that resolve.
func TestSkillVersionHasContent(t *testing.T) {
	files := legacySkillFiles()
	for status, want := range map[string]bool{
		store.SkillVersionStatusPublished:  true,
		store.SkillVersionStatusDeprecated: true,
		store.SkillVersionStatusDraft:      false,
		store.SkillVersionStatusArchived:   false,
	} {
		assert.Equal(t, want, skillVersionHasContent(&store.SkillVersion{Status: status, Files: files}), status)
	}
	assert.False(t, skillVersionHasContent(&store.SkillVersion{Status: store.SkillVersionStatusPublished}))
	assert.False(t, skillVersionHasContent(nil))
}

// Golden hashes of the gh:// skill in ghModesTestTree: SKILL.md (0644) and
// an executable scripts/run.sh (0755), hashed over their git blob IDs.
const (
	goldenGHModesTreeV1 = "sha256:f8b36eda330478cbc2d7ad0c621929f05127ad7aa056a707796e950ea9e20f73"
	goldenGHModesTreeV2 = "sha256:5901c8d81ab5c6890d31faac7e28058ed46f2325f4209c7d14b46b96713b9af6"
)

// POST /skills/resolve for a gh:// skill: with the experiment off the
// response has no contentHashV2 key; with it on, contentHashV2 covers the
// executable bit from the git tree and contentHash stays v1.
func TestSkillsResolve_GitHubContentHashV2(t *testing.T) {
	const (
		owner     = "acme"
		repo      = "hashv2-repo"
		skillPath = "skills/tool"
		commitSHA = "8888888888888888888888888888888888888888"
		uri       = "gh://" + owner + "/" + repo + "/tool@main"
	)
	srv, _, alice, _, project := setupSkillAuthzTest(t)
	srv.ghResolutionStore = NewGitHubResolutionStore(enttest.NewClient(t))
	var treeCalls atomic.Int64
	var failing atomic.Bool
	ghModesTestServer(t, srv, owner, repo, skillPath, commitSHA, &treeCalls, &failing)

	resolveRaw := func() (ResolvedSkillResponse, map[string]json.RawMessage) {
		t.Helper()
		rec := doRequestAsUser(t, srv, alice, http.MethodPost, "/api/v1/skills/resolve",
			ResolveSkillsRequest{Skills: []ResolveSkillRef{{URI: uri}}, ProjectID: project.ID})
		require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())
		var raw struct {
			Resolved []map[string]json.RawMessage `json:"resolved"`
		}
		body := rec.Body.Bytes()
		require.NoError(t, json.Unmarshal(body, &raw))
		require.Len(t, raw.Resolved, 1)
		var resp ResolveSkillsResponse
		require.NoError(t, json.Unmarshal(body, &resp))
		require.Len(t, resp.Resolved, 1)
		return resp.Resolved[0], raw.Resolved[0]
	}

	got, raw := resolveRaw()
	assert.Equal(t, goldenGHModesTreeV1, got.ContentHash)
	_, present := raw["contentHashV2"]
	assert.False(t, present, "off: no contentHashV2 key")

	setSkillContentHashV2Experiment(t, srv, true)
	got, _ = resolveRaw() // served from the resolution cache
	assert.Equal(t, goldenGHModesTreeV1, got.ContentHash, "contentHash stays v1")
	assert.Equal(t, goldenGHModesTreeV2, got.ContentHashV2)
	assert.Equal(t, int64(1), treeCalls.Load())
}
