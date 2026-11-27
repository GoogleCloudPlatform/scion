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

package hub

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"path"
	"strconv"
	"strings"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/experiments"
	"github.com/GoogleCloudPlatform/scion/pkg/storage"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/telemetrycontract"
)

const (
	// mutationTypeTrajectorySegmentRead is the MutationAuditRecord.MutationType
	// recorded synchronously before issuing any signed trajectory download URL.
	mutationTypeTrajectorySegmentRead = "trajectory_segment_read"

	// trajectoryDownloadSigDomain and trajectoryDownloadSigDomainV2 domain-separate
	// Hub-proxied trajectory download HMAC capability URLs from upload URLs and
	// skill download URLs even though they share s.downloadKeys.
	trajectoryDownloadSigDomain   = "trajectory-segment-download"
	trajectoryDownloadSigDomainV2 = "trajectory-segment-download-v2"

	trajectoryDownloadKindParam    = "kind"
	trajectoryDownloadKindSegment  = "segment"
	trajectoryDownloadKindManifest = "manifest"
	trajectoryDownloadURLClockSkew = time.Minute
	trajectoryContentTypeJSON      = "application/json"
	defaultTrajectorySegmentsLimit = 100
	maxTrajectorySegmentsLimit     = 500
	trajectoryReadAuditTargetType  = "trajectory_segment"
)

func (s *Server) trajectoryNow() time.Time {
	if s.trajectoryClock != nil {
		return s.trajectoryClock().UTC()
	}
	return time.Now().UTC()
}

func (s *Server) trajectoryDownloadURLTTLDuration() time.Duration {
	if s.trajectoryDownloadURLExpiry > 0 {
		return s.trajectoryDownloadURLExpiry
	}
	return telemetrycontract.DefaultTrajectoryDownloadURLExpiry
}

func computeTrajectoryDownloadHMAC(key []byte, agentID, segmentID, kind, objectPath string, exp int64) []byte {
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(trajectoryDownloadSigDomain + "\n" + agentID + "\n" + segmentID + "\n" +
		kind + "\n" + objectPath + "\n" + strconv.FormatInt(exp, 10)))
	return mac.Sum(nil)
}

func computeTrajectoryDownloadHMACv2(key []byte, kid, agentID, segmentID, kind, objectPath string, exp int64) []byte {
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(trajectoryDownloadSigDomainV2 + "\n" + kid + "\n" + agentID + "\n" + segmentID + "\n" +
		kind + "\n" + objectPath + "\n" + strconv.FormatInt(exp, 10)))
	return mac.Sum(nil)
}

// signTrajectoryDownloadURL builds the HMAC capability query string for
// GET /api/v1/agents/{id}/trajectory/segments/{segment_id}/content.
func (s *Server) signTrajectoryDownloadURL(agentID, segmentID, kind, objectPath string, expiry time.Time) (string, error) {
	if !s.downloadKeys.configured() {
		return "", errors.New("capability signing key is not configured")
	}
	key, versioned, err := s.downloadKeys.issue()
	if err != nil {
		return "", fmt.Errorf("issue capability signing key: %w", err)
	}
	exp := expiry.Unix()
	prefix := ""
	if kind != "" && kind != trajectoryDownloadKindSegment {
		prefix = trajectoryDownloadKindParam + "=" + url.QueryEscape(kind) + "&"
	}
	expPart := skillFileExpParam + "=" + strconv.FormatInt(exp, 10) + "&"
	if versioned {
		sig := base64.RawURLEncoding.EncodeToString(
			computeTrajectoryDownloadHMACv2(key.Bytes(), key.ID, agentID, segmentID, kind, objectPath, exp))
		return prefix + skillFileVersionParam + "=" + skillFileVersion2 + "&" +
			skillFileKeyIDParam + "=" + url.QueryEscape(key.ID) + "&" +
			expPart + skillFileSigParam + "=" + url.QueryEscape(sig), nil
	}
	sig := base64.RawURLEncoding.EncodeToString(
		computeTrajectoryDownloadHMAC(key.Bytes(), agentID, segmentID, kind, objectPath, exp))
	return prefix + expPart + skillFileSigParam + "=" + url.QueryEscape(sig), nil
}

// verifyTrajectoryDownloadSignature validates the HMAC capability query
// parameters on GET /api/v1/agents/{id}/trajectory/segments/{segment_id}/content.
func (s *Server) verifyTrajectoryDownloadSignature(r *http.Request, agentID, segmentID, kind, objectPath string, now time.Time) bool {
	if !s.downloadKeys.configured() {
		return false
	}
	q := r.URL.Query()
	if len(q[skillFileSigParam]) != 1 || len(q[skillFileExpParam]) != 1 {
		return false
	}
	kv, hasKV := q[skillFileVersionParam]
	kids, hasKid := q[skillFileKeyIDParam]
	kid := ""
	switch {
	case !hasKV && !hasKid:
	case len(kv) == 1 && len(kids) == 1 && kv[0] == skillFileVersion2 && capabilityKeyIDPattern.MatchString(kids[0]):
		kid = kids[0]
	default:
		return false
	}
	expStr := q.Get(skillFileExpParam)
	exp, err := strconv.ParseInt(expStr, 10, 64)
	if err != nil || strconv.FormatInt(exp, 10) != expStr {
		return false
	}
	nowUnix := now.Unix()
	if nowUnix > exp {
		return false
	}
	maxTTL := s.trajectoryDownloadURLTTLDuration() + trajectoryDownloadURLClockSkew
	if exp > nowUnix+int64(maxTTL/time.Second) {
		return false
	}
	got, err := base64.RawURLEncoding.Strict().DecodeString(q.Get(skillFileSigParam))
	if err != nil {
		return false
	}
	for _, k := range s.downloadKeys.candidates(kid) {
		var want []byte
		if kid == "" {
			want = computeTrajectoryDownloadHMAC(k.Bytes(), agentID, segmentID, kind, objectPath, exp)
		} else {
			want = computeTrajectoryDownloadHMACv2(k.Bytes(), kid, agentID, segmentID, kind, objectPath, exp)
		}
		if hmac.Equal(got, want) {
			return true
		}
	}
	return false
}

// isSignedTrajectoryContentRequest reports whether r has the shape of a
// capability-URL trajectory segment content download: a credential-less GET of
// /api/v1/agents/{id}/trajectory/segments/{segmentId}/content carrying exp and
// sig query parameters. UnifiedAuthMiddleware lets such requests through
// without an identity; handleAgentTrajectorySegmentContentRead verifies the
// HMAC signature unconditionally before serving any bytes.
func isSignedTrajectoryContentRequest(r *http.Request) bool {
	if r.Method != http.MethodGet {
		return false
	}
	rest, ok := strings.CutPrefix(r.URL.Path, "/api/v1/agents/")
	if !ok {
		return false
	}
	if path.Clean(r.URL.Path) != r.URL.Path {
		return false
	}
	parts := strings.Split(rest, "/")
	if len(parts) != 5 || parts[0] == "" || parts[1] != "trajectory" || parts[2] != "segments" || parts[3] == "" || parts[4] != "content" {
		return false
	}
	if !strings.Contains(r.URL.RawQuery, "sig=") || !strings.Contains(r.URL.RawQuery, "exp=") {
		return false
	}
	q := r.URL.Query()
	return q.Get(skillFileSigParam) != "" && q.Get(skillFileExpParam) != ""
}

// requireUserTrajectoryReadAuth rejects unauthenticated callers (401) and
// agent-token callers (403). Agents must never read their own raw audit trail
// through the user-facing trajectory read API.
func requireUserTrajectoryReadAuth(w http.ResponseWriter, r *http.Request) bool {
	ctx := r.Context()
	if GetAgentIdentityFromContext(ctx) != nil {
		writeError(w, http.StatusForbidden, ErrCodeForbidden,
			"Agent tokens cannot access trajectory read endpoints", nil)
		return false
	}
	if GetIdentityFromContext(ctx) == nil {
		Unauthorized(w)
		return false
	}
	return true
}

// authorizeAgentTrajectoryRead checks that the authenticated user principal
// holds project.trajectory.read on agent's project. Never bundled into
// agent.read; cross-project callers are refused with 403.
func (s *Server) authorizeAgentTrajectoryRead(w http.ResponseWriter, r *http.Request, agent *store.Agent) bool {
	ctx := r.Context()
	if agent == nil || agent.ProjectID == "" {
		writeError(w, http.StatusForbidden, ErrCodeForbidden,
			"Access denied: project.trajectory.read is required", nil)
		return false
	}
	decision := s.authzService.DecideFromContext(ctx, Resource{
		Type: "project",
		ID:   agent.ProjectID,
	}, ActionTrajectoryRead)
	if !decision.Allowed {
		writeError(w, http.StatusForbidden, ErrCodeForbidden,
			"Access denied: project.trajectory.read is required", nil)
		return false
	}
	return true
}

func encodeTrajectoryListCursor(offset int) string {
	return base64.RawURLEncoding.EncodeToString([]byte(strconv.Itoa(offset)))
}

func decodeTrajectoryListCursor(cursor string) (int, error) {
	if cursor == "" {
		return 0, nil
	}
	raw, err := base64.RawURLEncoding.DecodeString(cursor)
	if err != nil {
		return 0, fmt.Errorf("invalid cursor")
	}
	n, err := strconv.Atoi(string(raw))
	if err != nil || n < 0 {
		return 0, fmt.Errorf("invalid cursor")
	}
	return n, nil
}

// handleAgentTrajectorySegmentsList serves
// GET /api/v1/agents/{id}/trajectory/segments.
//
// Returns committed segment metadata only (never raw content). Listing does
// not write a read-audit row: the UI invokes this endpoint whenever an
// authorized user opens the Trajectory tab or pages/filters, and no verbatim
// conversation bytes leave the Hub; mandatory read-audit rows are written on
// every signed download (design §5.5).
func (s *Server) handleAgentTrajectorySegmentsList(w http.ResponseWriter, r *http.Request, id string) {
	if !s.experimentEnabled(experiments.TokenomicsTrajectoryCapture) {
		NotFound(w, "route")
		return
	}
	if r.Method != http.MethodGet {
		MethodNotAllowed(w, http.MethodGet)
		return
	}
	if !requireUserTrajectoryReadAuth(w, r) {
		return
	}

	ctx := r.Context()
	agent, err := s.store.GetAgent(ctx, id)
	if err != nil {
		writeErrorFromErr(w, err, "")
		return
	}
	if agent == nil {
		NotFound(w, "Agent")
		return
	}
	if !s.authorizeAgentTrajectoryRead(w, r, agent) {
		return
	}

	q := r.URL.Query()
	filter := store.TrajectorySegmentListFilter{
		AgentID:            agent.ID,
		RunID:              strings.TrimSpace(q.Get("run_id")),
		NativeRelativePath: strings.TrimSpace(q.Get("native_relative_path")),
		Limit:              defaultTrajectorySegmentsLimit,
	}

	if genStr := strings.TrimSpace(q.Get("generation")); genStr != "" {
		gen, err := strconv.Atoi(genStr)
		if err != nil || gen < 0 {
			ValidationError(w, "generation must be a non-negative integer", nil)
			return
		}
		filter.Generation = &gen
	}

	if limitStr := strings.TrimSpace(q.Get("limit")); limitStr != "" {
		limit, err := strconv.Atoi(limitStr)
		if err != nil || limit < 1 || limit > maxTrajectorySegmentsLimit {
			ValidationError(w, fmt.Sprintf("limit must be between 1 and %d", maxTrajectorySegmentsLimit), nil)
			return
		}
		filter.Limit = limit
	}

	if cursorStr := strings.TrimSpace(q.Get("cursor")); cursorStr != "" {
		offset, err := decodeTrajectoryListCursor(cursorStr)
		if err != nil {
			ValidationError(w, "invalid cursor", nil)
			return
		}
		filter.Offset = offset
	}

	segments, hasMore, err := s.store.ListCommittedTrajectorySegments(ctx, filter)
	if err != nil {
		writeErrorFromErr(w, err, "")
		return
	}

	summaries := make([]telemetrycontract.TrajectorySegmentSummary, 0, len(segments))
	for _, seg := range segments {
		if seg == nil {
			continue
		}
		committedAt := ""
		if seg.CommittedAt != nil {
			committedAt = seg.CommittedAt.UTC().Format(time.RFC3339Nano)
		}
		summaries = append(summaries, telemetrycontract.TrajectorySegmentSummary{
			SegmentID:          seg.ID,
			RunID:              seg.RunID,
			Harness:            seg.Harness,
			NativeRelativePath: seg.NativeRelativePath,
			ArtifactKind:       seg.ArtifactKind,
			Generation:         seg.Generation,
			ByteStart:          seg.ByteStart,
			ByteEnd:            seg.ByteEnd,
			RawSize:            seg.ByteEnd - seg.ByteStart,
			CompressedSize:     seg.CompressedSize,
			RawSHA256:          seg.RawSHA256,
			CompressedSHA256:   seg.CompressedSHA256,
			IncompleteTail:     seg.IncompleteTail,
			SessionID:          seg.SessionID,
			CommittedAt:        committedAt,
		})
	}

	resp := telemetrycontract.TrajectorySegmentsListResponse{
		AgentID:   agent.ID,
		ProjectID: agent.ProjectID,
		Segments:  summaries,
	}
	if hasMore {
		resp.NextCursor = encodeTrajectoryListCursor(filter.Offset + len(segments))
	}
	writeJSON(w, http.StatusOK, resp)
}

type trajectoryReadAuditSummary struct {
	ProjectID       string `json:"project_id"`
	AgentID         string `json:"agent_id"`
	RunID           string `json:"run_id"`
	SegmentID       string `json:"segment_id"`
	ObjectPath      string `json:"object_path"`
	ManifestPath    string `json:"manifest_path,omitempty"`
	IncludeManifest bool   `json:"include_manifest,omitempty"`
}

// handleAgentTrajectorySegmentDownload serves
// GET /api/v1/agents/{id}/trajectory/segments/{segment_id}/download.
//
// Writes a mandatory read-audit row synchronously BEFORE signing or returning
// any download URL. If the audit write fails, the request fails and no URL is
// issued.
func (s *Server) handleAgentTrajectorySegmentDownload(w http.ResponseWriter, r *http.Request, id, segmentID string) {
	if !s.experimentEnabled(experiments.TokenomicsTrajectoryCapture) {
		NotFound(w, "route")
		return
	}
	if r.Method != http.MethodGet {
		MethodNotAllowed(w, http.MethodGet)
		return
	}
	if !requireUserTrajectoryReadAuth(w, r) {
		return
	}

	ctx := r.Context()
	agent, err := s.store.GetAgent(ctx, id)
	if err != nil {
		writeErrorFromErr(w, err, "")
		return
	}
	if agent == nil {
		NotFound(w, "Agent")
		return
	}
	if !s.authorizeAgentTrajectoryRead(w, r, agent) {
		return
	}

	seg, err := s.store.GetTrajectorySegment(ctx, segmentID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			NotFound(w, "Trajectory segment")
			return
		}
		writeErrorFromErr(w, err, "")
		return
	}
	if seg == nil || seg.AgentID != agent.ID || seg.ProjectID != agent.ProjectID || seg.Status != telemetrycontract.TrajectorySegmentStatusCommitted {
		NotFound(w, "Trajectory segment")
		return
	}

	stor := s.GetStorage()
	if stor == nil {
		RuntimeError(w, "Hub storage is not configured")
		return
	}

	incParam := strings.TrimSpace(strings.ToLower(r.URL.Query().Get("include_manifest")))
	includeManifest := (incParam == "true" || incParam == "1") && seg.ManifestPath != ""

	now := s.trajectoryNow().Truncate(time.Microsecond)
	auditSummary := trajectoryReadAuditSummary{
		ProjectID:       seg.ProjectID,
		AgentID:         seg.AgentID,
		RunID:           seg.RunID,
		SegmentID:       seg.ID,
		ObjectPath:      seg.ObjectPath,
		IncludeManifest: includeManifest,
	}
	if includeManifest {
		auditSummary.ManifestPath = seg.ManifestPath
	}
	summaryBytes, err := json.Marshal(auditSummary)
	if err != nil {
		RuntimeError(w, "Failed to serialize trajectory read audit record")
		return
	}

	auditRecord := &store.MutationAuditRecord{
		Timestamp:    now,
		MutationType: mutationTypeTrajectorySegmentRead,
		TargetType:   trajectoryReadAuditTargetType,
		TargetID:     seg.ID,
		AfterSummary: string(summaryBytes),
	}
	auditActorFromContext(ctx).ApplyActor(auditRecord)
	if err := s.store.CreateMutationAudit(ctx, auditRecord); err != nil {
		slog.ErrorContext(ctx, "trajectory segment download: mandatory read-audit write failed",
			"agent_id", seg.AgentID, "project_id", seg.ProjectID, "segment_id", seg.ID, "error", err)
		writeError(w, http.StatusInternalServerError, ErrCodeInternalError,
			"Failed to record trajectory read audit", nil)
		return
	}

	segLink, err := s.buildTrajectoryDownloadLink(ctx, r, stor, seg, trajectoryDownloadKindSegment, seg.ObjectPath, trajectoryContentTypeGzip, now)
	if err != nil {
		slog.ErrorContext(ctx, "trajectory segment download: failed to issue signed URL",
			"agent_id", seg.AgentID, "segment_id", seg.ID, "provider", stor.Provider(), "error", err)
		RuntimeError(w, "Failed to issue trajectory segment download URL")
		return
	}

	resp := telemetrycontract.TrajectorySegmentDownloadResponse{
		SegmentID:        seg.ID,
		ObjectPath:       segLink.ObjectPath,
		URL:              segLink.URL,
		Method:           segLink.Method,
		ExpiresAt:        segLink.ExpiresAt,
		ContentType:      segLink.ContentType,
		CompressedSize:   seg.CompressedSize,
		RawSHA256:        seg.RawSHA256,
		CompressedSHA256: seg.CompressedSHA256,
	}

	if includeManifest {
		manifestLink, err := s.buildTrajectoryDownloadLink(ctx, r, stor, seg, trajectoryDownloadKindManifest, seg.ManifestPath, trajectoryContentTypeJSON, now)
		if err != nil {
			slog.ErrorContext(ctx, "trajectory segment download: failed to issue signed manifest URL",
				"agent_id", seg.AgentID, "segment_id", seg.ID, "provider", stor.Provider(), "error", err)
			RuntimeError(w, "Failed to issue trajectory manifest download URL")
			return
		}
		resp.Manifest = manifestLink
	}

	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, resp)
}

func trajectoryDownloadFilename(seg *store.TrajectorySegment, kind string) string {
	base := fmt.Sprintf("g%d-%016d.seg.gz", seg.Generation, seg.ByteStart)
	if kind == trajectoryDownloadKindManifest {
		return base + ".manifest.json"
	}
	return base
}

// buildTrajectoryDownloadLink produces a short-lived GCS V4 signed GET URL or
// a Hub-proxied HMAC capability GET URL (for ProviderLocal or GCS without
// signBlob permission).
func (s *Server) buildTrajectoryDownloadLink(
	ctx context.Context,
	r *http.Request,
	stor storage.Storage,
	seg *store.TrajectorySegment,
	kind, objectPath, contentType string,
	now time.Time,
) (*telemetrycontract.TrajectorySegmentDownloadLink, error) {
	ttl := s.trajectoryDownloadURLTTLDuration()
	expiresAt := now.Add(ttl)
	disposition := fmt.Sprintf("attachment; filename=%q", trajectoryDownloadFilename(seg, kind))

	if stor.Provider() == storage.ProviderGCS {
		signed, err := stor.GenerateSignedURL(ctx, objectPath, storage.SignedURLOptions{
			Method:                     http.MethodGet,
			Expires:                    ttl,
			ResponseContentType:        contentType,
			ResponseContentDisposition: disposition,
		})
		if err == nil && signed != nil && signed.URL != "" {
			exp := signed.Expires
			if exp.IsZero() {
				exp = expiresAt
			}
			return &telemetrycontract.TrajectorySegmentDownloadLink{
				ObjectPath:  objectPath,
				URL:         signed.URL,
				Method:      http.MethodGet,
				ExpiresAt:   exp.UTC().Format(time.RFC3339),
				ContentType: contentType,
			}, nil
		}
		slog.WarnContext(ctx, "trajectory segment download: GCS signed GET unavailable; falling back to Hub-proxied HMAC URL",
			"agent_id", seg.AgentID, "segment_id", seg.ID, "kind", kind, "error", err)
	}

	sigQuery, err := s.signTrajectoryDownloadURL(seg.AgentID, seg.ID, kind, objectPath, expiresAt)
	if err != nil {
		return nil, err
	}
	downloadURL := s.requestBaseURL(r) + telemetrycontract.TrajectorySegmentContentAgentPath(seg.AgentID, seg.ID) + "?" + sigQuery
	return &telemetrycontract.TrajectorySegmentDownloadLink{
		ObjectPath:  objectPath,
		URL:         downloadURL,
		Method:      http.MethodGet,
		ExpiresAt:   expiresAt.UTC().Format(time.RFC3339),
		ContentType: contentType,
	}, nil
}

// handleAgentTrajectorySegmentContentRead serves
// GET /api/v1/agents/{id}/trajectory/segments/{segment_id}/content.
//
// Authorized exclusively by the short-lived HMAC capability URL issued by the
// audit-logged download endpoint. Unsigned requests (even with a valid user or
// agent bearer token) are rejected so downloads cannot bypass read-audit.
func (s *Server) handleAgentTrajectorySegmentContentRead(w http.ResponseWriter, r *http.Request, id, segmentID string) {
	if !s.experimentEnabled(experiments.TokenomicsTrajectoryCapture) {
		NotFound(w, "route")
		return
	}
	if r.Method != http.MethodGet {
		MethodNotAllowed(w, http.MethodGet, http.MethodPut)
		return
	}

	ctx := r.Context()
	seg, err := s.store.GetTrajectorySegment(ctx, segmentID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			NotFound(w, "Trajectory segment")
			return
		}
		writeErrorFromErr(w, err, "")
		return
	}
	if seg == nil || seg.AgentID != id || seg.Status != telemetrycontract.TrajectorySegmentStatusCommitted {
		NotFound(w, "Trajectory segment")
		return
	}

	kind := strings.TrimSpace(r.URL.Query().Get(trajectoryDownloadKindParam))
	if kind == "" {
		kind = trajectoryDownloadKindSegment
	}
	var objectPath, contentType string
	switch kind {
	case trajectoryDownloadKindSegment:
		objectPath = seg.ObjectPath
		contentType = trajectoryContentTypeGzip
	case trajectoryDownloadKindManifest:
		if seg.ManifestPath == "" {
			NotFound(w, "Trajectory segment manifest")
			return
		}
		objectPath = seg.ManifestPath
		contentType = trajectoryContentTypeJSON
	default:
		writeError(w, http.StatusForbidden, ErrCodeForbidden,
			"Invalid trajectory download kind", nil)
		return
	}

	if !s.verifyTrajectoryDownloadSignature(r, id, seg.ID, kind, objectPath, s.trajectoryNow()) {
		writeError(w, http.StatusForbidden, ErrCodeForbidden,
			"Invalid or expired trajectory download signature", nil)
		return
	}

	stor := s.GetStorage()
	if stor == nil {
		RuntimeError(w, "Hub storage is not configured")
		return
	}

	rc, _, err := stor.Download(ctx, objectPath)
	if err != nil {
		if errors.Is(err, storage.ErrNotFound) {
			NotFound(w, "Trajectory segment content")
			return
		}
		slog.ErrorContext(ctx, "trajectory segment content download failed",
			"agent_id", id, "segment_id", seg.ID, "kind", kind, "error", err)
		RuntimeError(w, "Failed to read trajectory segment content")
		return
	}
	defer func() { _ = rc.Close() }()

	w.Header().Set("Content-Type", contentType)
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", trajectoryDownloadFilename(seg, kind)))
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(http.StatusOK)
	_, _ = io.Copy(w, rc)
}
