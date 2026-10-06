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
	"net/http"
	"net/url"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// Agent list views selected by the "view" query parameter of
// GET /api/v1/agents and GET /api/v1/projects/{id}/agents.
const (
	agentListViewFull    = "full"
	agentListViewCompact = "compact"
)

// AgentCompactItem is one agent in a view=compact agent list: the fields a
// graph or tree consumer needs (identity, project, status, detail message,
// lineage, messaging mode, creator) plus the same per-item capabilities and
// messageability and deletion view the full view carries. It deliberately
// has no appliedConfig: the only value taken from the applied configuration
// is CreatorName.
//
// Message, the agent's detail status message, has the same type and tag as
// the full item's field (store.Agent.Message) and is copied from the same
// full item, so compact emits exactly what full emits for every caller,
// including omitting it when empty.
//
// The JSON key set is fixed by TestAgentCompactView_KeySetIsAllowlist.
type AgentCompactItem struct {
	ID              string            `json:"id"`
	Slug            string            `json:"slug"`
	Name            string            `json:"name"`
	Template        string            `json:"template,omitempty"`
	ProjectID       string            `json:"projectId"`
	Project         string            `json:"project,omitempty"`
	Labels          map[string]string `json:"labels,omitempty"`
	Phase           string            `json:"phase,omitempty"`
	Activity        string            `json:"activity,omitempty"`
	ContainerStatus string            `json:"containerStatus,omitempty"`
	Message         string            `json:"message,omitempty"`
	MessageMode     string            `json:"messageMode"`
	Ancestry        []string          `json:"ancestry,omitempty"`
	CreatedBy       string            `json:"createdBy,omitempty"`
	CreatorName     string            `json:"creatorName,omitempty"`
	Created         time.Time         `json:"created"`
	Updated         time.Time         `json:"updated"`
	// LastActivityEvent is always emitted, as 0001-01-01T00:00:00Z when
	// unset, exactly like the full view.
	LastActivityEvent time.Time     `json:"lastActivityEvent"`
	Cap               *Capabilities `json:"_capabilities,omitempty"`
	// Messageability has the same type and tag as the full item's field and
	// is copied as is, so compact emits exactly what full emits for any
	// value. It is set only where the full view sets it (the global
	// endpoint).
	Messageability interface{} `json:"_messageability,omitempty"`
	// Deletion has the same type and tag as the full item's field (always
	// emitted, an explicit null when no delete is active or failed) and is
	// copied as is, so a graph loaded from the compact view shows the
	// same deletion state as one loaded from the full view.
	Deletion *store.DeletionInfo `json:"deletion"`
}

// listAgentsCompactResponse is ListAgentsResponse with its agents replaced
// by compact items. The outer Agents field shadows the embedded one, so
// every other response field (nextCursor, totalCount, sort, dir, complete,
// stats, serverTime, _capabilities) is serialized from the very same value
// the full view would have written.
//
// This relies on ListAgentsResponse having no MarshalJSON method: one would
// be promoted onto this type and override the compact Agents field.
// TestAgentCompactView_ListAgentsResponseHasNoMarshalJSON fails if one is
// added.
type listAgentsCompactResponse struct {
	Agents []AgentCompactItem `json:"agents"`
	ListAgentsResponse
}

// toCompact maps one fully built full-view list item (capabilities,
// redaction and messageability already applied) to its compact form. It
// makes no decision and reads nothing: it only copies fields.
func toCompact(a AgentWithCapabilities) AgentCompactItem {
	item := AgentCompactItem{
		ID:                a.ID,
		Slug:              a.Slug,
		Name:              a.Name,
		Template:          a.Template,
		ProjectID:         a.ProjectID,
		Project:           a.Project,
		Labels:            a.Labels,
		Phase:             a.Phase,
		Activity:          a.Activity,
		ContainerStatus:   a.ContainerStatus,
		Message:           a.Message,
		MessageMode:       a.MessageMode,
		Ancestry:          a.Ancestry,
		CreatedBy:         a.CreatedBy,
		Created:           a.Created,
		Updated:           a.Updated,
		LastActivityEvent: a.LastActivityEvent,
		Cap:               a.Cap,
		Messageability:    a.Messageability,
		Deletion:          a.Deletion,
	}
	if a.AppliedConfig != nil {
		item.CreatorName = a.AppliedConfig.CreatorName
	}
	return item
}

// parseSortedAgentListView validates the view parameter of a sorted-mode
// request: absent, empty or "full" selects the full view, "compact" the compact
// view, and anything else is a 400 written here.
func parseSortedAgentListView(w http.ResponseWriter, query url.Values) (string, bool) {
	switch v := query.Get("view"); v {
	case "", agentListViewFull:
		return agentListViewFull, true
	case agentListViewCompact:
		return agentListViewCompact, true
	default:
		BadRequest(w, "invalid view")
		return "", false
	}
}

// legacyAgentListView returns the view of a request without sort. Only
// "compact" selects the compact view; any other value, valid or not, keeps
// the legacy response unchanged, because legacy callers never got a 400 for
// an unknown parameter.
func legacyAgentListView(query url.Values) string {
	if query.Get("view") == agentListViewCompact {
		return agentListViewCompact
	}
	return agentListViewFull
}

// writeAgentList writes a fully built agent list response in the requested
// view. It is the last step of every agent list response on both
// endpoints: everything that decides membership, order, cursors, counts,
// stats and capabilities has already run, identically for both views, and
// only the per-item shape differs.
func writeAgentList(w http.ResponseWriter, view string, resp ListAgentsResponse) {
	if view != agentListViewCompact {
		writeJSON(w, http.StatusOK, resp)
		return
	}
	var items []AgentCompactItem
	if resp.Agents != nil {
		items = make([]AgentCompactItem, len(resp.Agents))
		for i := range resp.Agents {
			items[i] = toCompact(resp.Agents[i])
		}
	}
	writeJSON(w, http.StatusOK, listAgentsCompactResponse{Agents: items, ListAgentsResponse: resp})
}
