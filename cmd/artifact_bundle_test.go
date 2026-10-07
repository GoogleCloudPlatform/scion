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
	"fmt"
	"io"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/artifacts"
	"github.com/GoogleCloudPlatform/scion/pkg/clitime"
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
func (oneAgentHost) MemberScopes(context.Context) ([]string, error) {
	return []string{"project-1"}, nil
}

// SealCursor and OpenCursor pass the list position through unchanged: this
// test host has one caller, so there is nothing to bind it to.
func (oneAgentHost) SealCursor(_ context.Context, position, _ string) (string, error) {
	return position, nil
}
func (oneAgentHost) OpenCursor(_ context.Context, cursor, _ string) (string, error) {
	return cursor, nil
}

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
	require.NoError(t, getArtifact(ctx, svc, &stdout, &stderr, ref+"@1", dir, false))
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
	require.NoError(t, getArtifact(ctx, svc, &stdout, &stderr, ref, "", false))
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

	// A title given when appending is not applied, and the CLI says so.
	errOut.Reset()
	require.NoError(t, publishBundle(ctx, svc, &out, &errOut, "", root, bundlePublishOptions{Key: "site", Title: "Renamed"}))
	assert.Contains(t, errOut.String(), "keeps its title")

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
	require.NoError(t, os.Symlink("/etc/hostname", filepath.Join(linked, "linked")))
	assert.ErrorContains(t, publishBundle(ctx, svc, &out, &errOut, "", linked, bundlePublishOptions{}), "symbolic link")

	assert.ErrorContains(t, publishBundle(ctx, svc, &out, &errOut, "", t.TempDir(), bundlePublishOptions{}), "no files")
}

func TestCheckBundleName(t *testing.T) {
	for _, bad := range []string{"", "../x", "a/../../x", "/etc/passwd", "a\\b", "..", "a//b", "./a",
		".git/config", "docs/.hidden/a", ".envrc", "a/.b", "..\\x", "c:/x", "C:x"} {
		assert.Error(t, checkBundleName(bad), bad)
	}
	for _, good := range []string{"img/a.png", "a.md", "docs/sub/b.txt"} {
		assert.NoError(t, checkBundleName(good), good)
	}
}

func TestWriteBundleRefusesFoldedCollisions(t *testing.T) {
	files := []hubclient.ArtifactFile{{Path: "a.md", SHA256: "x"}, {Path: "A.md", SHA256: "y"}}
	err := writeBundle(context.Background(), nil, &bytes.Buffer{}, "id", 1, files, t.TempDir(), false)
	assert.ErrorContains(t, err, "same file")
}

func TestArtifactBundleEdgeCases(t *testing.T) {
	svc := realArtifactHub(t)
	ctx := context.Background()
	var out, errOut, stdout, stderr bytes.Buffer

	// A one-file bundle whose file sits in a folder keeps its path.
	root := writeTree(t, map[string]string{"docs/a.md": "# a"})
	require.NoError(t, publishBundle(ctx, svc, &out, &errOut, "", root, bundlePublishOptions{}))
	ref := refLine.FindStringSubmatch(out.String())[1]
	dir := filepath.Join(t.TempDir(), "new", "dir")
	require.NoError(t, getArtifact(ctx, svc, &stdout, &stderr, ref, dir, false))
	got, err := os.ReadFile(filepath.Join(dir, "docs", "a.md"))
	require.NoError(t, err)
	assert.Equal(t, "# a", string(got))

	// A symbolic link to a file publishes with flags too.
	target := filepath.Join(t.TempDir(), "real.md")
	require.NoError(t, os.WriteFile(target, []byte("# real"), 0o644))
	link := filepath.Join(t.TempDir(), "notes.md")
	require.NoError(t, os.Symlink(target, link))
	out.Reset()
	require.NoError(t, publishBundle(ctx, svc, &out, &errOut, "", link, bundlePublishOptions{Key: "notes"}))
	// And a symbolic link to a folder publishes the folder.
	linkDir := filepath.Join(t.TempDir(), "site")
	require.NoError(t, os.Symlink(root, linkDir))
	out.Reset()
	require.NoError(t, publishBundle(ctx, svc, &out, &errOut, "", linkDir, bundlePublishOptions{Entry: "docs/a.md"}))
}

// noDigestService serves a version whose manifest does not list its entry.
type noDigestService struct{ hubclient.ArtifactService }

func (noDigestService) Get(context.Context, string) (*hubclient.ArtifactResponse, error) {
	return &hubclient.ArtifactResponse{Version: &hubclient.ArtifactVersion{Seq: 1, EntryPath: "a.md",
		Files: []hubclient.ArtifactFile{{Path: "other.md", SHA256: "x"}}}}, nil
}

func TestGetArtifactRefusesAnEntryWithoutDigest(t *testing.T) {
	var stdout, stderr bytes.Buffer
	err := getArtifact(context.Background(), noDigestService{}, &stdout, &stderr, "5f1c2d3e-0000-4000-8000-000000000001", "", false)
	assert.ErrorContains(t, err, "no digest")
	assert.Empty(t, stdout.String())
}

// remoteRowsService serves a version whose manifest holds the files the
// publisher sent plus remote image rows the hub added: one fetched, one
// failed (no digest).
type remoteRowsService struct {
	hubclient.ArtifactService
	files map[string]string
	entry string
}

func (s remoteRowsService) Get(context.Context, string) (*hubclient.ArtifactResponse, error) {
	v := &hubclient.ArtifactVersion{Seq: 1, EntryPath: s.entry}
	for p, body := range s.files {
		v.Files = append(v.Files, hubclient.ArtifactFile{Path: p, SHA256: sha256Hex([]byte(body)), Size: int64(len(body))})
	}
	v.Files = append(v.Files,
		hubclient.ArtifactFile{Path: "_remote/" + strings.Repeat("a", 64), SHA256: sha256Hex([]byte("png")), Origin: "remote", FetchStatus: "ok"},
		hubclient.ArtifactFile{Path: "_remote/" + strings.Repeat("b", 64), Origin: "remote", FetchStatus: "failed"})
	return &hubclient.ArtifactResponse{Version: v}, nil
}

func (s remoteRowsService) OpenFile(_ context.Context, _ string, _ int, p string) (io.ReadCloser, error) {
	body, ok := s.files[p]
	if !ok {
		return nil, fmt.Errorf("unexpected fetch of %s", p)
	}
	return io.NopCloser(strings.NewReader(body)), nil
}

// TestGetArtifactSkipsRemoteRows: the remote image rows the hub added are
// not part of what get writes, so a single-file artifact with images stays
// a single file and a bundle writes exactly its own files.
func TestGetArtifactSkipsRemoteRows(t *testing.T) {
	ctx := context.Background()
	var stdout, stderr bytes.Buffer
	single := remoteRowsService{files: map[string]string{"design.md": "# d"}, entry: "design.md"}
	out := filepath.Join(t.TempDir(), "copy.md")
	require.NoError(t, getArtifact(ctx, single, &stdout, &stderr, testArtifactID, out, false))
	got, err := os.ReadFile(out)
	require.NoError(t, err)
	assert.Equal(t, "# d", string(got))
	require.NoError(t, getArtifact(ctx, single, &stdout, &stderr, testArtifactID, "", false))
	assert.Equal(t, "# d", stdout.String())

	pair := remoteRowsService{files: map[string]string{"index.md": "# i", "b.txt": "b"}, entry: "index.md"}
	dir := t.TempDir()
	require.NoError(t, getArtifact(ctx, pair, &stdout, &stderr, testArtifactID, dir, false))
	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	assert.ElementsMatch(t, []string{"index.md", "b.txt"}, names)
}

func TestCopyVerified(t *testing.T) {
	body := []byte("hello")
	var out bytes.Buffer
	assert.NoError(t, copyVerified(&out, bytes.NewReader(body), sha256Hex(body), 5))
	assert.ErrorContains(t, copyVerified(&out, bytes.NewReader(body), "", 5), "no digest")
	assert.ErrorContains(t, copyVerified(&out, bytes.NewReader(append(body, '!')), sha256Hex(body), 5), "size")
	assert.ErrorContains(t, copyVerified(&out, bytes.NewReader(body[:4]), sha256Hex(body), 5), "size")
	assert.ErrorContains(t, copyVerified(&out, bytes.NewReader([]byte("HELLO")), sha256Hex(body), 5), "sha256")
	// No more than size+1 bytes are read from a source that runs on.
	r := &countingReader{}
	_ = copyVerified(io.Discard, r, sha256Hex(body), 5)
	assert.LessOrEqual(t, r.n, 6)
}

type countingReader struct{ n int }

func (c *countingReader) Read(p []byte) (int, error) {
	c.n += len(p)
	for i := range p {
		p[i] = 'x'
	}
	return len(p), nil
}

// filesService serves a version of the given files (no remote rows).
type filesService struct {
	hubclient.ArtifactService
	files map[string]string
	entry string
}

func (s filesService) Get(context.Context, string) (*hubclient.ArtifactResponse, error) {
	v := &hubclient.ArtifactVersion{Seq: 1, EntryPath: s.entry}
	for p, body := range s.files {
		v.Files = append(v.Files, hubclient.ArtifactFile{Path: p, SHA256: sha256Hex([]byte(body)), Size: int64(len(body))})
	}
	return &hubclient.ArtifactResponse{Version: v}, nil
}

func (s filesService) OpenFile(_ context.Context, _ string, _ int, p string) (io.ReadCloser, error) {
	return io.NopCloser(strings.NewReader(s.files[p])), nil
}

// TestGetArtifactOutRules: get --out writes only plain relative names, never
// through a symbolic link below the directory, and replaces an existing
// file only with --force.
func TestGetArtifactOutRules(t *testing.T) {
	ctx := context.Background()
	var stdout, stderr bytes.Buffer

	// A version listing a name that starts with '.' is refused and writes
	// nothing.
	dir := t.TempDir()
	dotted := filesService{files: map[string]string{"index.md": "# i", ".git/config": "x"}, entry: "index.md"}
	assert.ErrorContains(t, getArtifact(ctx, dotted, &stdout, &stderr, testArtifactID, dir, false), "starting with '.'")
	entries, _ := os.ReadDir(dir)
	assert.Empty(t, entries)
	single := filesService{files: map[string]string{".notes": "x"}, entry: ".notes"}
	assert.ErrorContains(t, getArtifact(ctx, single, &stdout, &stderr, testArtifactID, dir, false), "starting with '.'")

	// A symbolic link to a folder below --out is not written through.
	outside := t.TempDir()
	dir = t.TempDir()
	require.NoError(t, os.Symlink(outside, filepath.Join(dir, "docs")))
	bundle := filesService{files: map[string]string{"index.md": "# i", "docs/a.md": "a"}, entry: "index.md"}
	assert.ErrorContains(t, getArtifact(ctx, bundle, &stdout, &stderr, testArtifactID, dir, true), "symbolic link")
	entries, _ = os.ReadDir(outside)
	assert.Empty(t, entries, "nothing written through the link")

	// An existing file is replaced only with --force.
	dir = t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "index.md"), []byte("mine"), 0o644))
	assert.ErrorContains(t, getArtifact(ctx, bundle, &stdout, &stderr, testArtifactID, dir, false), "--force")
	got, _ := os.ReadFile(filepath.Join(dir, "index.md"))
	assert.Equal(t, "mine", string(got))
	require.NoError(t, getArtifact(ctx, bundle, &stdout, &stderr, testArtifactID, dir, true))
	got, _ = os.ReadFile(filepath.Join(dir, "index.md"))
	assert.Equal(t, "# i", string(got))

	// A single file written to a path that ends in a separator goes into
	// that directory, created if needed.
	slashDir := filepath.Join(t.TempDir(), "newdir") + string(os.PathSeparator)
	require.NoError(t, getArtifact(ctx, filesService{files: map[string]string{"a.md": "x"}, entry: "a.md"}, &stdout, &stderr, testArtifactID, slashDir, false))
	got, err := os.ReadFile(filepath.Join(slashDir, "a.md"))
	require.NoError(t, err)
	assert.Equal(t, "x", string(got))

	// The same holds for a single file written to a named path.
	one := filesService{files: map[string]string{"a.md": "new"}, entry: "a.md"}
	target := filepath.Join(t.TempDir(), "copy.md")
	require.NoError(t, os.WriteFile(target, []byte("old"), 0o644))
	assert.ErrorContains(t, getArtifact(ctx, one, &stdout, &stderr, testArtifactID, target, false), "--force")
	require.NoError(t, getArtifact(ctx, one, &stdout, &stderr, testArtifactID, target, true))
	got, _ = os.ReadFile(target)
	assert.Equal(t, "new", string(got))
}

func TestPublishTitleNoteIgnoresSurroundingSpace(t *testing.T) {
	svc := realArtifactHub(t)
	var out, errOut bytes.Buffer
	root := writeTree(t, map[string]string{"a.md": "# a"})
	require.NoError(t, publishBundle(context.Background(), svc, &out, &errOut, "", root, bundlePublishOptions{Key: "t", Title: "  Q3  "}))
	assert.NotContains(t, errOut.String(), "keeps its title")
}

// noVersionService answers the create step without a version.
type noVersionService struct{ hubclient.ArtifactService }

func (noVersionService) CreateVersion(context.Context, string, *hubclient.CreateVersionRequest) (*hubclient.PendingVersionResponse, error) {
	return &hubclient.PendingVersionResponse{Artifact: hubclient.Artifact{ID: "5f1c2d3e-0000-4000-8000-000000000001"}}, nil
}

// TestPublishBundleReplyWithoutVersion: a create reply without a version
// is an error, not a crash.
func TestPublishBundleReplyWithoutVersion(t *testing.T) {
	root := writeTree(t, map[string]string{"index.md": "# hi"})
	var out, errOut bytes.Buffer
	err := publishBundle(context.Background(), noVersionService{}, &out, &errOut, "", root, bundlePublishOptions{})
	assert.ErrorContains(t, err, "has no version")
	assert.Empty(t, out.String())
}

// oneVersionService serves an artifact with one version.
type oneVersionService struct{ hubclient.ArtifactService }

func (oneVersionService) Get(context.Context, string) (*hubclient.ArtifactResponse, error) {
	return &hubclient.ArtifactResponse{Artifact: hubclient.Artifact{CurrentSeq: 1}}, nil
}

func (oneVersionService) ListVersions(context.Context, string) ([]hubclient.ArtifactVersion, error) {
	at := time.Date(2026, 10, 6, 18, 20, 0, 0, time.UTC)
	return []hubclient.ArtifactVersion{{Seq: 1, Kind: "publish", CreatedAt: at, CreatedByKind: "agent", FileCount: 1}}, nil
}

// TestArtifactVersionsShowsZone: the PUBLISHED column carries a zone.
func TestArtifactVersionsShowsZone(t *testing.T) {
	clitime.SetZone(time.UTC)
	t.Cleanup(func() { clitime.SetZone(nil) })
	var out bytes.Buffer
	require.NoError(t, listArtifactVersions(context.Background(), oneVersionService{}, &out, "5f1c2d3e-0000-4000-8000-000000000001"))
	assert.Contains(t, out.String(), "2026-10-06 18:20 UTC")
}
