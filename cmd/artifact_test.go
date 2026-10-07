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
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const testArtifactID = "5f1c2d3e-0000-4000-8000-000000000001"

// fakeArtifactHub serves a minimal artifact API holding one single-file
// artifact whose content is body.
func fakeArtifactHub(t *testing.T, body []byte, digest string) (*httptest.Server, *[]*http.Request) {
	t.Helper()
	var seen []*http.Request
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = append(seen, r.Clone(context.Background()))
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/artifacts":
			got, _ := io.ReadAll(r.Body)
			sum := sha256.Sum256(got)
			if r.Header.Get("X-Content-SHA256") != hex.EncodeToString(sum[:]) {
				t.Errorf("publish digest header %q does not match body", r.Header.Get("X-Content-SHA256"))
			}
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(hubclient.ArtifactResponse{
				Artifact: hubclient.Artifact{ID: testArtifactID, ScopeRef: "proj-1", CurrentSeq: 1, Title: r.URL.Query().Get("title")},
				Version:  &hubclient.ArtifactVersion{Seq: 1, EntryPath: r.URL.Query().Get("name")},
			})
		case r.URL.Path == "/api/v1/artifacts/"+testArtifactID:
			_ = json.NewEncoder(w).Encode(hubclient.ArtifactResponse{
				Artifact: hubclient.Artifact{ID: testArtifactID, CurrentSeq: 1},
				Version: &hubclient.ArtifactVersion{Seq: 1, EntryPath: "design.md",
					Files: []hubclient.ArtifactFile{{Path: "design.md", SHA256: digest}}},
			})
		case r.URL.Path == "/api/v1/artifacts/"+testArtifactID+"/files/design.md",
			r.URL.Path == "/api/v1/artifacts/"+testArtifactID+"/versions/1/files/design.md":
			w.Header().Set("Content-Type", "text/markdown")
			_, _ = w.Write(body)
		default:
			w.WriteHeader(http.StatusNotFound)
			_, _ = io.WriteString(w, `{"error":{"code":"not_found","message":"not found"}}`)
		}
	}))
	t.Cleanup(srv.Close)
	return srv, &seen
}

func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func TestPublishArtifact(t *testing.T) {
	body := []byte("# Design\n")
	srv, seen := fakeArtifactHub(t, body, sha256Hex(body))
	c, err := hubclient.New(srv.URL)
	require.NoError(t, err)

	file := filepath.Join(t.TempDir(), "design.md")
	require.NoError(t, os.WriteFile(file, body, 0o644))
	var out bytes.Buffer
	require.NoError(t, publishArtifact(context.Background(), c.Artifacts(), &out, "https://hub.example/", file, "My design", "proj-1"))

	assert.Equal(t, "scion://artifact/"+testArtifactID+"  (v1)\nhttps://hub.example/projects/proj-1/artifacts/"+testArtifactID+"\n", out.String())
	require.Len(t, *seen, 1)
	q := (*seen)[0].URL.Query()
	assert.Equal(t, "design.md", q.Get("name"))
	assert.Equal(t, "My design", q.Get("title"))
	assert.Equal(t, "proj-1", q.Get("scope"))

	err = publishArtifact(context.Background(), c.Artifacts(), &out, "", t.TempDir(), "", "")
	assert.ErrorContains(t, err, "not a regular file")
}

func TestGetArtifact(t *testing.T) {
	body := []byte("# Design\n\nbody\n")
	srv, _ := fakeArtifactHub(t, body, sha256Hex(body))
	c, err := hubclient.New(srv.URL)
	require.NoError(t, err)
	ctx := context.Background()

	for _, ref := range []string{"scion://artifact/" + testArtifactID, testArtifactID, "scion://artifact/" + testArtifactID + "@1"} {
		var stdout, stderr bytes.Buffer
		require.NoError(t, getArtifact(ctx, c.Artifacts(), &stdout, &stderr, ref, ""), ref)
		assert.Equal(t, string(body), stdout.String(), ref)
	}

	// --out to a file and to a directory.
	dir := t.TempDir()
	var stdout, stderr bytes.Buffer
	outFile := filepath.Join(dir, "copy.md")
	require.NoError(t, getArtifact(ctx, c.Artifacts(), &stdout, &stderr, testArtifactID, outFile))
	got, _ := os.ReadFile(outFile)
	assert.Equal(t, body, got)
	assert.Empty(t, stdout.String())
	require.NoError(t, getArtifact(ctx, c.Artifacts(), &stdout, &stderr, testArtifactID, dir))
	got, _ = os.ReadFile(filepath.Join(dir, "design.md"))
	assert.Equal(t, body, got)

	assert.Error(t, getArtifact(ctx, c.Artifacts(), &stdout, &stderr, "scion://artifact/nope", ""))
	assert.Error(t, getArtifact(ctx, c.Artifacts(), &stdout, &stderr, "5f1c2d3e-0000-4000-8000-000000000002", ""))
}

func TestGetArtifactDetectsCorruptionBeforeStdout(t *testing.T) {
	srv, _ := fakeArtifactHub(t, []byte("tampered"), sha256Hex([]byte("original")))
	c, err := hubclient.New(srv.URL)
	require.NoError(t, err)
	var stdout, stderr bytes.Buffer
	err = getArtifact(context.Background(), c.Artifacts(), &stdout, &stderr, testArtifactID, "")
	assert.ErrorContains(t, err, "sha256")
	assert.Empty(t, stdout.String(), "unverified bytes must not reach stdout")
}

func TestGetArtifactOutMode(t *testing.T) {
	body := []byte("# Design\n")
	srv, _ := fakeArtifactHub(t, body, sha256Hex(body))
	c, err := hubclient.New(srv.URL)
	require.NoError(t, err)
	out := filepath.Join(t.TempDir(), "d.md")
	var stdout, stderr bytes.Buffer
	require.NoError(t, getArtifact(context.Background(), c.Artifacts(), &stdout, &stderr, testArtifactID, out))
	st, err := os.Stat(out)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o644), st.Mode().Perm())
}

func TestArtifactPublishScope(t *testing.T) {
	settings := &config.Settings{ProjectID: "local-only", Hub: &config.HubClientConfig{ProjectID: "hub-proj"}}
	t.Setenv("SCION_AGENT_ID", "")
	assert.Equal(t, "hub-proj", artifactPublishScope(settings), "a user names the hub project")
	assert.Equal(t, "", artifactPublishScope(&config.Settings{ProjectID: "local-only"}), "never a local-only project id")
	assert.ErrorContains(t, checkArtifactPublishScope(&config.Settings{ProjectID: "local-only"}), "not linked to a hub project")
	assert.NoError(t, checkArtifactPublishScope(settings))
	t.Setenv("SCION_AGENT_ID", "agent-1")
	assert.Equal(t, "", artifactPublishScope(settings), "a hub agent names no scope; the hub uses its project")
	assert.NoError(t, checkArtifactPublishScope(&config.Settings{}), "an agent needs no linked project")
}

func TestGetArtifactDetectsCorruption(t *testing.T) {
	srv, _ := fakeArtifactHub(t, []byte("tampered"), sha256Hex([]byte("original")))
	c, err := hubclient.New(srv.URL)
	require.NoError(t, err)
	out := filepath.Join(t.TempDir(), "x.md")
	var stdout, stderr bytes.Buffer
	err = getArtifact(context.Background(), c.Artifacts(), &stdout, &stderr, testArtifactID, out)
	assert.ErrorContains(t, err, "sha256")
	_, statErr := os.Stat(out)
	assert.True(t, os.IsNotExist(statErr), "a corrupt download must not be written to --out")
}

// TestAgentModeArtifactVerbs pins the D15 mode decision for the P1 verbs:
// artifact, artifact.publish and artifact.get are agent-callable.
func TestAgentModeArtifactVerbs(t *testing.T) {
	for _, key := range []string{"artifact", "artifact.publish", "artifact.get"} {
		assert.True(t, agentAllowed[key], "agentAllowed should contain %s", key)
		assert.NotNil(t, resolveCommandPath(rootCmd, key), "%s must resolve to a real command", key)
	}
	t.Setenv("SCION_CLI_MODE", "agent")
	root := &cobra.Command{Use: "scion"}
	real := resolveCommandPath(rootCmd, "artifact")
	require.NotNil(t, real)
	root.AddCommand(cloneCommandShape(real))
	applyModeRestrictions(root)
	names := collectCommandNames(root)
	assert.Equal(t, []string{"artifact", "artifact.get", "artifact.publish"}, names)
	assert.False(t, strings.Contains(strings.Join(names, ","), "share"))
}

func TestArtifactErrorHints(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodPost {
			w.WriteHeader(http.StatusForbidden)
			_, _ = io.WriteString(w, `{"error":{"code":"forbidden","message":"not allowed"}}`)
			return
		}
		w.WriteHeader(http.StatusNotFound)
		_, _ = io.WriteString(w, `{"error":{"code":"not_found","message":"not found"}}`)
	}))
	t.Cleanup(srv.Close)
	c, err := hubclient.New(srv.URL)
	require.NoError(t, err)
	var stdout, stderr bytes.Buffer
	err = getArtifact(context.Background(), c.Artifacts(), &stdout, &stderr, testArtifactID, "")
	assert.ErrorContains(t, err, "project:artifact:read")

	file := filepath.Join(t.TempDir(), "a.md")
	require.NoError(t, os.WriteFile(file, []byte("a"), 0o644))
	err = publishArtifact(context.Background(), c.Artifacts(), &stdout, "", file, "", "")
	assert.ErrorContains(t, err, "project:artifact:write")

	unauthorized := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = io.WriteString(w, `{"error":{"code":"unauthorized","message":"authentication required"}}`)
	}))
	t.Cleanup(unauthorized.Close)
	c401, err := hubclient.New(unauthorized.URL)
	require.NoError(t, err)
	err = publishArtifact(context.Background(), c401.Artifacts(), &stdout, "", file, "", "")
	assert.ErrorContains(t, err, "project:artifact:read")
	err = getArtifact(context.Background(), c401.Artifacts(), &stdout, &stderr, testArtifactID, "")
	assert.ErrorContains(t, err, "invalid or expired")
	assert.NotContains(t, err.Error(), "project:artifact:read", "a 401 on get is about the credential, not the read scope")
}
