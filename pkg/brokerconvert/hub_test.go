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

package brokerconvert

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/hub/capacityplan"
	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
)

const testBrokerID = "b-legacy"

// fakeHub answers the GET endpoints the plan reads. Any other method is
// recorded and answered 405; every test asserts none was sent.
type fakeHub struct {
	*httptest.Server
	t *testing.T

	mu       sync.Mutex
	nonGET   []string
	requests []string

	// Per-test answers; a status overrides the body for its route.
	row            map[string]any
	rowStatus      int
	settings       map[string]any
	settingsStatus int
	agentPages     [][]map[string]any
	agentsStatus   int
	// agentsHangup closes the connection on the agent list request, a
	// transport failure with no HTTP status.
	agentsHangup    bool
	projects        []map[string]any
	projectSettings map[string]map[string]any
	projectStatus   map[string]int
}

func newFakeHub(t *testing.T) *fakeHub {
	t.Helper()
	h := &fakeHub{
		t: t,
		row: map[string]any{
			"id":   testBrokerID,
			"name": "build-host",
			"profiles": []map[string]any{
				{"name": "build", "type": "docker"},
				{"name": "k8s", "type": "kubernetes"},
				{"name": "gpu", "type": "kubernetes"},
			},
		},
		settings: map[string]any{
			"brokerId": testBrokerID,
			"settings": map[string]any{"maxAgents": 10},
			"effective": map[string]any{"maxAgents": map[string]any{
				"value": 10, "source": "broker", "count": 2,
				"inherited": map[string]any{"value": 25, "source": "default"},
			}},
		},
		agentPages: [][]map[string]any{
			{
				{"id": "a-1", "name": "alpha", "projectId": "p-1", "project": "web", "phase": "running", "appliedConfig": map[string]any{"profile": "local"}},
				{"id": "a-2", "name": "beta", "projectId": "p-1", "project": "web", "phase": "stopped", "appliedConfig": map[string]any{"profile": "k8s"}},
			},
			{
				{"id": "a-3", "name": "gamma", "projectId": "p-2", "project": "api", "phase": "running", "appliedConfig": map[string]any{"profile": "legacy-podman"}},
				{"id": "a-4", "name": "delta", "projectId": "p-2", "project": "api", "phase": "created"},
			},
		},
		projects: []map[string]any{
			{"projectId": "p-1", "projectName": "web"},
			{"projectId": "p-2", "projectName": "api"},
		},
		projectSettings: map[string]map[string]any{
			"p-1": {"activeProfile": "k8s", "defaultGCPIdentityServiceAccountIDByProfile": map[string]string{"local": "sa-secret-id-1", "legacy-podman": "sa-secret-id-2"}},
			"p-2": {"defaultGCPIdentityServiceAccountIDByProfile": nil},
		},
		projectStatus: map[string]int{},
	}
	h.Server = httptest.NewServer(http.HandlerFunc(h.serve))
	t.Cleanup(h.Close)
	return h
}

func (h *fakeHub) serve(w http.ResponseWriter, r *http.Request) {
	h.mu.Lock()
	h.requests = append(h.requests, r.Method+" "+r.URL.Path)
	if r.Method != http.MethodGet {
		h.nonGET = append(h.nonGET, r.Method+" "+r.URL.Path)
		h.mu.Unlock()
		h.t.Errorf("the plan sent a non-GET request: %s %s", r.Method, r.URL.Path)
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	h.mu.Unlock()

	reply := func(status int, body any) {
		w.Header().Set("Content-Type", "application/json")
		if status != 0 && status != http.StatusOK {
			w.WriteHeader(status)
			_, _ = w.Write([]byte(`{"error":{"code":"x","message":"refused"}}`))
			return
		}
		_ = json.NewEncoder(w).Encode(body)
	}
	base := "/api/v1/runtime-brokers/" + testBrokerID
	switch {
	case r.URL.Path == base:
		reply(h.rowStatus, h.row)
	case r.URL.Path == base+"/settings":
		reply(h.settingsStatus, h.settings)
	case r.URL.Path == base+"/projects":
		reply(0, map[string]any{"projects": h.projects})
	case r.URL.Path == "/api/v1/agents" && h.agentsHangup:
		hj, ok := w.(http.Hijacker)
		if !ok {
			h.t.Errorf("the fake Hub cannot hijack the connection")
			return
		}
		conn, _, err := hj.Hijack()
		if err != nil {
			h.t.Errorf("hijack: %v", err)
			return
		}
		_ = conn.Close()
	case r.URL.Path == "/api/v1/agents":
		if got := r.URL.Query().Get("runtimeBrokerId"); got != testBrokerID {
			h.t.Errorf("agent list filter: got %q", got)
		}
		page := 0
		if c := r.URL.Query().Get("cursor"); c != "" {
			page = int(c[len(c)-1] - '0')
		}
		body := map[string]any{"agents": h.agentPages[page]}
		if page+1 < len(h.agentPages) {
			body["nextCursor"] = "page" + string(rune('0'+page+1))
		}
		reply(h.agentsStatus, body)
	case strings.HasPrefix(r.URL.Path, "/api/v1/projects/") && strings.HasSuffix(r.URL.Path, "/settings"):
		id := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/api/v1/projects/"), "/settings")
		reply(h.projectStatus[id], h.projectSettings[id])
	default:
		http.NotFound(w, r)
	}
}

func (h *fakeHub) client(t *testing.T) hubclient.Client {
	t.Helper()
	c, err := hubclient.New(h.URL)
	require.NoError(t, err)
	return c
}

func (h *fakeHub) assertGETOnly(t *testing.T) {
	t.Helper()
	h.mu.Lock()
	defer h.mu.Unlock()
	assert.Empty(t, h.nonGET, "the plan must send GET requests only")
	assert.NotEmpty(t, h.requests)
}

func hubPlan(t *testing.T, h *fakeHub) (*Plan, error) {
	t.Helper()
	p, err := BuildLocal(loadInput(t, "four_profiles.yaml"))
	require.NoError(t, err)
	err = CollectHub(context.Background(), h.client(t), testBrokerID, p)
	h.assertGETOnly(t)
	return p, err
}

func TestCollectHub_GoldenSections(t *testing.T) {
	h := newFakeHub(t)
	p, err := hubPlan(t, h)
	require.NoError(t, err)
	human, js := render(t, p)
	golden(t, "four_profiles.hub.txt", human)
	golden(t, "four_profiles.hub.json", js)

	assert.Equal(t, testBrokerID, p.LegacyBrokerID)
	require.NotNil(t, p.LegacyBroker)
	assert.Equal(t, []string{"legacy-podman", "local"}, p.LegacyBroker.LocalOnly)
	assert.Equal(t, []string{"gpu"}, p.LegacyBroker.HubOnly)

	// Paged to the end; the stopped agent is counted, not listed.
	require.NotNil(t, p.Agents)
	var ids []string
	for _, a := range p.Agents.Items {
		ids = append(ids, a.ID)
	}
	assert.ElementsMatch(t, []string{"a-1", "a-3", "a-4"}, ids)
	assert.Equal(t, 1, p.Agents.TerminalCount)
	for _, a := range p.Agents.Items {
		switch a.ID {
		case "a-1":
			assert.Equal(t, MappingMapped, a.Mapping)
		case "a-3":
			assert.Equal(t, MappingNoMapping, a.Mapping)
		case "a-4":
			assert.Equal(t, MappingProfileUnknown, a.Mapping)
		}
	}
	assert.Contains(t, string(human), AgentsNotMovableNote)

	// Projects: activeProfile and the per-profile service account keys,
	// never the service account IDs.
	require.NotNil(t, p.Projects)
	require.Len(t, p.Projects.Items, 2)
	web := p.Projects.Items[1]
	assert.Equal(t, "web", web.Name)
	require.NotNil(t, web.ActiveProfile)
	assert.Equal(t, MappingMapped, web.ActiveProfile.Mapping)
	require.Len(t, web.ServiceAccountProfiles, 2)
	assert.Equal(t, "legacy-podman", web.ServiceAccountProfiles[0].Profile)
	assert.Equal(t, MappingNoMapping, web.ServiceAccountProfiles[0].Mapping)
	assert.NotContains(t, string(js), "sa-secret-id")
	assert.NotContains(t, string(human), "sa-secret-id")
}

func TestCollectHub_MaxAgentsSplit(t *testing.T) {
	h := newFakeHub(t)
	p, err := hubPlan(t, h)
	require.NoError(t, err)
	c := p.Capacity
	assert.Equal(t, CapacityPlanned, c.Status)
	assert.Equal(t, capacityplan.OutcomePreserveBySplit, c.Outcome)
	require.Len(t, c.Allocations, 3)
	var got []int64
	for _, a := range c.Allocations {
		got = append(got, a.MaxAgents)
	}
	assert.Equal(t, []int64{4, 3, 3}, got, "10 over 3 instances, the remainder to the first key")
	assert.Contains(t, c.Flags, FlagEnforcementNotVisible)
	for _, r := range append(c.Reasons, c.Flags...) {
		assert.False(t, strings.HasPrefix(r, capacityplan.ReasonNotEnforced), "the planner's not_enforced flag is replaced: %s", r)
	}
	// Blocking podman finding: exit 3, but no decision is required.
	assert.False(t, p.Summary.DecisionRequired)
	assert.Equal(t, ExitBlocked, p.ExitCode())
}

func TestCollectHub_UnsetOverrideNeedsDecision(t *testing.T) {
	h := newFakeHub(t)
	h.settings["settings"] = map[string]any{}
	p, err := BuildLocal(func() Input {
		in := loadInput(t, "four_profiles.yaml")
		in.Profiles = []string{"build", "local", "k8s"}
		return in
	}())
	require.NoError(t, err)
	require.NoError(t, CollectHub(context.Background(), h.client(t), testBrokerID, p))
	h.assertGETOnly(t)

	assert.Equal(t, capacityplan.OutcomeOperatorDecisionRequired, p.Capacity.Outcome)
	require.NotEmpty(t, p.Capacity.Reasons)
	assert.Contains(t, p.Capacity.Reasons[0], "inherits 25")
	assert.Equal(t, 0, p.Summary.BlockingFindings)
	assert.True(t, p.Summary.DecisionRequired)
	assert.Equal(t, ResultDecisionsRequired, p.Summary.Result)
	assert.Equal(t, ExitBlocked, p.ExitCode())
}

func TestCollectHub_AgentList403IsPartialNotError(t *testing.T) {
	h := newFakeHub(t)
	h.agentsStatus = http.StatusForbidden
	p, err := hubPlan(t, h)
	require.NoError(t, err)
	require.NotNil(t, p.Agents)
	assert.Contains(t, p.Agents.Note, "not visible to you")
	assert.Empty(t, p.Agents.Items)
	assert.Equal(t, ExitBlocked, p.ExitCode(), "exit by findings, not 1")
}

func TestCollectHub_SettingsForbiddenLeavesCapacityUnavailable(t *testing.T) {
	h := newFakeHub(t)
	h.settingsStatus = http.StatusForbidden
	p, err := hubPlan(t, h)
	require.NoError(t, err)
	assert.Equal(t, CapacityUnavailable, p.Capacity.Status)
	assert.Contains(t, p.Capacity.Note, "not visible to you")
}

func TestCollectHub_ServerErrorOnSecondaryCallExits1(t *testing.T) {
	h := newFakeHub(t)
	h.projectStatus["p-2"] = http.StatusInternalServerError
	p, err := hubPlan(t, h)
	require.NoError(t, err, "the plan is still produced")
	require.NotNil(t, p.Projects)
	assert.Equal(t, ExitError, p.ExitCode())
	var note string
	for _, e := range p.Projects.Items {
		if e.ID == "p-2" {
			note = e.Note
		}
	}
	assert.Equal(t, "could not be read from the Hub (HTTP 500): this project's settings", note)
	assertNoHubAddress(t, h, p)
}

func TestCollectHub_AgentList5xxNoteAndExit1(t *testing.T) {
	h := newFakeHub(t)
	h.agentsStatus = http.StatusInternalServerError
	p, err := hubPlan(t, h)
	require.NoError(t, err, "the plan is still produced")
	require.NotNil(t, p.Agents)
	assert.Equal(t, "could not be read from the Hub (HTTP 500): the agent list", p.Agents.Note)
	assert.Empty(t, p.Agents.Items)
	assert.Equal(t, ExitError, p.ExitCode())
	assertNoHubAddress(t, h, p)
}

// A transport failure (no HTTP status) on a secondary read gets the fixed
// "(request failed)" note; the transport error, which names the Hub's
// address, is never used.
func TestCollectHub_TransportFailureNoteIsFixedText(t *testing.T) {
	h := newFakeHub(t)
	h.agentsHangup = true
	p, err := hubPlan(t, h)
	require.NoError(t, err, "the plan is still produced")
	require.NotNil(t, p.Agents)
	assert.Equal(t, "could not be read from the Hub (request failed): the agent list", p.Agents.Note)
	assert.Equal(t, ExitError, p.ExitCode())
	assertNoHubAddress(t, h, p)
}

func TestCollectHub_VisibilityNotesAreFixedText(t *testing.T) {
	h := newFakeHub(t)
	h.agentsStatus = http.StatusForbidden
	h.settingsStatus = http.StatusForbidden
	p, err := hubPlan(t, h)
	require.NoError(t, err)
	assert.Equal(t, "not visible to you: the agent list", p.Agents.Note)
	assert.Equal(t, "not visible to you: the Runtime Broker's settings", p.Capacity.Note)
	assertNoHubAddress(t, h, p)
}

// assertNoHubAddress checks that no part of the plan, in either format,
// carries the Hub's address or the fake Hub's error message.
func assertNoHubAddress(t *testing.T, h *fakeHub, p *Plan) {
	t.Helper()
	human, js := render(t, p)
	host := strings.TrimPrefix(h.URL, "http://")
	for _, out := range []string{string(human), string(js)} {
		assert.NotContains(t, out, host)
		assert.NotContains(t, out, "refused", "the Hub's error message is not quoted")
	}
}

func TestCollectHub_FlatRowRefused(t *testing.T) {
	h := newFakeHub(t)
	h.row["runtimeTarget"] = map[string]any{"type": "docker"}
	_, err := hubPlan(t, h)
	assert.ErrorIs(t, err, ErrFlatRuntimeBroker)
}

func TestCollectHub_RowNotFound(t *testing.T) {
	h := newFakeHub(t)
	h.rowStatus = http.StatusNotFound
	_, err := hubPlan(t, h)
	require.Error(t, err)
	assert.Contains(t, err.Error(), testBrokerID)
	assert.Contains(t, err.Error(), "scion hub brokers info")
}

func TestCollectHub_NotRegistered(t *testing.T) {
	h := newFakeHub(t)
	p, err := BuildLocal(loadInput(t, "four_profiles.yaml"))
	require.NoError(t, err)
	err = CollectHub(context.Background(), h.client(t), "", p)
	assert.ErrorIs(t, err, ErrNotRegistered)
}
