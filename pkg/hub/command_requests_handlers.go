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
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/experiments"
	"github.com/GoogleCloudPlatform/scion/pkg/messages"
	"github.com/GoogleCloudPlatform/scion/pkg/msgaction"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

const (
	commandRequestsRoutePrefix   = "/api/v1/command-requests/"
	agentCommandRequestsExactURL = "/api/v1/agent/command-requests"

	// commandRunWakeBudget is the wake + readiness budget used to set the
	// initial command_runs.lease_until at approval (§4.7 step 5, §4.8 step 3).
	commandRunWakeBudget         = 5 * time.Minute
	commandRunApprovalLeaseGrace = 60 * time.Second
)

// CommandRunLauncherFunc adapts a plain function to CommandRunLauncher.
type CommandRunLauncherFunc func(req *store.CommandRequest, run *store.CommandRun)

// EnqueueCommandRun invokes f(req, run).
func (f CommandRunLauncherFunc) EnqueueCommandRun(req *store.CommandRequest, run *store.CommandRun) {
	f(req, run)
}

// SetCommandRunLauncher wires the post-commit executor launcher for approved
// command runs.
func (s *Server) SetCommandRunLauncher(l CommandRunLauncher) {
	s.mu.Lock()
	s.commandRunLauncher = l
	s.mu.Unlock()
}

func (s *Server) enqueueCommandRun(req *store.CommandRequest, run *store.CommandRun) {
	s.mu.RLock()
	l := s.commandRunLauncher
	s.mu.RUnlock()
	if l != nil {
		l.EnqueueCommandRun(req, run)
		return
	}
	s.EnqueueCommandRun(req, run)
}

// handleCommandRequestRoutes serves the responder command-request endpoints,
// 404 while hub.command_requests is off:
//
//	POST /api/v1/command-requests/from-message
//	GET  /api/v1/command-requests/{id}
//	POST /api/v1/command-requests/{id}/approve
//	GET  /api/v1/command-requests/{id}/output
func (s *Server) handleCommandRequestRoutes(w http.ResponseWriter, r *http.Request) {
	s.requireExperiment(experiments.CommandRequests, func(w http.ResponseWriter, r *http.Request) {
		rest := strings.TrimPrefix(r.URL.Path, commandRequestsRoutePrefix)
		reqID, sub, hasSlash := strings.Cut(rest, "/")
		if reqID == "" {
			NotFound(w, "Command request")
			return
		}
		if reqID == "from-message" {
			if hasSlash && sub != "" {
				NotFound(w, "Command request")
				return
			}
			if r.Method != http.MethodPost {
				MethodNotAllowed(w, http.MethodPost)
				return
			}
			s.handleCommandRequestFromMessage(w, r)
			return
		}
		if !hasSlash || sub == "" {
			if r.Method != http.MethodGet {
				MethodNotAllowed(w, http.MethodGet)
				return
			}
			s.handleCommandRequestGet(w, r, reqID)
			return
		}
		if strings.Contains(sub, "/") {
			NotFound(w, "Command request")
			return
		}
		switch sub {
		case "approve":
			if r.Method != http.MethodPost {
				MethodNotAllowed(w, http.MethodPost)
				return
			}
			s.handleCommandRequestApprove(w, r, reqID)
		case "output":
			if r.Method != http.MethodGet {
				MethodNotAllowed(w, http.MethodGet)
				return
			}
			s.handleCommandRequestOutput(w, r, reqID)
		default:
			NotFound(w, "Command request")
		}
	})(w, r)
}

// handleAgentCommandRequestRoutes serves the agent self-service command-request
// endpoint, 404 while hub.command_requests is off:
//
//	GET /api/v1/agent/command-requests
func (s *Server) handleAgentCommandRequestRoutes(w http.ResponseWriter, r *http.Request) {
	s.requireExperiment(experiments.CommandRequests, func(w http.ResponseWriter, r *http.Request) {
		agentIdent := agentDecisionCaller(w, r)
		if agentIdent == nil {
			return
		}
		if r.URL.Path != agentCommandRequestsExactURL {
			NotFound(w, "Command request")
			return
		}
		if r.Method != http.MethodGet {
			MethodNotAllowed(w, http.MethodGet)
			return
		}
		s.handleAgentCommandRequestList(w, r, agentIdent)
	})(w, r)
}

func (s *Server) emitCommandRequestRefused(ctx context.Context, ev *MessageActionEvent) {
	if ev == nil {
		return
	}
	ev.Kind = msgaction.KindCommandRequest
	ev.EventType = MessageActionEventRefused
	if ev.Timestamp.IsZero() {
		ev.Timestamp = time.Now().UTC()
	}
	s.emitMessageActionEvent(ctx, ev)
}

func normalizeCommandRequestSurface(raw string) (string, bool) {
	switch strings.TrimSpace(raw) {
	case "", msgaction.CommandRequestSurfaceNative:
		return msgaction.CommandRequestSurfaceNative, true
	case msgaction.CommandRequestSurfaceStandalonePage:
		return msgaction.CommandRequestSurfaceStandalonePage, true
	case msgaction.CommandRequestSurfaceCLI:
		return msgaction.CommandRequestSurfaceCLI, true
	default:
		return "", false
	}
}

func (s *Server) loadCommandRequestWithMessage(ctx context.Context, reqID string, allowMessageIDFallback bool) (*store.CommandRequest, *store.Message, string, error) {
	row, err := s.store.GetCommandRequest(ctx, reqID)
	if err != nil && allowMessageIDFallback && errors.Is(err, store.ErrNotFound) {
		row, err = s.store.GetCommandRequestByMessageID(ctx, reqID)
	}
	if err != nil {
		return nil, nil, "", err
	}
	var promptMsg *store.Message
	if row.MessageID != "" {
		if m, merr := s.store.GetMessage(ctx, row.MessageID); merr == nil && m != nil {
			promptMsg = m
		}
	}
	chatKey := row.ChatKey
	if chatKey == "" && promptMsg != nil {
		chatKey = promptMsg.ThreadID
	}
	return row, promptMsg, chatKey, nil
}

func (s *Server) isCommandRequestRecipient(ctx context.Context, user UserIdentity, row *store.CommandRequest, chatKey string, promptMsg *store.Message, answering bool) bool {
	if user == nil || row == nil {
		return false
	}
	if chatKey != "" || promptMsg != nil {
		return s.isMessageActionRecipient(ctx, user, chatKey, promptMsg, answering)
	}
	return user.ID() == row.ApproverUserID
}

func (s *Server) handleCommandRequestGet(w http.ResponseWriter, r *http.Request, reqID string) {
	user := s.messageActionHumanSessionCaller(w, r, msgaction.KindCommandRequest, reqID, "")
	if user == nil {
		return
	}
	ctx := r.Context()
	row, promptMsg, chatKey, err := s.loadCommandRequestWithMessage(ctx, reqID, true)
	if err != nil {
		if errors.Is(err, store.ErrInvalidInput) || errors.Is(err, store.ErrNotFound) {
			NotFound(w, "Command request")
			return
		}
		writeErrorFromErr(w, err, "")
		return
	}
	if !s.isCommandRequestRecipient(ctx, user, row, chatKey, promptMsg, false) {
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
		})
		Forbidden(w)
		return
	}

	var replacedBy *store.CommandRequest
	if row.ReplacedByID != "" {
		if rb, rerr := s.store.GetCommandRequest(ctx, row.ReplacedByID); rerr == nil {
			replacedBy = rb
		}
	}
	var runRow *store.CommandRun
	if run, rerr := s.store.GetCommandRunByRequestID(ctx, row.ID); rerr == nil {
		runRow = run
	}

	now := time.Now().UTC()
	isApprover := user.ID() == row.ApproverUserID
	canApprove := isApprover && row.Status == store.CommandRequestStatusPending && row.ExpiresAt.After(now)
	reason := ""
	if !isApprover {
		reason = msgaction.ConflictCmdNotApprover
	} else if row.Status != store.CommandRequestStatusPending {
		reason = msgaction.ConflictCode(msgaction.CommandRequestKindSpec, row.Status)
	} else if !row.ExpiresAt.After(now) {
		reason = msgaction.ConflictExpired
	}

	var execView *messages.CommandRequestExecContainerView
	if isApprover {
		res, rerr := s.resolveExecContainerForIdentity(ctx, user)
		if rerr != nil {
			execView = &messages.CommandRequestExecContainerView{
				State:  string(ExecContainerStateUnavailable),
				Reason: "lookup_error",
			}
		} else {
			execView = s.buildExecContainerView(ctx, user, res)
		}
	}

	projectName := ""
	if row.ProjectID != "" {
		if proj, perr := s.store.GetProject(ctx, row.ProjectID); perr == nil && proj != nil {
			projectName = proj.Name
			if projectName == "" {
				projectName = proj.Slug
			}
		}
	}
	intro := ""
	if promptMsg != nil && row.IntroLength > 0 && row.IntroLength <= len(promptMsg.Msg) {
		intro = promptMsg.Msg[:row.IntroLength]
	}
	approverName := ""
	approverEmail := ""
	if row.ApproverUserID != "" {
		if approver, uerr := s.store.GetUser(ctx, row.ApproverUserID); uerr == nil && approver != nil {
			approverName = approver.DisplayName
			approverEmail = approver.Email
		}
	}
	var prevUnknown *messages.CommandRequestPreviousUnknown
	if row.Version > 1 {
		if listRes, lerr := s.store.ListAgentCommandRequests(ctx, &store.ListAgentCommandRequestsOptions{
			AgentID: row.AgentID,
			Key:     row.Key,
			Limit:   store.DefaultCommandRequestListLimit,
		}); lerr == nil && listRes != nil {
			for _, item := range listRes.Items {
				if item.Version == row.Version-1 {
					if prevRun := listRes.Runs[item.ID]; prevRun != nil && prevRun.Status == store.CommandRunStatusUnknown {
						prevUnknown = &messages.CommandRequestPreviousUnknown{
							Version: item.Version,
							RowID:   item.ID,
							RunID:   prevRun.ID,
						}
					}
					break
				}
			}
		}
	}

	outputAvailable := false
	if isApprover && runRow != nil && runRow.FinishedAt != nil && user.ID() == runRow.ApproverUserID {
		if has, herr := s.store.HasActionRunOutput(ctx, runRow.ID); herr == nil && has {
			outputAvailable = true
		}
	}

	runView := commandRunView(runRow)
	writeJSON(w, http.StatusOK, messages.CommandRequestDetailResponse{
		Request:                commandRequestView(row, replacedBy, runRow),
		Run:                    runView,
		ExecContainer:          execView,
		ProjectName:            projectName,
		ThreadID:               chatKey,
		Intro:                  intro,
		ApproverName:           approverName,
		ApproverEmail:          approverEmail,
		PreviousVersionUnknown: prevUnknown,
		CanApprove:             canApprove,
		OutputAvailable:        outputAvailable,
		Reason:                 reason,
	})
}

func (s *Server) buildExecContainerView(ctx context.Context, actor UserIdentity, res ExecContainerResolution) *messages.CommandRequestExecContainerView {
	v := &messages.CommandRequestExecContainerView{
		State:          string(res.State),
		HomePersistent: res.HomePersistent,
	}
	if res.State == ExecContainerStateSuspended && res.Agent != nil && actor != nil && !isManagedAgentRuntime(res.Agent.Runtime) {
		v.CanWake = s.agentLifecycleAllowed(ctx, actor, res.Agent)
	}
	if res.Agent != nil {
		v.AgentID = res.Agent.ID
		v.AgentName = agentDisplayName(res.Agent)
		v.ProjectID = res.Agent.ProjectID
		if res.Agent.ProjectID != "" {
			if proj, err := s.store.GetProject(ctx, res.Agent.ProjectID); err == nil && proj != nil {
				v.ProjectName = proj.Name
				if v.ProjectName == "" {
					v.ProjectName = proj.Slug
				}
			}
		}
	}
	return v
}

func (s *Server) handleCommandRequestApprove(w http.ResponseWriter, r *http.Request, reqID string) {
	if pendingSecretBrowserRequestRefused(w, r) {
		return
	}
	// Step 2: interactive human session caller only.
	user := s.messageActionHumanSessionCaller(w, r, msgaction.KindCommandRequest, reqID, "")
	if user == nil {
		return
	}
	ctx := r.Context()

	r.Body = http.MaxBytesReader(w, r.Body, msgaction.MaxRequestBodyBytes)
	var body messages.ApproveCommandRequestBody
	dec := json.NewDecoder(r.Body)
	if err := dec.Decode(&body); err != nil && !errors.Is(err, io.EOF) {
		BadRequest(w, "Invalid request body: "+err.Error())
		return
	}
	if body.RowID != "" && body.RowID != reqID {
		ValidationError(w, "row_id does not match URL id", map[string]interface{}{"field": "row_id"})
		return
	}
	hashHex := strings.ToLower(strings.TrimSpace(body.CommandSHA256))
	if len(hashHex) != 64 {
		ValidationError(w, "command_sha256 must be a 64-character lowercase hex SHA-256 digest", map[string]interface{}{"field": "command_sha256"})
		return
	}
	if _, err := hex.DecodeString(hashHex); err != nil {
		ValidationError(w, "command_sha256 must be valid hex", map[string]interface{}{"field": "command_sha256"})
		return
	}
	surface, ok := normalizeCommandRequestSurface(body.Surface)
	if !ok {
		ValidationError(w, "unsupported surface", map[string]interface{}{"field": "surface"})
		return
	}
	if len(body.IdempotencyKey) > msgaction.MaxIdempotencyKeyLen {
		ValidationError(w, "idempotency_key exceeds maximum length", map[string]interface{}{"field": "idempotency_key"})
		return
	}

	row, promptMsg, chatKey, err := s.loadCommandRequestWithMessage(ctx, reqID, false)
	if err != nil {
		if errors.Is(err, store.ErrInvalidInput) || errors.Is(err, store.ErrNotFound) {
			NotFound(w, "Command request")
			return
		}
		writeErrorFromErr(w, err, "")
		return
	}

	emitRefused := func(reason string, details map[string]string) {
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
			Reason:    reason,
			Details:   details,
		})
	}

	// Step 3: caller must be a recipient (answering=true) AND the named approver.
	if !s.isCommandRequestRecipient(ctx, user, row, chatKey, promptMsg, true) || user.ID() != row.ApproverUserID {
		emitRefused(msgaction.ConflictCmdNotApprover, map[string]string{
			"approver_user_id": row.ApproverUserID,
			"command_sha256":   row.CommandSHA256,
		})
		writeError(w, http.StatusForbidden, msgaction.ConflictCmdNotApprover,
			"only the named approver may approve this command request", nil)
		return
	}

	// Step 4: resolve caller's execution container.
	execRes, ok := s.resolveExecContainerForApproval(ctx, w, user, row.CommandSHA256, emitRefused)
	if !ok {
		return
	}

	// Step 5: single transaction — CAS pending -> approved + INSERT command_runs(queued).
	// Share output is explicit opt-in only: omitting share_output defaults to
	// false so older clients never share output without an explicit approver choice (M1).
	shareOutput := body.ShareOutput != nil && *body.ShareOutput
	now := time.Now().UTC().Truncate(time.Microsecond)
	appRes, err := s.store.ApproveCommandRequest(ctx, &store.ApproveCommandRequestInput{
		RequestID:      row.ID,
		CommandSHA256:  hashHex,
		UserID:         user.ID(),
		UserName:       user.DisplayName(),
		UserEmail:      user.Email(),
		Surface:        surface,
		IdempotencyKey: body.IdempotencyKey,
		ExecAgentID:    execRes.Agent.ID,
		ExecProjectID:  execRes.Agent.ProjectID,
		ShareOutput:    shareOutput,
		LeaseUntil:     now.Add(commandRunWakeBudget + commandRunApprovalLeaseGrace),
		Now:            now,
	})
	if err != nil {
		writeErrorFromErr(w, err, "")
		return
	}
	if appRes.Conflict != nil {
		emitRefused(appRes.ConflictCode, map[string]string{
			"command_sha256":   row.CommandSHA256,
			"approver_user_id": user.ID(),
		})
		writeError(w, http.StatusConflict, appRes.ConflictCode,
			"command request cannot be approved: "+appRes.ConflictCode,
			map[string]interface{}{
				"status": appRes.Conflict.Status,
			})
		return
	}

	reqView := commandRequestView(appRes.Request, nil, appRes.Run)
	runView := *commandRunView(appRes.Run)

	// Step 6: idempotent replay returns 200 without re-launching or re-auditing.
	if appRes.Replayed {
		writeJSON(w, http.StatusOK, messages.ApproveCommandRequestResponse{
			Request:  reqView,
			Run:      runView,
			Replayed: true,
		})
		return
	}

	// Step 7: post-commit SSE card update, audit event, and handoff to executor.
	if chatKey != "" {
		s.publishActionUpdated(msgaction.KindCommandRequest, appRes.Request.ProjectID, appRes.Request.MessageID, chatKey, commandRequestSSEView(appRes.Request, nil, appRes.Run))
	}
	s.emitMessageActionEvent(ctx, &MessageActionEvent{
		Kind:      msgaction.KindCommandRequest,
		EventType: msgaction.CommandRequestResolvedStatus,
		RowID:     appRes.Request.ID,
		MessageID: appRes.Request.MessageID,
		ProjectID: appRes.Request.ProjectID,
		AgentID:   appRes.Request.AgentID,
		AgentName: appRes.Request.AgentName,
		Key:       appRes.Request.Key,
		Version:   appRes.Request.Version,
		ActorKind: MessageActionActorUser,
		ActorID:   user.ID(),
		Details: map[string]string{
			"command_sha256":   appRes.Request.CommandSHA256,
			"exec_agent_id":    appRes.Run.ExecAgentID,
			"exec_project_id":  appRes.Run.ExecProjectID,
			"run_id":           appRes.Run.ID,
			"approver_user_id": user.ID(),
			"workdir_kind":     messages.CommandWorkdirKind(appRes.Request.Workdir),
			"share_output":     strconv.FormatBool(appRes.Run.ShareOutput),
		},
		Timestamp: now,
	})
	s.enqueueCommandRun(appRes.Request, appRes.Run)

	writeJSON(w, http.StatusAccepted, messages.ApproveCommandRequestResponse{
		Request: reqView,
		Run:     runView,
	})
}

func (s *Server) resolveExecContainerForApproval(
	ctx context.Context,
	w http.ResponseWriter,
	user UserIdentity,
	commandSHA256 string,
	emitRefused func(reason string, details map[string]string),
) (ExecContainerResolution, bool) {
	execRes, err := s.resolveExecContainerForIdentity(ctx, user)
	if err != nil {
		writeErrorFromErr(w, err, "")
		return ExecContainerResolution{}, false
	}
	switch execRes.State {
	case ExecContainerStateUnconfigured:
		emitRefused(msgaction.ConflictCmdExecUnconfigured, map[string]string{
			"command_sha256":   commandSHA256,
			"approver_user_id": user.ID(),
		})
		writeError(w, http.StatusConflict, msgaction.ConflictCmdExecUnconfigured,
			"no execution container is configured; configure one in your profile first", nil)
		return ExecContainerResolution{}, false
	case ExecContainerStateUnavailable:
		emitRefused(msgaction.ConflictCmdExecUnavailable, map[string]string{
			"command_sha256":   commandSHA256,
			"approver_user_id": user.ID(),
		})
		writeError(w, http.StatusConflict, msgaction.ConflictCmdExecUnavailable,
			"your execution container is unavailable", nil)
		return ExecContainerResolution{}, false
	case ExecContainerStateStopped:
		details := map[string]string{
			"command_sha256":   commandSHA256,
			"approver_user_id": user.ID(),
		}
		if execRes.Agent != nil {
			details["exec_agent_id"] = execRes.Agent.ID
			details["exec_project_id"] = execRes.Agent.ProjectID
		}
		emitRefused(msgaction.ConflictCmdExecStopped, details)
		writeError(w, http.StatusConflict, msgaction.ConflictCmdExecStopped,
			"Start your execution container, then approve", nil)
		return ExecContainerResolution{}, false
	case ExecContainerStateReady, ExecContainerStateStarting, ExecContainerStateSuspended:
		if execRes.Agent == nil {
			emitRefused(msgaction.ConflictCmdExecUnavailable, map[string]string{
				"command_sha256":   commandSHA256,
				"approver_user_id": user.ID(),
			})
			writeError(w, http.StatusConflict, msgaction.ConflictCmdExecUnavailable,
				"your execution container is unavailable", nil)
			return ExecContainerResolution{}, false
		}
		if execRes.State == ExecContainerStateSuspended && !s.agentLifecycleAllowed(ctx, user, execRes.Agent) {
			emitRefused(msgaction.ConflictCmdExecUnavailable, map[string]string{
				"command_sha256":     commandSHA256,
				"approver_user_id":   user.ID(),
				"exec_agent_id":      execRes.Agent.ID,
				"exec_project_id":    execRes.Agent.ProjectID,
				"not_started_reason": "wake_not_permitted",
			})
			writeError(w, http.StatusConflict, msgaction.ConflictCmdExecUnavailable,
				"your execution container is suspended and you do not have permission to resume it",
				map[string]interface{}{"detail": "wake_not_permitted"})
			return ExecContainerResolution{}, false
		}
		return execRes, true
	default:
		emitRefused(msgaction.ConflictCmdExecUnavailable, map[string]string{
			"command_sha256":   commandSHA256,
			"approver_user_id": user.ID(),
		})
		writeError(w, http.StatusConflict, msgaction.ConflictCmdExecUnavailable,
			"your execution container is unavailable", nil)
		return ExecContainerResolution{}, false
	}
}

// handleCommandRequestFromMessage serves POST /api/v1/command-requests/from-message
// (§4.14 steps 1-6): create-and-resolve a version-1 command request from a
// verbatim fenced shell block in an agent-authored chat message.
func (s *Server) handleCommandRequestFromMessage(w http.ResponseWriter, r *http.Request) {
	if pendingSecretBrowserRequestRefused(w, r) {
		return
	}
	// Step 1: interactive human session caller only.
	user := s.messageActionHumanSessionCaller(w, r, msgaction.KindCommandRequest, "", "")
	if user == nil {
		return
	}
	ctx := r.Context()

	r.Body = http.MaxBytesReader(w, r.Body, msgaction.MaxRequestBodyBytes)
	var body messages.CreateCommandRequestFromMessageBody
	dec := json.NewDecoder(r.Body)
	if err := dec.Decode(&body); err != nil {
		BadRequest(w, "Invalid request body: "+err.Error())
		return
	}
	msgID := strings.TrimSpace(body.MessageID)
	if msgID == "" {
		ValidationError(w, "message_id is required", map[string]interface{}{"field": "message_id"})
		return
	}
	hashHex := strings.ToLower(strings.TrimSpace(body.CommandSHA256))
	if len(hashHex) != 64 {
		ValidationError(w, "command_sha256 must be a 64-character lowercase hex SHA-256 digest", map[string]interface{}{"field": "command_sha256"})
		return
	}
	if _, err := hex.DecodeString(hashHex); err != nil {
		ValidationError(w, "command_sha256 must be valid hex", map[string]interface{}{"field": "command_sha256"})
		return
	}
	if len(body.IdempotencyKey) > msgaction.MaxIdempotencyKeyLen {
		ValidationError(w, "idempotency_key exceeds maximum length", map[string]interface{}{"field": "idempotency_key"})
		return
	}

	// Step 2: load message and verify recipient + sender + age + non-card.
	msg, err := s.store.GetMessage(ctx, msgID)
	if err != nil {
		if errors.Is(err, store.ErrInvalidInput) || errors.Is(err, store.ErrNotFound) {
			NotFound(w, "Message")
			return
		}
		writeErrorFromErr(w, err, "")
		return
	}

	chatKey := strings.TrimSpace(msg.ThreadID)
	if chatKey == "" && strings.HasPrefix(msg.Sender, "agent:") && msg.SenderID != "" &&
		strings.HasPrefix(msg.Recipient, "user:") && msg.RecipientID != "" {
		if dmKey, dmErr := messages.DMConversationKey("agent", msg.SenderID, "user", msg.RecipientID); dmErr == nil {
			chatKey = dmKey
		}
	}

	agentName := strings.TrimPrefix(msg.Sender, "agent:")
	emitRefused := func(reason string, details map[string]string) {
		s.emitCommandRequestRefused(ctx, &MessageActionEvent{
			MessageID: msg.ID,
			ProjectID: msg.ProjectID,
			AgentID:   msg.SenderID,
			AgentName: agentName,
			ActorKind: MessageActionActorUser,
			ActorID:   user.ID(),
			Reason:    reason,
			Details:   details,
		})
	}

	// Caller must be a recipient of the message's DM or conversation first.
	if !s.isMessageActionRecipient(ctx, user, chatKey, msg, true) {
		emitRefused(msgaction.ConflictCmdNotApprover, map[string]string{
			"approver_user_id": user.ID(),
		})
		writeError(w, http.StatusForbidden, msgaction.ConflictCmdNotApprover,
			"you are not a recipient of this message", nil)
		return
	}

	// Message sender must be an agent, and the message must be an ordinary
	// agent chat message (allow-list: "", "instruction", "reply") — never a
	// command-request card, notification/state-change, decision prompt, or
	// other hub-composed row (audit L1, §4.14).
	if !strings.HasPrefix(msg.Sender, "agent:") || msg.SenderID == "" || !isMarkdownExecuteAllowedMessageType(msg.Type) {
		emitRefused(msgaction.ConflictCmdMarkdownBlock, map[string]string{
			"approver_user_id": user.ID(),
		})
		writeError(w, http.StatusUnprocessableEntity, msgaction.ConflictCmdMarkdownBlock,
			"Ask the agent to send this as a command request", nil)
		return
	}

	// Caller must be the DM user or the single addressed human in a group/thread (D6).
	if !s.isSingleAddressedHumanForMarkdownExecute(ctx, user, msg, chatKey) {
		emitRefused(msgaction.ConflictCmdNotApprover, map[string]string{
			"approver_user_id": user.ID(),
		})
		writeError(w, http.StatusForbidden, msgaction.ConflictCmdNotApprover,
			"this message is not addressed to you alone; ask the agent to send a command request", nil)
		return
	}

	// Message must be younger than 24 h (D4).
	now := time.Now().UTC().Truncate(time.Microsecond)
	if !msg.CreatedAt.Add(msgaction.CommandRequestTTL).After(now) {
		emitRefused(msgaction.ConflictExpired, map[string]string{
			"approver_user_id": user.ID(),
		})
		writeError(w, http.StatusConflict, msgaction.ConflictExpired,
			"this message is older than 24 hours and can no longer be executed", nil)
		return
	}

	// Step 3: send-time script validation, verbatim-fence check, and canonical hash check.
	if err := messages.ValidateCommandRequestScript(body.Script); err != nil {
		emitRefused(ErrCodeValidationError, map[string]string{
			"approver_user_id": user.ID(),
		})
		ValidationError(w, err.Error(), map[string]interface{}{"field": "script"})
		return
	}
	if !messages.HasVerbatimExecutableShellFence(msg.Msg, body.Script) {
		emitRefused(msgaction.ConflictCmdMarkdownBlock, map[string]string{
			"approver_user_id": user.ID(),
		})
		writeError(w, http.StatusUnprocessableEntity, msgaction.ConflictCmdMarkdownBlock,
			"Ask the agent to send this as a command request", nil)
		return
	}
	serverHash := messages.CommandRequestCanonicalHash(body.Script, "", messages.DefaultCommandRequestTimeoutS)
	if hashHex != serverHash {
		emitRefused(msgaction.ConflictCmdHashMismatch, map[string]string{
			"command_sha256":   serverHash,
			"approver_user_id": user.ID(),
		})
		writeError(w, http.StatusConflict, msgaction.ConflictCmdHashMismatch,
			"command_sha256 does not match the canonical hash of the submitted script", nil)
		return
	}

	// Step 4: resolve caller's execution container (§4.7 step 4).
	execRes, ok := s.resolveExecContainerForApproval(ctx, w, user, serverHash, emitRefused)
	if !ok {
		return
	}

	// Resolve sender agent name if available.
	if senderAgent, aerr := s.store.GetAgent(ctx, msg.SenderID); aerr == nil && senderAgent != nil {
		if senderAgent.Slug != "" {
			agentName = senderAgent.Slug
		} else if senderAgent.Name != "" {
			agentName = senderAgent.Name
		}
	}

	// Step 5: one transaction — insert v1 approved + command_runs (insert-only).
	shareOutput := body.ShareOutput != nil && *body.ShareOutput
	appRes, err := s.store.CreateAndApproveMarkdownCommandRequest(ctx, &store.CreateMarkdownCommandRequestInput{
		ProjectID:      msg.ProjectID,
		AgentID:        msg.SenderID,
		AgentName:      agentName,
		MessageID:      msg.ID,
		ConversationID: msg.ConversationID,
		ChatKey:        chatKey,
		Script:         body.Script,
		CommandSHA256:  serverHash,
		UserID:         user.ID(),
		UserName:       user.DisplayName(),
		UserEmail:      user.Email(),
		IdempotencyKey: body.IdempotencyKey,
		ExecAgentID:    execRes.Agent.ID,
		ExecProjectID:  execRes.Agent.ProjectID,
		ShareOutput:    shareOutput,
		LeaseUntil:     now.Add(commandRunWakeBudget + commandRunApprovalLeaseGrace),
		Now:            now,
		TTL:            msgaction.CommandRequestTTL,
	})
	if err != nil {
		writeErrorFromErr(w, err, "")
		return
	}
	if appRes.Conflict != nil {
		emitRefused(appRes.ConflictCode, map[string]string{
			"command_sha256":   serverHash,
			"approver_user_id": user.ID(),
		})
		details := map[string]interface{}{
			"status":  appRes.Conflict.Status,
			"rowId":   appRes.Conflict.ID,
			"request": commandRequestView(appRes.Conflict, nil, appRes.ConflictRun),
		}
		if appRes.ConflictRun != nil {
			details["run"] = commandRunView(appRes.ConflictRun)
		}
		writeError(w, http.StatusConflict, appRes.ConflictCode,
			"this command block has already been executed", details)
		return
	}

	reqView := commandRequestView(appRes.Request, nil, appRes.Run)
	runView := *commandRunView(appRes.Run)

	if appRes.Replayed {
		writeJSON(w, http.StatusOK, messages.ApproveCommandRequestResponse{
			Request:  reqView,
			Run:      runView,
			Replayed: true,
		})
		return
	}

	// Step 6: post-commit SSE update, audit event, and handoff to executor.
	if chatKey != "" {
		s.publishActionUpdated(msgaction.KindCommandRequest, appRes.Request.ProjectID, appRes.Request.MessageID, chatKey, commandRequestSSEView(appRes.Request, nil, appRes.Run))
	}
	s.emitMessageActionEvent(ctx, &MessageActionEvent{
		Kind:      msgaction.KindCommandRequest,
		EventType: msgaction.CommandRequestResolvedStatus,
		RowID:     appRes.Request.ID,
		MessageID: appRes.Request.MessageID,
		ProjectID: appRes.Request.ProjectID,
		AgentID:   appRes.Request.AgentID,
		AgentName: appRes.Request.AgentName,
		Key:       appRes.Request.Key,
		Version:   appRes.Request.Version,
		ActorKind: MessageActionActorUser,
		ActorID:   user.ID(),
		Details: map[string]string{
			"command_sha256":   appRes.Request.CommandSHA256,
			"exec_agent_id":    appRes.Run.ExecAgentID,
			"exec_project_id":  appRes.Run.ExecProjectID,
			"run_id":           appRes.Run.ID,
			"approver_user_id": user.ID(),
			"workdir_kind":     messages.CommandWorkdirKind(appRes.Request.Workdir),
			"share_output":     strconv.FormatBool(appRes.Run.ShareOutput),
		},
		Timestamp: now,
	})
	s.enqueueCommandRun(appRes.Request, appRes.Run)

	writeJSON(w, http.StatusAccepted, messages.ApproveCommandRequestResponse{
		Request: reqView,
		Run:     runView,
	})
}

// isMarkdownExecuteAllowedMessageType allow-lists ordinary agent chat message
// types for Markdown Execute (audit L1 / §4.14). Hub-composed or non-chat rows
// (notification, state-change, input-needed, decision-prompt, secret-request,
// command-request, system, etc.) are refused.
func isMarkdownExecuteAllowedMessageType(msgType string) bool {
	switch msgType {
	case "", messages.TypeInstruction, messages.TypeReply:
		return true
	default:
		return false
	}
}

// isSingleAddressedHumanForMarkdownExecute enforces the §4.14 / D6 recipient
// rule: in a DM, the DM user must be caller; in a group conversation or thread,
// the message must be addressed to exactly one human (via `to` / Recipient or a
// single human @-mention in prose) and that human must be caller.
func (s *Server) isSingleAddressedHumanForMarkdownExecute(ctx context.Context, user UserIdentity, msg *store.Message, chatKey string) bool {
	if user == nil || msg == nil {
		return false
	}

	// 1. Direct message key ("dm:...").
	if strings.HasPrefix(chatKey, "dm:") {
		kindA, idA, kindB, idB, err := messages.ParseDMKey(chatKey)
		if err != nil {
			return false
		}
		var humans []string
		if kindA == "user" && idA != "" {
			humans = append(humans, idA)
		}
		if kindB == "user" && idB != "" && (kindA != "user" || idB != idA) {
			humans = append(humans, idB)
		}
		return len(humans) == 1 && humans[0] == user.ID()
	}

	// 2. Direct conversation record (when ThreadID is not a dm: key).
	if msg.ConversationID != "" {
		if conv, err := s.store.GetConversation(ctx, msg.ConversationID); err == nil && conv != nil && conv.Kind == "direct" {
			parts, perr := s.store.ListParticipants(ctx, msg.ConversationID)
			if perr != nil {
				return false
			}
			var humans []string
			for _, p := range parts {
				if p.PrincipalKind == "user" && p.PrincipalID != "" {
					humans = append(humans, p.PrincipalID)
				}
			}
			return len(humans) == 1 && humans[0] == user.ID()
		}
	}

	// 3. Legacy DM without ConversationID or ThreadID.
	if chatKey == "" && msg.ConversationID == "" && strings.HasPrefix(msg.Recipient, "user:") {
		return msg.RecipientID == user.ID()
	}

	// 4. Group conversation or project thread: addressed to exactly one human
	// via `to` (Recipient) or a single human @-mention in prose.
	addressedHumans := make(map[string]struct{})
	if strings.HasPrefix(msg.Recipient, "user:") && msg.RecipientID != "" {
		addressedHumans[msg.RecipientID] = struct{}{}
	}

	// Build the canonical human roster (participants + project human members +
	// caller) and resolve mentions via mentionedHumanIDs (same canonical forms
	// as notifications: display name, hyphenated display-name slug, full email,
	// email local part). Fail closed on any ambiguous mention or unrecognized
	// email mention (R2 / D6).
	humansByID := make(map[string]chatMemberEntry)
	addHuman := func(id, email, displayName string) {
		if id == "" {
			return
		}
		cur := humansByID[id]
		cur.ID = id
		cur.Kind = "user"
		if email != "" && cur.Email == "" {
			cur.Email = strings.TrimSpace(email)
		}
		if displayName != "" && cur.DisplayName == "" {
			cur.DisplayName = strings.TrimSpace(displayName)
		}
		humansByID[id] = cur
	}
	addHuman(user.ID(), user.Email(), user.DisplayName())
	if msg.ConversationID != "" {
		if parts, err := s.store.ListParticipants(ctx, msg.ConversationID); err == nil {
			for _, p := range parts {
				if p.PrincipalKind != "user" || p.PrincipalID == "" {
					continue
				}
				if u, uerr := s.store.GetUser(ctx, p.PrincipalID); uerr == nil && u != nil {
					addHuman(u.ID, u.Email, u.DisplayName)
				}
			}
		}
	}
	if msg.ProjectID != "" {
		for _, m := range s.resolveProjectHumanMembers(ctx, msg.ProjectID) {
			if m.Kind == "user" {
				addHuman(m.ID, m.Email, m.DisplayName)
			}
		}
	}

	humanMembers := make([]chatMemberEntry, 0, len(humansByID))
	canonicalOwners := make(map[string]map[string]struct{}, len(humansByID)*4)
	recordKey := func(key, uid string) {
		if key == "" {
			return
		}
		if canonicalOwners[key] == nil {
			canonicalOwners[key] = make(map[string]struct{})
		}
		canonicalOwners[key][uid] = struct{}{}
	}
	for _, m := range humansByID {
		humanMembers = append(humanMembers, m)
		if m.DisplayName != "" {
			lower := strings.ToLower(m.DisplayName)
			recordKey(lower, m.ID)
			if slug := strings.ToLower(strings.ReplaceAll(m.DisplayName, " ", "-")); slug != "" {
				recordKey(slug, m.ID)
			}
		}
		if m.Email != "" {
			lower := strings.ToLower(m.Email)
			recordKey(lower, m.ID)
			if at := strings.IndexByte(lower, '@'); at > 0 {
				recordKey(lower[:at], m.ID)
			}
		}
	}

	mentions := messages.ExtractProseMentionsWithOptions(msg.Msg, s.mentionOptions())
	for _, raw := range mentions {
		lower := strings.ToLower(strings.TrimSpace(raw))
		if lower == "" {
			continue
		}
		owners := canonicalOwners[lower]
		if len(owners) > 1 {
			// Ambiguous mention (matches multiple distinct humans) -> fail closed.
			return false
		}
		if len(owners) == 0 && strings.Contains(lower, "@") {
			// Fail closed on an unrecognized email mention (another human).
			return false
		}
	}
	for _, uid := range mentionedHumanIDs(humanMembers, mentions, "") {
		addressedHumans[uid] = struct{}{}
	}

	if len(addressedHumans) != 1 {
		return false
	}
	_, ok := addressedHumans[user.ID()]
	return ok
}

func (s *Server) handleAgentCommandRequestList(w http.ResponseWriter, r *http.Request, agentIdent AgentIdentity) {
	q := r.URL.Query()
	limit := store.DefaultCommandRequestListLimit
	if rawLimit := strings.TrimSpace(q.Get("limit")); rawLimit != "" {
		n, err := strconv.Atoi(rawLimit)
		if err != nil || n <= 0 {
			ValidationError(w, "limit must be a positive integer", map[string]interface{}{"field": "limit"})
			return
		}
		limit = n
	}
	res, err := s.store.ListAgentCommandRequests(r.Context(), &store.ListAgentCommandRequestsOptions{
		AgentID: agentIdent.ID(),
		Key:     strings.TrimSpace(q.Get("key")),
		Cursor:  strings.TrimSpace(q.Get("cursor")),
		Limit:   limit,
	})
	if err != nil {
		if errors.Is(err, store.ErrInvalidInput) {
			ValidationError(w, err.Error(), nil)
			return
		}
		writeErrorFromErr(w, err, "")
		return
	}
	items := make([]messages.CommandRequestView, len(res.Items))
	for i, row := range res.Items {
		run := res.Runs[row.ID]
		items[i] = commandRequestSSEView(row, nil, run)
		if items[i].Run != nil && run != nil && run.ShareOutput {
			tail, _ := s.loadSharedCommandRunOutputTail(r.Context(), run)
			items[i].Run.OutputTail = tail
		}
	}
	writeJSON(w, http.StatusOK, messages.AgentCommandRequestListResponse{
		Items:      items,
		NextCursor: res.NextCursor,
	})
}

// commandRequestSSEView builds a CommandRequestView with the human's execution
// container IDs stripped (used for SSE broadcasts and the agent list view).
func commandRequestSSEView(row *store.CommandRequest, replacedBy *store.CommandRequest, run *store.CommandRun) messages.CommandRequestView {
	v := commandRequestView(row, replacedBy, run)
	if v.Run != nil {
		v.Run.ExecAgentID = ""
		v.Run.ExecProjectID = ""
	}
	return v
}

func commandRequestView(row *store.CommandRequest, replacedBy *store.CommandRequest, run *store.CommandRun) messages.CommandRequestView {
	if row == nil {
		return messages.CommandRequestView{}
	}
	v := messages.CommandRequestView{
		RowID:                row.ID,
		MessageID:            row.MessageID,
		ProjectID:            row.ProjectID,
		AgentID:              row.AgentID,
		AgentName:            row.AgentName,
		Key:                  row.Key,
		Version:              row.Version,
		Description:          row.Description,
		Script:               row.Script,
		Workdir:              row.Workdir,
		WorkdirKind:          messages.CommandWorkdirKind(row.Workdir),
		TimeoutS:             row.TimeoutS,
		ShareOutputRequested: row.ShareOutputRequested,
		CommandSHA256:        row.CommandSHA256,
		ApproverUserID:       row.ApproverUserID,
		ConversationID:       row.ConversationID,
		Status:               row.Status,
		CreatedAt:            row.CreatedAt,
		ExpiresAt:            row.ExpiresAt,
		ResolvedAt:           row.ResolvedAt,
		Run:                  commandRunView(run),
	}
	if row.ReplacedByID != "" && row.ReplacedAt != nil {
		rb := &messages.CommandRequestReplacedBy{
			RowID: row.ReplacedByID,
			At:    *row.ReplacedAt,
		}
		if replacedBy != nil {
			rb.MessageID = replacedBy.MessageID
			rb.Version = replacedBy.Version
		}
		v.ReplacedBy = rb
	}
	if row.Status == store.CommandRequestStatusApproved && row.ResolvedAt != nil {
		v.Approved = &messages.CommandRequestApproved{
			At:       *row.ResolvedAt,
			ByUserID: row.ResolvedByUserID,
			ByName:   row.ResolvedByName,
			ByEmail:  row.ResolvedByEmail,
			Surface:  row.Surface,
		}
	}
	return v
}

func commandRunView(run *store.CommandRun) *messages.CommandRequestRunView {
	if run == nil {
		return nil
	}
	return &messages.CommandRequestRunView{
		RunID:         run.ID,
		RequestID:     run.RequestID,
		Status:        run.Status,
		Reason:        run.Reason,
		Wake:          run.Wake,
		ExecAgentID:   run.ExecAgentID,
		ExecProjectID: run.ExecProjectID,
		ShareOutput:   run.ShareOutput,
		TimeoutS:      run.TimeoutS,
		Workdir:       run.Workdir,
		ExitCode:      run.ExitCode,
		DurationMS:    run.DurationMS,
		OutputBytes:   run.OutputBytes,
		OutputStatus:  run.OutputStatus,
		CreatedAt:     run.CreatedAt,
		DispatchedAt:  run.DispatchedAt,
		FinishedAt:    run.FinishedAt,
	}
}
