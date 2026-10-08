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
	"os"
	"path/filepath"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/artifacts/critic"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	cliReviewParent = "# Plan\n\nWe ship in Q3.\nOwners: docs.\n"
	cliReviewMarked = "# Plan\n\nWe {~~ship~>launch~~} in Q3.{>>date?<<}\nOwners: docs.\n"
)

func TestArtifactReviewRoundTrip(t *testing.T) {
	svc := realArtifactHub(t)
	ctx := context.Background()
	root := writeTree(t, map[string]string{"plan.md": cliReviewParent, "chart.png": "\x89PNG\r\n\x1a\nfake"})
	var out, errOut bytes.Buffer
	require.NoError(t, publishBundle(ctx, svc, &out, &errOut, "", root, bundlePublishOptions{Key: "plan", Entry: "plan.md"}))
	m := refLine.FindStringSubmatch(out.String())
	require.NotNil(t, m, out.String())
	ref := m[1]

	// A single local file reviews the entry; its own name does not matter
	// and the bundle's other file is carried over without an upload.
	review := filepath.Join(t.TempDir(), "my-review.md")
	require.NoError(t, os.WriteFile(review, []byte(cliReviewMarked), 0o644))
	out.Reset()
	require.NoError(t, publishReview(ctx, svc, &out, &errOut, "", review, ref, bundlePublishOptions{Note: "looks good"}))
	assert.Equal(t, ref+"  (v2, review)\n", out.String())

	get := func(r string, opts getOptions) string {
		t.Helper()
		var stdout, stderr bytes.Buffer
		require.NoError(t, getArtifactWith(ctx, svc, &stdout, &stderr, r, "", false, opts))
		return stdout.String()
	}
	assert.Equal(t, cliReviewMarked, get(ref, getOptions{}))
	assert.Equal(t, cliReviewParent, get(ref, getOptions{Mode: critic.Clean}))
	assert.Equal(t, string(critic.AcceptText([]byte(cliReviewMarked))), get(ref, getOptions{Mode: critic.Accept}))
	assert.Equal(t, cliReviewParent, get(ref, getOptions{Kind: "publish"}))
	assert.Equal(t, cliReviewMarked, get(ref+"@2", getOptions{Kind: "review"}))

	var stdout, stderr bytes.Buffer
	err := getArtifactWith(ctx, svc, &stdout, &stderr, ref+"@1", "", false, getOptions{Kind: "review"})
	assert.ErrorContains(t, err, "no review version at or before v1")

	// --clean over a whole bundle projects text files and leaves others.
	dir := filepath.Join(t.TempDir(), "out")
	require.NoError(t, getArtifactWith(ctx, svc, &stdout, &stderr, ref, dir, false, getOptions{Mode: critic.Clean}))
	got, err := os.ReadFile(filepath.Join(dir, "plan.md"))
	require.NoError(t, err)
	assert.Equal(t, cliReviewParent, string(got))
	png, err := os.ReadFile(filepath.Join(dir, "chart.png"))
	require.NoError(t, err)
	assert.Equal(t, "\x89PNG\r\n\x1a\nfake", string(png))

	// A review of a version that is no longer current is refused locally.
	err = publishReview(ctx, svc, &out, &errOut, "", review, ref+"@1", bundlePublishOptions{})
	assert.ErrorContains(t, err, "not the current version (v2 is)")
}

func TestArtifactReviewUnmarkedChanges(t *testing.T) {
	svc := realArtifactHub(t)
	ctx := context.Background()
	root := writeTree(t, map[string]string{"plan.md": cliReviewParent})
	var out, errOut bytes.Buffer
	require.NoError(t, publishBundle(ctx, svc, &out, &errOut, "", root, bundlePublishOptions{Key: "plan"}))
	ref := refLine.FindStringSubmatch(out.String())[1]

	edited := filepath.Join(t.TempDir(), "plan.md")
	require.NoError(t, os.WriteFile(edited, []byte("# Plan\n\nWe {++really ++}ship in Q3.\nOwner: docs.\n"), 0o644))
	errOut.Reset()
	err := publishReview(ctx, svc, &out, &errOut, "", edited, ref, bundlePublishOptions{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "publish without --review")
	report := errOut.String()
	for _, want := range []string{"plan.md (modified)", "line 4:", "- Owners: docs.", "+ Owner: docs."} {
		assert.Contains(t, report, want)
	}
	// Nothing changed: the current version is still v1.
	var stdout, stderr bytes.Buffer
	require.NoError(t, getArtifactWith(ctx, svc, &stdout, &stderr, ref, "", false, getOptions{}))
	assert.Equal(t, cliReviewParent, stdout.String())
}

func TestArtifactGetOptions(t *testing.T) {
	_, err := artifactGetOptions(true, true, "")
	assert.ErrorContains(t, err, "cannot be used together")
	_, err = artifactGetOptions(false, false, "draft")
	assert.ErrorContains(t, err, "--kind must be")
	o, err := artifactGetOptions(false, true, "publish")
	require.NoError(t, err)
	assert.Equal(t, getOptions{Mode: critic.Accept, Kind: "publish"}, o)
}
