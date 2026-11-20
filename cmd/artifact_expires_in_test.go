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
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestArtifactPublishExpiresInFlagAndMode(t *testing.T) {
	assert.True(t, agentAllowed["artifact.publish"])
	f := artifactPublishCmd.Flags().Lookup("expires-in")
	require.NotNil(t, f, "--expires-in flag must be registered on artifact publish")

	// Verify artifact publish (with its flags) survives agent mode restrictions.
	root := &cobra.Command{Use: "scion"}
	art := &cobra.Command{Use: "artifact", Long: artifactCmd.Long}
	pub := &cobra.Command{Use: "publish"}
	pub.Flags().String("expires-in", "", "")
	art.AddCommand(pub)
	root.AddCommand(art)
	applyModeRestrictions(root, ModeAgent)
	cmd, _, err := root.Find([]string{"artifact", "publish"})
	require.NoError(t, err)
	assert.NotNil(t, cmd.Flags().Lookup("expires-in"))
}

func TestPublishArtifactSingleFileSendsExpiresIn(t *testing.T) {
	body := []byte("# Demo\n")
	srv, seen := fakeArtifactHub(t, body, sha256Hex(body))
	c, err := hubclient.New(srv.URL)
	require.NoError(t, err)

	file := filepath.Join(t.TempDir(), "demo.md")
	require.NoError(t, os.WriteFile(file, body, 0o644))

	origExpires := artifactPublishExpiresIn
	origTitle, origKey, origNote, origEntry, origReview, origOf := artifactPublishTitle, artifactPublishKey, artifactPublishNote, artifactPublishEntry, artifactPublishReview, artifactPublishOf
	t.Cleanup(func() {
		artifactPublishExpiresIn = origExpires
		artifactPublishTitle, artifactPublishKey, artifactPublishNote, artifactPublishEntry, artifactPublishReview, artifactPublishOf = origTitle, origKey, origNote, origEntry, origReview, origOf
	})
	artifactPublishTitle, artifactPublishKey, artifactPublishNote, artifactPublishEntry, artifactPublishReview, artifactPublishOf = "", "", "", "", "", ""
	artifactPublishExpiresIn = "720h"

	var out bytes.Buffer
	cmd := &cobra.Command{}
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetContext(context.Background())

	settings := &config.Settings{Hub: &config.HubClientConfig{ProjectID: "proj-1"}}
	require.NoError(t, publishArtifactCmd(cmd, settings, c, srv.URL, file))
	require.Len(t, *seen, 1)
	assert.Equal(t, "720h", (*seen)[0].URL.Query().Get("expires_in"))
}

func TestPublishBundleSendsExpiresIn(t *testing.T) {
	var gotReq hubclient.CreateVersionRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/artifacts":
			require.NoError(t, json.NewDecoder(r.Body).Decode(&gotReq))
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(hubclient.PendingVersionResponse{
				Artifact: hubclient.Artifact{ID: testArtifactID, ScopeRef: "proj-1", Title: "Demo"},
				Version:  &hubclient.ArtifactVersion{Seq: 1, EntryPath: "demo.md"},
			})
		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/artifacts/"+testArtifactID+"/versions/1/finalize":
			_ = json.NewEncoder(w).Encode(hubclient.ArtifactResponse{
				Artifact: hubclient.Artifact{ID: testArtifactID, ScopeRef: "proj-1", CurrentSeq: 1, Title: "Demo"},
				Version:  &hubclient.ArtifactVersion{Seq: 1, EntryPath: "demo.md"},
			})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	c, err := hubclient.New(srv.URL)
	require.NoError(t, err)

	file := filepath.Join(t.TempDir(), "demo.md")
	require.NoError(t, os.WriteFile(file, []byte("# Demo\n"), 0o644))

	var out bytes.Buffer
	require.NoError(t, publishBundle(context.Background(), c.Artifacts(), &out, &out, srv.URL, file, bundlePublishOptions{
		Key: "demo-key", Scope: "proj-1", ExpiresIn: "30d",
	}))
	assert.Equal(t, "30d", gotReq.ExpiresIn)
}

func TestPublishArtifactCmdRejectsInvalidExpiresIn(t *testing.T) {
	file := filepath.Join(t.TempDir(), "demo.md")
	require.NoError(t, os.WriteFile(file, []byte("hi"), 0o644))

	origExpires := artifactPublishExpiresIn
	origTitle, origKey, origNote, origEntry, origReview, origOf := artifactPublishTitle, artifactPublishKey, artifactPublishNote, artifactPublishEntry, artifactPublishReview, artifactPublishOf
	t.Cleanup(func() {
		artifactPublishExpiresIn = origExpires
		artifactPublishTitle, artifactPublishKey, artifactPublishNote, artifactPublishEntry, artifactPublishReview, artifactPublishOf = origTitle, origKey, origNote, origEntry, origReview, origOf
	})

	settings := &config.Settings{Hub: &config.HubClientConfig{ProjectID: "proj-1"}}
	cmd := &cobra.Command{}
	cmd.SetContext(context.Background())

	artifactPublishTitle, artifactPublishKey, artifactPublishNote, artifactPublishEntry, artifactPublishReview, artifactPublishOf = "", "", "", "", "", ""
	artifactPublishExpiresIn = "bogus"
	err := publishArtifactCmd(cmd, settings, nil, "", file)
	assert.ErrorContains(t, err, "--expires-in")

	artifactPublishExpiresIn = "720h"
	artifactPublishOf = "scion://artifact/" + testArtifactID
	err = publishArtifactCmd(cmd, settings, nil, "", file)
	assert.ErrorContains(t, err, "--expires-in")
}
