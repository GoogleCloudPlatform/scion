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
	"fmt"
	"log/slog"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/experiments"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/store/agentsort"
)

const (
	inboxSummaryDefaultPreviews = 3
	inboxSummaryMaxPreviews     = 10
	inboxCategoryTimeout        = 1500 * time.Millisecond
	inboxWaitingLimit           = 200
	inboxPreviewQuestionMax     = 200

	// InboxCategory* are the stable keys returned in InboxSummaryResponse.Categories.
	InboxCategoryDecisions      = "decisions"
	InboxCategoryWaiting        = "waiting"
	InboxCategoryReviews        = "reviews"
	InboxCategoryPendingSecrets = "pending_secrets"
	InboxCategoryChat           = "chat"

	// InboxBreakdown* are the documented stable keys for InboxCategory.Breakdown.
	InboxBreakdownAgents              = "agents"
	InboxBreakdownProjects            = "projects"
	InboxBreakdownToAddress           = "toAddress"
	InboxBreakdownRequested           = "requested"
	InboxBreakdownUnreadConversations = "unreadConversations"
	InboxBreakdownDMs                 = "dms"
	InboxBreakdownMentions            = "mentions"

	// InboxCategoryNotifications is the SSE hint category emitted when a user
	// acknowledges one or all bell notifications (design §5.2).
	InboxCategoryNotifications = "notifications"

	// InboxState* are the valid values for InboxCategory.State.
	InboxStateOK          = "ok"
	InboxStateDisabled    = "disabled"
	InboxStateUnavailable = "unavailable"
	InboxStateError       = "error"

	// InboxError* are the valid values for InboxCategory.ErrorCode when State == "error".
	InboxErrorTimeout  = "timeout"
	InboxErrorInternal = "internal"

	// InboxChatFilter* are the valid values for the ?chat= query parameter.
	InboxChatFilterAll      = "all"
	InboxChatFilterDMs      = "dms"
	InboxChatFilterMentions = "mentions"

	// InboxPendingSecretKind is the kind value for pending user-scope secrets.
	InboxPendingSecretKind = "pending_secret"
)

// InboxSummaryResponse is the body of GET /api/v1/users/me/inbox/summary.
type InboxSummaryResponse struct {
	GeneratedAt string                   `json:"generatedAt"`
	Total       int                      `json:"total"`
	TotalCapped bool                     `json:"totalCapped"`
	Categories  map[string]InboxCategory `json:"categories"`
}

// InboxCategory is the common summary envelope for one inbox category.
type InboxCategory struct {
	State             string         `json:"state"`
	Count             int            `json:"count"`
	Capped            bool           `json:"capped"`
	CountsTowardTotal bool           `json:"countsTowardTotal"`
	Href              string         `json:"href"`
	ErrorCode         string         `json:"errorCode,omitempty"`
	Previews          []any          `json:"previews"`
	AgentIDs          []string       `json:"agentIds,omitempty"`
	Breakdown         map[string]int `json:"breakdown,omitempty"`
	// NextExpiresAt is the earliest expiry among the items counted, when the
	// category's items expire (pending_secrets, agent_secrets). It lets
	// previews=0 clients schedule expiry refreshes without fetching previews.
	NextExpiresAt *time.Time `json:"nextExpiresAt,omitempty"`
}

// MarshalJSON preserves a non-nil empty AgentIDs slice as `"agentIds": []`
// (for the "waiting" category when state == "ok" and count == 0) while
// omitting the field when AgentIDs is nil (all other categories and error state).
func (c InboxCategory) MarshalJSON() ([]byte, error) {
	type alias InboxCategory
	if c.AgentIDs != nil && len(c.AgentIDs) == 0 {
		type withEmptyAgentIDs struct {
			alias
			AgentIDs []string `json:"agentIds"`
		}
		return json.Marshal(withEmptyAgentIDs{
			alias:    alias(c),
			AgentIDs: []string{},
		})
	}
	return json.Marshal(alias(c))
}

// InboxPendingSecretPreview is one preview item in the "pending_secrets" category.
// Never contains secret values or ValueRef.
type InboxPendingSecretPreview struct {
	ID               string    `json:"id"`
	Kind             string    `json:"kind"`
	ProjectID        string    `json:"projectId"`
	ProjectName      string    `json:"projectName"`
	CreatedAt        time.Time `json:"createdAt"`
	Href             string    `json:"href"`
	Key              string    `json:"key"`
	AgentSlug        string    `json:"agentSlug"`
	InjectionMode    string    `json:"injectionMode,omitempty"`
	ReplacesExisting bool      `json:"replacesExisting"`
	AllowProgeny     bool      `json:"allowProgeny"`
	ExpiresAt        time.Time `json:"expiresAt"`
}

// InboxWaitingAgentPreview is one preview item in the "waiting" category.
type InboxWaitingAgentPreview struct {
	ID          string    `json:"id"`
	ProjectID   string    `json:"projectId"`
	ProjectName string    `json:"projectName"`
	CreatedAt   time.Time `json:"createdAt"`
	Href        string    `json:"href"`
	AgentID     string    `json:"agentId"`
	AgentName   string    `json:"agentName"`
	AgentSlug   string    `json:"agentSlug"`
	Question    string    `json:"question"`
	Since       time.Time `json:"since"`
}

// InboxDecisionChoice is one selectable option in an inline decision preview.
type InboxDecisionChoice struct {
	ID    string `json:"id"`
	Label string `json:"label"`
}

// InboxDecisionInline carries the inline-answer payload for single-decision
// prompts the viewer can answer directly from the Inbox card.
type InboxDecisionInline struct {
	DecisionRowID string                `json:"decisionRowId"`
	Choices       []InboxDecisionChoice `json:"choices"`
	AllowsOther   bool                  `json:"allowsOther"`
}

// InboxDecisionPreview is one preview item in the "decisions" category.
type InboxDecisionPreview struct {
	ID                string               `json:"id"`
	ProjectID         string               `json:"projectId"`
	ProjectName       string               `json:"projectName"`
	CreatedAt         time.Time            `json:"createdAt"`
	Href              string               `json:"href"`
	PromptMessageID   string               `json:"promptMessageId"`
	ConversationLabel string               `json:"conversationLabel"`
	AskerAgentName    string               `json:"askerAgentName"`
	Question          string               `json:"question"`
	DecisionCount     int                  `json:"decisionCount"`
	AnswerMode        string               `json:"answerMode"`
	CanAnswer         bool                 `json:"canAnswer"`
	DisabledReason    string               `json:"disabledReason,omitempty"`
	Inline            *InboxDecisionInline `json:"inline,omitempty"`
}

type inboxViewer struct {
	userID string
	user   UserIdentity
	token  *ScopedUserIdentity
}

func (v inboxViewer) credKey() string {
	if v.token == nil {
		return "session"
	}
	if cid := v.token.CredentialID(); cid != "" {
		return "uat:" + cid
	}
	scopes := append([]string(nil), v.token.ScopedScopes()...)
	sort.Strings(scopes)
	return "uat:" + tokenBoundaryProject(v.token) + ":" + strings.Join(scopes, ",")
}

type inboxSourceFunc func(ctx context.Context, v inboxViewer, previews int, chatFilter string) InboxCategory

type inboxSourceEntry struct {
	key string
	run inboxSourceFunc
}

// publishInboxHint emits an inbox invalidation hint on user.<userID>.inbox
// when the web.inbox_dashboard experiment is enabled. Waiting hints carry only
// agent ID and activity, which project members can already observe on the
// project status stream.
func (s *Server) publishInboxHint(ctx context.Context, userID string, hint InboxHint) {
	if userID == "" || !s.experimentEnabled(experiments.InboxDashboard) {
		return
	}
	s.events.PublishUserInboxHint(ctx, userID, hint)
}

// inboxWaitingHintRecipients returns the deduplicated user IDs that should
// receive waiting inbox invalidation hints for an agent: its owner (if set)
// plus all members of projectID, so teammates whose Agents Waiting count
// includes the agent refresh live (ptone/scion#9300).
func (s *Server) inboxWaitingHintRecipients(ctx context.Context, projectID, ownerID string) []string {
	seen := make(map[string]struct{})
	var out []string
	add := func(uid string) {
		if uid == "" {
			return
		}
		if _, ok := seen[uid]; ok {
			return
		}
		seen[uid] = struct{}{}
		out = append(out, uid)
	}
	add(ownerID)
	if projectID != "" && s.store != nil {
		lctx, cancel := context.WithTimeout(ctx, 500*time.Millisecond)
		defer cancel()
		if members, err := s.store.ListProjectMembers(lctx, projectID); err == nil {
			for _, m := range members {
				if m != nil {
					add(m.UserID)
				}
			}
		}
	}
	return out
}

// handleUserMeInboxSummary serves GET /api/v1/users/me/inbox/summary.
// Non-GET methods return 405 before the experiment gate, which returns 404
// while web.inbox_dashboard is off.
func (s *Server) handleUserMeInboxSummary(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		MethodNotAllowed(w, http.MethodGet)
		return
	}
	s.requireExperiment(experiments.InboxDashboard, s.getUserMeInboxSummary)(w, r)
}

func (s *Server) getUserMeInboxSummary(w http.ResponseWriter, r *http.Request) {
	viewer, ok := s.inboxSummaryCaller(w, r)
	if !ok {
		return
	}
	previews, chatFilter, ok := parseInboxSummaryQuery(w, r)
	if !ok {
		return
	}

	flightKey := fmt.Sprintf("%s|%d|%s|%s", viewer.userID, previews, chatFilter, viewer.credKey())
	val, _, _ := s.inboxSummaryFlight.Do(flightKey, func() (any, error) {
		ctx := context.WithoutCancel(r.Context())
		return s.buildInboxSummary(ctx, viewer, previews, chatFilter), nil
	})

	resp := val.(InboxSummaryResponse)
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, resp)
}

func (s *Server) inboxSummaryCaller(w http.ResponseWriter, r *http.Request) (inboxViewer, bool) {
	ctx := r.Context()
	if GetAgentIdentityFromContext(ctx) != nil {
		writeError(w, http.StatusForbidden, ErrCodeForbidden, "agents cannot read inbox summary", nil)
		return inboxViewer{}, false
	}
	identity := GetIdentityFromContext(ctx)
	if isNilIdentity(identity) {
		Unauthorized(w)
		return inboxViewer{}, false
	}
	if isVisitorIdentity(identity) {
		logAuthzDenial(r, identity, Resource{Type: "user", ID: identity.ID()}, ActionRead, "inbox summary is not open to visitors")
		writeVisitorRefusal(w)
		return inboxViewer{}, false
	}
	cls, token := inboxCredentialOf(ctx)
	if cls != inboxCredentialSession && cls != inboxCredentialToken {
		logAuthzDenial(r, identity, Resource{Type: "user", ID: identity.ID()}, ActionRead, "credential cannot read inbox summary")
		Forbidden(w)
		return inboxViewer{}, false
	}
	if cls == inboxCredentialToken {
		if !s.authorizeInboxToken(w, r, token, permInboxRead) {
			return inboxViewer{}, false
		}
	}
	user, ok := identity.(UserIdentity)
	if !ok {
		Forbidden(w)
		return inboxViewer{}, false
	}
	return inboxViewer{
		userID: user.ID(),
		user:   user,
		token:  token,
	}, true
}

func parseInboxSummaryQuery(w http.ResponseWriter, r *http.Request) (previews int, chatFilter string, ok bool) {
	q := r.URL.Query()
	previews = inboxSummaryDefaultPreviews
	if q.Has("previews") {
		raw := strings.TrimSpace(q.Get("previews"))
		n, err := strconv.Atoi(raw)
		if err != nil || n < 0 || n > inboxSummaryMaxPreviews {
			ValidationError(w, fmt.Sprintf("previews must be an integer between 0 and %d", inboxSummaryMaxPreviews),
				map[string]interface{}{"field": "previews"})
			return 0, "", false
		}
		previews = n
	}

	chatFilter = strings.TrimSpace(q.Get("chat"))
	switch chatFilter {
	case "", InboxChatFilterAll:
		chatFilter = InboxChatFilterAll
	case InboxChatFilterDMs, InboxChatFilterMentions:
	default:
		ValidationError(w, fmt.Sprintf("chat %q is not valid; expected %q, %q, or %q",
			chatFilter, InboxChatFilterAll, InboxChatFilterDMs, InboxChatFilterMentions),
			map[string]interface{}{"field": "chat"})
		return 0, "", false
	}

	return previews, chatFilter, true
}

func (s *Server) inboxSources() []inboxSourceEntry {
	if s.inboxSourcesForTest != nil {
		return s.inboxSourcesForTest
	}
	return []inboxSourceEntry{
		{key: InboxCategoryPendingSecrets, run: s.inboxPendingSecretsCategory},
		{key: InboxCategoryWaiting, run: s.inboxWaitingCategory},
		{key: InboxCategoryReviews, run: s.inboxReviewsCategory},
		{key: InboxCategoryChat, run: s.inboxChatCategory},
		{key: InboxCategoryDecisions, run: s.inboxDecisionsCategory},
	}
}

func inboxDisabledCategory(href string) inboxSourceFunc {
	return func(_ context.Context, _ inboxViewer, _ int, _ string) InboxCategory {
		return InboxCategory{
			State:             InboxStateDisabled,
			Count:             0,
			Capped:            false,
			CountsTowardTotal: false,
			Href:              href,
			Previews:          []any{},
		}
	}
}

func (s *Server) buildInboxSummary(ctx context.Context, v inboxViewer, previews int, chatFilter string) InboxSummaryResponse {
	ctx = withAuthzInputMemo(ctx)
	ctx = withChatMemberProjectIDsMemo(ctx)
	ctx, cancel := context.WithTimeout(ctx, s.chatSearchTimeout())
	defer cancel()

	categories := make(map[string]InboxCategory, 5)
	for _, src := range s.inboxSources() {
		cctx, ccancel := context.WithTimeout(ctx, inboxCategoryTimeout)
		cat := src.run(cctx, v, previews, chatFilter)
		ccancel()
		if cat.Previews == nil {
			cat.Previews = []any{}
		}
		categories[src.key] = cat
	}

	total, totalCapped := sumInboxCounted(categories)
	return InboxSummaryResponse{
		GeneratedAt: time.Now().UTC().Format(time.RFC3339),
		Total:       total,
		TotalCapped: totalCapped,
		Categories:  categories,
	}
}

func sumInboxCounted(categories map[string]InboxCategory) (total int, capped bool) {
	for _, cat := range categories {
		if cat.State == InboxStateOK && cat.CountsTowardTotal {
			total += cat.Count
			if cat.Capped {
				capped = true
			}
		}
	}
	return total, capped
}

func inboxCategoryErrorCode(ctx context.Context, err error) string {
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return InboxErrorTimeout
	}
	return InboxErrorInternal
}

func (s *Server) inboxPendingSecretsCategory(ctx context.Context, v inboxViewer, previews int, _ string) InboxCategory {
	const href = "/profile/secrets"
	if v.token != nil {
		return InboxCategory{
			State:             InboxStateUnavailable,
			Count:             0,
			Capped:            false,
			CountsTowardTotal: false,
			Href:              href,
			Previews:          []any{},
		}
	}

	rows, err := s.store.ListPendingSecretsByOwner(ctx, store.ScopeUser, v.userID)
	if err != nil {
		code := inboxCategoryErrorCode(ctx, err)
		slog.Warn("inbox summary: category source failed",
			"category", InboxCategoryPendingSecrets,
			"error_code", code,
			"error", err)
		return InboxCategory{
			State:             InboxStateError,
			Count:             0,
			Capped:            false,
			CountsTowardTotal: false,
			Href:              href,
			ErrorCode:         code,
			Previews:          []any{},
		}
	}

	now := time.Now()
	live := make([]store.PendingSecret, 0, len(rows))
	for _, p := range rows {
		if p.Expired(now) {
			continue
		}
		live = append(live, p)
	}

	sort.Slice(live, func(i, j int) bool {
		if !live[i].ExpiresAt.Equal(live[j].ExpiresAt) {
			return live[i].ExpiresAt.Before(live[j].ExpiresAt)
		}
		if !live[i].Created.Equal(live[j].Created) {
			return live[i].Created.Before(live[j].Created)
		}
		return live[i].ID < live[j].ID
	})

	seenAgents := make(map[string]struct{}, len(live))
	seenProjects := make(map[string]struct{}, len(live))
	for _, p := range live {
		agentKey := p.AgentID
		if agentKey == "" {
			agentKey = p.AgentSlug
		}
		if agentKey != "" {
			seenAgents[agentKey] = struct{}{}
		}
		if p.ProjectID != "" {
			seenProjects[p.ProjectID] = struct{}{}
		}
	}

	var nextExpiresAt *time.Time
	if len(live) > 0 && !live[0].ExpiresAt.IsZero() {
		t := live[0].ExpiresAt.UTC()
		nextExpiresAt = &t
	}

	capN := previews
	if capN > len(live) {
		capN = len(live)
	}
	items := make([]any, 0, capN)
	for i := 0; i < capN; i++ {
		p := live[i]
		items = append(items, InboxPendingSecretPreview{
			ID:               p.ID,
			Kind:             InboxPendingSecretKind,
			ProjectID:        p.ProjectID,
			ProjectName:      p.ProjectName,
			CreatedAt:        p.Created,
			Href:             href,
			Key:              p.Key,
			AgentSlug:        p.AgentSlug,
			InjectionMode:    p.InjectionMode,
			ReplacesExisting: p.ReplacesSecretID != "",
			AllowProgeny:     p.AllowProgeny,
			ExpiresAt:        p.ExpiresAt,
		})
	}

	return InboxCategory{
		State:             InboxStateOK,
		Count:             len(live),
		Capped:            len(live) >= store.PendingSecretOwnerCap,
		CountsTowardTotal: true,
		Href:              href,
		Previews:          items,
		Breakdown: map[string]int{
			InboxBreakdownAgents:   len(seenAgents),
			InboxBreakdownProjects: len(seenProjects),
		},
		NextExpiresAt: nextExpiresAt,
	}
}

func waitingAgentSince(a store.Agent) time.Time {
	if a.ActivitySince != nil && !a.ActivitySince.IsZero() {
		return a.ActivitySince.UTC()
	}
	if !a.LastActivityEvent.IsZero() {
		return a.LastActivityEvent.UTC()
	}
	if !a.Updated.IsZero() {
		return a.Updated.UTC()
	}
	return a.Created.UTC()
}

func inboxAgentDetailHref(agentID string) string {
	return "/agents/" + agentID
}

func (s *Server) waitingLimit() int {
	if s.inboxWaitingLimitForTest > 0 {
		return s.inboxWaitingLimitForTest
	}
	return inboxWaitingLimit
}

func (s *Server) inboxWaitingCategory(ctx context.Context, v inboxViewer, previews int, _ string) InboxCategory {
	const href = "/agents?q=activity%3Awaiting_for_input"
	limit := s.waitingLimit()

	scopeResult, err := s.authzService.ResolveListScopes(ctx, v.user, "agent.list")
	if err != nil {
		code := inboxCategoryErrorCode(ctx, err)
		slog.Warn("inbox summary: category source failed",
			"category", InboxCategoryWaiting,
			"error_code", code,
			"error", err)
		return InboxCategory{
			State:             InboxStateError,
			Count:             0,
			Capped:            false,
			CountsTowardTotal: false,
			Href:              href,
			ErrorCode:         code,
			Previews:          []any{},
		}
	}
	if scopeResult.Scopes.IsNone() {
		return InboxCategory{
			State:             InboxStateOK,
			Count:             0,
			Capped:            false,
			CountsTowardTotal: true,
			Href:              href,
			Previews:          []any{},
			AgentIDs:          []string{},
			Breakdown: map[string]int{
				InboxBreakdownProjects: 0,
			},
		}
	}

	filter := store.AgentFilter{
		Activity: string(state.ActivityWaitingForInput),
	}
	if !scopeResult.Scopes.IsAll() {
		filter.AuthorizedProjectIDs = canonicalizeStringSlice(scopeResult.Scopes.ProjectIDs())
	}
	if len(scopeResult.ExcludedProjectIDs) > 0 {
		filter.ExcludedProjectIDs = canonicalizeStringSlice(append([]string{}, scopeResult.ExcludedProjectIDs...))
	}
	// Defence in depth behind ResolveListScopes's applyCredentialCaveats:
	// pin project-bounded tokens to their boundary project in SQL as well.
	if v.token != nil {
		if boundaryProject := tokenBoundaryProject(v.token); boundaryProject != "" {
			filter.ProjectID = boundaryProject
		}
	}

	// Request oldest-updated (COALESCE(last_activity_event, updated) ASC) so
	// when a caller has more than `limit` waiting agents, the window keeps the
	// longest-waiting agents rather than dropping them under created DESC.
	res, err := s.store.ListAgents(ctx, filter, store.ListOptions{
		Limit:          limit,
		SkipTotalCount: true,
		SortBy:         agentsort.Updated,
		SortDir:        agentsort.Asc,
	})
	if err != nil {
		code := inboxCategoryErrorCode(ctx, err)
		slog.Warn("inbox summary: category source failed",
			"category", InboxCategoryWaiting,
			"error_code", code,
			"error", err)
		return InboxCategory{
			State:             InboxStateError,
			Count:             0,
			Capped:            false,
			CountsTowardTotal: false,
			Href:              href,
			ErrorCode:         code,
			Previews:          []any{},
		}
	}

	// Capped is computed on the pre-read-filter window, so if unreadable rows
	// are dropped by readableAgentRows below it remains a conservative lower-
	// bound signal ("N+ agents waiting").
	capped := len(res.Items) >= limit

	// Defence in depth behind ResolveListScopes's applyCredentialCaveats:
	// re-verify each row's project against the token's inbox:read self-scope.
	var candidates []store.Agent
	if v.token != nil {
		scopeCheck := s.newSelfScopeCheck(ctx, v.token, permInboxRead)
		candidates = make([]store.Agent, 0, len(res.Items))
		for _, a := range res.Items {
			if scopeCheck.allows(a.ProjectID) {
				candidates = append(candidates, a)
			}
		}
	} else {
		candidates = res.Items
	}

	matched, err := s.readableAgentRows(ctx, v.user, candidates)
	if err != nil {
		code := inboxCategoryErrorCode(ctx, err)
		slog.Warn("inbox summary: category source failed",
			"category", InboxCategoryWaiting,
			"error_code", code,
			"error", err)
		return InboxCategory{
			State:             InboxStateError,
			Count:             0,
			Capped:            false,
			CountsTowardTotal: false,
			Href:              href,
			ErrorCode:         code,
			Previews:          []any{},
		}
	}

	sort.Slice(matched, func(i, j int) bool {
		si, sj := waitingAgentSince(matched[i]), waitingAgentSince(matched[j])
		if !si.Equal(sj) {
			return si.Before(sj)
		}
		if !matched[i].Created.Equal(matched[j].Created) {
			return matched[i].Created.Before(matched[j].Created)
		}
		return matched[i].ID < matched[j].ID
	})

	seenProjects := make(map[string]struct{}, len(matched))
	agentIDs := make([]string, len(matched))
	for i, a := range matched {
		agentIDs[i] = a.ID
		if a.ProjectID != "" {
			seenProjects[a.ProjectID] = struct{}{}
		}
	}

	capN := previews
	if capN > len(matched) {
		capN = len(matched)
	}

	projectNames := make(map[string]string, capN)
	if capN > 0 {
		seen := make(map[string]struct{}, capN)
		projIDs := make([]string, 0, capN)
		for i := 0; i < capN; i++ {
			pid := matched[i].ProjectID
			if pid == "" {
				continue
			}
			if _, ok := seen[pid]; !ok {
				seen[pid] = struct{}{}
				projIDs = append(projIDs, pid)
			}
		}
		if len(projIDs) > 0 {
			if projRes, pErr := s.store.ListProjectSummaries(ctx, store.ProjectFilter{
				MemberProjectIDs: projIDs,
			}, store.ListOptions{
				Limit:          len(projIDs),
				SkipTotalCount: true,
			}); pErr == nil && projRes != nil {
				for _, p := range projRes.Items {
					projectNames[p.ID] = p.Name
				}
			}
		}
	}

	items := make([]any, 0, capN)
	for i := 0; i < capN; i++ {
		a := matched[i]
		items = append(items, InboxWaitingAgentPreview{
			ID:          a.ID,
			ProjectID:   a.ProjectID,
			ProjectName: projectNames[a.ProjectID],
			CreatedAt:   a.Created,
			Href:        inboxAgentDetailHref(a.ID),
			AgentID:     a.ID,
			AgentName:   a.Name,
			AgentSlug:   a.Slug,
			Question:    truncateRunes(a.Message, inboxPreviewQuestionMax),
			Since:       waitingAgentSince(a),
		})
	}

	return InboxCategory{
		State:             InboxStateOK,
		Count:             len(matched),
		Capped:            capped,
		CountsTowardTotal: true,
		Href:              href,
		Previews:          items,
		AgentIDs:          agentIDs,
		Breakdown: map[string]int{
			InboxBreakdownProjects: len(seenProjects),
		},
	}
}

func inboxDecisionPromptHref(set *decisionScopeSet, item classifiedOpenPrompt) string {
	const fallback = "/profile/decisions?tab=pending"
	p := item.raw
	if set == nil || p == nil || !set.conversationAvailable[p.ChatKey] {
		return fallback
	}
	if strings.HasPrefix(p.ChatKey, "dm:") {
		if item.promptAvail && p.MessageID != "" {
			return starDMPermalink(p.ChatKey, p.MessageID)
		}
		return chatDMWebPath(p.ChatKey)
	}
	if p.ProjectID == "" {
		return fallback
	}
	if item.promptAvail && p.MessageID != "" {
		return starPermalink(p.ProjectID, p.ChatKey, p.MessageID)
	}
	return chatThreadWebPath(p.ProjectID, p.ChatKey)
}

func (s *Server) inboxDecisionsCategory(ctx context.Context, v inboxViewer, previews int, _ string) InboxCategory {
	const href = "/profile/decisions?tab=pending"
	if !s.experimentEnabled(experiments.Decisions) {
		return InboxCategory{
			State:             InboxStateDisabled,
			Count:             0,
			Capped:            false,
			CountsTowardTotal: false,
			Href:              href,
			Previews:          []any{},
		}
	}

	res, err := s.waitingOnYouSummary(ctx, v.user, v.token, previews)
	if err != nil {
		code := inboxCategoryErrorCode(ctx, err)
		slog.Warn("inbox summary: category source failed",
			"category", InboxCategoryDecisions,
			"error_code", code,
			"error", err)
		return InboxCategory{
			State:             InboxStateError,
			Count:             0,
			Capped:            false,
			CountsTowardTotal: false,
			Href:              href,
			ErrorCode:         code,
			Previews:          []any{},
		}
	}

	items := make([]any, 0, len(res.prompts))
	for _, item := range res.prompts {
		p := item.raw
		if p == nil {
			continue
		}
		projRef := res.scopeSet.projectRef(p.ProjectID)
		askerName := p.AskerAgentName
		if askerName == "" {
			askerName = res.scopeSet.agentNames[p.AskerAgentID]
		}
		var question string
		var firstDec *store.Decision
		for _, d := range p.Decisions {
			if d != nil {
				firstDec = d
				question = truncateRunes(plainLine(d.Question), decisionResponseQuestionMax)
				break
			}
		}
		decCount := len(p.Decisions)
		answerMode := decisionAnswerMode(decCount)

		var inline *InboxDecisionInline
		if answerMode == DecisionAnswerModeSingle && decCount == 1 && item.canAnswer && firstDec != nil {
			choices := make([]InboxDecisionChoice, 0, len(firstDec.Choices))
			for _, c := range firstDec.Choices {
				choices = append(choices, InboxDecisionChoice{
					ID:    c.ID,
					Label: c.Label,
				})
			}
			inline = &InboxDecisionInline{
				DecisionRowID: firstDec.ID,
				Choices:       choices,
				AllowsOther:   firstDec.Other != nil,
			}
		}

		items = append(items, InboxDecisionPreview{
			ID:                p.MessageID,
			ProjectID:         p.ProjectID,
			ProjectName:       projRef.Name,
			CreatedAt:         p.CreatedAt,
			Href:              inboxDecisionPromptHref(res.scopeSet, item),
			PromptMessageID:   p.MessageID,
			ConversationLabel: res.scopeSet.conversationLabel[p.ChatKey],
			AskerAgentName:    askerName,
			Question:          question,
			DecisionCount:     decCount,
			AnswerMode:        answerMode,
			CanAnswer:         item.canAnswer,
			DisabledReason:    item.disabledReason,
			Inline:            inline,
		})
	}

	return InboxCategory{
		State:             InboxStateOK,
		Count:             res.total,
		Capped:            res.truncatedProjects,
		CountsTowardTotal: true,
		Href:              href,
		Previews:          items,
		Breakdown: map[string]int{
			InboxBreakdownAgents:   res.agents,
			InboxBreakdownProjects: res.projects,
		},
	}
}
