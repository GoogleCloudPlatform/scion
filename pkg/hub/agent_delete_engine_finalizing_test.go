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

//go:build !no_sqlite

package hub

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A delete that fails after teardown (revoke_failed, finalize_failed) leaves
// the row finalizing. Retrying it finishes the delete under the request the
// original claim stored: soft or hard, delete-files and remove-branch are
// never re-derived from the retry's parameters or the current retention
// setting (ptone/scion#4183). Fresh claims (none/failed rows, a lapsed
// deleting row) still derive a new request.

// finalizingFixture is an engine test server whose store can fail revokes,
// plus one broker agent.
type finalizingFixture struct {
	srv   *Server
	s     store.Store
	hooks *engineHookStore
	disp  *engineStubDispatcher
	agent *store.Agent
}

func newFinalizingFixture(t *testing.T, suffix string, retention time.Duration) *finalizingFixture {
	t.Helper()
	srv, s, _, disp := engineTestServer(t)
	hooks, _ := installStoreFault(t, srv, func(inner store.Store, _ *storeFaultSwitch) *engineHookStore {
		return &engineHookStore{Store: inner}
	})
	srv.config.SoftDeleteRetention = retention
	srv.config.SoftDeleteRetainFiles = false
	return &finalizingFixture{
		srv: srv, s: s, hooks: hooks, disp: disp,
		agent: setupBrokerAgentInPhase(t, s, suffix, state.PhaseRunning),
	}
}

func (f *finalizingFixture) del(t *testing.T, query string) *httptest.ResponseRecorder {
	t.Helper()
	return doRequest(t, f.srv, http.MethodDelete, "/api/v1/agents/"+f.agent.ID+query, nil)
}

// requireFinalizingFailed asserts the row is left finalizing with code and
// returns it.
func (f *finalizingFixture) requireFinalizingFailed(t *testing.T, rec *httptest.ResponseRecorder, code string) *store.Agent {
	t.Helper()
	require.NotEqual(t, http.StatusNoContent, rec.Code, rec.Body.String())
	got := mustGetAgent(t, f.s, f.agent.ID)
	require.Equal(t, store.DeletionStateFinalizing, got.DeletionState)
	require.Equal(t, code, got.DeletionCode)
	require.True(t, got.DeletedAt.IsZero())
	return got
}

// revokeFailed runs a DELETE with query whose revoke fails, leaving the row
// finalizing/revoke_failed, then lets revokes succeed again.
func (f *finalizingFixture) revokeFailed(t *testing.T, query string) *store.Agent {
	t.Helper()
	f.hooks.setRevokeErr(errors.New("credential store unavailable"))
	got := f.requireFinalizingFailed(t, f.del(t, query), store.DeletionCodeRevokeFailed)
	f.hooks.setRevokeErr(nil)
	return got
}

// failFinalizeTimes makes the next n finalize transactions fail.
func failFinalizeTimes(t *testing.T, n int) {
	t.Helper()
	var mu sync.Mutex
	left := n
	old := agentDeletionFinalizeSeam
	t.Cleanup(func() { agentDeletionFinalizeSeam = old })
	agentDeletionFinalizeSeam = func(context.Context, store.Store, *store.Agent, store.DeletionFinalizeMode) error {
		mu.Lock()
		defer mu.Unlock()
		if left > 0 {
			left--
			return errors.New("injected finalize error")
		}
		return nil
	}
}

// requireSoftDeleted asserts the retry answered 204 and kept the row,
// soft-deleted.
func requireSoftDeleted(t *testing.T, s store.Store, id string, rec *httptest.ResponseRecorder) {
	t.Helper()
	require.Equal(t, http.StatusNoContent, rec.Code, rec.Body.String())
	require.False(t, agentGone(t, s, id), "the stored soft delete keeps the row")
	assert.False(t, mustGetAgent(t, s, id).DeletedAt.IsZero(), "soft-deleted")
}

// requireHardDeleted asserts the retry answered 204 and removed the row.
func requireHardDeleted(t *testing.T, s store.Store, id string, rec *httptest.ResponseRecorder) {
	t.Helper()
	require.Equal(t, http.StatusNoContent, rec.Code, rec.Body.String())
	assert.True(t, agentGone(t, s, id), "hard-deleted")
}

// fallbackWarning is the claim's log message for a finalizing row with no
// usable stored request.
const fallbackWarning = "finalizing row has no stored request"

// captureLifecycleLog sends the server's lifecycle log to a buffer.
func captureLifecycleLog(srv *Server) *syncBuffer {
	logs := &syncBuffer{}
	srv.agentLifecycleLog = slog.New(slog.NewTextHandler(logs, nil))
	return logs
}

// fallbackWarnings returns the logged fallback warning lines.
func fallbackWarnings(logs *syncBuffer) []string {
	var out []string
	for _, line := range strings.Split(logs.String(), "\n") {
		if strings.Contains(line, fallbackWarning) {
			out = append(out, line)
		}
	}
	return out
}

// emptyStoredRequest blanks the stored request of a's finalizing row,
// pinned to its claim.
func emptyStoredRequest(t *testing.T, s store.Store, a *store.Agent) {
	t.Helper()
	claim := a.DeletionClaim
	empty := ""
	n, err := s.UpdateAgentDeletion(context.Background(), a.ID,
		store.DeletionPredicate{Claim: &claim, States: []string{store.DeletionStateFinalizing}},
		store.DeletionFields{Request: &empty})
	require.NoError(t, err)
	require.Equal(t, 1, n)
}

// A soft delete that failed its revoke, retried with force, still ends soft.
func TestAgentDeleteFinalizingRetry_ForceKeepsSoft(t *testing.T) {
	f := newFinalizingFixture(t, "fin-force", time.Hour)
	got := f.revokeFailed(t, "")
	require.True(t, got.ParseDeletionRequest().Soft)

	rec := f.del(t, "?force=true")
	requireSoftDeleted(t, f.s, f.agent.ID, rec)
	assert.Equal(t, 1, f.disp.callCount(), "the finalizing re-claim skips the dispatch")
}

// A soft delete that failed its finalize still ends soft after retention
// is turned off.
func TestAgentDeleteFinalizingRetry_RetentionOffKeepsSoft(t *testing.T) {
	f := newFinalizingFixture(t, "fin-retoff", time.Hour)
	failFinalizeTimes(t, 1)
	got := f.requireFinalizingFailed(t, f.del(t, ""), store.DeletionCodeFinalizeFailed)
	require.True(t, got.ParseDeletionRequest().Soft)

	f.srv.config.SoftDeleteRetention = 0
	requireSoftDeleted(t, f.s, f.agent.ID, f.del(t, ""))
}

// A hard delete that failed its revoke still ends hard after retention is
// turned on.
func TestAgentDeleteFinalizingRetry_RetentionOnKeepsHard(t *testing.T) {
	f := newFinalizingFixture(t, "fin-reton", 0)
	got := f.revokeFailed(t, "")
	require.False(t, got.ParseDeletionRequest().Soft)

	f.srv.config.SoftDeleteRetention = time.Hour
	requireHardDeleted(t, f.s, f.agent.ID, f.del(t, ""))
}

// The retry's deleteFiles/removeBranch do not override the stored values:
// after a retry that fails its finalize again, the stored request is
// unchanged byte for byte.
func TestAgentDeleteFinalizingRetry_FilesAndBranchKeepStored(t *testing.T) {
	f := newFinalizingFixture(t, "fin-files", time.Hour)
	failFinalizeTimes(t, 2)
	first := f.requireFinalizingFailed(t, f.del(t, "?deleteFiles=false&removeBranch=false"), store.DeletionCodeFinalizeFailed)
	stored := first.ParseDeletionRequest()
	require.True(t, stored.Soft)
	require.False(t, stored.DeleteFiles)
	require.False(t, stored.RemoveBranch)

	// Defaults: deleteFiles=true, removeBranch=true.
	second := f.requireFinalizingFailed(t, f.del(t, ""), store.DeletionCodeFinalizeFailed)
	require.Equal(t, first.DeletionClaim+1, second.DeletionClaim, "the retry re-claimed the row")
	assert.Equal(t, first.DeletionRequest, second.DeletionRequest, "the stored request is unchanged")

	requireSoftDeleted(t, f.s, f.agent.ID, f.del(t, ""))
}

// The claim itself: re-claiming a lapsed finalizing row returns the stored
// request in the plan, whatever the retrying caller asked for, and leaves
// the stored column untouched.
func TestAgentDeleteFinalizingRetry_ClaimUsesStoredRequest(t *testing.T) {
	cases := []struct {
		name   string
		params agentDeleteParams
	}{
		{"force", agentDeleteParams{force: true, requestedBy: "retrier"}},
		{"files and branch", agentDeleteParams{deleteFiles: true, removeBranch: true, requestedBy: "retrier"}},
		{"all", agentDeleteParams{deleteFiles: true, removeBranch: true, force: true, requestedBy: "retrier"}},
	}
	for i, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFinalizingFixture(t, "fin-claim-"+string(rune('a'+i)), time.Hour)
			before := f.revokeFailed(t, "?deleteFiles=false&removeBranch=false")
			stored := before.ParseDeletionRequest()
			require.True(t, stored.Soft)
			require.NotEqual(t, "retrier", stored.RequestedBy)

			logs := captureLifecycleLog(f.srv)
			plan, err := f.srv.claimAgentDeletion(context.Background(), f.agent.ID, tc.params)
			require.NoError(t, err)
			require.NotNil(t, plan, "the lapsed finalizing row is re-claimed")
			assert.True(t, plan.skipDispatch)
			assert.Equal(t, stored, plan.req, "the plan carries the stored request")
			assert.Empty(t, fallbackWarnings(logs), "a stored request is no fallback")

			after := mustGetAgent(t, f.s, f.agent.ID)
			assert.Equal(t, before.DeletionClaim+1, after.DeletionClaim)
			assert.Equal(t, store.DeletionStateFinalizing, after.DeletionState)
			assert.Equal(t, before.DeletionRequest, after.DeletionRequest, "the stored column is untouched")
		})
	}
}

// The claim itself, with no usable stored request: the plan and the stored
// column carry only the current retention decision and the retrier, never
// the retry's force, deleteFiles or removeBranch.
func TestAgentDeleteFinalizingRetry_ClaimFallbackIgnoresRetryParams(t *testing.T) {
	cases := []struct {
		name      string
		retention time.Duration
		wantSoft  bool
	}{
		{"retention on", time.Hour, true},
		{"retention off", 0, false},
	}
	for i, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFinalizingFixture(t, "fin-claimfb-"+string(rune('a'+i)), tc.retention)
			got := f.revokeFailed(t, "")
			claim := got.DeletionClaim
			emptyStoredRequest(t, f.s, got)

			logs := captureLifecycleLog(f.srv)
			plan, err := f.srv.claimAgentDeletion(context.Background(), f.agent.ID,
				agentDeleteParams{deleteFiles: true, removeBranch: true, force: true, requestedBy: "retrier"})
			require.NoError(t, err)
			require.NotNil(t, plan, "the lapsed finalizing row is re-claimed")
			warnings := fallbackWarnings(logs)
			if assert.Len(t, warnings, 1, "one fallback warning per confirmed claim") {
				assert.Contains(t, warnings[0], fmt.Sprintf("claim=%d", claim+1))
			}
			assert.True(t, plan.skipDispatch)
			want := store.DeletionRequestInfo{Soft: tc.wantSoft, RequestedBy: "retrier"}
			assert.Equal(t, want, plan.req, "the plan carries the fallback request")

			wantJSON, err := json.Marshal(want)
			require.NoError(t, err)
			after := mustGetAgent(t, f.s, f.agent.ID)
			assert.Equal(t, claim+1, after.DeletionClaim)
			assert.Equal(t, string(wantJSON), after.DeletionRequest, "the fallback request is stored")
		})
	}
}

// The fallback warning is logged only for a confirmed claim: when another
// claim takes the row between this claim's write and its re-read, the claim
// is lost and nothing is logged.
func TestAgentDeleteFinalizingRetry_FallbackNotLoggedForLostClaim(t *testing.T) {
	f := newFinalizingFixture(t, "fin-lostlog", time.Hour)
	got := f.revokeFailed(t, "")
	emptyStoredRequest(t, f.s, got)

	// Right after the claim's write, a newer claim takes the row.
	var once sync.Once
	f.hooks.afterDeletionWrite = func(_ store.DeletionPredicate, n int) {
		if n != 1 {
			return
		}
		once.Do(func() {
			mine := got.DeletionClaim + 1
			bumped, err := f.s.UpdateAgentDeletion(context.Background(), f.agent.ID,
				store.DeletionPredicate{Claim: &mine}, store.DeletionFields{BumpClaim: true})
			require.NoError(t, err)
			require.Equal(t, 1, bumped)
		})
	}

	logs := captureLifecycleLog(f.srv)
	plan, err := f.srv.claimAgentDeletion(context.Background(), f.agent.ID,
		agentDeleteParams{force: true, requestedBy: "retrier"})
	require.NoError(t, err)
	require.Nil(t, plan, "the claim was lost")
	assert.Equal(t, got.DeletionClaim+2, mustGetAgent(t, f.s, f.agent.ID).DeletionClaim)
	assert.Empty(t, fallbackWarnings(logs), "a lost claim logs no fallback warning")
}

// claimHardAndRun takes a hard claim (as the test identity purge does) and
// runs the engine to its end; it returns the plan.
func (f *finalizingFixture) claimHardAndRun(t *testing.T) *agentDeletionPlan {
	t.Helper()
	plan, err := f.srv.claimAgentDeletion(context.Background(), f.agent.ID,
		agentDeleteParams{deleteFiles: true, removeBranch: true, hard: true, requestedBy: "purge"})
	require.NoError(t, err)
	require.NotNil(t, plan)
	select {
	case out := <-f.srv.runAgentDeletion(context.Background(), plan):
		require.Equal(t, deletionOutcomeDeleted, out.kind, "%+v", out)
	case <-time.After(30 * time.Second):
		t.Fatal("the delete engine did not finish")
	}
	return plan
}

// A fresh hard claim is hard even with retention on.
func TestAgentDeleteFinalizingRetry_FreshHardClaimIsHard(t *testing.T) {
	f := newFinalizingFixture(t, "fin-hard-fresh", time.Hour)
	plan := f.claimHardAndRun(t)
	assert.False(t, plan.skipDispatch)
	assert.False(t, plan.req.Soft, "a hard claim is never soft")
	assert.True(t, agentGone(t, f.s, f.agent.ID), "hard-deleted")
}

// A hard claim on a finalizing row with no usable stored request falls
// back to hard, even with retention on.
func TestAgentDeleteFinalizingRetry_HardFallbackIsHard(t *testing.T) {
	f := newFinalizingFixture(t, "fin-hard-fb", time.Hour)
	got := f.revokeFailed(t, "")
	emptyStoredRequest(t, f.s, got)

	logs := captureLifecycleLog(f.srv)
	plan, err := f.srv.claimAgentDeletion(context.Background(), f.agent.ID,
		agentDeleteParams{deleteFiles: true, removeBranch: true, hard: true, requestedBy: "purge"})
	require.NoError(t, err)
	require.NotNil(t, plan)
	assert.True(t, plan.skipDispatch)
	want := store.DeletionRequestInfo{Soft: false, RequestedBy: "purge"}
	assert.Equal(t, want, plan.req, "the fallback honors hard")
	warnings := fallbackWarnings(logs)
	if assert.Len(t, warnings, 1, "one fallback warning per confirmed claim") {
		assert.Contains(t, warnings[0], fmt.Sprintf("claim=%d", got.DeletionClaim+1))
	}
	wantJSON, err := json.Marshal(want)
	require.NoError(t, err)
	assert.Equal(t, string(wantJSON), mustGetAgent(t, f.s, f.agent.ID).DeletionRequest, "the fallback request is stored")

	select {
	case out := <-f.srv.runAgentDeletion(context.Background(), plan):
		require.Equal(t, deletionOutcomeDeleted, out.kind, "%+v", out)
	case <-time.After(30 * time.Second):
		t.Fatal("the delete engine did not finish")
	}
	assert.True(t, agentGone(t, f.s, f.agent.ID), "hard-deleted")
}

// A hard claim on a finalizing row keeps the stored soft request: teardown
// ran under it, so the finish stays soft (ptone/scion#4183).
func TestAgentDeleteFinalizingRetry_HardKeepsStoredSoft(t *testing.T) {
	f := newFinalizingFixture(t, "fin-hard-stored", time.Hour)
	stored := f.revokeFailed(t, "").ParseDeletionRequest()
	require.True(t, stored.Soft)

	plan := f.claimHardAndRun(t)
	assert.True(t, plan.skipDispatch)
	assert.Equal(t, stored, plan.req, "the stored request is kept")
	require.False(t, agentGone(t, f.s, f.agent.ID), "the stored soft delete keeps the row")
	assert.False(t, mustGetAgent(t, f.s, f.agent.ID).DeletedAt.IsZero(), "soft-deleted")
}

// A finalizing row with no usable stored request falls back to the current
// retention setting (and an incomplete create is always hard); the retry's
// force is never used.
func TestAgentDeleteFinalizingRetry_MissingRequestFallback(t *testing.T) {
	cases := []struct {
		name       string
		retention  time.Duration
		stored     string
		incomplete bool
		retry      string
		wantSoft   bool
	}{
		{"empty, retention on, force retry", time.Hour, "", false, "?force=true", true},
		{"malformed, retention on, force retry", time.Hour, "{", false, "?force=true", true},
		{"empty, retention off", 0, "", false, "", false},
		{"empty, incomplete create, retention on", time.Hour, "", true, "", false},
	}
	for i, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFinalizingFixture(t, "fin-miss-"+string(rune('a'+i)), tc.retention)
			if tc.incomplete {
				f.agent, _ = launchingAgent(t, f.s, "fin-miss-ic", state.PhaseProvisioning, 5*time.Minute)
			}
			got := f.revokeFailed(t, "")
			if tc.incomplete {
				require.True(t, got.IsIncompleteCreate(), "precondition: an incomplete create")
			}

			// Replace the stored request, pinned to the failed claim.
			claim := got.DeletionClaim
			n, err := f.s.UpdateAgentDeletion(context.Background(), f.agent.ID,
				store.DeletionPredicate{Claim: &claim, States: []string{store.DeletionStateFinalizing}},
				store.DeletionFields{Request: &tc.stored})
			require.NoError(t, err)
			require.Equal(t, 1, n)

			rec := f.del(t, tc.retry)
			if tc.wantSoft {
				requireSoftDeleted(t, f.s, f.agent.ID, rec)
			} else {
				requireHardDeleted(t, f.s, f.agent.ID, rec)
			}
		})
	}
}

// Fresh claims are unchanged: a failed (rolled back) row and a lapsed
// deleting row both derive a new request, so a force retry hard-deletes an
// originally soft delete.
func TestAgentDeleteFinalizingRetry_FreshClaimsRederive(t *testing.T) {
	t.Run("failed row", func(t *testing.T) {
		f := newFinalizingFixture(t, "fin-fresh-failed", time.Hour)
		f.disp.setFn(func(context.Context, *store.Agent) error { return errors.New("broker boom") })
		rec := f.del(t, "")
		require.Equal(t, http.StatusBadGateway, rec.Code, rec.Body.String())
		got := mustGetAgent(t, f.s, f.agent.ID)
		require.Equal(t, store.DeletionStateFailed, got.DeletionState)
		require.True(t, got.ParseDeletionRequest().Soft)

		f.disp.setFn(nil)
		requireHardDeleted(t, f.s, f.agent.ID, f.del(t, "?force=true"))
	})
	t.Run("lapsed deleting row", func(t *testing.T) {
		f := newFinalizingFixture(t, "fin-fresh-deleting", time.Hour)
		// The deleting -> finalizing write errors, so the engine abandons
		// the claim with the row still deleting.
		f.hooks.setFailDeletionWrite(func(set store.DeletionFields) bool {
			return !set.BumpClaim && set.State != nil && *set.State == store.DeletionStateFinalizing
		})
		rec := f.del(t, "")
		require.NotEqual(t, http.StatusNoContent, rec.Code, rec.Body.String())
		got := mustGetAgent(t, f.s, f.agent.ID)
		require.Equal(t, store.DeletionStateDeleting, got.DeletionState)
		require.False(t, got.DeletionActive(time.Now()), "abandoned: lease lapsed")
		require.True(t, got.ParseDeletionRequest().Soft)

		requireHardDeleted(t, f.s, f.agent.ID, f.del(t, "?force=true"))
	})
}
