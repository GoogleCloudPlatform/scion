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
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestProjectWorkspaceWrite_DotScionPathRefused(t *testing.T) {
	srv, _ := testServer(t)
	project, workspacePath := createTestHubManagedProject(t, srv, "WS Write DotScion")

	existingSettingsPath := filepath.Join(workspacePath, ".scion", "settings.yaml")
	beforeSettings, err := os.ReadFile(existingSettingsPath)
	require.NoError(t, err)

	for name, p := range map[string]string{
		"top":          ".scion/settings.yaml",
		"uppercase":    ".SCION/x",
		"trailing_dot": ".scion./x",
		"nested":       "sub/.scion/x",
	} {
		t.Run(name, func(t *testing.T) {
			body := ProjectWorkspaceWriteRequest{Content: "injected: true\n"}
			rec := doRequest(t, srv, http.MethodPut, fmt.Sprintf("/api/v1/projects/%s/workspace/files/%s", project.ID, p), body)
			assert.Equal(t, http.StatusBadRequest, rec.Code, "body: %s", rec.Body.String())
			assert.Contains(t, rec.Body.String(), ".scion component")
			if name == "top" {
				afterSettings, readErr := os.ReadFile(existingSettingsPath)
				require.NoError(t, readErr)
				assert.Equal(t, string(beforeSettings), string(afterSettings), ".scion/settings.yaml must not be modified")
			} else {
				assertNoEntry(t, workspacePath, p)
				assertNoEntry(t, workspacePath, strings.SplitN(p, "/", 2)[0])
			}
		})
	}
}

func TestProjectWorkspaceUpload_DotScionPathRefused(t *testing.T) {
	srv, _ := testServer(t)
	project, workspacePath := createTestHubManagedProject(t, srv, "WS Upload DotScion")

	existingSettingsPath := filepath.Join(workspacePath, ".scion", "settings.yaml")
	beforeSettings, err := os.ReadFile(existingSettingsPath)
	require.NoError(t, err)

	for name, badPath := range map[string]string{
		"top":          ".scion/settings.yaml",
		"uppercase":    ".SCION/x",
		"trailing_dot": ".scion./x",
		"nested":       "sub/.scion/x",
	} {
		t.Run(name, func(t *testing.T) {
			files := map[string][]byte{
				badPath:      []byte("injected: true\n"),
				"allowed.go": []byte("package main\n"),
			}
			rec := doMultipartRequest(t, srv, http.MethodPost, fmt.Sprintf("/api/v1/projects/%s/workspace/files", project.ID), files)
			assert.Equal(t, http.StatusBadRequest, rec.Code, "body: %s", rec.Body.String())
			assert.Contains(t, rec.Body.String(), ".scion component")
			if name == "top" {
				afterSettings, readErr := os.ReadFile(existingSettingsPath)
				require.NoError(t, readErr)
				assert.Equal(t, string(beforeSettings), string(afterSettings), ".scion/settings.yaml must not be modified")
			} else {
				assertNoEntry(t, workspacePath, badPath)
			}
			assertNoEntry(t, workspacePath, "allowed.go")
		})
	}
}

func TestProjectWorkspaceDelete_DotScionPathRefused(t *testing.T) {
	srv, _ := testServer(t)
	project, workspacePath := createTestHubManagedProject(t, srv, "WS Delete DotScion")

	topCfg := filepath.Join(workspacePath, ".scion", "settings.yaml")
	require.NoError(t, os.MkdirAll(filepath.Dir(topCfg), 0o755))
	require.NoError(t, os.WriteFile(topCfg, []byte("schema_version: \"1\"\n"), 0o644))

	nestedCfg := filepath.Join(workspacePath, "sub", ".scion", "x")
	require.NoError(t, os.MkdirAll(filepath.Dir(nestedCfg), 0o755))
	require.NoError(t, os.WriteFile(nestedCfg, []byte("keep\n"), 0o644))

	for _, p := range []string{".scion/settings.yaml", "sub/.scion/x", ".SCION/x", ".scion./x"} {
		rec := doRequest(t, srv, http.MethodDelete, fmt.Sprintf("/api/v1/projects/%s/workspace/files/%s", project.ID, p), nil)
		assert.Equal(t, http.StatusBadRequest, rec.Code, "path %s body: %s", p, rec.Body.String())
		assert.Contains(t, rec.Body.String(), ".scion component")
	}

	// Deleting a parent directory containing .scion must not remove it.
	rec := doRequest(t, srv, http.MethodDelete, fmt.Sprintf("/api/v1/projects/%s/workspace/files/sub", project.ID), nil)
	assert.NotEqual(t, http.StatusNoContent, rec.Code, "deleting directory containing .scion must be refused")

	_, err := os.Stat(topCfg)
	assert.NoError(t, err, ".scion/settings.yaml must still exist")
	_, err = os.Stat(nestedCfg)
	assert.NoError(t, err, "sub/.scion/x must still exist")
}

func TestProjectWorkspaceWriteAndDelete_SymlinkToDotScionRefused(t *testing.T) {
	srv, _ := testServer(t)
	project, workspacePath := createTestHubManagedProject(t, srv, "WS Symlink DotScion")

	cfg := filepath.Join(workspacePath, ".scion", "settings.yaml")
	require.NoError(t, os.MkdirAll(filepath.Dir(cfg), 0o755))
	require.NoError(t, os.WriteFile(cfg, []byte("schema_version: \"1\"\n"), 0o644))
	require.NoError(t, os.Symlink(".scion", filepath.Join(workspacePath, "link")))

	// PUT through relative symlink link -> .scion must be refused.
	body := ProjectWorkspaceWriteRequest{Content: "injected\n"}
	rec := doRequest(t, srv, http.MethodPut, fmt.Sprintf("/api/v1/projects/%s/workspace/files/link/settings.yaml", project.ID), body)
	assert.Equal(t, http.StatusBadRequest, rec.Code, "body: %s", rec.Body.String())
	assert.Contains(t, rec.Body.String(), ".scion component")

	// DELETE through relative symlink link -> .scion must be refused.
	rec = doRequest(t, srv, http.MethodDelete, fmt.Sprintf("/api/v1/projects/%s/workspace/files/link/settings.yaml", project.ID), nil)
	assert.Equal(t, http.StatusBadRequest, rec.Code, "body: %s", rec.Body.String())
	assert.Contains(t, rec.Body.String(), ".scion component")

	got, err := os.ReadFile(cfg)
	require.NoError(t, err)
	assert.Equal(t, "schema_version: \"1\"\n", string(got))
}

func TestProjectWorkspaceReadAndList_DotScionAllowed(t *testing.T) {
	srv, _ := testServer(t)
	project, workspacePath := createTestHubManagedProject(t, srv, "WS Read DotScion")

	const settingsContent = "schema_version: \"1\"\ndefault_template: claude\n"
	cfg := filepath.Join(workspacePath, ".scion", "settings.yaml")
	require.NoError(t, os.MkdirAll(filepath.Dir(cfg), 0o755))
	require.NoError(t, os.WriteFile(cfg, []byte(settingsContent), 0o644))

	// Download of .scion/settings.yaml must still succeed.
	rec := doRequest(t, srv, http.MethodGet, fmt.Sprintf("/api/v1/projects/%s/workspace/files/.scion/settings.yaml", project.ID), nil)
	require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())
	assert.Equal(t, settingsContent, rec.Body.String())

	// Listing must still include .scion/settings.yaml.
	rec = doRequest(t, srv, http.MethodGet, fmt.Sprintf("/api/v1/projects/%s/workspace/files", project.ID), nil)
	require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())
	var resp ProjectWorkspaceListResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	var found bool
	for _, f := range resp.Files {
		if f.Path == ".scion/settings.yaml" {
			found = true
			break
		}
	}
	assert.True(t, found, "expected .scion/settings.yaml in workspace listing")
}

func TestProjectWorkspaceWrite_DotScionVolumesAllowed(t *testing.T) {
	srv, _ := testServer(t)
	project, workspacePath := createTestHubManagedProject(t, srv, "WS Write DotScionVolumes")

	body := ProjectWorkspaceWriteRequest{Content: "shared data\n"}
	rec := doRequest(t, srv, http.MethodPut, fmt.Sprintf("/api/v1/projects/%s/workspace/files/.scion-volumes/shared/data.txt", project.ID), body)
	require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())

	got, err := os.ReadFile(filepath.Join(workspacePath, ".scion-volumes", "shared", "data.txt"))
	require.NoError(t, err)
	assert.Equal(t, "shared data\n", string(got))
}
