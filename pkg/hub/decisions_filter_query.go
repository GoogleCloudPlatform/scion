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
	"fmt"
	"net/http"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/GoogleCloudPlatform/scion/pkg/filterquery"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// parseDecisionsFilterQ parses and validates the `q` query parameter for both
// project and profile decisions handlers (design-8985 §3.2). All queries are
// parsed and validated against schema (max 512 bytes, 200 runes of search text);
// pure-text queries also enforce the 2-rune minimum, while structured queries
// return parsed != nil and text = parsed.Text.
func parseDecisionsFilterQ(
	w http.ResponseWriter,
	rawQ string,
	schema filterquery.Schema,
) (text string, parsed *filterquery.Query, ok bool) {
	if strings.TrimSpace(rawQ) == "" {
		return "", nil, true
	}
	q, fqErr := filterquery.Parse(rawQ, schema.HasKey)
	if fqErr != nil {
		writeInvalidFilterQuery(w, fqErr)
		return "", nil, false
	}
	if vErr := schema.Validate(q); vErr != nil {
		writeInvalidFilterQuery(w, vErr)
		return "", nil, false
	}
	if q.Mode == filterquery.ModeText {
		if q.Text != "" {
			if n := utf8.RuneCountInString(q.Text); n < minProjectDecisionsQueryRunes {
				ValidationError(w, fmt.Sprintf("q must be at least %d characters", minProjectDecisionsQueryRunes),
					map[string]interface{}{"field": "q"})
				return "", nil, false
			}
			return q.Text, nil, true
		}
		return "", nil, true
	}
	return q.Text, &q, true
}

// resolvedDecisionsQuery is the scope-resolved translation of a structured
// filterquery.Query for GET /api/v1/projects/{id}/decisions and
// GET /api/v1/users/me/decisions (design-8985 §3.2).
// Every value is resolved strictly within the caller's decisionScopeSet and
// readable facet entries; an unresolvable positive term sets impossible=true so
// the handler returns an empty 200 without acting as an existence oracle.
type resolvedDecisionsQuery struct {
	listKeys           []string
	agentIDs           []string
	excludeAgentIDs    []string
	userIDs            []string
	excludeUserIDs     []string
	provenances        []string
	excludeProvenances []string
	excludeTexts       []string
	impossible         bool
}

func resolveProjectDecisionsFilterQuery(
	set *decisionScopeSet,
	viewerID string,
	pq projectDecisionsQuery,
	facetAgents []store.ProjectDecisionActor,
	facetAnswerers []store.ProjectDecisionActor,
	initialKeys []string,
) resolvedDecisionsQuery {
	res := resolvedDecisionsQuery{
		listKeys: initialKeys,
	}
	if pq.parsedQuery == nil || pq.parsedQuery.Mode != filterquery.ModeStructured {
		return res
	}

	q := pq.parsedQuery
	res.excludeTexts = q.ExcludeText

	var (
		hasPosAgents bool
		posAgents    map[string]bool
		excAgents    map[string]bool

		hasPosUsers bool
		posUsers    map[string]bool
		excUsers    map[string]bool

		hasPosProvs bool
		posProvs    map[string]bool
		excProvs    map[string]bool
	)

	// Separate query params AND with structured q terms.
	if pq.agentID != "" {
		hasPosAgents = true
		posAgents = map[string]bool{pq.agentID: true}
	}
	if pq.tab == ProjectDecisionsTabMade {
		if pq.madeFilter.UserID != "" {
			hasPosUsers = true
			posUsers = map[string]bool{pq.madeFilter.UserID: true}
		}
		if pq.madeFilter.Provenance != "" {
			hasPosProvs = true
			posProvs = map[string]bool{pq.madeFilter.Provenance: true}
		}
	}

	keySet := make(map[string]bool, len(initialKeys))
	for _, k := range initialKeys {
		keySet[k] = true
	}

	resolveProjects := func(values []string) map[string]bool {
		matched := make(map[string]bool)
		for _, raw := range values {
			for _, pid := range set.projectIDs {
				p := set.projects[pid]
				if p == nil {
					continue
				}
				if strings.EqualFold(p.ID, raw) || (p.Slug != "" && strings.EqualFold(p.Slug, raw)) {
					matched[pid] = true
				}
			}
		}
		return matched
	}

	resolveAgents := func(values []string) map[string]bool {
		matched := make(map[string]bool)
		for _, raw := range values {
			for _, a := range facetAgents {
				label := a.Name
				if live := set.liveAgents[a.ID]; live != nil {
					if name := agentDisplayName(live); name != "" {
						label = name
					}
				}
				if label == "" {
					label = set.agentNames[a.ID]
				}
				if strings.EqualFold(a.ID, raw) ||
					(label != "" && strings.EqualFold(label, raw)) ||
					(a.Name != "" && strings.EqualFold(a.Name, raw)) {
					matched[a.ID] = true
				}
			}
		}
		return matched
	}

	resolveConversations := func(values []string) map[string]bool {
		matched := make(map[string]bool)
		for _, raw := range values {
			for _, k := range set.chatKeys {
				if !set.readable[k] {
					continue
				}
				label := set.conversationLabel[k]
				if strings.EqualFold(k, raw) || (label != "" && strings.EqualFold(label, raw)) {
					matched[k] = true
				}
			}
		}
		return matched
	}

	resolveAnswerers := func(values []string) map[string]bool {
		matched := make(map[string]bool)
		for _, raw := range values {
			if strings.EqualFold(raw, "me") {
				if viewerID != "" {
					matched[viewerID] = true
				}
				continue
			}
			for _, a := range facetAnswerers {
				if strings.EqualFold(a.ID, raw) ||
					(a.Email != "" && strings.EqualFold(a.Email, raw)) ||
					(a.Name != "" && strings.EqualFold(a.Name, raw)) {
					matched[a.ID] = true
				}
			}
		}
		return matched
	}

	intersectSet := func(hasExisting bool, existing, incoming map[string]bool) (map[string]bool, bool) {
		if len(incoming) == 0 {
			return nil, false
		}
		if !hasExisting {
			return incoming, true
		}
		out := make(map[string]bool)
		for k := range existing {
			if incoming[k] {
				out[k] = true
			}
		}
		return out, len(out) > 0
	}

	for _, term := range q.Terms {
		key := strings.ToLower(term.Key)
		switch key {
		case "agent":
			matched := resolveAgents(term.Values)
			if term.Negate {
				if len(matched) > 0 {
					if excAgents == nil {
						excAgents = make(map[string]bool, len(matched))
					}
					for id := range matched {
						excAgents[id] = true
					}
				}
			} else {
				var ok bool
				posAgents, ok = intersectSet(hasPosAgents, posAgents, matched)
				hasPosAgents = true
				if !ok {
					res.impossible = true
					return res
				}
			}

		case "conversation":
			matched := resolveConversations(term.Values)
			if term.Negate {
				for k := range matched {
					delete(keySet, k)
				}
			} else {
				if len(matched) == 0 {
					res.impossible = true
					return res
				}
				next := make(map[string]bool)
				for k := range keySet {
					if matched[k] {
						next[k] = true
					}
				}
				keySet = next
				if len(keySet) == 0 {
					res.impossible = true
					return res
				}
			}

		case "project":
			matched := resolveProjects(term.Values)
			if term.Negate {
				for k := range keySet {
					if matched[set.keyProject[k]] {
						delete(keySet, k)
					}
				}
			} else {
				if len(matched) == 0 {
					res.impossible = true
					return res
				}
				nextKeys := make(map[string]bool)
				for k := range keySet {
					if matched[set.keyProject[k]] {
						nextKeys[k] = true
					}
				}
				keySet = nextKeys
				if len(keySet) == 0 {
					res.impossible = true
					return res
				}
			}

		case "by":
			// Made-only key: ignored on Pending tab (design-8985 §3.1).
			if pq.tab != ProjectDecisionsTabMade {
				continue
			}
			matched := resolveAnswerers(term.Values)
			if term.Negate {
				if len(matched) > 0 {
					if excUsers == nil {
						excUsers = make(map[string]bool, len(matched))
					}
					for id := range matched {
						excUsers[id] = true
					}
				}
			} else {
				var ok bool
				posUsers, ok = intersectSet(hasPosUsers, posUsers, matched)
				hasPosUsers = true
				if !ok {
					res.impossible = true
					return res
				}
			}

		case "provenance":
			// Made-only key: ignored on Pending tab (design-8985 §3.1).
			if pq.tab != ProjectDecisionsTabMade {
				continue
			}
			matched := make(map[string]bool, len(term.Values))
			for _, v := range term.Values {
				matched[strings.ToLower(v)] = true
			}
			if term.Negate {
				if excProvs == nil {
					excProvs = make(map[string]bool, len(matched))
				}
				for p := range matched {
					excProvs[p] = true
				}
			} else {
				var ok bool
				posProvs, ok = intersectSet(hasPosProvs, posProvs, matched)
				hasPosProvs = true
				if !ok {
					res.impossible = true
					return res
				}
			}
		}
	}

	// Materialize surviving keys in original order.
	if len(keySet) == 0 {
		res.listKeys = nil
	} else {
		filteredKeys := make([]string, 0, len(keySet))
		for _, k := range initialKeys {
			if keySet[k] {
				filteredKeys = append(filteredKeys, k)
			}
		}
		res.listKeys = filteredKeys
	}

	if hasPosAgents {
		res.agentIDs = sortedMapKeys(posAgents)
	}
	if len(excAgents) > 0 {
		res.excludeAgentIDs = sortedMapKeys(excAgents)
	}
	if hasPosUsers {
		res.userIDs = sortedMapKeys(posUsers)
	}
	if len(excUsers) > 0 {
		res.excludeUserIDs = sortedMapKeys(excUsers)
	}
	if hasPosProvs {
		res.provenances = sortedMapKeys(posProvs)
	}
	if len(excProvs) > 0 {
		res.excludeProvenances = sortedMapKeys(excProvs)
	}

	return res
}

func sortedMapKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// decisionAnswererFacetViews builds the answerers facet sorted by display name
// (case-insensitive, tie-breaking by user ID) and capped at
// maxProjectDecisionFacetOptions (design-8985 §3.3).
func decisionAnswererFacetViews(answerers []store.ProjectDecisionActor) []ProjectDecisionActorView {
	out := make([]ProjectDecisionActorView, 0, len(answerers))
	for _, a := range answerers {
		hint := ""
		if a.Email != "" && !strings.EqualFold(a.Email, a.Name) {
			hint = a.Email
		}
		out = append(out, ProjectDecisionActorView{
			ID:   a.ID,
			Name: a.Name,
			Hint: hint,
		})
	}
	sort.Slice(out, func(i, j int) bool {
		ni, nj := strings.ToLower(out[i].Name), strings.ToLower(out[j].Name)
		if ni != nj {
			return ni < nj
		}
		return out[i].ID < out[j].ID
	})
	if len(out) > maxProjectDecisionFacetOptions {
		out = out[:maxProjectDecisionFacetOptions]
	}
	return out
}
