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
	commandRunWakeBudget = 5 * time.Minute
	commandRunLeaseGrace = 60 * time.Second
)

// CommandRunLauncher receives newly approved command runs post-commit for
// asynchronous execution (§4.7 step 7, §4.8).
type CommandRunLauncher interface {
	EnqueueCommandRun(req *store.CommandRequest, run *store.CommandRun)
}

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
	}
}

// handleCommandRequestRoutes serves the responder command-request endpoints,
// 404 while hub.command_requests is off:
//
//	GET  /api/v1/command-requests/{id}
//	POST /api/v1/command-requests/{id}/approve
func (s *Server) handleCommandRequestRoutes(w http.ResponseWriter, r *http.Request) {
	s.requireExperiment(experiments.CommandRequests, func(w http.ResponseWriter, r *http.Request) {
		rest := strings.TrimPrefix(r.URL.Path, commandRequestsRoutePrefix)
		reqID, sub, hasSlash := strings.Cut(rest, "/")
		if reqID == "" {
			NotFound(w, "Command request")
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

func commandWorkdirKind(workdir string) string {
	switch {
	case workdir == "":
		return "home"
	case workdir == "/workspace" || strings.HasPrefix(workdir, "/workspace/"):
		return "workspace"
	default:
		return "explicit"
	}
}

func (s *Server) loadCommandRequestWithMessage(ctx context.Context, reqID string) (*store.CommandRequest, *store.Message, string, error) {
	row, err := s.store.GetCommandRequest(ctx, reqID)
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
		return s.isSecretRequestRecipient(ctx, user, chatKey, promptMsg, answering)
	}
	return user.ID() == row.ApproverUserID
}

func (s *Server) handleCommandRequestGet(w http.ResponseWriter, r *http.Request, reqID string) {
	user := s.messageActionHumanSessionCaller(w, r, msgaction.KindCommandRequest, reqID, "")
	if user == nil {
		return
	}
	ctx := r.Context()
	row, promptMsg, chatKey, err := s.loadCommandRequestWithMessage(ctx, reqID)
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
			execView = s.buildExecContainerView(ctx, res)
		}
	}

	runView := commandRunView(runRow)
	writeJSON(w, http.StatusOK, messages.CommandRequestDetailResponse{
		Request:       commandRequestView(row, replacedBy, runRow),
		Run:           runView,
		ExecContainer: execView,
		CanApprove:    canApprove,
		Reason:        reason,
	})
}

func (s *Server) buildExecContainerView(ctx context.Context, res ExecContainerResolution) *messages.CommandRequestExecContainerView {
	v := &messages.CommandRequestExecContainerView{
		State:          string(res.State),
		HomePersistent: res.HomePersistent,
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

	row, promptMsg, chatKey, err := s.loadCommandRequestWithMessage(ctx, reqID)
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
	execRes, err := s.resolveExecContainerForIdentity(ctx, user)
	if err != nil {
		writeErrorFromErr(w, err, "")
		return
	}
	switch execRes.State {
	case ExecContainerStateUnconfigured:
		emitRefused(msgaction.ConflictCmdExecUnconfigured, map[string]string{
			"command_sha256":   row.CommandSHA256,
			"approver_user_id": user.ID(),
		})
		writeError(w, http.StatusConflict, msgaction.ConflictCmdExecUnconfigured,
			"no execution container is configured; configure one in your profile first", nil)
		return
	case ExecContainerStateUnavailable:
		emitRefused(msgaction.ConflictCmdExecUnavailable, map[string]string{
			"command_sha256":   row.CommandSHA256,
			"approver_user_id": user.ID(),
		})
		writeError(w, http.StatusConflict, msgaction.ConflictCmdExecUnavailable,
			"your execution container is unavailable", nil)
		return
	case ExecContainerStateStopped:
		emitRefused(msgaction.ConflictCmdExecStopped, map[string]string{
			"command_sha256":   row.CommandSHA256,
			"approver_user_id": user.ID(),
			"exec_agent_id":    execRes.Agent.ID,
			"exec_project_id":  execRes.Agent.ProjectID,
		})
		writeError(w, http.StatusConflict, msgaction.ConflictCmdExecStopped,
			"Start your execution container, then approve", nil)
		return
	case ExecContainerStateReady, ExecContainerStateStarting, ExecContainerStateSuspended:
		if execRes.Agent == nil {
			emitRefused(msgaction.ConflictCmdExecUnavailable, map[string]string{
				"command_sha256":   row.CommandSHA256,
				"approver_user_id": user.ID(),
			})
			writeError(w, http.StatusConflict, msgaction.ConflictCmdExecUnavailable,
				"your execution container is unavailable", nil)
			return
		}
	default:
		emitRefused(msgaction.ConflictCmdExecUnavailable, map[string]string{
			"command_sha256":   row.CommandSHA256,
			"approver_user_id": user.ID(),
		})
		writeError(w, http.StatusConflict, msgaction.ConflictCmdExecUnavailable,
			"your execution container is unavailable", nil)
		return
	}

	// Step 5: single transaction — CAS pending -> approved + INSERT command_runs(queued).
	// Share-output opt-in is P4; P1 always records ShareOutput=false.
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
		ShareOutput:    false,
		LeaseUntil:     now.Add(commandRunWakeBudget + commandRunLeaseGrace),
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
		s.publishActionUpdated(msgaction.KindCommandRequest, appRes.Request.ProjectID, appRes.Request.MessageID, chatKey, reqView)
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
			"workdir_kind":     commandWorkdirKind(appRes.Request.Workdir),
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
		items[i] = commandRequestView(row, nil, res.Runs[row.ID])
	}
	writeJSON(w, http.StatusOK, messages.AgentCommandRequestListResponse{
		Items:      items,
		NextCursor: res.NextCursor,
	})
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
