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
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/experiments"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

const (
	inboxSummaryDefaultPreviews = 3
	inboxSummaryMaxPreviews     = 10
	inboxCategoryTimeout        = 1500 * time.Millisecond

	// InboxCategory* are the five stable keys returned in InboxSummaryResponse.Categories.
	InboxCategoryDecisions = "decisions"
	InboxCategoryWaiting   = "waiting"
	InboxCategoryReviews   = "reviews"
	InboxCategoryApprovals = "approvals"
	InboxCategoryChat      = "chat"

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

	// InboxApprovalKindPendingSecret is the kind value for pending user-scope secrets.
	InboxApprovalKindPendingSecret = "pending_secret"
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
}

// InboxApprovalPreview is one preview item in the "approvals" category.
// Never contains secret values or ValueRef.
type InboxApprovalPreview struct {
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

type inboxViewer struct {
	userID string
	user   UserIdentity
	token  *ScopedUserIdentity
}

func (v inboxViewer) credKey() string {
	if v.token == nil {
		return "session"
	}
	return "uat:" + tokenBoundaryProject(v.token)
}

type inboxSourceFunc func(ctx context.Context, v inboxViewer, previews int, chatFilter string) InboxCategory

type inboxSourceEntry struct {
	key string
	run inboxSourceFunc
}

// publishInboxHint emits a content-free invalidation hint on user.<userID>.inbox
// when the web.inbox_dashboard experiment is enabled.
func (s *Server) publishInboxHint(ctx context.Context, userID string, hint InboxHint) {
	if userID == "" || !s.experimentEnabled(experiments.InboxDashboard) {
		return
	}
	s.events.PublishUserInboxHint(ctx, userID, hint)
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
		{key: InboxCategoryApprovals, run: s.inboxApprovalsCategory},
		{key: InboxCategoryWaiting, run: inboxDisabledCategory("/agents")},
		{key: InboxCategoryReviews, run: inboxDisabledCategory("/artifacts?review_pending=1&owner=me")},
		{key: InboxCategoryChat, run: inboxDisabledCategory("/chat?filter=unread")},
		{key: InboxCategoryDecisions, run: inboxDisabledCategory("/profile/decisions?tab=pending")},
	}
}

func inboxDisabledCategory(href string) inboxSourceFunc {
	return func(_ context.Context, _ inboxViewer, _ int, _ string) InboxCategory {
		return InboxCategory{
			State:             InboxStateDisabled,
			Count:             0,
			Capped:            false,
			CountsTowardTotal: true,
			Href:              href,
			Previews:          []any{},
		}
	}
}

func (s *Server) buildInboxSummary(ctx context.Context, v inboxViewer, previews int, chatFilter string) InboxSummaryResponse {
	ctx = withAuthzInputMemo(ctx)
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

func (s *Server) inboxApprovalsCategory(ctx context.Context, v inboxViewer, previews int, _ string) InboxCategory {
	const href = "/profile/secrets"
	if v.token != nil {
		return InboxCategory{
			State:             InboxStateUnavailable,
			Count:             0,
			Capped:            false,
			CountsTowardTotal: true,
			Href:              href,
			Previews:          []any{},
		}
	}

	rows, err := s.store.ListPendingSecretsByOwner(ctx, store.ScopeUser, v.userID)
	if err != nil {
		code := inboxCategoryErrorCode(ctx, err)
		slog.Warn("inbox summary: category source failed",
			"category", InboxCategoryApprovals,
			"error_code", code,
			"error", err)
		return InboxCategory{
			State:             InboxStateError,
			Count:             0,
			Capped:            false,
			CountsTowardTotal: true,
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

	capN := previews
	if capN > len(live) {
		capN = len(live)
	}
	items := make([]any, 0, capN)
	for i := 0; i < capN; i++ {
		p := live[i]
		items = append(items, InboxApprovalPreview{
			ID:               p.ID,
			Kind:             InboxApprovalKindPendingSecret,
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
		Capped:            len(rows) >= store.PendingSecretOwnerCap,
		CountsTowardTotal: true,
		Href:              href,
		Previews:          items,
	}
}
