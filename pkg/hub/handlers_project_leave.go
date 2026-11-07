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
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"

	"github.com/GoogleCloudPlatform/scion/pkg/experiments"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// ---------------------------------------------------------------------------
// POST /api/v1/projects/{id}/leave — project self-leave (ptone/scion#7494)
// ---------------------------------------------------------------------------

// leaveProjectRequest is the optional body of POST …/leave.
type leaveProjectRequest struct {
	// DryRun computes the LeaveResult and changes nothing.
	DryRun bool `json:"dryRun"`
}

// projectLeavePublish publishes the conduit authorization change after a
// leave commits, so the leaver's open streams in the project are re-checked
// and closed. A variable only so a mutant test can replace it with a no-op
// and see the stream-closure test fail (TestProjectLeave_ClosesLeaverConduitStreams_MutantNoPublish).
var projectLeavePublish = (*Server).publishConduitAuthzChanged

// projectLeavePublishMembership publishes the membership change after a
// leave commits, so the leaver's web events and agent message streams
// re-run their authorization. A variable only so a mutant test can replace
// it with a no-op and see the direct chat stream test fail
// (TestProjectLeave_ChatStreamClosesOnLeave_MutantNoPublish).
var projectLeavePublishMembership = (*Server).publishMembershipChanged

// handleProjectLeave serves POST /api/v1/projects/{id}/leave. The method
// is checked first, then the hub.project_self_leave experiment (404 while
// it is off), then the credential: an interactive session only, every
// access token is refused with the catalog's session-only reason. A caller
// who cannot read the project, or has no direct binding in it, gets the
// same 404 as for a project that does not exist.
func (s *Server) handleProjectLeave(w http.ResponseWriter, r *http.Request, projectID string) {
	if r.Method != http.MethodPost {
		MethodNotAllowed(w, http.MethodPost)
		return
	}
	s.requireExperiment(experiments.ProjectSelfLeave, func(w http.ResponseWriter, r *http.Request) {
		s.leaveProject(w, r, projectID)
	})(w, r)
}

// leaveProject runs a leave once the method and the experiment are checked.
func (s *Server) leaveProject(w http.ResponseWriter, r *http.Request, projectID string) {
	ctx := r.Context()
	actor, ok := s.requireSessionCredentialFor(w, ctx, leaveSessionOnlyReason)
	if !ok {
		return
	}

	// Self only: the leaving member is always the caller. The body names
	// no target; any field other than dryRun (a user, principal or binding
	// ID, for example) is refused, so no request can aim a leave at
	// another principal.
	var req leaveProjectRequest
	if r.Body != nil {
		dec := json.NewDecoder(r.Body)
		dec.DisallowUnknownFields()
		if err := dec.Decode(&req); err != nil && !errors.Is(err, io.EOF) {
			BadRequest(w, "invalid request body: "+err.Error())
			return
		}
	}

	project, err := s.store.GetProject(ctx, projectID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			NotFound(w, "Project")
			return
		}
		writeErrorFromErr(w, err, "")
		return
	}
	if !s.authzService.CheckAccess(ctx, actor, projectResource(project), ActionRead).Allowed {
		NotFound(w, "Project")
		return
	}

	if s.membershipService == nil {
		writeError(w, http.StatusInternalServerError, ErrCodeInternalError, "membership service not configured", nil)
		return
	}
	result, decision := s.membershipService.LeaveProject(ctx, actor, project.ID, req.DryRun)
	if decision != nil {
		slog.Info("project leave refused",
			"project_id", project.ID, "actor", actor.ID(), "dry_run", req.DryRun,
			"denial_code", decision.DenialCode, "reason", decision.Reason)
		if decision.HTTPStatus == http.StatusNotFound {
			NotFound(w, "Project")
			return
		}
		writeError(w, decision.HTTPStatus, decision.DenialCode, decision.Reason, decision.Details)
		return
	}
	if !req.DryRun {
		projectLeavePublish(s, conduitAuthzMatch{UserID: actor.ID(), ProjectID: project.ID})
		projectLeavePublishMembership(s, actor.ID(), project.ID)
	}
	writeJSON(w, http.StatusOK, result)
}

// projectLeaveCapabilities returns the caller's leave capabilities on the
// project while hub.project_self_leave is on: canLeave, and the reason a
// leave is blocked when there is one. With the experiment off, or without
// a membership service, it returns nil and "", and the fields are left
// out of the response.
func (s *Server) projectLeaveCapabilities(ctx context.Context, userID, projectID string) (*bool, string) {
	if s.membershipService == nil || !s.experimentEnabled(experiments.ProjectSelfLeave) {
		return nil, ""
	}
	can, reason := s.membershipService.leaveCapabilities(ctx, userID, projectID)
	return &can, reason
}
