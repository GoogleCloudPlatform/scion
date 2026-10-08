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
	"database/sql"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/artifacts"
	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
	"github.com/GoogleCloudPlatform/scion/pkg/storage"
	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// oneUserHost is oneAgentHost serving a user instead.
type oneUserHost struct{ oneAgentHost }

func (oneUserHost) Principal(context.Context) (string, string, string, bool) {
	return artifacts.PrincipalKindUser, "user-1", "", true
}

// shareHub runs the artifact service as host serves it and returns a
// client and the server's URL.
func shareHub(t *testing.T, host artifacts.Host) (hubclient.ArtifactService, string) {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+filepath.Join(t.TempDir(), "a.db")+"?_pragma=busy_timeout(5000)")
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	st := artifacts.NewStore(db, "sqlite")
	require.NoError(t, st.Init(context.Background()))
	blobs, err := storage.NewLocal(storage.Config{Provider: storage.ProviderLocal, Bucket: "b", LocalPath: t.TempDir()})
	require.NoError(t, err)
	svc := artifacts.NewService(host)
	svc.SetStore(st)
	svc.SetBlobStorage(blobs, "hub-1")
	svc.SetViewKey([]byte("0123456789abcdef0123456789abcdef"))
	srv := httptest.NewServer(svc)
	t.Cleanup(srv.Close)
	c, err := hubclient.New(srv.URL)
	require.NoError(t, err)
	return c.Artifacts(), srv.URL
}

func TestArtifactShareCreateListRevoke(t *testing.T) {
	svc, base := shareHub(t, oneUserHost{})
	ctx := context.Background()
	body := "# shared"
	pub, err := svc.Publish(ctx, &hubclient.PublishArtifactRequest{Name: "s.md", Scope: "project-1",
		Content: strings.NewReader(body), Size: int64(len(body))})
	require.NoError(t, err)
	ref := pub.Artifact.Ref

	var out bytes.Buffer
	require.NoError(t, shareArtifact(ctx, svc, &out, base+"/", ref, "2d", false, ""))
	lines := strings.Split(out.String(), "\n")
	link := lines[0]
	require.True(t, strings.HasPrefix(link, base+artifacts.RouteShared), out.String())
	assert.Contains(t, out.String(), "shown only once")
	assert.Contains(t, out.String(), "--revoke ")

	// The printed link works without credentials.
	noRedirect := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := noRedirect.Get(link)
	require.NoError(t, err)
	_ = resp.Body.Close()
	assert.Equal(t, http.StatusSeeOther, resp.StatusCode)

	links, err := svc.ListLinks(ctx, pub.Artifact.ID)
	require.NoError(t, err)
	require.Len(t, links, 1)
	assert.WithinDuration(t, time.Now().Add(48*time.Hour), links[0].ExpiresAt, time.Minute)

	out.Reset()
	require.NoError(t, shareArtifact(ctx, svc, &out, base, ref, "", true, ""))
	assert.Contains(t, out.String(), links[0].ID)
	assert.NotContains(t, out.String(), strings.TrimPrefix(link, base+artifacts.RouteShared))

	out.Reset()
	require.NoError(t, shareArtifact(ctx, svc, &out, base, ref, "", false, links[0].ID))
	resp, err = noRedirect.Get(link)
	require.NoError(t, err)
	_ = resp.Body.Close()
	assert.Equal(t, http.StatusNotFound, resp.StatusCode)

	out.Reset()
	require.NoError(t, shareArtifact(ctx, svc, &out, base, ref, "", true, ""))
	assert.Contains(t, out.String(), "No active share links")

	for _, bad := range [][3]string{{"0h", "", ""}, {"7w", "", ""}, {"x", "", ""}, {"1d", "list", ""}, {"", "list", "id"}} {
		err := shareArtifact(ctx, svc, &out, base, ref, bad[0], bad[1] != "", bad[2])
		assert.Error(t, err, "%v", bad)
	}
	assert.Error(t, shareArtifact(ctx, svc, &out, base, "not-a-ref", "", false, ""))
}

// TestArtifactShareRefusedForAgents: the hub refuses links to agents,
// whatever the CLI allows.
func TestArtifactShareRefusedForAgents(t *testing.T) {
	svc, base := shareHub(t, oneAgentHost{})
	ctx := context.Background()
	pub, err := svc.Publish(ctx, &hubclient.PublishArtifactRequest{Name: "a.md", Content: strings.NewReader("a"), Size: 1})
	require.NoError(t, err)
	var out bytes.Buffer
	err = shareArtifact(ctx, svc, &out, base, pub.Artifact.Ref, "", false, "")
	require.Error(t, err)
	assert.Empty(t, out.String())
}

func TestParseShareTTL(t *testing.T) {
	for in, want := range map[string]int{"": 0, "1h": 1, "24h": 24, "7d": 168, "30d": 720} {
		got, err := parseShareTTL(in)
		require.NoError(t, err, in)
		assert.Equal(t, want, got, in)
	}
	for _, in := range []string{"0d", "-1h", "h", "1.5h", "1m", "10000000d"} {
		_, err := parseShareTTL(in)
		assert.Error(t, err, in)
	}
}

// TestArtifactShareModes: share is available to users in human and
// assistant mode and removed in agent mode, while the other artifact verbs
// stay available to agents.
func TestArtifactShareModes(t *testing.T) {
	assert.False(t, agentAllowed["artifact.share"])
	assert.False(t, assistantDenied["artifact.share"])
	assert.False(t, assistantDenied["artifact"])
	assert.True(t, agentAllowed["artifact.get"])

	build := func() *cobra.Command {
		root := &cobra.Command{Use: "scion"}
		art := &cobra.Command{Use: "artifact"}
		for _, v := range []string{"share", "get", "publish", "versions"} {
			art.AddCommand(&cobra.Command{Use: v})
		}
		root.AddCommand(art)
		return root
	}
	for mode, wantShare := range map[string]bool{"human": true, "assistant": true, "agent": false} {
		t.Run(mode, func(t *testing.T) {
			t.Setenv("SCION_CLI_MODE", mode)
			root := build()
			applyModeRestrictions(root)
			names := collectCommandNames(root)
			assert.Equal(t, wantShare, slices.Contains(names, "artifact.share"), "artifact.share in %s mode", mode)
			assert.Contains(t, names, "artifact.get")
		})
	}
}
