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
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

func overwriteWarnRecords(h *levelCapturingHandler) []slog.Record {
	h.mu.Lock()
	defer h.mu.Unlock()
	var out []slog.Record
	for _, r := range h.records {
		if strings.Contains(r.Message, "hub record replaced from local disk copy") {
			out = append(out, r)
		}
	}
	return out
}

// ptone/scion#611 (workstation mode): when the startup bootstrap replaces
// an existing hub harness-config from the ~/.scion copy, it must WARN with
// the old and new content hashes. An unchanged re-run must stay quiet.
func TestBootstrapHarnessConfigsFromDir_OverwriteWarnsWithHashes(t *testing.T) {
	srv, s, _ := testTemplateBootstrapServer(t)
	ctx := context.Background()
	h := &levelCapturingHandler{}
	srv.resourceLog = slog.New(h)

	dir := makeHarnessConfigDir(t, "ws-claude", map[string]string{
		"config.yaml": "harness: claude\n",
	})
	require.NoError(t, srv.BootstrapHarnessConfigsFromDir(ctx, dir))
	before, err := s.GetHarnessConfigBySlug(ctx, "ws-claude", store.HarnessConfigScopeGlobal, "")
	require.NoError(t, err)

	// Unchanged disk copy: no overwrite, no warning.
	require.NoError(t, srv.BootstrapHarnessConfigsFromDir(ctx, dir))
	assert.Empty(t, overwriteWarnRecords(h), "an unchanged re-import must not warn")

	require.NoError(t, os.WriteFile(filepath.Join(dir, "ws-claude", "config.yaml"),
		[]byte("harness: claude\nimage: changed:latest\n"), 0644))
	require.NoError(t, srv.BootstrapHarnessConfigsFromDir(ctx, dir))

	after, err := s.GetHarnessConfigBySlug(ctx, "ws-claude", store.HarnessConfigScopeGlobal, "")
	require.NoError(t, err)
	require.NotEqual(t, before.ContentHash, after.ContentHash)

	found := overwriteWarnRecords(h)
	require.Len(t, found, 1)
	assert.Equal(t, slog.LevelWarn, found[0].Level)
	oldHash, ok := recordAttr(found[0], "oldHash")
	require.True(t, ok)
	assert.Equal(t, before.ContentHash, oldHash.String())
	newHash, ok := recordAttr(found[0], "newHash")
	require.True(t, ok)
	assert.Equal(t, after.ContentHash, newHash.String())
}

// Same for templates: hub-side template edits are overwritten from disk too.
func TestBootstrapTemplatesFromDir_OverwriteWarnsWithHashes(t *testing.T) {
	srv, s, _ := testTemplateBootstrapServer(t)
	ctx := context.Background()
	// The template WARN goes to the template subsystem logger, alongside
	// the template bootstrap's other lines.
	h := &levelCapturingHandler{}
	srv.templateLog = slog.New(h)

	dir := makeTemplateDir(t, "ws-tmpl", map[string]string{
		"scion-agent.yaml": "harness: claude\n",
	})
	require.NoError(t, srv.BootstrapTemplatesFromDir(ctx, dir))
	before, err := s.GetTemplateBySlug(ctx, "ws-tmpl", string(store.TemplateScopeGlobal), "")
	require.NoError(t, err)

	// Unchanged disk copy: no overwrite, no warning (template sync computes
	// its own "changed", so a false positive would WARN on every start).
	require.NoError(t, srv.BootstrapTemplatesFromDir(ctx, dir))
	assert.Empty(t, overwriteWarnRecords(h), "an unchanged re-import must not warn")

	require.NoError(t, os.WriteFile(filepath.Join(dir, "ws-tmpl", "scion-agent.yaml"),
		[]byte("harness: claude\nmodel: other\n"), 0644))
	require.NoError(t, srv.BootstrapTemplatesFromDir(ctx, dir))

	after, err := s.GetTemplateBySlug(ctx, "ws-tmpl", string(store.TemplateScopeGlobal), "")
	require.NoError(t, err)
	require.NotEqual(t, before.ContentHash, after.ContentHash)

	found := overwriteWarnRecords(h)
	require.Len(t, found, 1)
	assert.Equal(t, slog.LevelWarn, found[0].Level)
	oldHash, ok := recordAttr(found[0], "oldHash")
	require.True(t, ok)
	assert.Equal(t, before.ContentHash, oldHash.String())
	newHash, ok := recordAttr(found[0], "newHash")
	require.True(t, ok)
	assert.Equal(t, after.ContentHash, newHash.String())
	kind, ok := recordAttr(found[0], "kind")
	require.True(t, ok)
	assert.Equal(t, "template", kind.String())
}
