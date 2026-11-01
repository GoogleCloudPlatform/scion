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
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/config/opsettings"
	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Tests for ptone/scion#3925: a project's default count limits are
// tri-state. null clears, 0 is Unlimited and n is a limit.

func putProjectSettings(t *testing.T, srv *Server, projectID, body string) (int, string) {
	t.Helper()
	rec := doRequest(t, srv, http.MethodPut, "/api/v1/projects/"+projectID+"/settings", json.RawMessage(body))
	return rec.Code, rec.Body.String()
}

func projectUnlimitedAudits(t *testing.T, s store.Store, projectID string) []*store.MutationAuditRecord {
	t.Helper()
	recs, _, err := s.ListMutationAudits(context.Background(), store.MutationAuditFilter{
		MutationType: mutationTypeProjectSettingsLimitUnlimited, TargetID: projectID, Limit: 20,
	})
	require.NoError(t, err)
	return recs
}

// waitProjectUnlimitedAudits waits for at least want records (audit writes
// are asynchronous) and returns them.
func waitProjectUnlimitedAudits(t *testing.T, s store.Store, projectID string, want int) []*store.MutationAuditRecord {
	t.Helper()
	var got []*store.MutationAuditRecord
	require.Eventually(t, func() bool {
		got = projectUnlimitedAudits(t, s, projectID)
		return len(got) >= want
	}, 5*time.Second, 20*time.Millisecond)
	return got
}

func TestCountLimitAnnotation(t *testing.T) {
	m := map[string]string{"zero": "0", "seven": "7", "neg": "-3", "bad": "x", "empty": ""}
	assert.Equal(t, api.IntPtr(0), countLimitAnnotation(m, "zero"))
	assert.Equal(t, api.IntPtr(7), countLimitAnnotation(m, "seven"))
	assert.Nil(t, countLimitAnnotation(m, "absent"))
	assert.Nil(t, countLimitAnnotation(m, "neg"), "a negative value reads as unset")
	assert.Nil(t, countLimitAnnotation(m, "bad"))
	assert.Nil(t, countLimitAnnotation(m, "empty"))

	out := map[string]string{"k": "5"}
	setOrDeleteCountLimit(out, "k", api.IntPtr(0))
	assert.Equal(t, "0", out["k"], "0 (Unlimited) is stored, not deleted")
	setOrDeleteCountLimit(out, "k", api.IntPtr(9))
	assert.Equal(t, "9", out["k"])
	setOrDeleteCountLimit(out, "k", nil)
	_, ok := out["k"]
	assert.False(t, ok, "nil deletes")
}

// TestProjectSettings_CountLimitTriState: through the PUT and GET, 0 is
// stored and returned as Unlimited, null clears, a value replaces, an
// absent field keeps the stored value, and a negative value is refused
// with 422 and nothing written.
func TestProjectSettings_CountLimitTriState(t *testing.T) {
	srv, s := testServer(t)
	project := createTestProjectForSettings(t, s)
	get := func() hubclient.ProjectSettings {
		rec := doRequest(t, srv, http.MethodGet, "/api/v1/projects/"+project.ID+"/settings", nil)
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		var ps hubclient.ProjectSettings
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &ps))
		return ps
	}

	code, body := putProjectSettings(t, srv, project.ID, `{"defaultMaxTurns":0,"defaultMaxModelCalls":12}`)
	require.Equal(t, http.StatusOK, code, body)
	assert.Contains(t, body, `"defaultMaxTurns":0`, "Unlimited is serialized, not dropped")
	ps := get()
	assert.Equal(t, api.IntPtr(0), ps.DefaultMaxTurns)
	assert.Equal(t, api.IntPtr(12), ps.DefaultMaxModelCalls)

	code, body = putProjectSettings(t, srv, project.ID, `{"defaultModel":"m"}`)
	require.Equal(t, http.StatusOK, code, body)
	ps = get()
	assert.Equal(t, api.IntPtr(0), ps.DefaultMaxTurns, "an absent field keeps its value")
	assert.Equal(t, api.IntPtr(12), ps.DefaultMaxModelCalls)

	code, body = putProjectSettings(t, srv, project.ID, `{"defaultMaxTurns":null,"defaultMaxModelCalls":0}`)
	require.Equal(t, http.StatusOK, code, body)
	ps = get()
	assert.Nil(t, ps.DefaultMaxTurns, "null clears")
	assert.Equal(t, api.IntPtr(0), ps.DefaultMaxModelCalls)

	for _, field := range []string{"defaultMaxTurns", "defaultMaxModelCalls"} {
		code, body = putProjectSettings(t, srv, project.ID, `{"`+field+`":-1,"defaultModel":"other"}`)
		require.Equal(t, http.StatusUnprocessableEntity, code, body)
		assert.Contains(t, body, ErrCodeValidationError)
		assert.Contains(t, body, `"field":"`+field+`"`)
	}
	ps = get()
	assert.Equal(t, "m", ps.DefaultModel, "a refused PUT writes nothing")
	assert.Nil(t, ps.DefaultMaxTurns)
	assert.Equal(t, api.IntPtr(0), ps.DefaultMaxModelCalls)
}

// TestApplyProjectDefaults_CountLimitPointers: a project Unlimited is
// stamped as an inline 0 when the agent sets nothing; an inline value or
// Unlimited of the agent's own wins; no project value stamps nothing.
func TestApplyProjectDefaults_CountLimitPointers(t *testing.T) {
	project := func(turns string) *store.Project {
		p := &store.Project{Annotations: map[string]string{}}
		if turns != "" {
			p.Annotations[projectSettingDefaultMaxTurns] = turns
		}
		return p
	}
	for _, tc := range []struct {
		name    string
		project string
		inline  *int
		want    *int
	}{
		{"project Unlimited, inline unset", "0", nil, api.IntPtr(0)},
		{"project Unlimited, inline value", "0", api.IntPtr(7), api.IntPtr(7)},
		{"project value, inline Unlimited", "5", api.IntPtr(0), api.IntPtr(0)},
		{"project value, inline unset", "5", nil, api.IntPtr(5)},
		{"no project value, inline unset", "", nil, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ac := &store.AgentAppliedConfig{InlineConfig: &api.ScionConfig{MaxTurns: api.CloneIntPtr(tc.inline)}}
			applyProjectDefaults(ac, project(tc.project))
			assert.Equal(t, tc.want, ac.InlineConfig.MaxTurns)
		})
	}
	// The stamp copies the project value: changing it later cannot alias.
	ac := &store.AgentAppliedConfig{}
	applyProjectDefaults(ac, project("0"))
	require.NotNil(t, ac.InlineConfig)
	assert.Equal(t, api.IntPtr(0), ac.InlineConfig.MaxTurns)
	assert.Nil(t, ac.InlineConfig.MaxModelCalls)
}

// TestProjectSettings_UnlimitedTransitionSignal pins the D4 mitigation:
// unset -> 0 logs a warning and records the audit event; a value -> 0
// records only the audit event; a PUT that keeps an existing 0 records
// neither.
func TestProjectSettings_UnlimitedTransitionSignal(t *testing.T) {
	srv, s := testServer(t)
	project := createTestProjectForSettings(t, s)
	var logs bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelWarn})))
	t.Cleanup(func() { slog.SetDefault(prev) })
	const warnText = "a default count limit changed from unset to Unlimited"

	code, body := putProjectSettings(t, srv, project.ID, `{"defaultMaxTurns":0}`)
	require.Equal(t, http.StatusOK, code, body)
	recs := waitProjectUnlimitedAudits(t, s, project.ID, 1)
	require.Len(t, recs, 1)
	assert.Equal(t, "project", recs[0].TargetType)
	assert.Contains(t, recs[0].AfterSummary, `"field":"defaultMaxTurns"`)
	assert.Contains(t, recs[0].AfterSummary, `"from":"unset"`)
	assert.Equal(t, 1, strings.Count(logs.String(), warnText), logs.String())

	// A PUT that keeps the stored 0 is no change.
	code, body = putProjectSettings(t, srv, project.ID, `{"defaultMaxTurns":0}`)
	require.Equal(t, http.StatusOK, code, body)

	// A value -> 0 is audited without the warning.
	code, body = putProjectSettings(t, srv, project.ID, `{"defaultMaxModelCalls":5}`)
	require.Equal(t, http.StatusOK, code, body)
	code, body = putProjectSettings(t, srv, project.ID, `{"defaultMaxModelCalls":0}`)
	require.Equal(t, http.StatusOK, code, body)
	recs = waitProjectUnlimitedAudits(t, s, project.ID, 2)
	time.Sleep(200 * time.Millisecond) // let any stray write for the kept 0 land
	recs = projectUnlimitedAudits(t, s, project.ID)
	require.Len(t, recs, 2, "the kept 0 recorded no event")
	var fromFive int
	for _, r := range recs {
		if strings.Contains(r.AfterSummary, `"field":"defaultMaxModelCalls"`) && strings.Contains(r.AfterSummary, `"from":"5"`) {
			fromFive++
		}
	}
	assert.Equal(t, 1, fromFive)
	assert.Equal(t, 1, strings.Count(logs.String(), warnText), "only unset -> 0 logs: %s", logs.String())
}

// TestProjectSettings_UnlimitedBrokerWarning: a PUT that sets a count
// limit to 0 warns when a broker providing the project lacks
// unlimitedCountLimits, and not when every provider has it or the PUT sets
// no Unlimited.
func TestProjectSettings_UnlimitedBrokerWarning(t *testing.T) {
	srv, s := testServer(t)
	project := createTestProjectForSettings(t, s)
	linkTestBrokerForSettings(t, s, project, "unlimited-broker", "docker")
	brokerID := tid("unlimited-broker-" + t.Name())
	warnings := func(body string) []string {
		code, resp := putProjectSettings(t, srv, project.ID, body)
		require.Equal(t, http.StatusOK, code, resp)
		var out struct {
			Warnings []string `json:"warnings"`
		}
		require.NoError(t, json.Unmarshal([]byte(resp), &out))
		return out.Warnings
	}

	setBrokerCapabilities(t, s, brokerID, &store.BrokerCapabilities{ClearedFields: true})
	got := warnings(`{"defaultMaxTurns":0}`)
	require.Len(t, got, 1, "%v", got)
	assert.Contains(t, got[0], "defaultMaxTurns")
	assert.Contains(t, got[0], "unlimited-broker")

	assert.Empty(t, warnings(`{"defaultMaxModelCalls":4}`), "no Unlimited set")

	setBrokerCapabilities(t, s, brokerID, &store.BrokerCapabilities{ClearedFields: true, UnlimitedCountLimits: true})
	assert.Empty(t, warnings(`{"defaultMaxModelCalls":0}`), "every provider supports Unlimited")
}

// TestProjectSettingsResolved_CountLimitUnlimited: a project Unlimited is
// reported as set, with the raw value "0".
func TestProjectSettingsResolved_CountLimitUnlimited(t *testing.T) {
	srv, s := testServer(t)
	project := createTestProjectForSettings(t, s)
	code, body := putProjectSettings(t, srv, project.ID, `{"defaultMaxTurns":0}`)
	require.Equal(t, http.StatusOK, code, body)
	rec := doRequest(t, srv, http.MethodGet, "/api/v1/projects/"+project.ID+"/settings/resolved", nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var resp ResolvedProjectSettings
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	turns := resp.Settings[projectSettingDefaultMaxTurns]
	assert.True(t, turns.ProjectSet)
	require.NotNil(t, turns.ProjectValue)
	assert.Equal(t, "0", *turns.ProjectValue)
	assert.False(t, resp.Settings[projectSettingDefaultMaxModelCalls].ProjectSet)
}

// TestTemplateTiers_ProjectUnlimitedDispatched: a project Unlimited reaches
// the create dispatch as an inline max_turns 0, above the hub default 9
// (which travels in HubAgentDefaults), so the broker's default fill never
// replaces it.
func TestTemplateTiers_ProjectUnlimitedDispatched(t *testing.T) {
	f := newTiersFixture(t, tiersCapable)
	ctx := t.Context()
	setHubAgentDefaults(f.srv, opsettings.AgentDefaultsSettings{DefaultMaxTurns: 9})
	f.project.Annotations = map[string]string{projectSettingDefaultMaxTurns: "0"}
	req, err := f.d.buildCreateRequest(ctx, f.newAgent(t, ctx, "limits-project-unlimited", nil), "test")
	require.NoError(t, err)
	require.NotNil(t, req.InlineConfig)
	assert.Equal(t, api.IntPtr(0), req.InlineConfig.MaxTurns)
	require.NotNil(t, req.Config.HubAgentDefaults)
	assert.Equal(t, 9, req.Config.HubAgentDefaults.MaxTurns)
}

// TestBrokersWithoutUnlimitedCountLimits: the brokers lacking
// unlimitedCountLimits are named (ID when unnamed), sorted; capable brokers
// and nil entries are skipped, so a nil entry never panics.
func TestBrokersWithoutUnlimitedCountLimits(t *testing.T) {
	got := brokersWithoutUnlimitedCountLimits([]*store.RuntimeBroker{
		nil,
		{ID: "id-b", Name: "zeta"},
		{ID: "id-c", Capabilities: &store.BrokerCapabilities{ClearedFields: true}},
		{ID: "id-d", Name: "capable", Capabilities: &store.BrokerCapabilities{UnlimitedCountLimits: true}},
		nil,
	})
	assert.Equal(t, []string{"id-c", "zeta"}, got)
	assert.Nil(t, brokersWithoutUnlimitedCountLimits(nil))
	assert.Nil(t, brokersWithoutUnlimitedCountLimits([]*store.RuntimeBroker{nil}))
}
