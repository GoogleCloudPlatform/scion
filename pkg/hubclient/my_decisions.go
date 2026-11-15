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

package hubclient

import (
	"context"
	"net/url"
	"strconv"

	"github.com/GoogleCloudPlatform/scion/pkg/apiclient"
)

// Sort modes of GET /api/v1/users/me/decisions (design R4, R5, R6).
const (
	DecisionsSortRecent  = "recent"
	DecisionsSortProject = "project"
)

// MyDecisionService reads the calling user's Decisions roll-up across their
// member projects (GET /api/v1/users/me/decisions).
type MyDecisionService interface {
	// List returns one page (or overview) of the Waiting on you (tab=pending)
	// or Made (tab=made) roll-up with its facets and counts.
	List(ctx context.Context, opts *ListMyDecisionsOptions) (*MyDecisionsResponse, error)
}

type myDecisionService struct {
	c *client
}

// ListMyDecisionsOptions are the query parameters of a profile decisions
// request. Empty fields are left out; Limit is sent when > 0.
type ListMyDecisionsOptions struct {
	Tab          string // pending (default) or made
	Sort         string // project or recent (R6 defaults: project on pending, recent on made)
	Cursor       string
	Limit        int
	ProjectID    string // project UUID narrow filter
	Query        string // q: substring search, 2 to 256 runes
	Conversation string // a conversation chat key
	AgentID      string // asking agent UUID
	// Made only.
	UserID     string // answered_by user ID
	Provenance string // direct, interpreted, confirmed or overridden
	From       string // YYYY-MM-DD or RFC3339, answer time
	To         string // YYYY-MM-DD or RFC3339, answer time
}

// ProjectDecisionMadeGroup is one project section on tab=made&sort=project
// (up to 5 newest entries per project, with More when others exist).
type ProjectDecisionMadeGroup struct {
	Project ProjectDecisionProjectRef `json:"project"`
	Entries []ProjectDecisionLogEntry `json:"entries"`
	More    bool                      `json:"more"`
}

// MyDecisionsResponse is the response of GET /api/v1/users/me/decisions:
// Prompts on tab=pending, Entries on tab=made&sort=recent, and Groups on
// tab=made&sort=project.
type MyDecisionsResponse struct {
	Counts            ProjectDecisionCounts         `json:"counts"`
	FilteredCounts    *ProjectDecisionCounts        `json:"filteredCounts,omitempty"`
	Prompts           []ProjectDecisionPrompt       `json:"prompts,omitempty"`
	Entries           []ProjectDecisionLogEntry     `json:"entries,omitempty"`
	Groups            []ProjectDecisionMadeGroup    `json:"groups,omitempty"`
	Agents            []ProjectDecisionActor        `json:"agents"`
	Answerers         []ProjectDecisionActor        `json:"answerers"`
	Conversations     []ProjectDecisionConversation `json:"conversations"`
	Projects          []ProjectDecisionProjectRef   `json:"projects"`
	TruncatedProjects bool                          `json:"truncatedProjects"`
	NextCursor        string                        `json:"nextCursor,omitempty"`
}

// SupportsSearch reports whether the hub that answered supports decision
// search and the conversation filter.
func (r *MyDecisionsResponse) SupportsSearch() bool {
	return r != nil && r.Conversations != nil
}

func (s *myDecisionService) List(ctx context.Context, opts *ListMyDecisionsOptions) (*MyDecisionsResponse, error) {
	query := url.Values{}
	if opts != nil {
		set := func(k, v string) {
			if v != "" {
				query.Set(k, v)
			}
		}
		set("tab", opts.Tab)
		set("sort", opts.Sort)
		set("cursor", opts.Cursor)
		if opts.Limit > 0 {
			query.Set("limit", strconv.Itoa(opts.Limit))
		}
		set("project", opts.ProjectID)
		set("q", opts.Query)
		set("conversation", opts.Conversation)
		set("agent", opts.AgentID)
		set("user", opts.UserID)
		set("provenance", opts.Provenance)
		set("from", opts.From)
		set("to", opts.To)
	}
	resp, err := s.c.getWithQuery(ctx, "/api/v1/users/me/decisions", query, nil)
	if err != nil {
		return nil, err
	}
	return apiclient.DecodeRequired[MyDecisionsResponse](resp)
}
