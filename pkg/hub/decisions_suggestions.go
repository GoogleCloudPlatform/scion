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
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/GoogleCloudPlatform/scion/pkg/experiments"
	"github.com/GoogleCloudPlatform/scion/pkg/filterquery"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

const (
	defaultDecisionSuggestionsLimit  = 20
	maxDecisionSuggestionsLimit      = 100
	maxDecisionSuggestionPrefixRunes = 200
	maxDecisionSuggestionCursorBytes = 512

	errCodeInvalidKey    = "invalid_key"
	errCodeInvalidCursor = "invalid_cursor"
)

type decisionFacetKind string

const (
	decisionFacetAll          decisionFacetKind = ""
	decisionFacetRawOnly      decisionFacetKind = "raw"
	decisionFacetAgent        decisionFacetKind = "agent"
	decisionFacetAnswerer     decisionFacetKind = "by"
	decisionFacetConversation decisionFacetKind = "conversation"
)

// ProjectDecisionFacetsTruncated reports which filter facets were truncated at
// maxProjectDecisionFacetOptions in a decisions list response (design-8985 §3.3).
type ProjectDecisionFacetsTruncated struct {
	Agents        bool `json:"agents,omitempty"`
	Answerers     bool `json:"answerers,omitempty"`
	Conversations bool `json:"conversations,omitempty"`
}

// DecisionSuggestionValue is one suggestion option returned by
// GET /api/v1/projects/{id}/decisions/suggestions and
// GET /api/v1/users/me/decisions/suggestions (design-8985 §3.3).
type DecisionSuggestionValue struct {
	Value string `json:"value"`
	Help  string `json:"help,omitempty"`
}

// DecisionSuggestionsResponse is the 200 envelope for decision value suggestions.
type DecisionSuggestionsResponse struct {
	Key        string                    `json:"key"`
	Values     []DecisionSuggestionValue `json:"values"`
	Total      int                       `json:"total"`
	NextCursor string                    `json:"nextCursor,omitempty"`
}

type decisionSuggestionCursor struct {
	Offset     int    `json:"offset"`
	Key        string `json:"key"`
	PrefixHash string `json:"prefixHash"`
}

type decisionSuggestionsQuery struct {
	key    string
	prefix string
	limit  int
	offset int
}

type decisionFacetsBundle struct {
	rawAgents     []store.ProjectDecisionActor
	rawAnswerers  []store.ProjectDecisionActor
	agents        []ProjectDecisionActorView
	answerers     []ProjectDecisionActorView
	conversations []ProjectDecisionConversationView
	projects      []ProjectDecisionProjectRef
	truncated     *ProjectDecisionFacetsTruncated
}

// buildDecisionFacets loads and formats decision filter facets for set.
// When kind == decisionFacetAll, all facets are built (and projectsFacet when
// includeProjects is true). When kind names a single facet (agent, by,
// conversation), only that facet is built. When capped is true, each facet is
// capped at maxProjectDecisionFacetOptions and truncated flags are populated.
func (s *Server) buildDecisionFacets(
	ctx context.Context,
	set *decisionScopeSet,
	kind decisionFacetKind,
	capped bool,
	includeProjects bool,
) (decisionFacetsBundle, error) {
	out := decisionFacetsBundle{
		agents:        []ProjectDecisionActorView{},
		answerers:     []ProjectDecisionActorView{},
		conversations: []ProjectDecisionConversationView{},
		projects:      []ProjectDecisionProjectRef{},
	}
	if set == nil || len(set.chatKeys) == 0 {
		return out, nil
	}

	facetAgents, facetAnswerers, convKeys, err := s.store.ListDecisionFacets(ctx, set.projectIDs, set.chatKeys)
	if err != nil {
		return decisionFacetsBundle{}, err
	}
	out.rawAgents = facetAgents
	out.rawAnswerers = facetAnswerers

	var truncAgents, truncAnswerers, truncConvs bool
	if kind == decisionFacetAll || kind == decisionFacetAgent {
		out.agents, truncAgents = decisionAgentFacetViews(set, facetAgents, capped)
	}
	if kind == decisionFacetAll || kind == decisionFacetAnswerer {
		out.answerers, truncAnswerers = decisionAnswererFacetViews(facetAnswerers, capped)
	}
	if kind == decisionFacetAll || kind == decisionFacetConversation {
		out.conversations, truncConvs = decisionConversationFacetViews(set, convKeys, capped)
	}
	if kind == decisionFacetAll && includeProjects {
		out.projects = decisionProjectFacetViews(set, convKeys)
	}
	if truncAgents || truncAnswerers || truncConvs {
		out.truncated = &ProjectDecisionFacetsTruncated{
			Agents:        truncAgents,
			Answerers:     truncAnswerers,
			Conversations: truncConvs,
		}
	}
	return out, nil
}

func decisionSuggestionPrefixHash(prefix string) string {
	sum := sha256.Sum256([]byte(prefix))
	return hex.EncodeToString(sum[:16])
}

func encodeDecisionSuggestionCursor(offset int, key, prefix string) (string, error) {
	raw, err := json.Marshal(decisionSuggestionCursor{
		Offset:     offset,
		Key:        key,
		PrefixHash: decisionSuggestionPrefixHash(prefix),
	})
	if err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(raw), nil
}

func decodeDecisionSuggestionCursor(token, key, prefix string) (int, error) {
	trimmed := strings.TrimSpace(token)
	if len(trimmed) > maxDecisionSuggestionCursorBytes {
		return 0, errors.New("cursor too long")
	}
	raw, err := base64.RawURLEncoding.DecodeString(trimmed)
	if err != nil {
		return 0, err
	}
	var c decisionSuggestionCursor
	if err := json.Unmarshal(raw, &c); err != nil {
		return 0, err
	}
	if c.Offset <= 0 || c.Key != key || c.PrefixHash != decisionSuggestionPrefixHash(prefix) {
		return 0, errors.New("cursor mismatch")
	}
	return c.Offset, nil
}

func parseDecisionSuggestionsQuery(w http.ResponseWriter, r *http.Request) (decisionSuggestionsQuery, bool) {
	q := r.URL.Query()
	key := strings.ToLower(strings.TrimSpace(q.Get("key")))
	switch decisionFacetKind(key) {
	case decisionFacetAgent, decisionFacetConversation, decisionFacetAnswerer:
	default:
		writeError(w, http.StatusBadRequest, errCodeInvalidKey,
			"key must be one of agent, conversation, by",
			map[string]interface{}{"field": "key"})
		return decisionSuggestionsQuery{}, false
	}

	prefix := q.Get("prefix")
	if utf8.RuneCountInString(prefix) > maxDecisionSuggestionPrefixRunes {
		ValidationError(w, fmt.Sprintf("prefix must be at most %d characters", maxDecisionSuggestionPrefixRunes),
			map[string]interface{}{"field": "prefix"})
		return decisionSuggestionsQuery{}, false
	}

	limit := defaultDecisionSuggestionsLimit
	if q.Has("limit") {
		rawLimit := strings.TrimSpace(q.Get("limit"))
		if rawLimit != "" {
			n, err := strconv.Atoi(rawLimit)
			if err != nil || n < 1 || n > maxDecisionSuggestionsLimit {
				ValidationError(w, fmt.Sprintf("limit must be an integer between 1 and %d", maxDecisionSuggestionsLimit),
					map[string]interface{}{"field": "limit"})
				return decisionSuggestionsQuery{}, false
			}
			limit = n
		}
	}

	offset := 0
	if rawCursor := strings.TrimSpace(q.Get("cursor")); rawCursor != "" {
		off, err := decodeDecisionSuggestionCursor(rawCursor, key, prefix)
		if err != nil {
			writeError(w, http.StatusBadRequest, errCodeInvalidCursor,
				"invalid cursor", map[string]interface{}{"field": "cursor"})
			return decisionSuggestionsQuery{}, false
		}
		offset = off
	}

	return decisionSuggestionsQuery{
		key:    key,
		prefix: prefix,
		limit:  limit,
		offset: offset,
	}, true
}

// handleProjectDecisionSuggestions serves
// GET /api/v1/projects/{id}/decisions/suggestions (design-8985 §3.3).
// Unsupported methods return 405 before the experiment gates, which return 404
// while web.decisions or filters.query_dialect is off.
func (s *Server) handleProjectDecisionSuggestions(w http.ResponseWriter, r *http.Request, projectID string) {
	if r.Method != http.MethodGet {
		MethodNotAllowed(w, http.MethodGet)
		return
	}
	s.requireExperiment(experiments.Decisions, func(w http.ResponseWriter, r *http.Request) {
		if !s.experimentEnabled(experiments.FiltersQueryDialect) {
			NotFound(w, "Decision suggestions")
			return
		}
		s.listProjectDecisionSuggestions(w, r, projectID)
	})(w, r)
}

func (s *Server) listProjectDecisionSuggestions(w http.ResponseWriter, r *http.Request, projectID string) {
	ctx := r.Context()
	user, token, ok := projectDecisionsCaller(w, r, projectID)
	if !ok {
		return
	}
	sq, ok := parseDecisionSuggestionsQuery(w, r)
	if !ok {
		return
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
	if !s.authorize(w, r, projectResource(project), ActionRead) {
		return
	}

	s.mu.RLock()
	wcs := s.webChatStore
	s.mu.RUnlock()

	searchCtx, cancel := context.WithTimeout(ctx, s.chatSearchTimeout())
	defer cancel()

	scope, err := s.resolveProjectReadableScope(searchCtx, project, user, token, wcs)
	if err != nil {
		writeDecisionsError(w, r, searchCtx, err)
		return
	}
	resp, err := s.decisionSuggestionsCore(searchCtx, user, newDecisionScopeSet([]*projectReadableScope{scope}), sq, false)
	if err == nil && errors.Is(searchCtx.Err(), context.DeadlineExceeded) {
		err = searchCtx.Err()
	}
	if err != nil {
		writeDecisionsError(w, r, searchCtx, err)
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

// handleUserMeDecisionSuggestions serves
// GET /api/v1/users/me/decisions/suggestions (design-8985 §3.3).
// Unsupported methods return 405 before the experiment gates, which return 404
// while web.decisions or filters.query_dialect is off.
func (s *Server) handleUserMeDecisionSuggestions(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		MethodNotAllowed(w, http.MethodGet)
		return
	}
	s.requireExperiment(experiments.Decisions, func(w http.ResponseWriter, r *http.Request) {
		if !s.experimentEnabled(experiments.FiltersQueryDialect) {
			NotFound(w, "Decision suggestions")
			return
		}
		s.listUserMeDecisionSuggestions(w, r)
	})(w, r)
}

func (s *Server) listUserMeDecisionSuggestions(w http.ResponseWriter, r *http.Request) {
	user, token, ok := profileDecisionsCaller(w, r)
	if !ok {
		return
	}
	sq, ok := parseDecisionSuggestionsQuery(w, r)
	if !ok {
		return
	}

	s.mu.RLock()
	wcs := s.webChatStore
	s.mu.RUnlock()

	rawSearchCtx, cancel := context.WithTimeout(r.Context(), s.chatSearchTimeout())
	defer cancel()
	searchCtx := withAuthzInputMemo(rawSearchCtx)

	selected, _, err := s.selectProfileDecisionProjects(searchCtx, user, token, "", false)
	if err != nil {
		writeDecisionsError(w, r, searchCtx, err)
		return
	}
	scopes, err := s.resolveProfileDecisionScopes(searchCtx, selected, user, token, wcs)
	if err != nil {
		writeDecisionsError(w, r, searchCtx, err)
		return
	}
	resp, err := s.decisionSuggestionsCore(searchCtx, user, newDecisionScopeSet(scopes), sq, true)
	if err == nil && errors.Is(searchCtx.Err(), context.DeadlineExceeded) {
		err = searchCtx.Err()
	}
	if err != nil {
		writeDecisionsError(w, r, searchCtx, err)
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

func (s *Server) decisionSuggestionsCore(
	ctx context.Context,
	user UserIdentity,
	set *decisionScopeSet,
	sq decisionSuggestionsQuery,
	profile bool,
) (DecisionSuggestionsResponse, error) {
	bundle, err := s.buildDecisionFacets(ctx, set, decisionFacetKind(sq.key), false, false)
	if err != nil {
		return DecisionSuggestionsResponse{}, err
	}

	candidates := decisionSuggestionCandidates(set, user, bundle, decisionFacetKind(sq.key), profile)
	normPrefix := filterquery.NormalizeQuery(sq.prefix)

	type rankedSuggestion struct {
		item       DecisionSuggestionValue
		tier       int
		lowerValue string
		lowerHelp  string
	}
	ranked := make([]rankedSuggestion, 0, len(candidates))
	for _, c := range candidates {
		tier := filterquery.MatchTierExact
		if normPrefix != "" {
			t, ok := filterquery.ClassifyMatch(normPrefix, c.Value)
			if ok {
				tier = t
			} else if c.Help != "" {
				ht, hok := filterquery.ClassifyMatch(normPrefix, c.Help)
				if !hok {
					continue
				}
				tier = ht + 4
			} else {
				continue
			}
		}
		ranked = append(ranked, rankedSuggestion{
			item:       c,
			tier:       tier,
			lowerValue: strings.ToLower(c.Value),
			lowerHelp:  strings.ToLower(c.Help),
		})
	}

	sort.Slice(ranked, func(i, j int) bool {
		if ranked[i].tier != ranked[j].tier {
			return ranked[i].tier < ranked[j].tier
		}
		if ranked[i].lowerValue != ranked[j].lowerValue {
			return ranked[i].lowerValue < ranked[j].lowerValue
		}
		if ranked[i].lowerHelp != ranked[j].lowerHelp {
			return ranked[i].lowerHelp < ranked[j].lowerHelp
		}
		if ranked[i].item.Value != ranked[j].item.Value {
			return ranked[i].item.Value < ranked[j].item.Value
		}
		return ranked[i].item.Help < ranked[j].item.Help
	})

	total := len(ranked)
	values := []DecisionSuggestionValue{}
	var nextCursor string
	if sq.offset < total {
		end := sq.offset + sq.limit
		if end > total {
			end = total
		}
		values = make([]DecisionSuggestionValue, 0, end-sq.offset)
		for _, r := range ranked[sq.offset:end] {
			values = append(values, r.item)
		}
		if end < total {
			nextCursor, err = encodeDecisionSuggestionCursor(end, sq.key, sq.prefix)
			if err != nil {
				return DecisionSuggestionsResponse{}, err
			}
		}
	}

	return DecisionSuggestionsResponse{
		Key:        sq.key,
		Values:     values,
		Total:      total,
		NextCursor: nextCursor,
	}, nil
}

func decisionSuggestionCandidates(
	set *decisionScopeSet,
	user UserIdentity,
	bundle decisionFacetsBundle,
	kind decisionFacetKind,
	profile bool,
) []DecisionSuggestionValue {
	seen := make(map[string]bool)
	var out []DecisionSuggestionValue
	add := func(val, help string) {
		if val == "" {
			return
		}
		k := val + "\x00" + help
		if seen[k] {
			return
		}
		seen[k] = true
		out = append(out, DecisionSuggestionValue{Value: val, Help: help})
	}

	switch kind {
	case decisionFacetAgent:
		for _, a := range bundle.agents {
			val := a.Name
			if val == "" {
				val = a.ID
			}
			help := a.Hint
			if profile && help == "" && a.ProjectID != "" {
				if p := set.projectRef(a.ProjectID); p.Name != "" {
					help = p.Name
				}
			}
			add(val, help)
		}

	case decisionFacetConversation:
		for _, c := range bundle.conversations {
			val := c.Label
			if val == "" {
				val = c.Key
			}
			help := ""
			if profile && c.ProjectID != "" {
				if p := set.projectRef(c.ProjectID); p.Name != "" {
					help = p.Name
				}
			}
			add(val, help)
		}

	case decisionFacetAnswerer:
		viewerID := ""
		viewerDisplayName := ""
		if user != nil {
			viewerID = user.ID()
			viewerDisplayName = user.DisplayName()
		}
		nameCounts := make(map[string]int, len(bundle.rawAnswerers))
		selfName := ""
		for _, a := range bundle.rawAnswerers {
			if viewerID != "" && a.ID == viewerID && a.Name != "" {
				selfName = a.Name
			}
			if lower := strings.ToLower(a.Name); lower != "" {
				nameCounts[lower]++
			}
		}
		if viewerID != "" {
			if selfName == "" {
				selfName = viewerDisplayName
			}
			help := "You"
			if selfName != "" {
				help = "You · " + selfName
			}
			add("me", help)
		}
		for _, a := range bundle.rawAnswerers {
			if viewerID != "" && a.ID == viewerID {
				continue
			}
			lower := strings.ToLower(a.Name)
			if lower != "" && nameCounts[lower] > 1 && a.Email != "" {
				add(a.Email, a.Name)
				continue
			}
			val := a.Name
			if val == "" {
				val = a.Email
			}
			if val == "" {
				val = a.ID
			}
			help := ""
			if a.Email != "" && !strings.EqualFold(a.Email, val) {
				help = a.Email
			}
			add(val, help)
		}
	}

	return out
}
