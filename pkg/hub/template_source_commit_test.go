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

//go:build !no_sqlite

package hub

import (
	"archive/tar"
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type sourceCommitCASFaultStore struct {
	store.Store
	fault    *storeFaultSwitch
	injected bool
}

func (w *sourceCommitCASFaultStore) UpdateTemplateContent(ctx context.Context, row *store.Template, expected store.TemplateContentPrecondition) error {
	if w.fault.Active() && !w.injected {
		w.injected = true
		cur, err := w.Store.GetTemplate(ctx, row.ID)
		if err != nil {
			return err
		}
		cur.ContentHash = "sha256:concurrent-new-content"
		cur.SourceCommit = ""
		if err := w.Store.UpdateTemplateContent(ctx, cur, expected); err != nil {
			return err
		}
		return store.ErrTemplateConflict
	}
	return w.Store.UpdateTemplateContent(ctx, row, expected)
}

func TestTemplateSourceCommit_InitialRemoteImportMultiTemplate(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	srv, s, _ := testTemplateBootstrapServer(t)
	ctx := context.Background()

	const wantCommit = "3333333333333333333333333333333333333333"
	archive := buildTarGz(t, []tarEntry{
		{name: "pax_global_header", typeflag: tar.TypeXGlobalHeader, body: wantCommit},
		{name: "repo-main/templates/alpha/scion-agent.yaml", body: "schema_version: \"1\"\nharness: claude\n"},
		{name: "repo-main/templates/beta/scion-agent.yaml", body: "schema_version: \"1\"\nharness: gemini\n"},
	})

	prevOverride := importDownloadOverride
	importDownloadOverride = func(_ context.Context, _, _ string) ([]byte, error) {
		return archive, nil
	}
	t.Cleanup(func() { importDownloadOverride = prevOverride })

	names, err := srv.importFromRemote(
		ctx, "", "https://github.com/acme/repo/tree/main/templates",
		store.TemplateScopeGlobal, srv.templateImportKind(), nil, nil,
	)
	require.NoError(t, err)
	assert.ElementsMatch(t, []string{"alpha", "beta"}, names)

	for _, slug := range []string{"alpha", "beta"} {
		tmpl, err := s.GetTemplateBySlug(ctx, slug, store.TemplateScopeGlobal, "")
		require.NoError(t, err)
		assert.Equal(t, wantCommit, tmpl.SourceCommit, "template %s must record archive commit", slug)
	}
}

func TestTemplateSourceCommit_WriterMatrix(t *testing.T) {
	ctx := context.Background()
	const commit1 = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	const commit2 = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	const sourceURL = "https://github.com/acme/repo/tree/main/templates/t"

	seedFromGitHub := func(t *testing.T, srv *Server, s store.Store, name string) (*store.Template, string) {
		t.Helper()
		dir := t.TempDir()
		require.NoError(t, os.WriteFile(filepath.Join(dir, "scion-agent.yaml"), []byte("schema_version: \"1\"\nharness: claude\n"), 0o644))
		require.NoError(t, os.WriteFile(filepath.Join(dir, "README.md"), []byte("v1"), 0o644))

		rs := srv.templateStore()
		rs.setSourceCommit(commit1)
		_, err := rs.Bootstrap(ctx, name, dir, store.TemplateScopeGlobal, "", sourceURL, false)
		require.NoError(t, err)

		tmpl, err := s.GetTemplateBySlug(ctx, name, store.TemplateScopeGlobal, "")
		require.NoError(t, err)
		require.Equal(t, commit1, tmpl.SourceCommit)
		require.Equal(t, sourceURL, tmpl.SourceURL)
		return tmpl, dir
	}

	t.Run("OnHashMatch advances source_commit when content unchanged", func(t *testing.T) {
		srv, s, _ := testTemplateBootstrapServer(t)
		tmpl, dir := seedFromGitHub(t, srv, s, "tpl-hashmatch-advance")

		rs := srv.templateStore()
		rs.setSourceCommit(commit2)
		changed, err := rs.Bootstrap(ctx, tmpl.Name, dir, store.TemplateScopeGlobal, "", sourceURL, false)
		require.NoError(t, err)
		assert.True(t, changed)

		got, err := s.GetTemplate(ctx, tmpl.ID)
		require.NoError(t, err)
		assert.Equal(t, tmpl.ContentHash, got.ContentHash)
		assert.Equal(t, commit2, got.SourceCommit)
	})

	t.Run("OnHashMatch CAS conflict with changed content does not resurrect old commit", func(t *testing.T) {
		srv, s, _ := testTemplateBootstrapServer(t)
		_, fault := installStoreFault(t, srv, func(inner store.Store, f *storeFaultSwitch) *sourceCommitCASFaultStore {
			return &sourceCommitCASFaultStore{Store: inner, fault: f}
		})
		tmpl, dir := seedFromGitHub(t, srv, s, "tpl-hashmatch-cas")

		fault.Arm()
		rs := srv.templateStore()
		rs.setSourceCommit(commit2)
		_, err := rs.Bootstrap(ctx, tmpl.Name, dir, store.TemplateScopeGlobal, "", sourceURL, false)
		require.NoError(t, err)

		got, err := s.GetTemplate(ctx, tmpl.ID)
		require.NoError(t, err)
		assert.Equal(t, "sha256:concurrent-new-content", got.ContentHash)
		assert.Empty(t, got.SourceCommit, "stale OnHashMatch must not resurrect a commit over newer content")
	})

	t.Run("finalize with changed files clears source_commit and keeps source_url", func(t *testing.T) {
		srv, s, _ := testTemplateBootstrapServer(t)
		admin := newReimportAdmin(t, s)
		tmpl, _ := seedFromGitHub(t, srv, s, "tpl-finalize-changed")

		next := map[string]string{
			"scion-agent.yaml": "schema_version: \"1\"\nharness: claude\n",
			"README.md":        "locally edited",
		}
		stageObjects(t, srv, tmpl, next)
		rec := doRequestAsUser(t, srv, admin, http.MethodPost, "/api/v1/templates/"+tmpl.ID+"/finalize", FinalizeRequest{
			Manifest: &TemplateManifest{Version: "1.0", Files: commitManifest(next)},
		})
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

		got, err := s.GetTemplate(ctx, tmpl.ID)
		require.NoError(t, err)
		assert.Empty(t, got.SourceCommit)
		assert.Equal(t, sourceURL, got.SourceURL)
	})

	t.Run("finalize with identical files keeps source_commit", func(t *testing.T) {
		srv, s, _ := testTemplateBootstrapServer(t)
		admin := newReimportAdmin(t, s)
		tmpl, _ := seedFromGitHub(t, srv, s, "tpl-finalize-same")

		same := map[string]string{
			"scion-agent.yaml": "schema_version: \"1\"\nharness: claude\n",
			"README.md":        "v1",
		}
		rec := doRequestAsUser(t, srv, admin, http.MethodPost, "/api/v1/templates/"+tmpl.ID+"/finalize", FinalizeRequest{
			Manifest: &TemplateManifest{Version: "1.0", Files: commitManifest(same)},
		})
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

		got, err := s.GetTemplate(ctx, tmpl.ID)
		require.NoError(t, err)
		assert.Equal(t, commit1, got.SourceCommit)
		assert.Equal(t, sourceURL, got.SourceURL)
	})

	t.Run("file API write clears source_commit and keeps source_url", func(t *testing.T) {
		srv, s, _ := testTemplateBootstrapServer(t)
		admin := newReimportAdmin(t, s)
		tmpl, _ := seedFromGitHub(t, srv, s, "tpl-file-edit")

		rec := doRequestAsUser(t, srv, admin, http.MethodPut, "/api/v1/templates/"+tmpl.ID+"/files/README.md", map[string]string{
			"content": "edited in web ui",
		})
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

		got, err := s.GetTemplate(ctx, tmpl.ID)
		require.NoError(t, err)
		assert.Empty(t, got.SourceCommit)
		assert.Equal(t, sourceURL, got.SourceURL)
	})

	t.Run("template clone has empty source_commit", func(t *testing.T) {
		srv, s, _ := testTemplateBootstrapServer(t)
		admin := newReimportAdmin(t, s)
		tmpl, _ := seedFromGitHub(t, srv, s, "tpl-clone-src")

		rec := doRequestAsUser(t, srv, admin, http.MethodPost, "/api/v1/templates/"+tmpl.ID+"/clone", map[string]string{
			"name":  "tpl-clone-dst",
			"scope": store.TemplateScopeGlobal,
		})
		require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())

		var clonedResp store.Template
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &clonedResp))
		cloned, err := s.GetTemplateBySlug(ctx, "tpl-clone-dst", store.TemplateScopeGlobal, "")
		require.NoError(t, err)
		assert.Empty(t, cloned.SourceCommit)
		assert.Empty(t, cloned.SourceURL)
		assert.Empty(t, clonedResp.SourceCommit)
	})

	t.Run("metadata update preserves source_commit", func(t *testing.T) {
		srv, s, _ := testTemplateBootstrapServer(t)
		admin := newReimportAdmin(t, s)
		tmpl, _ := seedFromGitHub(t, srv, s, "tpl-meta")

		rec := doRequestAsUser(t, srv, admin, http.MethodPut, "/api/v1/templates/"+tmpl.ID, map[string]string{
			"name":        tmpl.Name,
			"description": "new description",
		})
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

		got, err := s.GetTemplate(ctx, tmpl.ID)
		require.NoError(t, err)
		assert.Equal(t, "new description", got.Description)
		assert.Equal(t, commit1, got.SourceCommit)
	})

	t.Run("non-GitHub bootstrap with empty commit clears source_commit", func(t *testing.T) {
		srv, s, _ := testTemplateBootstrapServer(t)
		tmpl, dir := seedFromGitHub(t, srv, s, "tpl-non-gh")

		_, err := srv.templateStore().Bootstrap(ctx, tmpl.Name, dir, store.TemplateScopeGlobal, "", "", true)
		require.NoError(t, err)

		got, err := s.GetTemplate(ctx, tmpl.ID)
		require.NoError(t, err)
		assert.Empty(t, got.SourceCommit)
	})
}

func TestResolveDerivedConfig_RecordsTemplateProvenance(t *testing.T) {
	srv, s, _ := testTemplateBootstrapServer(t)
	ctx := context.Background()

	const commit1 = "cccccccccccccccccccccccccccccccccccccccc"
	const commit2 = "dddddddddddddddddddddddddddddddddddddddd"
	const sourceURL = "https://github.com/acme/repo/tree/main/.scion/templates/dev"

	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "scion-agent.yaml"), []byte("schema_version: \"1\"\nharness: claude\n"), 0o644))
	rs := srv.templateStore()
	rs.setSourceCommit(commit1)
	_, err := rs.Bootstrap(ctx, "dev", dir, store.TemplateScopeGlobal, "", sourceURL, true)
	require.NoError(t, err)

	tmpl, err := s.GetTemplateBySlug(ctx, "dev", store.TemplateScopeGlobal, "")
	require.NoError(t, err)

	agent := &store.Agent{
		ID:            "agent-prov-1",
		Slug:          "agent-prov-1",
		Name:          "agent-prov-1",
		AppliedConfig: &store.AgentAppliedConfig{},
	}
	require.NoError(t, srv.resolveDerivedConfig(ctx, agent, nil, tmpl))

	assert.Equal(t, tmpl.ID, agent.AppliedConfig.TemplateID)
	assert.Equal(t, tmpl.ContentHash, agent.AppliedConfig.TemplateHash)
	assert.Equal(t, sourceURL, agent.AppliedConfig.TemplateSourceURL)
	assert.Equal(t, commit1, agent.AppliedConfig.TemplateSourceCommit)

	// Refreshing the template to a newer commit must not alter the agent's recorded provenance.
	rs2 := srv.templateStore()
	rs2.setSourceCommit(commit2)
	_, err = rs2.Bootstrap(ctx, "dev", dir, store.TemplateScopeGlobal, "", sourceURL, true)
	require.NoError(t, err)

	refreshed, err := s.GetTemplate(ctx, tmpl.ID)
	require.NoError(t, err)
	assert.Equal(t, commit2, refreshed.SourceCommit)
	assert.Equal(t, commit1, agent.AppliedConfig.TemplateSourceCommit)
}
