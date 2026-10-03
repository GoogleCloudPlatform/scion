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

	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
	"github.com/GoogleCloudPlatform/scion/pkg/hubsync"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// getReply is one scripted answer to the poll GET.
type getReply struct {
	status int
	body   string
}

const (
	replyDeleting = `{"id":"uuid-1","name":"%s","phase":"stopping","deletion":{"state":"deleting","claim":1,"startedAt":"2026-10-03T10:00:00Z"}}`
	replyLive     = `{"id":"uuid-1","name":"%s","phase":"running","deletion":null}`
	replySoftGone = `{"id":"uuid-1","name":"%s","phase":"stopped","deletedAt":"2026-10-03T10:00:05Z","deletion":null}`
	replyInDoubt  = `{"id":"uuid-1","name":"%s","phase":"running","deletion":{"state":"failed","code":"in_doubt","error":"broker teardown outstanding","claim":1,"startedAt":"2026-10-03T10:00:00Z"}}`
	replyRuntime  = `{"id":"uuid-1","name":"%s","phase":"running","deletion":{"state":"failed","code":"runtime_error","error":"broker unreachable","claim":1,"startedAt":"2026-10-03T10:00:00Z"}}`
)

// asyncDeleteHub is a hub stub whose DELETE answers 202 and whose
// project-scoped GET of the agent ID follows a script (the last reply
// repeats). It records, for each poll, whether the local agent directory
// still existed at that moment.
type asyncDeleteHub struct {
	t         *testing.T
	projectID string
	agentName string
	agentDir  string
	replies   []getReply

	mu           sync.Mutex
	deletes      int
	stops        int
	polls        int
	dirAtPoll    []bool
	deleteStatus int
}

func (h *asyncDeleteHub) serve(w http.ResponseWriter, r *http.Request) {
	h.mu.Lock()
	defer h.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	agentPath := "/api/v1/projects/" + h.projectID + "/agents/"
	switch {
	case r.URL.Path == "/healthz":
		_ = json.NewEncoder(w).Encode(map[string]interface{}{"status": "ok"})
	case r.Method == http.MethodPost && r.URL.Path == agentPath+h.agentName+"/stop":
		h.stops++
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{}`))
	case r.Method == http.MethodDelete && r.URL.Path == agentPath+h.agentName:
		h.deletes++
		status := h.deleteStatus
		if status == 0 {
			status = http.StatusAccepted
		}
		w.WriteHeader(status)
		if status == http.StatusAccepted {
			_, _ = w.Write([]byte(`{"agentId":"uuid-1","deletion":{"state":"deleting","claim":1,"startedAt":"2026-10-03T10:00:00Z"}}`))
		}
	case r.Method == http.MethodGet && r.URL.Path == agentPath+"uuid-1":
		if h.agentDir != "" {
			_, err := os.Stat(h.agentDir)
			h.dirAtPoll = append(h.dirAtPoll, err == nil)
		}
		i := h.polls
		if i >= len(h.replies) {
			i = len(h.replies) - 1
		}
		h.polls++
		rep := h.replies[i]
		if rep.status == 0 {
			rep.status = http.StatusOK
		}
		w.WriteHeader(rep.status)
		if rep.body != "" {
			_, _ = w.Write([]byte(strings.ReplaceAll(rep.body, "%s", h.agentName)))
		} else {
			_, _ = w.Write([]byte(`{"error":{"code":"x","message":"` + http.StatusText(rep.status) + `"}}`))
		}
	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

// fastDeletionWait makes the poll fast for a test and restores it after.
func fastDeletionWait(t *testing.T) {
	t.Helper()
	orig := hubDeletionWaitOptions
	hubDeletionWaitOptions = hubclient.DeletionWaitOptions{Interval: time.Millisecond, Timeout: 100 * time.Millisecond}
	t.Cleanup(func() { hubDeletionWaitOptions = orig })
}

type asyncDeleteEnv struct {
	hub        *asyncDeleteHub
	hubCtx     *HubContext
	agentDir   string
	projectDir string
}

func setupAsyncDelete(t *testing.T, agentName string, replies ...getReply) *asyncDeleteEnv {
	t.Helper()
	orig := saveDeleteTestState()
	t.Cleanup(orig.restore)
	fastDeletionWait(t)

	tmpHome := t.TempDir()
	_ = os.Setenv("HOME", tmpHome)
	preserveBranch = true

	projectDir := filepath.Join(tmpHome, "project", ".scion")
	require.NoError(t, os.MkdirAll(projectDir, 0755))
	projectPath = projectDir
	agentDir := createAgentDir(t, projectDir, agentName)
	hubsync.AddSyncedAgent(projectDir, agentName)

	hub := &asyncDeleteHub{t: t, projectID: "project-async", agentName: agentName, agentDir: agentDir, replies: replies}
	srv := httptest.NewServer(http.HandlerFunc(hub.serve))
	t.Cleanup(srv.Close)
	client, err := hubclient.New(srv.URL)
	require.NoError(t, err)

	return &asyncDeleteEnv{
		hub:        hub,
		agentDir:   agentDir,
		projectDir: projectDir,
		hubCtx:     &HubContext{Client: client, Endpoint: srv.URL, ProjectID: hub.projectID, ProjectPath: projectDir},
	}
}

func (e *asyncDeleteEnv) dirExists() bool {
	_, err := os.Stat(e.agentDir)
	return err == nil
}

func (e *asyncDeleteEnv) stillSynced(t *testing.T) bool {
	t.Helper()
	st, err := config.LoadProjectState(e.projectDir)
	require.NoError(t, err)
	for _, n := range st.SyncedAgents {
		if n == e.hub.agentName {
			return true
		}
	}
	return false
}

// Acceptance (i): with a 202, the worktree is kept until the poll confirms.
func TestDeleteAgentsViaHub_202KeepsWorktreeUntilConfirmed(t *testing.T) {
	for _, tc := range []struct {
		name    string
		replies []getReply
	}{
		{"404", []getReply{{body: replyDeleting}, {body: replyDeleting}, {status: http.StatusNotFound}}},
		{"deletedAt", []getReply{{body: replyDeleting}, {body: replySoftGone}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env := setupAsyncDelete(t, "slow-agent", tc.replies...)
			require.NoError(t, deleteAgentsViaHub(env.hubCtx, []string{"slow-agent"}))

			assert.Equal(t, 1, env.hub.deletes)
			assert.GreaterOrEqual(t, env.hub.polls, 2)
			for i, existed := range env.hub.dirAtPoll {
				assert.True(t, existed, "worktree must exist at poll %d, before the delete is confirmed", i)
			}
			assert.False(t, env.dirExists(), "worktree removed once the delete is confirmed")
			assert.False(t, env.stillSynced(t), "synced-agent entry removed once confirmed")
		})
	}
}

// Acceptance (i): a 403 poll or a timeout keeps the worktree. Neither is a
// failure, so the command exits 0.
func TestDeleteAgentsViaHub_202UnobservableKeepsWorktree(t *testing.T) {
	for _, tc := range []struct {
		name    string
		replies []getReply
	}{
		{"403", []getReply{{body: replyDeleting}, {status: http.StatusForbidden}}},
		{"timeout", []getReply{{body: replyDeleting}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env := setupAsyncDelete(t, "slow-agent", tc.replies...)
			require.NoError(t, deleteAgentsViaHub(env.hubCtx, []string{"slow-agent"}))

			assert.True(t, env.dirExists(), "worktree kept when completion cannot be observed")
			assert.True(t, env.stillSynced(t), "sync state left alone, so a later sync sees the agent as stale")
		})
	}
}

func TestDeleteAgentsViaHub_202FailedKeepsWorktreeAndErrors(t *testing.T) {
	t.Run("runtime_error", func(t *testing.T) {
		env := setupAsyncDelete(t, "bad-agent", getReply{body: replyDeleting}, getReply{body: replyRuntime})
		err := deleteAgentsViaHub(env.hubCtx, []string{"bad-agent"})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "delete failed on the Hub (runtime_error): broker unreachable")
		assert.Contains(t, err.Error(), "local worktree kept")
		assert.Contains(t, err.Error(), "scion delete bad-agent")
		assert.Contains(t, err.Error(), "force-delete it from the web UI")
		assert.NotContains(t, err.Error(), "Starting the agent stays blocked")
		assert.True(t, env.dirExists())
		assert.True(t, env.stillSynced(t))
	})
	t.Run("in_doubt says start stays blocked", func(t *testing.T) {
		env := setupAsyncDelete(t, "bad-agent", getReply{body: replyInDoubt})
		err := deleteAgentsViaHub(env.hubCtx, []string{"bad-agent"})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "(in_doubt)")
		assert.Contains(t, err.Error(), "Starting the agent stays blocked until a retry succeeds or force is used")
		assert.True(t, env.dirExists())
	})
}

// After a 202, a live row with no deletion marker means no delete is running.
func TestDeleteAgentsViaHub_202NotTakenKeepsWorktreeAndErrors(t *testing.T) {
	env := setupAsyncDelete(t, "ghost-agent", getReply{body: replyLive})
	err := deleteAgentsViaHub(env.hubCtx, []string{"ghost-agent"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "delete did not take effect")
	assert.Contains(t, err.Error(), "scion delete ghost-agent")
	assert.Equal(t, 2, env.hub.polls, "one grace poll before deciding")
	assert.True(t, env.dirExists())
	assert.True(t, env.stillSynced(t))
}

// A 204 behaves as today: no poll, worktree removed.
func TestDeleteAgentsViaHub_204DoesNotPoll(t *testing.T) {
	env := setupAsyncDelete(t, "fast-agent", getReply{body: replyDeleting})
	env.hub.deleteStatus = http.StatusNoContent
	require.NoError(t, deleteAgentsViaHub(env.hubCtx, []string{"fast-agent"}))
	assert.Equal(t, 0, env.hub.polls)
	assert.False(t, env.dirExists())
}

func setStopRm(t *testing.T) {
	t.Helper()
	origRm, origAll, origConfirm := stopRm, stopAll, autoConfirm
	stopRm, stopAll, autoConfirm = true, false, true
	t.Cleanup(func() { stopRm, stopAll, autoConfirm = origRm, origAll, origConfirm })
}

func TestStopAgentViaHub_RmWaitsOn202(t *testing.T) {
	t.Run("confirmed", func(t *testing.T) {
		env := setupAsyncDelete(t, "stop-agent", getReply{body: replyDeleting}, getReply{status: http.StatusNotFound})
		setStopRm(t)
		require.NoError(t, stopAgentViaHub(env.hubCtx, "stop-agent"))
		assert.Equal(t, 1, env.hub.stops)
		assert.Equal(t, 2, env.hub.polls)
		assert.False(t, env.stillSynced(t), "confirmed removal updates the sync state")
	})
	t.Run("failed", func(t *testing.T) {
		env := setupAsyncDelete(t, "stop-agent", getReply{body: replyRuntime})
		setStopRm(t)
		err := stopAgentViaHub(env.hubCtx, "stop-agent")
		require.Error(t, err)
		assert.Contains(t, err.Error(), "agent stopped but failed to delete via Hub")
		assert.Contains(t, err.Error(), "(runtime_error)")
		assert.True(t, env.stillSynced(t))
	})
	t.Run("not taken", func(t *testing.T) {
		env := setupAsyncDelete(t, "stop-agent", getReply{body: replyLive})
		setStopRm(t)
		err := stopAgentViaHub(env.hubCtx, "stop-agent")
		require.Error(t, err)
		assert.Contains(t, err.Error(), "delete did not take effect")
	})
	for _, tc := range []struct {
		name    string
		replies []getReply
	}{
		{"403", []getReply{{status: http.StatusForbidden}}},
		{"timeout", []getReply{{body: replyDeleting}}},
	} {
		t.Run(tc.name+" is not a failure", func(t *testing.T) {
			env := setupAsyncDelete(t, "stop-agent", tc.replies...)
			setStopRm(t)
			require.NoError(t, stopAgentViaHub(env.hubCtx, "stop-agent"))
			assert.True(t, env.stillSynced(t), "sync state left alone until removal is confirmed")
		})
	}
}

func TestStopAllAgentsViaHub_RmWaitsOn202(t *testing.T) {
	for _, tc := range []struct {
		name       string
		replies    []getReply
		wantErr    string
		wantSynced bool
	}{
		{name: "confirmed", replies: []getReply{{status: http.StatusNotFound}}, wantSynced: false},
		{name: "failed", replies: []getReply{{body: replyInDoubt}}, wantErr: "(in_doubt)", wantSynced: true},
		{name: "timeout", replies: []getReply{{body: replyDeleting}}, wantSynced: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env := setupAsyncDelete(t, "all-agent", tc.replies...)
			setStopRm(t)
			list := func(w http.ResponseWriter, r *http.Request) bool {
				if r.Method == http.MethodGet && r.URL.Path == "/api/v1/projects/"+env.hub.projectID+"/agents" {
					w.Header().Set("Content-Type", "application/json")
					_ = json.NewEncoder(w).Encode(map[string]interface{}{
						"agents":     []map[string]interface{}{{"id": "uuid-1", "name": "all-agent", "phase": "running"}},
						"serverTime": time.Now().UTC().Format(time.RFC3339Nano),
					})
					return true
				}
				return false
			}
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if !list(w, r) {
					env.hub.serve(w, r)
				}
			}))
			t.Cleanup(srv.Close)
			client, err := hubclient.New(srv.URL)
			require.NoError(t, err)
			env.hubCtx.Client = client
			env.hubCtx.Endpoint = srv.URL

			err = stopAllAgentsViaHub(env.hubCtx)
			if tc.wantErr != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tc.wantErr)
			} else {
				require.NoError(t, err)
			}
			assert.Equal(t, tc.wantSynced, env.stillSynced(t))
		})
	}
}
