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
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/GoogleCloudPlatform/scion/pkg/experiments"
	"github.com/GoogleCloudPlatform/scion/pkg/messages"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/util/logging"
)

const (
	// maxConversationOpenPrompts caps the prompt items returned by
	// GET /api/v1/chat/conversations/{key}/decisions. Counts remain exact past
	// the cap and Truncated is set to true.
	maxConversationOpenPrompts = 100

	// maxConversationOpenQuestionRunes bounds each item's question preview.
	maxConversationOpenQuestionRunes = 300
)

// ConversationOpenPromptItem is one open decision prompt in a single
// conversation as returned by GET /api/v1/chat/conversations/{key}/decisions.
type ConversationOpenPromptItem struct {
	PromptMessageID      string    `json:"promptMessageId"`
	PromptAvailable      bool      `json:"promptAvailable"`
	CreatedAt            time.Time `json:"createdAt"`
	AskerAgentID         string    `json:"askerAgentId"`
	AskerAgentName       string    `json:"askerAgentName"`
	AnswerMode           string    `json:"answerMode"`
	OpenCount            int       `json:"openCount"`
	TotalCount           int       `json:"totalCount"`
	OpenRowIDs           []string  `json:"openRowIds,omitempty"`
	Question             string    `json:"question"`
	WaitingOnYou         bool      `json:"waitingOnYou"`
	AwaitingConfirmation bool      `json:"awaitingConfirmation"`
	DisabledReason       string    `json:"disabledReason,omitempty"`
}

// ConversationOpenDecisionsResponse is the response body of
// GET /api/v1/chat/conversations/{key}/decisions.
// Counts count prompts (messages), not decision rows.
type ConversationOpenDecisionsResponse struct {
	ProjectID   string                       `json:"projectId,omitempty"`
	ProjectSlug string                       `json:"projectSlug,omitempty"`
	Counts      ProjectDecisionCounts        `json:"counts"`
	Prompts     []ConversationOpenPromptItem `json:"prompts"`
	Truncated   bool                         `json:"truncated"`
}

// handleConversationDecisions serves GET /api/v1/chat/conversations/{key}/decisions.
// Unsupported methods return 405 before the experiment gate, which returns 404
// while web.decisions is off.
func (s *Server) handleConversationDecisions(w http.ResponseWriter, r *http.Request, key string) {
	if r.Method != http.MethodGet {
		MethodNotAllowed(w, http.MethodGet)
		return
	}
	s.requireExperiment(experiments.Decisions, func(w http.ResponseWriter, r *http.Request) {
		s.listConversationDecisions(w, r, key)
	})(w, r)
}

func (s *Server) listConversationDecisions(w http.ResponseWriter, r *http.Request, key string) {
	if _, ok := starQuery(w, r, nil, ""); !ok {
		return
	}
	ctx := r.Context()
	user := GetUserIdentityFromContext(ctx)
	if user == nil || user.ID() == "" {
		Forbidden(w)
		return
	}
	token, ok := s.requireChatSelector(w, r, permInboxRead)
	if !ok {
		return
	}

	isDM := strings.HasPrefix(key, "dm:")
	s.mu.RLock()
	wcs := s.webChatStore
	s.mu.RUnlock()

	scope := &projectReadableScope{
		chatKeys:              []string{key},
		conversationLabel:     make(map[string]string),
		conversationAvailable: map[string]bool{key: true},
		userCanPost:           make(map[string]bool),
		agentsByID:            make(map[string]*store.Agent),
		queriedAgentIDs:       make(map[string]bool),
		liveAgents:            make(map[string]*store.Agent),
		agentNames:            make(map[string]string),
	}

	var projectID, projectSlug string
	if isDM {
		if serr := authorizeDMKeyParticipant(ctx, user, key, logging.RequestPath(r)); serr != nil {
			serr.write(w)
			return
		}
		if canonical, err := canonicalStarDMKey(key); err != nil || canonical != key {
			BadRequest(w, "invalid DM key format")
			return
		}
		if !s.authorizeChatTokenConversation(w, r, token, permInboxRead, key, "", chatRefuseNotFound) {
			return
		}
		kindA, idA, kindB, idB, err := messages.ParseDMKey(key)
		if err != nil {
			BadRequest(w, "invalid DM key format")
			return
		}
		peerKind, peerID := kindA, idA
		if kindA == "user" && idA == user.ID() {
			peerKind, peerID = kindB, idB
		}
		if peerKind != "agent" {
			// User-to-user DMs never hold decisions.
			writeJSON(w, http.StatusOK, ConversationOpenDecisionsResponse{
				Prompts: []ConversationOpenPromptItem{},
			})
			return
		}
		agentsByID, err := s.store.GetAgentsByIDsIncludingDeleted(ctx, []string{peerID})
		if err != nil {
			writeErrorFromErr(w, err, "")
			return
		}
		peerAgent := agentsByID[peerID]
		if peerAgent == nil || peerAgent.ProjectID == "" {
			writeJSON(w, http.StatusOK, ConversationOpenDecisionsResponse{
				Prompts: []ConversationOpenPromptItem{},
			})
			return
		}
		projectID = peerAgent.ProjectID
		if proj, perr := s.store.GetProject(ctx, projectID); perr == nil && proj != nil {
			projectSlug = proj.Slug
		}
		scope.userCanPost[key] = true
		scope.queriedAgentIDs[peerID] = true
		scope.agentsByID[peerID] = peerAgent
		if name := agentDisplayName(peerAgent); name != "" {
			scope.agentNames[peerID] = name
		}
		if peerAgent.DeletedAt.IsZero() {
			scope.liveAgents[peerID] = peerAgent
		}
	} else {
		target, serr := s.authorizeChatSendRead(ctx, user, key)
		if serr != nil {
			serr.write(w)
			return
		}
		if !s.authorizeChatTokenConversation(w, r, token, permInboxRead, key, target.ProjectID, chatRefuseNotFound) {
			return
		}
		wcs = target.wcs
		projectID = target.ProjectID
		project, err := s.store.GetProject(ctx, projectID)
		if err != nil {
			if errors.Is(err, store.ErrNotFound) {
				NotFound(w, "Thread")
				return
			}
			writeErrorFromErr(w, err, "")
			return
		}
		projectSlug = project.Slug
		scope.project = project
		projectChatAccess := s.chatUserAccess(ctx, user, project,
			[]string{ChatActionRead}, chatThreadWriteActions, nil, false, false)
		scope.userCanPost[key] = projectChatAccess.Can(ChatActionPost)
	}

	openPrompts, err := s.store.ListOpenPrompts(ctx, []string{projectID}, []string{key})
	if err != nil {
		writeErrorFromErr(w, err, "")
		return
	}

	_, classified, err := s.classifyProjectOpenPrompts(ctx, user, token, wcs, scope, openPrompts)
	if err != nil {
		writeErrorFromErr(w, err, "")
		return
	}

	// Order oldest first within each section (Waiting on you first, then Waiting on others).
	sort.Slice(classified, func(i, j int) bool {
		if classified[i].section != classified[j].section {
			return classified[i].section < classified[j].section
		}
		if !classified[i].raw.CreatedAt.Equal(classified[j].raw.CreatedAt) {
			return classified[i].raw.CreatedAt.Before(classified[j].raw.CreatedAt)
		}
		return classified[i].raw.MessageID < classified[j].raw.MessageID
	})

	// Counts count prompts (messages), not decision rows, and stay exact past the cap.
	var counts ProjectDecisionCounts
	for _, item := range classified {
		if item.waitingOnYou {
			counts.WaitingOnYou++
		} else {
			counts.WaitingOnOthers++
		}
	}

	truncated := false
	if len(classified) > maxConversationOpenPrompts {
		truncated = true
		classified = classified[:maxConversationOpenPrompts]
	}

	viewerID := user.ID()
	items := make([]ConversationOpenPromptItem, 0, len(classified))
	for _, item := range classified {
		p := item.raw
		askerName := p.AskerAgentName
		if askerName == "" {
			askerName = scope.agentNames[p.AskerAgentID]
		}
		if askerName == "" {
			askerName = p.AskerAgentID
		}
		openCount := item.onOthers
		if item.waitingOnYou {
			openCount = item.onYou
		}
		question, awaitingConfirmation, openRowIDs := summarizeOpenPromptRows(p.Decisions, viewerID, item.waitingOnYou, item.waitingReasons)
		items = append(items, ConversationOpenPromptItem{
			PromptMessageID:      p.MessageID,
			PromptAvailable:      item.promptAvail,
			CreatedAt:            p.CreatedAt,
			AskerAgentID:         p.AskerAgentID,
			AskerAgentName:       askerName,
			AnswerMode:           decisionAnswerMode(len(p.Decisions)),
			OpenCount:            openCount,
			TotalCount:           len(p.Decisions),
			OpenRowIDs:           openRowIDs,
			Question:             truncateDecisionRunes(question, maxConversationOpenQuestionRunes),
			WaitingOnYou:         item.waitingOnYou,
			AwaitingConfirmation: awaitingConfirmation,
			DisabledReason:       item.disabledReason,
		})
	}

	writeJSON(w, http.StatusOK, ConversationOpenDecisionsResponse{
		ProjectID:   projectID,
		ProjectSlug: projectSlug,
		Counts:      counts,
		Prompts:     items,
		Truncated:   truncated,
	})
}

// summarizeOpenPromptRows returns the first open row's question for the prompt's
// section, whether any open row in that section is an agent-interpreted answer
// awaiting confirmation, and the decision row IDs counted in that section.
func summarizeOpenPromptRows(
	decisions []*store.Decision,
	viewerID string,
	waitingOnYou bool,
	waitingReasons map[string]string,
) (string, bool, []string) {
	var sectionQuestion, fallbackOpenQuestion, firstQuestion string
	var openRowIDs []string
	awaitingConfirm := false

	for _, d := range decisions {
		if d == nil {
			continue
		}
		if firstQuestion == "" {
			firstQuestion = d.Question
		}
		reason := store.DecisionActionableReason(d, viewerID)
		switch reason {
		case store.AnswerSetReasonReplaced, store.AnswerSetReasonWithdrawn, store.AnswerSetReasonDismissed, store.AnswerSetReasonDecided:
			continue
		}
		if fallbackOpenQuestion == "" {
			fallbackOpenQuestion = d.Question
		}
		inSection := false
		if waitingOnYou {
			inSection = reason == "" && waitingReasons[d.ID] == ""
		} else {
			inSection = waitingReasons[d.ID] != "" || reason == store.AnswerSetReasonNotRecordedAnswerer
		}
		if !inSection {
			continue
		}
		openRowIDs = append(openRowIDs, d.ID)
		if sectionQuestion == "" {
			sectionQuestion = d.Question
		}
		if d.Status == store.DecisionStatusDecided {
			if ans := d.EffectiveAnswer(); ans != nil && ans.Provenance == store.DecisionProvenanceAgentInterpreted {
				awaitingConfirm = true
			}
		}
	}

	if sectionQuestion != "" {
		return sectionQuestion, awaitingConfirm, openRowIDs
	}
	if fallbackOpenQuestion != "" {
		return fallbackOpenQuestion, awaitingConfirm, openRowIDs
	}
	return firstQuestion, awaitingConfirm, openRowIDs
}

func truncateDecisionRunes(s string, maxRunes int) string {
	if maxRunes <= 0 || utf8.RuneCountInString(s) <= maxRunes {
		return s
	}
	n := 0
	for i := range s {
		if n == maxRunes {
			return s[:i]
		}
		n++
	}
	return s
}
