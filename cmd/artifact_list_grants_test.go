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
	"strings"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const testArtifactRef = "scion://artifact/5f1c2d3e-6b1a-4c55-9f3e-0d6e7a1b2c3d"
const testArtifactUUID = "5f1c2d3e-6b1a-4c55-9f3e-0d6e7a1b2c3d"

// listPagesService serves List from fixed pages and records the options
// of each call.
type listPagesService struct {
	hubclient.ArtifactService
	pages []hubclient.ArtifactListResponse
	calls []hubclient.ListArtifactsOptions
}

func (s *listPagesService) List(_ context.Context, opts *hubclient.ListArtifactsOptions) (*hubclient.ArtifactListResponse, error) {
	s.calls = append(s.calls, *opts)
	p := s.pages[len(s.calls)-1]
	return &p, nil
}

func listItem(id, title, access string, seq int) hubclient.ArtifactListItem {
	return hubclient.ArtifactListItem{
		Artifact: hubclient.Artifact{ID: id, Title: title, CurrentSeq: seq, UpdatedAt: time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)},
		Access:   access,
	}
}

// TestListArtifactsPassesOptions: list hands every flag to the hub's list
// call, prints one page, and says that more follow (ptone/scion#4579).
func TestListArtifactsPassesOptions(t *testing.T) {
	svc := &listPagesService{pages: []hubclient.ArtifactListResponse{
		{Artifacts: []hubclient.ArtifactListItem{listItem("a1", "Report", "shared", 2)}, NextCursor: "c1"},
	}}
	var out bytes.Buffer
	opts := hubclient.ListArtifactsOptions{Query: "rep", ReviewPending: true, OwnedOnly: true, Limit: 5, Scope: "proj-1", Shared: true}
	require.NoError(t, listArtifacts(context.Background(), svc, &out, listArtifactsParams{Options: opts}))
	require.Len(t, svc.calls, 1)
	assert.Equal(t, opts, svc.calls[0])
	got := out.String()
	assert.Contains(t, got, "REF")
	assert.Contains(t, got, "scion://artifact/a1")
	assert.Contains(t, got, "Report")
	assert.Contains(t, got, "v2")
	assert.Contains(t, got, "shared")
	assert.Contains(t, got, "More artifacts follow; use --all to list them, or --cursor c1 for the next page.")
}

// TestListArtifactsAllFollowsCursor: --all follows nextCursor to the last
// page and prints every row once.
func TestListArtifactsAllFollowsCursor(t *testing.T) {
	svc := &listPagesService{pages: []hubclient.ArtifactListResponse{
		{Artifacts: []hubclient.ArtifactListItem{listItem("a1", "One", "owned", 1)}, NextCursor: "c1"},
		{Artifacts: []hubclient.ArtifactListItem{listItem("a2", "Two", "project", 3)}},
	}}
	var out bytes.Buffer
	require.NoError(t, listArtifacts(context.Background(), svc, &out, listArtifactsParams{All: true}))
	require.Len(t, svc.calls, 2)
	assert.Equal(t, "", svc.calls[0].Cursor)
	assert.Equal(t, "c1", svc.calls[1].Cursor)
	assert.Contains(t, out.String(), "scion://artifact/a1")
	assert.Contains(t, out.String(), "scion://artifact/a2")
	assert.NotContains(t, out.String(), "More artifacts follow")
}

// TestListArtifactsJSONAndEmpty: the JSON output carries the rows and the
// cursor; an empty list says so.
func TestListArtifactsJSONAndEmpty(t *testing.T) {
	svc := &listPagesService{pages: []hubclient.ArtifactListResponse{
		{Artifacts: []hubclient.ArtifactListItem{listItem("a1", "One", "owned", 1)}, NextCursor: "c1"},
	}}
	var out bytes.Buffer
	require.NoError(t, listArtifacts(context.Background(), svc, &out, listArtifactsParams{JSON: true}))
	var body struct {
		Artifacts  []hubclient.ArtifactListItem `json:"artifacts"`
		NextCursor string                       `json:"nextCursor"`
	}
	require.NoError(t, json.Unmarshal(out.Bytes(), &body))
	require.Len(t, body.Artifacts, 1)
	assert.Equal(t, "a1", body.Artifacts[0].ID)
	assert.Equal(t, "c1", body.NextCursor)

	empty := &listPagesService{pages: []hubclient.ArtifactListResponse{{}}}
	out.Reset()
	require.NoError(t, listArtifacts(context.Background(), empty, &out, listArtifactsParams{}))
	assert.Equal(t, "No artifacts.\n", out.String())
	assert.Error(t, listArtifacts(context.Background(), empty, &out, listArtifactsParams{Options: hubclient.ListArtifactsOptions{Limit: -1}}))
}

// grantsService records the grants calls.
type grantsService struct {
	hubclient.ArtifactService
	put     *hubclient.ArtifactGrantRequest
	removed string
	listed  bool
}

func (s *grantsService) ListGrants(_ context.Context, id string) (*hubclient.ArtifactGrantList, error) {
	s.listed = true
	return &hubclient.ArtifactGrantList{Grants: []hubclient.ArtifactGrant{
		{ID: "g1", SubjectKind: "scope", SubjectRef: "proj-1", Permission: "read", Home: true},
		{ID: "g2", SubjectKind: "principal", SubjectRef: "user:u1", Permission: "write"},
	}}, nil
}

func (s *grantsService) PutGrant(_ context.Context, id string, req *hubclient.ArtifactGrantRequest) (*hubclient.ArtifactGrant, error) {
	s.put = req
	return &hubclient.ArtifactGrant{ID: "g9", SubjectKind: req.SubjectKind, SubjectRef: req.SubjectRef, Permission: req.Permission}, nil
}

func (s *grantsService) DeleteGrant(_ context.Context, id, grantID string) error {
	s.removed = grantID
	return nil
}

// TestParseGrantSubject: user, agent and project subjects map to the hub's
// subjectKind and subjectRef; anything else is refused.
func TestParseGrantSubject(t *testing.T) {
	for in, want := range map[string][2]string{
		"user:u1":     {"principal", "user:u1"},
		"agent:a1":    {"principal", "agent:a1"},
		"project:p1":  {"scope", "p1"},
		" Project:p2": {"scope", "p2"},
	} {
		kind, ref, err := parseGrantSubject(in)
		require.NoError(t, err, in)
		assert.Equal(t, want, [2]string{kind, ref}, in)
	}
	for _, bad := range []string{"", "u1", "user:", "group:g1", ":x"} {
		_, _, err := parseGrantSubject(bad)
		assert.Error(t, err, bad)
	}
}

// TestArtifactGrantsList: without flags, grants lists the artifact's
// grants, marking the home project's (ptone/scion#4579).
func TestArtifactGrantsList(t *testing.T) {
	svc := &grantsService{}
	var out bytes.Buffer
	require.NoError(t, artifactGrants(context.Background(), svc, &out, testArtifactRef, grantsOptions{}))
	assert.True(t, svc.listed)
	got := out.String()
	assert.Contains(t, got, "project:proj-1 (home project)")
	assert.Contains(t, got, "user:u1")
	assert.Contains(t, got, "write")
}

// TestArtifactGrantsAddRemove: --add sends the parsed subject and the
// permission; --remove deletes by grant id; bad flag combinations and
// permissions are refused before any call.
func TestArtifactGrantsAddRemove(t *testing.T) {
	svc := &grantsService{}
	var out bytes.Buffer
	require.NoError(t, artifactGrants(context.Background(), svc, &out, testArtifactRef, grantsOptions{Add: "user:u7", Permission: "Review"}))
	require.NotNil(t, svc.put)
	assert.Equal(t, hubclient.ArtifactGrantRequest{SubjectKind: "principal", SubjectRef: "user:u7", Permission: "review"}, *svc.put)
	assert.Contains(t, out.String(), "Granted user:u7 review on scion://artifact/"+testArtifactUUID+" (grant g9)")

	out.Reset()
	require.NoError(t, artifactGrants(context.Background(), svc, &out, testArtifactRef, grantsOptions{Remove: "g2"}))
	assert.Equal(t, "g2", svc.removed)
	assert.Contains(t, out.String(), "Removed grant g2")

	fresh := &grantsService{}
	for _, opts := range []grantsOptions{
		{Add: "user:u1", Permission: "owner"},
		{Add: "user:u1"},
		{Add: "user:u1", Permission: "read", Remove: "g1"},
		{Permission: "read"},
		{Add: "team:t1", Permission: "read"},
	} {
		err := artifactGrants(context.Background(), fresh, &out, testArtifactRef, opts)
		assert.Error(t, err, "%+v", opts)
	}
	assert.Nil(t, fresh.put)
	assert.Empty(t, fresh.removed)
	assert.False(t, fresh.listed)

	// --remove is checked after trimming: a blank id is refused.
	err := artifactGrants(context.Background(), fresh, &out, testArtifactRef, grantsOptions{Remove: "   "})
	assert.ErrorContains(t, err, "--remove needs a grant id")
	assert.Empty(t, fresh.removed)
}

// TestListArtifactsStartsAtCursor: --cursor starts the list at that page.
func TestListArtifactsStartsAtCursor(t *testing.T) {
	svc := &listPagesService{pages: []hubclient.ArtifactListResponse{
		{Artifacts: []hubclient.ArtifactListItem{listItem("a2", "Two", "owned", 1)}},
	}}
	var out bytes.Buffer
	require.NoError(t, listArtifacts(context.Background(), svc, &out, listArtifactsParams{Options: hubclient.ListArtifactsOptions{Cursor: "c1"}}))
	require.Len(t, svc.calls, 1)
	assert.Equal(t, "c1", svc.calls[0].Cursor)
}

// TestListArtifactsSafeOutput: titles and other text from the hub cannot
// carry terminal escape sequences or break the table: control characters
// are replaced before printing.
func TestListArtifactsSafeOutput(t *testing.T) {
	evil := listItem("a1", "Report\x1b]52;c;aGk=\x07\nb1  Fake  v9  owned", "shared\x1b[2K", 1)
	svc := &listPagesService{pages: []hubclient.ArtifactListResponse{{Artifacts: []hubclient.ArtifactListItem{evil}}}}
	var out bytes.Buffer
	require.NoError(t, listArtifacts(context.Background(), svc, &out, listArtifactsParams{}))
	got := out.String()
	assert.NotContains(t, got, "\x1b")
	assert.NotContains(t, got, "\x07")
	assert.Equal(t, 2, strings.Count(got, "\n"), "one header and one row: %q", got)
	assert.Contains(t, got, "Report\uFFFD]52;c;aGk=\uFFFD\uFFFDb1")
}

// TestArtifactGrantsSafeOutput: grant ids and subjects from the hub are
// printed without control characters.
func TestArtifactGrantsSafeOutput(t *testing.T) {
	svc := &evilGrantsService{}
	var out bytes.Buffer
	require.NoError(t, artifactGrants(context.Background(), svc, &out, testArtifactRef, grantsOptions{}))
	assert.NotContains(t, out.String(), "\x1b")
	assert.Contains(t, out.String(), "user:u1\uFFFD[31m")
}

type evilGrantsService struct{ hubclient.ArtifactService }

func (evilGrantsService) ListGrants(context.Context, string) (*hubclient.ArtifactGrantList, error) {
	return &hubclient.ArtifactGrantList{Grants: []hubclient.ArtifactGrant{
		{ID: "g1\x1b]8;;https://example.com\x07", SubjectKind: "principal", SubjectRef: "user:u1\x1b[31m", Permission: "read"},
	}}, nil
}
