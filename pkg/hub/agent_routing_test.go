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
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// mockAgentLister implements agentLister with canned pagination for tests.
type mockAgentLister struct {
	agents []store.Agent
	// pageSize controls pagination; 0 means return all in one page.
	pageSize int
	// listErr, if non-nil, is returned by ListAgents.
	listErr error
}

func (m *mockAgentLister) ListAgents(_ context.Context, filter store.AgentFilter, opts store.ListOptions) (*store.ListResult[store.Agent], error) {
	if m.listErr != nil {
		return nil, m.listErr
	}
	agents := m.agents

	// Simple cursor pagination: cursor is the string index to start from.
	start := 0
	if opts.Cursor != "" {
		for i, a := range agents {
			if a.ID == opts.Cursor {
				start = i
				break
			}
		}
	}

	limit := len(agents)
	if m.pageSize > 0 && m.pageSize < limit-start {
		limit = start + m.pageSize
	} else {
		limit = len(agents)
	}
	if start > len(agents) {
		start = len(agents)
	}
	if limit > len(agents) {
		limit = len(agents)
	}

	items := agents[start:limit]
	next := ""
	if limit < len(agents) {
		next = agents[limit].ID
	}
	return &store.ListResult[store.Agent]{Items: items, NextCursor: next}, nil
}

func makeAgent(id, slug string) store.Agent {
	return store.Agent{ID: id, Slug: slug, Name: slug, ProjectID: "proj-1"}
}

func agentPtr(a store.Agent) *store.Agent {
	return &a
}

func slugs(agents []*store.Agent) []string {
	s := make([]string, len(agents))
	for i, a := range agents {
		s[i] = a.Slug
	}
	return s
}

func TestResolveRoutingAgents(t *testing.T) {
	alpha := makeAgent("id-alpha", "alpha")
	foo := makeAgent("id-foo", "foo")
	bar := makeAgent("id-bar", "bar")
	baz := makeAgent("id-baz", "baz")

	allAgents := []store.Agent{alpha, foo, bar, baz}

	tests := []struct {
		name             string
		content          string
		defaultAgent     *store.Agent
		agents           []store.Agent
		wantSlugs        []string
		wantUnresolved   []string
		wantEmpty        bool
		wantMentionNames []string
		pageSize         int
	}{
		{
			name:         "no mentions, default present",
			content:      "hello",
			defaultAgent: agentPtr(alpha),
			agents:       allAgents,
			wantSlugs:    []string{"alpha"},
		},
		{
			name:         "no mentions, no default",
			content:      "hello",
			defaultAgent: nil,
			agents:       allAgents,
			wantEmpty:    true,
		},
		{
			name:         "additive mention: hello @foo",
			content:      "hello @foo",
			defaultAgent: agentPtr(alpha),
			agents:       allAgents,
			wantSlugs:    []string{"alpha", "foo"},
		},
		{
			name:         "leading mention override: @foo hello",
			content:      "@foo hello",
			defaultAgent: agentPtr(alpha),
			agents:       allAgents,
			wantSlugs:    []string{"foo"},
		},
		{
			name:         "two leading mentions: @foo @bar hello",
			content:      "@foo @bar hello",
			defaultAgent: agentPtr(alpha),
			agents:       allAgents,
			wantSlugs:    []string{"foo", "bar"},
		},
		{
			name:         "default also mentioned: hello @alpha @foo",
			content:      "hello @alpha @foo",
			defaultAgent: agentPtr(alpha),
			agents:       allAgents,
			wantSlugs:    []string{"alpha", "foo"},
		},
		{
			name:             "unresolved leading: @unknown hello @foo",
			content:          "@unknown hello @foo",
			defaultAgent:     agentPtr(alpha),
			agents:           allAgents,
			wantSlugs:        []string{"alpha", "foo"},
			wantUnresolved:   []string{"unknown"},
			wantMentionNames: []string{"unknown", "foo"},
		},
		{
			name:         "case-varied mentions: @FOO @Foo hello",
			content:      "@FOO @Foo hello",
			defaultAgent: agentPtr(alpha),
			agents:       allAgents,
			// ExtractMentions deduplicates case-insensitively
			wantSlugs: []string{"foo"},
		},
		{
			name:         "duplicate mentions: @foo @bar @foo hello",
			content:      "@foo @bar @foo hello",
			defaultAgent: agentPtr(alpha),
			agents:       allAgents,
			wantSlugs:    []string{"foo", "bar"},
		},
		{
			name:             "all unresolved mentions",
			content:          "hello @nobody @noone",
			defaultAgent:     agentPtr(alpha),
			agents:           allAgents,
			wantSlugs:        []string{"alpha"},
			wantUnresolved:   []string{"nobody", "noone"},
			wantMentionNames: []string{"nobody", "noone"},
		},
		{
			name:         "leading mention is default: @alpha hello @foo",
			content:      "@alpha hello @foo",
			defaultAgent: agentPtr(alpha),
			agents:       allAgents,
			// Leading @alpha overrides → alpha is primary from mention, foo additive
			wantSlugs: []string{"alpha", "foo"},
		},
		{
			name:         "no default, single mention",
			content:      "@foo hello",
			defaultAgent: nil,
			agents:       allAgents,
			wantSlugs:    []string{"foo"},
		},
		{
			name:         "no default, multiple mentions, first is primary",
			content:      "hello @foo @bar",
			defaultAgent: nil,
			agents:       allAgents,
			// No default, non-leading: first resolved mention is primary.
			wantSlugs: []string{"foo", "bar"},
		},
		{
			name:         "leading whitespace before mention",
			content:      "  @foo hello",
			defaultAgent: agentPtr(alpha),
			agents:       allAgents,
			wantSlugs:    []string{"foo"},
		},
		{
			name:         "mention with trailing punctuation: @foo! hello",
			content:      "@foo! hello",
			defaultAgent: agentPtr(alpha),
			agents:       allAgents,
			// ExtractMentions strips trailing punctuation
			wantSlugs: []string{"foo"},
		},
		{
			name:         "pagination: agents beyond first page",
			content:      "hello @baz",
			defaultAgent: agentPtr(alpha),
			agents:       allAgents,
			pageSize:     2, // alpha, foo on page 1; bar, baz on page 2
			wantSlugs:    []string{"alpha", "baz"},
		},
		{
			name:         "empty content",
			content:      "",
			defaultAgent: agentPtr(alpha),
			agents:       allAgents,
			wantSlugs:    []string{"alpha"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			lister := &mockAgentLister{agents: tt.agents, pageSize: tt.pageSize}
			plan, err := resolveRoutingAgents(context.Background(), lister, "proj-1", tt.content, tt.defaultAgent)
			require.NoError(t, err)

			if tt.wantEmpty {
				assert.Empty(t, plan.Agents, "expected no agents")
				return
			}

			require.NotEmpty(t, plan.Agents, "expected agents")
			assert.Equal(t, tt.wantSlugs, slugs(plan.Agents), "agent slugs")

			if tt.wantUnresolved != nil {
				assert.Equal(t, tt.wantUnresolved, plan.UnresolvedMentions, "unresolved mentions")
			}

			if tt.wantMentionNames != nil {
				assert.Equal(t, tt.wantMentionNames, plan.MentionNames, "mention names")
			}
		})
	}
}

func TestResolveRoutingAgents_DeletedAgentsExcluded(t *testing.T) {
	alpha := makeAgent("id-alpha", "alpha")
	deletedFoo := makeAgent("id-foo", "foo")
	deletedFoo.DeletedAt = deletedFoo.Created.Add(1) // non-zero = deleted

	lister := &mockAgentLister{agents: []store.Agent{alpha, deletedFoo}}
	plan, err := resolveRoutingAgents(context.Background(), lister, "proj-1", "@foo hello", agentPtr(alpha))
	require.NoError(t, err)

	// foo is deleted, so it should not resolve
	assert.Equal(t, []string{"alpha"}, slugs(plan.Agents))
	assert.Contains(t, plan.UnresolvedMentions, "foo")
}

func TestResolveRoutingAgents_StoreError(t *testing.T) {
	lister := &mockAgentLister{listErr: fmt.Errorf("db connection lost")}
	_, err := resolveRoutingAgents(context.Background(), lister, "proj-1", "@foo hello", agentPtr(makeAgent("id-alpha", "alpha")))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "db connection lost")
}

func TestResolveRoutingAgents_MentionCapOrdering(t *testing.T) {
	// Create 12 agents — 10 cap + default + one over cap
	var agents []store.Agent
	for i := 0; i < 12; i++ {
		slug := fmt.Sprintf("agent-%02d", i)
		agents = append(agents, makeAgent(fmt.Sprintf("id-%02d", i), slug))
	}
	defaultAgent := agentPtr(agents[0])

	// Build content with 11 mentions (beyond the 10-cap)
	var mentions []string
	for i := 1; i < 12; i++ {
		mentions = append(mentions, fmt.Sprintf("@agent-%02d", i))
	}
	content := "hello " + joinStrings(mentions)

	lister := &mockAgentLister{agents: agents}
	plan, err := resolveRoutingAgents(context.Background(), lister, "proj-1", content, defaultAgent)
	require.NoError(t, err)

	// Default + 10 mention cap = 11 total agents
	assert.LessOrEqual(t, len(plan.Agents), 11, "should respect mention cap")
	assert.Equal(t, "agent-00", plan.Agents[0].Slug, "default should be primary")
}

func TestDeduplicateAgentsByID(t *testing.T) {
	a := &store.Agent{ID: "1", Slug: "alpha"}
	b := &store.Agent{ID: "2", Slug: "beta"}

	result := deduplicateAgentsByID([]*store.Agent{a, b, a, b, a})
	assert.Len(t, result, 2)
	assert.Equal(t, "alpha", result[0].Slug)
	assert.Equal(t, "beta", result[1].Slug)
}

func joinStrings(ss []string) string {
	result := ""
	for i, s := range ss {
		if i > 0 {
			result += " "
		}
		result += s
	}
	return result
}
