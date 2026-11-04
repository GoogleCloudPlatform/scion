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
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	yamlv3 "gopkg.in/yaml.v3"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/brokerconvert"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
	"github.com/GoogleCloudPlatform/scion/pkg/runtimebroker"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// conversionPlanFixture is a flat Hub fixture (a legacy and a flat Runtime
// Broker) with a captured lifecycle log.
type conversionPlanFixture struct {
	*flatHubFixture
	logs *bytes.Buffer
}

func newConversionPlanFixture(t *testing.T, experimentOn bool) *conversionPlanFixture {
	t.Helper()
	f := newFlatHubFixture(t, flatHubOpts{experimentOn: experimentOn, linkFlat: true})
	logs := &bytes.Buffer{}
	f.srv.agentLifecycleLog = slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	grantDevUserRuntimeBrokerAccess(t, f.s)
	return &conversionPlanFixture{flatHubFixture: f, logs: logs}
}

// samplePlan is a small, in-cap plan whose tag tells snapshots apart.
func samplePlan(tag string) *api.ConversionPlanPresentation {
	return &api.ConversionPlanPresentation{
		SchemaVersion: api.ConversionPlanPresentationSchemaVersion,
		Instances: []api.RuntimeTargetPresentation{
			{SchemaVersion: 1, Key: "docker-" + tag, Name: "docker-" + tag, Target: &api.PresentedTarget{Type: "docker"}},
			{SchemaVersion: 1, Key: "k8s-" + tag, Name: "k8s-" + tag, Target: &api.PresentedTarget{Type: "kubernetes", Namespace: "agents"}},
			{SchemaVersion: 1, Key: "k8s-batch-" + tag, Name: "k8s-batch-" + tag, Target: &api.PresentedTarget{Type: "kubernetes", Namespace: "batch"}},
		},
		ProfileMap: map[string]string{"local": "docker-" + tag, "k8s": "k8s-" + tag},
		Findings: []api.ConversionPlanFinding{
			{Code: "profile_conflict", Severity: "notice", Path: "profiles.*", Profiles: []string{"k8s", "batch"}},
		},
		ComputedAt: time.Date(2026, 11, 3, 12, 0, 0, 0, time.UTC),
	}
}

func planBeat(raw any) map[string]any {
	body := map[string]any{"status": store.BrokerStatusOnline}
	if raw != nil {
		body["conversionPlanPresentation"] = raw
	}
	return body
}

func (f *conversionPlanFixture) stored(t *testing.T) *api.ConversionPlanPresentation {
	t.Helper()
	b, err := f.s.GetRuntimeBroker(context.Background(), f.legacy.ID)
	require.NoError(t, err)
	return b.ConversionPlanPresentation
}

func (f *conversionPlanFixture) get(t *testing.T) *httptest.ResponseRecorder {
	t.Helper()
	return doRequest(t, f.srv, http.MethodGet, "/api/v1/runtime-brokers/"+f.legacy.ID+"/conversion-plan", nil)
}

func decodePlanResponse(t *testing.T, rec *httptest.ResponseRecorder) api.ConversionPlanResponse {
	t.Helper()
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var resp api.ConversionPlanResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	return resp
}

func TestConversionPlanHeartbeat_StoredFromBrokerSelfAndServed(t *testing.T) {
	f := newConversionPlanFixture(t, true)
	p := samplePlan("a")
	sendAsBroker(t, f.srv, f.legacy.ID, planBeat(p))
	require.NotNil(t, f.stored(t))

	resp := decodePlanResponse(t, f.get(t))
	assert.Equal(t, f.legacy.ID, resp.BrokerID)
	assert.Equal(t, p.Instances, resp.Plan.Instances)
	assert.Equal(t, p.ProfileMap, resp.Plan.ProfileMap)
	assert.Equal(t, p.Findings, resp.Plan.Findings)
	require.NotNil(t, resp.ObservedAt)

	// The broker's own GET never carries the plan.
	rec := doRequest(t, f.srv, http.MethodGet, "/api/v1/runtime-brokers/"+f.legacy.ID, nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.NotContains(t, rec.Body.String(), "docker-a")
}

// TestConversionPlanHeartbeat_AbsentKeepsStored: an omitted field (an older
// broker, or a broker whose settings could not be read) keeps the stored
// snapshot.
func TestConversionPlanHeartbeat_AbsentKeepsStored(t *testing.T) {
	f := newConversionPlanFixture(t, true)
	sendAsBroker(t, f.srv, f.legacy.ID, planBeat(samplePlan("a")))
	sendAsBroker(t, f.srv, f.legacy.ID, planBeat(nil))
	sendAsBroker(t, f.srv, f.legacy.ID, planBeat(json.RawMessage("null")))
	got := f.stored(t)
	require.NotNil(t, got)
	assert.Equal(t, "docker-a", got.Instances[0].Key)
}

// TestConversionPlanHeartbeat_UnknownSchemaVersionNotStored: a schema
// version this Hub does not know is never stored, and the stored snapshot
// is kept.
func TestConversionPlanHeartbeat_UnknownSchemaVersionNotStored(t *testing.T) {
	f := newConversionPlanFixture(t, true)
	sendAsBroker(t, f.srv, f.legacy.ID, planBeat(samplePlan("a")))
	for _, v := range []int{0, 2, 99} {
		p := samplePlan(fmt.Sprintf("v%d", v))
		p.SchemaVersion = v
		sendAsBroker(t, f.srv, f.legacy.ID, planBeat(p))
		assert.Equal(t, "docker-a", f.stored(t).Instances[0].Key, "schema version %d", v)
	}
	// Never reported: an unknown version leaves nothing to serve.
	g := newConversionPlanFixture(t, true)
	p := samplePlan("b")
	p.SchemaVersion = 2
	sendAsBroker(t, g.srv, g.legacy.ID, planBeat(p))
	assert.Nil(t, g.stored(t))
	requireAPIError(t, g.get(t), http.StatusNotFound, ErrCodeConversionPlanNotReported)
}

// TestConversionPlanHeartbeat_OverCapDroppedAndCounted: a snapshot over
// any cap is dropped: nothing is stored, the stored one is kept, the drop
// counter grows, and the log line carries no content.
func TestConversionPlanHeartbeat_OverCapDroppedAndCounted(t *testing.T) {
	const planted = "PLANTED-CAP-VALUE-c61"
	long := planted + strings.Repeat("v", api.ConversionPlanMaxStringBytes)
	cases := map[string]func(p *api.ConversionPlanPresentation){
		"instances": func(p *api.ConversionPlanPresentation) {
			for i := 0; i <= api.ConversionPlanMaxInstances; i++ {
				p.Instances = append(p.Instances, api.RuntimeTargetPresentation{SchemaVersion: 1, Key: fmt.Sprintf("%s-%d", planted, i)})
			}
		},
		"findings": func(p *api.ConversionPlanPresentation) {
			for i := 0; i <= api.ConversionPlanMaxFindings; i++ {
				p.Findings = append(p.Findings, api.ConversionPlanFinding{Code: "c", Path: planted})
			}
		},
		"profileMap": func(p *api.ConversionPlanPresentation) {
			for i := 0; i <= api.ConversionPlanMaxProfileMap; i++ {
				p.ProfileMap[fmt.Sprintf("%s-%d", planted, i)] = "k"
			}
		},
		"string": func(p *api.ConversionPlanPresentation) { p.Findings[0].Path = long },
		"nested string": func(p *api.ConversionPlanPresentation) {
			p.Instances[0].Target.NodeSelector = map[string]string{"k": long}
		},
		"bytes": func(p *api.ConversionPlanPresentation) {
			pad := planted + strings.Repeat("b", api.ConversionPlanMaxStringBytes-len(planted))
			for i := 0; i < api.ConversionPlanMaxFindings-1; i++ {
				p.Findings = append(p.Findings, api.ConversionPlanFinding{Code: "c", Path: pad})
			}
		},
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			f := newConversionPlanFixture(t, true)
			sendAsBroker(t, f.srv, f.legacy.ID, planBeat(samplePlan("a")))
			before := f.srv.ConversionPlanDrops()
			f.logs.Reset()

			p := samplePlan("big")
			mutate(p)
			raw, err := json.Marshal(p)
			require.NoError(t, err)
			sendAsBroker(t, f.srv, f.legacy.ID, planBeat(json.RawMessage(raw)))

			assert.Equal(t, "docker-a", f.stored(t).Instances[0].Key, "the stored snapshot is kept")
			assert.Equal(t, before+1, f.srv.ConversionPlanDrops(), "the drop is counted")
			assert.Contains(t, f.logs.String(), "conversion plan dropped")
			assert.NotContains(t, f.logs.String(), planted, "the log line carries no content")
			assert.NotContains(t, f.logs.String(), "docker-big")
		})
	}
}

// TestConversionPlanHeartbeat_InCapAtTheLimitsStored: a snapshot exactly
// at the count caps is stored.
func TestConversionPlanHeartbeat_InCapAtTheLimitsStored(t *testing.T) {
	f := newConversionPlanFixture(t, true)
	p := samplePlan("edge")
	p.Instances = nil
	for i := 0; i < api.ConversionPlanMaxInstances; i++ {
		p.Instances = append(p.Instances, api.RuntimeTargetPresentation{SchemaVersion: 1, Key: fmt.Sprintf("i%d", i)})
	}
	for i := len(p.ProfileMap); i < api.ConversionPlanMaxProfileMap; i++ {
		p.ProfileMap[fmt.Sprintf("p%d", i)] = "i0"
	}
	sendAsBroker(t, f.srv, f.legacy.ID, planBeat(p))
	got := f.stored(t)
	require.NotNil(t, got)
	assert.Len(t, got.Instances, api.ConversionPlanMaxInstances)
	assert.Zero(t, f.srv.ConversionPlanDrops())
}

// TestConversionPlanHeartbeat_OnlyBrokerSelfStores: a user's heartbeat and
// another broker's identity never store the plan.
func TestConversionPlanHeartbeat_OnlyBrokerSelfStores(t *testing.T) {
	f := newConversionPlanFixture(t, true)
	path := "/api/v1/runtime-brokers/" + f.legacy.ID + "/heartbeat"

	t.Run("user", func(t *testing.T) {
		// A hub member holding broker.update on a session is admitted to the
		// heartbeat (200), so the request reaches the non-broker-self branch,
		// which must store nothing.
		user := newHubMemberUser(t, f.s, "plan-heartbeat-updater")
		rd, err := f.s.CreateRoleDefinition(context.Background(), &store.RoleDefinition{
			Name: "plan-heartbeat-broker-update", ScopeType: store.RoleScopeSystem, Permissions: []string{"broker.update"},
		})
		require.NoError(t, err)
		_, err = f.s.CreateRoleBinding(context.Background(), &store.RoleBinding{
			RoleDefinitionID: rd.ID, PrincipalType: store.RoleBindingPrincipalUser, PrincipalID: user.ID,
			ScopeType: store.RoleScopeSystem, ScopeID: "", CreatedBy: "test",
		})
		require.NoError(t, err)
		f.logs.Reset()
		rec := doRequestAsUser(t, f.srv, user, http.MethodPost, path, planBeat(samplePlan("user")))
		require.Equal(t, http.StatusOK, rec.Code, "the user heartbeat is admitted: %s", rec.Body.String())
		assert.Nil(t, f.stored(t), "a user heartbeat never stores the plan")
		assert.Contains(t, f.logs.String(), "ignoring conversionPlanPresentation sent with a request identity other than the broker itself")
	})
	t.Run("another broker", func(t *testing.T) {
		raw, err := json.Marshal(planBeat(samplePlan("other")))
		require.NoError(t, err)
		req := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(raw))
		req.Header.Set("Content-Type", "application/json")
		ident := NewBrokerIdentity(f.flat.ID)
		req = req.WithContext(contextWithIdentity(contextWithBrokerIdentity(req.Context(), ident), ident))
		f.srv.mux.ServeHTTP(httptest.NewRecorder(), req)
		assert.Nil(t, f.stored(t))
	})
}

func TestConversionPlanHeartbeat_FlatRowAndExperimentOffNotStored(t *testing.T) {
	f := newConversionPlanFixture(t, true)
	sendAsBroker(t, f.srv, f.flat.ID, planBeat(samplePlan("flat")))
	b, err := f.s.GetRuntimeBroker(context.Background(), f.flat.ID)
	require.NoError(t, err)
	assert.Nil(t, b.ConversionPlanPresentation, "a flat row never stores a plan")

	off := newConversionPlanFixture(t, false)
	sendAsBroker(t, off.srv, off.legacy.ID, planBeat(samplePlan("off")))
	assert.Nil(t, off.stored(t), "experiment off: not stored")
}

func TestConversionPlanGet_Errors(t *testing.T) {
	f := newConversionPlanFixture(t, true)
	t.Run("not reported", func(t *testing.T) {
		requireAPIError(t, f.get(t), http.StatusNotFound, ErrCodeConversionPlanNotReported)
	})
	t.Run("flat row", func(t *testing.T) {
		rec := doRequest(t, f.srv, http.MethodGet, "/api/v1/runtime-brokers/"+f.flat.ID+"/conversion-plan", nil)
		requireAPIError(t, rec, http.StatusConflict, ErrCodeRuntimeBrokerNotLegacy)
	})
	t.Run("unknown broker", func(t *testing.T) {
		rec := doRequest(t, f.srv, http.MethodGet, "/api/v1/runtime-brokers/"+tid("no-such-broker")+"/conversion-plan", nil)
		assert.Equal(t, http.StatusNotFound, rec.Code, rec.Body.String())
	})
	t.Run("experiment off", func(t *testing.T) {
		sendAsBroker(t, f.srv, f.legacy.ID, planBeat(samplePlan("a")))
		setFlatExperiment(t, f.srv, false)
		t.Cleanup(func() { setFlatExperiment(t, f.srv, true) })
		rec := f.get(t)
		assert.Equal(t, http.StatusNotFound, rec.Code, rec.Body.String())
		assert.NotContains(t, rec.Body.String(), "docker-a")
	})
}

// TestConversionPlanGet_GetOnly: every other method is refused and writes
// nothing.
func TestConversionPlanGet_GetOnly(t *testing.T) {
	f := newConversionPlanFixture(t, true)
	sendAsBroker(t, f.srv, f.legacy.ID, planBeat(samplePlan("a")))
	path := "/api/v1/runtime-brokers/" + f.legacy.ID + "/conversion-plan"
	for _, m := range []string{http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete} {
		rec := doRequest(t, f.srv, m, path, samplePlan("write"))
		assert.Equal(t, http.StatusMethodNotAllowed, rec.Code, "%s: %s", m, rec.Body.String())
		assert.Equal(t, "docker-a", f.stored(t).Instances[0].Key, "%s wrote nothing", m)
	}
}

// TestConversionPlanGet_Visibility: a caller who can read only some of the
// bound agents and providing projects sees exactly those, plus the hidden
// counts; a hidden project's name and ID never appear.
func TestConversionPlanGet_Visibility(t *testing.T) {
	f := newConversionPlanFixture(t, true)
	ctx := context.Background()
	sendAsBroker(t, f.srv, f.legacy.ID, planBeat(samplePlan("a")))

	visible := &store.Project{ID: tid("plan-visible-" + t.Name()), Name: "VisibleProj", Slug: "plan-visible-" + tidSlugSafe(t.Name())}
	require.NoError(t, f.s.CreateProject(ctx, visible))
	f.srv.createProjectMembersGroup(ctx, visible)
	require.NoError(t, f.s.AddProjectProvider(ctx, &store.ProjectProvider{ProjectID: visible.ID, BrokerID: f.legacy.ID, BrokerName: f.legacy.Name, LocalPath: fixtureProviderCheckout(t), Status: store.BrokerStatusOnline}))
	visible.Annotations = map[string]string{projectSettingActiveProfile: "local"}
	require.NoError(t, f.s.UpdateProject(ctx, visible))
	// The fixture's own project is the hidden one.
	hidden := f.project

	agent := func(slug, projectID, phase, profile string) {
		require.NoError(t, f.s.CreateAgent(ctx, &store.Agent{
			ID: tid("plan-agent-" + slug + "-" + t.Name()), Slug: slug, Name: slug,
			ProjectID: projectID, RuntimeBrokerID: f.legacy.ID, Phase: phase,
			AppliedConfig: &store.AgentAppliedConfig{Profile: profile, CreateInputs: &store.AgentCreateInputs{}},
		}))
	}
	agent("seen-running", visible.ID, "running", "local")
	agent("seen-stopped", visible.ID, "stopped", "local")
	agent("hidden-running", hidden.ID, "running", "k8s")

	user := newHubMemberUser(t, f.s, "plan-visibility-user")
	msgAuthzAddProjectMember(t, f.s, user.ID, visible.ID, visible.Slug, store.GroupMemberRoleMember)

	rec := doRequestAsUser(t, f.srv, user, http.MethodGet, "/api/v1/runtime-brokers/"+f.legacy.ID+"/conversion-plan", nil)
	resp := decodePlanResponse(t, rec)
	require.Len(t, resp.Projects.Items, 1)
	assert.Equal(t, visible.ID, resp.Projects.Items[0].ID)
	require.NotNil(t, resp.Projects.Items[0].ActiveProfile)
	assert.Equal(t, api.ConversionPlanProfileRef{Profile: "local", Mapping: "mapped", Instance: "docker-a"}, *resp.Projects.Items[0].ActiveProfile)
	assert.Equal(t, 1, resp.Projects.HiddenCount)
	require.Len(t, resp.Agents.Items, 1)
	assert.Equal(t, "seen-running", resp.Agents.Items[0].Name)
	assert.Equal(t, "docker-a", resp.Agents.Items[0].Instance)
	assert.Equal(t, 1, resp.Agents.TerminalCount)
	assert.Equal(t, 1, resp.Agents.HiddenCount)
	body := rec.Body.String()
	for _, s := range []string{hidden.ID, hidden.Name, "hidden-running"} {
		assert.NotContains(t, body, s, "a hidden project or agent is only counted")
	}
}

// TestConversionPlanGet_Capacity: the Hub plans capacity with its own
// values: a stored maxAgents of 10 over three proposed instances gives
// 4/3/3, keyed by the proposed keys.
func TestConversionPlanGet_Capacity(t *testing.T) {
	f := newConversionPlanFixture(t, true)
	ten := int64(10)
	_, err := f.s.PutBrokerSettings(context.Background(), f.legacy.ID, store.BrokerSettings{MaxAgents: &ten}, 0, "test")
	require.NoError(t, err)
	sendAsBroker(t, f.srv, f.legacy.ID, planBeat(samplePlan("a")))
	resp := decodePlanResponse(t, f.get(t))
	assert.Equal(t, "preserve_by_split", resp.Capacity.Outcome)
	require.NotNil(t, resp.Capacity.MaxAgents)
	assert.Equal(t, int64(10), *resp.Capacity.MaxAgents)
	assert.Equal(t, []api.ConversionPlanAllocation{
		{Instance: "docker-a", MaxAgents: 4}, {Instance: "k8s-a", MaxAgents: 3}, {Instance: "k8s-batch-a", MaxAgents: 3},
	}, resp.Capacity.Allocations)
	assert.Contains(t, resp.Capacity.Flags, conversionPlanFlagProposedKeys)

	// No maxAgents: an operator decision.
	require.NoError(t, f.s.DeleteBrokerSettings(context.Background(), f.legacy.ID))
	resp = decodePlanResponse(t, f.get(t))
	assert.Equal(t, "operator_decision_required", resp.Capacity.Outcome)
	assert.Empty(t, resp.Capacity.Allocations)
}

// heartbeatToMux is the Runtime Broker's Hub client for the boundary test:
// it sends each heartbeat to the Hub's mux authenticated as the broker.
type heartbeatToMux struct {
	hubclient.RuntimeBrokerService
	t   *testing.T
	srv *Server
}

func (h *heartbeatToMux) Heartbeat(_ context.Context, brokerID string, hb *hubclient.BrokerHeartbeat) (*hubclient.BrokerHeartbeatResponse, error) {
	sendAsBroker(h.t, h.srv, brokerID, hb)
	return &hubclient.BrokerHeartbeatResponse{}, nil
}

// TestConversionPlan_ProducerConsumerBoundary is the producer/consumer
// boundary test: the producer is the Runtime Broker's heartbeat (a real
// runtimebroker.HeartbeatService carrying brokerconvert.Presentation of a
// settings fixture), the consumer is the Hub's heartbeat handler and store,
// read back through the GET. A planted value outside the publishable set
// never reaches the Hub.
func TestConversionPlan_ProducerConsumerBoundary(t *testing.T) {
	f := newConversionPlanFixture(t, true)
	raw, err := os.ReadFile(filepath.Join("..", "brokerconvert", "testdata", "planted_split.yaml"))
	require.NoError(t, err)
	vs := &config.VersionedSettings{}
	require.NoError(t, yamlv3.Unmarshal(raw, vs))
	plan, err := brokerconvert.BuildLocal(brokerconvert.Input{Settings: vs, RawSettings: raw})
	require.NoError(t, err)
	want := brokerconvert.Presentation(plan, time.Date(2026, 11, 3, 12, 0, 0, 0, time.UTC))

	svc := runtimebroker.NewHeartbeatService(&heartbeatToMux{t: t, srv: f.srv}, f.legacy.ID, time.Hour, &sameHomeManager{}, nil, slog.Default())
	send := want
	svc.SetConversionPlanSource(func() *api.ConversionPlanPresentation { return send })
	require.NoError(t, svc.ForceHeartbeat(context.Background()))

	rec := f.get(t)
	resp := decodePlanResponse(t, rec)
	assert.Equal(t, want.Instances, resp.Plan.Instances)
	assert.Equal(t, want.ProfileMap, resp.Plan.ProfileMap)
	assert.Equal(t, want.Findings, resp.Plan.Findings)
	assert.True(t, want.ComputedAt.Equal(resp.Plan.ComputedAt))
	assert.NotContains(t, rec.Body.String(), "PLANTED-", "no non-publishable value reaches the Hub")

	// An absent snapshot on the next beat keeps the stored one.
	send = nil
	require.NoError(t, svc.ForceHeartbeat(context.Background()))
	assert.Equal(t, want.Instances, decodePlanResponse(t, f.get(t)).Plan.Instances)

	// An unknown schema version on the next beat is not stored.
	bumped := *want
	bumped.SchemaVersion = api.ConversionPlanPresentationSchemaVersion + 1
	bumped.Instances = nil
	send = &bumped
	require.NoError(t, svc.ForceHeartbeat(context.Background()))
	assert.Equal(t, want.Instances, decodePlanResponse(t, f.get(t)).Plan.Instances)
}

// TestConversionPlanGet_ScopedTokens: a user access token with broker:read
// reads the plan; a token of the same user without it is refused with the
// read surface's 404.
func TestConversionPlanGet_ScopedTokens(t *testing.T) {
	f := newConversionPlanFixture(t, true)
	sendAsBroker(t, f.srv, f.legacy.ID, planBeat(samplePlan("a")))
	user := newHubMemberUser(t, f.s, "plan-token-user")
	path := "/api/v1/runtime-brokers/" + f.legacy.ID + "/conversion-plan"
	mint := func(scopes ...string) string {
		key, _, err := f.srv.uatService.CreateTokenWithParams(rs4MintContext(user.ID), CreateTokenParams{
			UserID: user.ID, Name: "plan-" + tid("tok"), Boundary: hubBoundary(), Scopes: scopes,
		})
		require.NoError(t, err)
		return key
	}

	rec := doRequestWithToken(t, f.srv, mint("broker:read"), http.MethodGet, path, nil)
	resp := decodePlanResponse(t, rec)
	assert.Equal(t, "docker-a", resp.Plan.Instances[0].Key)

	rec = doRequestWithToken(t, f.srv, mint("template:read"), http.MethodGet, path, nil)
	assert.Equal(t, http.StatusNotFound, rec.Code, rec.Body.String())
	assert.NotContains(t, rec.Body.String(), ErrCodeConversionPlanNotReported, "refused by the scope check, not for a missing plan")
	assert.NotContains(t, rec.Body.String(), "docker-a")
}

// cyclingAgentsStore returns, once armed, an empty page with a next cursor
// that alternates between two values for every bound-agent list: a cursor
// cycle longer than one.
type cyclingAgentsStore struct {
	store.Store
	armed atomic.Bool
	calls atomic.Int64
}

func (c *cyclingAgentsStore) ListAgents(ctx context.Context, filter store.AgentFilter, opts store.ListOptions) (*store.ListResult[store.Agent], error) {
	if !c.armed.Load() || filter.RuntimeBrokerID == "" {
		return c.Store.ListAgents(ctx, filter, opts)
	}
	c.calls.Add(1)
	next := "cycle-a"
	if opts.Cursor == "cycle-a" {
		next = "cycle-b"
	}
	return &store.ListResult[store.Agent]{NextCursor: next}, nil
}

// TestConversionPlanGet_AgentPagingBounded: a store whose next cursor
// cycles never makes the read loop forever; the scan stops at its page
// limit and the request fails closed.
func TestConversionPlanGet_AgentPagingBounded(t *testing.T) {
	var cyc *cyclingAgentsStore
	f := newFlatHubFixture(t, flatHubOpts{experimentOn: true, linkFlat: true,
		storeWrap: func(inner store.Store, _ *storeFaultSwitch) store.Store {
			cyc = &cyclingAgentsStore{Store: inner}
			return cyc
		}})
	grantDevUserRuntimeBrokerAccess(t, f.s)
	sendAsBroker(t, f.srv, f.legacy.ID, planBeat(samplePlan("a")))
	cyc.armed.Store(true)

	rec := doRequest(t, f.srv, http.MethodGet, "/api/v1/runtime-brokers/"+f.legacy.ID+"/conversion-plan", nil)
	assert.Equal(t, http.StatusInternalServerError, rec.Code, rec.Body.String())
	assert.NotContains(t, rec.Body.String(), "docker-a", "no partial plan is served")
	assert.Equal(t, int64(conversionPlanMaxAgentPages), cyc.calls.Load(), "the scan stops at its page limit")
}

// TestConversionPlanHeartbeat_StoresImageRegistryRedacted: the Hub stores a
// reported plan's instance image registries with the broker record's display
// redaction, whatever the Runtime Broker sent, so the GET never returns a
// registry's userinfo; a plain registry is stored unchanged.
func TestConversionPlanHeartbeat_StoresImageRegistryRedacted(t *testing.T) {
	f := newConversionPlanFixture(t, true)
	p := samplePlan("a")
	p.Instances[0].AgentDefaults = &api.PresentedAgentDefaults{ImageRegistry: "https://user:s3cret@registry.example/team"}
	p.Instances[1].AgentDefaults = &api.PresentedAgentDefaults{ImageRegistry: "registry.example/plain"}
	sendAsBroker(t, f.srv, f.legacy.ID, planBeat(p))

	stored := f.stored(t)
	require.NotNil(t, stored)
	assert.Equal(t, "https://***@registry.example/team", stored.Instances[0].AgentDefaults.ImageRegistry, "stored redacted")
	assert.Equal(t, "registry.example/plain", stored.Instances[1].AgentDefaults.ImageRegistry, "a plain registry is unchanged")

	rec := f.get(t)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.NotContains(t, rec.Body.String(), "s3cret", "the GET never returns a registry's userinfo")
	assert.Contains(t, rec.Body.String(), "https://***@registry.example/team")
}

// TestConversionPlanHeartbeat_IgnoredPlanLogsNoInfo: a legacy Runtime Broker
// sends its plan on every heartbeat, so a plan the Hub ignores (experiment
// off, the default; an unknown schema version; a flat row) is logged at Debug
// only, never at Info or above.
func TestConversionPlanHeartbeat_IgnoredPlanLogsNoInfo(t *testing.T) {
	assertNoInfo := func(t *testing.T, logs string, reason string) {
		t.Helper()
		for _, line := range strings.Split(logs, "\n") {
			if strings.Contains(line, "conversion plan") {
				assert.Contains(t, line, "level=DEBUG", "ignored plan logged above Debug: %s", line)
			}
		}
		assert.Contains(t, logs, "reason="+reason)
	}
	off := newConversionPlanFixture(t, false)
	off.logs.Reset()
	sendAsBroker(t, off.srv, off.legacy.ID, planBeat(samplePlan("a")))
	sendAsBroker(t, off.srv, off.legacy.ID, planBeat(samplePlan("a")))
	assertNoInfo(t, off.logs.String(), conversionPlanIgnoredExperimentOff)

	on := newConversionPlanFixture(t, true)
	p := samplePlan("v")
	p.SchemaVersion = api.ConversionPlanPresentationSchemaVersion + 1
	on.logs.Reset()
	sendAsBroker(t, on.srv, on.legacy.ID, planBeat(p))
	assertNoInfo(t, on.logs.String(), conversionPlanIgnoredSchemaVersion)

	// A plan reported for a flat row is ignored the same way.
	on.logs.Reset()
	sendAsBroker(t, on.srv, on.flat.ID, planBeat(samplePlan("f")))
	assertNoInfo(t, on.logs.String(), conversionPlanIgnoredFlatRow)
}

// TestConversionPlanMetrics_DropCountExported: the drop count is exported as
// scion.hub.broker.conversion_plan_dropped, a single data point with no
// attributes (it counts only; no snapshot content).
func TestConversionPlanMetrics_DropCountExported(t *testing.T) {
	f := newConversionPlanFixture(t, true)
	reader := sdkmetric.NewManualReader()
	require.NoError(t, f.srv.RegisterConversionPlanMetrics(sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))))
	exported := func() int64 {
		var rm metricdata.ResourceMetrics
		require.NoError(t, reader.Collect(context.Background(), &rm))
		for _, sm := range rm.ScopeMetrics {
			for _, m := range sm.Metrics {
				if m.Name != conversionPlanDroppedMetric {
					continue
				}
				sum, ok := m.Data.(metricdata.Sum[int64])
				require.True(t, ok, "counter type %T", m.Data)
				require.Len(t, sum.DataPoints, 1)
				assert.Zero(t, sum.DataPoints[0].Attributes.Len(), "the drop count carries no attributes")
				return sum.DataPoints[0].Value
			}
		}
		t.Fatalf("%s not exported", conversionPlanDroppedMetric)
		return 0
	}
	assert.Zero(t, exported())
	p := samplePlan("big")
	p.Findings[0].Path = strings.Repeat("x", api.ConversionPlanMaxStringBytes+1)
	sendAsBroker(t, f.srv, f.legacy.ID, planBeat(p))
	assert.Equal(t, int64(1), exported())
	assert.Equal(t, int64(1), f.srv.ConversionPlanDrops())
}

// TestConversionPlanGet_CapacityRealInputs: with no maxAgents setting, the
// plan uses the broker-scoped entitlement binding as the inherited limit; and
// the broker's active reservations are compared with the receiving instance's
// proposed allocation.
func TestConversionPlanGet_CapacityRealInputs(t *testing.T) {
	ctx := context.Background()
	t.Run("entitlement binding", func(t *testing.T) {
		f := newConversionPlanFixture(t, true)
		def, err := f.s.GetLimitDefinitionByName(ctx, store.LimitMaxAgentsPerBroker)
		require.NoError(t, err)
		seedBinding(t, f.s, def.ID, store.EntitlementSubjectSystemDefault, "", store.QuotaScopeBroker, f.legacy.ID, 7)
		sendAsBroker(t, f.srv, f.legacy.ID, planBeat(samplePlan("a")))
		resp := decodePlanResponse(t, f.get(t))
		assert.Equal(t, "operator_decision_required", resp.Capacity.Outcome)
		assert.Nil(t, resp.Capacity.MaxAgents)
		require.NotEmpty(t, resp.Capacity.Reasons)
		assert.Contains(t, strings.Join(resp.Capacity.Reasons, "\n"), "inherits 7; each of the 3 instances would inherit 7, an aggregate of 21")
	})
	t.Run("active reservations", func(t *testing.T) {
		f := newConversionPlanFixture(t, true)
		four := int64(4)
		_, err := f.s.PutBrokerSettings(ctx, f.legacy.ID, store.BrokerSettings{MaxAgents: &four}, 0, "test")
		require.NoError(t, err)
		for _, id := range []string{"plan-res-1", "plan-res-2", "plan-res-3"} {
			_, err := f.srv.quotaService.Reserve(ctx, store.LimitMaxAgentsPerBroker, f.legacy.ID, store.QuotaScopeBroker, f.legacy.ID, id)
			require.NoError(t, err)
		}
		sendAsBroker(t, f.srv, f.legacy.ID, planBeat(samplePlan("a")))
		resp := decodePlanResponse(t, f.get(t))
		assert.Equal(t, "preserve_by_split", resp.Capacity.Outcome)
		assert.Equal(t, []api.ConversionPlanAllocation{
			{Instance: "docker-a", MaxAgents: 2}, {Instance: "k8s-a", MaxAgents: 1}, {Instance: "k8s-batch-a", MaxAgents: 1},
		}, resp.Capacity.Allocations)
		assert.Contains(t, strings.Join(resp.Capacity.Flags, "\n"), "active_above_allocation: 3 active reservations")
	})
}
