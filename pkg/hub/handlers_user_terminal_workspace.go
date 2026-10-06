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
	"log/slog"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/hub/authzop"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// =============================================================================
// Per-user terminal workspace persistence (/api/v1/users/me/terminal-workspace)
//
// Stores the ordered list of open terminal agents the terminal viewer
// (/terminals) restores on open, plus which one was frontmost.
// =============================================================================

// terminalWorkspaceMaxAgentIDs is the maximum number of entries accepted on a
// PUT. The rail has no cap today; this is well above realistic use and bounds
// the per-read access checks below.
const terminalWorkspaceMaxAgentIDs = 32

// terminalWorkspaceMaxBodyBytes bounds the PUT request body. 32 IDs at 38
// bytes plus JSON overhead is under 2 KiB; 16 KiB leaves ample margin.
const terminalWorkspaceMaxBodyBytes = 16 * 1024

// canonicalUUIDPattern matches the same canonical (8-4-4-4-12, hyphenated)
// UUID form the web client validates (UUID_RE in terminal-layout.ts). Only
// this form is accepted; braces, URNs and bare 32-hex forms are rejected.
var canonicalUUIDPattern = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

// terminalWorkspaceResponse is the wire shape for both GET and PUT.
type terminalWorkspaceResponse struct {
	AgentIDs         []string   `json:"agentIds"`
	FrontmostAgentID *string    `json:"frontmostAgentId"`
	Revision         int64      `json:"revision"`
	UpdatedAt        *time.Time `json:"updatedAt"`
	Pruned           int        `json:"pruned"`
}

// emptyTerminalWorkspaceResponse is returned when the user has never saved a
// workspace. "Never saved" and "saved empty" both mean "start empty", so this
// is a 200, not a 404.
func emptyTerminalWorkspaceResponse() terminalWorkspaceResponse {
	return terminalWorkspaceResponse{
		AgentIDs:         []string{},
		FrontmostAgentID: nil,
		Revision:         0,
		UpdatedAt:        nil,
		Pruned:           0,
	}
}

// handleUserMeTerminalWorkspace routes GET/PUT on
// /api/v1/users/me/terminal-workspace. Both methods require an interactive
// session or dev credential: the path names no user, so the subject is
// always the authenticated caller. There is no admin override.
func (s *Server) handleUserMeTerminalWorkspace(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	actor, ok := s.requireSessionCredentialFor(w, ctx, authzop.ReasonInteractiveState)
	if !ok {
		return
	}

	switch r.Method {
	case http.MethodGet:
		s.getUserTerminalWorkspace(w, r, actor)
	case http.MethodPut:
		s.putUserTerminalWorkspace(w, r, actor)
	default:
		MethodNotAllowed(w, http.MethodGet, http.MethodPut)
	}
}

// getUserTerminalWorkspace handles GET. Pruning is authoritative here:
// agents that no longer exist, are soft-deleted, or that the caller can no
// longer attach to are dropped from the response. GET has no side effects —
// the pruned list is not written back; the client does that.
func (s *Server) getUserTerminalWorkspace(w http.ResponseWriter, r *http.Request, actor UserIdentity) {
	ctx := r.Context()

	row, err := s.store.GetUserTerminalWorkspace(ctx, actor.ID())
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeJSON(w, http.StatusOK, emptyTerminalWorkspaceResponse())
			return
		}
		slog.Error("failed to load terminal workspace", "user_id", actor.ID(), "error", err)
		writeError(w, http.StatusInternalServerError, ErrCodeInternalError, "failed to load terminal workspace", nil)
		return
	}

	if len(row.AgentIDs) == 0 {
		writeJSON(w, http.StatusOK, terminalWorkspaceResponse{
			AgentIDs:         []string{},
			FrontmostAgentID: nil,
			Revision:         row.Revision,
			UpdatedAt:        terminalWorkspaceTimePtr(row.Updated),
			Pruned:           0,
		})
		return
	}

	agents, err := s.store.GetAgentsByIDs(ctx, row.AgentIDs)
	if err != nil {
		slog.Error("failed to resolve terminal workspace agents", "user_id", actor.ID(), "error", err)
		writeError(w, http.StatusInternalServerError, ErrCodeInternalError, "failed to resolve terminal workspace agents", nil)
		return
	}

	survivors := make([]string, 0, len(row.AgentIDs))
	for _, id := range row.AgentIDs {
		agent, found := agents[id]
		if !found {
			// Missing or soft-deleted (GetAgentsByIDs already excludes
			// soft-deleted rows via DeletedAtIsNil): drop.
			continue
		}
		decision := s.authzService.CheckAccess(ctx, actor, agentResource(agent), ActionAttach)
		if decision.Allowed {
			survivors = append(survivors, id)
			continue
		}
		if decision.IsIndeterminate() {
			// An indeterminate answer keeps the entry and is not counted as
			// pruned: a store fault must not turn into permanent loss of the
			// list once the client writes back.
			survivors = append(survivors, id)
			continue
		}
		// Denied by policy: drop. Stopped/errored agents are not denied by
		// this check (ActionAttach does not depend on run state), so they
		// are kept.
	}

	pruned := len(row.AgentIDs) - len(survivors)

	var frontmost *string
	if row.FrontmostAgentID != "" && containsString(survivors, row.FrontmostAgentID) {
		frontmost = terminalWorkspaceStrPtr(row.FrontmostAgentID)
	}

	writeJSON(w, http.StatusOK, terminalWorkspaceResponse{
		AgentIDs:         survivors,
		FrontmostAgentID: frontmost,
		Revision:         row.Revision,
		UpdatedAt:        terminalWorkspaceTimePtr(row.Updated),
		Pruned:           pruned,
	})
}

// terminalWorkspacePutRequest is the strict PUT body. Unknown fields are
// rejected. AgentIDs is a pointer so a missing field is distinguishable from
// an explicit empty list: a client bug that omits the field (or sends a
// JSON null) must not silently wipe the saved list.
type terminalWorkspacePutRequest struct {
	AgentIDs         *[]string `json:"agentIds"`
	FrontmostAgentID *string   `json:"frontmostAgentId"`
}

// putUserTerminalWorkspace handles PUT. The write is unconditional
// (last-writer-wins): there is no If-Match or revision in the request. PUT
// does not prune and does not check that agents exist — a well-formed but
// non-existent UUID is accepted, so the endpoint cannot be used as an
// existence oracle.
func (s *Server) putUserTerminalWorkspace(w http.ResponseWriter, r *http.Request, actor UserIdentity) {
	ctx := r.Context()

	r.Body = http.MaxBytesReader(w, r.Body, terminalWorkspaceMaxBodyBytes)

	var body terminalWorkspacePutRequest
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&body); err != nil {
		BadRequest(w, "invalid JSON body")
		return
	}
	// Reject trailing content after the JSON object (for example a second
	// object, or garbage appended to the body): only one JSON value is ever
	// valid here.
	if err := dec.Decode(new(json.RawMessage)); err != io.EOF {
		BadRequest(w, "body must contain a single JSON object")
		return
	}

	if body.AgentIDs == nil {
		ValidationError(w, "agentIds is required", nil)
		return
	}
	agentIDsIn := *body.AgentIDs

	if len(agentIDsIn) > terminalWorkspaceMaxAgentIDs {
		ValidationError(w, "agentIds must contain at most 32 entries", nil)
		return
	}

	agentIDs := make([]string, len(agentIDsIn))
	seen := make(map[string]bool, len(agentIDsIn))
	for i, id := range agentIDsIn {
		if !canonicalUUIDPattern.MatchString(id) {
			ValidationError(w, "agentIds must contain canonical UUIDs", nil)
			return
		}
		lower := strings.ToLower(id)
		if seen[lower] {
			ValidationError(w, "agentIds must not contain duplicates", nil)
			return
		}
		seen[lower] = true
		agentIDs[i] = lower
	}

	var frontmostAgentID string
	if body.FrontmostAgentID != nil {
		frontmostAgentID = strings.ToLower(*body.FrontmostAgentID)
		if len(agentIDs) == 0 {
			ValidationError(w, "frontmostAgentId must be null when agentIds is empty", nil)
			return
		}
		if !containsString(agentIDs, frontmostAgentID) {
			ValidationError(w, "frontmostAgentId must be a member of agentIds", nil)
			return
		}
	}

	row, err := s.store.PutUserTerminalWorkspace(ctx, actor.ID(), agentIDs, frontmostAgentID)
	if err != nil {
		slog.Error("failed to save terminal workspace", "user_id", actor.ID(), "error", err)
		writeError(w, http.StatusInternalServerError, ErrCodeInternalError, "failed to save terminal workspace", nil)
		return
	}

	var frontmost *string
	if row.FrontmostAgentID != "" {
		frontmost = terminalWorkspaceStrPtr(row.FrontmostAgentID)
	}

	writeJSON(w, http.StatusOK, terminalWorkspaceResponse{
		AgentIDs:         row.AgentIDs,
		FrontmostAgentID: frontmost,
		Revision:         row.Revision,
		UpdatedAt:        terminalWorkspaceTimePtr(row.Updated),
		Pruned:           0,
	})
}

func terminalWorkspaceStrPtr(s string) *string { return &s }

func terminalWorkspaceTimePtr(t time.Time) *time.Time { return &t }
