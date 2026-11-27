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
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/agentcontrol"
	"github.com/GoogleCloudPlatform/scion/pkg/messages"
	"github.com/GoogleCloudPlatform/scion/pkg/msgaction"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// handleCommandRequestOutput serves GET /api/v1/command-requests/{id}/output
// (design §4.9, AC-S10):
//   - interactive human session only (no agent JWT, UAT/PAT, visitor, or federation)
//   - caller == run.ApproverUserID (and row.ApproverUserID)
//   - live agent.attach on run.ExecAgentID as caller (fails closed if deleted)
//   - Cache-Control: no-store
//
// This file (approver raw-output endpoint) and command_output_share.go
// (share_output-gated redacting helper) are the only allow-listed callers of
// GetActionRunOutput in pkg/hub (guarded by TestExecOutputReaders).
func (s *Server) handleCommandRequestOutput(w http.ResponseWriter, r *http.Request, reqID string) {
	user := s.messageActionHumanSessionCaller(w, r, msgaction.KindCommandRequest, reqID, "")
	if user == nil {
		return
	}
	ctx := r.Context()

	row, _, _, err := s.loadCommandRequestWithMessage(ctx, reqID, false)
	if err != nil {
		if errors.Is(err, store.ErrInvalidInput) || errors.Is(err, store.ErrNotFound) {
			NotFound(w, "Command request")
			return
		}
		writeErrorFromErr(w, err, "")
		return
	}

	run, err := s.store.GetCommandRunByRequestID(ctx, row.ID)
	if err != nil {
		if errors.Is(err, store.ErrInvalidInput) || errors.Is(err, store.ErrNotFound) {
			NotFound(w, "Command run output")
			return
		}
		writeErrorFromErr(w, err, "")
		return
	}

	if user.ID() != run.ApproverUserID || user.ID() != row.ApproverUserID {
		s.emitCommandRequestRefused(ctx, &MessageActionEvent{
			RowID:     row.ID,
			MessageID: row.MessageID,
			ProjectID: row.ProjectID,
			AgentID:   row.AgentID,
			AgentName: row.AgentName,
			Key:       row.Key,
			Version:   row.Version,
			ActorKind: MessageActionActorUser,
			ActorID:   user.ID(),
			Reason:    msgaction.ConflictCmdNotApprover,
			Details: map[string]string{
				"approver_user_id": run.ApproverUserID,
				"command_sha256":   row.CommandSHA256,
				"run_id":           run.ID,
			},
		})
		writeError(w, http.StatusForbidden, msgaction.ConflictCmdNotApprover,
			"only the named approver may view command output", nil)
		return
	}

	// Live agent.attach on exec_agent_id as the caller; fails closed when the
	// execution container is deleted or attach was revoked (design §4.9, AC-S10).
	if !s.newExecRevealer(ctx).reveals(run.ExecAgentID) {
		s.emitCommandRequestRefused(ctx, &MessageActionEvent{
			RowID:     row.ID,
			MessageID: row.MessageID,
			ProjectID: row.ProjectID,
			AgentID:   row.AgentID,
			AgentName: row.AgentName,
			Key:       row.Key,
			Version:   row.Version,
			ActorKind: MessageActionActorUser,
			ActorID:   user.ID(),
			Reason:    ErrCodeForbidden,
			Details: map[string]string{
				"approver_user_id": run.ApproverUserID,
				"command_sha256":   row.CommandSHA256,
				"exec_agent_id":    run.ExecAgentID,
				"exec_project_id":  run.ExecProjectID,
				"run_id":           run.ID,
			},
		})
		Forbidden(w)
		return
	}

	resp := messages.CommandRequestOutputResponse{
		RunID:        run.ID,
		CaptureMode:  run.CaptureMode,
		OutputStatus: run.OutputStatus,
		OutputBytes:  run.OutputBytes,
	}
	if run.OutputStatus == agentcontrol.ExecOutputCaptured || run.OutputStatus == agentcontrol.ExecOutputTruncated {
		content, err := s.store.GetActionRunOutput(ctx, run.ID)
		if err != nil && !errors.Is(err, store.ErrNotFound) {
			writeErrorFromErr(w, err, "")
			return
		}
		if len(content) > 0 {
			resp.Output = string(content)
		}
	}

	details := map[string]string{
		"command_sha256":   row.CommandSHA256,
		"exec_agent_id":    run.ExecAgentID,
		"exec_project_id":  run.ExecProjectID,
		"run_id":           run.ID,
		"approver_user_id": run.ApproverUserID,
		"share_output":     strconv.FormatBool(run.ShareOutput),
	}
	if run.OutputStatus != "" {
		details["output_status"] = run.OutputStatus
	}
	if run.OutputBytes != nil {
		details["output_bytes"] = strconv.FormatInt(*run.OutputBytes, 10)
	}
	s.emitMessageActionEvent(ctx, &MessageActionEvent{
		Kind:      msgaction.KindCommandRequest,
		EventType: msgaction.EventCmdOutputViewed,
		RowID:     row.ID,
		MessageID: row.MessageID,
		ProjectID: row.ProjectID,
		AgentID:   row.AgentID,
		AgentName: row.AgentName,
		Key:       row.Key,
		Version:   row.Version,
		ActorKind: MessageActionActorUser,
		ActorID:   user.ID(),
		Details:   details,
		Timestamp: time.Now().UTC(),
	})

	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, resp)
}
