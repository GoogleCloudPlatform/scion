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

package cmd

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// 'scion harness-config install' records its source URL as metadata on the
// Hub (ptone/scion#3853): the URL is sent in the finalize body for an install
// from a remote source, in whichever scope install already uses. These tests use the mock hub in
// harness_config_scope_test.go, which records each finalize's sourceUrl.

const installTestSourceURL = "https://github.com/GoogleCloudPlatform/scion/harnesses/codex"

// TestInstallSourceURL: installSourceURL returns the URL resolveInstallSource
// fetches from (normalizeHarnessConfigSourceURL, then IsRemoteURI), or "" for
// a source resolveInstallSource reads from local disk.
func TestInstallSourceURL(t *testing.T) {
	for _, tc := range []struct {
		source, want string
	}{
		{"https://github.com/GoogleCloudPlatform/scion/harnesses/claude", "https://github.com/GoogleCloudPlatform/scion/harnesses/claude"},
		{"github.com/org/repo/tree/main/harness-configs/custom", "https://github.com/org/repo/tree/main/harness-configs/custom"},
		{":gcs:my-bucket/harness-configs/prod-claude", ":gcs:my-bucket/harness-configs/prod-claude"},
		{"https://example.com/configs/claude.tgz", "https://example.com/configs/claude.tgz"},
		{"file:///home/me/harness-configs/claude", ""},
		{"/home/me/harness-configs/claude", ""},
		{"claude-local", ""},
	} {
		assert.Equal(t, tc.want, installSourceURL(tc.source), "installSourceURL(%q)", tc.source)
	}
}

func TestInstallToHub_RecordsSourceURL(t *testing.T) {
	t.Setenv("SCION_AGENT_ID", "")

	t.Run("global install from a remote source sends the URL", func(t *testing.T) {
		setGlobalMode(t, true)
		rec := &hcScopeRecorder{}
		hubCtx, dir := newHCScopeTestEnv(t, rec)

		require.NoError(t, installToHub(hubCtx, "codex", dir, "codex", installTestSourceURL, false))
		require.Len(t, rec.createReqs, 1)
		assert.Equal(t, "global", rec.createReqs[0].Scope)
		assert.Equal(t, []string{installTestSourceURL}, rec.finalizeSourceURLs)
	})

	t.Run("global install --force over an existing config sends the URL", func(t *testing.T) {
		setGlobalMode(t, true)
		rec := &hcScopeRecorder{}
		rec.existingIDs = map[string]string{rec.key("global", "", "codex"): "hc-global"}
		hubCtx, dir := newHCScopeTestEnv(t, rec)

		require.NoError(t, installToHub(hubCtx, "codex", dir, "codex", installTestSourceURL, true))
		assert.Empty(t, rec.createReqs, "the update path must not create")
		assert.Equal(t, []string{installTestSourceURL}, rec.finalizeSourceURLs)
	})

	t.Run("unchanged files with a new source URL still finalize to record it", func(t *testing.T) {
		setGlobalMode(t, true)
		rec := &hcScopeRecorder{}
		rec.existingIDs = map[string]string{rec.key("global", "", "codex"): "hc-global"}
		rec.downloadHashes = map[string]string{"config.yaml": configYAMLHashCodex}
		rec.existingSourceURL = "https://github.com/GoogleCloudPlatform/scion/tree/v0.9.0/harnesses/codex"
		hubCtx, dir := newHCScopeTestEnv(t, rec)

		require.NoError(t, installToHub(hubCtx, "codex", dir, "codex", installTestSourceURL, true))
		assert.Equal(t, []string{installTestSourceURL}, rec.finalizeSourceURLs)
	})

	t.Run("unchanged files with the same source URL are already up to date", func(t *testing.T) {
		setGlobalMode(t, true)
		rec := &hcScopeRecorder{}
		rec.existingIDs = map[string]string{rec.key("global", "", "codex"): "hc-global"}
		rec.downloadHashes = map[string]string{"config.yaml": configYAMLHashCodex}
		rec.existingSourceURL = installTestSourceURL
		hubCtx, dir := newHCScopeTestEnv(t, rec)

		require.NoError(t, installToHub(hubCtx, "codex", dir, "codex", installTestSourceURL, true))
		assert.Zero(t, rec.finalized, "nothing changed, so nothing is finalized")
	})

	t.Run("project-scoped install from a remote source sends the URL", func(t *testing.T) {
		setGlobalMode(t, false)
		rec := &hcScopeRecorder{}
		hubCtx, dir := newHCScopeTestEnv(t, rec)

		require.NoError(t, installToHub(hubCtx, "codex", dir, "codex", installTestSourceURL, false))
		require.Len(t, rec.createReqs, 1)
		assert.Equal(t, "project", rec.createReqs[0].Scope, "install keeps its existing scope choice")
		assert.Equal(t, "proj-1913", rec.createReqs[0].ScopeID)
		assert.Equal(t, []string{installTestSourceURL}, rec.finalizeSourceURLs)
	})

	t.Run("global install from a local source sends no source URL", func(t *testing.T) {
		setGlobalMode(t, true)
		rec := &hcScopeRecorder{}
		hubCtx, dir := newHCScopeTestEnv(t, rec)

		require.NoError(t, installToHub(hubCtx, "codex", dir, "codex", installSourceURL(dir), false))
		assert.Equal(t, []string{""}, rec.finalizeSourceURLs)
	})
}

// TestSyncLocalHarnessConfigToHub_SendsNoSourceURL: 'harness-config sync' and
// 'push' upload a local directory and never send a source URL, so a source
// recorded by an earlier install is left as is.
func TestSyncLocalHarnessConfigToHub_SendsNoSourceURL(t *testing.T) {
	t.Setenv("SCION_AGENT_ID", "")
	setGlobalMode(t, true)
	rec := &hcScopeRecorder{}
	rec.existingIDs = map[string]string{rec.key("global", "", "codex"): "hc-global"}
	rec.existingSourceURL = installTestSourceURL
	hubCtx, dir := newHCScopeTestEnv(t, rec)

	require.NoError(t, syncLocalHarnessConfigToHub(hubCtx, "codex", dir, "codex"))
	assert.Equal(t, []string{""}, rec.finalizeSourceURLs)
}
