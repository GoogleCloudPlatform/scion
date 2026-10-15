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
	"encoding/json"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/artifacts"
	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
	"github.com/GoogleCloudPlatform/scion/pkg/storage"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// realArtifactHubWithViews is realArtifactHub with artifact views
// configured, so the view route mints capabilities.
func realArtifactHubWithViews(t *testing.T) hubclient.ArtifactService {
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
	svc.SetViewKey([]byte("0123456789abcdef0123456789abcdef"))
	srv := httptest.NewServer(svc)
	t.Cleanup(srv.Close)
	c, err := hubclient.New(srv.URL)
	require.NoError(t, err)
	return c.Artifacts()
}

// publishSite publishes files as a bundle and returns its reference.
func publishSite(t *testing.T, svc hubclient.ArtifactService, files map[string]string) string {
	t.Helper()
	var out, errOut bytes.Buffer
	require.NoError(t, publishBundle(context.Background(), svc, &out, &errOut, "", writeTree(t, files), bundlePublishOptions{}))
	m := refLine.FindStringSubmatch(out.String())
	require.NotNil(t, m, out.String())
	return m[1]
}

// TestArtifactViewPrintsLink: view prints the absolute view link of the
// version and its expiry, and never needs a browser (ptone/scion#4572).
func TestArtifactViewPrintsLink(t *testing.T) {
	svc := realArtifactHubWithViews(t)
	ref := publishSite(t, svc, map[string]string{"index.html": "<p>hi</p>"})
	var out bytes.Buffer
	require.NoError(t, viewArtifact(context.Background(), svc, &out, "https://hub.example/", ref, viewOptions{}))
	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	require.GreaterOrEqual(t, len(lines), 2, out.String())
	assert.True(t, strings.HasPrefix(lines[0], "https://hub.example/api/v1/artifacts/view/"), lines[0])
	assert.True(t, strings.HasSuffix(lines[0], "/index.html"), lines[0])
	assert.Contains(t, lines[1], "View of "+ref+"@1, valid until ")
	assert.NotContains(t, out.String(), "Checked")
}

// TestArtifactViewCheckReportsBrokenLinks: --check reports every relative
// href and src that names no file of the version, with the file and
// attribute, and fails; links with a scheme, to another host and to a
// fragment are not checked, and a folder link resolves to its index.html.
func TestArtifactViewCheckReportsBrokenLinks(t *testing.T) {
	svc := realArtifactHubWithViews(t)
	ref := publishSite(t, svc, map[string]string{
		"index.html": `<link href="css/site.css" rel="stylesheet"><img src="img/missing.png">` +
			`<a href="docs/">docs</a><a href="https://example.com/x">ext</a><a href="#top">top</a>` +
			`<a href="mailto:a@example.com">mail</a><a href="//cdn.example.com/y.js">cdn</a>`,
		"css/site.css":    "body{}",
		"docs/index.html": `<a href="../index.html">home</a><script src="app.js"></script>`,
	})
	var out bytes.Buffer
	err := viewArtifact(context.Background(), svc, &out, "https://hub.example", ref, viewOptions{Check: true})
	require.ErrorIs(t, err, errBrokenLinks)
	got := out.String()
	assert.Contains(t, got, "Checked 2 HTML file(s): 2 relative link(s) name no file of the version:")
	assert.Contains(t, got, `  docs/index.html: src="app.js"`)
	assert.Contains(t, got, `  index.html: src="img/missing.png"`)
	for _, unchecked := range []string{"example.com", "#top", "mailto:", "css/site.css", "docs/\"", "../index.html"} {
		assert.NotContains(t, strings.SplitN(got, "name no file of the version:", 2)[1], unchecked)
	}
}

// TestArtifactViewCheckCleanBundle: a bundle whose relative links all
// resolve passes --check.
func TestArtifactViewCheckCleanBundle(t *testing.T) {
	svc := realArtifactHubWithViews(t)
	ref := publishSite(t, svc, map[string]string{
		"index.html": `<img src="img/a.png"><a href="page%20two.html?x=1#s">two</a>`,
		"img/a.png":  "png", "page two.html": "<p>2</p>",
	})
	var out bytes.Buffer
	require.NoError(t, viewArtifact(context.Background(), svc, &out, "", ref, viewOptions{Check: true}))
	assert.Contains(t, out.String(), "Checked 2 HTML file(s): every relative link names a file of the version.")
	assert.True(t, strings.HasPrefix(out.String(), "/api/v1/artifacts/view/"), "without an endpoint the hub path is printed")
}

// TestArtifactViewJSON: the JSON output carries the link, expiry and the
// check's result.
func TestArtifactViewJSON(t *testing.T) {
	svc := realArtifactHubWithViews(t)
	ref := publishSite(t, svc, map[string]string{"index.html": `<img src="gone.png">`})
	var out bytes.Buffer
	err := viewArtifact(context.Background(), svc, &out, "https://hub.example", ref, viewOptions{Check: true, JSON: true})
	require.ErrorIs(t, err, errBrokenLinks)
	var res viewResult
	require.NoError(t, json.Unmarshal(out.Bytes(), &res))
	assert.Equal(t, ref+"@1", res.Ref)
	assert.True(t, strings.HasPrefix(res.URL, "https://hub.example/api/v1/artifacts/view/"))
	assert.False(t, res.ExpiresAt.IsZero())
	assert.True(t, res.Checked)
	assert.Equal(t, 1, res.HTMLFiles)
	assert.Equal(t, []brokenLink{{File: "index.html", Attribute: "src", Target: "gone.png"}}, res.Broken)
}

// TestArtifactViewNonHTMLEntry: a version whose entry is not HTML has no
// view; the error says so.
func TestArtifactViewNonHTMLEntry(t *testing.T) {
	svc := realArtifactHubWithViews(t)
	ref := publishSite(t, svc, map[string]string{"README.md": "# hi"})
	var out bytes.Buffer
	err := viewArtifact(context.Background(), svc, &out, "", ref, viewOptions{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "Only a version whose entry file is HTML has a view.")
	assert.Empty(t, out.String())
}

// TestArtifactViewAgentModeHidesLink: with the link hidden (agent mode
// without --show-link), view prints the version and the link's expiry and
// no part of the link.
func TestArtifactViewAgentModeHidesLink(t *testing.T) {
	svc := realArtifactHubWithViews(t)
	ref := publishSite(t, svc, map[string]string{"index.html": "<p>hi</p>"})
	var out bytes.Buffer
	require.NoError(t, viewArtifact(context.Background(), svc, &out, "https://hub.example/", ref, viewOptions{HideLink: true}))
	got := out.String()
	assert.Contains(t, got, "View of "+ref+"@1, valid until ")
	assert.Contains(t, got, "run with --show-link to print it")
	assert.NotContains(t, got, "/api/v1/artifacts/view/")
	assert.NotContains(t, got, "https://hub.example")
}

// TestArtifactViewAgentModeJSONOmitsLink: the JSON output in agent mode
// without --show-link has no url, says the link is hidden, and keeps the
// expiry.
func TestArtifactViewAgentModeJSONOmitsLink(t *testing.T) {
	svc := realArtifactHubWithViews(t)
	ref := publishSite(t, svc, map[string]string{"index.html": "<p>hi</p>"})
	var out bytes.Buffer
	require.NoError(t, viewArtifact(context.Background(), svc, &out, "https://hub.example", ref, viewOptions{JSON: true, HideLink: true}))
	var raw map[string]any
	require.NoError(t, json.Unmarshal(out.Bytes(), &raw))
	_, hasURL := raw["url"]
	assert.False(t, hasURL, out.String())
	assert.Equal(t, true, raw["linkHidden"], out.String())
	assert.NotEmpty(t, raw["expiresAt"], out.String())
	assert.NotContains(t, out.String(), "/api/v1/artifacts/view/")
}

// TestArtifactViewCommandModes runs the view command itself against a
// test hub: in agent mode it leaves the link out unless --show-link is
// given, and in human mode it prints the link.
func TestArtifactViewCommandModes(t *testing.T) {
	svc := realArtifactHubWithViews(t)
	ref := publishSite(t, svc, map[string]string{"index.html": "<p>hi</p>"})
	prevClient, prevFormat, prevCtx := artifactViewClient, outputFormat, artifactViewCmd.Context()
	artifactViewClient = func() (string, hubclient.ArtifactService, error) { return "https://hub.example", svc, nil }
	outputFormat = ""
	t.Cleanup(func() {
		artifactViewClient, outputFormat = prevClient, prevFormat
		_ = artifactViewCmd.Flags().Set("show-link", "false")
		artifactViewCmd.SetOut(nil)
		artifactViewCmd.SetContext(prevCtx)
	})
	for _, tc := range []struct {
		mode     string
		showLink string
		wantLink bool
	}{
		{"agent", "false", false},
		{"agent", "true", true},
		{"human", "false", true},
	} {
		t.Setenv("SCION_CLI_MODE", tc.mode)
		require.NoError(t, artifactViewCmd.Flags().Set("show-link", tc.showLink))
		var out bytes.Buffer
		artifactViewCmd.SetOut(&out)
		artifactViewCmd.SetContext(context.Background())
		require.NoError(t, artifactViewCmd.RunE(artifactViewCmd, []string{ref}))
		got := out.String()
		assert.Contains(t, got, "View of "+ref+"@1, valid until ", "mode=%s show-link=%s", tc.mode, tc.showLink)
		assert.Equal(t, tc.wantLink, strings.Contains(got, "https://hub.example/api/v1/artifacts/view/"), "link printed: mode=%s show-link=%s\n%s", tc.mode, tc.showLink, got)
	}
}
