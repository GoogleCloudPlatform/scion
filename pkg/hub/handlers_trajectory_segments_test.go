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

	tok, err := srv.agentTokenService.GenerateAgentToken(agent.ID, proj.ID, []AgentTokenScope{ScopeAgentStatusUpdate}, nil)
	require.NoError(t, err)

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

	// 2. Upload via Hub-proxied PUT
	putRec := doTrajectoryAgentPut(t, srv, prepResp.Upload.URL, tok, comp, prepResp.Upload.Headers)
	require.Equal(t, http.StatusOK, putRec.Code, putRec.Body.String())

	// 2b. No-overwrite: identical bytes -> 200; different bytes -> 412.
	putIdentical := doTrajectoryAgentPut(t, srv, prepResp.Upload.URL, tok, comp, prepResp.Upload.Headers)
	require.Equal(t, http.StatusOK, putIdentical.Code, putIdentical.Body.String())

	diffComp := append([]byte(nil), comp...)
	diffComp[len(diffComp)-1] ^= 0xff
	putDiff := doTrajectoryAgentPut(t, srv, prepResp.Upload.URL, tok, diffComp, prepResp.Upload.Headers)
	require.Equal(t, http.StatusPreconditionFailed, putDiff.Code, putDiff.Body.String())

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
	otherTok, err := srv.agentTokenService.GenerateAgentToken(otherAgent.ID, proj.ID, []AgentTokenScope{ScopeAgentStatusUpdate}, nil)
	require.NoError(t, err)

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

	// Per-run cap -> 413 run_cap_exceeded.
	srv.trajectoryMaxRunRawBytes = int64(len(raw))
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
