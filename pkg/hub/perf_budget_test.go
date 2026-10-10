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

// Hub counter budgets for the agent list and agent endpoints.
//
// TestPerfBudget_AgentEndpoints serves the requests the web client sends
// for the agent pages, as a non-admin project member, against a fixed
// seeded SQLite store, with server.hub.perf_trace on. It reads the
// host-independent counts from the request's trace and fails when one goes
// over its budget:
//
//   - authzStoreCalls: authorization store reads (PerfTraceSnapshot.AuthzStoreCalls)
//   - decisions:       authorization decisions, one decision-audit record each
//     (PerfTraceSnapshot.AuditRecords)
//   - dbReads:         agent-row store reads (the list_db_read phase count)
//   - bytes:           response body size
//
// None of these depend on host speed or load, so the test runs in the
// ordinary pkg/hub SQLite CI job (make test-hub-sqlite). Wall-clock
// budgets are separate: see perf/bench/README.md, "Wall-clock budgets".
//
// Baselines are the values measured on main (commit in perfBudgetBaseline)
// with this fixture. A budget's limit is its baseline plus a margin:
//
//   - counts: baseline + max(2, 2% of baseline, rounded up)
//   - bytes:  baseline + 1%, rounded up
//
// The margins are tight on purpose. The seed is deterministic, so the
// counts are exact: they repeat on every run and every machine (given a
// monotonic wall clock with sub-microsecond resolution, so the agents'
// stamped updated times are strictly increasing and the sort order is
// stable). Measured
// byte jitter between runs is under 0.01% (serverTime and the agents'
// created and updated times, which the store stamps), well inside the 1%.
// The margin only absorbs small, intended per-request changes: a single
// extra read or decision per request sits inside the +2 floor and passes.
// An N+1 regression, one more store read, decision or row read per
// returned agent (25 on a first page, 100 on a full list), is over every
// count budget, and so is about 20 more bytes per agent on a full list.
//
// Updating a budget for an intended change: run
//
//	go test -run '^TestPerfBudget_AgentEndpoints$' -v ./pkg/hub/
//
// read the "measured" lines, set the baseline to the new value in
// perfBudgets, and say why in the commit message. See
// docs-site/src/content/docs/contributing/perf-tracing.md.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"math/rand"
	"net/http"
	"net/http/httptest"
	"os"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

const (
	// perfBudgetAgents is the seeded agent count in the member's project.
	perfBudgetAgents = 100
	// perfBudgetOtherAgents are agents in a project the member is not in,
	// so the global list has rows the member must not see.
	perfBudgetOtherAgents = 10
	// perfBudgetRandSeed fixes the synthetic agent shapes (as perf/bench
	// seed's --rand-seed does).
	perfBudgetRandSeed = 42
	// perfBudgetBaseline names the main commit the baselines were taken on.
	perfBudgetBaseline = "main d28338a"
)

// perfBudgetCounts is one request's measured counts, or its baseline.
type perfBudgetCounts struct {
	authzStoreCalls int64
	decisions       int64
	dbReads         int64
	bytes           int64
}

// perfBudget is one gated request. baseline holds the values measured on
// perfBudgetBaseline; the limits are derived from it (perfBudgetLimit).
type perfBudget struct {
	name     string
	path     func(f *perfBudgetFixture) string
	baseline perfBudgetCounts
}

// perfBudgets: the requests the web client sends for the project agent
// page, the global agents page and the agent detail page, plus the legacy
// full lists (used by older clients and the CLI).
var perfBudgets = []perfBudget{
	{
		name: "project list, first page (fit, stats)",
		path: func(f *perfBudgetFixture) string {
			return "/api/v1/projects/" + f.project.ID + "/agents?sort=updated&dir=desc&limit=25&fit=50&stats=1"
		},
		baseline: perfBudgetCounts{authzStoreCalls: 74, decisions: 280, dbReads: 4, bytes: 48478},
	},
	{
		name: "project list, page by ids (25)",
		path: func(f *perfBudgetFixture) string {
			return "/api/v1/projects/" + f.project.ID + "/agents?sort=updated&dir=desc&limit=25&ids=" + strings.Join(f.agentIDs[25:50], ",")
		},
		baseline: perfBudgetCounts{authzStoreCalls: 102, decisions: 205, dbReads: 4, bytes: 42847},
	},
	{
		name: "project list, graph (limit=500)",
		path: func(f *perfBudgetFixture) string {
			return "/api/v1/projects/" + f.project.ID + "/agents?limit=500"
		},
		baseline: perfBudgetCounts{authzStoreCalls: 312, decisions: 1005, dbReads: 4, bytes: 184699},
	},
	{
		name: "global list, first page (fit, stats)",
		path: func(f *perfBudgetFixture) string {
			return "/api/v1/agents?sort=updated&dir=desc&limit=25&fit=50&stats=1"
		},
		baseline: perfBudgetCounts{authzStoreCalls: 84, decisions: 474, dbReads: 5, bytes: 50436},
	},
	{
		name:     "global list, legacy full",
		path:     func(f *perfBudgetFixture) string { return "/api/v1/agents" },
		baseline: perfBudgetCounts{authzStoreCalls: 356, decisions: 1080, dbReads: 4, bytes: 192428},
	},
	{
		name:     "global list, legacy compact",
		path:     func(f *perfBudgetFixture) string { return "/api/v1/agents?view=compact" },
		baseline: perfBudgetCounts{authzStoreCalls: 356, decisions: 1080, dbReads: 4, bytes: 74744},
	},
	{
		// Store reads here are high; ptone/scion#3968 tracks tightening this bound.
		name: "agent by id",
		path: func(f *perfBudgetFixture) string { return "/api/v1/agents/" + f.agentIDs[10] },
		// Bytes: upstream #3070 added agent editability to the single-agent GET (main ca19871).
		baseline: perfBudgetCounts{authzStoreCalls: 309, decisions: 10, dbReads: 0, bytes: 14029},
	},
	{
		// Store reads here are high; ptone/scion#3968 tracks tightening this bound.
		name: "agent by id, project route",
		path: func(f *perfBudgetFixture) string {
			return "/api/v1/projects/" + f.project.ID + "/agents/" + f.agentIDs[10]
		},
		// Bytes: upstream #3070 added agent editability to the single-agent GET (main ca19871).
		baseline: perfBudgetCounts{authzStoreCalls: 309, decisions: 10, dbReads: 0, bytes: 14029},
	},
}

// perfBudgetLimit applies the margin policy in the file comment.
func perfBudgetLimit(b perfBudgetCounts) perfBudgetCounts {
	count := func(v int64) int64 {
		m := (v*2 + 99) / 100
		if m < 2 {
			m = 2
		}
		return v + m
	}
	return perfBudgetCounts{
		authzStoreCalls: count(b.authzStoreCalls),
		decisions:       count(b.decisions),
		dbReads:         count(b.dbReads),
		bytes:           b.bytes + (b.bytes+99)/100,
	}
}

type perfBudgetFixture struct {
	srv      *Server
	project  *store.Project
	member   *store.User
	agentIDs []string // the member project's agents, by seed index
}

// newPerfBudgetFixture seeds a fresh in-memory SQLite store: an owner
// (alice), a non-admin project member (carol, the caller), a second user
// (bob) with a project of his own, perfBudgetAgents agents in alice's
// project and perfBudgetOtherAgents in bob's. Agent shapes follow
// perf/bench/seed's synthetic agents (labels, activity, ancestry, a small
// appliedConfig on 90% and a several-KB pre-start hook script on 10%),
// generated from perfBudgetRandSeed with fixed IDs, so every run sees the
// same rows. The store stamps each agent's created and updated times when
// it is created, so those (and the order of equal sort keys) are the only
// values that differ between runs; none of the gated counts depend on them.
func newPerfBudgetFixture(t *testing.T) *perfBudgetFixture {
	t.Helper()
	s, err := newTestStore(t, ":memory:")
	if err != nil {
		if strings.Contains(err.Error(), "sqlite driver not registered") {
			t.Skip("sqlite driver not registered")
		}
		t.Fatalf("test store: %v", err)
	}
	ctx := context.Background()
	require.NoError(t, s.Migrate(ctx))
	_ = s.DeleteHubSetting(ctx, "migration_delegation_edge_backfill_v1")

	srv := newPerfServer(t, s, true)
	alice, bob, project := setupDemoPolicyOn(t, srv, s)

	carol := &store.User{
		ID: tid("user-carol"), Email: "carol@test.com", DisplayName: "Carol",
		Role: store.UserRoleMember, Status: "active", Created: time.Now(),
	}
	require.NoError(t, s.CreateUser(ctx, carol))
	ensureHubMembership(ctx, s, carol.ID)
	addProjectMemberWithRole(t, s, project, carol.ID, store.GroupMemberRoleMember)

	other := &store.Project{
		ID: tid("project-budget-other"), Name: "Other", Slug: "budget-other",
		OwnerID: bob.ID, CreatedBy: bob.ID,
	}
	require.NoError(t, s.CreateProject(ctx, other))
	srv.seedProjectCreatorMembership(ctx, other)

	f := &perfBudgetFixture{srv: srv, project: project, member: carol}
	rng := rand.New(rand.NewSource(perfBudgetRandSeed))
	now := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	phases := []string{"running", "running", "running", "stopped", "error", "created"}
	activities := []string{"idle", "thinking", "executing", "waiting_for_input"}
	mk := func(i int, p *store.Project, owner, member *store.User) string {
		id := tid(fmt.Sprintf("budget-%s-%d", p.Slug, i))
		phase := phases[rng.Intn(len(phases))]
		creator := owner.ID
		if rng.Intn(2) == 0 {
			creator = member.ID
		}
		tier := "small"
		if i%10 == 0 {
			tier = "large"
		}
		a := &store.Agent{
			ID:        id,
			Slug:      fmt.Sprintf("bench-agent-%05d", i),
			Name:      fmt.Sprintf("Bench Agent %d", i),
			Template:  "default",
			ProjectID: p.ID,
			Phase:     phase,
			Activity:  activities[rng.Intn(len(activities))],
			Labels:    map[string]string{"bench": "true", "bench-seq": fmt.Sprint(i), "bench-tier": tier},
			CreatedBy: creator,
			OwnerID:   creator,
			// The store overwrites Created and Updated (and so LastSeen) with
			// the current time; the rng draws are kept so the rng sequence,
			// and with it the seeded rows and the baselines, do not move.
			Created: now.Add(-time.Duration(rng.Intn(30*24)) * time.Hour),
			Updated: now.Add(-time.Duration(rng.Intn(24*60)) * time.Minute),
		}
		if phase != "created" {
			a.LastSeen = a.Updated
		}
		switch r := rng.Intn(100); {
		case r < 60:
		case r < 85 || len(f.agentIDs) == 0 || p != project:
			a.Ancestry = []string{creator}
		default:
			a.Ancestry = []string{creator, f.agentIDs[rng.Intn(len(f.agentIDs))]}
		}
		a.AppliedConfig = &store.AgentAppliedConfig{
			Image:         "ghcr.io/scion-project/claude-harness:latest",
			HarnessConfig: "default-claude",
			Model:         "claude-sonnet-5",
			Profile:       "default",
			TemplateID:    "tmpl-bench-default",
			TemplateHash:  "sha256:" + strings.Repeat("ab", 32),
			AgentRole:     "baseline",
			CreatorName:   "bench-owner@example.test",
			Env: map[string]string{
				"SCION_PROJECT":   p.Slug,
				"SCION_AGENT_SEQ": fmt.Sprint(i),
				"SCION_BENCH_RUN": "1",
				"SCION_LOG_LEVEL": "info",
			},
		}
		if tier == "large" {
			var b strings.Builder
			b.WriteString("#!/usr/bin/env bash\nset -euo pipefail\n")
			for j, n := 0, 80+rng.Intn(40); j < n; j++ {
				fmt.Fprintf(&b, "echo 'bench pre-start step %d: %d'\n", j, rng.Int63())
			}
			a.AppliedConfig.ProjectPreStartHookID = tid(fmt.Sprintf("budget-hook-%d", i))
			a.AppliedConfig.ProjectPreStartHookScript = b.String()
		}
		require.NoError(t, s.CreateAgent(ctx, a))
		return id
	}
	for i := 0; i < perfBudgetAgents; i++ {
		f.agentIDs = append(f.agentIDs, mk(i, project, alice, carol))
	}
	for i := 0; i < perfBudgetOtherAgents; i++ {
		mk(i, other, bob, bob)
	}
	return f
}

// measure serves path as the member and returns the request's counts.
func (f *perfBudgetFixture) measure(t *testing.T, path string) perfBudgetCounts {
	t.Helper()
	c, _ := f.serve(t, path)
	return c
}

// request builds a GET for path as the member.
func (f *perfBudgetFixture) request(t *testing.T, path string) *http.Request {
	t.Helper()
	token, _, _, err := f.srv.userTokenService.GenerateTokenPair(f.member.ID, f.member.Email, f.member.DisplayName, f.member.Role, ClientTypeWeb)
	require.NoError(t, err)
	req := httptest.NewRequest(http.MethodGet, path, nil)
	req.Header.Set("Authorization", "Bearer "+token)
	return req
}

// serveRaw serves path as the member, whatever the status.
func (f *perfBudgetFixture) serveRaw(t *testing.T, path string) *httptest.ResponseRecorder {
	t.Helper()
	rec, _ := perfTracedRequest(t, f.srv, f.request(t, path))
	return rec
}

// serve serves path as the member, requires a 200 and returns the
// request's counts and response body.
func (f *perfBudgetFixture) serve(t *testing.T, path string) (perfBudgetCounts, []byte) {
	t.Helper()
	rec, snap := perfTracedRequest(t, f.srv, f.request(t, path))
	require.Equal(t, http.StatusOK, rec.Code, "%s: %s", path, rec.Body.String())
	return perfBudgetCounts{
		authzStoreCalls: snap.AuthzStoreCalls,
		decisions:       snap.AuditRecords,
		dbReads:         snap.Phases[perfPhaseListDBRead.String()].Count,
		bytes:           int64(rec.Body.Len()),
	}, rec.Body.Bytes()
}

// TestPerfBudget_AgentEndpoints gates the counts above; see the file
// comment for the baselines, margins and how to update a budget.
func TestPerfBudget_AgentEndpoints(t *testing.T) {
	f := newPerfBudgetFixture(t)
	t.Run("web fixture schema is current", func(t *testing.T) { f.checkWebSchema(t) })
	for _, b := range perfBudgets {
		t.Run(b.name, func(t *testing.T) {
			path := b.path(f)
			// Two identical requests: the first runs with cold
			// per-request caches after the previous gate's request, the
			// second repeats it. Both must stay within the budget.
			var got perfBudgetCounts
			for i := 0; i < 2; i++ {
				m := f.measure(t, path)
				got.authzStoreCalls = max(got.authzStoreCalls, m.authzStoreCalls)
				got.decisions = max(got.decisions, m.decisions)
				got.dbReads = max(got.dbReads, m.dbReads)
				got.bytes = max(got.bytes, m.bytes)
			}
			limit := perfBudgetLimit(b.baseline)
			t.Logf("measured: authzStoreCalls=%d decisions=%d dbReads=%d bytes=%d (baseline %d/%d/%d/%d on %s; limit %d/%d/%d/%d)",
				got.authzStoreCalls, got.decisions, got.dbReads, got.bytes,
				b.baseline.authzStoreCalls, b.baseline.decisions, b.baseline.dbReads, b.baseline.bytes, perfBudgetBaseline,
				limit.authzStoreCalls, limit.decisions, limit.dbReads, limit.bytes)
			check := func(what string, got, limit, baseline int64) {
				if got > limit {
					t.Errorf("%s: %s = %d, over budget %d (baseline %d on %s). "+
						"If the increase is intended, update the baseline in perfBudgets "+
						"(see docs-site/src/content/docs/contributing/perf-tracing.md).",
						b.name, what, got, limit, baseline, perfBudgetBaseline)
				}
			}
			check("authz store calls", got.authzStoreCalls, limit.authzStoreCalls, b.baseline.authzStoreCalls)
			check("authz decisions", got.decisions, limit.decisions, b.baseline.decisions)
			check("agent-row DB reads", got.dbReads, limit.dbReads, b.baseline.dbReads)
			check("response bytes", got.bytes, limit.bytes, b.baseline.bytes)
			// Non-fatal: a value well under its baseline means the baseline
			// is stale, and the old headroom would let a later regression
			// through. Lower the baseline in the change that lowered it.
			low := func(what string, got, limit, baseline int64) {
				if margin := limit - baseline; got < baseline-margin {
					t.Logf("note: %s = %d is below baseline %d by more than the margin %d; consider lowering the baseline",
						what, got, baseline, margin)
				}
			}
			low("authz store calls", got.authzStoreCalls, limit.authzStoreCalls, b.baseline.authzStoreCalls)
			low("authz decisions", got.decisions, limit.decisions, b.baseline.decisions)
			low("agent-row DB reads", got.dbReads, limit.dbReads, b.baseline.dbReads)
			low("response bytes", got.bytes, limit.bytes, b.baseline.bytes)
		})
	}
}

// The web counter budgets (web/e2e-perf/budgets) render the project page
// from mocked API responses that a small deterministic generator builds at
// test time (web/e2e-perf/budgets/fixture.mjs). To keep that generator on
// the hub's real response shape, this test writes the field names and
// JSON types of the hub's responses for the requests the page sends, for
// this fixture and caller, to perfBudgetWebSchemaFile. The "web fixture
// schema is current" subtest fails when the hub's fields or their types
// no longer match the file, and a web
// unit test (npm run test:e2e-perf) fails when the generator's output does
// not match it. To refresh the file after an intended API change:
//
//	SCION_PERF_BUDGET_WRITE_WEB_SCHEMA=1 go test -run '^TestPerfBudget_AgentEndpoints$' ./pkg/hub/
//
// then update the generator until the web unit test passes.
const perfBudgetWebSchemaFile = "../../web/e2e-perf/budgets/fixture-schema.json"

// perfBudgetWebRequests are the hub API requests the project page sends
// (startup, shell trays, the page itself) for its grid, list and graph
// views. The web budget test fails on a request the generator does not
// answer, so a new request shows up there first; add it here too.
var perfBudgetWebRequests = []string{
	"/api/v1/settings/public",
	"/api/v1/experiments",
	"/api/v1/auth/admin-status",
	"/api/v1/system/status",
	"/api/v1/chat/spaces",
	"/api/v1/chat/dms",
	"/api/v1/messages?unread=true",
	"/api/v1/notifications?acknowledged=false",
	"/api/v1/projects?limit=1",
	"/api/v1/projects/{project}",
	"/api/v1/projects/{project}/metrics-summary",
	"/api/v1/projects/{project}/metrics/summary",
	"/api/v1/projects/{project}/workspace/files?limit=500",
	"/api/v1/projects/{project}/agents?sort=updated&dir=desc&limit=25&fit=50&stats=1",
	"/api/v1/projects/{project}/agents?limit=500",
}

// perfBudgetWebSchema is the file format; fixture.mjs computes the same
// structure from its generated responses.
type perfBudgetWebSchema struct {
	Comment   string                                 `json:"comment"`
	Agents    int                                    `json:"agents"`
	Endpoints map[string]perfBudgetWebSchemaEndpoint `json:"endpoints"`
}

// perfBudgetWebSchemaEndpoint is one request's status and the sorted,
// de-duplicated field paths of its JSON body, each with the JSON types of
// its values: object keys joined with ".", array elements as "[]" (so
// every agent in a list contributes to "agents[].<field>", and scalar
// elements are recorded at "<path>[]", e.g. "permissions[]:string"),
// then ":" and the "|"-joined sorted types seen there (array, boolean,
// null, number, object, string), e.g. "agents[].generation:number". A
// non-JSON body has no fields.
type perfBudgetWebSchemaEndpoint struct {
	Status int      `json:"status"`
	Fields []string `json:"fields"`
}

// perfBudgetJSONType names the JSON type of a decoded value.
func perfBudgetJSONType(v any) string {
	switch v.(type) {
	case map[string]any:
		return "object"
	case []any:
		return "array"
	case string:
		return "string"
	case float64:
		return "number"
	case bool:
		return "boolean"
	default:
		return "null"
	}
}

// perfBudgetFieldPaths returns the typed field paths of a decoded JSON
// value, as described on perfBudgetWebSchemaEndpoint. fixture.mjs's
// fieldPaths computes the same strings.
func perfBudgetFieldPaths(v any) []string {
	types := map[string]map[string]bool{}
	var walk func(v any, prefix string)
	walk = func(v any, prefix string) {
		switch x := v.(type) {
		case map[string]any:
			for k, c := range x {
				p := k
				if prefix != "" {
					p = prefix + "." + k
				}
				if types[p] == nil {
					types[p] = map[string]bool{}
				}
				types[p][perfBudgetJSONType(c)] = true
				walk(c, p)
			}
		case []any:
			p := prefix + "[]"
			for _, c := range x {
				switch c.(type) {
				case map[string]any, []any:
				default:
					// A scalar element: record its type at "<path>[]".
					if types[p] == nil {
						types[p] = map[string]bool{}
					}
					types[p][perfBudgetJSONType(c)] = true
				}
				walk(c, p)
			}
		}
	}
	walk(v, "")
	out := make([]string, 0, len(types))
	for p, ts := range types {
		names := make([]string, 0, len(ts))
		for t := range ts {
			names = append(names, t)
		}
		sort.Strings(names)
		out = append(out, p+":"+strings.Join(names, "|"))
	}
	sort.Strings(out)
	return out
}

func (f *perfBudgetFixture) webSchema(t *testing.T) []byte {
	t.Helper()
	schema := perfBudgetWebSchema{
		Comment: "Generated by TestPerfBudget_AgentEndpoints (pkg/hub/perf_budget_test.go) from the hub's " +
			"responses for its seeded fixture. Do not edit by hand; see contributing/perf-tracing.md.",
		Agents:    perfBudgetAgents,
		Endpoints: map[string]perfBudgetWebSchemaEndpoint{},
	}
	for _, tmpl := range perfBudgetWebRequests {
		rec := f.serveRaw(t, strings.ReplaceAll(tmpl, "{project}", f.project.ID))
		ep := perfBudgetWebSchemaEndpoint{Status: rec.Code, Fields: []string{}}
		var v any
		if json.Unmarshal(rec.Body.Bytes(), &v) == nil {
			ep.Fields = perfBudgetFieldPaths(v)
		}
		schema.Endpoints[tmpl] = ep
	}
	// GET /auth/me is served by the web server from the session, in its
	// own type.
	me, err := json.Marshal(webSessionUser{
		UserID: f.member.ID, Email: f.member.Email, Name: f.member.DisplayName, Role: f.member.Role,
	})
	require.NoError(t, err)
	var meV any
	require.NoError(t, json.Unmarshal(me, &meV))
	schema.Endpoints["/auth/me"] = perfBudgetWebSchemaEndpoint{Status: http.StatusOK, Fields: perfBudgetFieldPaths(meV)}

	out, err := json.MarshalIndent(schema, "", "  ")
	require.NoError(t, err)
	return append(out, '\n')
}

func (f *perfBudgetFixture) checkWebSchema(t *testing.T) {
	got := f.webSchema(t)
	if os.Getenv("SCION_PERF_BUDGET_WRITE_WEB_SCHEMA") == "1" {
		require.NoError(t, os.WriteFile(perfBudgetWebSchemaFile, got, 0o644))
		t.Logf("wrote %s (%d bytes)", perfBudgetWebSchemaFile, len(got))
		return
	}
	want, err := os.ReadFile(perfBudgetWebSchemaFile)
	require.NoError(t, err)
	if !bytes.Equal(want, got) {
		t.Errorf("%s does not match the field names and types of the hub's current responses. "+
			"If the API change is intended, refresh it with "+
			"SCION_PERF_BUDGET_WRITE_WEB_SCHEMA=1 go test -run '^TestPerfBudget_AgentEndpoints$' ./pkg/hub/ "+
			"and update web/e2e-perf/budgets/fixture.mjs to match (see perf-tracing.md).\n--- file\n%s\n--- hub\n%s",
			perfBudgetWebSchemaFile, want, got)
	}
}
