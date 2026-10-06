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
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/store/agentsort"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// This file covers sorted-mode boundaries on the global endpoint and the
// project endpoint's agent-JWT path: fit and stats boundaries, decision
// costs of the stats read, agent-JWT filtering, ordering and cursor binding,
// race drops isolated to the full-row read, short-circuit validation of fit,
// and tampered global cursors.

// --- global endpoint: fit and stats boundaries -------------------------------

// An incomplete fit request with stats=1 costs one read decision per stats
// candidate, the paged cost of its one returned row (one read decision per
// candidate for the readable count, one 50-row fill batch, 9 capability
// decisions for the row) and 4 scope decisions.
func TestListAgentsSorted_StatsWithIncompleteFitDecisionCount(t *testing.T) {
	f := globalSortedSetup(t)
	f.createAgentsBulk(t, 1200, "statscost", "stopped")
	f.createAgent(t, "statscost-run", "running")

	emitter := &recordingDecisionAuditEmitter{}
	f.srv.authzService.SetDecisionAuditEmitter(emitter)

	rec := doRequestAsUser(t, f.srv, f.admin, http.MethodGet, f.listPath("sort=updated&limit=1&fit=500&stats=1"), nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	resp := mustDecodeListAgentsResponse(t, rec.Body)
	require.NotNil(t, resp.Complete)
	assert.False(t, *resp.Complete)
	assert.Len(t, resp.Agents, 1)
	require.NotNil(t, resp.Stats)
	assert.Equal(t, 1201, resp.Stats.Total)
	assert.Equal(t, 1, resp.Stats.Running)
	require.NotNil(t, resp.Stats.Agents)
	assert.Len(t, *resp.Stats.Agents, 1201)

	assert.Len(t, emitter.records, 1201+1201+50+13, "stats reads, count-pass reads, one fill batch, 9 for the row, 4 scope")
}

// failingStatsStore fails the global endpoint's stats read (the stats
// member read) and passes every other call through.
type failingStatsStore struct {
	store.Store
	statsCalls int
}

func (f *failingStatsStore) ListAgentMembers(ctx context.Context, filter store.AgentFilter, sort, dir string, max int) ([]store.AgentMember, error) {
	f.statsCalls++
	return nil, errors.New("stats read failed")
}

// A stats read error is an error response that costs no decisions at all:
// the stats read happens before any row is decided.
func TestListAgentsSorted_StatsReadErrorCostsNoDecisions(t *testing.T) {
	f := globalSortedSetup(t)
	f.createAgentsBulk(t, 6, "statserr", "stopped")

	for _, q := range []string{
		"sort=updated&limit=2&stats=1",         // paged
		"sort=updated&limit=5&fit=500&stats=1", // complete
		"sort=created&limit=2&fit=5&stats=1",   // incomplete fit, then paged
		"sort=created&dir=asc&limit=1&stats=1", // paged, other sort
	} {
		failing := &failingStatsStore{Store: f.store}
		f.srv.store = failing
		emitter := &recordingDecisionAuditEmitter{}
		f.srv.authzService.SetDecisionAuditEmitter(emitter)

		rec := doRequestAsUser(t, f.srv, f.admin, http.MethodGet, f.listPath(q), nil)
		assert.Equal(t, http.StatusInternalServerError, rec.Code, "query=%q body=%s", q, rec.Body.String())
		assert.NotContains(t, rec.Body.String(), `"agents"`, "query=%q: an error response carries no page", q)
		assert.Equal(t, 1, failing.statsCalls, "query=%q", q)
		assert.Empty(t, emitter.records, "query=%q: a stats read error must cost zero decisions", q)
	}
	f.srv.store = f.store
}

// A candidate set of exactly fit rows is complete: the store's limit+1 probe
// finds no extra row.
func TestListAgentsSorted_FitCompleteAtExactlyFit(t *testing.T) {
	f := globalSortedSetup(t)
	f.createAgentsBulk(t, 5, "exact", "stopped")

	rec := doRequestAsUser(t, f.srv, f.admin, http.MethodGet, f.listPath("sort=created&fit=5&limit=5"), nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	resp := mustDecodeListAgentsResponse(t, rec.Body)
	require.NotNil(t, resp.Complete)
	assert.True(t, *resp.Complete, "n == fit must report complete:true")
	assert.Empty(t, resp.NextCursor)
	assert.Len(t, resp.Agents, 5)
	assert.Equal(t, 5, resp.TotalCount)
}

// A paged walk whose last page ends exactly at the last row carries no
// cursor on that page.
func TestListAgentsSorted_PageEndingAtLastRowHasNoCursor(t *testing.T) {
	f := globalSortedSetup(t)
	f.createAgentsBulk(t, 4, "lastpage", "stopped")

	rec := doRequestAsUser(t, f.srv, f.admin, http.MethodGet, f.listPath("sort=updated&limit=2"), nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	first := mustDecodeListAgentsResponse(t, rec.Body)
	require.Len(t, first.Agents, 2)
	require.NotEmpty(t, first.NextCursor)

	rec = doRequestAsUser(t, f.srv, f.admin, http.MethodGet, f.listPath(withCursor("sort=updated&limit=2", first.NextCursor)), nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	second := mustDecodeListAgentsResponse(t, rec.Body)
	assert.Len(t, second.Agents, 2)
	assert.Empty(t, second.NextCursor, "the page holding the last row must carry no cursor")
}

// stats.agents is still present at exactly 2,000 counted agents.
func TestListAgentsSorted_StatsIncludesAgentsAtExactly2000(t *testing.T) {
	f := globalSortedSetup(t)
	f.createAgentsBulk(t, 2000, "cap2000", "stopped")

	rec := doRequestAsUser(t, f.srv, f.admin, http.MethodGet, f.listPath("sort=updated&limit=1&fit=1&stats=1"), nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var raw map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &raw))
	var stats struct {
		Total  int          `json:"total"`
		Agents *[][2]string `json:"agents"`
	}
	require.NoError(t, json.Unmarshal(raw["stats"], &stats))
	assert.Equal(t, 2000, stats.Total)
	require.NotNil(t, stats.Agents, "stats.agents must be present at exactly 2000")
	assert.Len(t, *stats.Agents, 2000)
}

// --- global endpoint: short-circuits validate fit ----------------------------

// An invalid fit (or fit with cursor, or fit below limit) is the same 400
// for an unauthenticated or None-scope caller as for a caller with scope,
// and a valid fit still reports complete: true on the short-circuits.
func TestListAgentsSorted_ShortCircuitsValidateFit(t *testing.T) {
	f := globalSortedSetup(t)
	f.createAgent(t, "sc-fit", "stopped")
	noScope := noScopeUser(t, f.store)

	for _, q := range []string{
		"sort=updated&fit=0",
		"sort=updated&fit=abc",
		"sort=updated&fit=501",
		"sort=updated&fit=2&limit=5",
		"sort=created&fit=5&cursor=abc",
	} {
		scoped := doRequestAsUser(t, f.srv, f.admin, http.MethodGet, f.listPath(q), nil)
		require.Equal(t, http.StatusBadRequest, scoped.Code, "scoped query=%q body=%s", q, scoped.Body.String())

		none := doRequestAsUser(t, f.srv, noScope, http.MethodGet, f.listPath(q), nil)
		require.Equal(t, http.StatusBadRequest, none.Code, "none-scope query=%q body=%s", q, none.Body.String())
		assert.Equal(t, scoped.Body.String(), none.Body.String(), "none-scope query=%q", q)

		unauth := listAgentsUnauthenticated(f.srv, q)
		require.Equal(t, http.StatusBadRequest, unauth.Code, "unauthenticated query=%q body=%s", q, unauth.Body.String())
		assert.Equal(t, scoped.Body.String(), unauth.Body.String(), "unauthenticated query=%q", q)
	}

	for name, rec := range map[string]interface {
		Bytes() []byte
	}{
		"none-scope":      doRequestAsUser(t, f.srv, noScope, http.MethodGet, f.listPath("sort=updated&fit=5&limit=5&stats=1"), nil).Body,
		"unauthenticated": listAgentsUnauthenticated(f.srv, "sort=updated&fit=5&limit=5&stats=1").Body,
	} {
		resp := mustDecodeListAgentsResponse(t, rec)
		require.NotNil(t, resp.Complete, name)
		assert.True(t, *resp.Complete, name)
		require.NotNil(t, resp.Stats, name)
		assert.Empty(t, resp.Agents, name)
	}
}

// --- global endpoint: tampered cursors ----------------------------------------

// tamperGlobalCursor rewrites one field of a v2 cursor (index 3 = K,
// 4 = created, 5 = id, 6 = binding) and re-encodes it.
func tamperGlobalCursor(t *testing.T, cursor string, field int, value string) string {
	t.Helper()
	raw, err := base64.URLEncoding.DecodeString(cursor)
	require.NoError(t, err)
	parts := strings.SplitN(string(raw), ",", 7)
	require.Len(t, parts, 7)
	parts[field] = value
	return base64.URLEncoding.EncodeToString([]byte(strings.Join(parts, ",")))
}

// nonCanonicalCursorIDs returns the cursor's own (valid) id in the four
// non-canonical spellings uuid.Parse accepts: urn:uuid: prefix, braced,
// undashed and uppercase. Each must be rejected like any malformed id.
func nonCanonicalCursorIDs(t *testing.T, cursor string) []string {
	t.Helper()
	raw, err := base64.URLEncoding.DecodeString(cursor)
	require.NoError(t, err)
	parts := strings.SplitN(string(raw), ",", 7)
	require.Len(t, parts, 7)
	id := parts[5]
	require.Len(t, id, 36, "cursor id must be a canonical UUID")
	forms := []string{
		"urn:uuid:" + id,
		"{" + id + "}",
		strings.ReplaceAll(id, "-", ""),
		strings.ToUpper(id),
	}
	for _, f := range forms {
		require.NotEqual(t, id, f, "non-canonical form must differ from the canonical id")
	}
	return forms
}

// scopedViewerFixture is a global-endpoint fixture with a caller who can see
// only f.project (6 agents) and a second project, invisible to that caller,
// holding 6 more agents owned by f.admin.
type scopedViewerFixture struct {
	*globalSortedFixture
	viewer   *store.User
	visible  map[string]bool
	hiddenID string // one agent id in the hidden project
}

func scopedViewerSetup(t *testing.T) *scopedViewerFixture {
	t.Helper()
	f := globalSortedSetup(t)
	ctx := context.Background()

	// A caller who can see only f.project.
	viewer := &store.User{
		ID: tid("sg-tamper-viewer"), Email: "sg-tamper-viewer@test.com", DisplayName: "Viewer",
		Role: store.UserRoleMember, Status: "active",
	}
	require.NoError(t, f.store.CreateUser(ctx, viewer))
	ensureHubMembership(ctx, f.store, viewer.ID)
	createTestUserWithProjectRole(t, f.store, viewer.ID, viewer.Email, f.project.ID, store.ProjectRoleAdmin)

	visible := map[string]bool{}
	for _, a := range f.createAgentsBulk(t, 6, "tamper-vis", "stopped") {
		visible[a.ID] = true
	}

	// Another project the viewer has no binding on.
	hidden := &store.Project{
		ID: tid("sg-tamper-hidden"), Name: "Hidden", Slug: "sg-tamper-hidden",
		OwnerID: f.admin.ID, CreatedBy: f.admin.ID, Created: time.Now(), Updated: time.Now(),
	}
	require.NoError(t, f.store.CreateProject(ctx, hidden))
	var hiddenID string
	for i := 0; i < 6; i++ {
		a := &store.Agent{
			ID: tid(fmt.Sprintf("sg-tamper-hidden-%d", i)), Slug: fmt.Sprintf("tamper-hidden-%d", i), Name: fmt.Sprintf("tamper-hidden-%d", i),
			ProjectID: hidden.ID, Phase: "stopped", CreatedBy: f.admin.ID, OwnerID: f.admin.ID,
		}
		require.NoError(t, f.store.CreateAgent(ctx, a))
		hiddenID = a.ID
	}

	return &scopedViewerFixture{globalSortedFixture: f, viewer: viewer, visible: visible, hiddenID: hiddenID}
}

// A v2 cursor's K, created and id can be altered within a valid binding,
// which moves the page position but can never widen the result beyond the
// caller's own visible set: the SQL scope predicate still applies. Altering
// the binding itself is a 400.
func TestListAgentsSorted_TamperedCursorCannotWidenScope(t *testing.T) {
	sf := scopedViewerSetup(t)
	f, viewer, visible, hiddenID := sf.globalSortedFixture, sf.viewer, sf.visible, sf.hiddenID

	for _, sortKey := range []string{agentsort.Updated, agentsort.Created} {
		for _, dir := range []string{agentsort.Asc, agentsort.Desc} {
			q := fmt.Sprintf("sort=%s&dir=%s&limit=2", sortKey, dir)
			cur := f.mintGlobalCursor(t, viewer, q)

			far := []string{
				"1970-01-01T00:00:00Z",
				"2999-12-31T23:59:59.999999999Z",
			}
			var tampered []string
			for _, ts := range far {
				tampered = append(tampered,
					tamperGlobalCursor(t, cur, 3, ts),
					tamperGlobalCursor(t, cur, 4, ts),
					tamperGlobalCursor(t, tamperGlobalCursor(t, cur, 3, ts), 4, ts))
			}
			tampered = append(tampered,
				tamperGlobalCursor(t, cur, 5, hiddenID),
				tamperGlobalCursor(t, cur, 5, "00000000-0000-0000-0000-000000000000"))

			for i, tc := range tampered {
				// Walk from the tampered position to the end: every row on
				// every page must be in the viewer's visible set.
				next := tc
				for pages := 0; next != ""; pages++ {
					require.Less(t, pages, 10)
					rec := doRequestAsUser(t, f.srv, viewer, http.MethodGet, f.listPath(withCursor(q, next)), nil)
					require.Equal(t, http.StatusOK, rec.Code, "%s tampered #%d: %s", q, i, rec.Body.String())
					resp := mustDecodeListAgentsResponse(t, rec.Body)
					for _, a := range resp.Agents {
						assert.True(t, visible[a.ID], "%s tampered #%d returned agent %s outside the caller's visible set", q, i, a.ID)
					}
					assert.Equal(t, len(visible), resp.TotalCount, "%s tampered #%d: totalCount stays the visible count", q, i)
					next = resp.NextCursor
				}
			}

			// A binding that does not match the request is a 400.
			adminCur := f.mintGlobalCursor(t, f.admin, q)
			for _, bad := range []string{
				tamperGlobalCursor(t, cur, 6, "not-the-binding"),
				tamperGlobalCursor(t, cur, 6, ""),
				adminCur, // another principal's binding
			} {
				rec := doRequestAsUser(t, f.srv, viewer, http.MethodGet, f.listPath(withCursor(q, bad)), nil)
				assert.Equal(t, http.StatusBadRequest, rec.Code, "%s: %s", q, rec.Body.String())
				assert.Equal(t, "invalid cursor", decodeErrorMessage(t, rec.Body.Bytes()))
			}
		}
	}
}

// stats=1 on the global endpoint is computed over the caller's own scope:
// agents in a project the caller cannot see are neither counted nor listed,
// on the fit path and on the paged path.
func TestListAgentsSorted_StatsScopedToVisibleSet(t *testing.T) {
	sf := scopedViewerSetup(t)

	for _, q := range []string{"sort=updated&fit=500&stats=1", "sort=updated&limit=2&stats=1"} {
		rec := doRequestAsUser(t, sf.srv, sf.viewer, http.MethodGet, sf.listPath(q), nil)
		require.Equal(t, http.StatusOK, rec.Code, "%s: %s", q, rec.Body.String())
		resp := mustDecodeListAgentsResponse(t, rec.Body)
		require.NotNil(t, resp.Stats, q)
		require.NotNil(t, resp.Stats.Agents, q)
		assert.Equal(t, len(sf.visible), resp.Stats.Total, "%s: stats.total must equal the visible count", q)
		assert.Len(t, *resp.Stats.Agents, len(sf.visible), q)
		for _, ip := range *resp.Stats.Agents {
			assert.True(t, sf.visible[ip[0]], "%s: stats.agents lists %s outside the caller's visible set", q, ip[0])
		}
		assert.NotContains(t, rec.Body.String(), sf.hiddenID, q)
	}
}

// A v2 cursor whose binding, sort and dir all match the request but whose id
// is not a canonical UUID (malformed, or a valid UUID in a non-canonical
// spelling) is rejected with the generic invalid-cursor 400, the same
// body a binding mismatch gets, before any SQL runs.
func TestListAgentsSorted_NonUUIDCursorIDRejected(t *testing.T) {
	sf := scopedViewerSetup(t)
	q := "sort=updated&dir=desc&limit=2"
	cur := sf.mintGlobalCursor(t, sf.viewer, q)

	ref := doRequestAsUser(t, sf.srv, sf.viewer, http.MethodGet, sf.listPath(withCursor(q, tamperGlobalCursor(t, cur, 6, "not-the-binding"))), nil)
	require.Equal(t, http.StatusBadRequest, ref.Code, ref.Body.String())

	ids := append([]string{"not-a-uuid", "", "1 OR 1=1"}, nonCanonicalCursorIDs(t, cur)...)
	for _, id := range ids {
		rec := doRequestAsUser(t, sf.srv, sf.viewer, http.MethodGet, sf.listPath(withCursor(q, tamperGlobalCursor(t, cur, 5, id))), nil)
		require.Equal(t, http.StatusBadRequest, rec.Code, "id %q: %s", id, rec.Body.String())
		assert.Equal(t, "invalid cursor", decodeErrorMessage(t, rec.Body.Bytes()), "id %q", id)
		assert.Equal(t, ref.Body.String(), rec.Body.String(), "id %q: body must match the generic invalid-cursor body", id)
	}
}

// The same non-UUID and non-canonical id check on the project endpoint, for a user caller and
// for an agent JWT.
func TestListProjectAgentsSorted_NonUUIDCursorIDRejected(t *testing.T) {
	f := sortedListSetup(t)
	self := f.createAgent(t, "uuid-self", string(state.PhaseStopped), nil)
	f.createAgent(t, "uuid-sib-1", string(state.PhaseStopped), nil)
	f.createAgent(t, "uuid-sib-2", string(state.PhaseStopped), nil)
	tok := f.agentJWTFor(t, self.ID)
	q := "sort=updated&dir=desc&limit=1"

	callers := map[string]func(path string) *httptest.ResponseRecorder{
		"user": func(path string) *httptest.ResponseRecorder {
			return doRequestAsUser(t, f.srv, f.owner, http.MethodGet, path, nil)
		},
		"agent-jwt": func(path string) *httptest.ResponseRecorder {
			return doRequestWithAgentToken(t, f.srv, http.MethodGet, path, nil, tok)
		},
	}
	for name, call := range callers {
		first := call(f.listPath(q))
		require.Equal(t, http.StatusOK, first.Code, "%s: %s", name, first.Body.String())
		cur := mustDecodeListAgentsResponse(t, first.Body).NextCursor
		require.NotEmpty(t, cur, name)

		ref := call(f.listPath(withCursor(q, tamperGlobalCursor(t, cur, 6, "not-the-binding"))))
		require.Equal(t, http.StatusBadRequest, ref.Code, "%s: %s", name, ref.Body.String())

		ids := append([]string{"not-a-uuid", "", "1 OR 1=1"}, nonCanonicalCursorIDs(t, cur)...)
		for _, id := range ids {
			rec := call(f.listPath(withCursor(q, tamperGlobalCursor(t, cur, 5, id))))
			require.Equal(t, http.StatusBadRequest, rec.Code, "%s id %q: %s", name, id, rec.Body.String())
			assert.Equal(t, "invalid cursor", decodeErrorMessage(t, rec.Body.Bytes()), "%s id %q", name, id)
			assert.Equal(t, ref.Body.String(), rec.Body.String(), "%s id %q", name, id)
		}
	}
}

// --- agent-JWT path -------------------------------------------------------------

// Agent-JWT stats=1 counts every member, ignoring the request's phase filter,
// and costs no decisions.
func TestListProjectAgentsSortedAgentJWT_StatsCountsEveryMemberWithoutDecisions(t *testing.T) {
	f := sortedListSetup(t)
	self := f.createAgent(t, "stats-self", string(state.PhaseRunning), nil)
	f.createAgent(t, "stats-sib-1", string(state.PhaseStopped), nil)
	f.createAgent(t, "stats-sib-2", string(state.PhaseStopped), nil)
	tok := f.agentJWTFor(t, self.ID)

	emitter := &recordingDecisionAuditEmitter{}
	f.srv.authzService.SetDecisionAuditEmitter(emitter)

	rec := doRequestWithAgentToken(t, f.srv, http.MethodGet, f.listPath("sort=updated&limit=1&stats=1&phase=stopped"), nil, tok)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	resp := mustDecodeListAgentsResponse(t, rec.Body)
	require.NotNil(t, resp.Stats)
	assert.Equal(t, 3, resp.Stats.Total)
	assert.Equal(t, 1, resp.Stats.Running)
	require.NotNil(t, resp.Stats.Agents)
	assert.Len(t, *resp.Stats.Agents, 3)
	assert.Len(t, resp.Agents, 1)
	assert.Equal(t, 2, resp.TotalCount, "paged totalCount applies the phase filter")

	assert.Len(t, emitter.records, 4+8*1)
}

// The agent-JWT paged branch returns only rows matching the phase filter.
func TestListProjectAgentsSortedAgentJWT_PagedAppliesPhaseFilter(t *testing.T) {
	f := sortedListSetup(t)
	self := f.createAgent(t, "phase-self", string(state.PhaseRunning), nil)
	for i := 0; i < 3; i++ {
		f.createAgent(t, fmt.Sprintf("phase-sib-%d", i), string(state.PhaseStopped), nil)
	}
	tok := f.agentJWTFor(t, self.ID)

	var walked []string
	cursor := ""
	for i := 0; i < 10; i++ {
		q := "sort=updated&limit=2&phase=stopped"
		if cursor != "" {
			q += "&cursor=" + url.QueryEscape(cursor)
		}
		rec := doRequestWithAgentToken(t, f.srv, http.MethodGet, f.listPath(q), nil, tok)
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		resp := mustDecodeListAgentsResponse(t, rec.Body)
		assert.Equal(t, 3, resp.TotalCount)
		for _, a := range resp.Agents {
			assert.Equal(t, string(state.PhaseStopped), a.Phase)
			walked = append(walked, a.ID)
		}
		if resp.NextCursor == "" {
			break
		}
		cursor = resp.NextCursor
	}
	assert.Len(t, walked, 3)
	assert.NotContains(t, walked, self.ID)
}

// Agent-JWT fit: n = fit+1 is incomplete and pages; n == fit, with or without
// a phase filter, is complete with no cursor and the whole unphased set.
func TestListProjectAgentsSortedAgentJWT_FitBoundaries(t *testing.T) {
	f := sortedListSetup(t)
	self := f.createAgent(t, "fit-self", string(state.PhaseRunning), nil)
	for i := 0; i < 5; i++ {
		f.createAgent(t, fmt.Sprintf("fit-sib-%d", i), string(state.PhaseStopped), nil)
	}
	tok := f.agentJWTFor(t, self.ID)

	for _, q := range []string{"sort=updated&fit=5&limit=5", "sort=created&fit=5&limit=2&phase=stopped"} {
		rec := doRequestWithAgentToken(t, f.srv, http.MethodGet, f.listPath(q), nil, tok)
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		resp := mustDecodeListAgentsResponse(t, rec.Body)
		require.NotNil(t, resp.Complete, q)
		assert.False(t, *resp.Complete, q)
		assert.NotEmpty(t, resp.NextCursor, q)
	}

	rec := doRequestWithAgentToken(t, f.srv, http.MethodGet, f.listPath("sort=updated&fit=5&limit=5"), nil, tok)
	resp := mustDecodeListAgentsResponse(t, rec.Body)
	assert.Len(t, resp.Agents, 5)
	assert.Equal(t, 6, resp.TotalCount)

	rec = doRequestWithAgentToken(t, f.srv, http.MethodGet, f.listPath("sort=created&fit=5&limit=2&phase=stopped"), nil, tok)
	resp = mustDecodeListAgentsResponse(t, rec.Body)
	assert.Len(t, resp.Agents, 2)
	assert.Equal(t, 5, resp.TotalCount, "paged totalCount applies the phase filter")

	for _, q := range []string{"sort=updated&fit=6&limit=5", "sort=created&dir=asc&fit=6&limit=6&phase=stopped"} {
		rec := doRequestWithAgentToken(t, f.srv, http.MethodGet, f.listPath(q), nil, tok)
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		resp := mustDecodeListAgentsResponse(t, rec.Body)
		require.NotNil(t, resp.Complete, q)
		assert.True(t, *resp.Complete, "%s: n == fit must be complete", q)
		assert.Empty(t, resp.NextCursor, q)
		assert.Len(t, resp.Agents, 6, "%s: a complete response is the whole unphased set", q)
		assert.Equal(t, 6, resp.TotalCount, q)
	}
}

// Agent-JWT paged walks concatenate to the reference order for both sorts
// and both directions.
func TestListProjectAgentsSortedAgentJWT_WalkMatchesAgentsortReference(t *testing.T) {
	f := sortedListSetup(t)
	self := f.createAgent(t, "ord-self", string(state.PhaseRunning), nil)
	all := []*store.Agent{self}
	for i := 0; i < 6; i++ {
		all = append(all, f.createAgent(t, fmt.Sprintf("ord-sib-%d", i), string(state.PhaseStopped), nil))
	}
	// Exact ties on created and on the updated key, so the id tie-break and
	// the COALESCE(last_activity_event, updated) key both decide positions.
	const (
		t0 = "2026-01-01 00:00:00 +0000 UTC"
		t1 = "2026-01-01 00:00:01.5 +0000 UTC"
		t2 = "2026-01-01 00:00:02 +0000 UTC"
	)
	times := []struct{ created, updated, lae string }{
		{t0, t1, ""}, {t0, t1, ""}, {t0, t0, t1}, {t1, t0, t2}, {t1, t2, ""}, {t1, t0, ""}, {t2, t0, ""},
	}
	for i, a := range all {
		setRawAgentTimes(t, f.store, a.ID, times[i].created, times[i].updated, times[i].lae)
	}
	tok := f.agentJWTFor(t, self.ID)

	for _, sortKey := range []string{agentsort.Created, agentsort.Updated} {
		for _, dir := range []string{agentsort.Asc, agentsort.Desc} {
			// Re-read each row so the reference uses stored timestamps.
			rows := make([]agentsort.Row, len(all))
			for i, a := range all {
				got, err := f.store.GetAgent(t.Context(), a.ID)
				require.NoError(t, err)
				rows[i] = agentsort.KeyFor(sortKey, got.ID, got.Created, got.Updated, got.LastActivityEvent)
			}
			agentsort.SortRows(dir, rows)
			want := make([]string, len(rows))
			for i, row := range rows {
				want[i] = row.ID
			}

			for _, limit := range []int{1, 2, 3} {
				var walked []string
				cursor := ""
				for i := 0; i < 20; i++ {
					q := fmt.Sprintf("sort=%s&dir=%s&limit=%d", sortKey, dir, limit)
					if cursor != "" {
						q += "&cursor=" + url.QueryEscape(cursor)
					}
					rec := doRequestWithAgentToken(t, f.srv, http.MethodGet, f.listPath(q), nil, tok)
					require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
					resp := mustDecodeListAgentsResponse(t, rec.Body)
					for _, a := range resp.Agents {
						walked = append(walked, a.ID)
					}
					if resp.NextCursor == "" {
						break
					}
					cursor = resp.NextCursor
				}
				assert.Equal(t, want, walked, "sort=%s dir=%s limit=%d", sortKey, dir, limit)
			}
		}
	}
}

// Agent-JWT cursor binding covers sort and dir, the label filter and the
// project, field by field, the same as the user path: a cursor replayed
// under any other value is a 400, and replayed under its own values is a
// 200.
func TestListProjectAgentsSortedAgentJWT_CursorBindingPerField(t *testing.T) {
	f := sortedListSetup(t)
	ctx := context.Background()
	self := f.createAgent(t, "bind-self", string(state.PhaseRunning), map[string]string{"team": "a"})
	for i := 0; i < 3; i++ {
		f.createAgent(t, fmt.Sprintf("bind-sib-%d", i), string(state.PhaseStopped), map[string]string{"team": "a"})
	}
	tok := f.agentJWTFor(t, self.ID)

	mint := func(q string) string {
		t.Helper()
		rec := doRequestWithAgentToken(t, f.srv, http.MethodGet, f.listPath(q), nil, tok)
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		cur := mustDecodeListAgentsResponse(t, rec.Body).NextCursor
		require.NotEmpty(t, cur, q)
		return cur
	}

	cases := []struct {
		name, minted string
		replays      []string
	}{
		{"sort/dir", "sort=updated&dir=desc&limit=1", []string{
			"sort=updated&dir=asc&limit=1", "sort=created&dir=desc&limit=1", "sort=created&dir=asc&limit=1",
		}},
		{"label", "sort=updated&limit=1&label=team=a", []string{
			"sort=updated&limit=1&label=team=b", "sort=updated&limit=1", "sort=updated&limit=1&label=team=",
		}},
	}
	for _, tc := range cases {
		cur := mint(tc.minted)
		rec := doRequestWithAgentToken(t, f.srv, http.MethodGet, f.listPath(withCursor(tc.minted, cur)), nil, tok)
		require.Equal(t, http.StatusOK, rec.Code, "%s: own replay: %s", tc.name, rec.Body.String())
		for _, replay := range tc.replays {
			rec := doRequestWithAgentToken(t, f.srv, http.MethodGet, f.listPath(withCursor(replay, cur)), nil, tok)
			assert.Equal(t, http.StatusBadRequest, rec.Code, "%s: replay %q: %s", tc.name, replay, rec.Body.String())
			assert.Equal(t, "invalid cursor", decodeErrorMessage(t, rec.Body.Bytes()), "%s: replay %q", tc.name, replay)
		}
	}

	// Cross-project: the same agent holding a token for another project
	// cannot replay a cursor minted on this project's endpoint there.
	other := &store.Project{
		ID: tid("sl-bind-other"), Name: "Bind Other", Slug: "sl-bind-other",
		OwnerID: f.owner.ID, CreatedBy: f.owner.ID, Created: time.Now(), Updated: time.Now(),
	}
	require.NoError(t, f.store.CreateProject(ctx, other))
	for i := 0; i < 3; i++ {
		require.NoError(t, f.store.CreateAgent(ctx, &store.Agent{
			ID: tid(fmt.Sprintf("sl-bind-other-%d", i)), Slug: fmt.Sprintf("bind-other-%d", i), Name: fmt.Sprintf("bind-other-%d", i),
			ProjectID: other.ID, Phase: string(state.PhaseStopped), CreatedBy: f.owner.ID, OwnerID: f.owner.ID,
		}))
	}
	otherTok, err := f.srv.GetAgentTokenService().GenerateAgentToken(self.ID, other.ID, []AgentTokenScope{ScopeProjectRead}, nil)
	require.NoError(t, err)
	q := "sort=updated&limit=1"
	cur := mint(q)
	otherPath := "/api/v1/projects/" + other.ID + "/agents?" + withCursor(q, cur)
	rec := doRequestWithAgentToken(t, f.srv, http.MethodGet, otherPath, nil, otherTok)
	assert.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
	assert.Equal(t, "invalid cursor", decodeErrorMessage(t, rec.Body.Bytes()))
	// And the original token cannot reach the other project at all.
	rec = doRequestWithAgentToken(t, f.srv, http.MethodGet, otherPath, nil, tok)
	assert.NotEqual(t, http.StatusOK, rec.Code, rec.Body.String())
}

// hideFromFullRowReadStore hides one agent from the full-row read only (a
// ListAgents call by explicit IDs), leaving the member read and the
// filter re-check (both ListAgentMembers) untouched, so the race drop for
// a missing full row is exercised on its own.
type hideFromFullRowReadStore struct {
	store.Store
	agentID string
}

func (h *hideFromFullRowReadStore) ListAgents(ctx context.Context, filter store.AgentFilter, opts store.ListOptions) (*store.ListResult[store.Agent], error) {
	res, err := h.Store.ListAgents(ctx, filter, opts)
	if err != nil || len(filter.IDs) == 0 {
		return res, err
	}
	kept := res.Items[:0]
	for _, a := range res.Items {
		if a.ID != h.agentID {
			kept = append(kept, a)
		}
	}
	res.Items = kept
	return res, nil
}

// On the agent-JWT path, a row missing only from the full-row read is
// dropped: never leaked from the member snapshot and never an error. A
// complete response's totalCount reflects the drop; a paged response's
// totalCount stays the member count (a short page is valid) and still
// carries the cursor. The dropped row costs no decisions.
func TestListProjectAgentsSortedAgentJWT_RowMissingOnlyFromFullRowReadIsDropped(t *testing.T) {
	f := sortedListSetup(t)
	self := f.createAgent(t, "mf-self", string(state.PhaseRunning), nil)
	sibs := make([]string, 3)
	for i := range sibs {
		sibs[i] = f.createAgent(t, fmt.Sprintf("mf-sib-%d", i), string(state.PhaseStopped), nil).ID
	}
	// Pin the times so updated desc orders the siblings newest first
	// (sibs[2], sibs[1], sibs[0]) and self last.
	setRawAgentTimes(t, f.store, self.ID, "2026-01-01 00:00:00 +0000 UTC", "2026-01-01 00:00:00 +0000 UTC", "")
	for i, id := range sibs {
		ts := fmt.Sprintf("2026-01-01 00:00:0%d +0000 UTC", i+1)
		setRawAgentTimes(t, f.store, id, ts, ts, "")
	}
	tok := f.agentJWTFor(t, self.ID)
	hidden := sibs[len(sibs)-1]
	f.srv.store = &hideFromFullRowReadStore{Store: f.store, agentID: hidden}

	t.Run("complete", func(t *testing.T) {
		emitter := &recordingDecisionAuditEmitter{}
		f.srv.authzService.SetDecisionAuditEmitter(emitter)
		rec := doRequestWithAgentToken(t, f.srv, http.MethodGet, f.listPath("sort=updated&dir=desc&fit=500&limit=500&stats=1"), nil, tok)
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		resp := mustDecodeListAgentsResponse(t, rec.Body)
		require.NotNil(t, resp.Complete)
		assert.True(t, *resp.Complete)
		ids := agentIDs(resp)
		assert.NotContains(t, ids, hidden)
		assert.ElementsMatch(t, []string{self.ID, sibs[0], sibs[1]}, ids)
		assert.Equal(t, 3, resp.TotalCount, "a complete response's totalCount reflects the drop")
		require.NotNil(t, resp.Stats)
		assert.Equal(t, 4, resp.Stats.Total, "stats are taken from the member read, before the full-row read")
		assert.Len(t, emitter.records, 4+8*3)
	})

	t.Run("paged", func(t *testing.T) {
		emitter := &recordingDecisionAuditEmitter{}
		f.srv.authzService.SetDecisionAuditEmitter(emitter)
		rec := doRequestWithAgentToken(t, f.srv, http.MethodGet, f.listPath("sort=updated&dir=desc&limit=2"), nil, tok)
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		resp := mustDecodeListAgentsResponse(t, rec.Body)
		assert.Equal(t, []string{sibs[1]}, agentIDs(resp), "the hidden row is dropped, leaving a short page")
		assert.Equal(t, 4, resp.TotalCount, "a paged response's totalCount is the member count")
		require.NotEmpty(t, resp.NextCursor, "a short page still carries the cursor")
		assert.Len(t, emitter.records, 4+8*1)

		rec = doRequestWithAgentToken(t, f.srv, http.MethodGet, f.listPath(withCursor("sort=updated&dir=desc&limit=2", resp.NextCursor)), nil, tok)
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		next := mustDecodeListAgentsResponse(t, rec.Body)
		assert.NotContains(t, agentIDs(next), hidden)
		assert.NotContains(t, agentIDs(next), sibs[1], "no row repeats after a short page")
		assert.Equal(t, []string{sibs[0], self.ID}, agentIDs(next))
	})
	f.srv.store = f.store
}

func agentIDs(resp ListAgentsResponse) []string {
	ids := make([]string, len(resp.Agents))
	for i, a := range resp.Agents {
		ids[i] = a.ID
	}
	return ids
}
