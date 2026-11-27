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
	"bytes"
	"compress/gzip"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/GoogleCloudPlatform/scion/pkg/experiments"
	"github.com/GoogleCloudPlatform/scion/pkg/storage"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/telemetrycontract"
)

const (
	// trajectorySegmentRequestMaxBody bounds JSON request bodies for
	// segments:prepare and segments:commit.
	trajectorySegmentRequestMaxBody = 64 * 1024

	// trajectoryUploadSigDomain and trajectoryUploadSigDomainV2 domain-separate
	// Hub-proxied trajectory upload HMAC capability URLs from skill download
	// URLs even though both use s.downloadKeys.
	trajectoryUploadSigDomain   = "trajectory-segment-upload"
	trajectoryUploadSigDomainV2 = "trajectory-segment-upload-v2"

	trajectoryUploadURLMaxClockSkew = time.Minute

	trajectoryContentTypeGzip = "application/gzip"

	// trajectoryMetadataFieldMaxLen bounds client-supplied provenance/metadata
	// fields (session_id, origin_hub_id) persisted into the DB row and manifest.
	trajectoryMetadataFieldMaxLen = 256
)

var (
	errTrajectoryUploadSizeMismatch = errors.New("uploaded size does not match expected compressed_size")
	errTrajectoryUploadHashMismatch = errors.New("uploaded compressed_sha256 does not match prepared segment")
)

func (s *Server) maxTrajectorySegmentRawBytes() int64 {
	if s.trajectoryMaxSegmentRawBytes > 0 {
		return s.trajectoryMaxSegmentRawBytes
	}
	return telemetrycontract.DefaultTrajectoryMaxSegmentRawBytes
}

func (s *Server) maxTrajectoryRunRawBytes() int64 {
	if s.trajectoryMaxRunRawBytes > 0 {
		return s.trajectoryMaxRunRawBytes
	}
	return telemetrycontract.DefaultTrajectoryMaxRunRawBytes
}

func (s *Server) trajectoryPrepareTTLDuration() time.Duration {
	if s.trajectoryPrepareTTL > 0 {
		return s.trajectoryPrepareTTL
	}
	return telemetrycontract.DefaultTrajectoryPrepareTTL
}

func (s *Server) trajectoryUploadURLTTLDuration() time.Duration {
	if s.trajectoryUploadURLExpiry > 0 {
		return s.trajectoryUploadURLExpiry
	}
	return telemetrycontract.DefaultTrajectoryUploadURLExpiry
}

// effectiveTrajectoryCapturePolicy returns the effective Track 2 capture policy
// for project ("full", "metadata_only", or "off"). In P5 wave 1 this defaults
// to "full"; a later slice wires project settings and the Hub opt-out lock.
func (s *Server) effectiveTrajectoryCapturePolicy(ctx context.Context, project *store.Project) string {
	if s.trajectoryCapturePolicyOverride != nil {
		return s.trajectoryCapturePolicyOverride(ctx, project)
	}
	return telemetrycontract.TrajectoryCapturePolicyFull
}

// isValidHexSHA256 reports whether s is a 64-character lowercase hex SHA-256 digest.
func isValidHexSHA256(s string) bool {
	if len(s) != 64 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

func computeTrajectoryUploadHMAC(key []byte, agentID, segmentID string, compressedSize int64, compressedSHA256 string, exp int64) []byte {
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(trajectoryUploadSigDomain + "\n" + agentID + "\n" + segmentID + "\n" +
		strconv.FormatInt(compressedSize, 10) + "\n" + compressedSHA256 + "\n" + strconv.FormatInt(exp, 10)))
	return mac.Sum(nil)
}

func computeTrajectoryUploadHMACv2(key []byte, kid, agentID, segmentID string, compressedSize int64, compressedSHA256 string, exp int64) []byte {
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(trajectoryUploadSigDomainV2 + "\n" + kid + "\n" + agentID + "\n" + segmentID + "\n" +
		strconv.FormatInt(compressedSize, 10) + "\n" + compressedSHA256 + "\n" + strconv.FormatInt(exp, 10)))
	return mac.Sum(nil)
}

// signTrajectoryUploadURL builds the HMAC capability query string for
// PUT /api/v1/agents/{id}/trajectory/segments/{segment_id}/content.
func (s *Server) signTrajectoryUploadURL(agentID, segmentID string, compressedSize int64, compressedSHA256 string, expiry time.Time) (string, error) {
	if !s.downloadKeys.configured() {
		return "", errors.New("capability signing key is not configured")
	}
	key, versioned, err := s.downloadKeys.issue()
	if err != nil {
		return "", fmt.Errorf("issue capability signing key: %w", err)
	}
	exp := expiry.Unix()
	expPart := skillFileExpParam + "=" + strconv.FormatInt(exp, 10) + "&"
	if versioned {
		sig := base64.RawURLEncoding.EncodeToString(
			computeTrajectoryUploadHMACv2(key.Bytes(), key.ID, agentID, segmentID, compressedSize, compressedSHA256, exp))
		return skillFileVersionParam + "=" + skillFileVersion2 + "&" +
			skillFileKeyIDParam + "=" + url.QueryEscape(key.ID) + "&" +
			expPart + skillFileSigParam + "=" + url.QueryEscape(sig), nil
	}
	sig := base64.RawURLEncoding.EncodeToString(
		computeTrajectoryUploadHMAC(key.Bytes(), agentID, segmentID, compressedSize, compressedSHA256, exp))
	return expPart + skillFileSigParam + "=" + url.QueryEscape(sig), nil
}

// verifyTrajectoryUploadSignature validates the capability query parameters on
// PUT /api/v1/agents/{id}/trajectory/segments/{segment_id}/content.
func (s *Server) verifyTrajectoryUploadSignature(r *http.Request, agentID, segmentID string, compressedSize int64, compressedSHA256 string, now time.Time) bool {
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
	maxTTL := s.trajectoryUploadURLTTLDuration() + trajectoryUploadURLMaxClockSkew
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
			want = computeTrajectoryUploadHMAC(k.Bytes(), agentID, segmentID, compressedSize, compressedSHA256, exp)
		} else {
			want = computeTrajectoryUploadHMACv2(k.Bytes(), kid, agentID, segmentID, compressedSize, compressedSHA256, exp)
		}
		if hmac.Equal(got, want) {
			return true
		}
	}
	return false
}

// resolveTrajectoryRunAttribution resolves the Hub-frozen run_attribution row
// for the agent's run (claim from agent JWT, falling back to agent.RunID),
// lazily creating it for the live agent's current run_id just like usage ingest.
func (s *Server) resolveTrajectoryRunAttribution(ctx context.Context, agent *store.Agent, tokenRunID string) (*store.RunAttribution, error) {
	runID := strings.TrimSpace(tokenRunID)
	if runID == "" && agent != nil {
		runID = strings.TrimSpace(agent.RunID)
	}
	if runID == "" {
		return nil, store.ErrNotFound
	}
	attr, err := s.store.GetRunAttribution(ctx, runID)
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		return nil, err
	}
	if attr == nil && agent != nil && agent.DeletedAt.IsZero() && agent.RunID != "" && runID == agent.RunID {
		attr, err = s.store.EnsureRunAttributionForAgent(ctx, agent.ID, runID, s.usageLedgerHubID())
		if err != nil && !errors.Is(err, store.ErrNotFound) {
			return nil, err
		}
	}
	if attr == nil {
		return nil, store.ErrNotFound
	}
	if attr.AgentID != agent.ID {
		return nil, store.ErrNotFound
	}
	return attr, nil
}

// handleAgentTrajectorySegmentsPrepare serves
// POST /api/v1/agents/{id}/trajectory/segments:prepare.
func (s *Server) handleAgentTrajectorySegmentsPrepare(w http.ResponseWriter, r *http.Request, id string) {
	if !s.experimentEnabled(experiments.TokenomicsTrajectoryCapture) {
		NotFound(w, "route")
		return
	}
	if r.Method != http.MethodPost {
		MethodNotAllowed(w, http.MethodPost)
		return
	}

	ctx := r.Context()
	agentIdent := GetAgentIdentityFromContext(ctx)
	if agentIdent == nil {
		Unauthorized(w)
		return
	}
	if agentIdent.ID() != id {
		writeError(w, http.StatusForbidden, ErrCodeForbidden,
			"Agents can only prepare their own trajectory segments", nil)
		return
	}
	if s.agentStandingForbidden(ctx, w, id) {
		return
	}

	agent, err := s.store.GetAgent(ctx, id)
	if err != nil {
		writeErrorFromErr(w, err, "")
		return
	}
	if agent == nil {
		NotFound(w, "Agent")
		return
	}

	project, err := s.store.GetProject(ctx, agent.ProjectID)
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		writeErrorFromErr(w, err, "")
		return
	}

	switch s.effectiveTrajectoryCapturePolicy(ctx, project) {
	case telemetrycontract.TrajectoryCapturePolicyOff:
		writeError(w, http.StatusForbidden, telemetrycontract.TrajectoryErrCaptureDisabled,
			"Trajectory capture is disabled by policy", nil)
		return
	case telemetrycontract.TrajectoryCapturePolicyMetadataOnly:
		writeError(w, http.StatusForbidden, telemetrycontract.TrajectoryErrCaptureMetadataOnly,
			"Trajectory capture is restricted to metadata only", nil)
		return
	case telemetrycontract.TrajectoryCapturePolicyFull:
		// Allowed.
	default:
		writeError(w, http.StatusForbidden, telemetrycontract.TrajectoryErrCaptureDisabled,
			"Trajectory capture is disabled by policy", nil)
		return
	}

	body, err := io.ReadAll(io.LimitReader(r.Body, trajectorySegmentRequestMaxBody+1))
	if err != nil {
		writeError(w, http.StatusBadRequest, telemetrycontract.TrajectoryErrInvalidSegment,
			"Failed to read request body", nil)
		return
	}
	if len(body) > trajectorySegmentRequestMaxBody {
		writeError(w, http.StatusBadRequest, telemetrycontract.TrajectoryErrInvalidSegment,
			"Request body exceeds maximum size", nil)
		return
	}

	var req telemetrycontract.TrajectorySegmentPrepareRequest
	if err := json.Unmarshal(body, &req); err != nil {
		writeError(w, http.StatusBadRequest, telemetrycontract.TrajectoryErrInvalidSegment,
			"Invalid JSON request body", nil)
		return
	}

	if err := validateTrajectoryPrepareRequest(&req); err != nil {
		writeError(w, http.StatusBadRequest, telemetrycontract.TrajectoryErrInvalidSegment, err.Error(), nil)
		return
	}

	rawSize := req.ByteEnd - req.ByteStart
	maxSegRaw := s.maxTrajectorySegmentRawBytes()
	if rawSize > maxSegRaw {
		writeError(w, http.StatusRequestEntityTooLarge, telemetrycontract.TrajectoryErrSegmentTooLarge,
			fmt.Sprintf("raw segment size %d exceeds maximum %d bytes", rawSize, maxSegRaw), nil)
		return
	}
	// Compressed payload also cannot exceed the raw segment cap + generous gzip framing overhead.
	maxCompSize := maxSegRaw + 64*1024
	if req.CompressedSize > maxCompSize {
		writeError(w, http.StatusRequestEntityTooLarge, telemetrycontract.TrajectoryErrSegmentTooLarge,
			fmt.Sprintf("compressed segment size %d exceeds maximum %d bytes", req.CompressedSize, maxCompSize), nil)
		return
	}

	attr, err := s.resolveTrajectoryRunAttribution(ctx, agent, presentedAgentTokenRunID(ctx))
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, http.StatusBadRequest, telemetrycontract.TrajectoryErrInvalidSegment,
				"unable to resolve run attribution for agent", nil)
			return
		}
		writeErrorFromErr(w, err, "")
		return
	}

	stor := s.GetStorage()
	if stor == nil {
		RuntimeError(w, "Hub storage is not configured")
		return
	}

	now := time.Now().UTC().Truncate(time.Microsecond)
	hubID := s.usageLedgerHubID()
	originHubID := strings.TrimSpace(req.OriginHubID)
	if originHubID == "" {
		originHubID = hubID
	} else if originHubID != hubID {
		// P5 wave 1 has no relocation records (P6). Reject any client-supplied
		// origin_hub_id other than this Hub's ID so Hub-authored manifests
		// never carry unverified foreign provenance.
		writeError(w, http.StatusBadRequest, telemetrycontract.TrajectoryErrInvalidSegment,
			"origin_hub_id must be empty or match this Hub's ID", nil)
		return
	}

	// Check existing segment first so idempotent replays of already-committed
	// segments succeed even if the run later hit the cap, and so re-prepares
	// of pending segments don't double-count themselves against the run cap.
	existing, err := s.store.GetTrajectorySegmentByIdentity(ctx, store.TrajectorySegmentIdentity{
		AgentID:            attr.AgentID,
		RunID:              attr.RunID,
		Harness:            req.Harness,
		NativeRelativePath: req.NativeRelativePath,
		Generation:         req.Generation,
		ByteStart:          req.ByteStart,
	})
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		writeErrorFromErr(w, err, "")
		return
	}
	if existing != nil && existing.Status == telemetrycontract.TrajectorySegmentStatusCommitted {
		if existing.RawSHA256 == req.RawSHA256 && existing.ByteEnd == req.ByteEnd {
			writeJSON(w, http.StatusOK, telemetrycontract.TrajectorySegmentPrepareResponse{
				SegmentID:  existing.ID,
				State:      telemetrycontract.TrajectorySegmentStateCommitted,
				ObjectPath: existing.ObjectPath,
			})
			return
		}
		writeError(w, http.StatusConflict, telemetrycontract.TrajectoryErrSegmentConflict,
			"segment identity is already committed with different content", nil)
		return
	}

	excludeID := ""
	if existing != nil {
		excludeID = existing.ID
	}
	// Enforce per-run raw byte cap across all committed and pending rows
	// (including expired pending rows) to bound orphaned uncommitted object
	// storage per run until a retention sweep reclaims them. Re-prepares of
	// the same identity (e.g. retries or shipper capture probes) reuse the
	// existing row and pass its ID as excludeID so they do not double-count.
	// Note: this check-then-insert accounting is not serialized per run in
	// wave 1, so concurrent prepares for the same run can overshoot
	// maxTrajectoryRunRawBytes by up to concurrency * maxSegmentRawBytes.
	usedRunRaw, err := s.store.SumTrajectoryRunRawBytes(ctx, attr.AgentID, attr.RunID, excludeID)
	if err != nil {
		writeErrorFromErr(w, err, "")
		return
	}
	if usedRunRaw+rawSize > s.maxTrajectoryRunRawBytes() {
		writeError(w, http.StatusRequestEntityTooLarge, telemetrycontract.TrajectoryErrRunCapExceeded,
			fmt.Sprintf("run raw trajectory cap of %d bytes exceeded", s.maxTrajectoryRunRawBytes()), nil)
		return
	}

	objectPath := telemetrycontract.TrajectoryRawObjectPath(
		hubID, attr.ProjectID, attr.AgentID, attr.RunID,
		req.Harness, req.NativeRelativePath, req.Generation, req.ByteStart,
	)

	seg, err := s.store.PrepareTrajectorySegment(ctx, &store.TrajectorySegment{
		HubID:              hubID,
		OriginHubID:        originHubID,
		ProjectID:          attr.ProjectID,
		AgentID:            attr.AgentID,
		RunID:              attr.RunID,
		Harness:            req.Harness,
		NativeRelativePath: req.NativeRelativePath,
		ArtifactKind:       req.ArtifactKind,
		Generation:         req.Generation,
		ByteStart:          req.ByteStart,
		ByteEnd:            req.ByteEnd,
		RawSHA256:          req.RawSHA256,
		CompressedSize:     req.CompressedSize,
		CompressedSHA256:   req.CompressedSHA256,
		IncompleteTail:     req.IncompleteTail,
		SessionID:          req.SessionID,
		ObjectPath:         objectPath,
		Status:             telemetrycontract.TrajectorySegmentStatusPending,
		NormalizeStatus:    telemetrycontract.TrajectoryNormalizeStatusPending,
		CreatedAt:          now,
		PrepareExpiresAt:   now.Add(s.trajectoryPrepareTTLDuration()),
	})
	if err != nil {
		if errors.Is(err, store.ErrTrajectorySegmentConflict) {
			writeError(w, http.StatusConflict, telemetrycontract.TrajectoryErrSegmentConflict,
				"segment identity is already committed with different content", nil)
			return
		}
		writeErrorFromErr(w, err, "")
		return
	}

	if seg.Status == telemetrycontract.TrajectorySegmentStatusCommitted {
		writeJSON(w, http.StatusOK, telemetrycontract.TrajectorySegmentPrepareResponse{
			SegmentID:  seg.ID,
			State:      telemetrycontract.TrajectorySegmentStateCommitted,
			ObjectPath: seg.ObjectPath,
		})
		return
	}

	uploadInst, err := s.buildTrajectoryUploadInstruction(ctx, r, stor, seg, now)
	if err != nil {
		slog.ErrorContext(ctx, "trajectory segment prepare: failed to issue upload URL",
			"agent_id", seg.AgentID, "segment_id", seg.ID, "provider", stor.Provider(), "error", err)
		RuntimeError(w, "Failed to issue trajectory segment upload URL")
		return
	}

	writeJSON(w, http.StatusOK, telemetrycontract.TrajectorySegmentPrepareResponse{
		SegmentID:  seg.ID,
		State:      telemetrycontract.TrajectorySegmentStateUploadRequired,
		ObjectPath: seg.ObjectPath,
		Upload:     uploadInst,
	})
}

func validateTrajectoryPrepareRequest(req *telemetrycontract.TrajectorySegmentPrepareRequest) error {
	if req.Harness != "claude" {
		return fmt.Errorf("unsupported harness %q", req.Harness)
	}
	if err := telemetrycontract.ValidateTrajectoryRelativePath(req.Harness, req.NativeRelativePath); err != nil {
		return err
	}
	if req.ArtifactKind != telemetrycontract.TrajectoryArtifactKindJSONLAppend {
		return fmt.Errorf("unsupported artifact_kind %q", req.ArtifactKind)
	}
	if req.ContentEncoding != telemetrycontract.TrajectoryContentEncodingGzip {
		return fmt.Errorf("unsupported content_encoding %q", req.ContentEncoding)
	}
	if req.Generation < 0 {
		return fmt.Errorf("generation must be non-negative")
	}
	if req.ByteStart < 0 || req.ByteEnd <= req.ByteStart {
		return fmt.Errorf("invalid byte range [%d, %d)", req.ByteStart, req.ByteEnd)
	}
	if req.CompressedSize <= 0 {
		return fmt.Errorf("compressed_size must be positive")
	}
	if !isValidHexSHA256(req.RawSHA256) {
		return fmt.Errorf("raw_sha256 must be a 64-character lowercase hex digest")
	}
	if !isValidHexSHA256(req.CompressedSHA256) {
		return fmt.Errorf("compressed_sha256 must be a 64-character lowercase hex digest")
	}
	if len(req.SessionID) > trajectoryMetadataFieldMaxLen {
		return fmt.Errorf("session_id exceeds maximum length of %d bytes", trajectoryMetadataFieldMaxLen)
	}
	if !isPrintableTrajectoryField(req.SessionID) {
		return fmt.Errorf("session_id must contain only printable UTF-8 characters")
	}
	if len(req.OriginHubID) > trajectoryMetadataFieldMaxLen {
		return fmt.Errorf("origin_hub_id exceeds maximum length of %d bytes", trajectoryMetadataFieldMaxLen)
	}
	if !isPrintableTrajectoryField(req.OriginHubID) {
		return fmt.Errorf("origin_hub_id must contain only printable UTF-8 characters")
	}
	return nil
}

func isPrintableTrajectoryField(s string) bool {
	if !utf8.ValidString(s) {
		return false
	}
	for _, r := range s {
		if !unicode.IsPrint(r) {
			return false
		}
	}
	return true
}

// buildTrajectoryUploadInstruction produces either a GCS V4 no-overwrite signed
// PUT URL or a Hub-proxied HMAC capability PUT URL (for ProviderLocal or GCS
// deployments without signBlob permission).
func (s *Server) buildTrajectoryUploadInstruction(
	ctx context.Context,
	r *http.Request,
	stor storage.Storage,
	seg *store.TrajectorySegment,
	now time.Time,
) (*telemetrycontract.TrajectorySegmentUploadInstruction, error) {
	ttl := s.trajectoryUploadURLTTLDuration()
	expiresAt := now.Add(ttl)

	if stor.Provider() == storage.ProviderGCS {
		zero := int64(0)
		signed, err := stor.GenerateSignedURL(ctx, seg.ObjectPath, storage.SignedURLOptions{
			Method:             http.MethodPut,
			Expires:            ttl,
			ContentType:        trajectoryContentTypeGzip,
			ContentLengthRange: &storage.ContentLengthRange{Min: seg.CompressedSize, Max: seg.CompressedSize},
			IfGenerationMatch:  &zero,
		})
		if err == nil && signed != nil && signed.URL != "" {
			headers := make(map[string]string, len(signed.Headers)+1)
			for k, v := range signed.Headers {
				headers[k] = v
			}
			if _, ok := headers["Content-Type"]; !ok {
				headers["Content-Type"] = trajectoryContentTypeGzip
			}
			exp := signed.Expires
			if exp.IsZero() {
				exp = expiresAt
			}
			return &telemetrycontract.TrajectorySegmentUploadInstruction{
				Method:    http.MethodPut,
				URL:       signed.URL,
				Headers:   headers,
				ExpiresAt: exp.UTC().Format(time.RFC3339),
			}, nil
		}
		// Fallback for GCS without signBlob (e.g. Workload Identity without
		// iam.serviceAccounts.signBlob): route through the Hub-proxied PUT
		// endpoint, which enforces IfGenerationMatch=0 on GCS directly.
		slog.WarnContext(ctx, "trajectory segment prepare: GCS signed URL unavailable; falling back to Hub-proxied PUT",
			"agent_id", seg.AgentID, "segment_id", seg.ID, "error", err)
	}

	sigQuery, err := s.signTrajectoryUploadURL(seg.AgentID, seg.ID, seg.CompressedSize, seg.CompressedSHA256, expiresAt)
	if err != nil {
		return nil, err
	}
	uploadURL := s.trajectoryProxyBaseURL(r) + telemetrycontract.TrajectorySegmentContentAgentPath(seg.AgentID, seg.ID) + "?" + sigQuery
	return &telemetrycontract.TrajectorySegmentUploadInstruction{
		Method: http.MethodPut,
		URL:    uploadURL,
		Headers: map[string]string{
			"Content-Type": trajectoryContentTypeGzip,
		},
		ExpiresAt: expiresAt.UTC().Format(time.RFC3339),
	}, nil
}

// trajectoryProxyBaseURL returns the base URL for Hub-proxied trajectory
// segment PUTs so the upload URL's scheme and host match the agent's
// configured SCION_HUB_ENDPOINT even behind a TLS-terminating proxy without
// server.trusted_proxies. Precedence matches agent dispatch:
//  1. server.hub.agent_endpoint (s.config.AgentEndpoint), if configured.
//  2. HubEndpointFor(HubEndpointForAgents), unless it is a loopback URL and
//     the request arrived on a non-loopback host (e.g. Docker container-bridge
//     rewrite to host.docker.internal), in which case s.requestBaseURL(r) is
//     used so the container's origin still matches.
//  3. s.requestBaseURL(r) fallback (e.g. unconfigured test servers).
func (s *Server) trajectoryProxyBaseURL(r *http.Request) string {
	if ep := strings.TrimRight(strings.TrimSpace(s.config.AgentEndpoint), "/"); ep != "" {
		return ep
	}
	if ep := strings.TrimRight(strings.TrimSpace(s.HubEndpointFor(HubEndpointForAgents)), "/"); ep != "" {
		if u, err := url.Parse(ep); err == nil && u.Scheme != "" && u.Host != "" {
			if !isLoopbackTrajectoryHost(u.Hostname()) || r.Host == "" || isLoopbackTrajectoryHost(trajectoryRequestHostOnly(r.Host)) {
				return ep
			}
		}
	}
	return s.requestBaseURL(r)
}

func trajectoryRequestHostOnly(hostport string) string {
	if h, _, err := net.SplitHostPort(hostport); err == nil {
		return h
	}
	return strings.Trim(hostport, "[]")
}

func isLoopbackTrajectoryHost(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	if ip := net.ParseIP(host); ip != nil {
		return ip.IsLoopback()
	}
	return false
}

// handleAgentTrajectorySegmentContent serves
// PUT /api/v1/agents/{id}/trajectory/segments/{segment_id}/content.
// Used for ProviderLocal and as the Hub-proxied fallback when GCS signBlob is
// unavailable. Requires BOTH agent-self JWT auth AND a valid HMAC capability
// query signature. Enforces no-overwrite semantics:
//   - object does not exist -> atomically creates it, returns 200
//   - object already exists with identical bytes -> returns 200
//   - object already exists with different bytes -> returns 412 Precondition Failed
func (s *Server) handleAgentTrajectorySegmentContent(w http.ResponseWriter, r *http.Request, id, segmentID string) {
	if !s.experimentEnabled(experiments.TokenomicsTrajectoryCapture) {
		NotFound(w, "route")
		return
	}
	if r.Method != http.MethodPut {
		MethodNotAllowed(w, http.MethodPut)
		return
	}

	ctx := r.Context()
	agentIdent := GetAgentIdentityFromContext(ctx)
	if agentIdent == nil {
		Unauthorized(w)
		return
	}
	if agentIdent.ID() != id {
		writeError(w, http.StatusForbidden, ErrCodeForbidden,
			"Agents can only upload their own trajectory segments", nil)
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
	if seg.AgentID != id {
		NotFound(w, "Trajectory segment")
		return
	}

	now := time.Now().UTC()
	if !s.verifyTrajectoryUploadSignature(r, id, seg.ID, seg.CompressedSize, seg.CompressedSHA256, now) {
		writeError(w, http.StatusForbidden, ErrCodeForbidden,
			"Invalid or expired trajectory upload signature", nil)
		return
	}

	stor := s.GetStorage()
	if stor == nil {
		RuntimeError(w, "Hub storage is not configured")
		return
	}

	// Stream the upload body directly into storage while hashing and enforcing
	// the exact compressed_size and compressed_sha256 bound to the HMAC.
	// On size or hash mismatch, trajectoryVerifyingReader returns an error
	// before EOF so neither LocalStorage (temp file unlinked) nor GCSStorage
	// (CloseWithError + cancel) finalizes corrupt bytes at the immutable path.
	vr := newTrajectoryVerifyingReader(r.Body, seg.CompressedSize, seg.CompressedSHA256)
	zero := int64(0)
	_, err = stor.Upload(ctx, seg.ObjectPath, vr, storage.UploadOptions{
		ContentType:       trajectoryContentTypeGzip,
		IfGenerationMatch: &zero,
	})
	if err == nil {
		w.WriteHeader(http.StatusOK)
		return
	}
	if errors.Is(err, errTrajectoryUploadSizeMismatch) {
		writeError(w, http.StatusBadRequest, telemetrycontract.TrajectoryErrInvalidSegment,
			fmt.Sprintf("uploaded size %d does not match expected compressed_size %d", vr.bytesRead, seg.CompressedSize), nil)
		return
	}
	if errors.Is(err, errTrajectoryUploadHashMismatch) {
		writeError(w, http.StatusUnprocessableEntity, telemetrycontract.TrajectoryErrVerificationFailed,
			"uploaded compressed_sha256 does not match prepared segment", nil)
		return
	}
	if vr.readErr != nil {
		BadRequest(w, "Failed to read upload body")
		return
	}
	if !errors.Is(err, storage.ErrPreconditionFailed) {
		slog.ErrorContext(ctx, "trajectory segment upload failed",
			"agent_id", id, "segment_id", seg.ID, "error", err)
		RuntimeError(w, "Failed to store trajectory segment")
		return
	}

	// If the storage backend returned 412 before the stream reached EOF (e.g.
	// GCS mid-stream chunk rejection), finish verifying the client body so we
	// never return 200 for a corrupt or wrong-sized upload.
	if !vr.verified {
		if _, drainErr := io.Copy(io.Discard, vr); drainErr != nil {
			if errors.Is(drainErr, errTrajectoryUploadSizeMismatch) {
				writeError(w, http.StatusBadRequest, telemetrycontract.TrajectoryErrInvalidSegment,
					fmt.Sprintf("uploaded size %d does not match expected compressed_size %d", vr.bytesRead, seg.CompressedSize), nil)
				return
			}
			if errors.Is(drainErr, errTrajectoryUploadHashMismatch) {
				writeError(w, http.StatusUnprocessableEntity, telemetrycontract.TrajectoryErrVerificationFailed,
					"uploaded compressed_sha256 does not match prepared segment", nil)
				return
			}
			BadRequest(w, "Failed to read upload body")
			return
		}
	}

	// Object already exists: identical bytes -> 200, different bytes -> 412.
	identical, cmpErr := storageObjectMatchesHash(ctx, stor, seg.ObjectPath, seg.CompressedSize, seg.CompressedSHA256)
	if cmpErr != nil {
		slog.ErrorContext(ctx, "trajectory segment upload: failed to compare existing object",
			"agent_id", id, "segment_id", seg.ID, "error", cmpErr)
		RuntimeError(w, "Failed to verify existing trajectory segment")
		return
	}
	if identical {
		w.WriteHeader(http.StatusOK)
		return
	}
	writeError(w, http.StatusPreconditionFailed, telemetrycontract.TrajectoryErrSegmentConflict,
		"trajectory segment object already exists with different bytes", nil)
}

type trajectoryVerifyingReader struct {
	r            io.Reader
	hash         hash.Hash
	expectedSize int64
	expectedHash string
	bytesRead    int64
	verified     bool
	readErr      error
}

func newTrajectoryVerifyingReader(body io.Reader, expectedSize int64, expectedSHA256 string) *trajectoryVerifyingReader {
	return &trajectoryVerifyingReader{
		r:            io.LimitReader(body, expectedSize+1),
		hash:         sha256.New(),
		expectedSize: expectedSize,
		expectedHash: expectedSHA256,
	}
}

func (v *trajectoryVerifyingReader) Read(p []byte) (int, error) {
	n, err := v.r.Read(p)
	if n > 0 {
		v.bytesRead += int64(n)
		if v.bytesRead > v.expectedSize {
			return 0, errTrajectoryUploadSizeMismatch
		}
		_, _ = v.hash.Write(p[:n])
	}
	if err == io.EOF {
		if v.bytesRead != v.expectedSize {
			return 0, errTrajectoryUploadSizeMismatch
		}
		if hex.EncodeToString(v.hash.Sum(nil)) != v.expectedHash {
			return 0, errTrajectoryUploadHashMismatch
		}
		v.verified = true
		return n, io.EOF
	}
	if err != nil {
		v.readErr = err
		return n, err
	}
	return n, nil
}

type trajectoryCountingReader struct {
	r io.Reader
	n *int64
}

func (c *trajectoryCountingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	*c.n += int64(n)
	return n, err
}

func storageObjectMatchesHash(ctx context.Context, stor storage.Storage, objectPath string, expectedSize int64, expectedSHA256 string) (bool, error) {
	rc, _, err := stor.Download(ctx, objectPath)
	if err != nil {
		return false, err
	}
	defer func() { _ = rc.Close() }()
	h := sha256.New()
	n, err := io.Copy(h, io.LimitReader(rc, expectedSize+1))
	if err != nil {
		return false, err
	}
	if n != expectedSize {
		return false, nil
	}
	return hex.EncodeToString(h.Sum(nil)) == expectedSHA256, nil
}

// handleAgentTrajectorySegmentsCommit serves
// POST /api/v1/agents/{id}/trajectory/segments:commit.
func (s *Server) handleAgentTrajectorySegmentsCommit(w http.ResponseWriter, r *http.Request, id string) {
	if !s.experimentEnabled(experiments.TokenomicsTrajectoryCapture) {
		NotFound(w, "route")
		return
	}
	if r.Method != http.MethodPost {
		MethodNotAllowed(w, http.MethodPost)
		return
	}

	ctx := r.Context()
	agentIdent := GetAgentIdentityFromContext(ctx)
	if agentIdent == nil {
		Unauthorized(w)
		return
	}
	if agentIdent.ID() != id {
		writeError(w, http.StatusForbidden, ErrCodeForbidden,
			"Agents can only commit their own trajectory segments", nil)
		return
	}

	body, err := io.ReadAll(io.LimitReader(r.Body, trajectorySegmentRequestMaxBody+1))
	if err != nil {
		writeError(w, http.StatusBadRequest, telemetrycontract.TrajectoryErrInvalidSegment,
			"Failed to read request body", nil)
		return
	}
	if len(body) > trajectorySegmentRequestMaxBody {
		writeError(w, http.StatusBadRequest, telemetrycontract.TrajectoryErrInvalidSegment,
			"Request body exceeds maximum size", nil)
		return
	}

	var req telemetrycontract.TrajectorySegmentCommitRequest
	if err := json.Unmarshal(body, &req); err != nil || strings.TrimSpace(req.SegmentID) == "" {
		writeError(w, http.StatusBadRequest, telemetrycontract.TrajectoryErrInvalidSegment,
			"segment_id is required", nil)
		return
	}

	seg, err := s.store.GetTrajectorySegment(ctx, req.SegmentID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			NotFound(w, "Trajectory segment")
			return
		}
		writeErrorFromErr(w, err, "")
		return
	}
	if seg.AgentID != id {
		NotFound(w, "Trajectory segment")
		return
	}

	// Idempotent replay of an already-committed segment.
	if seg.Status == telemetrycontract.TrajectorySegmentStatusCommitted {
		writeJSON(w, http.StatusOK, telemetrycontract.TrajectorySegmentCommitResponse{
			State:        telemetrycontract.TrajectorySegmentStateCommitted,
			SegmentID:    seg.ID,
			ManifestPath: seg.ManifestPath,
		})
		return
	}

	now := time.Now().UTC().Truncate(time.Microsecond)
	if now.After(seg.PrepareExpiresAt) {
		writeError(w, http.StatusGone, telemetrycontract.TrajectoryErrSegmentExpired,
			"prepared segment has expired; prepare again", nil)
		return
	}

	stor := s.GetStorage()
	if stor == nil {
		RuntimeError(w, "Hub storage is not configured")
		return
	}

	if err := s.verifyStoredTrajectorySegment(ctx, stor, seg); err != nil {
		writeError(w, http.StatusUnprocessableEntity, telemetrycontract.TrajectoryErrVerificationFailed,
			err.Error(), nil)
		return
	}

	if s.trajectoryCommitAfterVerifyHook != nil {
		s.trajectoryCommitAfterVerifyHook(ctx, seg)
	}

	// Resolve run_attribution to stamp the immutable Hub-authored manifest.
	agent, err := s.store.GetAgent(ctx, id)
	if err != nil {
		writeErrorFromErr(w, err, "")
		return
	}
	attr, err := s.resolveTrajectoryRunAttribution(ctx, agent, seg.RunID)
	if err != nil {
		writeErrorFromErr(w, err, "")
		return
	}

	manifestPath := telemetrycontract.TrajectoryRawManifestPath(seg.ObjectPath)
	manifestBytes, err := buildTrajectorySegmentManifestJSON(seg, attr, now)
	if err != nil {
		RuntimeError(w, "Failed to build trajectory segment manifest")
		return
	}

	commitTimestamp := now
	zero := int64(0)
	_, err = stor.Upload(ctx, manifestPath, bytes.NewReader(manifestBytes), storage.UploadOptions{
		ContentType:       "application/json",
		IfGenerationMatch: &zero,
	})
	if errors.Is(err, storage.ErrPreconditionFailed) {
		// An object already exists at manifestPath (e.g., a retry after a prior
		// commit wrote the manifest but failed before updating the DB row).
		// Read and verify the existing manifest matches this segment's identity
		// and hashes before accepting it, and reuse its committed_at timestamp
		// so the DB row agrees with the immutable manifest.
		existingCommittedAt, verr := verifyExistingTrajectoryManifest(ctx, stor, manifestPath, seg, attr)
		if verr != nil {
			writeError(w, http.StatusUnprocessableEntity, telemetrycontract.TrajectoryErrVerificationFailed,
				"existing trajectory manifest mismatch: "+verr.Error(), nil)
			return
		}
		if !existingCommittedAt.IsZero() {
			commitTimestamp = existingCommittedAt
		}
	} else if err != nil {
		slog.ErrorContext(ctx, "trajectory segment commit: failed to write manifest",
			"agent_id", id, "segment_id", seg.ID, "error", err)
		RuntimeError(w, "Failed to write trajectory segment manifest")
		return
	}

	committed, err := s.store.CommitTrajectorySegment(ctx, seg.ID, manifestPath, commitTimestamp, store.TrajectorySegmentCommitExpectation{
		ByteEnd:          seg.ByteEnd,
		RawSHA256:        seg.RawSHA256,
		CompressedSize:   seg.CompressedSize,
		CompressedSHA256: seg.CompressedSHA256,
		IncompleteTail:   seg.IncompleteTail,
		SessionID:        seg.SessionID,
	})
	if err != nil {
		if errors.Is(err, store.ErrTrajectorySegmentConflict) {
			writeError(w, http.StatusConflict, telemetrycontract.TrajectoryErrSegmentConflict,
				"segment identity is already committed with different content", nil)
			return
		}
		if errors.Is(err, store.ErrTrajectorySegmentModified) {
			writeError(w, http.StatusUnprocessableEntity, telemetrycontract.TrajectoryErrVerificationFailed,
				"prepared segment was modified concurrently before commit; re-verify and retry", nil)
			return
		}
		writeErrorFromErr(w, err, "")
		return
	}

	writeJSON(w, http.StatusOK, telemetrycontract.TrajectorySegmentCommitResponse{
		State:        telemetrycontract.TrajectorySegmentStateCommitted,
		SegmentID:    committed.ID,
		ManifestPath: committed.ManifestPath,
	})
}

func (s *Server) verifyStoredTrajectorySegment(ctx context.Context, stor storage.Storage, seg *store.TrajectorySegment) error {
	obj, err := stor.GetObject(ctx, seg.ObjectPath)
	if err != nil {
		if errors.Is(err, storage.ErrNotFound) {
			return fmt.Errorf("segment object not found")
		}
		return fmt.Errorf("failed to stat segment object: %w", err)
	}
	if obj.Size != seg.CompressedSize {
		return fmt.Errorf("segment object size %d does not match expected %d", obj.Size, seg.CompressedSize)
	}

	rc, _, err := stor.Download(ctx, seg.ObjectPath)
	if err != nil {
		if errors.Is(err, storage.ErrNotFound) {
			return fmt.Errorf("segment object not found")
		}
		return fmt.Errorf("failed to download segment object: %w", err)
	}
	defer func() { _ = rc.Close() }()

	compHash := sha256.New()
	var compLen int64
	compReader := &trajectoryCountingReader{
		r: io.TeeReader(io.LimitReader(rc, seg.CompressedSize+1), compHash),
		n: &compLen,
	}

	gz, err := gzip.NewReader(compReader)
	if err != nil {
		return fmt.Errorf("invalid gzip content: %w", err)
	}
	defer func() { _ = gz.Close() }()

	expectedRawLen := seg.ByteEnd - seg.ByteStart
	rawHash := sha256.New()
	rawLen, err := io.Copy(rawHash, io.LimitReader(gz, expectedRawLen+1))
	if err != nil {
		return fmt.Errorf("failed to decompress gzip content: %w", err)
	}
	if rawLen != expectedRawLen {
		return fmt.Errorf("decompressed raw length %d does not match expected %d", rawLen, expectedRawLen)
	}
	if err := gz.Close(); err != nil {
		return fmt.Errorf("corrupt gzip trailer: %w", err)
	}
	// Drain any remaining compressed bytes (up to the +1 limit) so compLen and
	// compHash cover the entire stored object without buffering it in memory.
	if _, err := io.Copy(io.Discard, compReader); err != nil {
		return fmt.Errorf("failed to read segment object: %w", err)
	}
	if compLen != seg.CompressedSize {
		return fmt.Errorf("segment object byte length %d does not match expected %d", compLen, seg.CompressedSize)
	}
	if hex.EncodeToString(compHash.Sum(nil)) != seg.CompressedSHA256 {
		return fmt.Errorf("compressed_sha256 mismatch")
	}
	if hex.EncodeToString(rawHash.Sum(nil)) != seg.RawSHA256 {
		return fmt.Errorf("raw_sha256 mismatch")
	}
	return nil
}

// buildTrajectorySegmentManifestJSON serializes the Hub-authored manifest for
// seg and stamps manifest_digest as the lowercase hex SHA-256 of the JSON
// encoding with "manifest_digest":"" present (verifiers must blank the field,
// not omit it). Both the object path and the manifest use seg.Harness as the
// single canonical harness source.
func buildTrajectorySegmentManifestJSON(seg *store.TrajectorySegment, attr *store.RunAttribution, committedAt time.Time) ([]byte, error) {
	m := telemetrycontract.TrajectorySegmentManifest{
		SchemaVersion:      telemetrycontract.TrajectoryManifestSchemaVersion,
		HubID:              seg.HubID,
		OriginHubID:        seg.OriginHubID,
		ProjectID:          attr.ProjectID,
		AgentID:            attr.AgentID,
		RunID:              attr.RunID,
		Harness:            seg.Harness,
		HarnessConfig:      attr.HarnessConfig,
		Template:           attr.Template,
		BillingUserID:      copyUsageStringPtr(attr.BillingUserID),
		NativeRelativePath: seg.NativeRelativePath,
		ArtifactKind:       seg.ArtifactKind,
		Generation:         seg.Generation,
		ByteStart:          seg.ByteStart,
		ByteEnd:            seg.ByteEnd,
		RawSHA256:          seg.RawSHA256,
		CompressedSize:     seg.CompressedSize,
		CompressedSHA256:   seg.CompressedSHA256,
		ContentEncoding:    telemetrycontract.TrajectoryContentEncodingGzip,
		IncompleteTail:     seg.IncompleteTail,
		SessionID:          seg.SessionID,
		ObjectPath:         seg.ObjectPath,
		CommittedAt:        committedAt.UTC().Format(time.RFC3339),
	}
	canonical, err := json.Marshal(m)
	if err != nil {
		return nil, err
	}
	digest := sha256.Sum256(canonical)
	m.ManifestDigest = hex.EncodeToString(digest[:])
	return json.Marshal(m)
}

func verifyExistingTrajectoryManifest(ctx context.Context, stor storage.Storage, manifestPath string, seg *store.TrajectorySegment, attr *store.RunAttribution) (time.Time, error) {
	rc, _, err := stor.Download(ctx, manifestPath)
	if err != nil {
		return time.Time{}, fmt.Errorf("download existing manifest: %w", err)
	}
	defer func() { _ = rc.Close() }()

	data, err := io.ReadAll(io.LimitReader(rc, trajectorySegmentRequestMaxBody+1))
	if err != nil {
		return time.Time{}, fmt.Errorf("read existing manifest: %w", err)
	}
	if len(data) > trajectorySegmentRequestMaxBody {
		return time.Time{}, fmt.Errorf("existing manifest exceeds maximum size")
	}
	var m telemetrycontract.TrajectorySegmentManifest
	if err := json.Unmarshal(data, &m); err != nil {
		return time.Time{}, fmt.Errorf("parse existing manifest: %w", err)
	}
	if m.ProjectID != attr.ProjectID ||
		m.AgentID != attr.AgentID ||
		m.RunID != attr.RunID ||
		m.Harness != seg.Harness ||
		m.NativeRelativePath != seg.NativeRelativePath ||
		m.Generation != seg.Generation ||
		m.ByteStart != seg.ByteStart ||
		m.ByteEnd != seg.ByteEnd ||
		m.RawSHA256 != seg.RawSHA256 ||
		m.CompressedSize != seg.CompressedSize ||
		m.CompressedSHA256 != seg.CompressedSHA256 ||
		m.IncompleteTail != seg.IncompleteTail ||
		m.SessionID != seg.SessionID ||
		m.ObjectPath != seg.ObjectPath {
		return time.Time{}, fmt.Errorf("existing manifest identity or hashes do not match segment")
	}
	if m.CommittedAt != "" {
		if ts, err := time.Parse(time.RFC3339, m.CommittedAt); err == nil {
			return ts.UTC().Truncate(time.Microsecond), nil
		}
	}
	return time.Time{}, nil
}
