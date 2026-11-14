//go:build !no_sqlite

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
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/messages"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// decisionsSearchFixture is the choices fixture plus one DM-only agent and
// one open DM prompt to Bob, for the Decisions search and filter tests
// (ptone/scion#7992). The fixture topic is "#sched-topic", asked by
// "sched-agent"; Alice owns the project and Bob is a member.
type decisionsSearchFixture struct {
	*choicesFixture
	dmAgent  *store.Agent
	dmKey    string
	dmMsg    *store.Message
	dmRowID  string
	basePath string
}

func newDecisionsSearchFixture(t *testing.T) *decisionsSearchFixture {
	t.Helper()
	f := newChoicesFixture(t)
	setProjectDecisionsExperiments(t, f.srv, true, true)
	ctx := context.Background()

	dmAgent := &store.Agent{
		ID: api.NewUUID(), Slug: "nc-x", Name: "nc-x",
		ProjectID: f.project.ID, OwnerID: f.bob.ID, CreatedBy: f.bob.ID, Ancestry: []string{f.bob.ID},
		Phase: string(state.PhaseRunning), MessageMode: store.MessageModeProject,
	}
	require.NoError(t, f.store.CreateAgent(ctx, dmAgent))
	dmKey, err := messages.DMConversationKey("agent", dmAgent.ID, "user", f.bob.ID)
	require.NoError(t, err)
	decs, err := messages.NormalizeDecisions("choices", decisionInputs(t, `[{"id":"rotate-key","question":"Rotate your staging key now?","choices":["rotate","later"]}]`))
	require.NoError(t, err)
	dmMsg := &store.Message{
		ID: api.NewUUID(), ProjectID: f.project.ID, Sender: "agent:" + dmAgent.Slug, SenderID: dmAgent.ID,
		Recipient: "user:" + f.bob.ID, ThreadID: dmKey, Channel: "web", CreatedAt: time.Now().UTC().Add(-time.Hour),
	}
	res, err := f.store.CreateChoicePrompt(ctx, &store.ChoicePromptInput{
		Message: dmMsg, Intro: "DM", AskerAgentID: dmAgent.ID, AskerAgentName: dmAgent.Name, Decisions: decs,
	})
	require.NoError(t, err)
	return &decisionsSearchFixture{
		choicesFixture: f,
		dmAgent:        dmAgent,
		dmKey:          dmKey,
		dmMsg:          dmMsg,
		dmRowID:        res.Decisions[0].RowID,
		basePath:       "/api/v1/projects/" + f.project.ID + "/decisions",
	}
}

func (f *decisionsSearchFixture) get(t *testing.T, u *store.User, params url.Values) (int, []byte) {
	t.Helper()
	path := f.basePath
	if len(params) > 0 {
		path += "?" + params.Encode()
	}
	rr := doRequestAsUser(t, f.srv, u, http.MethodGet, path, nil)
	return rr.Code, rr.Body.Bytes()
}

func (f *decisionsSearchFixture) pending(t *testing.T, u *store.User, params url.Values) ProjectPendingDecisionsResponse {
	t.Helper()
	if params == nil {
		params = url.Values{}
	}
	params.Set("tab", ProjectDecisionsTabPending)
	code, body := f.get(t, u, params)
	require.Equal(t, http.StatusOK, code, "body: %s", body)
	var out ProjectPendingDecisionsResponse
	require.NoError(t, json.Unmarshal(body, &out))
	return out
}

func (f *decisionsSearchFixture) made(t *testing.T, u *store.User, params url.Values) ProjectMadeDecisionsResponse {
	t.Helper()
	if params == nil {
		params = url.Values{}
	}
	params.Set("tab", ProjectDecisionsTabMade)
	code, body := f.get(t, u, params)
	require.Equal(t, http.StatusOK, code, "body: %s", body)
	var out ProjectMadeDecisionsResponse
	require.NoError(t, json.Unmarshal(body, &out))
	return out
}

func promptKeys(prompts []ProjectDecisionPromptView) []string {
	var out []string
	for _, p := range prompts {
		for _, d := range p.Decisions {
			out = append(out, d.Key)
		}
	}
	return out
}

func entryKeys(entries []ProjectDecisionLogEntryView) []string {
	var out []string
	for _, e := range entries {
		out = append(out, e.Decision.Key)
	}
	return out
}

func TestProjectDecisions_SearchAndFilterValidation(t *testing.T) {
	f := newDecisionsSearchFixture(t)
	for name, params := range map[string]url.Values{
		"one rune":                {"q": {"a"}},
		"one rune after trim":     {"q": {"  a  "}},
		"257 runes":               {"q": {strings.Repeat("é", 257)}},
		"257 runes on made":       {"tab": {"made"}, "q": {strings.Repeat("x", 257)}},
		"agent not a uuid":        {"agent": {"release-bot"}},
		"conversation too long":   {"conversation": {strings.Repeat("k", 257)}},
		"made conversation width": {"tab": {"made"}, "conversation": {strings.Repeat("k", 257)}},
	} {
		code, body := f.get(t, f.bob, params)
		assert.Equal(t, http.StatusBadRequest, code, "%s: %s", name, body)
	}
	for name, params := range map[string]url.Values{
		"two runes":        {"q": {"ro"}},
		"256 multibyte":    {"q": {strings.Repeat("é", 256)}},
		"blank is no q":    {"q": {"   "}},
		"conversation 256": {"conversation": {strings.Repeat("k", 256)}},
	} {
		code, body := f.get(t, f.bob, params)
		assert.Equal(t, http.StatusOK, code, "%s: %s", name, body)
	}
}

func TestProjectDecisions_ConversationFilterAndFacetPrivacy(t *testing.T) {
	f := newDecisionsSearchFixture(t)
	f.topicPrompt(t, `[{"id":"ship-now","question":"Deploy after tagging?","choices":["yes","no"]}]`, false)

	// Bob reads the topic and his DM; the facet is sorted by label.
	bob := f.pending(t, f.bob, nil)
	assert.Equal(t, ProjectDecisionCounts{WaitingOnYou: 2}, bob.Counts)
	assert.Nil(t, bob.FilteredCounts, "no filter, no filteredCounts")
	assert.Equal(t, []ProjectDecisionConversationView{
		{Key: f.topicID, Label: "#sched-topic", ProjectID: f.project.ID},
		{Key: f.dmKey, Label: "DM with nc-x", ProjectID: f.project.ID},
	}, bob.Conversations)
	require.Len(t, bob.Prompts, 2)
	for _, p := range bob.Prompts {
		assert.Equal(t, ProjectDecisionProjectRef{ID: f.project.ID, Name: f.project.Name, Slug: f.project.Slug}, p.Project, "S2 project on every prompt")
	}

	// Bob narrowed to his DM.
	bobDM := f.pending(t, f.bob, url.Values{"conversation": {f.dmKey}})
	assert.Equal(t, []string{"rotate-key"}, promptKeys(bobDM.Prompts))
	assert.Equal(t, ProjectDecisionCounts{WaitingOnYou: 2}, bobDM.Counts, "counts stay unfiltered")
	require.NotNil(t, bobDM.FilteredCounts)
	assert.Equal(t, ProjectDecisionCounts{WaitingOnYou: 1}, *bobDM.FilteredCounts)

	// Alice (project owner) never sees Bob's DM: not in the facet, and its
	// key gives an empty 200 with unchanged counts on both tabs.
	alice := f.pending(t, f.alice, nil)
	assert.Equal(t, []ProjectDecisionConversationView{{Key: f.topicID, Label: "#sched-topic", ProjectID: f.project.ID}}, alice.Conversations)
	aliceDM := f.pending(t, f.alice, url.Values{"conversation": {f.dmKey}})
	assert.Empty(t, aliceDM.Prompts)
	assert.Equal(t, alice.Counts, aliceDM.Counts)
	require.NotNil(t, aliceDM.FilteredCounts)
	assert.Equal(t, ProjectDecisionCounts{}, *aliceDM.FilteredCounts)
	unknown := f.pending(t, f.alice, url.Values{"conversation": {"no-such-key"}})
	assert.Empty(t, unknown.Prompts)

	// Bob answers the DM; Alice's Made with his DM key is still empty.
	require.Equal(t, http.StatusOK, f.answer(t, f.bob, f.dmRowID, map[string]interface{}{"choice_id": "1"}).Code)
	assert.Equal(t, []string{"rotate-key"}, entryKeys(f.made(t, f.bob, url.Values{"conversation": {f.dmKey}}).Entries))
	aliceMade := f.made(t, f.alice, url.Values{"conversation": {f.dmKey}})
	assert.Empty(t, aliceMade.Entries)
	assert.Equal(t, alice.Counts, aliceMade.Counts)
	assert.Empty(t, f.made(t, f.bob, url.Values{"conversation": {f.topicID}}).Entries)
}

func TestProjectDecisions_SearchKeepsDMPrivacy(t *testing.T) {
	f := newDecisionsSearchFixture(t)
	f.topicPrompt(t, `[{"id":"ship-now","question":"Deploy after tagging?","choices":["yes","no"]}]`, false)

	// q equal to Bob's DM label matches Bob's DM for Bob only.
	q := url.Values{"q": {"DM with nc-x"}}
	assert.Equal(t, []string{"rotate-key"}, promptKeys(f.pending(t, f.bob, q).Prompts))
	alice := f.pending(t, f.alice, q)
	assert.Empty(t, alice.Prompts)
	require.NotNil(t, alice.FilteredCounts)
	assert.Equal(t, ProjectDecisionCounts{}, *alice.FilteredCounts)

	require.Equal(t, http.StatusOK, f.answer(t, f.bob, f.dmRowID, map[string]interface{}{"choice_id": "1"}).Code)
	assert.Equal(t, []string{"rotate-key"}, entryKeys(f.made(t, f.bob, q).Entries))
	assert.Empty(t, f.made(t, f.alice, q).Entries)
	// The DM agent's name and the question are DM content too.
	assert.Empty(t, f.made(t, f.alice, url.Values{"q": {"nc-x"}}).Entries)
	assert.Empty(t, f.made(t, f.alice, url.Values{"q": {"staging key"}}).Entries)
}

func TestProjectDecisions_PendingFiltersInMemory(t *testing.T) {
	f := newDecisionsSearchFixture(t)
	f.topicPrompt(t, `[{"id":"ship-now","question":"Deploy after tagging v0.9?","choices":["yes","no"]}]`, false)
	f.topicPrompt(t, `[{"id":"migrate","question":"Run the BACKFILL first?","choices":["yes","no"]}]`, false)

	cases := map[string]struct {
		params url.Values
		want   []string
	}{
		"question, case-insensitive": {url.Values{"q": {"backfill"}}, []string{"migrate"}},
		"key":                        {url.Values{"q": {"SHIP-n"}}, []string{"ship-now"}},
		"asker agent name":           {url.Values{"q": {"sched-agent"}}, []string{"migrate", "ship-now"}},
		"conversation label":         {url.Values{"q": {"#sched"}}, []string{"migrate", "ship-now"}},
		"literal percent":            {url.Values{"q": {"v0%"}}, nil},
		"agent":                      {url.Values{"agent": {f.dmAgent.ID}}, []string{"rotate-key"}},
		"agent and q":                {url.Values{"agent": {f.agent.ID}, "q": {"deploy"}}, []string{"ship-now"}},
		"conversation and q":         {url.Values{"conversation": {f.topicID}, "q": {"rotate"}}, nil},
	}
	for name, tc := range cases {
		got := f.pending(t, f.bob, tc.params)
		assert.ElementsMatch(t, tc.want, promptKeys(got.Prompts), name)
		assert.Equal(t, ProjectDecisionCounts{WaitingOnYou: 3}, got.Counts, "%s: counts stay unfiltered", name)
		require.NotNil(t, got.FilteredCounts, name)
		assert.Equal(t, ProjectDecisionCounts{WaitingOnYou: len(tc.want)}, *got.FilteredCounts, name)
	}

	// Paging keeps the filter: two matches, one per page.
	page1 := f.pending(t, f.bob, url.Values{"q": {"sched-agent"}, "limit": {"1"}})
	require.Len(t, page1.Prompts, 1)
	require.NotEmpty(t, page1.NextCursor)
	page2 := f.pending(t, f.bob, url.Values{"q": {"sched-agent"}, "limit": {"1"}, "cursor": {page1.NextCursor}})
	require.Len(t, page2.Prompts, 1)
	assert.Empty(t, page2.NextCursor)
	assert.ElementsMatch(t, []string{"ship-now", "migrate"}, append(promptKeys(page1.Prompts), promptKeys(page2.Prompts)...))
}

func TestProjectDecisions_MadeSearchAndConversation(t *testing.T) {
	f := newDecisionsSearchFixture(t)
	_, ship := f.topicPrompt(t, `[{"id":"ship-now","question":"Deploy after tagging?","choices":["yes","no"]}]`, false)
	_, window := f.topicPrompt(t, `[{"id":"window","question":"Which window?","choices":[{"id":"a","label":"Overnight"},{"id":"b","label":"Lunch"}]}]`, false)
	require.Equal(t, http.StatusOK, f.answer(t, f.bob, ship.Decisions[0].RowID, map[string]interface{}{"choice_id": "1"}).Code)
	require.Equal(t, http.StatusOK, f.answer(t, f.alice, window.Decisions[0].RowID, map[string]interface{}{"choice_id": "a"}).Code)
	require.Equal(t, http.StatusOK, f.answer(t, f.bob, f.dmRowID, map[string]interface{}{"choice_id": "1"}).Code)

	for name, tc := range map[string]struct {
		params url.Values
		want   []string
	}{
		"choice label":              {url.Values{"q": {"overnight"}}, []string{"window"}},
		"conversation label":        {url.Values{"q": {"sched-topic"}}, []string{"window", "ship-now"}},
		"dm label":                  {url.Values{"q": {"dm with"}}, []string{"rotate-key"}},
		"q and conversation":        {url.Values{"q": {"rotate"}, "conversation": {f.topicID}}, nil},
		"q and user":                {url.Values{"q": {"sched-topic"}, "user": {f.alice.ID}}, []string{"window"}},
		"conversation and agent":    {url.Values{"conversation": {f.topicID}, "agent": {f.dmAgent.ID}}, nil},
		"agent on made":             {url.Values{"agent": {f.dmAgent.ID}}, []string{"rotate-key"}},
		"q and provenance mismatch": {url.Values{"q": {"overnight"}, "provenance": {"interpreted"}}, nil},
	} {
		got := f.made(t, f.bob, tc.params)
		assert.ElementsMatch(t, tc.want, entryKeys(got.Entries), name)
	}

	made := f.made(t, f.bob, nil)
	require.Len(t, made.Entries, 3)
	for _, e := range made.Entries {
		assert.Equal(t, f.project.ID, e.Project.ID, "S2 project on every entry")
		assert.Equal(t, f.project.Slug, e.Project.Slug)
	}
}

func TestProjectDecisions_SearchTimeoutMapsToChatSearchError(t *testing.T) {
	f := newDecisionsSearchFixture(t)
	f.srv.config.ChatSearchTimeout = time.Nanosecond
	for _, tab := range []string{ProjectDecisionsTabPending, ProjectDecisionsTabMade} {
		code, body := f.get(t, f.bob, url.Values{"tab": {tab}, "q": {"rotate"}})
		assert.Equal(t, http.StatusServiceUnavailable, code, "%s: %s", tab, body)
		assert.Contains(t, string(body), "SEARCH_TIMEOUT", tab)
	}
	// Without q there is no time limit.
	code, body := f.get(t, f.bob, url.Values{"tab": {ProjectDecisionsTabPending}})
	assert.Equal(t, http.StatusOK, code, "%s", body)
}

// TestDecisionsCore_MultiScope runs the core over two projects' scopes, as
// the profile endpoint will: prompts, entries and facets from both, each
// tagged with its project, and nothing from a key outside the scopes.
func TestDecisionsCore_MultiScope(t *testing.T) {
	f := newDecisionsSearchFixture(t)
	ctx := context.Background()
	other := &store.Project{ID: api.NewUUID(), Name: "Other Project", Slug: "other-project", Created: time.Now(), Updated: time.Now()}
	require.NoError(t, f.store.CreateProject(ctx, other))
	otherBot := api.NewUUID()
	otherTopic := api.NewUUID()
	strayTopic := api.NewUUID()
	seed := func(projectID, chatKey, key string) string {
		decs, err := messages.NormalizeDecisions("choices", decisionInputs(t, `[{"id":"`+key+`","question":"`+key+`?","choices":["yes","no"]}]`))
		require.NoError(t, err)
		res, err := f.store.CreateChoicePrompt(ctx, &store.ChoicePromptInput{
			Message: &store.Message{
				ID: api.NewUUID(), ProjectID: projectID, Sender: "agent:other-bot", SenderID: otherBot,
				Recipient: "chat", ThreadID: chatKey, Channel: "web", CreatedAt: time.Now().UTC(),
			},
			AskerAgentID: otherBot, AskerAgentName: "other-bot", Decisions: decs,
		})
		require.NoError(t, err)
		return res.Decisions[0].RowID
	}
	seed(other.ID, otherTopic, "other-open")
	seed(other.ID, strayTopic, "stray-open")
	f.topicPrompt(t, `[{"id":"home-open","question":"Home?","choices":["yes","no"]}]`, false)

	bob := NewAuthenticatedUser(f.bob.ID, f.bob.Email, f.bob.DisplayName, f.bob.Role, string(ClientTypeWeb))
	home, err := f.srv.resolveProjectReadableScope(ctx, f.project, bob, nil, f.wcs)
	require.NoError(t, err)
	otherScope := &projectReadableScope{
		project:               other,
		chatKeys:              []string{otherTopic},
		conversationLabel:     map[string]string{otherTopic: "#other-topic"},
		conversationAvailable: map[string]bool{otherTopic: true},
		userCanPost:           map[string]bool{otherTopic: true},
		agentsByID:            map[string]*store.Agent{},
		queriedAgentIDs:       map[string]bool{otherBot: true},
		liveAgents:            map[string]*store.Agent{},
		agentNames:            map[string]string{otherBot: "other-bot"},
	}

	raw, err := f.srv.decisionsCore(ctx, bob, nil, f.wcs, []*projectReadableScope{home, otherScope},
		projectDecisionsQuery{tab: ProjectDecisionsTabPending, limit: 50})
	require.NoError(t, err)
	resp := raw.(ProjectPendingDecisionsResponse)
	byKey := map[string]string{}
	for _, p := range resp.Prompts {
		byKey[p.Decisions[0].Key] = p.Project.Slug
	}
	assert.Equal(t, map[string]string{"home-open": f.project.Slug, "other-open": "other-project", "rotate-key": f.project.Slug}, byKey)
	assert.Equal(t, ProjectDecisionCounts{WaitingOnYou: 3}, resp.Counts)
	var convProjects []string
	for _, c := range resp.Conversations {
		convProjects = append(convProjects, c.Label+"@"+c.ProjectID)
	}
	assert.Equal(t, []string{"#other-topic@" + other.ID, "#sched-topic@" + f.project.ID, "DM with nc-x@" + f.project.ID}, convProjects)

	// A search over both scopes matches the other project's label.
	raw, err = f.srv.decisionsCore(ctx, bob, nil, f.wcs, []*projectReadableScope{home, otherScope},
		projectDecisionsQuery{tab: ProjectDecisionsTabPending, limit: 50, q: "other-topic"})
	require.NoError(t, err)
	assert.Equal(t, []string{"other-open"}, promptKeys(raw.(ProjectPendingDecisionsResponse).Prompts))
}

func TestDecisionAgentFacetViews_LabelsAndHints(t *testing.T) {
	liveA := &store.Agent{ID: "aaaa1111-0000-0000-0000-000000000000", Name: "release-bot"}
	liveB := &store.Agent{ID: "bbbb2222-0000-0000-0000-000000000000", Name: "Release-Bot"}
	renamed := &store.Agent{ID: "dddd4444-0000-0000-0000-000000000000", Name: "builder"}
	set := &decisionScopeSet{projectReadableScope: &projectReadableScope{
		liveAgents: map[string]*store.Agent{liveA.ID: liveA, liveB.ID: liveB, renamed.ID: renamed},
		agentNames: map[string]string{},
	}}
	deletedID := "3f9c0000-0000-0000-0000-000000000000"
	got := decisionAgentFacetViews(set, []store.ProjectDecisionActor{
		{ID: liveA.ID, Name: "release-bot"},
		{ID: deletedID, Name: "release-bot"},
		{ID: renamed.ID, Name: "old-builder-name"},
		{ID: "eeee5555-0000-0000-0000-000000000000", Name: "solo-deleted"},
	})
	assert.Equal(t, []ProjectDecisionActorView{
		{ID: renamed.ID, Name: "builder"},
		{ID: liveA.ID, Name: "release-bot"},
		{ID: deletedID, Name: "release-bot", Hint: "deleted · 3f9c"},
		{ID: "eeee5555-0000-0000-0000-000000000000", Name: "solo-deleted"},
	}, got, "live name wins; only a colliding deleted agent gets a hint")

	// Two live agents whose names fold to the same label both get the short id.
	got = decisionAgentFacetViews(set, []store.ProjectDecisionActor{
		{ID: liveA.ID, Name: "x"},
		{ID: liveB.ID, Name: "y"},
	})
	assert.Equal(t, []ProjectDecisionActorView{
		{ID: liveA.ID, Name: "release-bot", Hint: "aaaa"},
		{ID: liveB.ID, Name: "Release-Bot", Hint: "bbbb"},
	}, got)
}
