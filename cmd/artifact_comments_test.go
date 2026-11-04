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

package cmd

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/artifacts"
	"github.com/GoogleCloudPlatform/scion/pkg/artifacts/critic"
	"github.com/GoogleCloudPlatform/scion/pkg/clitime"
	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const cliCommentsMarked = "# Plan\n\nWe {~~ship~>launch~~} in Q3.{>>date?<<}\nOwners: {==docs==}{>>which team?<<}.\n"

// publishCommentsFixture publishes plan.md (v1) and a review of it (v2)
// with a note, and returns the artifact's reference.
func publishCommentsFixture(t *testing.T, svc hubclient.ArtifactService) string {
	t.Helper()
	ctx := context.Background()
	root := writeTree(t, map[string]string{"plan.md": cliReviewParent, "chart.png": "\x89PNG\r\n\x1a\n{--keep--}"})
	var out, errOut bytes.Buffer
	require.NoError(t, publishBundle(ctx, svc, &out, &errOut, "", root, bundlePublishOptions{Key: "plan", Entry: "plan.md"}))
	m := refLine.FindStringSubmatch(out.String())
	require.NotNil(t, m, out.String())
	review := filepath.Join(t.TempDir(), "plan.md")
	require.NoError(t, os.WriteFile(review, []byte(cliCommentsMarked), 0o644))
	require.NoError(t, publishReview(ctx, svc, &out, &errOut, "", review, m[1], bundlePublishOptions{Note: "Two questions.\nOtherwise fine."}))
	return m[1]
}

func TestArtifactCommentsRoundTrip(t *testing.T) {
	svc := realArtifactHub(t)
	ctx := context.Background()
	ref := publishCommentsFixture(t, svc)
	id, _, err := artifacts.ParseRef(ref)
	require.NoError(t, err)

	// A newer published version does not hide the review: a bare
	// reference lists the newest review, and says which version it read.
	edited := filepath.Join(t.TempDir(), "plan.md")
	require.NoError(t, os.WriteFile(edited, []byte(cliReviewParent), 0o644))
	var out, errOut bytes.Buffer
	require.NoError(t, publishVersionOf(ctx, svc, &out, &errOut, "", edited, ref, bundlePublishOptions{Note: "resolved"}))

	var stderr bytes.Buffer
	rc, err := fetchReviewComments(ctx, svc, &stderr, ref)
	require.NoError(t, err)
	assert.Equal(t, "Using "+artifacts.FormatRef(id, 2)+" (review)\n", stderr.String())
	assert.Equal(t, reviewCommentsSchemaVersion, rc.SchemaVersion)
	assert.Equal(t, ref, rc.Artifact)
	assert.Equal(t, artifacts.FormatRef(id, 2), rc.Ref)
	assert.Equal(t, 2, rc.Seq)
	assert.Equal(t, "Two questions.\nOtherwise fine.", rc.Note)
	author := reviewAuthor{Kind: artifacts.PrincipalKindAgent, Ref: "agent-1"}
	assert.Equal(t, author, rc.Author)
	assert.False(t, rc.CreatedAt.IsZero())
	assert.Equal(t, time.UTC, rc.CreatedAt.Location())

	// Only text files are read: the image's bytes that look like a mark
	// are not an entry. Every entry has the review's author and time.
	require.Len(t, rc.Entries, 3)
	for _, e := range rc.Entries {
		assert.Equal(t, "plan.md", e.File)
		assert.Equal(t, author, e.Author)
		assert.Equal(t, rc.CreatedAt, e.CreatedAt)
	}
	assert.Equal(t, reviewCommentEntry{Kind: critic.EntrySuggestion, Change: critic.ChangeReplace, File: "plan.md",
		Line: 3, Column: 4, Start: 11, End: 29, Anchor: "ship", Suggestion: "launch", Author: author, CreatedAt: rc.CreatedAt}, rc.Entries[0])
	assert.Equal(t, reviewCommentEntry{Kind: critic.EntryComment, File: "plan.md",
		Line: 3, Column: 29, Start: 36, End: 47, Comment: "date?", Author: author, CreatedAt: rc.CreatedAt}, rc.Entries[1])
	assert.Equal(t, reviewCommentEntry{Kind: critic.EntryComment, File: "plan.md",
		Line: 4, Column: 9, Start: 56, End: 83, Anchor: "docs", Comment: "which team?", Author: author, CreatedAt: rc.CreatedAt}, rc.Entries[2])

	// A reference pinning the review reads it without a notice.
	stderr.Reset()
	pinned, err := fetchReviewComments(ctx, svc, &stderr, artifacts.FormatRef(id, 2))
	require.NoError(t, err)
	assert.Empty(t, stderr.String())
	assert.Equal(t, rc.Entries, pinned.Entries)

	// A pinned version that is not a review is refused.
	_, err = fetchReviewComments(ctx, svc, &stderr, artifacts.FormatRef(id, 1))
	assert.ErrorContains(t, err, artifacts.FormatRef(id, 1)+" is a publish version, not a review")
	_, err = fetchReviewComments(ctx, svc, &stderr, artifacts.FormatRef(id, 3))
	assert.ErrorContains(t, err, "is a publish version, not a review")

	// The command prints JSON or the human listing.
	var stdout bytes.Buffer
	require.NoError(t, listReviewComments(ctx, svc, &stdout, &stderr, ref, true))
	assert.Contains(t, stdout.String(), `"schemaVersion": 1`)
	assert.Contains(t, stdout.String(), `"comment": "which team?"`)
	stdout.Reset()
	require.NoError(t, listReviewComments(ctx, svc, &stdout, &stderr, ref, false))
	assert.Contains(t, stdout.String(), "plan.md:4:9  comment\n  on \"docs\"\n  which team?\n")
	assert.Contains(t, stdout.String(), "2 comments, 1 suggestion, 0 highlights\n")
}

func TestArtifactCommentsNoReview(t *testing.T) {
	svc := realArtifactHub(t)
	root := writeTree(t, map[string]string{"plan.md": cliReviewParent})
	var out, errOut bytes.Buffer
	require.NoError(t, publishBundle(context.Background(), svc, &out, &errOut, "", root, bundlePublishOptions{Key: "plan"}))
	ref := refLine.FindStringSubmatch(out.String())[1]

	var stdout, stderr bytes.Buffer
	err := listReviewComments(context.Background(), svc, &stdout, &stderr, ref, false)
	assert.ErrorContains(t, err, "has no review version at or before v1")
	assert.Empty(t, stdout.String())
}

// TestArtifactCommentsNotReadable: the hub's uniform 404 for an artifact
// the caller may not read gives the same explanation as artifact get, and
// prints nothing.
func TestArtifactCommentsNotReadable(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = io.WriteString(w, `{"error":{"code":"not_found","message":"not found"}}`)
	}))
	t.Cleanup(srv.Close)
	c, err := hubclient.New(srv.URL)
	require.NoError(t, err)
	for _, ref := range []string{testArtifactID, testArtifactID + "@2"} {
		var stdout, stderr bytes.Buffer
		err = listReviewComments(context.Background(), c.Artifacts(), &stdout, &stderr, ref, true)
		assert.ErrorContains(t, err, "you cannot read it")
		assert.ErrorContains(t, err, "project:artifact:read")
		assert.Empty(t, stdout.String())
	}
}

// goldenReviewComments is a fixed listing for the golden outputs.
func goldenReviewComments() *reviewComments {
	at := time.Date(2026, 11, 2, 9, 30, 0, 0, time.UTC)
	author := reviewAuthor{Kind: "user", Ref: "user-1"}
	ref := "scion://artifact/5f1c2d3e-6b1a-4c55-9f3e-0d6e7a1b2c3d"
	rc := &reviewComments{
		SchemaVersion: reviewCommentsSchemaVersion, Artifact: ref, Ref: ref + "@2", Seq: 2,
		Author: author, CreatedAt: at, Note: "Two questions.\nOtherwise fine.",
	}
	src := "# Plan\n\nWe {~~ship~>launch~~} in Q3.{>>date?<<}\n{++New line.++} Drop {--this--}.\n{==Owners==}: docs.{>>which\nteam?<<}\n"
	for _, e := range critic.Entries([]byte(src)) {
		rc.Entries = append(rc.Entries, reviewCommentEntry{
			Kind: e.Kind, Change: e.Change, File: "plan.md", Line: e.Line, Column: e.Column, Start: e.Start, End: e.End,
			Anchor: e.Anchor, Comment: e.Comment, Suggestion: e.Suggestion, Author: author, CreatedAt: at,
		})
	}
	return rc
}

// TestArtifactCommentsGolden pins the JSON (schemaVersion 1) and the human
// listing byte for byte. Run with SCION_TEST_UPDATE_GOLDEN=1 to write the
// files again after a deliberate change.
func TestArtifactCommentsGolden(t *testing.T) {
	clitime.SetZone(time.UTC)
	t.Cleanup(func() { clitime.SetZone(nil) })
	for _, tc := range []struct {
		golden string
		json   bool
	}{
		{"artifact-comments.json", true},
		{"artifact-comments.txt", false},
	} {
		t.Run(tc.golden, func(t *testing.T) {
			var out bytes.Buffer
			if tc.json {
				require.NoError(t, encodeReviewCommentsJSON(&out, goldenReviewComments()))
			} else {
				require.NoError(t, printReviewComments(&out, goldenReviewComments()))
			}
			path := filepath.Join("testdata", tc.golden)
			if os.Getenv(projectListGoldenEnv) == "1" {
				require.NoError(t, os.WriteFile(path, out.Bytes(), 0o644))
				return
			}
			want, err := os.ReadFile(path)
			require.NoError(t, err)
			assert.Equal(t, string(want), out.String())
		})
	}
}

func TestArtifactCommentsEmptyListing(t *testing.T) {
	clitime.SetZone(time.UTC)
	t.Cleanup(func() { clitime.SetZone(nil) })
	rc := &reviewComments{Ref: "scion://artifact/x@2", Author: reviewAuthor{Kind: "agent", Ref: "a"},
		CreatedAt: time.Date(2026, 11, 2, 9, 30, 0, 0, time.UTC), Entries: []reviewCommentEntry{}}
	var out bytes.Buffer
	require.NoError(t, printReviewComments(&out, rc))
	assert.Equal(t, "scion://artifact/x@2  review by agent:a, 2026-11-02 09:30 UTC\n\nNo comments, suggestions or highlights.\n", out.String())
	out.Reset()
	require.NoError(t, encodeReviewCommentsJSON(&out, rc))
	assert.Contains(t, out.String(), `"entries": []`)
	assert.Contains(t, out.String(), `"note": ""`)
}
