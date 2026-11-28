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
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
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

func enableTrajectoryCaptureExperiment(t *testing.T, srv *Server) {
	t.Helper()
	cur := srv.experimentRegistry()
	var active []experiments.Experiment
	for _, e := range cur.All() {
		if e.Name == experiments.TokenomicsTrajectoryCapture {
			e.Default = true
		}
		active = append(active, e)
	}
	reg, err := experiments.NewRegistry(active, cur.Retired())
	require.NoError(t, err)
	srv.setExperimentRegistry(reg)
}

func setupTrajectorySegmentFixture(t *testing.T) (*Server, store.Store, *storage.LocalStorage, *store.Project, *store.Agent, string) {
	t.Helper()
	srv, s := testServer(t)
	enableTrajectoryCaptureExperiment(t, srv)

	loc, err := storage.NewLocal(storage.Config{
		Provider:  storage.ProviderLocal,
		Bucket:    "local",
		LocalPath: t.TempDir(),
	})
	require.NoError(t, err)
	srv.SetStorage(loc)

	ctx := context.Background()
	proj := &store.Project{
		ID:   api.NewUUID(),
		Name: "traj-proj",
		Slug: "traj-proj",
	}
	require.NoError(t, s.CreateProject(ctx, proj))

	runID := "run-" + api.NewUUID()
	agent := &store.Agent{
		ID:        api.NewUUID(),
		Slug:      "traj-agent",
		Name:      "traj-agent",
		ProjectID: proj.ID,
		RunID:     runID,
		OwnerID:   DevUserID,
		CreatedBy: DevUserID,
		AppliedConfig: &store.AgentAppliedConfig{
			Harness:       "claude",
			HarnessConfig: "claude-default",
		},
		Template: "developer",
	}
	require.NoError(t, s.CreateAgent(ctx, agent))

	_, err = s.EnsureRunAttribution(ctx, &store.RunAttribution{
		RunID:         runID,
		HubID:         srv.usageLedgerHubID(),
		ProjectID:     proj.ID,
		AgentID:       agent.ID,
		AgentSlug:     agent.Slug,
		Harness:       "claude",
		HarnessConfig: "claude-default",
		Template:      "developer",
	})
	require.NoError(t, err)

	tok, cred, err := srv.agentTokenService.SignAgentToken(AgentTokenGrant{
		AgentID:   agent.ID,
		ProjectID: proj.ID,
		Scopes:    []AgentTokenScope{ScopeAgentStatusUpdate},
	}, runID)
	require.NoError(t, err)
	require.NoError(t, s.CreateAgentCredential(ctx, cred))

	return srv, s, loc, proj, agent, tok
}

func gzipAndHash(t *testing.T, raw []byte) (compressed []byte, rawSHA, compSHA string) {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	_, err := gz.Write(raw)
	require.NoError(t, err)
	require.NoError(t, gz.Close())
	compressed = buf.Bytes()

	rSum := sha256.Sum256(raw)
	cSum := sha256.Sum256(compressed)
	return compressed, hex.EncodeToString(rSum[:]), hex.EncodeToString(cSum[:])
}

func doTrajectoryAgentJSON(t *testing.T, srv *Server, method, path, token string, body any) *httptest.ResponseRecorder {
	t.Helper()
	var bodyReader io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		require.NoError(t, err)
		bodyReader = bytes.NewReader(b)
	}
	req := httptest.NewRequest(method, path, bodyReader)
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("X-Scion-Agent-Token", token)
	}
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	return rec
}

func doTrajectoryAgentPut(t *testing.T, srv *Server, rawURL, token string, payload []byte, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	u, err := url.Parse(rawURL)
	require.NoError(t, err)
	req := httptest.NewRequest(http.MethodPut, u.RequestURI(), bytes.NewReader(payload))
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	if token != "" {
		req.Header.Set("X-Scion-Agent-Token", token)
	}
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	return rec
}

func decodeTrajectoryErrorCode(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	var env struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &env), "body=%s", rec.Body.String())
	return env.Error.Code
}

func TestTrajectorySegments_EndToEndLocalHappyPathAndReplay(t *testing.T) {
	srv, s, loc, proj, agent, tok := setupTrajectorySegmentFixture(t)
	ctx := context.Background()

	raw := []byte("{\"type\":\"user\",\"text\":\"synthetic line 1\"}\n{\"type\":\"assistant\",\"text\":\"synthetic line 2\"}\n")
	comp, rawSHA, compSHA := gzipAndHash(t, raw)

	prepReq := telemetrycontract.TrajectorySegmentPrepareRequest{
		Harness:            "claude",
		NativeRelativePath: ".claude/projects/-workspace/sess-abc.jsonl",
		ArtifactKind:       telemetrycontract.TrajectoryArtifactKindJSONLAppend,
		Generation:         0,
		ByteStart:          0,
		ByteEnd:            int64(len(raw)),
		RawSHA256:          rawSHA,
		CompressedSize:     int64(len(comp)),
		CompressedSHA256:   compSHA,
		ContentEncoding:    telemetrycontract.TrajectoryContentEncodingGzip,
		IncompleteTail:     false,
		SessionID:          "sess-abc",
	}

	// 1. Prepare
	rec := doTrajectoryAgentJSON(t, srv, http.MethodPost,
		telemetrycontract.TrajectorySegmentsPrepareAgentPath(agent.ID), tok, prepReq)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	var prepResp telemetrycontract.TrajectorySegmentPrepareResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &prepResp))
	assert.NotEmpty(t, prepResp.SegmentID)
	assert.Equal(t, telemetrycontract.TrajectorySegmentStateUploadRequired, prepResp.State)
	require.NotNil(t, prepResp.Upload)
	assert.Equal(t, http.MethodPut, prepResp.Upload.Method)
	assert.Contains(t, prepResp.Upload.URL, "/trajectory/segments/"+prepResp.SegmentID+"/content")
	assert.Contains(t, prepResp.Upload.URL, "sig=")
	assert.Contains(t, prepResp.Upload.URL, "exp=")

	// 2. O1: Uploading same-length wrong bytes BEFORE object exists returns 422
	// and does NOT occupy the immutable path, so the valid upload still succeeds.
	diffComp := append([]byte(nil), comp...)
	diffComp[len(diffComp)-1] ^= 0xff
	putBadHash := doTrajectoryAgentPut(t, srv, prepResp.Upload.URL, tok, diffComp, prepResp.Upload.Headers)
	require.Equal(t, http.StatusUnprocessableEntity, putBadHash.Code, putBadHash.Body.String())
	assert.Equal(t, telemetrycontract.TrajectoryErrVerificationFailed, decodeTrajectoryErrorCode(t, putBadHash))

	// 2a. Upload valid bytes via Hub-proxied PUT -> 200.
	putRec := doTrajectoryAgentPut(t, srv, prepResp.Upload.URL, tok, comp, prepResp.Upload.Headers)
	require.Equal(t, http.StatusOK, putRec.Code, putRec.Body.String())

	// 2b. No-overwrite: repeating identical bytes -> 200.
	putIdentical := doTrajectoryAgentPut(t, srv, prepResp.Upload.URL, tok, comp, prepResp.Upload.Headers)
	require.Equal(t, http.StatusOK, putIdentical.Code, putIdentical.Body.String())

	// 2c. Tampered signature -> 403.
	putBadSig := doTrajectoryAgentPut(t, srv, prepResp.Upload.URL+"tamper", tok, comp, prepResp.Upload.Headers)
	require.Equal(t, http.StatusForbidden, putBadSig.Code, putBadSig.Body.String())

	// 3. Commit
	commitRec := doTrajectoryAgentJSON(t, srv, http.MethodPost,
		telemetrycontract.TrajectorySegmentsCommitAgentPath(agent.ID), tok,
		telemetrycontract.TrajectorySegmentCommitRequest{SegmentID: prepResp.SegmentID})
	require.Equal(t, http.StatusOK, commitRec.Code, commitRec.Body.String())

	var commitResp telemetrycontract.TrajectorySegmentCommitResponse
	require.NoError(t, json.Unmarshal(commitRec.Body.Bytes(), &commitResp))
	assert.Equal(t, telemetrycontract.TrajectorySegmentStateCommitted, commitResp.State)
	assert.Equal(t, prepResp.SegmentID, commitResp.SegmentID)
	assert.Equal(t, prepResp.ObjectPath+".manifest.json", commitResp.ManifestPath)

	// Verify DB row is committed.
	row, err := s.GetTrajectorySegment(ctx, prepResp.SegmentID)
	require.NoError(t, err)
	assert.Equal(t, telemetrycontract.TrajectorySegmentStatusCommitted, row.Status)
	assert.Equal(t, commitResp.ManifestPath, row.ManifestPath)
	require.NotNil(t, row.CommittedAt)

	// Verify Hub-authored manifest exists and is stamped from run_attribution.
	rc, _, err := loc.Download(ctx, commitResp.ManifestPath)
	require.NoError(t, err)
	defer func() { _ = rc.Close() }()
	manifestBytes, err := io.ReadAll(rc)
	require.NoError(t, err)

	var manifest telemetrycontract.TrajectorySegmentManifest
	require.NoError(t, json.Unmarshal(manifestBytes, &manifest))
	assert.Equal(t, telemetrycontract.TrajectoryManifestSchemaVersion, manifest.SchemaVersion)
	assert.Equal(t, proj.ID, manifest.ProjectID)
	assert.Equal(t, agent.ID, manifest.AgentID)
	assert.Equal(t, agent.RunID, manifest.RunID)
	assert.Equal(t, "claude", manifest.Harness)
	assert.Equal(t, "claude-default", manifest.HarnessConfig)
	assert.Equal(t, "developer", manifest.Template)
	assert.Equal(t, rawSHA, manifest.RawSHA256)
	assert.Equal(t, compSHA, manifest.CompressedSHA256)
	assert.Len(t, manifest.ManifestDigest, 64)

	// 4. Idempotent commit replay -> 200 committed.
	commitAgain := doTrajectoryAgentJSON(t, srv, http.MethodPost,
		telemetrycontract.TrajectorySegmentsCommitAgentPath(agent.ID), tok,
		telemetrycontract.TrajectorySegmentCommitRequest{SegmentID: prepResp.SegmentID})
	require.Equal(t, http.StatusOK, commitAgain.Code, commitAgain.Body.String())

	// 5. Re-prepare of committed identity with same hash -> 200 committed, upload omitted.
	rePrepRec := doTrajectoryAgentJSON(t, srv, http.MethodPost,
		telemetrycontract.TrajectorySegmentsPrepareAgentPath(agent.ID), tok, prepReq)
	require.Equal(t, http.StatusOK, rePrepRec.Code, rePrepRec.Body.String())
	var rePrepResp telemetrycontract.TrajectorySegmentPrepareResponse
	require.NoError(t, json.Unmarshal(rePrepRec.Body.Bytes(), &rePrepResp))
	assert.Equal(t, telemetrycontract.TrajectorySegmentStateCommitted, rePrepResp.State)
	assert.Nil(t, rePrepResp.Upload)
	assert.Equal(t, prepResp.SegmentID, rePrepResp.SegmentID)

	// 6. Re-prepare of committed identity with different hash -> 409 segment_conflict.
	conflictReq := prepReq
	conflictReq.RawSHA256 = strings.Repeat("f", 64)
	conflictRec := doTrajectoryAgentJSON(t, srv, http.MethodPost,
		telemetrycontract.TrajectorySegmentsPrepareAgentPath(agent.ID), tok, conflictReq)
	require.Equal(t, http.StatusConflict, conflictRec.Code, conflictRec.Body.String())
	assert.Equal(t, telemetrycontract.TrajectoryErrSegmentConflict, decodeTrajectoryErrorCode(t, conflictRec))
}

func TestTrajectorySegments_VerificationFailuresAndExpiry(t *testing.T) {
	srv, _, loc, _, agent, tok := setupTrajectorySegmentFixture(t)
	ctx := context.Background()

	raw := []byte("{\"msg\":\"hello\"}\n")
	comp, rawSHA, compSHA := gzipAndHash(t, raw)

	prepReq := telemetrycontract.TrajectorySegmentPrepareRequest{
		Harness:            "claude",
		NativeRelativePath: ".claude/projects/-workspace/verify.jsonl",
		ArtifactKind:       telemetrycontract.TrajectoryArtifactKindJSONLAppend,
		Generation:         0,
		ByteStart:          0,
		ByteEnd:            int64(len(raw)),
		RawSHA256:          rawSHA,
		CompressedSize:     int64(len(comp)),
		CompressedSHA256:   compSHA,
		ContentEncoding:    telemetrycontract.TrajectoryContentEncodingGzip,
	}

	rec := doTrajectoryAgentJSON(t, srv, http.MethodPost,
		telemetrycontract.TrajectorySegmentsPrepareAgentPath(agent.ID), tok, prepReq)
	require.Equal(t, http.StatusOK, rec.Code)
	var prepResp telemetrycontract.TrajectorySegmentPrepareResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &prepResp))

	// Commit before uploading object -> 422 segment_verification_failed.
	commitMissing := doTrajectoryAgentJSON(t, srv, http.MethodPost,
		telemetrycontract.TrajectorySegmentsCommitAgentPath(agent.ID), tok,
		telemetrycontract.TrajectorySegmentCommitRequest{SegmentID: prepResp.SegmentID})
	require.Equal(t, http.StatusUnprocessableEntity, commitMissing.Code, commitMissing.Body.String())
	assert.Equal(t, telemetrycontract.TrajectoryErrVerificationFailed, decodeTrajectoryErrorCode(t, commitMissing))

	// Upload corrupt bytes of the right length -> 422 segment_verification_failed.
	corrupt := bytes.Repeat([]byte("x"), len(comp))
	_, err := loc.Upload(ctx, prepResp.ObjectPath, bytes.NewReader(corrupt), storage.UploadOptions{})
	require.NoError(t, err)

	commitCorrupt := doTrajectoryAgentJSON(t, srv, http.MethodPost,
		telemetrycontract.TrajectorySegmentsCommitAgentPath(agent.ID), tok,
		telemetrycontract.TrajectorySegmentCommitRequest{SegmentID: prepResp.SegmentID})
	require.Equal(t, http.StatusUnprocessableEntity, commitCorrupt.Code, commitCorrupt.Body.String())
	assert.Equal(t, telemetrycontract.TrajectoryErrVerificationFailed, decodeTrajectoryErrorCode(t, commitCorrupt))

	// Pending TTL expiry -> 410 segment_expired.
	srv.trajectoryPrepareTTL = time.Nanosecond
	expReq := prepReq
	expReq.Generation = 1
	expPrepRec := doTrajectoryAgentJSON(t, srv, http.MethodPost,
		telemetrycontract.TrajectorySegmentsPrepareAgentPath(agent.ID), tok, expReq)
	require.Equal(t, http.StatusOK, expPrepRec.Code)
	var expPrepResp telemetrycontract.TrajectorySegmentPrepareResponse
	require.NoError(t, json.Unmarshal(expPrepRec.Body.Bytes(), &expPrepResp))

	time.Sleep(5 * time.Millisecond)
	commitExpired := doTrajectoryAgentJSON(t, srv, http.MethodPost,
		telemetrycontract.TrajectorySegmentsCommitAgentPath(agent.ID), tok,
		telemetrycontract.TrajectorySegmentCommitRequest{SegmentID: expPrepResp.SegmentID})
	require.Equal(t, http.StatusGone, commitExpired.Code, commitExpired.Body.String())
	assert.Equal(t, telemetrycontract.TrajectoryErrSegmentExpired, decodeTrajectoryErrorCode(t, commitExpired))
}

func TestTrajectorySegments_AuthExperimentPolicyValidationAndCaps(t *testing.T) {
	srv, s, _, proj, agent, tok := setupTrajectorySegmentFixture(t)
	ctx := context.Background()

	// Create a second agent to test wrong-agent 403.
	otherAgent := &store.Agent{
		ID:        api.NewUUID(),
		Slug:      "other-agent",
		Name:      "other-agent",
		ProjectID: proj.ID,
		RunID:     "run-" + api.NewUUID(),
		OwnerID:   DevUserID,
		CreatedBy: DevUserID,
	}
	require.NoError(t, s.CreateAgent(ctx, otherAgent))
	otherTok, otherCred, err := srv.agentTokenService.SignAgentToken(AgentTokenGrant{
		AgentID:   otherAgent.ID,
		ProjectID: proj.ID,
		Scopes:    []AgentTokenScope{ScopeAgentStatusUpdate},
	}, otherAgent.RunID)
	require.NoError(t, err)
	require.NoError(t, s.CreateAgentCredential(ctx, otherCred))

	raw := []byte("{\"msg\":\"line\"}\n")
	comp, rawSHA, compSHA := gzipAndHash(t, raw)
	validReq := telemetrycontract.TrajectorySegmentPrepareRequest{
		Harness:            "claude",
		NativeRelativePath: ".claude/projects/-workspace/s1.jsonl",
		ArtifactKind:       telemetrycontract.TrajectoryArtifactKindJSONLAppend,
		Generation:         0,
		ByteStart:          0,
		ByteEnd:            int64(len(raw)),
		RawSHA256:          rawSHA,
		CompressedSize:     int64(len(comp)),
		CompressedSHA256:   compSHA,
		ContentEncoding:    telemetrycontract.TrajectoryContentEncodingGzip,
	}

	// Wrong agent -> 403 on prepare and commit.
	rec := doTrajectoryAgentJSON(t, srv, http.MethodPost,
		telemetrycontract.TrajectorySegmentsPrepareAgentPath(agent.ID), otherTok, validReq)
	require.Equal(t, http.StatusForbidden, rec.Code)

	rec = doTrajectoryAgentJSON(t, srv, http.MethodPost,
		telemetrycontract.TrajectorySegmentsCommitAgentPath(agent.ID), otherTok,
		telemetrycontract.TrajectorySegmentCommitRequest{SegmentID: "any"})
	require.Equal(t, http.StatusForbidden, rec.Code)

	// Path traversal rejected -> 400 invalid_segment.
	badPathReq := validReq
	badPathReq.NativeRelativePath = ".claude/projects/../../etc/passwd.jsonl"
	rec = doTrajectoryAgentJSON(t, srv, http.MethodPost,
		telemetrycontract.TrajectorySegmentsPrepareAgentPath(agent.ID), tok, badPathReq)
	require.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Equal(t, telemetrycontract.TrajectoryErrInvalidSegment, decodeTrajectoryErrorCode(t, rec))

	// Stubbed capture policy: off -> 403 capture_disabled; metadata_only -> 403 capture_metadata_only.
	srv.trajectoryCapturePolicyOverride = func(context.Context, *store.Project) string {
		return telemetrycontract.TrajectoryCapturePolicyOff
	}
	rec = doTrajectoryAgentJSON(t, srv, http.MethodPost,
		telemetrycontract.TrajectorySegmentsPrepareAgentPath(agent.ID), tok, validReq)
	require.Equal(t, http.StatusForbidden, rec.Code)
	assert.Equal(t, telemetrycontract.TrajectoryErrCaptureDisabled, decodeTrajectoryErrorCode(t, rec))

	srv.trajectoryCapturePolicyOverride = func(context.Context, *store.Project) string {
		return telemetrycontract.TrajectoryCapturePolicyMetadataOnly
	}
	rec = doTrajectoryAgentJSON(t, srv, http.MethodPost,
		telemetrycontract.TrajectorySegmentsPrepareAgentPath(agent.ID), tok, validReq)
	require.Equal(t, http.StatusForbidden, rec.Code)
	assert.Equal(t, telemetrycontract.TrajectoryErrCaptureMetadataOnly, decodeTrajectoryErrorCode(t, rec))
	srv.trajectoryCapturePolicyOverride = nil

	// Single-segment cap -> 413 segment_too_large.
	srv.trajectoryMaxSegmentRawBytes = 8
	rec = doTrajectoryAgentJSON(t, srv, http.MethodPost,
		telemetrycontract.TrajectorySegmentsPrepareAgentPath(agent.ID), tok, validReq)
	require.Equal(t, http.StatusRequestEntityTooLarge, rec.Code)
	assert.Equal(t, telemetrycontract.TrajectoryErrSegmentTooLarge, decodeTrajectoryErrorCode(t, rec))
	srv.trajectoryMaxSegmentRawBytes = 0

	// Per-run cap -> 413 run_cap_exceeded (charged per segment as max(raw, compressed)).
	chargedOne := max(int64(len(raw)), int64(len(comp)))
	srv.trajectoryMaxRunRawBytes = chargedOne
	rec = doTrajectoryAgentJSON(t, srv, http.MethodPost,
		telemetrycontract.TrajectorySegmentsPrepareAgentPath(agent.ID), tok, validReq)
	require.Equal(t, http.StatusOK, rec.Code)

	secondSeg := validReq
	secondSeg.ByteStart = int64(len(raw))
	secondSeg.ByteEnd = int64(len(raw) * 2)
	rec = doTrajectoryAgentJSON(t, srv, http.MethodPost,
		telemetrycontract.TrajectorySegmentsPrepareAgentPath(agent.ID), tok, secondSeg)
	require.Equal(t, http.StatusRequestEntityTooLarge, rec.Code)
	assert.Equal(t, telemetrycontract.TrajectoryErrRunCapExceeded, decodeTrajectoryErrorCode(t, rec))
	srv.trajectoryMaxRunRawBytes = 0

	// Experiment off -> 404.
	srv.setExperimentRegistry(experiments.Default())
	rec = doTrajectoryAgentJSON(t, srv, http.MethodPost,
		telemetrycontract.TrajectorySegmentsPrepareAgentPath(agent.ID), tok, validReq)
	require.Equal(t, http.StatusNotFound, rec.Code)

	rec = doTrajectoryAgentJSON(t, srv, http.MethodPost,
		telemetrycontract.TrajectorySegmentsCommitAgentPath(agent.ID), tok,
		telemetrycontract.TrajectorySegmentCommitRequest{SegmentID: "any"})
	require.Equal(t, http.StatusNotFound, rec.Code)
}

// gcsNoSignBlobStorage wraps LocalStorage while reporting ProviderGCS and
// failing GenerateSignedURL, exercising the Hub-proxied streaming PUT fallback
// for GCS deployments without iam.serviceAccounts.signBlob.
type gcsNoSignBlobStorage struct {
	*storage.LocalStorage
}

func (g *gcsNoSignBlobStorage) Provider() storage.Provider { return storage.ProviderGCS }

func (g *gcsNoSignBlobStorage) GenerateSignedURL(context.Context, string, storage.SignedURLOptions) (*storage.SignedURL, error) {
	return nil, errors.New("signBlob permission denied")
}

func TestTrajectorySegments_GCSWithoutSignBlobFallsBackToHubProxy(t *testing.T) {
	srv, _, loc, _, agent, tok := setupTrajectorySegmentFixture(t)
	srv.SetStorage(&gcsNoSignBlobStorage{LocalStorage: loc})

	raw := []byte("{\"msg\":\"fallback\"}\n")
	comp, rawSHA, compSHA := gzipAndHash(t, raw)

	prepReq := telemetrycontract.TrajectorySegmentPrepareRequest{
		Harness:            "claude",
		NativeRelativePath: ".claude/projects/-workspace/fallback.jsonl",
		ArtifactKind:       telemetrycontract.TrajectoryArtifactKindJSONLAppend,
		Generation:         0,
		ByteStart:          0,
		ByteEnd:            int64(len(raw)),
		RawSHA256:          rawSHA,
		CompressedSize:     int64(len(comp)),
		CompressedSHA256:   compSHA,
		ContentEncoding:    telemetrycontract.TrajectoryContentEncodingGzip,
	}

	rec := doTrajectoryAgentJSON(t, srv, http.MethodPost,
		telemetrycontract.TrajectorySegmentsPrepareAgentPath(agent.ID), tok, prepReq)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	var prepResp telemetrycontract.TrajectorySegmentPrepareResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &prepResp))
	require.NotNil(t, prepResp.Upload)
	assert.Contains(t, prepResp.Upload.URL, "/trajectory/segments/"+prepResp.SegmentID+"/content")

	putRec := doTrajectoryAgentPut(t, srv, prepResp.Upload.URL, tok, comp, prepResp.Upload.Headers)
	require.Equal(t, http.StatusOK, putRec.Code, putRec.Body.String())

	commitRec := doTrajectoryAgentJSON(t, srv, http.MethodPost,
		telemetrycontract.TrajectorySegmentsCommitAgentPath(agent.ID), tok,
		telemetrycontract.TrajectorySegmentCommitRequest{SegmentID: prepResp.SegmentID})
	require.Equal(t, http.StatusOK, commitRec.Code, commitRec.Body.String())
}

func TestTrajectorySegments_CommitTOCTOUAndIntegrityEdgeCases(t *testing.T) {
	srv, s, loc, proj, agent, tok := setupTrajectorySegmentFixture(t)
	ctx := context.Background()

	// Create a second agent to test cross-agent segment IDs -> 404.
	otherAgent := &store.Agent{
		ID:        api.NewUUID(),
		Slug:      "other-edge-agent",
		Name:      "other-edge-agent",
		ProjectID: proj.ID,
		RunID:     "run-" + api.NewUUID(),
		OwnerID:   DevUserID,
		CreatedBy: DevUserID,
	}
	require.NoError(t, s.CreateAgent(ctx, otherAgent))
	otherTok, otherCred, err := srv.agentTokenService.SignAgentToken(AgentTokenGrant{
		AgentID:   otherAgent.ID,
		ProjectID: proj.ID,
		Scopes:    []AgentTokenScope{ScopeAgentStatusUpdate},
	}, otherAgent.RunID)
	require.NoError(t, err)
	require.NoError(t, s.CreateAgentCredential(ctx, otherCred))

	rawA := []byte("{\"msg\":\"segment-A\"}\n")
	compA, rawSHAA, compSHAA := gzipAndHash(t, rawA)
	rawB := []byte("{\"msg\":\"segment-B-different\"}\n")
	compB, rawSHAB, compSHAB := gzipAndHash(t, rawB)

	// R2 handler-level TOCTOU interleave:
	// prepare(A) -> PUT A -> commit(A) [hook prepares B on same identity after verify] -> 422 refused, row stays pending.
	prepA := telemetrycontract.TrajectorySegmentPrepareRequest{
		Harness:            "claude",
		NativeRelativePath: ".claude/projects/-workspace/toctou.jsonl",
		ArtifactKind:       telemetrycontract.TrajectoryArtifactKindJSONLAppend,
		Generation:         0,
		ByteStart:          0,
		ByteEnd:            int64(len(rawA)),
		RawSHA256:          rawSHAA,
		CompressedSize:     int64(len(compA)),
		CompressedSHA256:   compSHAA,
		ContentEncoding:    telemetrycontract.TrajectoryContentEncodingGzip,
	}
	rec := doTrajectoryAgentJSON(t, srv, http.MethodPost,
		telemetrycontract.TrajectorySegmentsPrepareAgentPath(agent.ID), tok, prepA)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var prepRespA telemetrycontract.TrajectorySegmentPrepareResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &prepRespA))

	putA := doTrajectoryAgentPut(t, srv, prepRespA.Upload.URL, tok, compA, prepRespA.Upload.Headers)
	require.Equal(t, http.StatusOK, putA.Code, putA.Body.String())

	// Cross-agent segment ID on content PUT and commit -> 404.
	crossPut := doTrajectoryAgentPut(t, srv,
		telemetrycontract.TrajectorySegmentContentAgentPath(otherAgent.ID, prepRespA.SegmentID)+"?exp=9999999999&sig=abc",
		otherTok, compA, prepRespA.Upload.Headers)
	require.Equal(t, http.StatusNotFound, crossPut.Code, crossPut.Body.String())

	crossCommit := doTrajectoryAgentJSON(t, srv, http.MethodPost,
		telemetrycontract.TrajectorySegmentsCommitAgentPath(otherAgent.ID), otherTok,
		telemetrycontract.TrajectorySegmentCommitRequest{SegmentID: prepRespA.SegmentID})
	require.Equal(t, http.StatusNotFound, crossCommit.Code, crossCommit.Body.String())

	// Interleave a concurrent pending refresh to B right after
	// verifyStoredTrajectorySegment in commit(A) (via store directly, simulating
	// a race before the PUT completed) so the handler's CommitTrajectorySegment
	// expectation guard is exercised.
	srv.trajectoryCommitAfterVerifyHook = func(hookCtx context.Context, seg *store.TrajectorySegment) {
		refreshed := *seg
		refreshed.ID = ""
		refreshed.ByteEnd = int64(len(rawB))
		refreshed.RawSHA256 = rawSHAB
		refreshed.CompressedSize = int64(len(compB))
		refreshed.CompressedSHA256 = compSHAB
		_, hookErr := s.PrepareTrajectorySegment(hookCtx, &refreshed)
		require.NoError(t, hookErr)
	}
	commitA := doTrajectoryAgentJSON(t, srv, http.MethodPost,
		telemetrycontract.TrajectorySegmentsCommitAgentPath(agent.ID), tok,
		telemetrycontract.TrajectorySegmentCommitRequest{SegmentID: prepRespA.SegmentID})
	srv.trajectoryCommitAfterVerifyHook = nil
	require.Equal(t, http.StatusUnprocessableEntity, commitA.Code, commitA.Body.String())
	assert.Equal(t, telemetrycontract.TrajectoryErrVerificationFailed, decodeTrajectoryErrorCode(t, commitA))

	rowAfterTOCTOU, err := s.GetTrajectorySegment(ctx, prepRespA.SegmentID)
	require.NoError(t, err)
	assert.Equal(t, telemetrycontract.TrajectorySegmentStatusPending, rowAfterTOCTOU.Status)

	// Re-uploading B's bytes to the path already occupied by A's bytes returns 412.
	prepBReq := prepA
	prepBReq.ByteEnd = int64(len(rawB))
	prepBReq.RawSHA256 = rawSHAB
	prepBReq.CompressedSize = int64(len(compB))
	prepBReq.CompressedSHA256 = compSHAB
	recB := doTrajectoryAgentJSON(t, srv, http.MethodPost,
		telemetrycontract.TrajectorySegmentsPrepareAgentPath(agent.ID), tok, prepBReq)
	require.Equal(t, http.StatusOK, recB.Code)
	var prepRespB telemetrycontract.TrajectorySegmentPrepareResponse
	require.NoError(t, json.Unmarshal(recB.Body.Bytes(), &prepRespB))
	putBOnA := doTrajectoryAgentPut(t, srv, prepRespB.Upload.URL, tok, compB, prepRespB.Upload.Headers)
	require.Equal(t, http.StatusPreconditionFailed, putBOnA.Code, putBOnA.Body.String())

	// O5: compressed_sha256 matches, but raw_sha256 is wrong -> 422.
	badRawHashReq := prepA
	badRawHashReq.Generation = 1
	badRawHashReq.RawSHA256 = strings.Repeat("9", 64)
	recBadRaw := doTrajectoryAgentJSON(t, srv, http.MethodPost,
		telemetrycontract.TrajectorySegmentsPrepareAgentPath(agent.ID), tok, badRawHashReq)
	require.Equal(t, http.StatusOK, recBadRaw.Code)
	var badRawResp telemetrycontract.TrajectorySegmentPrepareResponse
	require.NoError(t, json.Unmarshal(recBadRaw.Body.Bytes(), &badRawResp))
	putBadRaw := doTrajectoryAgentPut(t, srv, badRawResp.Upload.URL, tok, compA, badRawResp.Upload.Headers)
	require.Equal(t, http.StatusOK, putBadRaw.Code)
	commitBadRaw := doTrajectoryAgentJSON(t, srv, http.MethodPost,
		telemetrycontract.TrajectorySegmentsCommitAgentPath(agent.ID), tok,
		telemetrycontract.TrajectorySegmentCommitRequest{SegmentID: badRawResp.SegmentID})
	require.Equal(t, http.StatusUnprocessableEntity, commitBadRaw.Code, commitBadRaw.Body.String())
	assert.Equal(t, telemetrycontract.TrajectoryErrVerificationFailed, decodeTrajectoryErrorCode(t, commitBadRaw))

	// O5: compressed_sha256 matches, but declared decompressed length (byte_end - byte_start) is wrong -> 422.
	badRawLenReq := prepA
	badRawLenReq.Generation = 2
	badRawLenReq.ByteEnd = int64(len(rawA) - 1)
	recBadLen := doTrajectoryAgentJSON(t, srv, http.MethodPost,
		telemetrycontract.TrajectorySegmentsPrepareAgentPath(agent.ID), tok, badRawLenReq)
	require.Equal(t, http.StatusOK, recBadLen.Code)
	var badLenResp telemetrycontract.TrajectorySegmentPrepareResponse
	require.NoError(t, json.Unmarshal(recBadLen.Body.Bytes(), &badLenResp))
	putBadLen := doTrajectoryAgentPut(t, srv, badLenResp.Upload.URL, tok, compA, badLenResp.Upload.Headers)
	require.Equal(t, http.StatusOK, putBadLen.Code)
	commitBadLen := doTrajectoryAgentJSON(t, srv, http.MethodPost,
		telemetrycontract.TrajectorySegmentsCommitAgentPath(agent.ID), tok,
		telemetrycontract.TrajectorySegmentCommitRequest{SegmentID: badLenResp.SegmentID})
	require.Equal(t, http.StatusUnprocessableEntity, commitBadLen.Code, commitBadLen.Body.String())
	assert.Equal(t, telemetrycontract.TrajectoryErrVerificationFailed, decodeTrajectoryErrorCode(t, commitBadLen))

	// O2: mismatched pre-existing manifest on disk -> 422.
	badManReq := prepA
	badManReq.Generation = 3
	recBadMan := doTrajectoryAgentJSON(t, srv, http.MethodPost,
		telemetrycontract.TrajectorySegmentsPrepareAgentPath(agent.ID), tok, badManReq)
	require.Equal(t, http.StatusOK, recBadMan.Code)
	var badManResp telemetrycontract.TrajectorySegmentPrepareResponse
	require.NoError(t, json.Unmarshal(recBadMan.Body.Bytes(), &badManResp))
	putBadMan := doTrajectoryAgentPut(t, srv, badManResp.Upload.URL, tok, compA, badManResp.Upload.Headers)
	require.Equal(t, http.StatusOK, putBadMan.Code)
	_, err = loc.Upload(ctx, telemetrycontract.TrajectoryRawManifestPath(badManResp.ObjectPath),
		strings.NewReader(`{"raw_sha256":"mismatch"}`), storage.UploadOptions{ContentType: "application/json"})
	require.NoError(t, err)
	commitBadMan := doTrajectoryAgentJSON(t, srv, http.MethodPost,
		telemetrycontract.TrajectorySegmentsCommitAgentPath(agent.ID), tok,
		telemetrycontract.TrajectorySegmentCommitRequest{SegmentID: badManResp.SegmentID})
	require.Equal(t, http.StatusUnprocessableEntity, commitBadMan.Code, commitBadMan.Body.String())
	assert.Equal(t, telemetrycontract.TrajectoryErrVerificationFailed, decodeTrajectoryErrorCode(t, commitBadMan))

	// O5: expired HMAC signature on proxied PUT -> 403.
	expiredSig, err := srv.signTrajectoryUploadURL(agent.ID, badManResp.SegmentID, int64(len(compA)), compSHAA, time.Now().Add(-time.Minute))
	require.NoError(t, err)
	expiredURL := telemetrycontract.TrajectorySegmentContentAgentPath(agent.ID, badManResp.SegmentID) + "?" + expiredSig
	putExpired := doTrajectoryAgentPut(t, srv, expiredURL, tok, compA, badManResp.Upload.Headers)
	require.Equal(t, http.StatusForbidden, putExpired.Code, putExpired.Body.String())

	// O5: two concurrent local PUTs with identical bytes both succeed (one creates, one sees identical -> 200).
	concReq := prepA
	concReq.Generation = 4
	recConc := doTrajectoryAgentJSON(t, srv, http.MethodPost,
		telemetrycontract.TrajectorySegmentsPrepareAgentPath(agent.ID), tok, concReq)
	require.Equal(t, http.StatusOK, recConc.Code)
	var concResp telemetrycontract.TrajectorySegmentPrepareResponse
	require.NoError(t, json.Unmarshal(recConc.Body.Bytes(), &concResp))

	var wg sync.WaitGroup
	codes := make([]int, 2)
	for i := range 2 {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			r := doTrajectoryAgentPut(t, srv, concResp.Upload.URL, tok, compA, concResp.Upload.Headers)
			codes[idx] = r.Code
		}(i)
	}
	wg.Wait()
	assert.Equal(t, []int{http.StatusOK, http.StatusOK}, codes)
}

func TestTrajectorySegments_ProvenanceValidation(t *testing.T) {
	srv, _, loc, _, agent, tok := setupTrajectorySegmentFixture(t)
	ctx := context.Background()

	raw := []byte("{\"type\":\"assistant\",\"text\":\"provenance test payload\"}\n")
	comp, rawSHA, compSHA := gzipAndHash(t, raw)

	baseReq := telemetrycontract.TrajectorySegmentPrepareRequest{
		Harness:            "claude",
		NativeRelativePath: ".claude/projects/-workspace/provenance.jsonl",
		ArtifactKind:       telemetrycontract.TrajectoryArtifactKindJSONLAppend,
		Generation:         0,
		ByteStart:          0,
		ByteEnd:            int64(len(raw)),
		RawSHA256:          rawSHA,
		CompressedSize:     int64(len(comp)),
		CompressedSHA256:   compSHA,
		ContentEncoding:    telemetrycontract.TrajectoryContentEncodingGzip,
		SessionID:          "sess-harden",
	}

	// Foreign origin_hub_id, oversize origin_hub_id/session_id, and
	// non-printable characters are all rejected with 400 invalid_segment.
	for _, tc := range []struct {
		name   string
		mutate func(*telemetrycontract.TrajectorySegmentPrepareRequest)
	}{
		{
			name:   "foreign origin_hub_id",
			mutate: func(r *telemetrycontract.TrajectorySegmentPrepareRequest) { r.OriginHubID = "other-hub" },
		},
		{
			name: "oversize origin_hub_id (10 KiB)",
			mutate: func(r *telemetrycontract.TrajectorySegmentPrepareRequest) {
				r.OriginHubID = strings.Repeat("a", 10*1024)
			},
		},
		{
			name:   "oversize session_id (257 bytes)",
			mutate: func(r *telemetrycontract.TrajectorySegmentPrepareRequest) { r.SessionID = strings.Repeat("s", 257) },
		},
		{
			name:   "control char in session_id",
			mutate: func(r *telemetrycontract.TrajectorySegmentPrepareRequest) { r.SessionID = "sess\nbad" },
		},
		{
			name:   "control char in origin_hub_id",
			mutate: func(r *telemetrycontract.TrajectorySegmentPrepareRequest) { r.OriginHubID = "hub\tbad" },
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := baseReq
			tc.mutate(&req)
			rec := doTrajectoryAgentJSON(t, srv, http.MethodPost,
				telemetrycontract.TrajectorySegmentsPrepareAgentPath(agent.ID), tok, req)
			require.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
			assert.Equal(t, telemetrycontract.TrajectoryErrInvalidSegment, decodeTrajectoryErrorCode(t, rec))
		})
	}

	// Matching this Hub's ID succeeds and stamps this Hub's ID into the manifest.
	ownHubReq := baseReq
	ownHubReq.OriginHubID = srv.usageLedgerHubID()
	rec := doTrajectoryAgentJSON(t, srv, http.MethodPost,
		telemetrycontract.TrajectorySegmentsPrepareAgentPath(agent.ID), tok, ownHubReq)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var ownHubPrep telemetrycontract.TrajectorySegmentPrepareResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &ownHubPrep))

	putOK := doTrajectoryAgentPut(t, srv, ownHubPrep.Upload.URL, tok, comp, ownHubPrep.Upload.Headers)
	require.Equal(t, http.StatusOK, putOK.Code, putOK.Body.String())
	commitOK := doTrajectoryAgentJSON(t, srv, http.MethodPost,
		telemetrycontract.TrajectorySegmentsCommitAgentPath(agent.ID), tok,
		telemetrycontract.TrajectorySegmentCommitRequest{SegmentID: ownHubPrep.SegmentID})
	require.Equal(t, http.StatusOK, commitOK.Code, commitOK.Body.String())
	var commitResp telemetrycontract.TrajectorySegmentCommitResponse
	require.NoError(t, json.Unmarshal(commitOK.Body.Bytes(), &commitResp))

	rc, _, err := loc.Download(ctx, commitResp.ManifestPath)
	require.NoError(t, err)
	defer func() { _ = rc.Close() }()
	var manifest telemetrycontract.TrajectorySegmentManifest
	require.NoError(t, json.NewDecoder(rc).Decode(&manifest))
	assert.Equal(t, srv.usageLedgerHubID(), manifest.OriginHubID)
	assert.Equal(t, "sess-harden", manifest.SessionID)
}

// midStreamPrecondStorage wraps LocalStorage and simulates a GCS mid-stream
// HTTP 412 Precondition Failed: when the target object already exists and
// IfGenerationMatch != nil, it reads only readBefore412 bytes (< full body)
// and returns storage.ErrPreconditionFailed without reading to EOF.
type midStreamPrecondStorage struct {
	*storage.LocalStorage
	readBefore412 int64
}

func (m *midStreamPrecondStorage) Upload(ctx context.Context, objectPath string, reader io.Reader, opts storage.UploadOptions) (*storage.Object, error) {
	if opts.IfGenerationMatch != nil {
		exists, err := m.Exists(ctx, objectPath)
		if err == nil && exists {
			_, _ = io.CopyN(io.Discard, reader, m.readBefore412)
			return nil, storage.ErrPreconditionFailed
		}
	}
	return m.LocalStorage.Upload(ctx, objectPath, reader, opts)
}

func TestTrajectorySegments_StreamingPutVerificationAndMidStream412Drain(t *testing.T) {
	srv, _, loc, _, agent, tok := setupTrajectorySegmentFixture(t)
	ctx := context.Background()

	raw := []byte("{\"type\":\"assistant\",\"text\":\"streaming verification payload\"}\n")
	comp, rawSHA, compSHA := gzipAndHash(t, raw)

	prepReq := telemetrycontract.TrajectorySegmentPrepareRequest{
		Harness:            "claude",
		NativeRelativePath: ".claude/projects/-workspace/streaming.jsonl",
		ArtifactKind:       telemetrycontract.TrajectoryArtifactKindJSONLAppend,
		Generation:         0,
		ByteStart:          0,
		ByteEnd:            int64(len(raw)),
		RawSHA256:          rawSHA,
		CompressedSize:     int64(len(comp)),
		CompressedSHA256:   compSHA,
		ContentEncoding:    telemetrycontract.TrajectoryContentEncodingGzip,
	}

	rec := doTrajectoryAgentJSON(t, srv, http.MethodPost,
		telemetrycontract.TrajectorySegmentsPrepareAgentPath(agent.ID), tok, prepReq)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var prep telemetrycontract.TrajectorySegmentPrepareResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &prep))

	// Streaming PUT rejects undersize and oversize payloads with 400 without
	// writing the immutable object path.
	putShort := doTrajectoryAgentPut(t, srv, prep.Upload.URL, tok, comp[:len(comp)-1], prep.Upload.Headers)
	require.Equal(t, http.StatusBadRequest, putShort.Code, putShort.Body.String())
	assert.Equal(t, telemetrycontract.TrajectoryErrInvalidSegment, decodeTrajectoryErrorCode(t, putShort))

	putLong := doTrajectoryAgentPut(t, srv, prep.Upload.URL, tok, append(append([]byte(nil), comp...), 0x00), prep.Upload.Headers)
	require.Equal(t, http.StatusBadRequest, putLong.Code, putLong.Body.String())
	assert.Equal(t, telemetrycontract.TrajectoryErrInvalidSegment, decodeTrajectoryErrorCode(t, putLong))

	exists, err := loc.Exists(ctx, prep.ObjectPath)
	require.NoError(t, err)
	assert.False(t, exists, "rejected streaming PUTs must not leave an object at the immutable path")

	// Upload valid object so the path now exists.
	putOK := doTrajectoryAgentPut(t, srv, prep.Upload.URL, tok, comp, prep.Upload.Headers)
	require.Equal(t, http.StatusOK, putOK.Code, putOK.Body.String())

	// O1: mid-stream 412 before EOF triggers drain + verification.
	// A wrong-hash body of the same length must return 422, never 200.
	srv.SetStorage(&midStreamPrecondStorage{LocalStorage: loc, readBefore412: 4})
	corruptComp := append([]byte(nil), comp...)
	corruptComp[len(corruptComp)-1] ^= 0xff
	putCorrupt412 := doTrajectoryAgentPut(t, srv, prep.Upload.URL, tok, corruptComp, prep.Upload.Headers)
	require.Equal(t, http.StatusUnprocessableEntity, putCorrupt412.Code, putCorrupt412.Body.String())
	assert.Equal(t, telemetrycontract.TrajectoryErrVerificationFailed, decodeTrajectoryErrorCode(t, putCorrupt412))

	// A matching body on the same mid-stream 412 path drains cleanly, matches
	// the existing object hash, and returns 200.
	putValid412 := doTrajectoryAgentPut(t, srv, prep.Upload.URL, tok, comp, prep.Upload.Headers)
	require.Equal(t, http.StatusOK, putValid412.Code, putValid412.Body.String())
}

func TestTrajectorySegments_CommitExpectationMetadataTOCTOU(t *testing.T) {
	srv, s, _, _, agent, tok := setupTrajectorySegmentFixture(t)
	ctx := context.Background()

	raw := []byte("{\"type\":\"assistant\",\"text\":\"commit expectation metadata toctou\"}\n")
	comp, rawSHA, compSHA := gzipAndHash(t, raw)

	n1Req := telemetrycontract.TrajectorySegmentPrepareRequest{
		Harness:            "claude",
		NativeRelativePath: ".claude/projects/-workspace/n1-toctou.jsonl",
		ArtifactKind:       telemetrycontract.TrajectoryArtifactKindJSONLAppend,
		Generation:         0,
		ByteStart:          0,
		ByteEnd:            int64(len(raw)),
		RawSHA256:          rawSHA,
		CompressedSize:     int64(len(comp)),
		CompressedSHA256:   compSHA,
		ContentEncoding:    telemetrycontract.TrajectoryContentEncodingGzip,
		IncompleteTail:     false,
		SessionID:          "sess-before",
	}

	prepRec := doTrajectoryAgentJSON(t, srv, http.MethodPost,
		telemetrycontract.TrajectorySegmentsPrepareAgentPath(agent.ID), tok, n1Req)
	require.Equal(t, http.StatusOK, prepRec.Code, prepRec.Body.String())
	var prep telemetrycontract.TrajectorySegmentPrepareResponse
	require.NoError(t, json.Unmarshal(prepRec.Body.Bytes(), &prep))

	putRec := doTrajectoryAgentPut(t, srv, prep.Upload.URL, tok, comp, prep.Upload.Headers)
	require.Equal(t, http.StatusOK, putRec.Code, putRec.Body.String())

	// Flipping only incomplete_tail between verify and commit UPDATE (via store
	// directly, simulating a race before PUT completed) must be refused with 422
	// and leave the row pending.
	srv.trajectoryCommitAfterVerifyHook = func(hookCtx context.Context, seg *store.TrajectorySegment) {
		flip := *seg
		flip.ID = ""
		flip.IncompleteTail = true
		_, hookErr := s.PrepareTrajectorySegment(hookCtx, &flip)
		require.NoError(t, hookErr)
	}
	commitRec := doTrajectoryAgentJSON(t, srv, http.MethodPost,
		telemetrycontract.TrajectorySegmentsCommitAgentPath(agent.ID), tok,
		telemetrycontract.TrajectorySegmentCommitRequest{SegmentID: prep.SegmentID})
	srv.trajectoryCommitAfterVerifyHook = nil
	require.Equal(t, http.StatusUnprocessableEntity, commitRec.Code, commitRec.Body.String())
	assert.Equal(t, telemetrycontract.TrajectoryErrVerificationFailed, decodeTrajectoryErrorCode(t, commitRec))

	row, err := s.GetTrajectorySegment(ctx, prep.SegmentID)
	require.NoError(t, err)
	assert.Equal(t, telemetrycontract.TrajectorySegmentStatusPending, row.Status)
}

func TestTrajectorySegments_OrphanStorageAndRowCaps(t *testing.T) {
	t.Run("expired uncommitted pending row counts toward cap and same-identity re-prepare reuses row", func(t *testing.T) {
		srv, s, _, proj, agent, tok := setupTrajectorySegmentFixture(t)
		ctx := context.Background()

		raw := []byte("{\"type\":\"assistant\",\"text\":\"orphan cap test payload\"}\n")
		comp, rawSHA, compSHA := gzipAndHash(t, raw)
		chargedOne := max(int64(len(raw)), int64(len(comp)))

		// Seed an already-expired uncommitted pending row deterministically
		// (PrepareExpiresAt in the past, no time.Sleep).
		expiredPath := ".claude/projects/-workspace/orphan.jsonl"
		objPath := telemetrycontract.TrajectoryRawObjectPath(
			srv.usageLedgerHubID(), proj.ID, agent.ID, agent.RunID,
			"claude", expiredPath, 0, 0,
		)
		seeded, err := s.PrepareTrajectorySegment(ctx, &store.TrajectorySegment{
			HubID:              srv.usageLedgerHubID(),
			OriginHubID:        srv.usageLedgerHubID(),
			ProjectID:          proj.ID,
			AgentID:            agent.ID,
			RunID:              agent.RunID,
			Harness:            "claude",
			NativeRelativePath: expiredPath,
			ArtifactKind:       telemetrycontract.TrajectoryArtifactKindJSONLAppend,
			Generation:         0,
			ByteStart:          0,
			ByteEnd:            int64(len(raw)),
			RawSHA256:          rawSHA,
			CompressedSize:     int64(len(comp)),
			CompressedSHA256:   compSHA,
			ObjectPath:         objPath,
			Status:             telemetrycontract.TrajectorySegmentStatusPending,
			NormalizeStatus:    telemetrycontract.TrajectoryNormalizeStatusPending,
			CreatedAt:          time.Now().UTC().Add(-2 * time.Hour),
			PrepareExpiresAt:   time.Now().UTC().Add(-time.Hour),
		})
		require.NoError(t, err)

		// Cap equals one segment's charged bytes.
		srv.trajectoryMaxRunRawBytes = chargedOne

		// Re-preparing the SAME expired identity reuses the row and excludes
		// itself from the cap check, so retries and probes do not exhaust it.
		sameReq := telemetrycontract.TrajectorySegmentPrepareRequest{
			Harness:            "claude",
			NativeRelativePath: expiredPath,
			ArtifactKind:       telemetrycontract.TrajectoryArtifactKindJSONLAppend,
			Generation:         0,
			ByteStart:          0,
			ByteEnd:            int64(len(raw)),
			RawSHA256:          rawSHA,
			CompressedSize:     int64(len(comp)),
			CompressedSHA256:   compSHA,
			ContentEncoding:    telemetrycontract.TrajectoryContentEncodingGzip,
		}
		rePrep := doTrajectoryAgentJSON(t, srv, http.MethodPost,
			telemetrycontract.TrajectorySegmentsPrepareAgentPath(agent.ID), tok, sameReq)
		require.Equal(t, http.StatusOK, rePrep.Code, rePrep.Body.String())
		var rePrepResp telemetrycontract.TrajectorySegmentPrepareResponse
		require.NoError(t, json.Unmarshal(rePrep.Body.Bytes(), &rePrepResp))
		assert.Equal(t, seeded.ID, rePrepResp.SegmentID)

		// Preparing a NEW identity over the remaining cap must get 413 run_cap_exceeded
		// even when the prior row is expired and uncommitted.
		newReq := sameReq
		newReq.Generation = 1
		overCap := doTrajectoryAgentJSON(t, srv, http.MethodPost,
			telemetrycontract.TrajectorySegmentsPrepareAgentPath(agent.ID), tok, newReq)
		require.Equal(t, http.StatusRequestEntityTooLarge, overCap.Code, overCap.Body.String())
		assert.Equal(t, telemetrycontract.TrajectoryErrRunCapExceeded, decodeTrajectoryErrorCode(t, overCap))
	})

	t.Run("deflate worst-case bound rejects multi-MiB compressed_size on 1-byte raw segment", func(t *testing.T) {
		srv, _, _, _, agent, tok := setupTrajectorySegmentFixture(t)
		req := telemetrycontract.TrajectorySegmentPrepareRequest{
			Harness:            "claude",
			NativeRelativePath: ".claude/projects/-workspace/padded.jsonl",
			ArtifactKind:       telemetrycontract.TrajectoryArtifactKindJSONLAppend,
			Generation:         0,
			ByteStart:          0,
			ByteEnd:            1,
			RawSHA256:          strings.Repeat("a", 64),
			CompressedSize:     4 * 1024 * 1024, // 4 MiB for 1 raw byte
			CompressedSHA256:   strings.Repeat("b", 64),
			ContentEncoding:    telemetrycontract.TrajectoryContentEncodingGzip,
		}
		rec := doTrajectoryAgentJSON(t, srv, http.MethodPost,
			telemetrycontract.TrajectorySegmentsPrepareAgentPath(agent.ID), tok, req)
		require.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
		assert.Equal(t, telemetrycontract.TrajectoryErrInvalidSegment, decodeTrajectoryErrorCode(t, rec))
	})

	t.Run("stored compressed bytes are charged against run cap for 1-raw-byte segments", func(t *testing.T) {
		srv, _, _, _, agent, tok := setupTrajectorySegmentFixture(t)
		// Cap is 900 bytes. Each 1-raw-byte segment declares 500 compressed bytes
		// (within the 1024-byte deflate framing allowance). Two segments charge
		// 1000 > 900 -> 413 run_cap_exceeded even though raw bytes total only 2.
		srv.trajectoryMaxRunRawBytes = 900
		makeReq := func(offset int64) telemetrycontract.TrajectorySegmentPrepareRequest {
			return telemetrycontract.TrajectorySegmentPrepareRequest{
				Harness:            "claude",
				NativeRelativePath: ".claude/projects/-workspace/small-raw.jsonl",
				ArtifactKind:       telemetrycontract.TrajectoryArtifactKindJSONLAppend,
				Generation:         0,
				ByteStart:          offset,
				ByteEnd:            offset + 1,
				RawSHA256:          strings.Repeat("a", 64),
				CompressedSize:     500,
				CompressedSHA256:   strings.Repeat("b", 64),
				ContentEncoding:    telemetrycontract.TrajectoryContentEncodingGzip,
			}
		}

		first := doTrajectoryAgentJSON(t, srv, http.MethodPost,
			telemetrycontract.TrajectorySegmentsPrepareAgentPath(agent.ID), tok, makeReq(0))
		require.Equal(t, http.StatusOK, first.Code, first.Body.String())

		second := doTrajectoryAgentJSON(t, srv, http.MethodPost,
			telemetrycontract.TrajectorySegmentsPrepareAgentPath(agent.ID), tok, makeReq(1))
		require.Equal(t, http.StatusRequestEntityTooLarge, second.Code, second.Body.String())
		assert.Equal(t, telemetrycontract.TrajectoryErrRunCapExceeded, decodeTrajectoryErrorCode(t, second))
	})

	t.Run("per-run segment row cap rejects tiny segments with 413 run_cap_exceeded", func(t *testing.T) {
		srv, _, _, _, agent, tok := setupTrajectorySegmentFixture(t)
		srv.trajectoryMaxRunSegments = 2

		raw := []byte("x")
		comp, rawSHA, compSHA := gzipAndHash(t, raw)
		makeReq := func(offset int64) telemetrycontract.TrajectorySegmentPrepareRequest {
			return telemetrycontract.TrajectorySegmentPrepareRequest{
				Harness:            "claude",
				NativeRelativePath: ".claude/projects/-workspace/rowcap.jsonl",
				ArtifactKind:       telemetrycontract.TrajectoryArtifactKindJSONLAppend,
				Generation:         0,
				ByteStart:          offset,
				ByteEnd:            offset + 1,
				RawSHA256:          rawSHA,
				CompressedSize:     int64(len(comp)),
				CompressedSHA256:   compSHA,
				ContentEncoding:    telemetrycontract.TrajectoryContentEncodingGzip,
			}
		}

		for i := int64(0); i < 2; i++ {
			rec := doTrajectoryAgentJSON(t, srv, http.MethodPost,
				telemetrycontract.TrajectorySegmentsPrepareAgentPath(agent.ID), tok, makeReq(i))
			require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		}

		// Re-preparing an existing identity still succeeds at the row cap.
		rePrep := doTrajectoryAgentJSON(t, srv, http.MethodPost,
			telemetrycontract.TrajectorySegmentsPrepareAgentPath(agent.ID), tok, makeReq(0))
		require.Equal(t, http.StatusOK, rePrep.Code, rePrep.Body.String())

		// A 3rd distinct segment exceeds the row cap -> 413 run_cap_exceeded.
		third := doTrajectoryAgentJSON(t, srv, http.MethodPost,
			telemetrycontract.TrajectorySegmentsPrepareAgentPath(agent.ID), tok, makeReq(2))
		require.Equal(t, http.StatusRequestEntityTooLarge, third.Code, third.Body.String())
		assert.Equal(t, telemetrycontract.TrajectoryErrRunCapExceeded, decodeTrajectoryErrorCode(t, third))
	})

	t.Run("shrink-after-upload re-prepare refused with 409 and run cap is not undercounted", func(t *testing.T) {
		srv, _, _, _, agent, tok := setupTrajectorySegmentFixture(t)

		bigRaw := bytes.Repeat([]byte("A"), 16*1024)
		bigComp, bigRawSHA, bigCompSHA := gzipAndHash(t, bigRaw)
		bigCharged := max(int64(len(bigRaw)), int64(len(bigComp)))

		// Cap allows 1 big segment, not 2.
		srv.trajectoryMaxRunRawBytes = bigCharged + bigCharged/2

		bigReq := telemetrycontract.TrajectorySegmentPrepareRequest{
			Harness:            "claude",
			NativeRelativePath: ".claude/projects/-workspace/shrink.jsonl",
			ArtifactKind:       telemetrycontract.TrajectoryArtifactKindJSONLAppend,
			Generation:         0,
			ByteStart:          0,
			ByteEnd:            int64(len(bigRaw)),
			RawSHA256:          bigRawSHA,
			CompressedSize:     int64(len(bigComp)),
			CompressedSHA256:   bigCompSHA,
			ContentEncoding:    telemetrycontract.TrajectoryContentEncodingGzip,
		}
		prep1 := doTrajectoryAgentJSON(t, srv, http.MethodPost,
			telemetrycontract.TrajectorySegmentsPrepareAgentPath(agent.ID), tok, bigReq)
		require.Equal(t, http.StatusOK, prep1.Code, prep1.Body.String())
		var prep1Resp telemetrycontract.TrajectorySegmentPrepareResponse
		require.NoError(t, json.Unmarshal(prep1.Body.Bytes(), &prep1Resp))

		// Upload the big object (200), leaving it uncommitted.
		put1 := doTrajectoryAgentPut(t, srv, prep1Resp.Upload.URL, tok, bigComp, prep1Resp.Upload.Headers)
		require.Equal(t, http.StatusOK, put1.Code, put1.Body.String())

		// Attempt to re-prepare the SAME pending identity with tiny values to
		// shrink its cap charge while the big object stays on disk -> 409 segment_conflict.
		tinyRaw := []byte("x")
		tinyComp, tinyRawSHA, tinyCompSHA := gzipAndHash(t, tinyRaw)
		shrinkReq := bigReq
		shrinkReq.ByteEnd = int64(len(tinyRaw))
		shrinkReq.RawSHA256 = tinyRawSHA
		shrinkReq.CompressedSize = int64(len(tinyComp))
		shrinkReq.CompressedSHA256 = tinyCompSHA
		shrinkRec := doTrajectoryAgentJSON(t, srv, http.MethodPost,
			telemetrycontract.TrajectorySegmentsPrepareAgentPath(agent.ID), tok, shrinkReq)
		require.Equal(t, http.StatusConflict, shrinkRec.Code, shrinkRec.Body.String())
		assert.Equal(t, telemetrycontract.TrajectoryErrSegmentConflict, decodeTrajectoryErrorCode(t, shrinkRec))

		// A new big identity must still get 413 run_cap_exceeded (no undercounting).
		gen1Req := bigReq
		gen1Req.Generation = 1
		overCap := doTrajectoryAgentJSON(t, srv, http.MethodPost,
			telemetrycontract.TrajectorySegmentsPrepareAgentPath(agent.ID), tok, gen1Req)
		require.Equal(t, http.StatusRequestEntityTooLarge, overCap.Code, overCap.Body.String())
		assert.Equal(t, telemetrycontract.TrajectoryErrRunCapExceeded, decodeTrajectoryErrorCode(t, overCap))
	})

	t.Run("capture probe without upload allows content-changing re-prepare and lost-ACK identical replay stays idempotent", func(t *testing.T) {
		srv, _, _, _, agent, tok := setupTrajectorySegmentFixture(t)

		probeRaw := []byte("{\"probe\":1}\n")
		probeComp, probeRawSHA, probeCompSHA := gzipAndHash(t, probeRaw)
		probeReq := telemetrycontract.TrajectorySegmentPrepareRequest{
			Harness:            "claude",
			NativeRelativePath: ".claude/projects/-workspace/probe-and-replay.jsonl",
			ArtifactKind:       telemetrycontract.TrajectoryArtifactKindJSONLAppend,
			Generation:         0,
			ByteStart:          0,
			ByteEnd:            int64(len(probeRaw)),
			RawSHA256:          probeRawSHA,
			CompressedSize:     int64(len(probeComp)),
			CompressedSHA256:   probeCompSHA,
			ContentEncoding:    telemetrycontract.TrajectoryContentEncodingGzip,
		}
		// 1. Capture probe prepares without uploading.
		rec1 := doTrajectoryAgentJSON(t, srv, http.MethodPost,
			telemetrycontract.TrajectorySegmentsPrepareAgentPath(agent.ID), tok, probeReq)
		require.Equal(t, http.StatusOK, rec1.Code, rec1.Body.String())
		var resp1 telemetrycontract.TrajectorySegmentPrepareResponse
		require.NoError(t, json.Unmarshal(rec1.Body.Bytes(), &resp1))

		// 2. Later, shipper prepares real content at the same identity (no object
		// was uploaded yet) -> 200 OK, reuses row.
		realRaw := []byte("{\"probe\":1}\n{\"real\":\"line 2\"}\n")
		realComp, realRawSHA, realCompSHA := gzipAndHash(t, realRaw)
		realReq := probeReq
		realReq.ByteEnd = int64(len(realRaw))
		realReq.RawSHA256 = realRawSHA
		realReq.CompressedSize = int64(len(realComp))
		realReq.CompressedSHA256 = realCompSHA
		rec2 := doTrajectoryAgentJSON(t, srv, http.MethodPost,
			telemetrycontract.TrajectorySegmentsPrepareAgentPath(agent.ID), tok, realReq)
		require.Equal(t, http.StatusOK, rec2.Code, rec2.Body.String())
		var resp2 telemetrycontract.TrajectorySegmentPrepareResponse
		require.NoError(t, json.Unmarshal(rec2.Body.Bytes(), &resp2))
		assert.Equal(t, resp1.SegmentID, resp2.SegmentID)

		// 3. Shipper uploads the object, then loses ACK and re-prepares IDENTICAL
		// content -> 200 OK idempotently even though the object exists.
		putRec := doTrajectoryAgentPut(t, srv, resp2.Upload.URL, tok, realComp, resp2.Upload.Headers)
		require.Equal(t, http.StatusOK, putRec.Code, putRec.Body.String())

		replayRec := doTrajectoryAgentJSON(t, srv, http.MethodPost,
			telemetrycontract.TrajectorySegmentsPrepareAgentPath(agent.ID), tok, realReq)
		require.Equal(t, http.StatusOK, replayRec.Code, replayRec.Body.String())
		var replayResp telemetrycontract.TrajectorySegmentPrepareResponse
		require.NoError(t, json.Unmarshal(replayRec.Body.Bytes(), &replayResp))
		assert.Equal(t, resp2.SegmentID, replayResp.SegmentID)
	})
}

func TestTrajectorySegments_TLSProxyAndBrokerRewrittenUploadURL(t *testing.T) {
	srv, _, _, _, agent, tok := setupTrajectorySegmentFixture(t)

	raw := []byte("{\"type\":\"assistant\",\"text\":\"proxy url test\"}\n")
	comp, rawSHA, compSHA := gzipAndHash(t, raw)
	baseReq := telemetrycontract.TrajectorySegmentPrepareRequest{
		Harness:            "claude",
		NativeRelativePath: ".claude/projects/-workspace/proxy-url.jsonl",
		ArtifactKind:       telemetrycontract.TrajectoryArtifactKindJSONLAppend,
		Generation:         0,
		ByteStart:          0,
		ByteEnd:            int64(len(raw)),
		RawSHA256:          rawSHA,
		CompressedSize:     int64(len(comp)),
		CompressedSHA256:   compSHA,
		ContentEncoding:    telemetrycontract.TrajectoryContentEncodingGzip,
	}

	doPrepareWithReq := func(t *testing.T, host, remoteAddr, xfh, xfp string, gen int) string {
		t.Helper()
		reqBody := baseReq
		reqBody.Generation = gen
		b, err := json.Marshal(reqBody)
		require.NoError(t, err)
		httpReq := httptest.NewRequest(http.MethodPost,
			telemetrycontract.TrajectorySegmentsPrepareAgentPath(agent.ID), bytes.NewReader(b))
		httpReq.Host = host
		if remoteAddr != "" {
			httpReq.RemoteAddr = remoteAddr
		}
		httpReq.Header.Set("Content-Type", "application/json")
		httpReq.Header.Set("X-Scion-Agent-Token", tok)
		if xfh != "" {
			httpReq.Header.Set("X-Forwarded-Host", xfh)
		}
		if xfp != "" {
			httpReq.Header.Set("X-Forwarded-Proto", xfp)
		}
		rec := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rec, httpReq)
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		var resp telemetrycontract.TrajectorySegmentPrepareResponse
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
		require.NotNil(t, resp.Upload)
		return resp.Upload.URL
	}

	_, trustedNet, err := net.ParseCIDR("10.0.0.0/8")
	require.NoError(t, err)

	cases := []struct {
		name          string
		hubEndpoint   string
		agentEndpoint string
		internalURL   string
		trustedNets   []*net.IPNet
		remoteAddr    string
		requestHost   string
		xfh           string
		xfp           string
		wantPrefix    string
	}{
		{
			name:        "hosted HTTPS behind TLS proxy without trusted_proxies",
			hubEndpoint: "https://hub.example.com",
			requestHost: "hub.example.com",
			xfp:         "https", // untrusted without server.trusted_proxies
			wantPrefix:  "https://hub.example.com/api/v1/agents/",
		},
		{
			name:          "AgentEndpoint HTTPS behind TLS proxy without trusted_proxies",
			hubEndpoint:   "https://hub.example.com",
			agentEndpoint: "https://agents.example.com",
			requestHost:   "agents.example.com",
			xfp:           "https",
			wantPrefix:    "https://agents.example.com/api/v1/agents/",
		},
		{
			name:        "broker-rewritten internal host against public HTTPS endpoint preserves request host",
			hubEndpoint: "https://hub.example.com",
			requestHost: "hub-internal:8080",
			xfp:         "https",
			wantPrefix:  "http://hub-internal:8080/api/v1/agents/",
		},
		{
			name:        "localhost HubEndpoint with container-bridge host preserves request host",
			hubEndpoint: "http://localhost:8080",
			requestHost: "host.docker.internal:8080",
			wantPrefix:  "http://host.docker.internal:8080/api/v1/agents/",
		},
		{
			name:        "localhost HubEndpoint with localhost request host matches configured endpoint",
			hubEndpoint: "http://localhost:8080",
			requestHost: "localhost:8080",
			wantPrefix:  "http://localhost:8080/api/v1/agents/",
		},
		{
			name:        "O-a trusted X-Forwarded-Host overrides rewritten Host even when InternalURL matches raw Host",
			hubEndpoint: "https://hub.example.com",
			internalURL: "http://scion-hub:8080",
			trustedNets: []*net.IPNet{trustedNet},
			remoteAddr:  "10.0.0.5:12345",
			requestHost: "scion-hub:8080",
			xfh:         "hub.example.com",
			xfp:         "https",
			wantPrefix:  "https://hub.example.com/api/v1/agents/",
		},
		{
			name:        "O-a untrusted X-Forwarded-Host is ignored",
			hubEndpoint: "https://hub.example.com",
			trustedNets: []*net.IPNet{trustedNet},
			remoteAddr:  "192.0.2.10:12345",
			requestHost: "hub-internal:8080",
			xfh:         "hub.example.com",
			xfp:         "https",
			wantPrefix:  "http://hub-internal:8080/api/v1/agents/",
		},
		{
			name:        "O-b candidate with explicit :443 matches request Host without port and emits request Host",
			hubEndpoint: "https://hub.example.com:443",
			requestHost: "hub.example.com",
			wantPrefix:  "https://hub.example.com/api/v1/agents/",
		},
		{
			name:        "O-b candidate without :443 matches request Host with :443 and emits request Host",
			hubEndpoint: "https://hub.example.com",
			requestHost: "hub.example.com:443",
			wantPrefix:  "https://hub.example.com:443/api/v1/agents/",
		},
		{
			name:        "O-b candidate with explicit :80 matches request Host without port and emits request Host",
			hubEndpoint: "http://hub.internal:80",
			requestHost: "hub.internal",
			wantPrefix:  "http://hub.internal/api/v1/agents/",
		},
	}

	for i, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv.config.HubEndpoint = tc.hubEndpoint
			srv.config.HubEndpoints = nil
			srv.config.AgentEndpoint = tc.agentEndpoint
			srv.config.InternalURL = tc.internalURL
			srv.transportTrustedNets = tc.trustedNets
			gotURL := doPrepareWithReq(t, tc.requestHost, tc.remoteAddr, tc.xfh, tc.xfp, i)
			assert.True(t, strings.HasPrefix(gotURL, tc.wantPrefix), "got %q, want prefix %q", gotURL, tc.wantPrefix)
		})
	}
}
