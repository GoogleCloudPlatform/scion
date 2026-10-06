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
	"fmt"
	"net/http"
	"net/url"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/store/agentsort"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// globalSortedFixture is a hub-admin bound project-admin on the one project
// every test agent lives in, for the global endpoint's sorted-mode tests.
// Every row is then in scope and fully readable with every action granted,
// and each page costs 9 decisions per returned row plus 4 scope-capability
// decisions, all through real authzService decisions.
//
// The caller is deliberately store.UserRoleMember + SystemRoleHubAdmin, not
// UserRoleAdmin + SystemRoleSuperAdmin: authorizeAgentMessage's super-admin
// bypass (authorize_message.go) keys on the flat User.Role being "admin" and
// short-circuits ComputeMessageability to zero decisions. For the same
// reason the project role is project-admin, not project-owner:
// authorizeUserToAgent's project-owner bypass would also zero out
// messageability's decision cost.
type globalSortedFixture struct {
	srv     *Server
	store   store.Store
	admin   *store.User
	project *store.Project
}

func globalSortedSetup(t *testing.T) *globalSortedFixture {
	t.Helper()
	srv, s := testServer(t)
	ctx := context.Background()

	adminID := tid("sg-admin")
	createTestUserWithRole(t, s, adminID, "sg-admin@test.com", store.UserRoleMember, store.SystemRoleHubAdmin)
	admin, err := s.GetUser(ctx, adminID)
	require.NoError(t, err)

	project := &store.Project{
		ID: tid("sg-project"), Name: "Sorted Global Project", Slug: "sg-project",
		OwnerID: adminID, CreatedBy: adminID, Created: time.Now(), Updated: time.Now(),
	}
	require.NoError(t, s.CreateProject(ctx, project))
	createTestUserWithProjectRole(t, s, adminID, "sg-admin@test.com", project.ID, store.ProjectRoleAdmin)

	return &globalSortedFixture{srv: srv, store: s, admin: admin, project: project}
}

func (f *globalSortedFixture) listPath(query string) string {
	p := "/api/v1/agents"
	if query != "" {
		p += "?" + query
	}
	return p
}

func (f *globalSortedFixture) createAgent(t *testing.T, slug, phase string) *store.Agent {
	t.Helper()
	a := &store.Agent{
		ID: tid("sg-agent-" + slug), Slug: slug, Name: slug,
		ProjectID: f.project.ID, Phase: phase,
		CreatedBy: f.admin.ID, OwnerID: f.admin.ID,
	}
	require.NoError(t, f.store.CreateAgent(context.Background(), a))
	return a
}

// createAgentsBulk inserts n agents inside one transaction (fast for the
// 500+ row sizes the decision-count tests below need), mirroring
// sortedListFixture.createAgentsBulk in agent_sorted_project_list_test.go.
func (f *globalSortedFixture) createAgentsBulk(t *testing.T, n int, slugPrefix, phase string) []*store.Agent {
	t.Helper()
	agents := make([]*store.Agent, n)
	err := f.store.WithTx(context.Background(), func(tx store.Store) error {
		for i := 0; i < n; i++ {
			slug := fmt.Sprintf("%s-%d", slugPrefix, i)
			a := &store.Agent{
				ID: tid("sg-bulk-" + slug), Slug: slug, Name: slug,
				ProjectID: f.project.ID, Phase: phase,
				CreatedBy: f.admin.ID, OwnerID: f.admin.ID,
			}
			if err := tx.CreateAgent(context.Background(), a); err != nil {
				return err
			}
			agents[i] = a
		}
		return nil
	})
	require.NoError(t, err)
	return agents
}

// --- parameter validation -------------------------------------------------

func TestListAgentsSorted_InvalidParams(t *testing.T) {
	f := globalSortedSetup(t)
	f.createAgent(t, "a1", "stopped")

	cases := []struct {
		name  string
		query string
	}{
		{"invalid sort value", "sort=bogus"},
		{"invalid dir", "sort=updated&dir=sideways"},
		{"invalid dir with sort=created", "sort=created&dir=sideways"},
		{"fit with cursor", "sort=updated&fit=10&cursor=AAAA"},
		{"fit too large", "sort=updated&fit=501"},
		{"fit zero", "sort=updated&fit=0"},
		{"fit below limit", "sort=updated&fit=5&limit=10"},
		{"malformed cursor", "sort=updated&cursor=not-valid-base64!!"},
		{"legacy-shaped cursor in sorted mode", "sort=updated&cursor=MjAyNi0wMS0wMVQwMDowMDowMFosYWJj"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := doRequestAsUser(t, f.srv, f.admin, http.MethodGet, f.listPath(tc.query), nil)
			assert.Equal(t, http.StatusBadRequest, rec.Code, "query=%q body=%s", tc.query, rec.Body.String())
		})
	}
}

// TestListAgentsSorted_InvalidCursorSameEnvelope pins that the global
// endpoint's sorted-mode 400 for a malformed cursor uses the same error
// envelope as the legacy invalid-cursor 400, and pins the legacy body itself
// byte for byte against a golden string.
func TestListAgentsSorted_InvalidCursorSameEnvelope(t *testing.T) {
	f := globalSortedSetup(t)

	legacyRec := doRequestAsUser(t, f.srv, f.admin, http.MethodGet, f.listPath("cursor=not-valid-base64!!"), nil)
	require.Equal(t, http.StatusBadRequest, legacyRec.Code)
	assert.Equal(t, legacyInvalidCursorBody, legacyRec.Body.String(), "the legacy invalid-cursor body must not change")

	sortedRec := doRequestAsUser(t, f.srv, f.admin, http.MethodGet, f.listPath("sort=updated&cursor=not-valid-base64!!"), nil)
	require.Equal(t, http.StatusBadRequest, sortedRec.Code)

	var legacyBody, sortedBody struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	require.NoError(t, json.Unmarshal(legacyRec.Body.Bytes(), &legacyBody))
	require.NoError(t, json.Unmarshal(sortedRec.Body.Bytes(), &sortedBody))
	// Same envelope (code invalid_request, an "invalid cursor" message), but
	// not the same message: the legacy decoder's message includes the
	// underlying decode error, while the sorted path's is the fixed string
	// "invalid cursor".
	assert.Equal(t, legacyBody.Error.Code, sortedBody.Error.Code)
	assert.Contains(t, legacyBody.Error.Message, "invalid cursor")
	assert.Equal(t, "invalid cursor", sortedBody.Error.Message)
}

// legacyInvalidCursorBody is the exact legacy 400 body for
// cursor=not-valid-base64!! on the global endpoint.
const legacyInvalidCursorBody = `{"error":{"code":"invalid_request","message":"invalid cursor: illegal base64 data at input byte 16"}}` + "\n"

// TestListAgentsSorted_DirValidatedBeforeSort pins the validation order:
// with both sort and dir invalid, the error reports dir.
func TestListAgentsSorted_DirValidatedBeforeSort(t *testing.T) {
	f := globalSortedSetup(t)
	rec := doRequestAsUser(t, f.srv, f.admin, http.MethodGet, f.listPath("sort=bogus&dir=sideways"), nil)
	require.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
	assert.Equal(t, "invalid dir", decodeErrorMessage(t, rec.Body.Bytes()))

	rec = doRequestAsUser(t, f.srv, f.admin, http.MethodGet, f.listPath("sort=bogus&dir=asc"), nil)
	require.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
	assert.Equal(t, "invalid sort", decodeErrorMessage(t, rec.Body.Bytes()))
}

func decodeErrorMessage(t *testing.T, body []byte) string {
	t.Helper()
	var e struct {
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	require.NoError(t, json.Unmarshal(body, &e))
	return e.Error.Message
}

// --- legacy byte identity --------------------------------------------------

// TestListAgentsSorted_LegacyIgnoresSortedParamsWithoutSort pins the rule
// that without "sort", fit/stats/dir (even invalid values) are ignored, and
// the response is byte-identical (apart from serverTime) to the same
// request without them. limit=1 with two agents makes the legacy nextCursor
// part of the comparison.
func TestListAgentsSorted_LegacyIgnoresSortedParamsWithoutSort(t *testing.T) {
	f := globalSortedSetup(t)
	f.createAgent(t, "a1", "stopped")
	f.createAgent(t, "a2", "running")

	base := doRequestAsUser(t, f.srv, f.admin, http.MethodGet, f.listPath("limit=1"), nil)
	require.Equal(t, http.StatusOK, base.Code)
	require.NotEmpty(t, mustDecodeListAgentsResponse(t, base.Body).NextCursor, "limit=1 over two agents must produce a legacy cursor to compare")

	extra := doRequestAsUser(t, f.srv, f.admin, http.MethodGet, f.listPath("limit=1&fit=500&stats=1&dir=asc"), nil)
	require.Equal(t, http.StatusOK, extra.Code)
	assertResponsesEqualIgnoringServerTime(t, base.Body.Bytes(), extra.Body.Bytes())

	invalid := doRequestAsUser(t, f.srv, f.admin, http.MethodGet, f.listPath("limit=1&dir=sideways&fit=0"), nil)
	require.Equal(t, http.StatusOK, invalid.Code, "dir/fit without sort must never 400")
	assertResponsesEqualIgnoringServerTime(t, base.Body.Bytes(), invalid.Body.Bytes())

	unlimited := doRequestAsUser(t, f.srv, f.admin, http.MethodGet, f.listPath(""), nil)
	require.Equal(t, http.StatusOK, unlimited.Code)
	withExtra := doRequestAsUser(t, f.srv, f.admin, http.MethodGet, f.listPath("fit=500&stats=1&dir=asc"), nil)
	require.Equal(t, http.StatusOK, withExtra.Code)
	assertResponsesEqualIgnoringServerTime(t, unlimited.Body.Bytes(), withExtra.Body.Bytes())
}

// assertResponsesEqualIgnoringServerTime compares two ListAgentsResponse
// JSON bodies for equality except the serverTime field.
func assertResponsesEqualIgnoringServerTime(t *testing.T, a, b []byte) {
	t.Helper()
	var am, bm map[string]interface{}
	require.NoError(t, json.Unmarshal(a, &am))
	require.NoError(t, json.Unmarshal(b, &bm))
	delete(am, "serverTime")
	delete(bm, "serverTime")
	aj, err := json.Marshal(am)
	require.NoError(t, err)
	bj, err := json.Marshal(bm)
	require.NoError(t, err)
	assert.JSONEq(t, string(aj), string(bj))
}

// --- sort order ----------------------------------------------------------

// TestListAgentsSorted_OrderMatchesAgentsortReference walks every page for
// both sort keys and both directions and asserts the concatenation matches
// the agentsort reference order computed from the agents' own returned
// Created/Updated/LastActivityEvent (not an assumption about CreateAgent's
// real-clock insertion timing, which can tie within a tick).
func TestListAgentsSorted_OrderMatchesAgentsortReference(t *testing.T) {
	f := globalSortedSetup(t)
	seeded := f.createAgentsBulk(t, 12, "ord", "stopped")

	for _, sortKey := range []string{agentsort.Created, agentsort.Updated} {
		for _, dir := range []string{agentsort.Asc, agentsort.Desc} {
			rows := make([]agentsort.Row, len(seeded))
			for i, a := range seeded {
				rows[i] = agentsort.KeyFor(sortKey, a.ID, a.Created, a.Updated, a.LastActivityEvent)
			}
			agentsort.SortRows(dir, rows)
			want := make([]string, len(rows))
			for i, row := range rows {
				want[i] = row.ID
			}

			var walked []string
			cursor := ""
			for i := 0; i < 20; i++ {
				q := fmt.Sprintf("sort=%s&dir=%s&limit=3", sortKey, dir)
				if cursor != "" {
					q += "&cursor=" + url.QueryEscape(cursor)
				}
				rec := doRequestAsUser(t, f.srv, f.admin, http.MethodGet, f.listPath(q), nil)
				require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
				resp := mustDecodeListAgentsResponse(t, rec.Body)
				assert.Equal(t, sortKey, resp.Sort)
				assert.Equal(t, dir, resp.Dir)
				for _, a := range resp.Agents {
					walked = append(walked, a.ID)
				}
				if resp.NextCursor == "" {
					break
				}
				cursor = resp.NextCursor
			}
			assert.Equal(t, want, walked, "sort=%s dir=%s", sortKey, dir)
		}
	}
}

// --- fit/complete ----------------------------------------------------------

func TestListAgentsSorted_FitComplete(t *testing.T) {
	f := globalSortedSetup(t)
	f.createAgentsBulk(t, 5, "fit", "stopped")

	rec := doRequestAsUser(t, f.srv, f.admin, http.MethodGet, f.listPath("sort=updated&fit=500&limit=500&phase=running"), nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	resp := mustDecodeListAgentsResponse(t, rec.Body)
	require.NotNil(t, resp.Complete)
	assert.True(t, *resp.Complete)
	assert.Empty(t, resp.NextCursor)
	assert.Len(t, resp.Agents, 5, "a complete fit response ignores the request's own phase filter")
}

func TestListAgentsSorted_FitPagedWhenOverFit(t *testing.T) {
	f := globalSortedSetup(t)
	f.createAgentsBulk(t, 6, "over", "stopped")

	rec := doRequestAsUser(t, f.srv, f.admin, http.MethodGet, f.listPath("sort=updated&fit=5&limit=5"), nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	resp := mustDecodeListAgentsResponse(t, rec.Body)
	require.NotNil(t, resp.Complete)
	assert.False(t, *resp.Complete, "n=fit+1 must never report complete:true, even though n is only one over fit")
	assert.NotEmpty(t, resp.NextCursor)
	assert.Len(t, resp.Agents, 5)
}

// --- stats -----------------------------------------------------------------

func TestListAgentsSorted_StatsIgnoresPhaseFilter(t *testing.T) {
	f := globalSortedSetup(t)
	f.createAgent(t, "run-1", "running")
	f.createAgent(t, "run-2", "running")
	f.createAgent(t, "stop-1", "stopped")

	rec := doRequestAsUser(t, f.srv, f.admin, http.MethodGet, f.listPath("sort=updated&fit=500&stats=1&phase=stopped"), nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	resp := mustDecodeListAgentsResponse(t, rec.Body)
	require.NotNil(t, resp.Stats)
	assert.Equal(t, 3, resp.Stats.Total)
	assert.Equal(t, 2, resp.Stats.Running)
	require.NotNil(t, resp.Stats.Agents)
	assert.Len(t, *resp.Stats.Agents, 3)
}

// TestListAgentsSorted_StatsOmitsAgentsAbove2000 pins the omission rule:
// above 2,000 candidates the stats block reads only the first 2,000, so
// total is a lower bound flagged by totalApproximate and stats.agents is
// nil (omitted from the JSON).
func TestListAgentsSorted_StatsOmitsAgentsAbove2000(t *testing.T) {
	f := globalSortedSetup(t)
	f.createAgentsBulk(t, 2001, "cap", "stopped")

	rec := doRequestAsUser(t, f.srv, f.admin, http.MethodGet, f.listPath("sort=updated&limit=1&fit=1&stats=1"), nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	var raw map[string]interface{}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &raw))
	statsRaw, ok := raw["stats"].(map[string]interface{})
	require.True(t, ok)
	assert.Equal(t, float64(2000), statsRaw["total"])
	assert.Equal(t, true, statsRaw["totalApproximate"])
	_, hasAgents := statsRaw["agents"]
	assert.False(t, hasAgents, "stats.agents must be omitted entirely above 2000, not an empty array")
}

// --- short-circuit echo -----------------------------------------------------

// TestListAgentsSorted_NoneScopeEchoesSortDir exercises the
// scopeResult.Scopes.IsNone() short-circuit (a user with no project
// bindings) rather than identity==nil directly (constructing a genuinely
// unauthenticated request through the full auth middleware is out of scope
// for this harness): both short circuits echo sort/dir, report
// complete: true, and report stats: {total: 0, running: 0, agents: []},
// sharing the same response-building code (sortedShortCircuitResponse), so
// this exercises the same response-building path.
func TestListAgentsSorted_NoneScopeEchoesSortDir(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()
	user := &store.User{
		ID: tid("sg-none-u"), Email: "sg-none@test.com",
		DisplayName: "No Scope User", Role: store.UserRoleMember, Status: "active",
	}
	require.NoError(t, s.CreateUser(ctx, user))
	// No hub membership, no project bindings: scope resolves to None.

	rec := doRequestAsUser(t, srv, user, http.MethodGet, "/api/v1/agents?sort=updated&dir=asc&fit=500&stats=1", nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	resp := mustDecodeListAgentsResponse(t, rec.Body)
	assert.Equal(t, "updated", resp.Sort)
	assert.Equal(t, "asc", resp.Dir)
	require.NotNil(t, resp.Complete)
	assert.True(t, *resp.Complete)
	require.NotNil(t, resp.Stats)
	assert.Equal(t, 0, resp.Stats.Total)
	require.NotNil(t, resp.Stats.Agents)
	assert.Empty(t, *resp.Stats.Agents)
	assert.Empty(t, resp.Agents)
}

// --- decision counts --------------------------------------------------------

// TestListAgentsSorted_PagedDecisionCount_Limit500 asserts the exact
// decision count of a full limit=500 page on the global endpoint: paged
// with 501 authorized agents, it costs one read decision per candidate for the
// readable count (501), the fill pass's reads up to the first row past the
// page (501), 9 decisions per returned row and 4 fixed scope-capability
// decisions.
func TestListAgentsSorted_PagedDecisionCount_Limit500(t *testing.T) {
	f := globalSortedSetup(t)
	f.createAgentsBulk(t, 501, "ceiling", "stopped")

	emitter := &recordingDecisionAuditEmitter{}
	f.srv.authzService.SetDecisionAuditEmitter(emitter)

	rec := doRequestAsUser(t, f.srv, f.admin, http.MethodGet, f.listPath("sort=updated&dir=desc&limit=500"), nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	resp := mustDecodeListAgentsResponse(t, rec.Body)
	assert.Len(t, resp.Agents, 500)
	assert.NotEmpty(t, resp.NextCursor)
	assert.Equal(t, 501, resp.TotalCount)

	assert.Len(t, emitter.records, 501+501+9*500+4, "count reads + fill reads + 9P+4 at P=500")
}

// TestListAgentsSorted_CompleteDecisionCount pins the complete-branch
// decision count (one read decision per candidate, 9 decisions per agent
// plus 4 fixed scope-capability decisions) at 25 agents.
func TestListAgentsSorted_CompleteDecisionCount(t *testing.T) {
	f := globalSortedSetup(t)
	f.createAgentsBulk(t, 25, "cx", "stopped")

	emitter := &recordingDecisionAuditEmitter{}
	f.srv.authzService.SetDecisionAuditEmitter(emitter)

	rec := doRequestAsUser(t, f.srv, f.admin, http.MethodGet, f.listPath("sort=updated&fit=500&limit=500"), nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	resp := mustDecodeListAgentsResponse(t, rec.Body)
	require.NotNil(t, resp.Complete)
	assert.True(t, *resp.Complete)

	assert.Len(t, emitter.records, 25+9*25+4)
}

// TestListAgentsSorted_IncompleteFitLimit1DecisionCount pins the home/graph
// probe cost: an incomplete fit request with limit=1 costs one read
// decision per candidate for the readable count, one 50-row fill batch, 9
// for the one returned row and 4 fixed scope-capability decisions.
func TestListAgentsSorted_IncompleteFitLimit1DecisionCount(t *testing.T) {
	f := globalSortedSetup(t)
	f.createAgentsBulk(t, 1200, "probe", "stopped")

	emitter := &recordingDecisionAuditEmitter{}
	f.srv.authzService.SetDecisionAuditEmitter(emitter)

	rec := doRequestAsUser(t, f.srv, f.admin, http.MethodGet, f.listPath("sort=updated&limit=1&fit=1"), nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	resp := mustDecodeListAgentsResponse(t, rec.Body)
	require.NotNil(t, resp.Complete)
	assert.False(t, *resp.Complete)
	assert.Len(t, resp.Agents, 1)

	assert.Len(t, emitter.records, 1200+50+13)
}
