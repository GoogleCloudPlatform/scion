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
	"sort"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/experiments"
	"github.com/GoogleCloudPlatform/scion/pkg/msgaction"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

const agentSecretsInboxHref = "/profile/agent-secrets"

type secretRequestActionInboxKind struct {
	srv *Server
}

func (k *secretRequestActionInboxKind) Kind() string                { return msgaction.KindSecretRequest }
func (k *secretRequestActionInboxKind) Experiment() string          { return experiments.AgentSecrets }
func (k *secretRequestActionInboxKind) Category() string            { return InboxCategoryAgentSecrets }
func (k *secretRequestActionInboxKind) Href() string                { return agentSecretsInboxHref }
func (k *secretRequestActionInboxKind) Scope() actionInboxScopeMode { return scopeMemberProjects }

func secretRequestToActionInboxRow(r *store.SecretRequest) actionInboxRow {
	cls, _ := msgaction.SecretRequestKindSpec.Classify(r.Status)
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

func (k *secretRequestActionInboxKind) ListOpen(ctx context.Context, q actionOpenQuery) ([]actionInboxRow, error) {
	rows, err := k.srv.store.ListOpenSecretRequests(ctx, q.ProjectIDs, q.ViewerID, q.Now, q.ScanCap)
	if err != nil {
		return nil, err
	}
	out := make([]actionInboxRow, 0, len(rows))
	for _, r := range rows {
		if r == nil {
			continue
		}
		out = append(out, secretRequestToActionInboxRow(r))
	}
	return out, nil
}

func (k *secretRequestActionInboxKind) ListResolved(ctx context.Context, q actionHistoryQuery) ([]actionInboxRow, error) {
	hq := store.SecretRequestHistoryQuery{
		ProjectIDs:      q.ProjectIDs,
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
	rows, err := k.srv.store.ListResolvedSecretRequests(ctx, hq)
	if err != nil {
		return nil, err
	}
	out := make([]actionInboxRow, 0, len(rows))
	for _, r := range rows {
		if r == nil {
			continue
		}
		out = append(out, secretRequestToActionInboxRow(r))
	}
	return out, nil
}

func (k *secretRequestActionInboxKind) Recipient(ctx context.Context, sc *actionInboxScope, row actionInboxRow, answering bool) bool {
	return sc.isRecipient(ctx, row.ChatKey, row.MessageID, answering)
}

func (k *secretRequestActionInboxKind) CanRead(ctx context.Context, sc *actionInboxScope, row actionInboxRow) bool {
	return sc.checkAgentPerm(ctx, row.AgentID, row.ProjectID, ActionRead)
}

func (k *secretRequestActionInboxKind) CanRespond(ctx context.Context, sc *actionInboxScope, row actionInboxRow) bool {
	return sc.checkAgentPerm(ctx, row.AgentID, row.ProjectID, ActionAttach)
}

func (k *secretRequestActionInboxKind) CardsFor(ctx context.Context, sc *actionInboxScope, rows []authorizedActionRow) (any, error) {
	if len(rows) == 0 {
		return []SecretRequestCardResponse{}, nil
	}

	type cardBucket struct {
		msgID   string
		rawRows []*store.SecretRequest
	}
	byMsg := make(map[string]*cardBucket, len(rows))
	order := make([]*cardBucket, 0, len(rows))
	allRaw := make([]*store.SecretRequest, 0, len(rows))
	projIDs := make([]string, 0, len(rows))

	for _, r := range rows {
		sr, ok := r.Row.Raw.(*store.SecretRequest)
		if !ok || sr == nil {
			continue
		}
		b, exists := byMsg[sr.MessageID]
		if !exists {
			b = &cardBucket{msgID: sr.MessageID}
			byMsg[sr.MessageID] = b
			order = append(order, b)
			if sr.ProjectID != "" {
				projIDs = append(projIDs, sr.ProjectID)
			}
		}
		b.rawRows = append(b.rawRows, sr)
		allRaw = append(allRaw, sr)
	}

	sc.preloadProjectNames(ctx, projIDs)

	msgIDs := make([]string, 0, len(order))
	for _, b := range order {
		if b.msgID != "" {
			msgIDs = append(msgIDs, b.msgID)
		}
	}
	sc.preloadPromptMessages(ctx, msgIDs)

	// Batch-load any ReplacedBy rows across all cards in one store call.
	var replIDs []string
	seenRepl := make(map[string]struct{})
	for _, r := range allRaw {
		if r.ReplacedByID != "" {
			if _, ok := seenRepl[r.ReplacedByID]; !ok {
				seenRepl[r.ReplacedByID] = struct{}{}
				replIDs = append(replIDs, r.ReplacedByID)
			}
		}
	}
	var replMap map[string]*store.SecretRequest
	if len(replIDs) > 0 {
		if m, err := k.srv.store.GetSecretRequestsByIDs(ctx, replIDs); err == nil {
			replMap = m
		}
	}

	now := time.Now().UTC()
	cards := make([]SecretRequestCardResponse, 0, len(order))
	for _, b := range order {
		sort.Slice(b.rawRows, func(i, j int) bool {
			if b.rawRows[i].Position != b.rawRows[j].Position {
				return b.rawRows[i].Position < b.rawRows[j].Position
			}
			return b.rawRows[i].ID < b.rawRows[j].ID
		})
		first := b.rawRows[0]
		chatKey := first.ChatKey
		promptMsg := sc.getPromptMessage(ctx, b.msgID)
		if chatKey == "" && promptMsg != nil {
			chatKey = promptMsg.ThreadID
		}
		intro := ""
		if promptMsg != nil && first.IntroLength > 0 && first.IntroLength <= len(promptMsg.Msg) {
			intro = promptMsg.Msg[:first.IntroLength]
		}

		canAnswer := sc.isRecipient(ctx, chatKey, b.msgID, true)
		canAttach := sc.checkAgentPerm(ctx, first.AgentID, first.ProjectID, ActionAttach)
		hasPending := false
		views := make([]SecretRequestView, len(b.rawRows))
		for i, sr := range b.rawRows {
			if sr.Status == store.SecretRequestStatusPending && sr.ExpiresAt.After(now) {
				hasPending = true
			}
			var repl *store.SecretRequest
			if sr.ReplacedByID != "" && replMap != nil {
				repl = replMap[sr.ReplacedByID]
			}
			views[i] = secretRequestView(sr, repl)
		}

		canRespond := true
		reason := ""
		switch {
		case !canAnswer:
			canRespond = false
			reason = "you are not an eligible responder for this secret request"
		case !canAttach:
			canRespond = false
			reason = "you need agent.attach permission on this agent to supply or dismiss secrets"
		case !hasPending:
			canRespond = false
			reason = "all secret requests on this card are closed"
		}

		cards = append(cards, SecretRequestCardResponse{
			MessageID:       b.msgID,
			ProjectID:       first.ProjectID,
			ProjectName:     sc.projectName(ctx, first.ProjectID),
			AgentID:         first.AgentID,
			AgentName:       first.AgentName,
			ConversationID:  first.ConversationID,
			ThreadID:        chatKey,
			Intro:           intro,
			Rows:            views,
			CanRespond:      canRespond,
			PromptAvailable: promptMsg != nil,
			Reason:          reason,
		})
	}
	return cards, nil
}

func (s *Server) inboxAgentSecretsCategory(ctx context.Context, v inboxViewer, _ int, _ string) InboxCategory {
	return s.actionInboxCategory(ctx, v, &secretRequestActionInboxKind{srv: s})
}
