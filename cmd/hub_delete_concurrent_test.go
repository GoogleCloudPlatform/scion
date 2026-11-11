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
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
	"github.com/GoogleCloudPlatform/scion/pkg/hubsync"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// multiDeleteHub is a hub stub for several agents whose DELETE answers 202
// (or deleteStatus[name]). GET of an agent's ID answers "deleting" until
// release says the delete is done, then 404 (or final[name]). It tracks how
// many agents were mid-poll at once.
type multiDeleteHub struct {
	projectID    string
	dirs         map[string]string   // local agent dirs, for release rules
	deleteStatus map[string]int      // DELETE status per agent; default 202
	final        map[string]getReply // reply once released; default 404

	mu sync.Mutex
	// release reports whether name's delete is done. It runs under mu.
	release     func(h *multiDeleteHub, name string) bool
	polls       map[string]int
	deletes     map[string]int // DELETE requests per agent
	firstPolls  []string       // agents in the order of their first poll
	released    map[string]bool
	releaseSeq  []string
	inFlight    int
	maxInFlight int
}

func (h *multiDeleteHub) serve(w http.ResponseWriter, r *http.Request) {
	h.mu.Lock()
	defer h.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	agentPath := "/api/v1/projects/" + h.projectID + "/agents/"
	rest := strings.TrimPrefix(r.URL.Path, agentPath)
	switch {
	case r.URL.Path == "/healthz":
		_ = json.NewEncoder(w).Encode(map[string]interface{}{"status": "ok"})
	case r.Method == http.MethodDelete && strings.HasPrefix(r.URL.Path, agentPath):
		h.deletes[rest]++
		if st, ok := h.deleteStatus[rest]; ok && st != http.StatusAccepted {
			w.WriteHeader(st)
			if st >= 400 {
				_, _ = w.Write([]byte(`{"error":{"code":"conflict","message":"agent is busy"}}`))
			}
			return
		}
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write([]byte(`{"agentId":"id-` + rest + `","deletion":{"state":"deleting","claim":1,"startedAt":"2026-10-03T10:00:00Z"}}`))
	case r.Method == http.MethodGet && strings.HasPrefix(rest, "id-"):
		name := strings.TrimPrefix(rest, "id-")
		if h.released[name] {
			h.writeFinal(w, name)
			return
		}
		if h.polls[name] == 0 {
			h.firstPolls = append(h.firstPolls, name)
			h.inFlight++
			h.maxInFlight = max(h.maxInFlight, h.inFlight)
		}
		h.polls[name]++
		if h.release(h, name) {
			h.released[name] = true
			h.releaseSeq = append(h.releaseSeq, name)
			h.inFlight--
			h.writeFinal(w, name)
			return
		}
		_, _ = w.Write([]byte(`{"id":"id-` + name + `","name":"` + name + `","phase":"stopping","deletion":{"state":"deleting","claim":1,"startedAt":"2026-10-03T10:00:00Z"}}`))
	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

func (h *multiDeleteHub) writeFinal(w http.ResponseWriter, name string) {
	rep, ok := h.final[name]
	if !ok {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"error":{"code":"not_found","message":"gone"}}`))
		return
	}
	if rep.status == 0 {
		rep.status = http.StatusOK
	}
	w.WriteHeader(rep.status)
	if rep.body != "" {
		_, _ = w.Write([]byte(strings.ReplaceAll(rep.body, "%s", name)))
	} else {
		_, _ = w.Write([]byte(`{"error":{"code":"x","message":"` + http.StatusText(rep.status) + `"}}`))
	}
}

func dirGone(p string) bool {
	_, err := os.Stat(p)
	return os.IsNotExist(err)
}

type multiDeleteEnv struct {
	hub        *multiDeleteHub
	hubCtx     *HubContext
	projectDir string
	dirs       map[string]string
}

// setupMultiDelete creates local agent dirs for names and a hub stub. The
// poll budget is generous (5s) so that only a sequential poll, never a slow
// machine, makes these tests time out.
func setupMultiDelete(t *testing.T, limit int, release func(h *multiDeleteHub, name string) bool, names ...string) *multiDeleteEnv {
	t.Helper()
	orig := saveDeleteTestState()
	t.Cleanup(orig.restore)
	origWait, origLimit := hubDeletionWaitOptions, hubDeletePollConcurrency
	hubDeletionWaitOptions = hubclient.DeletionWaitOptions{Interval: time.Millisecond, Timeout: 5 * time.Second}
	hubDeletePollConcurrency = limit
	t.Cleanup(func() { hubDeletionWaitOptions, hubDeletePollConcurrency = origWait, origLimit })

	tmpHome := t.TempDir()
	_ = os.Setenv("HOME", tmpHome)
	preserveBranch = true
	projectDir := filepath.Join(tmpHome, "project", ".scion")
	require.NoError(t, os.MkdirAll(projectDir, 0755))
	projectPath = projectDir

	dirs := map[string]string{}
	for _, n := range names {
		dirs[n] = createAgentDir(t, projectDir, n)
		hubsync.AddSyncedAgent(projectDir, n)
	}
	hub := &multiDeleteHub{projectID: "project-multi", dirs: dirs, release: release, polls: map[string]int{}, deletes: map[string]int{}, released: map[string]bool{}}
	srv := httptest.NewServer(http.HandlerFunc(hub.serve))
	t.Cleanup(srv.Close)
	client, err := hubclient.New(srv.URL)
	require.NoError(t, err)
	return &multiDeleteEnv{
		hub:        hub,
		projectDir: projectDir,
		dirs:       dirs,
		hubCtx:     &HubContext{Client: client, Endpoint: srv.URL, ProjectID: hub.projectID, ProjectPath: projectDir},
	}
}

// releaseFirstLast holds every delete until all agents have been polled at
// least once, and holds the first agent's until every other one is done and
// its local dir has been removed. A sequential poll can never satisfy it
// (the first agent's poll would run alone until its budget ran out), and
// neither can cleanup that waits for the first agent's result.
func releaseFirstLast(names []string) func(h *multiDeleteHub, name string) bool {
	return func(h *multiDeleteHub, name string) bool {
		for _, n := range names {
			if h.polls[n] == 0 {
				return false
			}
		}
		if name != names[0] {
			return true
		}
		for _, n := range names[1:] {
			if !h.released[n] || !dirGone(h.dirs[n]) {
				return false
			}
		}
		return true
	}
}

// ptone/scion#2895: accepted deletes are polled concurrently, and results are
// still reported (and cleaned up) in the order the agents were named.
func TestDeleteAgentsViaHub_202PollsConcurrentlyInOrder(t *testing.T) {
	names := []string{"agent-a", "agent-b", "agent-c"}

	t.Run("text", func(t *testing.T) {
		env := setupMultiDelete(t, 4, releaseFirstLast(names), names...)
		start := time.Now()
		_, stderr := captureStdIO(t, func() {
			require.NoError(t, deleteAgentsViaHub(env.hubCtx, names))
		})
		assert.Less(t, time.Since(start), 4*time.Second, "polls must overlap, not run one after another")
		require.Len(t, env.hub.releaseSeq, len(names), "every delete confirmed")
		assert.Equal(t, names[0], env.hub.releaseSeq[len(env.hub.releaseSeq)-1], "the first agent was confirmed last")

		last := -1
		for _, n := range names {
			i := strings.Index(stderr, "Agent '"+n+"' deleted via Hub.")
			require.GreaterOrEqual(t, i, 0, "missing result for %s in:\n%s", n, stderr)
			assert.Greater(t, i, last, "results are printed in input order")
			last = i
			_, err := os.Stat(env.dirs[n])
			assert.True(t, os.IsNotExist(err), "worktree of %s removed once confirmed", n)
		}
		// The others were cleaned up while the head agent was still being
		// polled, and said so at once, before the head agent's result.
		for _, n := range names[1:] {
			i := strings.Index(stderr, "Cleaned up locally: "+n+"\n")
			require.GreaterOrEqual(t, i, 0, "missing cleanup progress for %s in:\n%s", n, stderr)
			assert.Less(t, i, strings.Index(stderr, "Agent 'agent-a' deleted via Hub."))
		}
		assert.Contains(t, stderr, "Cleaned up locally: agent-a\n")
		// Every DELETE is sent (and its progress line printed) before any
		// result, since results wait on the polls.
		assert.Less(t, strings.Index(stderr, "Deleting agent 'agent-c'..."), strings.Index(stderr, "Agent 'agent-a' deleted via Hub."))
	})

	t.Run("json", func(t *testing.T) {
		env := setupMultiDelete(t, 4, releaseFirstLast(names), names...)
		setJSONOutput(t)
		stdout, _ := captureStdIO(t, func() {
			require.NoError(t, deleteAgentsViaHub(env.hubCtx, names))
		})
		var out struct {
			Status  string                   `json:"status"`
			Results []map[string]interface{} `json:"results"`
		}
		require.NoError(t, json.Unmarshal([]byte(stdout), &out), stdout)
		assert.Equal(t, "success", out.Status)
		require.Len(t, out.Results, len(names))
		for i, n := range names {
			assert.Equal(t, n, out.Results[i]["agent"], "JSON results are in input order")
			assert.Equal(t, "success", out.Results[i]["status"])
		}
	})
}

// ptone/scion#2895: exactly hubDeletePollConcurrency polls run at once. Each
// delete is held until the limit is reached (or too few agents remain to
// reach it), so a lower effective bound deterministically times out. Each
// held poll also runs for 20 rounds, long enough that an extra poll started
// past the limit shows up in maxInFlight.
func TestDeleteAgentsViaHub_202PollConcurrencyIsBounded(t *testing.T) {
	const limit = 2
	names := []string{"agent-1", "agent-2", "agent-3", "agent-4", "agent-5"}
	release := func(h *multiDeleteHub, name string) bool {
		reached := h.inFlight >= limit || len(names)-len(h.released) < limit
		return reached && h.polls[name] >= 20
	}
	env := setupMultiDelete(t, limit, release, names...)

	_, stderr := captureStdIO(t, func() {
		require.NoError(t, deleteAgentsViaHub(env.hubCtx, names))
	})
	assert.Equal(t, limit, env.hub.maxInFlight, "exactly the limit of polls in flight")
	assert.Len(t, env.hub.releaseSeq, len(names), "every delete confirmed")
	for _, n := range names {
		assert.Contains(t, stderr, "Agent '"+n+"' deleted via Hub.")
	}
}

// ptone/scion#2895: per-agent outcomes and the exit status are unchanged when
// different outcomes are polled in one run.
func TestDeleteAgentsViaHub_202MixedOutcomes(t *testing.T) {
	names := []string{"ok-204", "ok-202", "fail-202", "err-delete", "pending-403"}
	release := func(h *multiDeleteHub, name string) bool { return h.polls[name] >= 2 }
	setup := func(t *testing.T) *multiDeleteEnv {
		env := setupMultiDelete(t, 4, release, names...)
		env.hub.deleteStatus = map[string]int{"ok-204": http.StatusNoContent, "err-delete": http.StatusConflict}
		env.hub.final = map[string]getReply{
			"fail-202":    {body: replyRuntime},
			"pending-403": {status: http.StatusForbidden},
		}
		return env
	}
	checkDirs := func(t *testing.T, env *multiDeleteEnv) {
		assert.True(t, dirGone(env.dirs["ok-204"]), "204 cleans up")
		assert.True(t, dirGone(env.dirs["ok-202"]), "confirmed 202 cleans up")
		assert.False(t, dirGone(env.dirs["fail-202"]), "failed delete keeps the worktree")
		assert.False(t, dirGone(env.dirs["err-delete"]), "DELETE error keeps the worktree")
		assert.False(t, dirGone(env.dirs["pending-403"]), "unobservable delete keeps the worktree")
	}

	t.Run("text", func(t *testing.T) {
		env := setup(t)
		var err error
		_, stderr := captureStdIO(t, func() {
			err = deleteAgentsViaHub(env.hubCtx, names)
		})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "failed to delete some agents via Hub")
		assert.Contains(t, err.Error(), "fail-202: delete failed on the Hub (runtime_error): broker unreachable")
		assert.Contains(t, err.Error(), "err-delete: ")
		for _, n := range []string{"ok-204", "ok-202", "pending-403"} {
			assert.NotContains(t, err.Error(), n+":")
		}
		want := []string{
			"Agent 'ok-204' deleted via Hub.",
			"Agent 'ok-202' deleted via Hub.",
			"Agent 'pending-403': delete accepted; cannot observe completion; local worktree kept (the Hub did not allow reading the agent).",
		}
		last := -1
		for _, w := range want {
			i := strings.Index(stderr, w)
			require.GreaterOrEqual(t, i, 0, "missing %q in:\n%s", w, stderr)
			assert.Greater(t, i, last, "results are printed in input order")
			last = i
		}
		checkDirs(t, env)
	})

	t.Run("json", func(t *testing.T) {
		env := setup(t)
		setJSONOutput(t)
		var err error
		stdout, _ := captureStdIO(t, func() {
			err = deleteAgentsViaHub(env.hubCtx, names)
		})
		require.Error(t, err, "a partial failure exits non-zero in JSON mode too (ptone/scion#2894)")
		assert.True(t, isReportedInJSON(err))
		var out struct {
			Status  string                   `json:"status"`
			Results []map[string]interface{} `json:"results"`
		}
		require.NoError(t, json.Unmarshal([]byte(stdout), &out), stdout)
		assert.Equal(t, "partial", out.Status)
		require.Len(t, out.Results, len(names))
		wantStatus := []string{"success", "success", "error", "error", "accepted"}
		for i, n := range names {
			assert.Equal(t, n, out.Results[i]["agent"], "JSON results are in input order")
			assert.Equal(t, wantStatus[i], out.Results[i]["status"], n)
		}
		assert.Contains(t, out.Results[2]["error"], "(runtime_error)")
		assert.Equal(t, true, out.Results[4]["worktreeKept"])
		checkDirs(t, env)
	})
}

// ptone/scion#2895: with more accepted deletes than poll slots, slots are
// handed out in input order, so the agent the results wait on first is
// polled first. This pins the observable order (it catches, say, LIFO or
// map-order hand-out). It cannot tell the feeder apart from one goroutine
// per poll blocking on the semaphore: DELETEs are sent one by one and Go
// queues channel waiters FIFO, so that also yields input order in practice.
func TestDeleteAgentsViaHub_202PollSlotsInInputOrder(t *testing.T) {
	names := []string{"agent-a", "agent-b", "agent-c"}
	release := func(h *multiDeleteHub, name string) bool { return h.polls[name] >= 3 }
	env := setupMultiDelete(t, 1, release, names...)

	_, _ = captureStdIO(t, func() {
		require.NoError(t, deleteAgentsViaHub(env.hubCtx, names))
	})
	assert.Equal(t, names, env.hub.firstPolls, "polls start in input order")
	assert.Equal(t, 1, env.hub.maxInFlight)
}

// ptone/scion#2895: a name given twice is deleted, cleaned up and reported
// once, at its first position.
func TestDeleteAgentsViaHub_DuplicateNamesDeletedOnce(t *testing.T) {
	release := func(h *multiDeleteHub, name string) bool { return h.polls[name] >= 2 }
	input := []string{"agent-a", "agent-b", "agent-a"}

	t.Run("text", func(t *testing.T) {
		env := setupMultiDelete(t, 4, release, "agent-a", "agent-b")
		_, stderr := captureStdIO(t, func() {
			require.NoError(t, deleteAgentsViaHub(env.hubCtx, input))
		})
		assert.Equal(t, map[string]int{"agent-a": 1, "agent-b": 1}, env.hub.deletes)
		assert.Equal(t, 1, strings.Count(stderr, "Deleting agent 'agent-a'..."))
		assert.Equal(t, 1, strings.Count(stderr, "Agent 'agent-a' deleted via Hub."))
		assert.Less(t, strings.Index(stderr, "Agent 'agent-a' deleted via Hub."), strings.Index(stderr, "Agent 'agent-b' deleted via Hub."))
		assert.True(t, dirGone(env.dirs["agent-a"]))
		assert.True(t, dirGone(env.dirs["agent-b"]))
	})
	t.Run("json", func(t *testing.T) {
		env := setupMultiDelete(t, 4, release, "agent-a", "agent-b")
		setJSONOutput(t)
		stdout, stderr := captureStdIO(t, func() {
			require.NoError(t, deleteAgentsViaHub(env.hubCtx, input))
		})
		assert.NotContains(t, stderr, "Cleaned up locally", "no progress lines in JSON mode")
		var out struct {
			Status  string                   `json:"status"`
			Results []map[string]interface{} `json:"results"`
		}
		require.NoError(t, json.Unmarshal([]byte(stdout), &out), stdout)
		require.Len(t, out.Results, 2)
		assert.Equal(t, "agent-a", out.Results[0]["agent"])
		assert.Equal(t, "agent-b", out.Results[1]["agent"])
	})
}
