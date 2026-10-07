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
	"database/sql"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/artifacts"
	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
	"github.com/GoogleCloudPlatform/scion/pkg/storage"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	_ "modernc.org/sqlite"
)

// oneAgentHost serves every request as one agent of project-1, allowed to
// read and publish there.
type oneAgentHost struct{}

func (oneAgentHost) Principal(context.Context) (string, string, string, bool) {
	return artifacts.PrincipalKindAgent, "agent-1", "project-1", true
}
func (oneAgentHost) Authorize(_ context.Context, scope, _ string) bool { return scope == "project-1" }
func (oneAgentHost) Permits(_ context.Context, scope, _ string) bool   { return scope == "project-1" }

// realArtifactHub runs the artifact service itself, on SQLite and local
// storage, so the CLI is tested against the real API.
func realArtifactHub(t *testing.T) hubclient.ArtifactService {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+filepath.Join(t.TempDir(), "a.db")+"?_pragma=busy_timeout(5000)")
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	st := artifacts.NewStore(db, "sqlite")
	require.NoError(t, st.Init(context.Background()))
	blobs, err := storage.NewLocal(storage.Config{Provider: storage.ProviderLocal, Bucket: "b", LocalPath: t.TempDir()})
	require.NoError(t, err)
	svc := artifacts.NewService(oneAgentHost{})
	svc.SetStore(st)
	svc.SetBlobStorage(blobs, "hub-1")
	srv := httptest.NewServer(svc)
	t.Cleanup(srv.Close)
	c, err := hubclient.New(srv.URL)
	require.NoError(t, err)
	return c.Artifacts()
}

func writeTree(t *testing.T, files map[string]string) string {
	t.Helper()
	root := t.TempDir()
	for p, body := range files {
		full := filepath.Join(root, filepath.FromSlash(p))
		require.NoError(t, os.MkdirAll(filepath.Dir(full), 0o755))
		require.NoError(t, os.WriteFile(full, []byte(body), 0o644))
	}
	return root
}

var refLine = regexp.MustCompile(`^(scion://artifact/[0-9a-f-]{36})  \(v(\d+)\)\n`)

func TestArtifactBundleRoundTrip(t *testing.T) {
	svc := realArtifactHub(t)
	ctx := context.Background()
	site := map[string]string{"index.html": "<img src=img/a.png>", "img/a.png": "png", "css/s.css": "body{}", ".git/HEAD": "x", ".env": "secret"}
	root := writeTree(t, site)

	var out, errOut bytes.Buffer
	require.NoError(t, publishBundle(ctx, svc, &out, &errOut, "https://hub.example", root, bundlePublishOptions{Key: "site", Note: "first"}))
	m := refLine.FindStringSubmatch(out.String())
	require.NotNil(t, m, out.String())
	ref := m[1]
	assert.Equal(t, "1", m[2])
	assert.Contains(t, out.String(), "https://hub.example/projects/project-1/artifacts/")

	// Same key: version 2 of the same artifact.
	require.NoError(t, os.WriteFile(filepath.Join(root, "index.html"), []byte("<p>v2</p>"), 0o644))
	out.Reset()
	require.NoError(t, publishBundle(ctx, svc, &out, &errOut, "", root, bundlePublishOptions{Key: "site", Note: "second\nline"}))
	m2 := refLine.FindStringSubmatch(out.String())
	require.NotNil(t, m2, out.String())
	assert.Equal(t, ref, m2[1])
	assert.Equal(t, "2", m2[2])

	// The whole bundle of version 1, hidden files left out.
	dir := filepath.Join(t.TempDir(), "v1")
	var stdout, stderr bytes.Buffer
	require.NoError(t, getArtifact(ctx, svc, &stdout, &stderr, ref+"@1", dir))
	for p, want := range map[string]string{"index.html": "<img src=img/a.png>", "img/a.png": "png", "css/s.css": "body{}"} {
		got, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(p)))
		require.NoError(t, err, p)
		assert.Equal(t, want, string(got), p)
	}
	for _, hidden := range []string{".git/HEAD", ".env"} {
		_, err := os.Stat(filepath.Join(dir, hidden))
		assert.True(t, os.IsNotExist(err), "hidden file %s was published", hidden)
	}
	assert.Contains(t, stderr.String(), "Wrote 3 files")

	// Without --out, the entry file of the current version.
	stdout.Reset()
	require.NoError(t, getArtifact(ctx, svc, &stdout, &stderr, ref, ""))
	assert.Equal(t, "<p>v2</p>", stdout.String())

	// versions lists both, newest first, current marked.
	stdout.Reset()
	require.NoError(t, listArtifactVersions(ctx, svc, &stdout, ref))
	lines := strings.Split(strings.TrimSpace(stdout.String()), "\n")
	require.Len(t, lines, 3, stdout.String())
	assert.True(t, strings.HasPrefix(lines[1], "* "+ref+"@2"), lines[1])
	assert.Contains(t, lines[1], "second line")
	assert.True(t, strings.HasPrefix(lines[2], "  "+ref+"@1"), lines[2])
	assert.Contains(t, lines[2], "first")

	// A single file with --key goes through the two-step API too.
	file := filepath.Join(t.TempDir(), "notes.md")
	require.NoError(t, os.WriteFile(file, []byte("# n"), 0o644))
	out.Reset()
	require.NoError(t, publishBundle(ctx, svc, &out, &errOut, "", file, bundlePublishOptions{Key: "notes"}))
	assert.Regexp(t, refLine, out.String())
}

func TestArtifactBundleEntryAndRefusals(t *testing.T) {
	svc := realArtifactHub(t)
	ctx := context.Background()
	var out, errOut bytes.Buffer

	noEntry := writeTree(t, map[string]string{"a.txt": "a", "b.txt": "b"})
	err := publishBundle(ctx, svc, &out, &errOut, "", noEntry, bundlePublishOptions{})
	assert.ErrorContains(t, err, "--entry")
	require.NoError(t, publishBundle(ctx, svc, &out, &errOut, "", noEntry, bundlePublishOptions{Entry: "b.txt"}))
	assert.ErrorContains(t, publishBundle(ctx, svc, &out, &errOut, "", noEntry, bundlePublishOptions{Entry: "c.txt"}), "not a file of the bundle")

	readme := writeTree(t, map[string]string{"README.md": "r", "docs/x.md": "x"})
	files, err := collectBundle(readme)
	require.NoError(t, err)
	e, err := bundleEntry(files, "")
	require.NoError(t, err)
	assert.Equal(t, "README.md", e)

	linked := writeTree(t, map[string]string{"index.md": "i"})
	require.NoError(t, os.Symlink("/etc/hostname", filepath.Join(linked, "leak")))
	assert.ErrorContains(t, publishBundle(ctx, svc, &out, &errOut, "", linked, bundlePublishOptions{}), "symbolic link")

	assert.ErrorContains(t, publishBundle(ctx, svc, &out, &errOut, "", t.TempDir(), bundlePublishOptions{}), "no files")
}

func TestSafeBundlePath(t *testing.T) {
	for _, bad := range []string{"", "../x", "a/../../x", "/etc/passwd", "a\\b", "..", "a//b", "./a"} {
		_, err := safeBundlePath("/out", bad)
		assert.Error(t, err, bad)
	}
	got, err := safeBundlePath("/out", "img/a.png")
	require.NoError(t, err)
	assert.Equal(t, filepath.Join("/out", "img", "a.png"), got)
}
