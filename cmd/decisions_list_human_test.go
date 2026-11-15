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

package cmd

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/clitime"
	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	humanDecisionsTopic = "7d0f1c2e-1111-4a5b-9c8d-000000000001"
	humanDecisionsDM    = "dm:agent:9a9a9a9a-0000-4000-8000-000000000002:user:u-alice"
	humanDecisionsBot   = "aaaa1111-0000-4000-8000-000000000003"
)

// humanDecisionsPendingBody is a tab=pending response: one prompt waiting
// on the viewer (a 2-row set, one row already answered) and one waiting
// on others.
const humanDecisionsPendingBody = `{
  "counts": {"waitingOnYou": 2, "waitingOnOthers": 1},
  "prompts": [
    {"project": {"id": "proj-1", "name": "Scion", "slug": "scion"}, "promptMessageId": "m1", "promptAvailable": true,
     "chatKey": "` + humanDecisionsTopic + `", "conversationLabel": "#release", "conversationAvailable": true,
     "askerAgentId": "` + humanDecisionsBot + `", "askerAgentName": "release-bot", "createdAt": "2026-11-13T04:48:00Z",
     "answerMode": "set", "waitingOnYou": true, "canAnswer": true,
     "decisions": [
       {"rowId": "r1", "messageId": "m1", "key": "ship-now", "ref": "release-bot/ship-now", "version": 1, "status": "pending",
        "askerAgentId": "` + humanDecisionsBot + `", "askerAgentName": "release-bot", "question": "Deploy to prod after tagging v0.9?", "choices": []},
       {"rowId": "r2", "messageId": "m1", "key": "notes", "ref": "release-bot/notes", "version": 1, "status": "decided",
        "askerAgentId": "` + humanDecisionsBot + `", "askerAgentName": "release-bot", "question": "Publish notes?", "choices": [],
        "decision": {"choiceId": "1", "label": "Yes", "provenance": "direct", "surface": "native", "responseMessageId": "x", "at": "2026-11-13T04:50:00Z"}}
     ]},
    {"project": {"id": "proj-1", "name": "Scion", "slug": "scion"}, "promptMessageId": "m2", "promptAvailable": true,
     "chatKey": "` + humanDecisionsTopic + `", "conversationLabel": "#release", "conversationAvailable": true,
     "askerAgentId": "` + humanDecisionsBot + `", "askerAgentName": "release-bot", "createdAt": "2026-11-13T03:00:00Z",
     "answerMode": "single", "waitingOnYou": false, "canAnswer": false, "disabledReason": "Read-only in this conversation",
     "waitingReasons": {"r3": "read_only"},
     "decisions": [
       {"rowId": "r3", "messageId": "m2", "key": "other", "ref": "release-bot/other", "version": 1, "status": "pending",
        "askerAgentId": "` + humanDecisionsBot + `", "askerAgentName": "release-bot", "question": "Someone else?", "choices": []}
     ]}
  ],
  "agents": [{"id": "` + humanDecisionsBot + `", "name": "release-bot"}],
  "answerers": [],
  "conversations": [
    {"key": "` + humanDecisionsTopic + `", "label": "#release", "projectId": "proj-1"},
    {"key": "` + humanDecisionsDM + `", "label": "DM with nc-x", "projectId": "proj-1"}
  ]
}`

const humanDecisionsMadeBody = `{
  "counts": {"waitingOnYou": 2, "waitingOnOthers": 1},
  "entries": [
    {"project": {"id": "proj-1", "name": "Scion", "slug": "scion"}, "answerId": "a1",
     "decision": {"rowId": "r9", "promptMessageId": "m9", "promptAvailable": true, "key": "migrate", "ref": "nc-x/migrate", "version": 2,
                  "question": "Run the backfill on staging first?", "askerAgentId": "b", "askerAgentName": "nc-x"},
     "choiceId": "1", "label": "Yes", "matchedDefault": false, "answeredByUserId": "u-alice", "answeredByName": "Alice",
     "provenance": "direct", "outcome": "answer", "surface": "decisions_page", "sourceMessageAvailable": false,
     "at": "2026-11-12T10:00:00Z", "chatKey": "` + humanDecisionsTopic + `", "conversationLabel": "#release", "conversationAvailable": true},
    {"project": {"id": "proj-1", "name": "Scion", "slug": "scion"}, "answerId": "a2",
     "decision": {"rowId": "r8", "promptMessageId": "m8", "promptAvailable": true, "key": "migrate", "ref": "nc-x/migrate", "version": 1,
                  "question": "Backfill before the cutover?", "askerAgentId": "b", "askerAgentName": "nc-x"},
     "choiceId": "1", "label": "Yes", "matchedDefault": false, "answeredByUserId": "u-bob", "answeredByName": "Bob",
     "provenance": "direct", "outcome": "confirm", "surface": "native", "sourceMessageAvailable": false,
     "at": "2026-11-02T10:00:00Z", "chatKey": "` + humanDecisionsTopic + `", "conversationLabel": "#release", "conversationAvailable": true}
  ],
  "agents": [], "answerers": [], "conversations": []
}`

// humanDecisionsHub is a mock hub for the human-mode list: it records every
// project and profile decisions query and answers with the configured body.
type humanDecisionsHub struct {
	mu             sync.Mutex
	queries        []url.Values
	profileQueries []url.Values
	pending        string
	made           string
	profilePending string
	profileMade    string
	status         int
	errBody        string
	me             string
}

func newHumanDecisionsHub(t *testing.T) *humanDecisionsHub {
	t.Helper()
	h := &humanDecisionsHub{
		pending:        humanDecisionsPendingBody,
		made:           humanDecisionsMadeBody,
		profilePending: allProjectsDecisionsPendingBody,
		profileMade:    allProjectsDecisionsMadeBody,
		me:             `{"id":"u-alice","email":"alice@example.com","displayName":"Alice","role":"member"}`,
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/projects/proj-1/decisions", func(w http.ResponseWriter, r *http.Request) {
		h.mu.Lock()
		defer h.mu.Unlock()
		h.queries = append(h.queries, r.URL.Query())
		w.Header().Set("Content-Type", "application/json")
		if h.status != 0 {
			w.WriteHeader(h.status)
			_, _ = w.Write([]byte(h.errBody))
			return
		}
		if r.URL.Query().Get("tab") == "made" {
			_, _ = w.Write([]byte(h.made))
			return
		}
		_, _ = w.Write([]byte(h.pending))
	})
	mux.HandleFunc("/api/v1/users/me/decisions", func(w http.ResponseWriter, r *http.Request) {
		h.mu.Lock()
		defer h.mu.Unlock()
		h.profileQueries = append(h.profileQueries, r.URL.Query())
		w.Header().Set("Content-Type", "application/json")
		if h.status != 0 {
			w.WriteHeader(h.status)
			_, _ = w.Write([]byte(h.errBody))
			return
		}
		if r.URL.Query().Get("tab") == "made" {
			_, _ = w.Write([]byte(h.profileMade))
			return
		}
		_, _ = w.Write([]byte(h.profilePending))
	})
	mux.HandleFunc("/api/v1/auth/me", func(w http.ResponseWriter, _ *http.Request) {
		h.mu.Lock()
		defer h.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(h.me))
	})
	mux.HandleFunc("/api/v1/agent/decisions", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"items":[]}`))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	origTarget := decisionsHumanTarget
	decisionsHumanTarget = func() (hubclient.Client, string, error) {
		c, err := hubclient.New(srv.URL, hubclient.WithBearerToken("user-token"))
		return c, "proj-1", err
	}
	t.Cleanup(func() { decisionsHumanTarget = origTarget })
	origAllProjects := decisionsAllProjectsClient
	decisionsAllProjectsClient = func() (hubclient.Client, error) {
		return hubclient.New(srv.URL, hubclient.WithBearerToken("user-token"))
	}
	t.Cleanup(func() { decisionsAllProjectsClient = origAllProjects })
	origFactory := decisionsClientFactory
	decisionsClientFactory = func() (hubclient.Client, error) {
		return hubclient.New(srv.URL, hubclient.WithAgentToken("agent-token"))
	}
	t.Cleanup(func() { decisionsClientFactory = origFactory })
	return h
}

func (h *humanDecisionsHub) lastProfile() url.Values {
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.profileQueries) == 0 {
		return nil
	}
	return h.profileQueries[len(h.profileQueries)-1]
}

func (h *humanDecisionsHub) profileCount() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.profileQueries)
}

func (h *humanDecisionsHub) last() url.Values {
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.queries) == 0 {
		return nil
	}
	return h.queries[len(h.queries)-1]
}

func (h *humanDecisionsHub) count() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.queries)
}

func fixDecisionsClock(t *testing.T) {
	t.Helper()
	restore := clitime.SetNow(func() time.Time { return time.Date(2026, 11, 13, 5, 0, 0, 0, time.UTC) })
	t.Cleanup(restore)
	clitime.SetZone(time.UTC)
	t.Cleanup(func() { clitime.SetZone(nil) })
}

func TestDecisionsListHuman_ModeDispatch(t *testing.T) {
	h := newHumanDecisionsHub(t)

	t.Setenv("SCION_CLI_MODE", "human")
	_, err := runDecisionsTestCmd(t, "decisions", "list")
	require.NoError(t, err)
	require.Equal(t, 1, h.count(), "human mode calls the project endpoint")
	assert.Equal(t, url.Values{"tab": {"pending"}, "limit": {"50"}}, h.last(), "--limit defaults to 50")

	t.Setenv("SCION_CLI_MODE", "agent")
	out, err := runDecisionsTestCmd(t, "decisions", "list")
	require.NoError(t, err)
	assert.Equal(t, 1, h.count(), "agent mode keeps the agent endpoint")
	assert.Contains(t, out, "No pending decisions.")
}

func TestDecisionsList_FlagsOfTheOtherMode(t *testing.T) {
	newHumanDecisionsHub(t)

	t.Setenv("SCION_CLI_MODE", "agent")
	for _, args := range [][]string{
		{"--made"}, {"--waiting"}, {"--all-projects"}, {"--sort", "project"}, {"--conversation", "x"},
		{"--agent", "x"}, {"--answered-by", "me"}, {"--provenance", "direct"}, {"--since", "2026-11-01"},
		{"--until", "2026-11-01"}, {"--limit", "5"},
	} {
		_, err := runDecisionsTestCmd(t, append([]string{"decisions", "list"}, args...)...)
		require.Error(t, err, "%v", args)
		assert.True(t, isUsageError(err), "%v", args)
		assert.Contains(t, err.Error(), "only in human mode; this CLI is in agent mode", "%v", args)
	}

	t.Setenv("SCION_CLI_MODE", "human")
	for _, flag := range []string{"--pending", "--decided", "--all"} {
		_, err := runDecisionsTestCmd(t, "decisions", "list", flag)
		require.Error(t, err, flag)
		assert.True(t, isUsageError(err), flag)
		assert.Contains(t, err.Error(), "only in agent mode; this CLI is in human mode", flag)
	}

	// Help hides the other mode's flags.
	for mode, want := range map[CLIMode]struct{ shown, hidden []string }{
		ModeAgent: {shown: []string{"pending", "decided", "all", "search", "json"}, hidden: []string{"made", "waiting", "all-projects", "sort", "conversation", "agent", "answered-by", "provenance", "since", "until", "limit"}},
		ModeHuman: {shown: []string{"made", "waiting", "all-projects", "sort", "search", "conversation", "agent", "limit", "json"}, hidden: []string{"pending", "decided", "all"}},
	} {
		root := &cobra.Command{Use: "scion"}
		dec := newDecisionsCmd()
		root.AddCommand(dec)
		applyModeRestrictions(root, mode)
		list, _, err := root.Find([]string{"decisions", "list"})
		require.NoError(t, err)
		for _, name := range want.shown {
			assert.False(t, list.Flags().Lookup(name).Hidden, "%s mode shows --%s", mode, name)
		}
		for _, name := range want.hidden {
			assert.True(t, list.Flags().Lookup(name).Hidden, "%s mode hides --%s", mode, name)
		}
	}
}

func TestDecisionsListHuman_FlagValidation(t *testing.T) {
	h := newHumanDecisionsHub(t)
	t.Setenv("SCION_CLI_MODE", "human")
	for _, args := range [][]string{
		{"--waiting", "--made"},
		{"--limit", "0"},
		{"--limit", "201"},
		{"-q", "a"},
		{"--answered-by", "me"},
		{"--since", "2026-11-01"},
		{"--made", "--provenance", "bogus"},
		{"--made", "--since", "last week"},
	} {
		_, err := runDecisionsTestCmd(t, append([]string{"decisions", "list"}, args...)...)
		require.Error(t, err, "%v", args)
		assert.True(t, isUsageError(err), "%v: %v", args, err)
	}
	assert.Zero(t, h.count(), "invalid flags send no request")

	_, err := runDecisionsTestCmd(t, "decisions", "list", "--made", "--limit", "10", "-q", "backfill",
		"--answered-by", "me", "--provenance", "direct", "--since", "2026-11-01", "--until", "2026-11-12T23:00:00Z")
	require.NoError(t, err)
	assert.Equal(t, url.Values{
		"tab": {"made"}, "limit": {"10"}, "q": {"backfill"}, "user": {"u-alice"}, "provenance": {"direct"},
		"from": {"2026-11-01"}, "to": {"2026-11-12T23:00:00Z"},
	}, h.last())
}

func TestDecisionsListHuman_NameResolution(t *testing.T) {
	h := newHumanDecisionsHub(t)
	t.Setenv("SCION_CLI_MODE", "human")

	for value, want := range map[string]string{
		"release":            humanDecisionsTopic,
		"#Release":           humanDecisionsTopic,
		"dm with NC-X":       humanDecisionsDM,
		humanDecisionsTopic:  humanDecisionsTopic,
		humanDecisionsDM:     humanDecisionsDM,
		"dm:agent:x:user:yy": "dm:agent:x:user:yy", // not readable: passed through, the hub answers empty
	} {
		_, err := runDecisionsTestCmd(t, "decisions", "list", "--conversation", value)
		require.NoError(t, err, value)
		assert.Equal(t, want, h.last().Get("conversation"), value)
	}
	_, err := runDecisionsTestCmd(t, "decisions", "list", "--conversation", "nowhere")
	require.Error(t, err)
	assert.Contains(t, err.Error(), `no conversation with Decisions matches "nowhere"`)

	_, err = runDecisionsTestCmd(t, "decisions", "list", "--agent", "RELEASE-BOT")
	require.NoError(t, err)
	assert.Equal(t, humanDecisionsBot, h.last().Get("agent"))
	_, err = runDecisionsTestCmd(t, "decisions", "list", "--agent", "ghost")
	require.Error(t, err)

	// Two conversations and two agents with one name: ambiguous, exit 2.
	h.pending = `{"counts":{"waitingOnYou":0,"waitingOnOthers":0},"prompts":[],"answerers":[],
	  "agents":[{"id":"a-1","name":"bot"},{"id":"a-2","name":"Bot","hint":"deleted · a-2"}],
	  "conversations":[{"key":"k1","label":"#ops","projectId":"p"},{"key":"k2","label":"ops","projectId":"p"}]}`
	_, err = runDecisionsTestCmd(t, "decisions", "list", "--conversation", "ops")
	require.Error(t, err)
	assert.Equal(t, 2, exitCodeFor(err))
	assert.Contains(t, err.Error(), "matches 2 conversations")
	assert.Contains(t, err.Error(), "k1")
	assert.Contains(t, err.Error(), "k2")
	_, err = runDecisionsTestCmd(t, "decisions", "list", "--agent", "bot")
	require.Error(t, err)
	assert.Equal(t, 2, exitCodeFor(err))
	assert.Contains(t, err.Error(), "Bot (deleted · a-2)  a-2")
}

func TestDecisionsListHuman_ExperimentOffAndOldHub(t *testing.T) {
	h := newHumanDecisionsHub(t)
	t.Setenv("SCION_CLI_MODE", "human")

	h.status = http.StatusNotFound
	h.errBody = `{"error":{"code":"not_found","message":"route not found"}}`
	_, err := runDecisionsTestCmd(t, "decisions", "list")
	require.Error(t, err)
	assert.Equal(t, "Decisions are not enabled on this hub (experiment web.project_decisions)", err.Error())

	h.errBody = `{"error":{"code":"not_found","message":"Project not found"}}`
	_, err = runDecisionsTestCmd(t, "decisions", "list")
	require.Error(t, err)
	assert.Equal(t, "project not found on this hub", err.Error())

	// An older hub sends no conversations facet.
	h.status = 0
	h.pending = `{"counts":{"waitingOnYou":0,"waitingOnOthers":0},"prompts":[],"agents":[],"answerers":[]}`
	for _, args := range [][]string{{"-q", "release"}, {"--conversation", "release"}, {"--agent", "bot"}} {
		_, err = runDecisionsTestCmd(t, append([]string{"decisions", "list"}, args...)...)
		require.Error(t, err, "%v", args)
		assert.Equal(t, "this hub does not support decision search", err.Error(), "%v", args)
	}
	_, err = runDecisionsTestCmd(t, "decisions", "list")
	require.NoError(t, err, "without a filter an older hub still lists")
}

func TestDecisionsListHuman_TableAndJSONGolden(t *testing.T) {
	fixDecisionsClock(t)
	newHumanDecisionsHub(t)
	t.Setenv("SCION_CLI_MODE", "human")

	out, err := runDecisionsTestCmd(t, "decisions", "list")
	require.NoError(t, err)
	assert.Equal(t, ""+
		"KEY       VER  QUESTION                            AGENT        CONVERSATION  AGE\n"+
		"ship-now  v1   Deploy to prod after tagging v0.9?  release-bot  #release      12m ago\n"+
		"1 more waiting on others (use --json for details)\n", out)

	out, err = runDecisionsTestCmd(t, "decisions", "list", "--made")
	require.NoError(t, err)
	assert.Equal(t, ""+
		"WHEN            KEY      VER  QUESTION                            ANSWER  ANSWERED BY  PROVENANCE  AGENT  CONVERSATION\n"+
		"2026-11-12 UTC  migrate  v2   Run the backfill on staging first?  Yes     you          direct      nc-x   #release\n"+
		"2026-11-02 UTC  migrate  v1   Backfill before the cutover?        Yes     Bob          confirmed   nc-x   #release\n", out)

	for tab, body := range map[string]string{"pending": humanDecisionsPendingBody, "made": humanDecisionsMadeBody} {
		args := []string{"decisions", "list", "--json"}
		if tab == "made" {
			args = append(args, "--made")
		}
		out, err = runDecisionsTestCmd(t, args...)
		require.NoError(t, err)
		var got, want map[string]any
		require.NoError(t, json.Unmarshal([]byte(out), &got), out)
		require.NoError(t, json.Unmarshal([]byte(body), &want))
		assert.Equal(t, want, got, "--json prints the API shape (%s)", tab)
	}
}

func TestDecisionsListHuman_EmptyAndFilteredMessages(t *testing.T) {
	h := newHumanDecisionsHub(t)
	t.Setenv("SCION_CLI_MODE", "human")
	h.pending = `{"counts":{"waitingOnYou":2,"waitingOnOthers":0},"filteredCounts":{"waitingOnYou":0,"waitingOnOthers":0},
	  "prompts":[],"agents":[],"answerers":[],"conversations":[]}`
	out, err := runDecisionsTestCmd(t, "decisions", "list", "-q", "nothing")
	require.NoError(t, err)
	assert.Equal(t, "No Decisions waiting on you match.\n", out)
	h.made = `{"counts":{"waitingOnYou":0,"waitingOnOthers":0},"entries":[],"agents":[],"answerers":[],"conversations":[]}`
	out, err = runDecisionsTestCmd(t, "decisions", "list", "--made")
	require.NoError(t, err)
	assert.Equal(t, "No Decisions made.\n", out)
}

func TestDecisionsListHuman_MoreHintOnlyForWaitingOnYou(t *testing.T) {
	fixDecisionsClock(t)
	h := newHumanDecisionsHub(t)
	t.Setenv("SCION_CLI_MODE", "human")

	// Every waiting-on-you row fits; the next page holds only prompts
	// waiting on others: no --limit hint.
	h.pending = `{"counts":{"waitingOnYou":1,"waitingOnOthers":3},"nextCursor":"c2",
	  "prompts":[{"promptMessageId":"m1","chatKey":"t","conversationLabel":"#release","askerAgentName":"release-bot",
	    "createdAt":"2026-11-13T04:48:00Z","waitingOnYou":true,
	    "decisions":[{"rowId":"r1","key":"ship-now","version":1,"status":"pending","question":"Deploy?"}]}],
	  "agents":[],"answerers":[],"conversations":[]}`
	out, err := runDecisionsTestCmd(t, "decisions", "list", "--limit", "1")
	require.NoError(t, err)
	assert.Equal(t, ""+
		"KEY       VER  QUESTION  AGENT        CONVERSATION  AGE\n"+
		"ship-now  v1   Deploy?   release-bot  #release      12m ago\n"+
		"3 more waiting on others (use --json for details)\n", out)

	// More waiting-on-you rows than the page shows: the hint.
	h.pending = `{"counts":{"waitingOnYou":2,"waitingOnOthers":0},"nextCursor":"c2",
	  "prompts":[{"promptMessageId":"m1","chatKey":"t","conversationLabel":"#release","askerAgentName":"release-bot",
	    "createdAt":"2026-11-13T04:48:00Z","waitingOnYou":true,
	    "decisions":[{"rowId":"r1","key":"ship-now","version":1,"status":"pending","question":"Deploy?"}]}],
	  "agents":[],"answerers":[],"conversations":[]}`
	out, err = runDecisionsTestCmd(t, "decisions", "list", "--limit", "1")
	require.NoError(t, err)
	assert.Equal(t, ""+
		"KEY       VER  QUESTION  AGENT        CONVERSATION  AGE\n"+
		"ship-now  v1   Deploy?   release-bot  #release      12m ago\n"+
		"More Decisions are waiting on you; raise --limit (up to 200) to list them.\n", out)
}

func TestDecisionsListHuman_AnsweredByMeNeedsAnIdentity(t *testing.T) {
	h := newHumanDecisionsHub(t)
	t.Setenv("SCION_CLI_MODE", "human")

	for _, body := range []string{`{}`, `{"id":""}`, `null`} {
		h.me = body
		before := h.count()
		_, err := runDecisionsTestCmd(t, "decisions", "list", "--made", "--answered-by", "me")
		require.Error(t, err, "me=%s", body)
		assert.Contains(t, err.Error(), "resolve --answered-by me", "me=%s", body)
		assert.NotContains(t, err.Error(), "%!w", "me=%s", body)
		assert.Equal(t, before, h.count(), "me=%s: no unfiltered list is requested", body)

		beforeProfile := h.profileCount()
		_, err = runDecisionsTestCmd(t, "decisions", "list", "--all-projects", "--made", "--answered-by", "me")
		require.Error(t, err, "all-projects me=%s", body)
		assert.Contains(t, err.Error(), "resolve --answered-by me", "all-projects me=%s", body)
		assert.NotContains(t, err.Error(), "%!w", "all-projects me=%s", body)
		assert.Equal(t, beforeProfile, h.profileCount(), "all-projects me=%s: no unfiltered list is requested", body)
	}

	// Without --answered-by me an empty identity only loses the "you" label.
	h.me = `{}`
	out, err := runDecisionsTestCmd(t, "decisions", "list", "--made")
	require.NoError(t, err)
	assert.Contains(t, out, "Alice")
	assert.NotContains(t, out, " you ")
}

const allProjectsDecisionsPendingBody = `{
  "counts": {"waitingOnYou": 3, "waitingOnOthers": 1},
  "prompts": [
    {"project": {"id": "proj-1", "name": "Scion", "slug": "scion"}, "promptMessageId": "m1", "promptAvailable": true,
     "chatKey": "` + humanDecisionsTopic + `", "conversationLabel": "#release", "conversationAvailable": true,
     "askerAgentId": "` + humanDecisionsBot + `", "askerAgentName": "release-bot", "createdAt": "2026-11-13T04:48:00Z",
     "answerMode": "single", "waitingOnYou": true, "canAnswer": true,
     "decisions": [
       {"rowId": "r1", "messageId": "m1", "key": "ship-now", "ref": "release-bot/ship-now", "version": 1, "status": "pending",
        "askerAgentId": "` + humanDecisionsBot + `", "askerAgentName": "release-bot", "question": "Deploy to prod after tagging v0.9?", "choices": []}
     ]},
    {"project": {"id": "proj-1", "name": "Scion", "slug": "scion"}, "promptMessageId": "m2", "promptAvailable": true,
     "chatKey": "` + humanDecisionsDM + `", "conversationLabel": "DM with nc-x", "conversationAvailable": true,
     "askerAgentId": "bbbb2222-0000-4000-8000-000000000004", "askerAgentName": "nc-x", "createdAt": "2026-11-13T03:00:00Z",
     "answerMode": "single", "waitingOnYou": true, "canAnswer": true,
     "decisions": [
       {"rowId": "r2", "messageId": "m2", "key": "migrate", "ref": "nc-x/migrate", "version": 3, "status": "pending",
        "askerAgentId": "bbbb2222-0000-4000-8000-000000000004", "askerAgentName": "nc-x", "question": "Rotate the staging DB password?", "choices": []}
     ]},
    {"project": {"id": "proj-2", "name": "Web Site", "slug": "web-site"}, "promptMessageId": "m3", "promptAvailable": true,
     "chatKey": "8e1f2d3c-2222-4b6c-8d9e-000000000002", "conversationLabel": "#landing", "conversationAvailable": true,
     "askerAgentId": "cccc3333-0000-4000-8000-000000000005", "askerAgentName": "design-bot", "createdAt": "2026-11-12T05:00:00Z",
     "answerMode": "single", "waitingOnYou": true, "canAnswer": true,
     "decisions": [
       {"rowId": "r3", "messageId": "m3", "key": "hero-img", "ref": "design-bot/hero-img", "version": 1, "status": "pending",
        "askerAgentId": "cccc3333-0000-4000-8000-000000000005", "askerAgentName": "design-bot", "question": "Use the dark hero image?", "choices": []}
     ]}
  ],
  "agents": [
    {"id": "` + humanDecisionsBot + `", "name": "release-bot"},
    {"id": "cccc3333-0000-4000-8000-000000000005", "name": "design-bot"}
  ],
  "answerers": [],
  "conversations": [
    {"key": "` + humanDecisionsTopic + `", "label": "#release", "projectId": "proj-1"},
    {"key": "8e1f2d3c-2222-4b6c-8d9e-000000000002", "label": "#landing", "projectId": "proj-2"}
  ],
  "projects": [
    {"id": "proj-1", "name": "Scion", "slug": "scion"},
    {"id": "proj-2", "name": "Web Site", "slug": "web-site"}
  ],
  "truncatedProjects": false
}`

const allProjectsDecisionsMadeBody = `{
  "counts": {"waitingOnYou": 3, "waitingOnOthers": 1},
  "entries": [
    {"project": {"id": "proj-1", "name": "Scion", "slug": "scion"}, "answerId": "a1",
     "decision": {"rowId": "r9", "promptMessageId": "m9", "promptAvailable": true, "key": "migrate", "ref": "nc-x/migrate", "version": 2,
                  "question": "Run the backfill on staging first?", "askerAgentId": "b", "askerAgentName": "nc-x"},
     "choiceId": "1", "label": "Yes", "matchedDefault": false, "answeredByUserId": "u-alice", "answeredByName": "Alice",
     "provenance": "direct", "outcome": "answer", "surface": "decisions_page", "sourceMessageAvailable": false,
     "at": "2026-11-12T10:00:00Z", "chatKey": "` + humanDecisionsTopic + `", "conversationLabel": "#release", "conversationAvailable": true},
    {"project": {"id": "proj-2", "name": "Web Site", "slug": "web-site"}, "answerId": "a2",
     "decision": {"rowId": "r8", "promptMessageId": "m8", "promptAvailable": true, "key": "hero-img", "ref": "design-bot/hero-img", "version": 1,
                  "question": "Use the dark hero image?", "askerAgentId": "c", "askerAgentName": "design-bot"},
     "choiceId": "2", "label": "No", "matchedDefault": false, "answeredByUserId": "u-bob", "answeredByName": "Bob",
     "provenance": "direct", "outcome": "confirm", "surface": "native", "sourceMessageAvailable": false,
     "at": "2026-11-02T10:00:00Z", "chatKey": "8e1f2d3c-2222-4b6c-8d9e-000000000002", "conversationLabel": "#landing", "conversationAvailable": true}
  ],
  "agents": [], "answerers": [], "conversations": [],
  "projects": [
    {"id": "proj-1", "name": "Scion", "slug": "scion"},
    {"id": "proj-2", "name": "Web Site", "slug": "web-site"}
  ],
  "truncatedProjects": false
}`

func TestDecisionsListHuman_AllProjectsProjectGateAndValidation(t *testing.T) {
	root := &cobra.Command{Use: "scion"}
	root.AddCommand(newDecisionsCmd())
	listCmd, _, err := root.Find([]string{"decisions", "list"})
	require.NoError(t, err)
	assert.True(t, commandRequiresProject(listCmd), "without --all-projects, decisions list requires a project")
	require.NoError(t, listCmd.Flags().Set("all-projects", "true"))
	assert.False(t, commandRequiresProject(listCmd), "with --all-projects, decisions list runs outside any local project")

	h := newHumanDecisionsHub(t)
	t.Setenv("SCION_CLI_MODE", "human")

	// --sort without --all-projects or with an invalid value is a usage error.
	for _, args := range [][]string{
		{"--sort", "recent"},
		{"--all-projects", "--sort", "bogus"},
	} {
		_, err = runDecisionsTestCmd(t, append([]string{"decisions", "list"}, args...)...)
		require.Error(t, err, "%v", args)
		assert.True(t, isUsageError(err), "%v", args)
	}
	assert.Zero(t, h.count())
	assert.Zero(t, h.profileCount())
}

func TestDecisionsListHuman_AllProjectsProjectResolutionAndAmbiguity(t *testing.T) {
	h := newHumanDecisionsHub(t)
	t.Setenv("SCION_CLI_MODE", "human")

	outsiderUUID := "99999999-8888-4777-8666-555555555555"
	for value, want := range map[string]string{
		"scion":      "proj-1",
		"SCION":      "proj-1",
		"Web Site":   "proj-2",
		"web-site":   "proj-2",
		"proj-2":     "proj-2",
		outsiderUUID: outsiderUUID, // unmatched UUID passed through; hub answers empty
	} {
		_, err := runDecisionsTestCmd(t, "decisions", "list", "--all-projects", "--project", value)
		require.NoError(t, err, value)
		assert.Equal(t, want, h.lastProfile().Get("project"), value)
	}
	assert.Zero(t, h.count(), "--all-projects never calls the single-project endpoint")

	_, err := runDecisionsTestCmd(t, "decisions", "list", "--all-projects", "--project", "nonexistent")
	require.Error(t, err)
	assert.Contains(t, err.Error(), `no project with Decisions matches "nonexistent"`)

	// Two projects sharing a display name: ambiguous, exit 2.
	h.profilePending = `{"counts":{"waitingOnYou":0,"waitingOnOthers":0},"prompts":[],"agents":[],"answerers":[],
	  "conversations":[],"projects":[{"id":"p-1","name":"App","slug":"app-1"},{"id":"p-2","name":"App","slug":"app-2"}],"truncatedProjects":false}`
	_, err = runDecisionsTestCmd(t, "decisions", "list", "--all-projects", "--project", "App")
	require.Error(t, err)
	assert.Equal(t, 2, exitCodeFor(err))
	assert.Contains(t, err.Error(), "matches 2 projects")
	assert.Contains(t, err.Error(), "p-1")
	assert.Contains(t, err.Error(), "p-2")

	// Experiment off (404) on the profile endpoint prints the design 5.4 message.
	h.status = http.StatusNotFound
	h.errBody = `{"error":{"code":"not_found","message":"route not found"}}`
	_, err = runDecisionsTestCmd(t, "decisions", "list", "--all-projects")
	require.Error(t, err)
	assert.Equal(t, "Decisions are not enabled on this hub (experiment web.project_decisions)", err.Error())
}

func TestDecisionsListHuman_AllProjectsTableAndJSONGolden(t *testing.T) {
	fixDecisionsClock(t)
	h := newHumanDecisionsHub(t)
	t.Setenv("SCION_CLI_MODE", "human")

	// Waiting table (Mock F-6: PROJECT KEY VER QUESTION AGENT CONVERSATION AGE).
	out, err := runDecisionsTestCmd(t, "decisions", "list", "--all-projects", "--sort", "recent")
	require.NoError(t, err)
	assert.Equal(t, url.Values{"tab": {"pending"}, "sort": {"recent"}, "limit": {"50"}}, h.lastProfile())
	assert.Equal(t, ""+
		"PROJECT   KEY       VER  QUESTION                            AGENT        CONVERSATION  AGE\n"+
		"scion     ship-now  v1   Deploy to prod after tagging v0.9?  release-bot  #release      12m ago\n"+
		"scion     migrate   v3   Rotate the staging DB password?     nc-x         DM with nc-x  2h ago\n"+
		"web-site  hero-img  v1   Use the dark hero image?            design-bot   #landing      1d ago\n"+
		"1 more waiting on others (use --json for details)\n", out)

	// Made sort=recent (default on --made).
	out, err = runDecisionsTestCmd(t, "decisions", "list", "--all-projects", "--made")
	require.NoError(t, err)
	assert.Equal(t, ""+
		"PROJECT   WHEN            KEY       VER  QUESTION                            ANSWER  ANSWERED BY  PROVENANCE  AGENT       CONVERSATION\n"+
		"scion     2026-11-12 UTC  migrate   v2   Run the backfill on staging first?  Yes     you          direct      nc-x        #release\n"+
		"web-site  2026-11-02 UTC  hero-img  v1   Use the dark hero image?            No      Bob          confirmed   design-bot  #landing\n", out)

	// Made sort=project overview (groups with more=true and truncatedProjects=true).
	h.profileMade = `{
	  "counts": {"waitingOnYou": 0, "waitingOnOthers": 0},
	  "groups": [
	    {"project": {"id": "proj-1", "name": "Scion", "slug": "scion"}, "more": true,
	     "entries": [
	       {"project": {"id": "proj-1", "name": "Scion", "slug": "scion"}, "answerId": "a1",
	        "decision": {"rowId": "r9", "promptMessageId": "m9", "promptAvailable": true, "key": "migrate", "ref": "nc-x/migrate", "version": 2,
	                     "question": "Run the backfill on staging first?", "askerAgentId": "b", "askerAgentName": "nc-x"},
	        "choiceId": "1", "label": "Yes", "answeredByUserId": "u-alice", "answeredByName": "Alice",
	        "provenance": "direct", "outcome": "answer", "surface": "decisions_page",
	        "at": "2026-11-12T10:00:00Z", "chatKey": "` + humanDecisionsTopic + `", "conversationLabel": "#release", "conversationAvailable": true}
	     ]}
	  ],
	  "agents": [], "answerers": [], "conversations": [], "projects": [], "truncatedProjects": true
	}`
	out, err = runDecisionsTestCmd(t, "decisions", "list", "--all-projects", "--made", "--sort", "project")
	require.NoError(t, err)
	assert.Equal(t, url.Values{"tab": {"made"}, "sort": {"project"}, "limit": {"50"}}, h.lastProfile())
	assert.Equal(t, ""+
		"PROJECT  WHEN            KEY      VER  QUESTION                            ANSWER  ANSWERED BY  PROVENANCE  AGENT  CONVERSATION\n"+
		"scion    2026-11-12 UTC  migrate  v2   Run the backfill on staging first?  Yes     you          direct      nc-x   #release\n"+
		"Some projects have more than 5 answers; pass --sort recent or --project <name> to list them.\n"+
		"Showing your 200 most recently active projects; pass --project <name> to see others.\n", out)

	// --json prints the profile API shape.
	out, err = runDecisionsTestCmd(t, "decisions", "list", "--all-projects", "--json")
	require.NoError(t, err)
	var got, want map[string]any
	require.NoError(t, json.Unmarshal([]byte(out), &got), out)
	require.NoError(t, json.Unmarshal([]byte(allProjectsDecisionsPendingBody), &want))
	assert.Equal(t, want, got)
}
