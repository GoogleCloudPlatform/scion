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

package hub

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/experiments"
	"github.com/GoogleCloudPlatform/scion/pkg/storage"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/telemetrycontract"
)

func createTrajectoryTestUserWithRole(t *testing.T, s store.Store, email, roleName, scopeType, scopeID string) *store.User {
	t.Helper()
	ctx := context.Background()
	u := &store.User{
		ID:          api.NewUUID(),
		Email:       email,
		DisplayName: email,
		Role:        store.UserRoleMember,
		Status:      "active",
	}
	require.NoError(t, s.CreateUser(ctx, u))

	role, err := s.GetRoleDefinitionByName(ctx, roleName, scopeType)
	require.NoError(t, err)

	_, err = s.CreateRoleBinding(ctx, &store.RoleBinding{
		ID:               api.NewUUID(),
		RoleDefinitionID: role.ID,
		PrincipalType:    "user",
		PrincipalID:      u.ID,
		ScopeType:        scopeType,
		ScopeID:          scopeID,
	})
	require.NoError(t, err)
	return u
}

func trajectoryUserSessionToken(t *testing.T, srv *Server, u *store.User) string {
	t.Helper()
	tok, _, _, err := srv.userTokenService.GenerateTokenPair(u.ID, u.Email, u.DisplayName, u.Role, ClientTypeWeb)
	require.NoError(t, err)
	return tok
}

func doTrajectoryUserGet(t *testing.T, srv *Server, path, bearerToken string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	if bearerToken != "" {
		req.Header.Set("Authorization", "Bearer "+bearerToken)
	}
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	return rec
}

func commitSampleTrajectorySegment(
	t *testing.T,
	srv *Server,
	agentID, agentTok, relPath string,
	gen int,
	byteStart int64,
	raw []byte,
) (string, []byte, string) {
	t.Helper()
	gz, rawSHA, compSHA := gzipAndHash(t, raw)
	prepReq := telemetrycontract.TrajectorySegmentPrepareRequest{
		Harness:            "claude",
		NativeRelativePath: relPath,
		ArtifactKind:       telemetrycontract.TrajectoryArtifactKindJSONLAppend,
		Generation:         gen,
		ByteStart:          byteStart,
		ByteEnd:            byteStart + int64(len(raw)),
		RawSHA256:          rawSHA,
		CompressedSize:     int64(len(gz)),
		CompressedSHA256:   compSHA,
		ContentEncoding:    telemetrycontract.TrajectoryContentEncodingGzip,
		SessionID:          "sess-read-test",
	}
	rec := doTrajectoryAgentJSON(t, srv, http.MethodPost,
		telemetrycontract.TrajectorySegmentsPrepareAgentPath(agentID), agentTok, prepReq)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	var prepResp telemetrycontract.TrajectorySegmentPrepareResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &prepResp))
	require.Equal(t, telemetrycontract.TrajectorySegmentStateUploadRequired, prepResp.State)

	u, err := url.Parse(prepResp.Upload.URL)
	require.NoError(t, err)
	putReq := httptest.NewRequest(http.MethodPut, u.RequestURI(), bytes.NewReader(gz))
	putReq.Header.Set("X-Scion-Agent-Token", agentTok)
	for k, v := range prepResp.Upload.Headers {
		putReq.Header.Set(k, v)
	}
	putRec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(putRec, putReq)
	require.Equal(t, http.StatusOK, putRec.Code, putRec.Body.String())

	commitRec := doTrajectoryAgentJSON(t, srv, http.MethodPost,
		telemetrycontract.TrajectorySegmentsCommitAgentPath(agentID), agentTok,
		telemetrycontract.TrajectorySegmentCommitRequest{SegmentID: prepResp.SegmentID})
	require.Equal(t, http.StatusOK, commitRec.Code, commitRec.Body.String())

	return prepResp.SegmentID, gz, prepResp.Upload.URL
}

func listTrajectoryReadAudits(t *testing.T, s store.Store, segmentID string) []*store.MutationAuditRecord {
	t.Helper()
	recs, _, err := s.ListMutationAudits(context.Background(), store.MutationAuditFilter{
		MutationType: mutationTypeTrajectorySegmentRead,
		TargetType:   trajectoryReadAuditTargetType,
		TargetID:     segmentID,
		Limit:        50,
	})
	require.NoError(t, err)
	return recs
}

func TestTrajectoryRead_AuthzMatrixAndReadAudit(t *testing.T) {
	srv, s, _, projA, agentA, agentTok := setupTrajectorySegmentFixture(t)
	ctx := context.Background()

	// Create a second project B for cross-project refusal testing.
	projB := &store.Project{
		ID:   api.NewUUID(),
		Name: "traj-proj-b",
		Slug: "traj-proj-b",
	}
	require.NoError(t, s.CreateProject(ctx, projB))

	relPath := ".claude/projects/-workspace/sess-authz.jsonl"
	raw := []byte("{\"type\":\"user\",\"text\":\"secret prompt\"}\n")
	segID, _, _ := commitSampleTrajectorySegment(t, srv, agentA.ID, agentTok, relPath, 0, 0, raw)

	// Prepare (but do NOT commit) a second segment to verify list never returns pending segments.
	rawPending := []byte("{\"type\":\"assistant\",\"text\":\"pending\"}\n")
	gzPending, rawPendingSHA, compPendingSHA := gzipAndHash(t, rawPending)
	prepPending := doTrajectoryAgentJSON(t, srv, http.MethodPost,
		telemetrycontract.TrajectorySegmentsPrepareAgentPath(agentA.ID), agentTok,
		telemetrycontract.TrajectorySegmentPrepareRequest{
			Harness:            "claude",
			NativeRelativePath: relPath,
			ArtifactKind:       telemetrycontract.TrajectoryArtifactKindJSONLAppend,
			Generation:         0,
			ByteStart:          int64(len(raw)),
			ByteEnd:            int64(len(raw) + len(rawPending)),
			RawSHA256:          rawPendingSHA,
			CompressedSize:     int64(len(gzPending)),
			CompressedSHA256:   compPendingSHA,
			ContentEncoding:    telemetrycontract.TrajectoryContentEncodingGzip,
		})
	require.Equal(t, http.StatusOK, prepPending.Code)

	memberA := createTrajectoryTestUserWithRole(t, s, "member-a@example.com", store.ProjectRoleMember, store.RoleScopeProject, projA.ID)
	viewerA := createTrajectoryTestUserWithRole(t, s, "viewer-a@example.com", store.ProjectRoleViewer, store.RoleScopeProject, projA.ID)
	adminA := createTrajectoryTestUserWithRole(t, s, "admin-a@example.com", store.ProjectRoleAdmin, store.RoleScopeProject, projA.ID)
	ownerA := createTrajectoryTestUserWithRole(t, s, "owner-a@example.com", store.ProjectRoleOwner, store.RoleScopeProject, projA.ID)
	adminB := createTrajectoryTestUserWithRole(t, s, "admin-b@example.com", store.ProjectRoleAdmin, store.RoleScopeProject, projB.ID)
	hubAdmin := createTrajectoryTestUserWithRole(t, s, "hub-admin@example.com", store.SystemRoleHubAdmin, store.RoleScopeSystem, "")

	listPath := telemetrycontract.TrajectorySegmentsListAgentPath(agentA.ID)
	dlPath := telemetrycontract.TrajectorySegmentDownloadAgentPath(agentA.ID, segID)

	// 1. Unauthenticated -> 401.
	assert.Equal(t, http.StatusUnauthorized, doTrajectoryUserGet(t, srv, listPath, "").Code)
	assert.Equal(t, http.StatusUnauthorized, doTrajectoryUserGet(t, srv, dlPath, "").Code)

	// 2. Agent token -> 403 (agents must not read their own audit trail).
	agentListReq := httptest.NewRequest(http.MethodGet, listPath, nil)
	agentListReq.Header.Set("X-Scion-Agent-Token", agentTok)
	agentListRec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(agentListRec, agentListReq)
	assert.Equal(t, http.StatusForbidden, agentListRec.Code)

	agentDlReq := httptest.NewRequest(http.MethodGet, dlPath, nil)
	agentDlReq.Header.Set("X-Scion-Agent-Token", agentTok)
	agentDlRec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(agentDlRec, agentDlReq)
	assert.Equal(t, http.StatusForbidden, agentDlRec.Code)

	// 3. agent.read-only users (project-member and project-viewer) -> 403.
	for _, u := range []*store.User{memberA, viewerA} {
		tok := trajectoryUserSessionToken(t, srv, u)
		assert.Equal(t, http.StatusForbidden, doTrajectoryUserGet(t, srv, listPath, tok).Code, "list for %s", u.Email)
		assert.Equal(t, http.StatusForbidden, doTrajectoryUserGet(t, srv, dlPath, tok).Code, "download for %s", u.Email)
	}

	// 4. Cross-project: project-admin of project B -> 403 for agent in project A.
	adminBTok := trajectoryUserSessionToken(t, srv, adminB)
	assert.Equal(t, http.StatusForbidden, doTrajectoryUserGet(t, srv, listPath, adminBTok).Code)
	assert.Equal(t, http.StatusForbidden, doTrajectoryUserGet(t, srv, dlPath, adminBTok).Code)

	// Zero audit rows written for any denied call.
	assert.Empty(t, listTrajectoryReadAudits(t, s, segID))

	// 5. project-admin of project A -> 200 on list (committed only, no audit row), 200 on download + 1 audit row.
	adminATok := trajectoryUserSessionToken(t, srv, adminA)
	listRec := doTrajectoryUserGet(t, srv, listPath, adminATok)
	require.Equal(t, http.StatusOK, listRec.Code, listRec.Body.String())
	var listResp telemetrycontract.TrajectorySegmentsListResponse
	require.NoError(t, json.Unmarshal(listRec.Body.Bytes(), &listResp))
	assert.Equal(t, agentA.ID, listResp.AgentID)
	assert.Equal(t, projA.ID, listResp.ProjectID)
	require.Len(t, listResp.Segments, 1, "pending segment must not be listed")
	assert.Equal(t, segID, listResp.Segments[0].SegmentID)
	assert.Equal(t, int64(len(raw)), listResp.Segments[0].RawSize)
	assert.NotEmpty(t, listResp.Segments[0].CommittedAt)
	// Listing does not write a read-audit row.
	assert.Empty(t, listTrajectoryReadAudits(t, s, segID))

	dlRec := doTrajectoryUserGet(t, srv, dlPath, adminATok)
	require.Equal(t, http.StatusOK, dlRec.Code, dlRec.Body.String())
	var dlResp telemetrycontract.TrajectorySegmentDownloadResponse
	require.NoError(t, json.Unmarshal(dlRec.Body.Bytes(), &dlResp))
	assert.Equal(t, segID, dlResp.SegmentID)
	assert.Equal(t, http.MethodGet, dlResp.Method)
	assert.NotEmpty(t, dlResp.URL)

	audits := listTrajectoryReadAudits(t, s, segID)
	require.Len(t, audits, 1, "download must write exactly one read-audit row")
	assert.Equal(t, adminA.ID, audits[0].ActorPrincipalID)
	assert.Equal(t, string(PrincipalKindUser), audits[0].ActorPrincipalKind)
	var summary trajectoryReadAuditSummary
	require.NoError(t, json.Unmarshal([]byte(audits[0].AfterSummary), &summary))
	assert.Equal(t, projA.ID, summary.ProjectID)
	assert.Equal(t, agentA.ID, summary.AgentID)
	assert.Equal(t, agentA.RunID, summary.RunID)
	assert.Equal(t, segID, summary.SegmentID)
	assert.Equal(t, dlResp.ObjectPath, summary.ObjectPath)

	// 6. project-owner and hub-admin -> 200 and each writes one audit row.
	ownerATok := trajectoryUserSessionToken(t, srv, ownerA)
	require.Equal(t, http.StatusOK, doTrajectoryUserGet(t, srv, dlPath, ownerATok).Code)
	hubAdminTok := trajectoryUserSessionToken(t, srv, hubAdmin)
	require.Equal(t, http.StatusOK, doTrajectoryUserGet(t, srv, dlPath, hubAdminTok).Code)
	assert.Len(t, listTrajectoryReadAudits(t, s, segID), 3)

	// 7. Scoped UATs are refused (session-only in P5; project.trajectory.read has no UATScope).
	uatAdmin, _, err := srv.uatService.CreateTokenWithParams(rs4MintContext(adminA.ID), CreateTokenParams{
		UserID:   adminA.ID,
		Name:     "traj-uat-admin-session-only",
		Boundary: projectBoundary(projA.ID),
		Scopes:   []string{"agent:read", "project:read"},
	})
	require.NoError(t, err)
	assert.Equal(t, http.StatusForbidden, doTrajectoryUserGet(t, srv, listPath, uatAdmin).Code)
	assert.Equal(t, http.StatusForbidden, doTrajectoryUserGet(t, srv, dlPath, uatAdmin).Code)
	assert.Len(t, listTrajectoryReadAudits(t, s, segID), 3)
}

func TestTrajectoryRead_ExperimentOffReturns404(t *testing.T) {
	srv, s, _, projA, agentA, agentTok := setupTrajectorySegmentFixture(t)
	segID, _, _ := commitSampleTrajectorySegment(t, srv, agentA.ID, agentTok,
		".claude/projects/-workspace/sess-exp.jsonl", 0, 0, []byte("{\"ok\":true}\n"))

	adminA := createTrajectoryTestUserWithRole(t, s, "exp-admin@example.com", store.ProjectRoleAdmin, store.RoleScopeProject, projA.ID)
	adminTok := trajectoryUserSessionToken(t, srv, adminA)

	// Obtain a valid signed content URL while experiment is on.
	dlRec := doTrajectoryUserGet(t, srv, telemetrycontract.TrajectorySegmentDownloadAgentPath(agentA.ID, segID), adminTok)
	require.Equal(t, http.StatusOK, dlRec.Code)
	var dlResp telemetrycontract.TrajectorySegmentDownloadResponse
	require.NoError(t, json.Unmarshal(dlRec.Body.Bytes(), &dlResp))

	// Turn tokenomics.trajectory_capture OFF.
	cur := srv.experimentRegistry()
	var disabled []experiments.Experiment
	for _, e := range cur.All() {
		if e.Name == experiments.TokenomicsTrajectoryCapture {
			e.Default = false
		}
		disabled = append(disabled, e)
	}
	reg, err := experiments.NewRegistry(disabled, cur.Retired())
	require.NoError(t, err)
	srv.setExperimentRegistry(reg)

	assert.Equal(t, http.StatusNotFound, doTrajectoryUserGet(t, srv, telemetrycontract.TrajectorySegmentsListAgentPath(agentA.ID), adminTok).Code)
	assert.Equal(t, http.StatusNotFound, doTrajectoryUserGet(t, srv, telemetrycontract.TrajectorySegmentDownloadAgentPath(agentA.ID, segID), adminTok).Code)

	u, err := url.Parse(dlResp.URL)
	require.NoError(t, err)
	contentRec := doTrajectoryUserGet(t, srv, u.RequestURI(), "")
	assert.Equal(t, http.StatusNotFound, contentRec.Code)
}

type trajectoryAuditFailStore struct {
	store.Store
	fault *storeFaultSwitch
}

func (f *trajectoryAuditFailStore) CreateMutationAudit(ctx context.Context, r *store.MutationAuditRecord) error {
	if f.fault.Active() && r.MutationType == mutationTypeTrajectorySegmentRead {
		return errors.New("injected audit DB outage")
	}
	return f.Store.CreateMutationAudit(ctx, r)
}

func TestTrajectoryRead_AuditFailureBlocksDownload(t *testing.T) {
	srv, s, loc, projA, agentA, agentTok := setupTrajectorySegmentFixture(t)
	_, fault := installStoreFault(t, srv, func(inner store.Store, sw *storeFaultSwitch) *trajectoryAuditFailStore {
		return &trajectoryAuditFailStore{Store: inner, fault: sw}
	})

	segID, _, _ := commitSampleTrajectorySegment(t, srv, agentA.ID, agentTok,
		".claude/projects/-workspace/sess-audit-fail.jsonl", 0, 0, []byte("{\"audit\":\"required\"}\n"))

	// Swap in the GCS storage double to prove GenerateSignedURL is never even
	// invoked when the mandatory read-audit write fails (strict audit-before-sign).
	fakeGCS := &fakeGCSSignedGetStorage{Storage: loc}
	srv.SetStorage(fakeGCS)

	adminA := createTrajectoryTestUserWithRole(t, s, "audit-admin@example.com", store.ProjectRoleAdmin, store.RoleScopeProject, projA.ID)
	adminTok := trajectoryUserSessionToken(t, srv, adminA)

	fault.Arm()

	dlRec := doTrajectoryUserGet(t, srv, telemetrycontract.TrajectorySegmentDownloadAgentPath(agentA.ID, segID), adminTok)
	assert.Equal(t, http.StatusInternalServerError, dlRec.Code)
	assert.NotContains(t, dlRec.Body.String(), "sig=", "must not issue a signed URL when audit write fails")
	assert.Zero(t, fakeGCS.signCalls, "GenerateSignedURL must not be called when audit write fails")
}

func TestTrajectoryRead_LocalHMACDownloadEndToEnd(t *testing.T) {
	srv, s, _, projA, agentA, agentTok := setupTrajectorySegmentFixture(t)

	now := time.Date(2026, 11, 27, 18, 0, 0, 0, time.UTC)
	srv.trajectoryClock = func() time.Time { return now }
	srv.trajectoryDownloadURLExpiry = 5 * time.Minute

	raw := []byte("{\"type\":\"user\",\"content\":\"hello verbatim\"}\n{\"type\":\"assistant\",\"content\":\"hi\"}\n")
	segID, expectedGZ, _ := commitSampleTrajectorySegment(t, srv, agentA.ID, agentTok,
		".claude/projects/-workspace/sess-e2e.jsonl", 0, 0, raw)

	adminA := createTrajectoryTestUserWithRole(t, s, "e2e-admin@example.com", store.ProjectRoleAdmin, store.RoleScopeProject, projA.ID)
	adminTok := trajectoryUserSessionToken(t, srv, adminA)

	// Request signed download URL including manifest.
	dlPath := telemetrycontract.TrajectorySegmentDownloadAgentPath(agentA.ID, segID) + "?include_manifest=true"
	dlRec := doTrajectoryUserGet(t, srv, dlPath, adminTok)
	require.Equal(t, http.StatusOK, dlRec.Code, dlRec.Body.String())

	var dlResp telemetrycontract.TrajectorySegmentDownloadResponse
	require.NoError(t, json.Unmarshal(dlRec.Body.Bytes(), &dlResp))
	require.NotEmpty(t, dlResp.URL)
	require.NotNil(t, dlResp.Manifest)
	require.NotEmpty(t, dlResp.Manifest.URL)

	// 1. Download segment via signed URL WITHOUT any Authorization header.
	segURL, err := url.Parse(dlResp.URL)
	require.NoError(t, err)
	contentRec := doTrajectoryUserGet(t, srv, segURL.RequestURI(), "")
	require.Equal(t, http.StatusOK, contentRec.Code, contentRec.Body.String())
	assert.Equal(t, trajectoryContentTypeGzip, contentRec.Header().Get("Content-Type"))
	assert.Equal(t, "no-store", contentRec.Header().Get("Cache-Control"))
	assert.Equal(t, expectedGZ, contentRec.Body.Bytes())

	gr, err := gzip.NewReader(bytes.NewReader(contentRec.Body.Bytes()))
	require.NoError(t, err)
	decompressed, err := io.ReadAll(gr)
	require.NoError(t, err)
	require.NoError(t, gr.Close())
	assert.Equal(t, raw, decompressed)

	// 2. Download manifest via signed URL WITHOUT any Authorization header.
	manURL, err := url.Parse(dlResp.Manifest.URL)
	require.NoError(t, err)
	manRec := doTrajectoryUserGet(t, srv, manURL.RequestURI(), "")
	require.Equal(t, http.StatusOK, manRec.Code, manRec.Body.String())
	assert.Equal(t, trajectoryContentTypeJSON, manRec.Header().Get("Content-Type"))
	var manifest telemetrycontract.TrajectorySegmentManifest
	require.NoError(t, json.Unmarshal(manRec.Body.Bytes(), &manifest))
	assert.Equal(t, agentA.ID, manifest.AgentID)
	_, expectedRawSHA, _ := gzipAndHash(t, raw)
	assert.Equal(t, expectedRawSHA, manifest.RawSHA256)

	// 3. Unsigned GET /content returns identical 403 for a real segment and a
	// random non-existent UUID (no existence oracle, and cannot bypass audit).
	unsignedReal := telemetrycontract.TrajectorySegmentContentAgentPath(agentA.ID, segID)
	unsignedMissing := telemetrycontract.TrajectorySegmentContentAgentPath(agentA.ID, api.NewUUID())
	assert.Equal(t, http.StatusForbidden, doTrajectoryUserGet(t, srv, unsignedReal, adminTok).Code)
	assert.Equal(t, http.StatusForbidden, doTrajectoryUserGet(t, srv, unsignedMissing, adminTok).Code)

	// 4. Tampered signature returns identical 403 for real and non-existent segments.
	tampered := strings.Replace(segURL.RequestURI(), "sig=", "sig=AAAA", 1)
	tamperedMissing := strings.Replace(tampered, segID, api.NewUUID(), 1)
	assert.Equal(t, http.StatusForbidden, doTrajectoryUserGet(t, srv, tampered, "").Code)
	assert.Equal(t, http.StatusForbidden, doTrajectoryUserGet(t, srv, tamperedMissing, "").Code)

	// 5. Signed URL expires after TTL.
	now = now.Add(6 * time.Minute)
	assert.Equal(t, http.StatusForbidden, doTrajectoryUserGet(t, srv, segURL.RequestURI(), "").Code)
	assert.Equal(t, http.StatusForbidden, doTrajectoryUserGet(t, srv, manURL.RequestURI(), "").Code)
}

func TestTrajectoryRead_CapabilityReplayProtection(t *testing.T) {
	srv, s, _, projA, agentA, agentTok := setupTrajectorySegmentFixture(t)

	raw1 := []byte("{\"seg\":1}\n")
	raw2 := []byte("{\"seg\":2}\n")
	seg1ID, gz1, seg1UploadURLStr := commitSampleTrajectorySegment(t, srv, agentA.ID, agentTok,
		".claude/projects/-workspace/sess-replay.jsonl", 0, 0, raw1)
	seg2ID, _, _ := commitSampleTrajectorySegment(t, srv, agentA.ID, agentTok,
		".claude/projects/-workspace/sess-replay.jsonl", 0, int64(len(raw1)), raw2)

	adminA := createTrajectoryTestUserWithRole(t, s, "replay-admin@example.com", store.ProjectRoleAdmin, store.RoleScopeProject, projA.ID)
	adminTok := trajectoryUserSessionToken(t, srv, adminA)

	// Issue a valid download URL (segment + manifest) for seg1.
	dlRec := doTrajectoryUserGet(t, srv,
		telemetrycontract.TrajectorySegmentDownloadAgentPath(agentA.ID, seg1ID)+"?include_manifest=true", adminTok)
	require.Equal(t, http.StatusOK, dlRec.Code, dlRec.Body.String())
	var dlResp telemetrycontract.TrajectorySegmentDownloadResponse
	require.NoError(t, json.Unmarshal(dlRec.Body.Bytes(), &dlResp))
	require.NotNil(t, dlResp.Manifest)

	seg1URL, err := url.Parse(dlResp.URL)
	require.NoError(t, err)
	man1URL, err := url.Parse(dlResp.Manifest.URL)
	require.NoError(t, err)

	// 1. Segment-ID swap: seg1 download capability replayed against seg2 -> 403.
	swappedSegPath := telemetrycontract.TrajectorySegmentContentAgentPath(agentA.ID, seg2ID) + "?" + seg1URL.RawQuery
	assert.Equal(t, http.StatusForbidden, doTrajectoryUserGet(t, srv, swappedSegPath, "").Code)

	// 2. kind=manifest swap: segment capability with kind=manifest -> 403,
	//    and manifest capability with kind=manifest stripped -> 403.
	segAsManifest := seg1URL.Path + "?" + seg1URL.RawQuery + "&kind=manifest"
	assert.Equal(t, http.StatusForbidden, doTrajectoryUserGet(t, srv, segAsManifest, "").Code)
	manQ := man1URL.Query()
	manQ.Del("kind")
	manAsSeg := man1URL.Path + "?" + manQ.Encode()
	assert.Equal(t, http.StatusForbidden, doTrajectoryUserGet(t, srv, manAsSeg, "").Code)

	// 3. Download capability replayed as PUT upload (without and with agent token).
	putNoTok := httptest.NewRequest(http.MethodPut, seg1URL.RequestURI(), bytes.NewReader(gz1))
	putNoTokRec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(putNoTokRec, putNoTok)
	assert.Equal(t, http.StatusUnauthorized, putNoTokRec.Code)

	putWithTok := httptest.NewRequest(http.MethodPut, seg1URL.RequestURI(), bytes.NewReader(gz1))
	putWithTok.Header.Set("X-Scion-Agent-Token", agentTok)
	putWithTokRec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(putWithTokRec, putWithTok)
	assert.Equal(t, http.StatusForbidden, putWithTokRec.Code)

	// 4. Upload capability (from segments:prepare) replayed as GET download
	//    (without and with agent token) -> 403.
	//    First, replay seg1's own upload capability against committed seg1 so
	//    agent ID, segment ID, object path, and committed status all match and
	//    HMAC domain separation alone must refuse the request.
	seg1UploadURL, err := url.Parse(seg1UploadURLStr)
	require.NoError(t, err)
	assert.Equal(t, http.StatusForbidden, doTrajectoryUserGet(t, srv, seg1UploadURL.RequestURI(), "").Code)
	sameSegGetWithAgentTok := httptest.NewRequest(http.MethodGet, seg1UploadURL.RequestURI(), nil)
	sameSegGetWithAgentTok.Header.Set("X-Scion-Agent-Token", agentTok)
	sameSegGetWithAgentTokRec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(sameSegGetWithAgentTokRec, sameSegGetWithAgentTok)
	assert.Equal(t, http.StatusForbidden, sameSegGetWithAgentTokRec.Code)

	raw3 := []byte("{\"seg\":3}\n")
	gz3, raw3SHA, comp3SHA := gzipAndHash(t, raw3)
	prepRec := doTrajectoryAgentJSON(t, srv, http.MethodPost,
		telemetrycontract.TrajectorySegmentsPrepareAgentPath(agentA.ID), agentTok,
		telemetrycontract.TrajectorySegmentPrepareRequest{
			Harness:            "claude",
			NativeRelativePath: ".claude/projects/-workspace/sess-replay.jsonl",
			ArtifactKind:       telemetrycontract.TrajectoryArtifactKindJSONLAppend,
			Generation:         0,
			ByteStart:          int64(len(raw1) + len(raw2)),
			ByteEnd:            int64(len(raw1) + len(raw2) + len(raw3)),
			RawSHA256:          raw3SHA,
			CompressedSize:     int64(len(gz3)),
			CompressedSHA256:   comp3SHA,
			ContentEncoding:    telemetrycontract.TrajectoryContentEncodingGzip,
		})
	require.Equal(t, http.StatusOK, prepRec.Code, prepRec.Body.String())
	var prepResp telemetrycontract.TrajectorySegmentPrepareResponse
	require.NoError(t, json.Unmarshal(prepRec.Body.Bytes(), &prepResp))
	uploadURL, err := url.Parse(prepResp.Upload.URL)
	require.NoError(t, err)

	// Replay upload capability as GET against both the pending segment and a committed segment.
	assert.Equal(t, http.StatusForbidden, doTrajectoryUserGet(t, srv, uploadURL.RequestURI(), "").Code)
	getWithAgentTok := httptest.NewRequest(http.MethodGet, uploadURL.RequestURI(), nil)
	getWithAgentTok.Header.Set("X-Scion-Agent-Token", agentTok)
	getWithAgentTokRec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(getWithAgentTokRec, getWithAgentTok)
	assert.Equal(t, http.StatusForbidden, getWithAgentTokRec.Code)

	uploadQueryOnCommitted := telemetrycontract.TrajectorySegmentContentAgentPath(agentA.ID, seg1ID) + "?" + uploadURL.RawQuery
	assert.Equal(t, http.StatusForbidden, doTrajectoryUserGet(t, srv, uploadQueryOnCommitted, "").Code)

	// Confirm the audit count stayed at exactly 1 (only the legitimate download).
	assert.Len(t, listTrajectoryReadAudits(t, s, seg1ID), 1)
}

func TestTrajectoryRead_ListFiltersAndPagination(t *testing.T) {
	srv, s, _, projA, agentA, agentTok := setupTrajectorySegmentFixture(t)

	s1, _, _ := commitSampleTrajectorySegment(t, srv, agentA.ID, agentTok, ".claude/projects/-workspace/a.jsonl", 0, 0, []byte("line1\n"))
	s2, _, _ := commitSampleTrajectorySegment(t, srv, agentA.ID, agentTok, ".claude/projects/-workspace/a.jsonl", 0, 6, []byte("line2\n"))
	s3, _, _ := commitSampleTrajectorySegment(t, srv, agentA.ID, agentTok, ".claude/projects/-workspace/b.jsonl", 1, 0, []byte("line3\n"))

	adminA := createTrajectoryTestUserWithRole(t, s, "page-admin@example.com", store.ProjectRoleAdmin, store.RoleScopeProject, projA.ID)
	adminTok := trajectoryUserSessionToken(t, srv, adminA)

	base := telemetrycontract.TrajectorySegmentsListAgentPath(agentA.ID)

	// Page 1 (limit=2).
	rec1 := doTrajectoryUserGet(t, srv, base+"?limit=2", adminTok)
	require.Equal(t, http.StatusOK, rec1.Code, rec1.Body.String())
	var page1 telemetrycontract.TrajectorySegmentsListResponse
	require.NoError(t, json.Unmarshal(rec1.Body.Bytes(), &page1))
	require.Len(t, page1.Segments, 2)
	assert.Equal(t, s1, page1.Segments[0].SegmentID)
	assert.Equal(t, s2, page1.Segments[1].SegmentID)
	require.NotEmpty(t, page1.NextCursor)

	// Page 2 using next_cursor.
	rec2 := doTrajectoryUserGet(t, srv, base+"?limit=2&cursor="+url.QueryEscape(page1.NextCursor), adminTok)
	require.Equal(t, http.StatusOK, rec2.Code, rec2.Body.String())
	var page2 telemetrycontract.TrajectorySegmentsListResponse
	require.NoError(t, json.Unmarshal(rec2.Body.Bytes(), &page2))
	require.Len(t, page2.Segments, 1)
	assert.Equal(t, s3, page2.Segments[0].SegmentID)
	assert.Empty(t, page2.NextCursor)

	// Filter by generation=1 & native_relative_path.
	recF := doTrajectoryUserGet(t, srv, base+"?generation=1&native_relative_path="+url.QueryEscape(".claude/projects/-workspace/b.jsonl"), adminTok)
	require.Equal(t, http.StatusOK, recF.Code, recF.Body.String())
	var filtered telemetrycontract.TrajectorySegmentsListResponse
	require.NoError(t, json.Unmarshal(recF.Body.Bytes(), &filtered))
	require.Len(t, filtered.Segments, 1)
	assert.Equal(t, s3, filtered.Segments[0].SegmentID)

	// Invalid query params return 400.
	assert.Equal(t, http.StatusBadRequest, doTrajectoryUserGet(t, srv, base+"?generation=-1", adminTok).Code)
	assert.Equal(t, http.StatusBadRequest, doTrajectoryUserGet(t, srv, base+"?limit=0", adminTok).Code)
	assert.Equal(t, http.StatusBadRequest, doTrajectoryUserGet(t, srv, base+"?cursor=!!!", adminTok).Code)
}

type fakeGCSSignedGetStorage struct {
	storage.Storage
	signCalls int
	lastPath  string
	lastOpts  storage.SignedURLOptions
}

func (f *fakeGCSSignedGetStorage) Provider() storage.Provider { return storage.ProviderGCS }

func (f *fakeGCSSignedGetStorage) GenerateSignedURL(_ context.Context, objectPath string, opts storage.SignedURLOptions) (*storage.SignedURL, error) {
	f.signCalls++
	f.lastPath = objectPath
	f.lastOpts = opts
	return &storage.SignedURL{
		URL:     "https://storage.googleapis.com/fake-bucket/" + objectPath + "?X-Goog-Signature=deadbeef",
		Method:  opts.Method,
		Expires: time.Now().UTC().Add(opts.Expires),
	}, nil
}

func TestTrajectoryRead_GCSSignedGET(t *testing.T) {
	srv, s, loc, projA, agentA, agentTok := setupTrajectorySegmentFixture(t)
	segID, _, _ := commitSampleTrajectorySegment(t, srv, agentA.ID, agentTok,
		".claude/projects/-workspace/sess-gcs.jsonl", 0, 0, []byte("{\"gcs\":true}\n"))

	fakeGCS := &fakeGCSSignedGetStorage{Storage: loc}
	srv.SetStorage(fakeGCS)

	adminA := createTrajectoryTestUserWithRole(t, s, "gcs-admin@example.com", store.ProjectRoleAdmin, store.RoleScopeProject, projA.ID)
	adminTok := trajectoryUserSessionToken(t, srv, adminA)

	dlRec := doTrajectoryUserGet(t, srv, telemetrycontract.TrajectorySegmentDownloadAgentPath(agentA.ID, segID), adminTok)
	require.Equal(t, http.StatusOK, dlRec.Code, dlRec.Body.String())

	var dlResp telemetrycontract.TrajectorySegmentDownloadResponse
	require.NoError(t, json.Unmarshal(dlRec.Body.Bytes(), &dlResp))
	assert.True(t, strings.HasPrefix(dlResp.URL, "https://storage.googleapis.com/fake-bucket/"))
	assert.Equal(t, http.MethodGet, fakeGCS.lastOpts.Method)
	assert.Equal(t, telemetrycontract.DefaultTrajectoryDownloadURLExpiry, fakeGCS.lastOpts.Expires)
	assert.Equal(t, trajectoryContentTypeGzip, fakeGCS.lastOpts.ResponseContentType)
	assert.Len(t, listTrajectoryReadAudits(t, s, segID), 1)
}
