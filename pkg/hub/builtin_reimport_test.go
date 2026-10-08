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
	"context"
	"io/fs"
	"os"
	"path/filepath"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/storage"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/resources"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Recovery of a deleted built-in by re-importing it from a URL
// (ptone/scion#3544). Startup bootstrap does not re-create a deleted built-in
// (the seeded-built-ins ledger remembers it), so the documented recovery is a
// global import (POST /api/v1/resources/import) from the canonical, unpinned
// URL https://github.com/GoogleCloudPlatform/scion/harnesses/<name>.
//
// These tests pin today's behaviour on purpose. Whether a re-imported row
// keeps receiving bundled updates on a hosted hub depends only on
// IsBuiltinManaged(row.SourceURL), a prefix check in resource_source.go: the
// canonical harness URL matches, while a /tree/<ref>/ URL (including a tag
// pin) and any URL for the default template do not, so those rows stay a
// frozen, user-managed copy. Workstation hubs sync existing rows from disk and
// update every case. If IsBuiltinManaged or the bootstrap overwrite policy
// changes, these tests fail and the user docs (harness-settings.md, the
// release note) must change with them.
//
// No network: the import runs the real global import path
// (NormalizeTemplateSourceURL, discoverResourceDirs, then importResourceDirs,
// as handleResourcesImport and importFromRemote do) with the remote fetch
// replaced by a local copy of the embedded built-in. The harness-config
// reimport route ('scion harness-config update <name> --url <url>') runs the
// same importFromRemote path.

const (
	reimportCanonicalClaudeURL = "https://github.com/GoogleCloudPlatform/scion/harnesses/claude"
)

type reimportCase struct {
	name string
	kind storage.ResourceKind
	slug string
	url  string
	// builtinManaged is the expected IsBuiltinManaged(row.SourceURL) after
	// the re-import; on a hosted hub it also decides whether bootstrap
	// updates the row.
	builtinManaged bool
}

// reimportCases are the five cases the docs describe. Only the first one,
// the canonical unpinned harness URL, is built-in-managed.
var reimportCases = []reimportCase{
	{"CanonicalHarnessURL", storage.ResourceKindHarnessConfig, "claude",
		reimportCanonicalClaudeURL, true},
	{"HarnessTreeMainURL", storage.ResourceKindHarnessConfig, "claude",
		"https://github.com/GoogleCloudPlatform/scion/tree/main/harnesses/claude", false},
	{"HarnessTagPinnedURL", storage.ResourceKindHarnessConfig, "claude",
		"https://github.com/GoogleCloudPlatform/scion/tree/v0.9.0/harnesses/claude", false},
	{"DefaultTemplateURL", storage.ResourceKindTemplate, "default",
		"https://github.com/GoogleCloudPlatform/scion/resources/templates/default", false},
	{"DefaultTemplateTagPinnedURL", storage.ResourceKindTemplate, "default",
		"https://github.com/GoogleCloudPlatform/scion/tree/v0.9.0/resources/templates/default", false},
}

// copyEmbeddedBuiltin writes the embedded files of the built-in (kind, slug)
// to a temp directory, standing in for the remote fetch of its URL.
func copyEmbeddedBuiltin(t *testing.T, kind storage.ResourceKind, slug string) string {
	t.Helper()
	var entries []resources.BundledResource
	if kind == storage.ResourceKindTemplate {
		entries = resources.BuiltinTemplates()
	} else {
		entries = resources.BuiltinHarnessConfigs()
	}
	var src *resources.BundledResource
	for i := range entries {
		if entries[i].Name == slug {
			src = &entries[i]
		}
	}
	require.NotNil(t, src, "built-in %s %q not in the catalog", kind, slug)

	dst := filepath.Join(t.TempDir(), slug)
	root := src.Root
	if root == "" {
		root = "."
	}
	err := fs.WalkDir(src.FS, root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, p)
		if err != nil {
			return err
		}
		if d.IsDir() {
			return os.MkdirAll(filepath.Join(dst, rel), 0o755)
		}
		data, err := fs.ReadFile(src.FS, p)
		if err != nil {
			return err
		}
		return os.WriteFile(filepath.Join(dst, rel), data, 0o644)
	})
	require.NoError(t, err)
	return dst
}

// reimportRow is the global row of a built-in under test.
type reimportRow struct {
	id, sourceURL, contentHash string
}

func getReimportRow(t *testing.T, s store.Store, kind storage.ResourceKind, slug string) (reimportRow, bool) {
	t.Helper()
	ctx := context.Background()
	if kind == storage.ResourceKindTemplate {
		tpl, err := s.GetTemplateBySlug(ctx, slug, string(store.TemplateScopeGlobal), "")
		if err == store.ErrNotFound {
			return reimportRow{}, false
		}
		require.NoError(t, err)
		return reimportRow{tpl.ID, tpl.SourceURL, tpl.ContentHash}, true
	}
	hc, err := s.GetHarnessConfigBySlug(ctx, slug, store.HarnessConfigScopeGlobal, "")
	if err == store.ErrNotFound {
		return reimportRow{}, false
	}
	require.NoError(t, err)
	return reimportRow{hc.ID, hc.SourceURL, hc.ContentHash}, true
}

func deleteReimportRow(t *testing.T, s store.Store, kind storage.ResourceKind, slug string) {
	t.Helper()
	row, ok := getReimportRow(t, s, kind, slug)
	require.True(t, ok, "%s %q must exist before the delete", kind, slug)
	if kind == storage.ResourceKindTemplate {
		require.NoError(t, s.DeleteTemplate(context.Background(), row.id))
	} else {
		require.NoError(t, s.DeleteHarnessConfig(context.Background(), row.id))
	}
	_, ok = getReimportRow(t, s, kind, slug)
	require.False(t, ok)
}

// markReimportRowStale gives the row a content hash that differs from the
// bundled content, so the next bootstrap has an update to apply (standing in
// for an upgrade that ships new built-in content).
func markReimportRowStale(t *testing.T, s store.Store, kind storage.ResourceKind, id string) {
	t.Helper()
	ctx := context.Background()
	if kind == storage.ResourceKindTemplate {
		tpl, err := s.GetTemplate(ctx, id)
		require.NoError(t, err)
		tpl.ContentHash = "stale"
		require.NoError(t, s.UpdateTemplate(ctx, tpl))
		return
	}
	hc, err := s.GetHarnessConfig(ctx, id)
	require.NoError(t, err)
	hc.ContentHash = "stale"
	require.NoError(t, s.UpdateHarnessConfig(ctx, hc))
}

// reimportFromURL runs the global import path for one built-in from url.
func reimportFromURL(t *testing.T, srv *Server, tc reimportCase) {
	t.Helper()
	kind := srv.harnessConfigImportKind()
	if tc.kind == storage.ResourceKindTemplate {
		kind = srv.templateImportKind()
	}
	sourceURL := config.NormalizeTemplateSourceURL(tc.url)
	require.Equal(t, tc.url, sourceURL, "the import handler must store the URL unchanged")
	dir := copyEmbeddedBuiltin(t, tc.kind, tc.slug)
	// discoverResourceDirs decides the stored name and source URL for a
	// fetched directory, as importFromRemote does after the fetch.
	dirs, skipped, err := discoverResourceDirs(dir, sourceURL, kind)
	require.NoError(t, err)
	require.Empty(t, skipped)
	require.Len(t, dirs, 1)
	require.Equal(t, tc.slug, dirs[0].name)
	require.Equal(t, tc.url, dirs[0].sourceURL)
	imported := srv.importResourceDirs(context.Background(), dirs, skipped, "global", "", kind, nil)
	require.Equal(t, []string{tc.slug}, imported)
}

// hostedBootstrap runs the hosted startup bootstrap with the options
// cmd/server_foreground.go uses.
func hostedBootstrap(t *testing.T, srv *Server) {
	t.Helper()
	require.NoError(t, srv.BootstrapBundledResources(context.Background(), BootstrapOptions{
		RepairStorage:   true,
		OverwritePolicy: OverwriteBuiltinManaged,
	}))
}

// TestReimportBuiltinFromURL_Hosted locks in, per case, on a hosted hub whose
// ledger lists the deleted built-in:
//
//	(a) the re-import creates the global row;
//	(b) IsBuiltinManaged(row.SourceURL) is true only for the canonical URL;
//	(c) the next bootstrap keeps the row (same ID);
//	(d) the next bootstrap updates the row only for the canonical URL; every
//	    other case stays a frozen, user-managed copy with its source URL.
func TestReimportBuiltinFromURL_Hosted(t *testing.T) {
	for _, tc := range reimportCases {
		t.Run(tc.name, func(t *testing.T) {
			srv, s := testServer(t)
			srv.SetStorage(newMockStorage("test-bucket"))
			ctx := context.Background()
			hostedBootstrap(t, srv)
			deleteReimportRow(t, s, tc.kind, tc.slug)

			// Deleted built-ins stay deleted across a restart: the ledger
			// lists the name.
			hostedBootstrap(t, srv)
			_, ok := getReimportRow(t, s, tc.kind, tc.slug)
			require.False(t, ok, "bootstrap must not re-create the deleted built-in")
			ledger, err := srv.loadBuiltinSeedLedger(ctx)
			require.NoError(t, err)
			require.True(t, ledger.Seen(tc.kind, tc.slug))

			reimportFromURL(t, srv, tc)

			// (a) created.
			row, ok := getReimportRow(t, s, tc.kind, tc.slug)
			require.True(t, ok, "(a) re-import must create the row")
			assert.Equal(t, tc.url, row.sourceURL)
			// (b) classification.
			assert.Equal(t, tc.builtinManaged, IsBuiltinManaged(row.sourceURL),
				"(b) IsBuiltinManaged(%q)", row.sourceURL)

			markReimportRowStale(t, s, tc.kind, row.id)
			hostedBootstrap(t, srv)

			// (c) survives bootstrap.
			after, ok := getReimportRow(t, s, tc.kind, tc.slug)
			require.True(t, ok, "(c) bootstrap must keep the re-imported row")
			assert.Equal(t, row.id, after.id, "(c) bootstrap must not replace the row")

			// (d) bundled updates.
			if tc.builtinManaged {
				assert.NotEqual(t, "stale", after.contentHash,
					"(d) bootstrap must update a built-in-managed row")
				assert.True(t, IsBuiltinManaged(after.sourceURL))
			} else {
				assert.Equal(t, "stale", after.contentHash,
					"(d) bootstrap must leave a user-managed row alone on a hosted hub")
				assert.Equal(t, tc.url, after.sourceURL,
					"a user-managed row keeps its own source URL")
			}
		})
	}
}

// TestReimportBuiltinFromURL_Hosted_DeleteAndCanonicalReimportRestoresUpdates
// covers the delete-then-re-import route for a harness re-imported from a
// non-canonical URL: deleting it and re-importing from the canonical URL also
// restores bundled updates, but the delete is not required. The documented
// in-place route is covered by
// TestReimportBuiltinFromURL_Hosted_CanonicalReimportOverPinnedRowRestoresUpdates.
func TestReimportBuiltinFromURL_Hosted_DeleteAndCanonicalReimportRestoresUpdates(t *testing.T) {
	srv, s := testServer(t)
	srv.SetStorage(newMockStorage("test-bucket"))
	hostedBootstrap(t, srv)
	deleteReimportRow(t, s, storage.ResourceKindHarnessConfig, "claude")

	pinned := reimportCases[2]
	require.False(t, pinned.builtinManaged)
	reimportFromURL(t, srv, pinned)
	row, ok := getReimportRow(t, s, pinned.kind, pinned.slug)
	require.True(t, ok)
	require.False(t, IsBuiltinManaged(row.sourceURL))

	deleteReimportRow(t, s, pinned.kind, pinned.slug)
	reimportFromURL(t, srv, reimportCases[0])
	row, ok = getReimportRow(t, s, pinned.kind, pinned.slug)
	require.True(t, ok)
	require.True(t, IsBuiltinManaged(row.sourceURL))

	markReimportRowStale(t, s, pinned.kind, row.id)
	hostedBootstrap(t, srv)
	after, ok := getReimportRow(t, s, pinned.kind, pinned.slug)
	require.True(t, ok)
	assert.Equal(t, row.id, after.id)
	assert.NotEqual(t, "stale", after.contentHash, "bootstrap must update the canonical re-import")
}

// TestReimportBuiltinFromURL_Hosted_CanonicalReimportOverPinnedRowRestoresUpdates
// covers the in-place way back: re-importing the canonical URL over an
// existing user-managed (tag-pinned) row, with no delete, keeps the same row,
// re-points its source URL so it is built-in-managed again, and bootstrap
// then updates it. This is what the web import of the canonical URL and
// 'scion harness-config update <name> --url <canonical URL>' do.
func TestReimportBuiltinFromURL_Hosted_CanonicalReimportOverPinnedRowRestoresUpdates(t *testing.T) {
	srv, s := testServer(t)
	srv.SetStorage(newMockStorage("test-bucket"))
	hostedBootstrap(t, srv)
	deleteReimportRow(t, s, storage.ResourceKindHarnessConfig, "claude")

	pinned := reimportCases[2]
	require.False(t, pinned.builtinManaged)
	reimportFromURL(t, srv, pinned)
	pinnedRow, ok := getReimportRow(t, s, pinned.kind, pinned.slug)
	require.True(t, ok)
	require.False(t, IsBuiltinManaged(pinnedRow.sourceURL))

	// Canonical re-import over the existing row, no delete.
	reimportFromURL(t, srv, reimportCases[0])
	row, ok := getReimportRow(t, s, pinned.kind, pinned.slug)
	require.True(t, ok)
	assert.Equal(t, pinnedRow.id, row.id, "the canonical re-import must update the existing row in place")
	assert.Equal(t, reimportCanonicalClaudeURL, row.sourceURL)
	assert.True(t, IsBuiltinManaged(row.sourceURL), "the canonical re-import must make the row built-in-managed again")

	markReimportRowStale(t, s, pinned.kind, row.id)
	hostedBootstrap(t, srv)
	after, ok := getReimportRow(t, s, pinned.kind, pinned.slug)
	require.True(t, ok)
	assert.Equal(t, row.id, after.id)
	assert.NotEqual(t, "stale", after.contentHash, "bootstrap must update the row after the canonical re-import")
}

// TestReimportBuiltinFromURL_Workstation locks in that a workstation hub
// updates the re-imported row in every case: the *FromDir bootstrap syncs an
// existing row from the re-materialized disk copy whatever its source URL.
func TestReimportBuiltinFromURL_Workstation(t *testing.T) {
	for _, tc := range reimportCases {
		t.Run(tc.name, func(t *testing.T) {
			srv, s := testWorkstationServer(t)
			srv.SetStorage(newMockStorage("test-bucket"))
			ctx := context.Background()
			home := t.TempDir()
			t.Setenv("HOME", home)
			globalDir := filepath.Join(home, ".scion")

			workstationStart := func() {
				t.Helper()
				require.NoError(t, config.MaterializeBundledResources(globalDir, config.MaterializeOptions{Force: true}))
				require.NoError(t, srv.BootstrapTemplatesFromDir(ctx, filepath.Join(globalDir, "templates")))
				require.NoError(t, srv.BootstrapHarnessConfigsFromDir(ctx, filepath.Join(globalDir, "harness-configs")))
			}
			workstationStart()
			deleteReimportRow(t, s, tc.kind, tc.slug)
			workstationStart()
			_, ok := getReimportRow(t, s, tc.kind, tc.slug)
			require.False(t, ok, "workstation bootstrap must not re-create the deleted built-in")

			reimportFromURL(t, srv, tc)
			row, ok := getReimportRow(t, s, tc.kind, tc.slug)
			require.True(t, ok, "(a) re-import must create the row")
			assert.Equal(t, tc.builtinManaged, IsBuiltinManaged(row.sourceURL), "(b)")

			markReimportRowStale(t, s, tc.kind, row.id)
			workstationStart()

			after, ok := getReimportRow(t, s, tc.kind, tc.slug)
			require.True(t, ok, "(c) workstation bootstrap must keep the re-imported row")
			assert.Equal(t, row.id, after.id, "(c)")
			assert.NotEqual(t, "stale", after.contentHash,
				"(d) workstation bootstrap must update the re-imported row from disk")
		})
	}
}
