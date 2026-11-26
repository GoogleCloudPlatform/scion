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
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestPublishFromAttachmentCLI(t *testing.T) {
	const attID = "9b4e1a2c-0000-4000-8000-000000000099"
	const artID = "5f1c2d3e-0000-4000-8000-000000000001"

	var gotReq hubclient.PublishFromAttachmentRequest
	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		assert.Equal(t, http.MethodPost, r.Method)
		assert.Equal(t, "/api/v1/artifacts", r.URL.Path)
		assert.Empty(t, r.URL.RawQuery)
		require.NoError(t, json.NewDecoder(r.Body).Decode(&gotReq))
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(hubclient.ArtifactResponse{
			Artifact: hubclient.Artifact{ID: artID, ScopeRef: "proj-1", CurrentSeq: 2, Title: gotReq.Title},
			Version: &hubclient.ArtifactVersion{
				Seq: 2, EntryPath: "shot.png",
				SourceAttachmentID: gotReq.FromAttachment,
				SourceMessageID:    "msg-1",
			},
			Warnings: []string{"test warning"},
		})
	}))
	defer srv.Close()

	c, err := hubclient.New(srv.URL)
	require.NoError(t, err)

	var out, warn bytes.Buffer
	err = publishFromAttachment(context.Background(), c.Artifacts(), &out, &warn, "https://hub.example/", "scion://attachment/"+attID, bundlePublishOptions{
		Title: "Screenshot", Scope: "proj-1", Key: "shots", Note: "round 2", ExpiresIn: "30d",
	})
	require.NoError(t, err)
	assert.Equal(t, 1, calls)
	assert.Equal(t, hubclient.PublishFromAttachmentRequest{
		FromAttachment: attID,
		Title:          "Screenshot",
		ScopeRef:       "proj-1",
		Key:            "shots",
		Note:           "round 2",
		ExpiresIn:      "30d",
	}, gotReq)
	assert.Equal(t, "scion://artifact/"+artID+"  (v2)\nhttps://hub.example/projects/proj-1/artifacts/"+artID+"\n", out.String())
	assert.Equal(t, "warning: test warning\n", warn.String())

	// Malformed attachment ref fails locally before hitting the hub.
	err = publishFromAttachment(context.Background(), c.Artifacts(), &out, &warn, "", "scion://attachment/not-a-uuid", bundlePublishOptions{})
	assert.Error(t, err)
	assert.Equal(t, 1, calls)
}

func TestPublishArtifactCmd_AttachmentRefFlagGuards(t *testing.T) {
	const attID = "9b4e1a2c-0000-4000-8000-000000000099"
	settings := &config.Settings{Hub: &config.HubClientConfig{ProjectID: "proj-1"}}
	cmd := &cobra.Command{}
	cmd.SetContext(context.Background())

	origReview, origOf, origEntry, origExpires := artifactPublishReview, artifactPublishOf, artifactPublishEntry, artifactPublishExpiresIn
	t.Cleanup(func() {
		artifactPublishReview, artifactPublishOf, artifactPublishEntry, artifactPublishExpiresIn = origReview, origOf, origEntry, origExpires
	})

	artifactPublishReview = "scion://artifact/" + testArtifactID
	artifactPublishOf, artifactPublishEntry, artifactPublishExpiresIn = "", "", ""
	err := publishArtifactCmd(cmd, settings, nil, "", "scion://attachment/"+attID)
	assert.ErrorContains(t, err, "--review and --version-of cannot be used with an attachment reference")

	artifactPublishReview = ""
	artifactPublishEntry = "index.md"
	err = publishArtifactCmd(cmd, settings, nil, "", "scion://attachment/"+attID)
	assert.ErrorContains(t, err, "--entry does not apply when publishing an attachment reference")

	artifactPublishEntry = ""
	artifactPublishExpiresIn = "bad"
	err = publishArtifactCmd(cmd, settings, nil, "", "scion://attachment/"+attID)
	assert.ErrorContains(t, err, "--expires-in")
}
