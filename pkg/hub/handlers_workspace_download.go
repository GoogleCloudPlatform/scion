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
	"github.com/GoogleCloudPlatform/scion/pkg/transfer"
)

// ErrCodeWorkspaceDownloadUnsupported is the 409 code of a workspace
// download on a Hub whose storage cannot issue signed URLs (not GCS).
const ErrCodeWorkspaceDownloadUnsupported = "workspace_download_unsupported"

// workspaceDownloadMaxBody bounds a workspace download page request body.
const workspaceDownloadMaxBody = 4 << 10

// Fixed client texts of the workspace download endpoint. None names a
// path, bucket, object or URL.
const (
	workspaceDownloadUnavailableMessage = "The agent has no workspace upload to download"
	workspaceDownloadUnsupportedMessage = "This Hub's storage cannot issue workspace download URLs"
	workspaceDownloadBadRequestMessage  = "Invalid workspace download request"
	workspaceDownloadListFailedMessage  = "Failed to list the workspace files"
	workspaceDownloadSignFailedMessage  = "Failed to issue workspace download URLs"
)

// handleWorkspaceDownload implements the broker-self workspace download,
// POST /api/v1/runtime-brokers/{brokerId}/agents/{agentId}/workspace-download
// (authz operation broker.agent.workspacedownload). A Runtime Broker that
// was told to bootstrap a hub-managed workspace with signed URLs
// (CreateAgentRequest.WorkspaceDownloadMode) pages through it: the Hub lists
// the agent's uploaded workspace files with its own storage credential and
// answers each page with one GET-only signed URL per file, so the broker
// needs no storage credential and never lists the bucket.
//
// The checks run in this order and all fail closed:
//  1. the caller is the broker itself (HMAC identity for brokerID), else 403;
//  2. the agent exists and is assigned to that broker, else 404, so the
//     answer does not show whether another broker's agent exists;
//  3. the broker still provides for the agent's project, else 403;
//  4. the agent has a workspace upload at the prefix the Hub derives for it
//     and is not being deleted, else 409;
//  5. the Hub's storage is GCS, else 409 workspace_download_unsupported;
//  6. the body is within its bound and the cursor lies inside the agent's
//     prefix, else 400.
//
// The prefix always comes from the agent row, never from the request. Names
// are filtered by transfer.ExcludeWorkspaceObject, the rule table the
// broker applies again. URLs are never logged: the Hub logs one line per
// page with counts and the expiry only.
func (s *Server) handleWorkspaceDownload(w http.ResponseWriter, r *http.Request, brokerID, agentID string) {
	ctx := r.Context()

	brokerIdent := GetBrokerIdentityFromContext(ctx)
	if brokerIdent == nil || brokerIdent.BrokerID() != brokerID {
		logAuthzDenial(r, GetIdentityFromContext(ctx), Resource{Type: permissions.ResourceBroker, ID: brokerID}, ActionRead,
			"a workspace download may only be requested by the broker itself")
		Forbidden(w)
		return
	}

	agent, err := s.store.GetAgent(ctx, agentID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			NotFound(w, "Agent")
			return
		}
		s.workspaceLog.Error("Workspace download: failed to read the agent", "agent_id", agentID, "broker_id", brokerID, "error", err)
		InternalError(w)
		return
	}
	if agent.RuntimeBrokerID != brokerID {
		NotFound(w, "Agent")
		return
	}

	if _, err := s.store.GetProjectProvider(ctx, agent.ProjectID, brokerID); err != nil {
		if !errors.Is(err, store.ErrNotFound) {
			s.workspaceLog.Error("Workspace download: failed to read the project provider", "agent_id", agentID, "broker_id", brokerID, "error", err)
			InternalError(w)
			return
		}
		logAuthzDenial(r, brokerIdent, Resource{Type: permissions.ResourceBroker, ID: brokerID}, ActionRead,
			"the broker no longer provides for the agent's project")
		Forbidden(w)
		return
	}

	prefix := storage.WorkspaceStoragePath(s.HubID(), agent.ProjectID, agent.ID)
	if agent.AppliedConfig == nil || agent.AppliedConfig.WorkspaceStoragePath == "" ||
		agent.AppliedConfig.WorkspaceStoragePath != prefix || workspaceDownloadAgentDeleting(agent) {
		Conflict(w, workspaceDownloadUnavailableMessage)
		return
	}

	stor := s.GetStorage()
	if stor == nil || stor.Provider() != storage.ProviderGCS {
		writeError(w, http.StatusConflict, ErrCodeWorkspaceDownloadUnsupported, workspaceDownloadUnsupportedMessage, nil)
		return
	}

	filesPrefix := prefix + "/files/"
	cursor, ok := readWorkspaceDownloadCursor(w, r)
	if !ok || (cursor != "" && (!strings.HasPrefix(cursor, filesPrefix) || len(cursor) == len(filesPrefix))) {
		BadRequest(w, workspaceDownloadBadRequestMessage)
		return
	}

	listed, err := stor.List(ctx, storage.ListOptions{
		Prefix:      filesPrefix,
		MaxResults:  api.WorkspaceDownloadPageSize,
		StartOffset: cursor,
	})
	if err != nil {
		s.workspaceLog.Error("Workspace download: failed to list the workspace files", "agent_id", agentID, "broker_id", brokerID, "error", err)
		RuntimeError(w, workspaceDownloadListFailedMessage)
		return
	}

	expires := time.Now().Add(SignedURLExpiry)
	files := make([]api.WorkspaceDownloadFile, 0, len(listed.Objects))
	excluded := 0
	for _, obj := range listed.Objects {
		rel, inPrefix := strings.CutPrefix(obj.Name, filesPrefix)
		if !inPrefix || transfer.ExcludeWorkspaceObject(rel) != transfer.WorkspaceObjectKept {
			excluded++
			continue
		}
		signed, err := stor.GenerateSignedURL(ctx, obj.Name, storage.SignedURLOptions{
			Method:  http.MethodGet,
			Expires: SignedURLExpiry,
		})
		if err != nil || signed == nil {
			// The signing error names no URL; it is logged for the operator
			// and the broker gets the fixed text.
			s.workspaceLog.Error("Workspace download: failed to sign a download URL", "agent_id", agentID, "broker_id", brokerID, "error", err)
			RuntimeError(w, workspaceDownloadSignFailedMessage)
			return
		}
		files = append(files, api.WorkspaceDownloadFile{Path: rel, URL: signed.URL, Size: obj.Size})
	}

	nextCursor := listed.NextOffset
	if nextCursor != "" && !strings.HasPrefix(nextCursor, filesPrefix) {
		// The listing ran past the agent's prefix: there is no next page.
		nextCursor = ""
	}

	s.workspaceLog.Info("Workspace download page issued", "agent_id", agentID, "broker_id", brokerID,
		"files", len(files), "excluded", excluded, "more", nextCursor != "", "expires", expires.UTC().Format(time.RFC3339))
	writeJSON(w, http.StatusOK, api.WorkspaceDownloadResponse{
		Files:      files,
		NextCursor: nextCursor,
		Expires:    expires,
	})
}

// readWorkspaceDownloadCursor reads the page request body, bounded by
// workspaceDownloadMaxBody, and returns its cursor. An empty body is the
// first page. It reports false for a body that is too large or not a
// WorkspaceDownloadRequest.
func readWorkspaceDownloadCursor(w http.ResponseWriter, r *http.Request) (string, bool) {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, workspaceDownloadMaxBody))
	if err != nil {
		return "", false
	}
	if len(strings.TrimSpace(string(body))) == 0 {
		return "", true
	}
	var req api.WorkspaceDownloadRequest
	if err := json.Unmarshal(body, &req); err != nil {
		return "", false
	}
	return req.Cursor, true
}

// workspaceDownloadAgentDeleting reports whether the agent is deleted or a
// delete has been requested for it (any deletion state). The gate is keyed on
// deletion, not on the lifecycle phase: a broker may download the workspace
// of an agent assigned to it at any phase until a delete begins.
func workspaceDownloadAgentDeleting(agent *store.Agent) bool {
	return !agent.DeletedAt.IsZero() || agent.DeletionState != store.DeletionStateNone
}
