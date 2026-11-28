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
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/experiments"
	"github.com/GoogleCloudPlatform/scion/pkg/messages"
	"github.com/GoogleCloudPlatform/scion/pkg/msgaction"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

const agentExecutionsInboxHref = "/profile/agent-executions"

type commandRequestActionInboxKind struct {
	srv *Server
}

func (k *commandRequestActionInboxKind) Kind() string                { return msgaction.KindCommandRequest }
func (k *commandRequestActionInboxKind) Experiment() string          { return experiments.CommandRequests }
func (k *commandRequestActionInboxKind) Category() string            { return InboxCategoryAgentExecutions }
func (k *commandRequestActionInboxKind) Href() string                { return agentExecutionsInboxHref }
func (k *commandRequestActionInboxKind) Scope() actionInboxScopeMode { return scopeViewer }

func commandRequestToActionInboxRow(r *store.CommandRequest) actionInboxRow {
	cls, _ := msgaction.CommandRequestKindSpec.Classify(r.Status)
	var resolvedAt time.Time
	if r.ResolvedAt != nil {
		resolvedAt = r.ResolvedAt.UTC()
	}
	return actionInboxRow{
		ID:          r.ID,
		ProjectID:   r.ProjectID,
		AgentID:     r.AgentID,
		AgentName:   r.AgentName,
		ChatKey:     r.ChatKey,
		MessageID:   r.MessageID,
		Key:         r.Key,
		Description: r.Description,
		Class:       cls,
		CreatedAt:   r.CreatedAt.UTC(),
		ExpiresAt:   r.ExpiresAt.UTC(),
		ResolvedAt:  resolvedAt,
		Raw:         r,
	}
}

func (k *commandRequestActionInboxKind) ListOpen(ctx context.Context, q actionOpenQuery) ([]actionInboxRow, error) {
	rows, err := k.srv.store.ListOpenCommandRequests(ctx, q.ViewerID, q.Now, q.ScanCap)
	if err != nil {
		return nil, err
	}
	out := make([]actionInboxRow, 0, len(rows))
	for _, r := range rows {
		if r == nil {
			continue
		}
		out = append(out, commandRequestToActionInboxRow(r))
	}
	return out, nil
}

func (k *commandRequestActionInboxKind) ListResolved(ctx context.Context, q actionHistoryQuery) ([]actionInboxRow, error) {
	hq := store.CommandRequestHistoryQuery{
		ViewerID:        q.ViewerID,
		FilterProjectID: q.FilterProjectID,
		FilterAgentID:   q.FilterAgentID,
		Limit:           q.Limit,
	}
	if q.Cursor != nil && !q.Cursor.Time.IsZero() && q.Cursor.ID != "" {
		t := q.Cursor.Time.UTC()
		hq.CursorTime = &t
		hq.CursorID = q.Cursor.ID
	}
	rows, err := k.srv.store.ListResolvedCommandRequests(ctx, hq)
	if err != nil {
		return nil, err
	}
	out := make([]actionInboxRow, 0, len(rows))
	for _, r := range rows {
		if r == nil {
			continue
		}
		out = append(out, commandRequestToActionInboxRow(r))
	}
	return out, nil
}

func (k *commandRequestActionInboxKind) Recipient(ctx context.Context, sc *actionInboxScope, row actionInboxRow, answering bool) bool {
	cr, ok := row.Raw.(*store.CommandRequest)
	if !ok || cr == nil {
		return false
	}
	chatKey := row.ChatKey
	var promptMsg *store.Message
	if chatKey == "" {
		promptMsg = sc.getPromptMessage(ctx, row.MessageID)
		if promptMsg != nil && promptMsg.ThreadID != "" {
			chatKey = promptMsg.ThreadID
		}
	}
	if chatKey != "" || promptMsg != nil {
		return sc.isRecipient(ctx, chatKey, row.MessageID, answering)
	}
	return sc.viewerID == cr.ApproverUserID
}

func (k *commandRequestActionInboxKind) CanRead(_ context.Context, _ *actionInboxScope, _ actionInboxRow) bool {
	return true
}

func (k *commandRequestActionInboxKind) CanRespond(_ context.Context, sc *actionInboxScope, row actionInboxRow) bool {
	cr, ok := row.Raw.(*store.CommandRequest)
	if !ok || cr == nil {
		return false
	}
	return sc.viewerID == cr.ApproverUserID
}

func (k *commandRequestActionInboxKind) CardsFor(ctx context.Context, sc *actionInboxScope, rows []authorizedActionRow) (any, error) {
	if len(rows) == 0 {
		return []messages.CommandRequestDetailResponse{}, nil
	}

	now := time.Now().UTC()
	rawRows := make([]*store.CommandRequest, 0, len(rows))
	reqIDs := make([]string, 0, len(rows))
	projIDs := make([]string, 0, len(rows))
	msgIDs := make([]string, 0, len(rows))
	var replIDs []string
	seenRepl := make(map[string]struct{})
	hasPending := false

	for _, r := range rows {
		cr, ok := r.Row.Raw.(*store.CommandRequest)
		if !ok || cr == nil {
			continue
		}
		rawRows = append(rawRows, cr)
		reqIDs = append(reqIDs, cr.ID)
		if cr.ProjectID != "" {
			projIDs = append(projIDs, cr.ProjectID)
		}
		if cr.MessageID != "" {
			msgIDs = append(msgIDs, cr.MessageID)
		}
		if cr.ReplacedByID != "" {
			if _, dup := seenRepl[cr.ReplacedByID]; !dup {
				seenRepl[cr.ReplacedByID] = struct{}{}
				replIDs = append(replIDs, cr.ReplacedByID)
			}
		}
		if cr.Status == store.CommandRequestStatusPending && cr.ExpiresAt.After(now) {
			hasPending = true
		}
	}

	if len(rawRows) == 0 {
		return []messages.CommandRequestDetailResponse{}, nil
	}

	sc.preloadProjectNames(ctx, projIDs)
	sc.preloadPromptMessages(ctx, msgIDs)

	runsMap := make(map[string]*store.CommandRun)
	if m, err := k.srv.store.GetCommandRunsByRequestIDs(ctx, reqIDs); err == nil && m != nil {
		runsMap = m
	}

	replMap := make(map[string]*store.CommandRequest)
	if len(replIDs) > 0 {
		if m, err := k.srv.store.GetCommandRequestsByIDs(ctx, replIDs); err == nil && m != nil {
			replMap = m
		}
	}

	var finishedRunIDs []string
	for _, run := range runsMap {
		if run != nil && run.FinishedAt != nil && run.ApproverUserID == sc.viewerID {
			finishedRunIDs = append(finishedRunIDs, run.ID)
		}
	}
	hasOutputMap := make(map[string]bool)
	if len(finishedRunIDs) > 0 {
		if m, err := k.srv.store.HasActionRunOutputs(ctx, finishedRunIDs); err == nil && m != nil {
			hasOutputMap = m
		}
	}

	var execView *messages.CommandRequestExecContainerView
	if hasPending {
		execView = k.srv.resolveExecContainerViewForIdentity(ctx, sc.user)
	}

	type approverInfo struct {
		name  string
		email string
	}
	approverCache := make(map[string]approverInfo)
	approverLookup := func(id string) (string, string) {
		if info, ok := approverCache[id]; ok {
			return info.name, info.email
		}
		var info approverInfo
		if u, err := k.srv.store.GetUser(ctx, id); err == nil && u != nil {
			info.name = u.DisplayName
			info.email = u.Email
		}
		approverCache[id] = info
		return info.name, info.email
	}

	deps := &commandRequestDetailDeps{
		Runs:               runsMap,
		ReplacedBy:         replMap,
		HasOutput:          hasOutputMap,
		ExecView:           execView,
		ExecLoaded:         true,
		ProjectName:        func(id string) string { return sc.projectName(ctx, id) },
		Approver:           approverLookup,
		SkipResolvedExtras: true,
	}

	cards := make([]messages.CommandRequestDetailResponse, 0, len(rawRows))
	for _, cr := range rawRows {
		promptMsg := sc.getPromptMessage(ctx, cr.MessageID)
		chatKey := cr.ChatKey
		if chatKey == "" && promptMsg != nil {
			chatKey = promptMsg.ThreadID
		}
		cards = append(cards, k.srv.buildCommandRequestDetail(ctx, sc.user, cr, promptMsg, chatKey, deps))
	}
	return cards, nil
}

func (s *Server) inboxAgentExecutionsCategory(ctx context.Context, v inboxViewer, _ int, _ string) InboxCategory {
	return s.actionInboxCategory(ctx, v, &commandRequestActionInboxKind{srv: s})
}

// publishCommandRequestInboxHint emits a content-free agent_executions inbox
// invalidation hint to row.ApproverUserID when row is eligible for the inbox
// (i.e. not a self-run markdown_block row).
func (s *Server) publishCommandRequestInboxHint(ctx context.Context, row *store.CommandRequest) {
	if row == nil || row.ApproverUserID == "" {
		return
	}
	if row.Surface == msgaction.CommandRequestSurfaceMarkdownBlock {
		return
	}
	s.publishInboxHint(ctx, row.ApproverUserID, InboxHint{Category: InboxCategoryAgentExecutions})
}

// publishCommandRequestInboxHintsOnce emits at most one agent_executions inbox
// invalidation hint per distinct eligible approver across rows.
func (s *Server) publishCommandRequestInboxHintsOnce(ctx context.Context, rows []*store.CommandRequest) {
	if len(rows) == 0 {
		return
	}
	seen := make(map[string]struct{}, len(rows))
	for _, row := range rows {
		if row == nil || row.ApproverUserID == "" {
			continue
		}
		if row.Surface == msgaction.CommandRequestSurfaceMarkdownBlock {
			continue
		}
		if _, dup := seen[row.ApproverUserID]; dup {
			continue
		}
		seen[row.ApproverUserID] = struct{}{}
		s.publishInboxHint(ctx, row.ApproverUserID, InboxHint{Category: InboxCategoryAgentExecutions})
	}
}

// notifyCommandRequestChanged publishes the live chat SSE card update (when a
// chat key is known) and emits the approver's agent_executions inbox hint.
func (s *Server) notifyCommandRequestChanged(ctx context.Context, row *store.CommandRequest, replacedBy *store.CommandRequest, run *store.CommandRun) {
	if row == nil {
		return
	}
	chatKey := row.ChatKey
	if chatKey == "" && row.MessageID != "" {
		if prompt, err := s.store.GetMessage(ctx, row.MessageID); err == nil && prompt != nil {
			chatKey = prompt.ThreadID
		}
	}
	if chatKey != "" {
		s.publishActionUpdated(msgaction.KindCommandRequest, row.ProjectID, row.MessageID, chatKey, commandRequestSSEView(row, replacedBy, run))
	}
	s.publishCommandRequestInboxHint(ctx, row)
}
