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
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/hub/permissions"
	"github.com/GoogleCloudPlatform/scion/pkg/storage"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/transfer/bundle"
)

const (
	// moveBundleRequestMaxBody bounds move-bundle signed-URL request bodies.
	moveBundleRequestMaxBody = 4 << 10
	// moveBundleManifestMaxBytes bounds reading manifest.json from storage.
	moveBundleManifestMaxBytes = 4 << 20
)

// Fixed client texts for move-bundle signed-URL endpoints. None names a path,
// bucket, object, or URL.
const (
	moveBundleUnsupportedMessage      = "This Hub's storage cannot issue move-bundle signed URLs"
	moveBundleBadRequestMessage       = "Invalid move-bundle request"
	moveBundleSignFailedMessage       = "Failed to issue move-bundle signed URL"
	moveBundleManifestNotReadyMessage = "Move bundle manifest is not available"
	moveBundleManifestInvalidMessage  = "Move bundle manifest is invalid"
)

// handleMoveBundleUploadURL implements
// POST /api/v1/runtime-brokers/{brokerId}/agents/{agentId}/move-bundle/{rid}/upload-url
// (authz operation broker.agent.movebundle.uploadurl, design §3.5).
//
// The checks run in fail-closed order:
//  1. caller is the broker itself (HMAC identity for brokerID), else 403;
//  2. reincarnation record exists, else 404;
//  3. caller is rec.SourceBrokerID, else 403;
//  4. rec.AgentID == agentID, else 404;
//  5. rec.TransferMode == "bundle" and rec.State == "stopping", else 409;
//  6. Hub storage is HTTP-signable (GCS), else 409 move_bundle_unsupported;
//  7. body is bounded and valid: root in {workspace, home}, 0 <= partIndex < maxParts(limits),
//     0 < size <= limits.PartSize, else 400.
//
// Keys are derived strictly by the Hub via storage.MoveBundlePartPath.
// Signed URLs never appear in logs or errors.
func (s *Server) handleMoveBundleUploadURL(w http.ResponseWriter, r *http.Request, brokerID, agentID, rid string) {
	ctx := r.Context()

	brokerIdent := GetBrokerIdentityFromContext(ctx)
	if brokerIdent == nil || brokerIdent.BrokerID() != brokerID {
		logAuthzDenial(r, GetIdentityFromContext(ctx), Resource{Type: permissions.ResourceBroker, ID: brokerID}, ActionWrite,
			"a move-bundle upload URL may only be requested by the source broker itself")
		Forbidden(w)
		return
	}

	rec, err := s.store.GetAgentReincarnation(ctx, rid)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) || errors.Is(err, store.ErrInvalidInput) {
			NotFound(w, "Reincarnation")
			return
		}
		s.workspaceLog.Error("Move bundle upload-url: failed to read reincarnation",
			"agent_id", agentID, "broker_id", brokerID, "reincarnation_id", rid, "error", err)
		InternalError(w)
		return
	}
	if rec.SourceBrokerID != brokerID {
		logAuthzDenial(r, brokerIdent, Resource{Type: permissions.ResourceBroker, ID: brokerID}, ActionWrite,
			"caller is not the source broker for this reincarnation")
		Forbidden(w)
		return
	}
	if rec.AgentID != agentID {
		NotFound(w, "Reincarnation")
		return
	}
	if rec.TransferMode != store.TransferModeBundle {
		Conflict(w, "reincarnation is not a bundle move")
		return
	}
	if rec.State != store.AgentReincarnationStateStopping {
		Conflict(w, "bundle upload URLs are only issued while the move is stopping")
		return
	}

	stor := s.GetStorage()
	if !isMoveBundleStorageSupported(stor) {
		writeError(w, http.StatusConflict, api.BrokerErrCodeMoveBundleUnsupported, moveBundleUnsupportedMessage, nil)
		return
	}

	var req api.MoveBundleUploadURLRequest
	if !decodeMoveBundleBody(w, r, &req, false) {
		BadRequest(w, moveBundleBadRequestMessage)
		return
	}

	limits := s.MoveBundleExportLimits()
	var rootLim bundle.Limits
	switch req.Root {
	case storage.MoveBundleRootWorkspace:
		rootLim = limits.WorkspaceLimits()
	case storage.MoveBundleRootHome:
		rootLim = limits.HomeLimits()
	default:
		BadRequest(w, moveBundleBadRequestMessage)
		return
	}
	if req.PartIndex < 0 || req.PartIndex >= rootLim.MaxParts() {
		BadRequest(w, moveBundleBadRequestMessage)
		return
	}
	if req.Size <= 0 || req.Size > rootLim.PartSize {
		BadRequest(w, moveBundleBadRequestMessage)
		return
	}

	key, err := storage.MoveBundlePartPath(s.HubID(), rid, req.Root, req.PartIndex)
	if err != nil {
		BadRequest(w, moveBundleBadRequestMessage)
		return
	}

	signed, err := stor.GenerateSignedURL(ctx, key, storage.SignedURLOptions{
		Method:             http.MethodPut,
		Expires:            SignedURLExpiry,
		ContentType:        "application/octet-stream",
		ContentLengthRange: &storage.ContentLengthRange{Min: req.Size, Max: req.Size},
	})
	if err != nil || signed == nil || signed.URL == "" {
		s.workspaceLog.Error("Move bundle upload-url: failed to sign upload URL",
			"agent_id", agentID, "broker_id", brokerID, "reincarnation_id", rid,
			"root", req.Root, "part_index", req.PartIndex, "error_class", workspaceStorageErrorClass(err))
		RuntimeError(w, moveBundleSignFailedMessage)
		return
	}

	headers := make(map[string]string, len(signed.Headers)+1)
	for k, v := range signed.Headers {
		headers[k] = v
	}
	if _, ok := headers["Content-Type"]; !ok {
		headers["Content-Type"] = "application/octet-stream"
	}

	expires := signed.Expires
	if expires.IsZero() {
		expires = time.Now().Add(SignedURLExpiry)
	}
	s.workspaceLog.Info("Move bundle upload URL issued",
		"agent_id", agentID, "broker_id", brokerID, "reincarnation_id", rid,
		"root", req.Root, "part_index", req.PartIndex, "size", req.Size,
		"expires", expires.UTC().Format(time.RFC3339))

	writeJSON(w, http.StatusOK, api.MoveBundleUploadURLResponse{
		URL:              signed.URL,
		Method:           http.MethodPut,
		Headers:          headers,
		Expires:          expires,
		ExpiresInSeconds: int64(SignedURLExpiry / time.Second),
	})
}

// handleMoveBundleDownloadURLs implements
// POST /api/v1/runtime-brokers/{brokerId}/agents/{agentId}/move-bundle/{rid}/download-urls
// (authz operation broker.agent.movebundle.downloadurls, design §3.5).
//
// The checks run in fail-closed order:
//  1. caller is the broker itself (HMAC identity for brokerID), else 403;
//  2. reincarnation record exists, else 404;
//  3. caller is rec.TargetBrokerID, else 403;
//  4. rec.AgentID == agentID, else 404;
//  5. rec.TransferMode == "bundle" and rec.State == "provisioning", else 409;
//  6. agent exists and is assigned to brokerID, else 404;
//  7. Hub storage is HTTP-signable (GCS), else 409 move_bundle_unsupported;
//  8. body is empty or a valid bounded JSON object, else 400.
//
// Reads the Hub-persisted manifest.json from storage and signs GET URLs for
// exactly its parts, using Hub-derived keys only. Signed URLs never appear in
// logs or errors.
func (s *Server) handleMoveBundleDownloadURLs(w http.ResponseWriter, r *http.Request, brokerID, agentID, rid string) {
	ctx := r.Context()

	brokerIdent := GetBrokerIdentityFromContext(ctx)
	if brokerIdent == nil || brokerIdent.BrokerID() != brokerID {
		logAuthzDenial(r, GetIdentityFromContext(ctx), Resource{Type: permissions.ResourceBroker, ID: brokerID}, ActionRead,
			"move-bundle download URLs may only be requested by the target broker itself")
		Forbidden(w)
		return
	}

	rec, err := s.store.GetAgentReincarnation(ctx, rid)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) || errors.Is(err, store.ErrInvalidInput) {
			NotFound(w, "Reincarnation")
			return
		}
		s.workspaceLog.Error("Move bundle download-urls: failed to read reincarnation",
			"agent_id", agentID, "broker_id", brokerID, "reincarnation_id", rid, "error", err)
		InternalError(w)
		return
	}
	if rec.TargetBrokerID != brokerID {
		logAuthzDenial(r, brokerIdent, Resource{Type: permissions.ResourceBroker, ID: brokerID}, ActionRead,
			"caller is not the target broker for this reincarnation")
		Forbidden(w)
		return
	}
	if rec.AgentID != agentID {
		NotFound(w, "Reincarnation")
		return
	}
	if rec.TransferMode != store.TransferModeBundle {
		Conflict(w, "reincarnation is not a bundle move")
		return
	}
	if rec.State != store.AgentReincarnationStateProvisioning {
		Conflict(w, "bundle download URLs are only issued while the move is provisioning")
		return
	}

	agent, err := s.store.GetAgent(ctx, agentID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			NotFound(w, "Agent")
			return
		}
		s.workspaceLog.Error("Move bundle download-urls: failed to read agent",
			"agent_id", agentID, "broker_id", brokerID, "reincarnation_id", rid, "error", err)
		InternalError(w)
		return
	}
	if agent.RuntimeBrokerID != brokerID {
		NotFound(w, "Agent")
		return
	}

	stor := s.GetStorage()
	if !isMoveBundleStorageSupported(stor) {
		writeError(w, http.StatusConflict, api.BrokerErrCodeMoveBundleUnsupported, moveBundleUnsupportedMessage, nil)
		return
	}

	var req api.MoveBundleDownloadURLsRequest
	if !decodeMoveBundleBody(w, r, &req, true) {
		BadRequest(w, moveBundleBadRequestMessage)
		return
	}

	manifestKey, err := storage.MoveBundleManifestPath(s.HubID(), rid)
	if err != nil {
		BadRequest(w, moveBundleBadRequestMessage)
		return
	}

	rc, _, err := stor.Download(ctx, manifestKey)
	if err != nil {
		if errors.Is(err, storage.ErrNotFound) {
			Conflict(w, moveBundleManifestNotReadyMessage)
			return
		}
		s.workspaceLog.Error("Move bundle download-urls: failed to read manifest",
			"agent_id", agentID, "broker_id", brokerID, "reincarnation_id", rid,
			"error_class", workspaceStorageErrorClass(err))
		RuntimeError(w, moveBundleManifestNotReadyMessage)
		return
	}
	defer func() { _ = rc.Close() }()

	rawManifest, err := io.ReadAll(io.LimitReader(rc, moveBundleManifestMaxBytes+1))
	if err != nil || len(rawManifest) > moveBundleManifestMaxBytes {
		s.workspaceLog.Error("Move bundle download-urls: failed to read manifest body",
			"agent_id", agentID, "broker_id", brokerID, "reincarnation_id", rid,
			"error_class", workspaceStorageErrorClass(err))
		RuntimeError(w, moveBundleManifestInvalidMessage)
		return
	}

	manifest, err := bundle.ParseManifest(rawManifest)
	if err != nil || manifest.ReincarnationID != rid {
		s.workspaceLog.Error("Move bundle download-urls: invalid manifest",
			"agent_id", agentID, "broker_id", brokerID, "reincarnation_id", rid)
		RuntimeError(w, moveBundleManifestInvalidMessage)
		return
	}

	expires := time.Now().Add(SignedURLExpiry)
	var parts []api.MoveBundleDownloadPartURL
	for _, rootName := range []string{storage.MoveBundleRootWorkspace, storage.MoveBundleRootHome} {
		rm, ok := manifest.Roots[rootName]
		if !ok {
			continue
		}
		for _, p := range rm.Parts {
			partKey, err := storage.MoveBundlePartPath(s.HubID(), rid, rootName, p.Index)
			if err != nil {
				RuntimeError(w, moveBundleManifestInvalidMessage)
				return
			}
			signed, err := stor.GenerateSignedURL(ctx, partKey, storage.SignedURLOptions{
				Method:  http.MethodGet,
				Expires: SignedURLExpiry,
			})
			if err != nil || signed == nil || signed.URL == "" {
				s.workspaceLog.Error("Move bundle download-urls: failed to sign download URL",
					"agent_id", agentID, "broker_id", brokerID, "reincarnation_id", rid,
					"root", rootName, "part_index", p.Index, "error_class", workspaceStorageErrorClass(err))
				RuntimeError(w, moveBundleSignFailedMessage)
				return
			}
			partExpires := signed.Expires
			if partExpires.IsZero() {
				partExpires = expires
			}
			parts = append(parts, api.MoveBundleDownloadPartURL{
				Root:    rootName,
				Index:   p.Index,
				URL:     signed.URL,
				Expires: partExpires,
			})
		}
	}

	s.workspaceLog.Info("Move bundle download URLs issued",
		"agent_id", agentID, "broker_id", brokerID, "reincarnation_id", rid,
		"parts", len(parts), "expires", expires.UTC().Format(time.RFC3339))

	writeJSON(w, http.StatusOK, api.MoveBundleDownloadURLsResponse{
		Parts:            parts,
		Expires:          expires,
		ExpiresInSeconds: int64(SignedURLExpiry / time.Second),
	})
}

func decodeMoveBundleBody(w http.ResponseWriter, r *http.Request, dst any, allowEmpty bool) bool {
	if r.Body == nil {
		return allowEmpty
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, moveBundleRequestMaxBody))
	if err != nil {
		return false
	}
	if len(strings.TrimSpace(string(body))) == 0 {
		return allowEmpty
	}
	return json.Unmarshal(body, dst) == nil
}
