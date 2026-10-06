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
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// This file covers cursor rejection cases beyond the basic sort/dir/identity
// replay checks, the fit-vs-readable-count completeness edge case, and
// legacy mode's byte-identical behavior when sort-only parameters are
// supplied without "sort".

// TestListProjectAgentsSorted_CursorLabelReplayRejected proves: a cursor
// minted under one label filter is rejected when replayed under another
// (the label filter is part of the cursor binding), mirroring the existing
// phase-replay test.
func TestListProjectAgentsSorted_CursorLabelReplayRejected(t *testing.T) {
	f := sortedListSetup(t)
	for i := 0; i < 3; i++ {
		f.createAgent(t, fmt.Sprintf("labelreplay-%d", i), string(state.PhaseStopped), map[string]string{"team": "a"})
	}

	rec := doRequestAsUser(t, f.srv, f.owner, http.MethodGet, f.listPath("sort=updated&dir=desc&limit=1&label=team=a"), nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	resp := mustDecodeListAgentsResponse(t, rec.Body)
	require.NotEmpty(t, resp.NextCursor)

	rec = doRequestAsUser(t, f.srv, f.owner, http.MethodGet,
		f.listPath("sort=updated&dir=desc&limit=1&label=team=b&cursor="+url.QueryEscape(resp.NextCursor)), nil)
	assert.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())

	// Also: the same cursor with no label at all must be rejected too.
	rec = doRequestAsUser(t, f.srv, f.owner, http.MethodGet,
		f.listPath("sort=updated&dir=desc&limit=1&cursor="+url.QueryEscape(resp.NextCursor)), nil)
	assert.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
}

// TestListProjectAgentsSorted_CursorCrossProjectRejected proves: a cursor
// minted on project A is rejected when replayed against project B, since
// the binding's endpoint string includes the project ID.
func TestListProjectAgentsSorted_CursorCrossProjectRejected(t *testing.T) {
	f := sortedListSetup(t)
	ctx := context.Background()
	for i := 0; i < 3; i++ {
		f.createAgent(t, fmt.Sprintf("crossproj-%d", i), string(state.PhaseStopped), nil)
	}

	other := &store.Project{
		ID: tid("sl-crossproj-other"), Name: "Other", Slug: "sl-crossproj-other",
		OwnerID: f.owner.ID, CreatedBy: f.owner.ID, Created: time.Now(), Updated: time.Now(),
	}
	require.NoError(t, f.store.CreateProject(ctx, other))
	f.srv.seedProjectCreatorMembership(ctx, other)
	createTestUserWithProjectRole(t, f.store, f.owner.ID, f.owner.Email, other.ID, store.ProjectRoleOwner)
	// At least one agent in the other project so the request is otherwise valid.
	otherAgent := &store.Agent{
		ID: tid("sl-crossproj-agent"), Slug: "crossproj-agent", Name: "crossproj-agent",
		ProjectID: other.ID, Phase: string(state.PhaseStopped), CreatedBy: f.owner.ID, OwnerID: f.owner.ID,
	}
	require.NoError(t, f.store.CreateAgent(ctx, otherAgent))

	rec := doRequestAsUser(t, f.srv, f.owner, http.MethodGet, f.listPath("sort=updated&dir=desc&limit=1"), nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	resp := mustDecodeListAgentsResponse(t, rec.Body)
	require.NotEmpty(t, resp.NextCursor)

	otherPath := "/api/v1/projects/" + other.ID + "/agents?sort=updated&dir=desc&limit=1&cursor=" + url.QueryEscape(resp.NextCursor)
	rec = doRequestAsUser(t, f.srv, f.owner, http.MethodGet, otherPath, nil)
	assert.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
}

// TestListProjectAgentsSorted_TamperedCursorRejected proves: a tampered v2
// cursor byte is rejected at the hub (HTTP) level, not just the store codec
// level.
func TestListProjectAgentsSorted_TamperedCursorRejected(t *testing.T) {
	f := sortedListSetup(t)
	for i := 0; i < 3; i++ {
		f.createAgent(t, fmt.Sprintf("tamper-%d", i), string(state.PhaseStopped), nil)
	}

	rec := doRequestAsUser(t, f.srv, f.owner, http.MethodGet, f.listPath("sort=updated&dir=desc&limit=1"), nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	resp := mustDecodeListAgentsResponse(t, rec.Body)
	require.NotEmpty(t, resp.NextCursor)

	tampered := "X" + resp.NextCursor[1:]
	rec = doRequestAsUser(t, f.srv, f.owner, http.MethodGet,
		f.listPath("sort=updated&dir=desc&limit=1&cursor="+url.QueryEscape(tampered)), nil)
	assert.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
}

// TestListProjectAgentsSorted_MalformedCursor_NoSQLBeforeRejection proves: a
// malformed v2 cursor must be rejected before any COUNT, member read or
// decision -- the store and authz spies record nothing beyond (in this
// case, not even) the agent.list gate.
func TestListProjectAgentsSorted_MalformedCursor_NoSQLBeforeRejection(t *testing.T) {
	f, counting, fault := sortedListSetupWithFault(t, newCountingAgentStore)
	f.createAgent(t, "malformed-precheck", string(state.PhaseStopped), nil)

	fault.Arm()

	emitter := &recordingDecisionAuditEmitter{}
	f.srv.authzService.SetDecisionAuditEmitter(emitter)

	rec := doRequestAsUser(t, f.srv, f.owner, http.MethodGet, f.listPath("sort=updated&cursor=not-a-valid-v2-cursor"), nil)
	require.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())

	assert.Equal(t, 0, counting.countAgentsCalls, "no COUNT before the cursor is validated")
	assert.Equal(t, 0, counting.membersCalls, "no member read before the cursor is validated")
	assert.Len(t, emitter.records, 1, "only the agent.list gate decision runs before cursor validation")
}

// --- fit vs. readable count: n = fit+1 with R <= fit (project user path) ----

// TestListProjectAgentsSorted_Fit_IncompleteEvenWhenReadableAtOrBelowFit
// proves: completeness is decided on the candidate count n, not the readable
// count R -- even when every readable agent would fit, a
// candidate pool above fit must still page.
func TestListProjectAgentsSorted_Fit_IncompleteEvenWhenReadableAtOrBelowFit(t *testing.T) {
	f := sortedListSetup(t)
	ctx := context.Background()

	caller := &store.User{
		ID: tid("sl-fitgap-caller"), Email: "sl-fitgap@test.com", DisplayName: "Caller",
		Role: store.UserRoleMember, Status: "active",
	}
	require.NoError(t, f.store.CreateUser(ctx, caller))
	ensureHubMembership(ctx, f.store, caller.ID)
	grantProjectListOnly(t, f.store, caller.ID, f.project.ID, "sl-fitgap-role")

	const n, fit = 6, 5 // R = 5 (caller owns 5), n = 6 > fit
	f.createAgentsBulk(t, n, "fitgap", string(state.PhaseStopped), func(i int) string {
		if i < fit {
			return caller.ID
		}
		return f.owner.ID
	})

	rec := doRequestAsUser(t, f.srv, caller, http.MethodGet, f.listPath(fmt.Sprintf("sort=updated&fit=%d&limit=%d", fit, fit)), nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	resp := mustDecodeListAgentsResponse(t, rec.Body)
	require.NotNil(t, resp.Complete)
	assert.False(t, *resp.Complete, "n=6 > fit=5 must page even though R=5 <= fit")
	// The paged response still serves exactly the readable set here (R=5
	// fits in one page of limit=5), so no cursor is needed -- the point
	// is complete=false despite R<=fit, not that a cursor must exist.
	assert.Equal(t, fit, resp.TotalCount)
	assert.Len(t, resp.Agents, fit)
}

// --- legacy mode ignores fit/stats/dir, byte-identical apart from serverTime ---

// serverTimeJSONRe matches the "serverTime":"..." field in a
// ListAgentsResponse's JSON encoding, so rawBodyWithoutServerTime can blank
// it out for an exact byte comparison of everything else.
var serverTimeJSONRe = regexp.MustCompile(`"serverTime":"[^"]*"`)

// rawBodyWithoutServerTime returns rec's raw response body with the
// serverTime value blanked out, for an exact byte-for-byte comparison
// against another response.
func rawBodyWithoutServerTime(rec *httptest.ResponseRecorder) []byte {
	return serverTimeJSONRe.ReplaceAll(rec.Body.Bytes(), []byte(`"serverTime":""`))
}

// TestListProjectAgentsLegacy_IgnoresFitStatsDir_ByteIdentical pins the
// legacy contract: without "sort", fit/stats/dir are silently ignored and
// the response is byte-identical to the same request without them, apart
// from serverTime. This covers more than the original version: only the
// project endpoint, only valid values, no cursor in play, and a
// decoded-map compare rather than raw bytes. This version adds invalid
// values (dir=sideways, fit=0, and others), a cursor already in play (so
// the emitted nextCursor/binding is part of what must match), raw byte
// comparison, and the global endpoint (whose legacy path P1b leaves
// unchanged, so cheap to add).
func TestListProjectAgentsLegacy_IgnoresFitStatsDir_ByteIdentical(t *testing.T) {
	f := sortedListSetup(t)
	f.createAgent(t, "legacy-a", string(state.PhaseRunning), nil)
	f.createAgent(t, "legacy-b", string(state.PhaseStopped), nil)
	// A third agent: with only 2 agents, the project_endpoint_cursor_present
	// sub-case's page-2 responses (the ones actually compared) were always
	// the *last* page, so neither ever had a nextCursor, even though its
	// comment claimed the emitted nextCursor/binding was part of the byte
	// comparison. A 3rd agent makes page 2 non-terminal, so it carries a
	// real nextCursor too.
	f.createAgent(t, "legacy-c", string(state.PhaseStopped), nil)

	// Valid values (the original test's case), plus invalid shapes such as
	// dir=sideways, fit=0, plus two more unparsable ones -- all of these
	// would be 400s in sorted mode, and must instead be
	// silently ignored here, exactly like the valid case.
	extras := []string{
		"fit=500&stats=1&dir=asc",
		"dir=sideways",
		"fit=0",
		"fit=abc&stats=yes",
	}

	t.Run("project_endpoint_no_cursor", func(t *testing.T) {
		base := doRequestAsUser(t, f.srv, f.owner, http.MethodGet, f.listPath(""), nil)
		require.Equal(t, http.StatusOK, base.Code, base.Body.String())
		baseBody := rawBodyWithoutServerTime(base)

		for _, extra := range extras {
			t.Run(extra, func(t *testing.T) {
				rec := doRequestAsUser(t, f.srv, f.owner, http.MethodGet, f.listPath(extra), nil)
				require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
				assert.Equal(t, string(baseBody), string(rawBodyWithoutServerTime(rec)),
					"project endpoint: fit/stats/dir (%s) without sort must be silently ignored, byte-identical apart from serverTime", extra)
			})
		}
	})

	t.Run("project_endpoint_cursor_present", func(t *testing.T) {
		// limit=1 over 3 agents: page 1 carries a cursor into page 2, and
		// page 2 (the one actually compared below) is *not* the last page
		// either, so it carries its own nextCursor too.
		page1 := doRequestAsUser(t, f.srv, f.owner, http.MethodGet, f.listPath("limit=1"), nil)
		require.Equal(t, http.StatusOK, page1.Code, page1.Body.String())
		resp1 := mustDecodeListAgentsResponse(t, page1.Body)
		require.NotEmpty(t, resp1.NextCursor, "need a cursor in play for this sub-case")
		cursorQS := "cursor=" + url.QueryEscape(resp1.NextCursor)

		base := doRequestAsUser(t, f.srv, f.owner, http.MethodGet, f.listPath("limit=1&"+cursorQS), nil)
		require.Equal(t, http.StatusOK, base.Code, base.Body.String())
		baseResp := mustDecodeListAgentsResponse(t, base.Body)
		require.NotEmpty(t, baseResp.NextCursor,
			"page 2 must itself emit a nextCursor, or the byte comparison below never actually exercises one")
		baseBody := rawBodyWithoutServerTime(base)

		for _, extra := range extras {
			t.Run(extra, func(t *testing.T) {
				rec := doRequestAsUser(t, f.srv, f.owner, http.MethodGet, f.listPath("limit=1&"+cursorQS+"&"+extra), nil)
				require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
				assert.Equal(t, string(baseBody), string(rawBodyWithoutServerTime(rec)),
					"project endpoint with a cursor already in play: fit/stats/dir (%s) must still be ignored -- including the emitted nextCursor/binding, which is part of this byte comparison", extra)
			})
		}
	})

	t.Run("global_endpoint", func(t *testing.T) {
		globalBase := "/api/v1/agents?projectId=" + f.project.ID
		base := doRequestAsUser(t, f.srv, f.owner, http.MethodGet, globalBase, nil)
		require.Equal(t, http.StatusOK, base.Code, base.Body.String())
		baseBody := rawBodyWithoutServerTime(base)

		for _, extra := range extras {
			t.Run(extra, func(t *testing.T) {
				rec := doRequestAsUser(t, f.srv, f.owner, http.MethodGet, globalBase+"&"+extra, nil)
				require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
				assert.Equal(t, string(baseBody), string(rawBodyWithoutServerTime(rec)),
					"global endpoint: fit/stats/dir (%s) must be ignored too -- P1b does not touch this endpoint's legacy path", extra)
			})
		}
	})
}
