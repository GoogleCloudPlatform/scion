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
	"maps"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/experiments"
	"github.com/GoogleCloudPlatform/scion/pkg/secret"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	crListedGSA   = "listed@example-project.iam.gserviceaccount.com"
	crUnlistedGSA = "unlisted@example-project.iam.gserviceaccount.com"
)

// crProfile is a Cloud Run profile whose broker reported, recently and at
// the current version, native identity support, an allow-list holding
// only crListedGSA and a block account.
func crProfile(now time.Time) store.BrokerProfile {
	at := now.Add(-time.Minute)
	return store.BrokerProfile{
		Name: "cr",
		Type: "cloudrun",
		ServiceAccountMappings: []store.BrokerProfileSAMapping{
			{GSA: crListedGSA, Source: api.BrokerSASourceCloudRunAllowlist},
		},
		MappingsReported:       true,
		MappingsComplete:       true,
		MappingsHash:           "h-cr",
		MappingsReportedAt:     &at,
		MappingsReportVersion:  api.BrokerSAReportVersion,
		CloudRunNativeIdentity: true,
		CloudRunBlockAccount:   true,
	}
}

// crReportedBroker is a broker holding profiles, with "cr" as its default
// profile.
func crReportedBroker(profiles ...store.BrokerProfile) *store.RuntimeBroker {
	return &store.RuntimeBroker{
		ID:             "b1",
		Name:           "broker-cr",
		DefaultProfile: "cr",
		Profiles:       profiles,
	}
}

func crAssign(email string) *store.GCPIdentityConfig {
	return &store.GCPIdentityConfig{MetadataMode: store.GCPMetadataModeAssign, ServiceAccountEmail: email}
}

func crMode(mode string) *store.GCPIdentityConfig {
	return &store.GCPIdentityConfig{MetadataMode: mode}
}

// setCloudRunNativeIdentity turns hub.cloudrun_native_identity on or off
// on srv through its operational settings.
func setCloudRunNativeIdentity(t *testing.T, srv *Server, on bool) {
	t.Helper()
	fakeStore := newFakeHubSettingStore()
	fakeStore.seed("experiments", json.RawMessage(fmt.Sprintf(`{"overrides":{%q:%t}}`, experiments.CloudRunNativeIdentity, on)))
	ops := NewOperationalSettings(fakeStore, emptyKoanf(), emptyKoanf())
	_, err := ops.Refresh(context.Background())
	require.NoError(t, err)
	srv.SetOperationalSettings(ops)
	require.Equal(t, on, srv.experimentEnabled(experiments.CloudRunNativeIdentity))
}

func TestCloudRunIdentityRefusal(t *testing.T) {
	now := time.Date(2026, 11, 4, 12, 0, 0, 0, time.UTC)
	noBlock := func(p *store.BrokerProfile) { p.CloudRunBlockAccount = false }
	v2 := func(p *store.BrokerProfile) { p.MappingsReportVersion = cloudRunSAReportMinVersion - 1 }
	tests := []struct {
		name    string
		profile func(p *store.BrokerProfile)
		sel     string
		gcpID   *store.GCPIdentityConfig
		want    string
	}{
		{name: "assign listed passes", gcpID: crAssign(crListedGSA)},
		{name: "assign listed in another case passes", gcpID: crAssign("Listed@Example-Project.iam.gserviceaccount.com")},
		{name: "assign not listed is refused", gcpID: crAssign(crUnlistedGSA), want: cloudRunRefusalNotAttachable},
		{name: "assign with an empty list is refused", gcpID: crAssign(crListedGSA),
			profile: func(p *store.BrokerProfile) { p.ServiceAccountMappings = nil }, want: cloudRunRefusalNotAttachable},
		{name: "assign without an account is left to the broker", gcpID: crAssign("")},
		{name: "block with a block account passes", gcpID: crMode(store.GCPMetadataModeBlock)},
		{name: "block without a block account is refused", gcpID: crMode(store.GCPMetadataModeBlock), profile: noBlock, want: cloudRunRefusalNoBlockAccount},
		{name: "unset with a block account passes", gcpID: nil},
		{name: "unset without a block account is refused", gcpID: nil, profile: noBlock, want: cloudRunRefusalNoBlockAccount},
		{name: "hub-default passthrough runs as block and is refused without a block account",
			gcpID: &store.GCPIdentityConfig{MetadataMode: store.GCPMetadataModePassthrough, RequireLocalRuntime: true}, profile: noBlock, want: cloudRunRefusalNoBlockAccount},
		{name: "hub-default passthrough with a block account passes",
			gcpID: &store.GCPIdentityConfig{MetadataMode: store.GCPMetadataModePassthrough, RequireLocalRuntime: true}},
		{name: "passthrough is refused", gcpID: crMode(store.GCPMetadataModePassthrough), want: cloudRunRefusalPassthrough},
		{name: "cloudrun-instances profile refuses assign", gcpID: crAssign(crListedGSA),
			profile: func(p *store.BrokerProfile) {
				p.Type, p.CloudRunNativeIdentity, p.CloudRunBlockAccount, p.ServiceAccountMappings = "cloudrun-instances", false, false, nil
			}, want: cloudRunRefusalUnsupported},
		{name: "cloudrun-instances profile refuses block", gcpID: crMode(store.GCPMetadataModeBlock),
			profile: func(p *store.BrokerProfile) { p.Type, p.CloudRunNativeIdentity = "cloudrun-instances", false }, want: cloudRunRefusalUnsupported},
		{name: "cloudrun-instances profile refuses passthrough", gcpID: crMode(store.GCPMetadataModePassthrough),
			profile: func(p *store.BrokerProfile) { p.Type, p.CloudRunNativeIdentity = "cloudrun-instances", false }, want: cloudRunRefusalPassthrough},
		{name: "version-2 report refuses assign", gcpID: crAssign(crListedGSA), profile: v2, want: cloudRunRefusalBrokerUpgrade},
		{name: "version-2 report refuses block", gcpID: crMode(store.GCPMetadataModeBlock), profile: v2, want: cloudRunRefusalBrokerUpgrade},
		{name: "version-2 report refuses passthrough", gcpID: crMode(store.GCPMetadataModePassthrough), profile: v2, want: cloudRunRefusalBrokerUpgrade},
		{name: "version-2 report refuses unset", gcpID: nil, profile: v2, want: cloudRunRefusalBrokerUpgrade},
		{name: "absent report refuses assign", gcpID: crAssign(crListedGSA), profile: func(p *store.BrokerProfile) {
			clearProfileSAReport(p)
		}, want: cloudRunRefusalBrokerUpgrade},
		{name: "version-2 report refuses even when stale", gcpID: crAssign(crListedGSA), profile: func(p *store.BrokerProfile) {
			v2(p)
			at := now.Add(-profileSAReportFreshFor - time.Hour)
			p.MappingsReportedAt = &at
		}, want: cloudRunRefusalBrokerUpgrade},
		{name: "stale current report refuses", gcpID: crAssign(crListedGSA), want: cloudRunRefusalReportStale, profile: func(p *store.BrokerProfile) {
			at := now.Add(-profileSAReportFreshFor - time.Second)
			p.MappingsReportedAt = &at
		}},
		{name: "current report without a confirmation time refuses", gcpID: crMode(store.GCPMetadataModeBlock), want: cloudRunRefusalReportStale, profile: func(p *store.BrokerProfile) {
			p.MappingsReportedAt = nil
		}},
		{name: "incomplete current report refuses", gcpID: crAssign(crListedGSA), want: cloudRunRefusalReportStale, profile: func(p *store.BrokerProfile) {
			p.MappingsComplete = false
			p.MappingsIncompleteReason = api.BrokerSAReportForceRuntime
		}},
		{name: "report just inside the threshold refuses", gcpID: crAssign(crUnlistedGSA), profile: func(p *store.BrokerProfile) {
			at := now.Add(-profileSAReportFreshFor)
			p.MappingsReportedAt = &at
		}, want: cloudRunRefusalNotAttachable},
		{name: "Kubernetes profile is not checked", gcpID: crMode(store.GCPMetadataModePassthrough), profile: func(p *store.BrokerProfile) { p.Type = "kubernetes" }},
		{name: "docker profile is not checked", gcpID: crMode(store.GCPMetadataModePassthrough), profile: func(p *store.BrokerProfile) { p.Type = "docker" }},
		{name: "cloudrun-sandbox profile is not checked", gcpID: crMode(store.GCPMetadataModePassthrough), profile: func(p *store.BrokerProfile) { p.Type = "cloudrun-sandbox" }},
		{name: "profile not on a native broker is refused", gcpID: crAssign(crListedGSA), sel: "other", want: cloudRunRefusalProfileUnknown},
		{name: "profile not on a native broker is refused for block", gcpID: crMode(store.GCPMetadataModeBlock), sel: "other", want: cloudRunRefusalProfileUnknown},
		{name: "profile not on a broker without native support is not checked", gcpID: crAssign(crUnlistedGSA), sel: "other",
			profile: func(p *store.BrokerProfile) { p.CloudRunNativeIdentity = false }},
		{name: "broker default profile is used when the agent names none", gcpID: crAssign(crUnlistedGSA), sel: "-", want: cloudRunRefusalNotAttachable},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := crProfile(now)
			if tt.profile != nil {
				tt.profile(&p)
			}
			broker := crReportedBroker(p)
			sel := "cr"
			switch tt.sel {
			case "":
			case "-":
				sel = ""
			default:
				sel = tt.sel
			}
			got := cloudRunIdentityRefusal(broker, sel, "agent-x", tt.gcpID, now)
			if tt.want == "" {
				assert.Nil(t, got)
				return
			}
			require.NotNil(t, got)
			assert.Equal(t, tt.want, got.CloudRun)
			wantProfile := sel
			if wantProfile == "" {
				wantProfile = "cr"
			}
			assert.Equal(t, wantProfile, got.Profile)
			assert.Equal(t, "broker-cr", got.Broker)
			assert.Equal(t, "agent-x", got.Agent)
			assert.Empty(t, got.Account, "a Cloud Run refusal carries no account")
		})
	}
	assert.Nil(t, cloudRunIdentityRefusal(nil, "cr", "agent-x", crAssign(crUnlistedGSA), now), "no broker")
}

// Each Cloud Run refusal is relayed as a 400 identity_not_mapped with
// checkedBy=hub_report and its reason, names the agent, the profile and
// the broker, and never names a service account.
func TestCloudRunIdentityRefusal_RelayNamesNoAccount(t *testing.T) {
	cases := map[string]string{
		cloudRunRefusalNotAttachable:  "cloudrun.assignable_service_accounts",
		cloudRunRefusalNoBlockAccount: "cloudrun.block_service_account",
		cloudRunRefusalPassthrough:    "passthrough is not available on Cloud Run Instances; assign an account or use block",
		cloudRunRefusalUnsupported:    "turn the experiment off",
		cloudRunRefusalBrokerUpgrade:  "upgrade the broker or turn the experiment off",
		cloudRunRefusalProfileUnknown: "is not known to the hub",
		cloudRunRefusalReportStale:    "broker report stale or broker downgraded",
	}
	for reason, want := range cases {
		t.Run(reason, func(t *testing.T) {
			pre := &identityNotMappedPrecheck{Profile: "cr", Broker: "broker-cr", CloudRun: reason, Agent: "agent-x", Mode: store.GCPMetadataModeBlock}
			rec := httptest.NewRecorder()
			require.True(t, relayIdentityMappingError(rec, fmt.Errorf("dispatch start: %w", pre)))
			assert.Equal(t, http.StatusBadRequest, rec.Code)
			var body struct {
				Error struct {
					Code    string                 `json:"code"`
					Message string                 `json:"message"`
					Details map[string]interface{} `json:"details"`
				} `json:"error"`
			}
			require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
			assert.Equal(t, ErrCodeIdentityNotMapped, body.Error.Code)
			assert.Contains(t, body.Error.Message, want)
			assert.Contains(t, body.Error.Message, `agent "agent-x"`)
			assert.Contains(t, body.Error.Message, `profile "cr" of broker "broker-cr"`)
			assert.NotContains(t, body.Error.Message, "@", "no account in the message")
			assert.NotContains(t, rec.Body.String(), "iam.gserviceaccount.com", "no account anywhere in the response")
			assert.Equal(t, identityCheckedByHubReport, body.Error.Details["checkedBy"])
			assert.Equal(t, reason, body.Error.Details["reason"])
			_, hasAccount := body.Error.Details[api.BrokerErrDetailServiceAccount]
			assert.False(t, hasAccount)
			assert.True(t, isConfirmedStartNotActedOnError(pre), "classified as not acted on")
		})
	}
	// An unset mode names itself rather than an explicit block.
	pre := &identityNotMappedPrecheck{Profile: "cr", Broker: "broker-cr", CloudRun: cloudRunRefusalNoBlockAccount, Agent: "agent-x"}
	assert.Contains(t, pre.translate().Message, "an unset GCP identity")
}

// A Cloud Run refusal raised on the executing node is carried on the
// dispatch row and rebuilt as the same typed error, although it has no
// account.
func TestCloudRunIdentityRefusal_CrossNodeEnvelope(t *testing.T) {
	pre := &identityNotMappedPrecheck{Profile: "cr", Broker: "broker-cr", CloudRun: cloudRunRefusalBrokerUpgrade, Agent: "agent-x", Mode: store.GCPMetadataModeAssign}
	result := dispatchFailureResult(fmt.Errorf("dispatch start: %w", pre))
	require.NotEmpty(t, result)
	assert.NotContains(t, result, "iam.gserviceaccount.com")
	err := dispatchFailureError(&store.BrokerDispatch{Op: "start", Result: result, Error: "text"})
	var got *identityNotMappedPrecheck
	require.ErrorAs(t, err, &got)
	assert.Equal(t, *pre, *got)
	_, ok := identityMappingDispatchError(err)
	assert.True(t, ok)
}

// crDispatcher stores a broker with profiles (mutate changes it before it
// is stored) and returns a dispatcher with a mock client and an unstored
// agent on profileName with gcpID (crPersist stores it). gateOn puts
// hub.cloudrun_native_identity in the dispatch experiments.
func crDispatcher(t *testing.T, profiles []store.BrokerProfile, profileName string, gcpID *store.GCPIdentityConfig, gateOn bool, mutate ...func(b *store.RuntimeBroker)) (*HTTPAgentDispatcher, *mockRuntimeBrokerClient, *store.Agent) {
	t.Helper()
	ctx := context.Background()
	memStore := createTestStore(t)
	broker := &store.RuntimeBroker{
		ID:       tid("host-cr"),
		Name:     "broker-cr",
		Slug:     "broker-cr",
		Endpoint: "http://localhost:9800",
		Status:   store.BrokerStatusOnline,
		Profiles: profiles,
	}
	for _, m := range mutate {
		m(broker)
	}
	require.NoError(t, memStore.CreateRuntimeBroker(ctx, broker))
	mockClient := &mockRuntimeBrokerClient{}
	d := NewHTTPAgentDispatcherWithClient(memStore, mockClient, false, slog.Default())
	if gateOn {
		d.SetDispatchExperimentsProvider(func() []string { return []string{experiments.CloudRunNativeIdentity} })
	}
	agent := &store.Agent{
		ID:              tid("agent-cr"),
		Name:            "cr-agent",
		Slug:            "cr-agent",
		ProjectID:       tid("project-cr"),
		RuntimeBrokerID: tid("host-cr"),
		AppliedConfig: &store.AgentAppliedConfig{
			HarnessConfig: "claude",
			Profile:       profileName,
			GCPIdentity:   gcpID,
		},
	}
	return d, mockClient, agent
}

// Every dispatch path runs the Cloud Run check before any token, run or
// broker call while the experiment is on; with it off, or on a Kubernetes
// or docker profile, nothing new is refused.
func TestDispatch_CloudRunIdentityPrecheck(t *testing.T) {
	ctx := context.Background()
	now := time.Now()
	v2Broker := crProfile(now)
	v2Broker.MappingsReportVersion = cloudRunSAReportMinVersion - 1
	for _, op := range precheckOps(ctx) {
		t.Run(op.name+" refuses an account not listed", func(t *testing.T) {
			d, m, a := crDispatcher(t, []store.BrokerProfile{crProfile(now)}, "cr", crAssign(crUnlistedGSA), true)
			tokens := &precheckTokenCounter{}
			d.SetTokenGenerator(tokens)
			err := op.run(d, a)
			var pre *identityNotMappedPrecheck
			require.ErrorAs(t, err, &pre)
			assert.Equal(t, cloudRunRefusalNotAttachable, pre.CloudRun)
			assert.False(t, op.called(m), "no broker call")
			assert.Zero(t, tokens.authorize, "no token authorized")
			assert.Zero(t, tokens.sign, "no token signed")
		})
		t.Run(op.name+" refuses passthrough", func(t *testing.T) {
			d, m, a := crDispatcher(t, []store.BrokerProfile{crProfile(now)}, "cr", crMode(store.GCPMetadataModePassthrough), true)
			err := op.run(d, a)
			var pre *identityNotMappedPrecheck
			require.ErrorAs(t, err, &pre)
			assert.Equal(t, cloudRunRefusalPassthrough, pre.CloudRun)
			assert.False(t, op.called(m))
		})
		t.Run(op.name+" refuses a version-2 broker", func(t *testing.T) {
			d, m, a := crDispatcher(t, []store.BrokerProfile{v2Broker}, "cr", crAssign(crListedGSA), true)
			err := op.run(d, a)
			var pre *identityNotMappedPrecheck
			require.ErrorAs(t, err, &pre)
			assert.Equal(t, cloudRunRefusalBrokerUpgrade, pre.CloudRun)
			assert.False(t, op.called(m))
		})
		t.Run(op.name+" dispatches a listed account", func(t *testing.T) {
			d, m, a := crDispatcher(t, []store.BrokerProfile{crProfile(now)}, "cr", crAssign(crListedGSA), true)
			crPersist(t, d, a)
			err := op.run(d, a)
			_, isIdentity := identityMappingDispatchError(err)
			assert.False(t, isIdentity, "no identity refusal: %v", err)
			assert.True(t, op.called(m), "broker called")
		})
		t.Run(op.name+" gate off refuses nothing", func(t *testing.T) {
			d, m, a := crDispatcher(t, []store.BrokerProfile{v2Broker}, "cr", crAssign(crUnlistedGSA), false)
			err := op.run(d, a)
			_, isIdentity := identityMappingDispatchError(err)
			assert.False(t, isIdentity, "no identity refusal: %v", err)
			assert.True(t, op.called(m), "broker called")
		})
		t.Run(op.name+" docker profile is not checked", func(t *testing.T) {
			p := store.BrokerProfile{Name: "local", Type: "docker"}
			d, m, a := crDispatcher(t, []store.BrokerProfile{p}, "local", crMode(store.GCPMetadataModePassthrough), true)
			err := op.run(d, a)
			_, isIdentity := identityMappingDispatchError(err)
			assert.False(t, isIdentity, "no identity refusal: %v", err)
			assert.True(t, op.called(m), "broker called")
		})
		t.Run(op.name+" Kubernetes profile keeps its own check", func(t *testing.T) {
			d, m, a := crDispatcher(t, []store.BrokerProfile{precheckProfile(now)}, "gke", crAssign(precheckOther), true)
			err := op.run(d, a)
			_, isIdentity := identityMappingDispatchError(err)
			assert.False(t, isIdentity, "a mapped GSA dispatches: %v", err)
			assert.True(t, op.called(m), "broker called")

			d, m, a = crDispatcher(t, []store.BrokerProfile{precheckProfile(now)}, "gke", crAssign(precheckGSA), true)
			err = op.run(d, a)
			var pre *identityNotMappedPrecheck
			require.ErrorAs(t, err, &pre)
			assert.Empty(t, pre.CloudRun, "the Kubernetes refusal, not a Cloud Run one")
			assert.False(t, op.called(m))
		})
	}
}

func TestBrokerProfileNativelyAssigns(t *testing.T) {
	now := time.Now()
	docker := func(p *store.BrokerProfile) { *p = store.BrokerProfile{Name: "cr", Type: "docker"} }
	tests := []struct {
		name    string
		profile func(p *store.BrokerProfile)
		sel     string
		want    bool
	}{
		{name: "current report with support", want: true},
		{name: "stale report with support still counts", want: true, profile: func(p *store.BrokerProfile) {
			at := now.Add(-24 * time.Hour)
			p.MappingsReportedAt = &at
		}},
		{name: "broker default profile", sel: "-", want: true},
		{name: "version-2 report", profile: func(p *store.BrokerProfile) { p.MappingsReportVersion = cloudRunSAReportMinVersion - 1 }},
		{name: "no report", profile: func(p *store.BrokerProfile) { clearProfileSAReport(p) }},
		{name: "support not reported", profile: func(p *store.BrokerProfile) { p.CloudRunNativeIdentity = false }},
		{name: "cloudrun-instances", profile: func(p *store.BrokerProfile) { p.Type = "cloudrun-instances" }},
		{name: "kubernetes", profile: func(p *store.BrokerProfile) { p.Type = "kubernetes" }},
		{name: "docker", profile: docker},
		{name: "profile not on a native broker", sel: "other", want: true},
		{name: "profile not on a broker without native support", sel: "other", profile: func(p *store.BrokerProfile) { p.CloudRunNativeIdentity = false }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := crProfile(now)
			if tt.profile != nil {
				tt.profile(&p)
			}
			broker := crReportedBroker(p)
			sel := "cr"
			switch tt.sel {
			case "":
			case "-":
				sel = ""
			default:
				sel = tt.sel
			}
			assert.Equal(t, tt.want, brokerProfileNativelyAssigns(broker, sel))
		})
	}
	assert.False(t, brokerProfileNativelyAssigns(nil, "cr"))
}

// attachCRBroker stores a broker with profile p and puts the fixture's
// agent on it under p's name.
func (f *gcpMintFixture) attachCRBroker(t *testing.T, p store.BrokerProfile) {
	t.Helper()
	f.attachBroker(t, []store.BrokerProfile{p}, p.Name)
}

// attachBroker stores a broker with profiles and puts the fixture's agent
// on it under agentProfile. It returns the broker ID.
func (f *gcpMintFixture) attachBroker(t *testing.T, profiles []store.BrokerProfile, agentProfile string) string {
	t.Helper()
	ctx := context.Background()
	brokerID := tid("broker-mint-" + strings.ReplaceAll(agentProfile, "-", ""))
	require.NoError(t, f.Store.CreateRuntimeBroker(ctx, &store.RuntimeBroker{
		ID: brokerID, Name: "broker-" + agentProfile, Slug: "broker-" + agentProfile,
		Endpoint: "http://localhost:9800", Status: store.BrokerStatusOnline,
		Profiles: profiles,
	}))
	f.mutateAgent(t, func(a *store.Agent) {
		a.RuntimeBrokerID = brokerID
		a.AppliedConfig.Profile = agentProfile
	})
	return brokerID
}

func grantHasGCPScope(t *testing.T, f *gcpMintFixture) bool {
	t.Helper()
	grant, err := f.Server.AuthorizeAgentToken(context.Background(), f.getAgent(t))
	require.NoError(t, err)
	for _, s := range grant.Scopes {
		if strings.HasPrefix(string(s), ScopeGCPTokenPrefix) {
			return true
		}
	}
	return false
}

// A natively assigned agent's token carries no GCP token scope; an assign
// agent on docker, with the experiment off, or on a version-2 broker keeps
// it.
func TestAuthorizeAgentToken_CloudRunNativeIdentityScope(t *testing.T) {
	now := time.Now()
	t.Run("baseline assign agent has the scope", func(t *testing.T) {
		f := newGCPMintFixture(t, "crscope0")
		require.True(t, grantHasGCPScope(t, f))
		require.Contains(t, mustAuthorizedScopes(t, f), GCPTokenScopeForSA(f.SA.ID))
	})
	t.Run("natively assigned agent lacks the scope", func(t *testing.T) {
		f := newGCPMintFixture(t, "crscope1")
		f.attachCRBroker(t, crProfile(now))
		setCloudRunNativeIdentity(t, f.Server, true)
		assert.False(t, grantHasGCPScope(t, f))
		assert.NotEmpty(t, mustAuthorizedScopes(t, f), "the role scopes stay")
	})
	t.Run("docker profile keeps the scope with the gate on", func(t *testing.T) {
		f := newGCPMintFixture(t, "crscope2")
		f.attachCRBroker(t, store.BrokerProfile{Name: "local", Type: "docker"})
		setCloudRunNativeIdentity(t, f.Server, true)
		assert.True(t, grantHasGCPScope(t, f))
	})
	t.Run("gate off keeps the scope on Cloud Run", func(t *testing.T) {
		f := newGCPMintFixture(t, "crscope3")
		f.attachCRBroker(t, crProfile(now))
		setCloudRunNativeIdentity(t, f.Server, false)
		assert.True(t, grantHasGCPScope(t, f))
	})
	t.Run("version-2 broker keeps the scope", func(t *testing.T) {
		f := newGCPMintFixture(t, "crscope4")
		p := crProfile(now)
		p.MappingsReportVersion = cloudRunSAReportMinVersion - 1
		f.attachCRBroker(t, p)
		setCloudRunNativeIdentity(t, f.Server, true)
		assert.True(t, grantHasGCPScope(t, f))
	})
}

func mustAuthorizedScopes(t *testing.T, f *gcpMintFixture) []AgentTokenScope {
	t.Helper()
	grant, err := f.Server.AuthorizeAgentToken(context.Background(), f.getAgent(t))
	require.NoError(t, err)
	return grant.Scopes
}

// Both mint endpoints refuse a natively assigned agent from its stored
// record, even though the token it presents still carries the GCP token
// scope (issued while the experiment was off). The refusal names no
// account, and no token is generated.
func TestAgentGCPToken_CloudRunNativeIdentityRefused(t *testing.T) {
	f := newGCPMintFixture(t, "crmint")
	gen := &countingGCPTokenGenerator{}
	f.Server.SetGCPTokenGenerator(gen)
	f.attachCRBroker(t, crProfile(time.Now()))
	setCloudRunNativeIdentity(t, f.Server, true)

	assertMintDenied(t, f)
	assert.Zero(t, gen.calls, "no GCP token generated")
	for _, tc := range []struct {
		path string
		body interface{}
	}{
		{"/api/v1/agent/gcp-token", nil},
		{"/api/v1/agent/gcp-identity-token", map[string]string{"audience": "https://example.com"}},
	} {
		rec := doRequestWithAgentToken(t, f.Server, http.MethodPost, tc.path, tc.body, f.Token)
		require.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
		assert.Contains(t, rec.Body.String(), "metadata server", tc.path)
		assert.NotContains(t, rec.Body.String(), f.SA.Email, tc.path)
		assert.NotContains(t, rec.Body.String(), "iam.gserviceaccount.com", tc.path)
	}
	assert.Zero(t, gen.calls, "no GCP token generated")

	// The same agent with the experiment off is served again.
	f.Server.SetGCPTokenGenerator(&mockGCPTokenGenerator{email: "hub-sa@test-hub-project.iam.gserviceaccount.com"})
	setCloudRunNativeIdentity(t, f.Server, false)
	assertMintAllowed(t, f)
}

// An assign agent that is not natively assigned is still served with the
// experiment on: on docker, and on a Cloud Run broker below report
// version 3.
func TestAgentGCPToken_CloudRunNativeIdentityNonNativeServed(t *testing.T) {
	t.Run("docker", func(t *testing.T) {
		f := newGCPMintFixture(t, "crmintdocker")
		f.attachCRBroker(t, store.BrokerProfile{Name: "local", Type: "docker"})
		setCloudRunNativeIdentity(t, f.Server, true)
		assertMintAllowed(t, f)
	})
	t.Run("version-2 Cloud Run broker", func(t *testing.T) {
		f := newGCPMintFixture(t, "crmintv2")
		p := crProfile(time.Now())
		p.MappingsReportVersion = cloudRunSAReportMinVersion - 1
		f.attachCRBroker(t, p)
		setCloudRunNativeIdentity(t, f.Server, true)
		assertMintAllowed(t, f)
	})
}

// countingGCPTokenGenerator counts token generations and refuses them.
type countingGCPTokenGenerator struct {
	mockGCPTokenGenerator
	calls int
}

func (g *countingGCPTokenGenerator) GenerateAccessToken(ctx context.Context, email string, scopes []string) (*GCPAccessToken, error) {
	g.calls++
	return nil, fmt.Errorf("unexpected access token generation")
}

func (g *countingGCPTokenGenerator) GenerateIDToken(ctx context.Context, email, audience string) (*GCPIDToken, error) {
	g.calls++
	return nil, fmt.Errorf("unexpected identity token generation")
}

// A heartbeat's Cloud Run report stores the allow-list and both
// capability booleans on the profile; a change of either boolean is
// stored, and a profile the report leaves out has them cleared.
func TestApplyProfileSAMappings_CloudRunCapability(t *testing.T) {
	now := time.Now()
	profiles := []store.BrokerProfile{{Name: "cr", Type: "cloudrun"}, {Name: "gke", Type: "kubernetes"}}
	r := brokerProfileSAMappings{
		Name:                   "cr",
		ServiceAccountMappings: []store.BrokerProfileSAMapping{{GSA: crListedGSA, Source: api.BrokerSASourceCloudRunAllowlist}},
		Complete:               true,
		ReportVersion:          api.BrokerSAReportVersion,
		CloudRunNativeIdentity: true,
		CloudRunBlockAccount:   true,
	}
	gke := brokerProfileSAMappings{Name: "gke", ServiceAccountMappings: []store.BrokerProfileSAMapping{}, Complete: true, ReportVersion: api.BrokerSAReportVersion}

	changed, requested := applyProfileSAMappings(profiles, []brokerProfileSAMappings{r, gke},
		[]brokerProfileSAMappingsHash{{Name: "cr", Hash: "h1"}, {Name: "gke", Hash: "g1"}}, now)
	require.True(t, changed)
	assert.False(t, requested)
	cr := profiles[0]
	assert.True(t, cr.MappingsReported)
	assert.True(t, cr.MappingsComplete)
	assert.Equal(t, api.BrokerSAReportVersion, cr.MappingsReportVersion)
	assert.True(t, cr.CloudRunNativeIdentity)
	assert.True(t, cr.CloudRunBlockAccount)
	assert.Equal(t, r.ServiceAccountMappings, cr.ServiceAccountMappings)
	assert.True(t, brokerProfileNativelyAssigns(&store.RuntimeBroker{Profiles: profiles}, "cr"))

	r.CloudRunBlockAccount = false
	changed, _ = applyProfileSAMappings(profiles, []brokerProfileSAMappings{r, gke},
		[]brokerProfileSAMappingsHash{{Name: "cr", Hash: "h2"}, {Name: "gke", Hash: "g1"}}, now)
	require.True(t, changed)
	assert.False(t, profiles[0].CloudRunBlockAccount, "a removed block account is stored")
	assert.True(t, profiles[0].CloudRunNativeIdentity)

	// Only the Kubernetes profile is named: the Cloud Run report is cleared.
	changed, _ = applyProfileSAMappings(profiles, []brokerProfileSAMappings{gke},
		[]brokerProfileSAMappingsHash{{Name: "gke", Hash: "g1"}}, now)
	require.True(t, changed)
	assert.False(t, profiles[0].MappingsReported)
	assert.False(t, profiles[0].CloudRunNativeIdentity)
	assert.False(t, profiles[0].CloudRunBlockAccount)
	assert.Nil(t, profiles[0].ServiceAccountMappings)
}

// A Cloud Run profile's report is never counted as a Kubernetes profile
// by the mapping warnings and the per-account status view.
func TestIsKubernetesBrokerProfile_CloudRunReportNotCounted(t *testing.T) {
	assert.False(t, isKubernetesBrokerProfile(crProfile(time.Now())))
	inst := crProfile(time.Now())
	inst.Type = "cloudrun-instances"
	assert.False(t, isKubernetesBrokerProfile(inst))
	assert.True(t, isKubernetesBrokerProfile(precheckProfile(time.Now())))
	assert.True(t, isKubernetesBrokerProfile(store.BrokerProfile{Name: "gke", Type: "custom", MappingsReported: true}))
}

// A complete Kubernetes report at version 2 is still decided by the
// Kubernetes precheck: version 3 adds only Cloud Run reports.
func TestKubernetesIdentityNotMapped_Version2ReportRefuses(t *testing.T) {
	now := time.Now()
	p := precheckProfile(now)
	p.MappingsReportVersion = kubernetesSAReportMinVersion
	require.Equal(t, 2, kubernetesSAReportMinVersion)
	broker := &store.RuntimeBroker{ID: "b1", Name: "broker-a", Profiles: []store.BrokerProfile{p}}
	got := kubernetesIdentityNotMapped(broker, "gke", precheckAssign(precheckGSA), now)
	require.NotNil(t, got)
	assert.Equal(t, identityNotMappedPrecheck{Account: precheckGSA, Profile: "gke", Broker: "broker-a", Namespace: "agents"}, *got)
	assert.Nil(t, kubernetesIdentityNotMapped(broker, "gke", precheckAssign(precheckOther), now), "a mapped GSA passes")
	d := profileSAMappingDecision(broker, "gke", precheckGSA, now)
	assert.Equal(t, SAMappingStateNotMapped, d.State, "the assign picker and status view decide it too")
}

// On every dispatch path, a version-2 Kubernetes report refuses an
// unmapped account with the experiment off and with it on.
func TestDispatch_KubernetesIdentityPrecheckVersion2Report(t *testing.T) {
	ctx := context.Background()
	p := precheckProfile(time.Now())
	p.MappingsReportVersion = kubernetesSAReportMinVersion
	for _, gateOn := range []bool{false, true} {
		for _, op := range precheckOps(ctx) {
			t.Run(fmt.Sprintf("%s gate on %t", op.name, gateOn), func(t *testing.T) {
				d, m, a := crDispatcher(t, []store.BrokerProfile{p}, "gke", crAssign(precheckGSA), gateOn)
				err := op.run(d, a)
				var pre *identityNotMappedPrecheck
				require.ErrorAs(t, err, &pre)
				assert.Equal(t, precheckGSA, pre.Account)
				assert.Empty(t, pre.CloudRun, "the Kubernetes refusal")
				assert.False(t, op.called(m), "no broker call")

				d, m, a = crDispatcher(t, []store.BrokerProfile{p}, "gke", crAssign(precheckOther), gateOn)
				err = op.run(d, a)
				_, isIdentity := identityMappingDispatchError(err)
				assert.False(t, isIdentity, "a mapped GSA dispatches: %v", err)
				assert.True(t, op.called(m), "broker called")
			})
		}
	}
}

// The mint addition: with the experiment on, an agent whose profile stops
// resolving (it is gone from the broker row) on a broker that runs native
// Cloud Run for another profile is natively assigned: its token loses the
// GCP token scope and both mint endpoints refuse it.
func TestAgentGCPToken_CloudRunProfileGoneFromBrokerRow(t *testing.T) {
	ctx := context.Background()
	f := newGCPMintFixture(t, "crgone")
	native := crProfile(time.Now())
	native.Name = "cr2"
	local := store.BrokerProfile{Name: "local", Type: "docker"}
	brokerID := f.attachBroker(t, []store.BrokerProfile{native, local}, "local")
	setCloudRunNativeIdentity(t, f.Server, true)
	require.True(t, grantHasGCPScope(t, f), "a docker profile that resolves keeps the scope")
	assertMintAllowed(t, f)

	broker, err := f.Store.GetRuntimeBroker(ctx, brokerID)
	require.NoError(t, err)
	broker.Profiles = []store.BrokerProfile{native}
	require.NoError(t, f.Store.UpdateRuntimeBroker(ctx, broker))

	assert.False(t, grantHasGCPScope(t, f), "the scope is withheld once the profile is gone")
	assertMintDenied(t, f)
	rec := doRequestWithAgentToken(t, f.Server, http.MethodPost, "/api/v1/agent/gcp-token", nil, f.Token)
	assert.Contains(t, rec.Body.String(), "metadata server")
	assert.NotContains(t, rec.Body.String(), f.SA.Email)
}

// The same agent with the experiment off keeps its scope and is served,
// even though its profile does not resolve.
func TestAgentGCPToken_CloudRunUnresolvableProfileGateOff(t *testing.T) {
	f := newGCPMintFixture(t, "crgoneoff")
	f.attachBroker(t, []store.BrokerProfile{crProfile(time.Now())}, "missing")
	setCloudRunNativeIdentity(t, f.Server, false)
	assert.True(t, grantHasGCPScope(t, f))
	assertMintAllowed(t, f)
}

// (c) at dispatch: with the experiment on, a profile the hub cannot
// resolve on a broker that runs native Cloud Run is refused on every path
// before any token, run or broker call, naming no account; with it off,
// or on a broker without native Cloud Run, it is not refused.
func TestDispatch_CloudRunProfileUnknown(t *testing.T) {
	ctx := context.Background()
	now := time.Now()
	for _, op := range precheckOps(ctx) {
		t.Run(op.name+" refused", func(t *testing.T) {
			d, m, a := crDispatcher(t, []store.BrokerProfile{crProfile(now)}, "missing", crAssign(crListedGSA), true)
			tokens := &precheckTokenCounter{}
			d.SetTokenGenerator(tokens)
			err := op.run(d, a)
			var pre *identityNotMappedPrecheck
			require.ErrorAs(t, err, &pre)
			assert.Equal(t, cloudRunRefusalProfileUnknown, pre.CloudRun)
			ime, ok := identityMappingDispatchError(err)
			require.True(t, ok)
			assert.Contains(t, ime.Message, `profile "missing" of broker "broker-cr" is not known to the hub`)
			assert.NotContains(t, ime.Message, "@")
			assert.False(t, op.called(m), "no broker call")
			assert.Zero(t, tokens.authorize, "no token authorized")
			assert.Zero(t, tokens.sign, "no token signed")
		})
		t.Run(op.name+" gate off is not refused", func(t *testing.T) {
			d, _, a := crDispatcher(t, []store.BrokerProfile{crProfile(now)}, "missing", crAssign(crListedGSA), false)
			err := op.run(d, a)
			_, isIdentity := identityMappingDispatchError(err)
			assert.False(t, isIdentity, "no identity refusal: %v", err)
		})
		t.Run(op.name+" broker without native Cloud Run is not refused", func(t *testing.T) {
			d, _, a := crDispatcher(t, []store.BrokerProfile{{Name: "local", Type: "docker"}}, "missing", crAssign(crListedGSA), true)
			err := op.run(d, a)
			_, isIdentity := identityMappingDispatchError(err)
			assert.False(t, isIdentity, "no identity refusal: %v", err)
		})
	}
}

// crPersist stores the project and the agent row of a crDispatcher agent.
func crPersist(t *testing.T, d *HTTPAgentDispatcher, a *store.Agent) {
	t.Helper()
	ctx := context.Background()
	require.NoError(t, d.store.CreateProject(ctx, &store.Project{ID: a.ProjectID, Name: "cr-native", Slug: "cr-native"}))
	require.NoError(t, d.store.CreateAgent(ctx, a))
}

// crRecorded reports whether the stored agent row carries the recorded
// native decision.
func crRecorded(t *testing.T, st store.Store, id string) bool {
	t.Helper()
	got, err := st.GetAgent(context.Background(), id)
	require.NoError(t, err)
	return got.AppliedConfig != nil && got.AppliedConfig.NativeCloudRun
}

// crMarkerSent returns what op sent the broker: the create field, and the
// resolved-env marker and whether it was present.
func crMarkerSent(t *testing.T, opName string, m *mockRuntimeBrokerClient) (field bool, env string, envSet bool) {
	t.Helper()
	switch opName {
	case "start":
		env, envSet = m.lastResolvedEnv[envNativeCloudRun]
		return false, env, envSet
	case "restart":
		env, envSet = m.lastRestartResolvedEnv[envNativeCloudRun]
		return false, env, envSet
	default:
		require.NotNil(t, m.lastCreateReq)
		if m.lastCreateReq.Config != nil && m.lastCreateReq.Config.GCPIdentity != nil {
			field = m.lastCreateReq.Config.GCPIdentity.NativeCloudRun
		}
		env, envSet = m.lastCreateReq.ResolvedEnv[envNativeCloudRun]
		return field, env, envSet
	}
}

// The marker reaches the broker only on a dispatch the hub decided native:
// the create field on create paths, the resolved-env key on every path.
// A client-supplied native flag (on the agent's config) or marker value
// (in its env) is never copied: a dispatch decided non-native sends
// neither, and the gate off sends neither.
func TestDispatch_CloudRunNativeMarker(t *testing.T) {
	ctx := context.Background()
	now := time.Now()
	for _, op := range precheckOps(ctx) {
		t.Run(op.name+" native sends the marker", func(t *testing.T) {
			d, m, a := crDispatcher(t, []store.BrokerProfile{crProfile(now)}, "cr", crAssign(crListedGSA), true)
			crPersist(t, d, a)
			_ = op.run(d, a)
			require.True(t, op.called(m), "broker called")
			field, env, envSet := crMarkerSent(t, op.name, m)
			if op.name != "start" && op.name != "restart" {
				assert.True(t, field, "create field set")
			}
			assert.True(t, envSet)
			assert.Equal(t, "1", env)
		})
		t.Run(op.name+" non-native ignores client-supplied values", func(t *testing.T) {
			d, m, a := crDispatcher(t, []store.BrokerProfile{{Name: "local", Type: "docker"}}, "local", crAssign(crListedGSA), true)
			a.AppliedConfig.NativeCloudRun = true
			a.AppliedConfig.Env = map[string]string{envNativeCloudRun: "1"}
			_ = op.run(d, a)
			require.True(t, op.called(m), "broker called")
			field, _, envSet := crMarkerSent(t, op.name, m)
			assert.False(t, field, "a client-supplied native flag is not copied")
			assert.False(t, envSet, "a client-supplied marker is removed")
		})
		t.Run(op.name+" gate off sends no marker", func(t *testing.T) {
			d, m, a := crDispatcher(t, []store.BrokerProfile{crProfile(now)}, "cr", crAssign(crListedGSA), false)
			_ = op.run(d, a)
			require.True(t, op.called(m), "broker called")
			field, _, envSet := crMarkerSent(t, op.name, m)
			assert.False(t, field)
			assert.False(t, envSet)
		})
	}
}

// The native decision is recorded on the agent row before the request is
// built, and on the agent the caller holds; a non-native decision leaves
// the record as it is; the gate off records nothing.
func TestMarkCloudRunNative_Records(t *testing.T) {
	ctx := context.Background()
	now := time.Now()

	d, _, a := crDispatcher(t, []store.BrokerProfile{crProfile(now)}, "cr", crAssign(crListedGSA), true)
	crPersist(t, d, a)
	got, err := d.gcpIdentityPrecheck(ctx, a)
	require.NoError(t, err)
	assert.True(t, cloudRunNativeFrom(got))
	assert.True(t, crRecorded(t, d.store, a.ID), "recorded on the row")
	assert.True(t, a.AppliedConfig.NativeCloudRun, "recorded on the caller's agent")

	// A later dispatch decided non-native (gate off) keeps the record.
	d.SetDispatchExperimentsProvider(func() []string { return nil })
	got, err = d.gcpIdentityPrecheck(ctx, a)
	require.NoError(t, err)
	assert.False(t, cloudRunNativeFrom(got))
	assert.True(t, crRecorded(t, d.store, a.ID), "kept until a confirmed stop or delete")

	d2, _, b := crDispatcher(t, []store.BrokerProfile{crProfile(now)}, "cr", crAssign(crListedGSA), false)
	crPersist(t, d2, b)
	got, err = d2.gcpIdentityPrecheck(ctx, b)
	require.NoError(t, err)
	assert.False(t, cloudRunNativeFrom(got))
	assert.False(t, crRecorded(t, d2.store, b.ID), "gate off records nothing")
}

// crFaultStore fails the broker read or the native flag write, and counts
// broker reads.
type crFaultStore struct {
	store.Store
	brokerErr   error
	setErr      error
	brokerReads int
}

func (s *crFaultStore) GetRuntimeBroker(ctx context.Context, id string) (*store.RuntimeBroker, error) {
	s.brokerReads++
	if s.brokerErr != nil {
		return nil, s.brokerErr
	}
	return s.Store.GetRuntimeBroker(ctx, id)
}

func (s *crFaultStore) SetAgentNativeCloudRun(ctx context.Context, id string, native bool) error {
	if s.setErr != nil {
		return s.setErr
	}
	return s.Store.SetAgentNativeCloudRun(ctx, id, native)
}

// A record write that fails refuses the dispatch on every path before any
// token, run or broker call, as a request that was not sent.
func TestDispatch_CloudRunNativeRecordFailureRefuses(t *testing.T) {
	ctx := context.Background()
	now := time.Now()
	injected := errors.New("injected store fault")
	for _, op := range precheckOps(ctx) {
		t.Run(op.name, func(t *testing.T) {
			d, m, a := crDispatcher(t, []store.BrokerProfile{crProfile(now)}, "cr", crAssign(crListedGSA), true)
			crPersist(t, d, a)
			d.store = &crFaultStore{Store: d.store, setErr: injected}
			tokens := &precheckTokenCounter{}
			d.SetTokenGenerator(tokens)
			err := op.run(d, a)
			require.ErrorIs(t, err, injected)
			assert.ErrorIs(t, err, errStartRequestNotSent)
			assert.True(t, isConfirmedStartNotActedOnError(err))
			assert.False(t, op.called(m), "no broker call")
			assert.Zero(t, tokens.authorize, "no token authorized")
			assert.Zero(t, tokens.sign, "no token signed")
		})
	}
}

// A broker read that fails refuses the native decision (the precheck
// before it leaves an unreadable broker to the dispatch); with the
// experiment off nothing is read.
func TestMarkCloudRunNative_BrokerReadFailureRefuses(t *testing.T) {
	ctx := context.Background()
	injected := errors.New("injected store fault")
	d, _, a := crDispatcher(t, []store.BrokerProfile{crProfile(time.Now())}, "cr", crAssign(crListedGSA), true)
	fs := &crFaultStore{Store: d.store, brokerErr: injected}
	d.store = fs
	got, err := d.markCloudRunNative(ctx, a)
	require.ErrorIs(t, err, injected)
	assert.ErrorIs(t, err, errStartRequestNotSent)
	assert.False(t, cloudRunNativeFrom(got))

	d.SetDispatchExperimentsProvider(func() []string { return nil })
	fs.brokerReads = 0
	got, err = d.markCloudRunNative(ctx, a)
	require.NoError(t, err)
	assert.False(t, cloudRunNativeFrom(got))
	assert.Zero(t, fs.brokerReads, "experiment off: the broker is not read")
}

// The record is removed only by a confirmed stop or delete; a failed stop
// keeps it.
func TestDispatch_CloudRunNativeClearedOnConfirmedStopOrDelete(t *testing.T) {
	ctx := context.Background()
	now := time.Now()
	setup := func(t *testing.T) (*HTTPAgentDispatcher, *mockRuntimeBrokerClient, *store.Agent) {
		d, m, a := crDispatcher(t, []store.BrokerProfile{crProfile(now)}, "cr", crAssign(crListedGSA), true)
		crPersist(t, d, a)
		require.NoError(t, d.store.SetAgentNativeCloudRun(ctx, a.ID, true))
		require.True(t, crRecorded(t, d.store, a.ID))
		a.AppliedConfig.NativeCloudRun = true
		return d, m, a
	}
	t.Run("stop failure keeps it", func(t *testing.T) {
		d, m, a := setup(t)
		m.returnErr = errors.New("broker unreachable")
		require.Error(t, d.DispatchAgentStop(ctx, a))
		assert.True(t, crRecorded(t, d.store, a.ID))
	})
	t.Run("confirmed stop clears it", func(t *testing.T) {
		d, m, a := setup(t)
		require.NoError(t, d.DispatchAgentStop(ctx, a))
		require.True(t, m.stopCalled)
		assert.False(t, crRecorded(t, d.store, a.ID))
		assert.False(t, a.AppliedConfig.NativeCloudRun)
	})
	t.Run("delete failure keeps it", func(t *testing.T) {
		d, m, a := setup(t)
		m.returnErr = errors.New("broker unreachable")
		require.Error(t, d.DispatchAgentDelete(ctx, a, false, false, false, time.Time{}))
		assert.True(t, crRecorded(t, d.store, a.ID))
	})
	t.Run("a stop from a copy that does not show it leaves it set", func(t *testing.T) {
		d, _, a := setup(t)
		a.AppliedConfig.NativeCloudRun = false
		require.NoError(t, d.DispatchAgentStop(ctx, a))
		assert.True(t, crRecorded(t, d.store, a.ID), "fails closed: the GCP token path stays closed")
	})
	t.Run("confirmed delete clears it", func(t *testing.T) {
		d, m, a := setup(t)
		require.NoError(t, d.DispatchAgentDelete(ctx, a, false, false, false, time.Time{}))
		require.True(t, m.deleteCalled)
		assert.False(t, crRecorded(t, d.store, a.ID))
	})
}

// The recorded decision alone withholds the scope and refuses mint, with
// the experiment off and a profile that is not native: it is read from the
// agent row, so it holds across a hub restart.
func TestAgentGCPToken_CloudRunRecordedNativeWithholds(t *testing.T) {
	f := newGCPMintFixture(t, "crrecorded")
	f.attachCRBroker(t, store.BrokerProfile{Name: "local", Type: "docker"})
	setCloudRunNativeIdentity(t, f.Server, false)
	require.True(t, grantHasGCPScope(t, f))
	require.NoError(t, f.Store.SetAgentNativeCloudRun(context.Background(), f.AgentID, true))
	assert.False(t, grantHasGCPScope(t, f))
	assertMintDenied(t, f)

	require.NoError(t, f.Store.SetAgentNativeCloudRun(context.Background(), f.AgentID, false))
	assert.True(t, grantHasGCPScope(t, f), "served again once the record is removed")
	assertMintAllowed(t, f)
}

// crMintFaultStore fails GetRuntimeBroker while its switch is active and
// counts the reads it saw then.
type crMintFaultStore struct {
	store.Store
	fault *storeFaultSwitch
	reads int
}

func (s *crMintFaultStore) GetRuntimeBroker(ctx context.Context, id string) (*store.RuntimeBroker, error) {
	if !s.fault.Active() {
		return s.Store.GetRuntimeBroker(ctx, id)
	}
	s.reads++
	return nil, errors.New("injected broker lookup fault")
}

// A broker lookup error with the experiment on fails the grant with
// errMintLookup and answers both mint endpoints 500 with no GCP token
// generated; with the experiment off nothing reads the broker.
func TestAgentGCPToken_CloudRunLookupErrorRefuses(t *testing.T) {
	srv, s := testServer(t)
	wrapped, fault := installStoreFault(t, srv, func(inner store.Store, f *storeFaultSwitch) *crMintFaultStore {
		return &crMintFaultStore{Store: inner, fault: f}
	})
	f := newGCPMintFixtureOn(t, "crlookup", srv, s)
	f.attachCRBroker(t, crProfile(time.Now()))
	gen := &countingGCPTokenGenerator{}

	setCloudRunNativeIdentity(t, srv, true)
	fault.Arm()
	_, err := srv.AuthorizeAgentToken(context.Background(), f.getAgent(t))
	require.ErrorIs(t, err, errMintLookup)
	srv.SetGCPTokenGenerator(gen)
	for _, tc := range []struct {
		path string
		body interface{}
	}{
		{"/api/v1/agent/gcp-token", nil},
		{"/api/v1/agent/gcp-identity-token", map[string]string{"audience": "https://example.com"}},
	} {
		rec := doRequestWithAgentToken(t, srv, http.MethodPost, tc.path, tc.body, f.Token)
		assert.Equal(t, http.StatusInternalServerError, rec.Code, tc.path)
	}
	assert.Zero(t, gen.calls, "no GCP token generated")

	setCloudRunNativeIdentity(t, srv, false)
	wrapped.reads = 0
	srv.SetGCPTokenGenerator(&mockGCPTokenGenerator{email: "hub-sa@test-hub-project.iam.gserviceaccount.com"})
	assert.True(t, grantHasGCPScope(t, f))
	assertMintAllowed(t, f)
	assert.Zero(t, wrapped.reads, "experiment off: the broker is not read")
}

// The gs:// link fetch resolves no account for a natively assigned agent
// (recorded or live), and a lookup error answers the lookup-failed reason.
func TestResolveCurrentSA_CloudRunNativeIdentity(t *testing.T) {
	ctx := context.Background()
	f := newGCPMintFixture(t, "crgcs")
	f.attachCRBroker(t, crProfile(time.Now()))

	setCloudRunNativeIdentity(t, f.Server, false)
	sa, reason := f.Server.resolveCurrentSA(ctx, f.getAgent(t))
	require.NotNil(t, sa, "baseline: the account resolves")
	assert.Empty(t, reason)

	setCloudRunNativeIdentity(t, f.Server, true)
	sa, reason = f.Server.resolveCurrentSA(ctx, f.getAgent(t))
	assert.Nil(t, sa)
	assert.Equal(t, GCSLinkReasonNativeIdentity, reason)

	setCloudRunNativeIdentity(t, f.Server, false)
	require.NoError(t, f.Store.SetAgentNativeCloudRun(ctx, f.AgentID, true))
	sa, reason = f.Server.resolveCurrentSA(ctx, f.getAgent(t))
	assert.Nil(t, sa)
	assert.Equal(t, GCSLinkReasonNativeIdentity, reason, "the recorded decision alone gates it")
}

func TestResolveCurrentSA_CloudRunLookupError(t *testing.T) {
	srv, s := testServer(t)
	_, fault := installStoreFault(t, srv, func(inner store.Store, f *storeFaultSwitch) *crMintFaultStore {
		return &crMintFaultStore{Store: inner, fault: f}
	})
	f := newGCPMintFixtureOn(t, "crgcsfault", srv, s)
	f.attachCRBroker(t, crProfile(time.Now()))
	setCloudRunNativeIdentity(t, srv, true)
	fault.Arm()
	sa, reason := srv.resolveCurrentSA(context.Background(), f.getAgent(t))
	assert.Nil(t, sa)
	assert.Equal(t, GCSLinkReasonSALookupFailed, reason)
}

// The marker key is a reserved env target and never persisted from a
// resolved env, so no stored, secret, progeny or template env carries it.
func TestNativeCloudRunEnvKeyReserved(t *testing.T) {
	assert.True(t, secret.IsReservedEnvTarget(envNativeCloudRun))
	assert.False(t, shouldPersistResolvedEnvKey(envNativeCloudRun, map[string]api.EnvKind{envNativeCloudRun: api.EnvKindPlain}))

	env := map[string]string{envNativeCloudRun: "1", "OTHER": "x"}
	cls := map[string]api.EnvKind{envNativeCloudRun: api.EnvKindPlain}
	applyNativeCloudRunEnv(env, &cls, false)
	assert.Equal(t, map[string]string{"OTHER": "x"}, env)
	_, classified := cls[envNativeCloudRun]
	assert.False(t, classified)
	applyNativeCloudRunEnv(env, &cls, true)
	assert.Equal(t, "1", env[envNativeCloudRun])
}

// The create field travels as native_cloud_run, and is left out when
// false.
func TestRemoteGCPIdentityConfig_NativeCloudRunWire(t *testing.T) {
	on, err := json.Marshal(RemoteGCPIdentityConfig{MetadataMode: store.GCPMetadataModeAssign, NativeCloudRun: true})
	require.NoError(t, err)
	assert.Contains(t, string(on), `"native_cloud_run":true`)
	off, err := json.Marshal(RemoteGCPIdentityConfig{MetadataMode: store.GCPMetadataModeAssign})
	require.NoError(t, err)
	assert.NotContains(t, string(off), "native_cloud_run")
}

// The SA removal impact view lists Kubernetes mappings only: a Cloud Run
// profile's allow-list entry for the account is not a broker mapping,
// with the experiment off.
func TestGCPSARemove_CloudRunAllowListNotABrokerMapping(t *testing.T) {
	srv, s, owner, _, _, project := setupGCPAuthzTest(t)
	ctx := context.Background()
	sa := mkSA(t, s, "sa-rm-cr", "rm-cr@example.com", store.ScopeProject, project.ID, owner.ID)
	cr := crProfile(time.Now())
	cr.ServiceAccountMappings = []store.BrokerProfileSAMapping{{GSA: sa.Email, Source: api.BrokerSASourceCloudRunAllowlist}}
	inst := crProfile(time.Now())
	inst.Name, inst.Type = "cr-inst", "cloudrun-instances"
	inst.ServiceAccountMappings = cr.ServiceAccountMappings
	broker := &store.RuntimeBroker{
		ID: tid("broker-rm-cr"), Name: "rm-cr-broker", Slug: "rm-cr-broker", Status: store.BrokerStatusOnline,
		Profiles: []store.BrokerProfile{
			{Name: "k8s", Type: "kubernetes", MappingsReported: true,
				ServiceAccountMappings: []store.BrokerProfileSAMapping{{GSA: sa.Email}}},
			cr,
			inst,
		},
	}
	require.NoError(t, s.CreateRuntimeBroker(ctx, broker))
	require.NoError(t, s.AddProjectProvider(ctx, &store.ProjectProvider{
		ProjectID: project.ID, BrokerID: broker.ID, BrokerName: broker.Name, Status: "online",
	}))

	rec := doRequestAsUser(t, srv, owner, http.MethodDelete, saRemovePath(project.ID, sa.ID), nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var resp DeleteGCPServiceAccountResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.Equal(t, []GCPServiceAccountImpactMapping{
		{BrokerID: broker.ID, BrokerName: broker.Name, Profile: "k8s"},
	}, resp.Impact.BrokerMappings)
}

// A config patch may send a stored reserved env key (SCION_*,
// GCE_METADATA_*) back byte-identical, but may not add one, change its
// value or remove it, and never names the native Cloud Run marker, even
// echoed. The refusal names the key, never its value.
func TestValidateAgentConfigPatch_ReservedEnvEdits(t *testing.T) {
	ctx := context.Background()
	srv, _ := testServer(t)
	stored := map[string]string{"A": "1", "SCION_HUB_ENDPOINT": "https://hub", envNativeCloudRun: "1"}
	agent := &store.Agent{ID: tid("agent-patch-cr"), ProjectID: tid("project-patch-cr"),
		AppliedConfig: &store.AgentAppliedConfig{Env: stored}}
	cases := []struct {
		name    string
		env     map[string]string
		refused string
	}{
		{name: "unchanged echo that drops a stored marker passes", env: map[string]string{"A": "2", "SCION_HUB_ENDPOINT": "https://hub"}},
		{name: "env untouched passes", env: nil},
		{name: "changed value refused", env: map[string]string{"SCION_HUB_ENDPOINT": "https://other"}, refused: "SCION_HUB_ENDPOINT"},
		{name: "new key refused", env: map[string]string{"SCION_HUB_ENDPOINT": "https://hub", "GCE_METADATA_HOST": "h"}, refused: "GCE_METADATA_HOST"},
		{name: "marker refused even echoed", env: map[string]string{"SCION_HUB_ENDPOINT": "https://hub", envNativeCloudRun: "1"}, refused: envNativeCloudRun},
		{name: "new metadata mode refused", env: map[string]string{"SCION_HUB_ENDPOINT": "https://hub", "SCION_METADATA_MODE": "passthrough"}, refused: "SCION_METADATA_MODE"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			ok := srv.validateAgentConfigPatch(ctx, rec, agent, &api.ScionConfig{Env: tc.env}, false)
			if tc.refused == "" {
				assert.True(t, ok, rec.Body.String())
				return
			}
			assert.False(t, ok)
			assert.Equal(t, http.StatusBadRequest, rec.Code)
			assert.Contains(t, rec.Body.String(), tc.refused)
			assert.NotContains(t, rec.Body.String(), "https://", "the value is never named")
		})
	}

	// Without the stored marker, a byte-identical echo of the other stored
	// reserved key passes, and dropping it is a removal.
	clean := &store.Agent{ID: tid("agent-patch-cr2"), ProjectID: tid("project-patch-cr"),
		AppliedConfig: &store.AgentAppliedConfig{Env: map[string]string{"A": "1", "SCION_HUB_ENDPOINT": "https://hub"}}}
	rec := httptest.NewRecorder()
	assert.True(t, srv.validateAgentConfigPatch(ctx, rec, clean, &api.ScionConfig{Env: map[string]string{"A": "2", "SCION_HUB_ENDPOINT": "https://hub"}}, false), rec.Body.String())
	rec = httptest.NewRecorder()
	assert.False(t, srv.validateAgentConfigPatch(ctx, rec, clean, &api.ScionConfig{Env: map[string]string{"A": "2"}}, false), "a removal is refused")
	assert.Contains(t, rec.Body.String(), "SCION_HUB_ENDPOINT")
}

// The comparison basis is the row version the patch's write is a
// compare-and-set on: a concurrent change of a stored reserved value after
// the request read the agent makes the write fail, so a patch validated as
// a byte-identical echo of the earlier value cannot land.
func TestValidateAgentConfigPatch_ReservedEnvConcurrentChange(t *testing.T) {
	ctx := context.Background()
	srv, s := testServer(t)
	require.NoError(t, s.CreateProject(ctx, &store.Project{ID: tid("project-patch-race"), Name: "race", Slug: "race"}))
	row := &store.Agent{ID: tid("agent-patch-race"), Name: "race", Slug: "race", ProjectID: tid("project-patch-race"),
		AppliedConfig: &store.AgentAppliedConfig{Env: map[string]string{"SCION_HUB_ENDPOINT": "https://hub"}}}
	require.NoError(t, s.CreateAgent(ctx, row))

	read, err := s.GetAgent(ctx, row.ID) // the PATCH request's read
	require.NoError(t, err)
	other, err := s.GetAgent(ctx, row.ID) // a concurrent writer
	require.NoError(t, err)
	other.AppliedConfig.Env = map[string]string{"SCION_HUB_ENDPOINT": "https://other"}
	require.NoError(t, s.UpdateAgent(ctx, other))

	cfg := &api.ScionConfig{Env: map[string]string{"SCION_HUB_ENDPOINT": "https://hub"}}
	rec := httptest.NewRecorder()
	require.True(t, srv.validateAgentConfigPatch(ctx, rec, read, cfg, false), "an echo of what this request read")
	read.AppliedConfig.Env = cfg.Env
	assert.ErrorIs(t, s.UpdateAgent(ctx, read), store.ErrVersionConflict, "the write is refused")
	got, err := s.GetAgent(ctx, row.ID)
	require.NoError(t, err)
	assert.Equal(t, "https://other", got.AppliedConfig.Env["SCION_HUB_ENDPOINT"])
}

// A marker stored in an agent's env (written before the patch strip
// existed, or by any other writer) never reaches the broker on a start
// the hub decided non-native; a native start carries the hub's "1".
func TestDispatch_CloudRunStoredEnvMarkerIgnored(t *testing.T) {
	ctx := context.Background()
	now := time.Now()
	for _, op := range precheckOps(ctx) {
		if op.name != "start" && op.name != "restart" {
			continue
		}
		t.Run(op.name+" non-native", func(t *testing.T) {
			d, m, a := crDispatcher(t, []store.BrokerProfile{crProfile(now)}, "cr", crAssign(crUnlistedGSA), true,
				func(b *store.RuntimeBroker) {
					b.Profiles[0].CloudRunNativeIdentity = false
					b.Profiles[0].Type = "docker"
				})
			a.AppliedConfig.Env = map[string]string{envNativeCloudRun: "1"}
			_ = op.run(d, a)
			require.True(t, op.called(m))
			_, env, envSet := crMarkerSent(t, op.name, m)
			assert.False(t, envSet, "got %q", env)
		})
		t.Run(op.name+" native", func(t *testing.T) {
			d, m, a := crDispatcher(t, []store.BrokerProfile{crProfile(now)}, "cr", crAssign(crListedGSA), true)
			a.AppliedConfig.Env = map[string]string{envNativeCloudRun: "yes"}
			crPersist(t, d, a)
			_ = op.run(d, a)
			require.True(t, op.called(m))
			_, env, envSet := crMarkerSent(t, op.name, m)
			assert.True(t, envSet)
			assert.Equal(t, "1", env, "the hub's own value")
		})
	}
}

// A native decision for an agent whose row cannot be written (no row, a
// soft-deleted one, or an unparsable ID) refuses the dispatch as not sent,
// with no marker and no broker call.
func TestDispatch_CloudRunNativeWithoutRowRefuses(t *testing.T) {
	ctx := context.Background()
	for _, op := range precheckOps(ctx) {
		t.Run(op.name, func(t *testing.T) {
			d, m, a := crDispatcher(t, []store.BrokerProfile{crProfile(time.Now())}, "cr", crAssign(crListedGSA), true)
			err := op.run(d, a)
			require.ErrorIs(t, err, store.ErrNotFound)
			assert.ErrorIs(t, err, errStartRequestNotSent)
			assert.True(t, isConfirmedStartNotActedOnError(err))
			assert.False(t, op.called(m), "no broker call, so no marker sent")
		})
	}
	t.Run("soft-deleted row", func(t *testing.T) {
		d, _, a := crDispatcher(t, []store.BrokerProfile{crProfile(time.Now())}, "cr", crAssign(crListedGSA), true)
		crPersist(t, d, a)
		row, err := d.store.GetAgent(ctx, a.ID)
		require.NoError(t, err)
		row.DeletedAt = time.Now()
		require.NoError(t, d.store.UpdateAgent(ctx, row))
		got, err := d.markCloudRunNative(ctx, a)
		require.ErrorIs(t, err, store.ErrNotFound)
		assert.ErrorIs(t, err, errStartRequestNotSent)
		assert.False(t, cloudRunNativeFrom(got), "no marker")
	})
	t.Run("no store refuses with the experiment on", func(t *testing.T) {
		d, _, a := crDispatcher(t, []store.BrokerProfile{crProfile(time.Now())}, "cr", crAssign(crListedGSA), true)
		d.store = nil
		got, err := d.markCloudRunNative(ctx, a)
		require.ErrorIs(t, err, errStartRequestNotSent)
		assert.False(t, cloudRunNativeFrom(got), "no marker")

		d.SetDispatchExperimentsProvider(func() []string { return nil })
		got, err = d.markCloudRunNative(ctx, a)
		require.NoError(t, err, "experiment off: no store is needed")
		assert.False(t, cloudRunNativeFrom(got))
	})
	t.Run("invalid agent ID", func(t *testing.T) {
		d, m, a := crDispatcher(t, []store.BrokerProfile{crProfile(time.Now())}, "cr", crAssign(crListedGSA), true)
		a.ID = "not-a-uuid"
		got, err := d.markCloudRunNative(ctx, a)
		require.ErrorIs(t, err, store.ErrInvalidInput)
		assert.ErrorIs(t, err, errStartRequestNotSent)
		assert.False(t, cloudRunNativeFrom(got), "no marker")
		assert.False(t, m.createCalled)
	})
}

// A restart dispatched with a copy that says recorded, against a row whose
// record is gone, writes the record again before the broker call.
func TestDispatch_CloudRunNativeRestartReRecordsStaleCopy(t *testing.T) {
	ctx := context.Background()
	d, m, a := crDispatcher(t, []store.BrokerProfile{crProfile(time.Now())}, "cr", crAssign(crListedGSA), true)
	crPersist(t, d, a)
	require.NoError(t, d.store.SetAgentNativeCloudRun(ctx, a.ID, false))
	a.AppliedConfig.NativeCloudRun = true
	_ = d.DispatchAgentRestart(ctx, a)
	require.True(t, m.restartCalled)
	assert.True(t, crRecorded(t, d.store, a.ID), "the row is recorded again")
	assert.Equal(t, "1", m.lastRestartResolvedEnv[envNativeCloudRun])
}

// A native agent's reincarnation completion write, which replaces the
// applied config with a fresh one that has no record, keeps the record
// (the store invariant), and the GCP token path stays closed with the
// experiment off.
func TestReincarnationCompletionKeepsCloudRunNativeRecord(t *testing.T) {
	ctx := context.Background()
	f := newGCPMintFixture(t, "crreinc")
	f.attachCRBroker(t, store.BrokerProfile{Name: "local", Type: "docker"})
	setCloudRunNativeIdentity(t, f.Server, false)
	// The new generation's start records the decision.
	require.NoError(t, f.Store.SetAgentNativeCloudRun(ctx, f.AgentID, true))

	cur := f.getAgent(t)
	fresh := &store.AgentAppliedConfig{
		Profile:     cur.AppliedConfig.Profile,
		GCPIdentity: cur.AppliedConfig.GCPIdentity,
		AgentRole:   cur.AppliedConfig.AgentRole,
	}
	require.False(t, fresh.NativeCloudRun)
	gen := cur.Generation + 1
	_, err := writeReincarnationStep(ctx, f.Store, f.AgentID, reincarnationStepUpdate{appliedConfig: fresh, generation: &gen})
	require.NoError(t, err)
	assert.True(t, crRecorded(t, f.Store, f.AgentID), "the completion write keeps the record")
	assert.False(t, grantHasGCPScope(t, f))
	assertMintDenied(t, f)
}

// The native marker cases of a config patch: adding it, changing a stored
// marker's value and a byte-identical echo of it are refused; removing a
// stored marker passes. Removing another stored reserved key stays
// refused. Every refusal is 400 and names the key, never the value.
func TestValidateAgentConfigPatch_NativeMarkerEdits(t *testing.T) {
	ctx := context.Background()
	srv, _ := testServer(t)
	withMarker := map[string]string{"A": "1", envNativeCloudRun: "marker-value-stored"}
	cases := []struct {
		name    string
		stored  map[string]string
		env     map[string]string
		refused string
	}{
		{name: "added to an env without one is refused", stored: map[string]string{"A": "1"},
			env: map[string]string{"A": "1", envNativeCloudRun: "marker-value-added"}, refused: envNativeCloudRun},
		{name: "stored value changed is refused", stored: withMarker,
			env: map[string]string{"A": "1", envNativeCloudRun: "marker-value-changed"}, refused: envNativeCloudRun},
		{name: "byte-identical echo is refused", stored: withMarker,
			env: map[string]string{"A": "1", envNativeCloudRun: "marker-value-stored"}, refused: envNativeCloudRun},
		{name: "removing a stored marker passes", stored: withMarker,
			env: map[string]string{"A": "2"}},
		{name: "removing another stored reserved key is refused", stored: map[string]string{"A": "1", "SCION_HUB_ENDPOINT": "marker-value-hub"},
			env: map[string]string{"A": "1"}, refused: "SCION_HUB_ENDPOINT"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			agent := &store.Agent{ID: tid("agent-marker-edit"), ProjectID: tid("project-marker-edit"),
				AppliedConfig: &store.AgentAppliedConfig{Env: tc.stored}}
			rec := httptest.NewRecorder()
			ok := srv.validateAgentConfigPatch(ctx, rec, agent, &api.ScionConfig{Env: tc.env}, false)
			if tc.refused == "" {
				assert.True(t, ok, rec.Body.String())
				return
			}
			assert.False(t, ok)
			assert.Equal(t, http.StatusBadRequest, rec.Code)
			assert.Contains(t, rec.Body.String(), tc.refused, "the key is named")
			assert.NotContains(t, rec.Body.String(), "marker-value", "the value is never named")
		})
	}
}

// crSentEnv returns the resolved env op sent the broker.
func crSentEnv(t *testing.T, opName string, m *mockRuntimeBrokerClient) map[string]string {
	t.Helper()
	switch opName {
	case "start":
		return m.lastResolvedEnv
	case "restart":
		return m.lastRestartResolvedEnv
	default:
		require.NotNil(t, m.lastCreateReq)
		return m.lastCreateReq.ResolvedEnv
	}
}

// On every dispatch path, no stored SCION_METADATA_* value reaches the
// broker: the hub deletes them all after every merge and writes only its
// own decision (the mode source is always the hub's own "hub"; the SA
// email and project only on start and restart of an assign agent).
func TestDispatch_StoredMetadataEnvRemoved(t *testing.T) {
	ctx := context.Background()
	stored := map[string]string{
		"A":                                    "1",
		"SCION_METADATA_SA_EMAIL":              "stored@example-project.iam.gserviceaccount.com",
		"SCION_METADATA_PROJECT_ID":            "stored-project",
		"SCION_METADATA_MODE":                  store.GCPMetadataModeAssign,
		"SCION_METADATA_MODE_SOURCE":           "hub",
		"SCION_METADATA_REQUIRE_LOCAL_RUNTIME": "true",
	}
	local := []store.BrokerProfile{{Name: "local", Type: "docker"}}
	for _, op := range precheckOps(ctx) {
		t.Run(op.name+" no hub identity", func(t *testing.T) {
			d, m, a := crDispatcher(t, local, "local", nil, false)
			a.AppliedConfig.Env = maps.Clone(stored)
			_ = op.run(d, a)
			require.True(t, op.called(m))
			env := crSentEnv(t, op.name, m)
			assert.Equal(t, "1", env["A"])
			for _, k := range []string{"SCION_METADATA_SA_EMAIL", "SCION_METADATA_PROJECT_ID", "SCION_METADATA_MODE", "SCION_METADATA_REQUIRE_LOCAL_RUNTIME"} {
				_, has := env[k]
				assert.False(t, has, "%s from the stored env is not sent", k)
			}
			assert.Equal(t, "hub", env["SCION_METADATA_MODE_SOURCE"], "the hub's own source marker")
		})
		t.Run(op.name+" hub assign", func(t *testing.T) {
			hubID := &store.GCPIdentityConfig{MetadataMode: store.GCPMetadataModeAssign,
				ServiceAccountEmail: "hub@example-project.iam.gserviceaccount.com", ProjectID: "hub-project"}
			d, m, a := crDispatcher(t, local, "local", hubID, false)
			a.AppliedConfig.Env = maps.Clone(stored)
			_ = op.run(d, a)
			require.True(t, op.called(m))
			env := crSentEnv(t, op.name, m)
			assert.Equal(t, store.GCPMetadataModeAssign, env["SCION_METADATA_MODE"])
			assert.Equal(t, "hub", env["SCION_METADATA_MODE_SOURCE"])
			_, has := env["SCION_METADATA_REQUIRE_LOCAL_RUNTIME"]
			assert.False(t, has)
			if op.name == "start" || op.name == "restart" {
				assert.Equal(t, "hub@example-project.iam.gserviceaccount.com", env["SCION_METADATA_SA_EMAIL"], "the hub's value, not the stored one")
				assert.Equal(t, "hub-project", env["SCION_METADATA_PROJECT_ID"])
			} else {
				_, has := env["SCION_METADATA_SA_EMAIL"]
				assert.False(t, has, "create carries the account in its config, not the env")
			}
		})
	}
}

// A held edit that names env with a null value clears the env, so it is
// checked as an empty env: it removes a stored reserved key (refused) but
// may drop a stored marker; on the live path a nil env changes nothing.
func TestValidateAgentConfigPatch_HeldNullEnv(t *testing.T) {
	ctx := context.Background()
	srv, _ := testServer(t)
	reserved := &store.Agent{ID: tid("agent-held-null"), ProjectID: tid("project-held-null"),
		AppliedConfig: &store.AgentAppliedConfig{Env: map[string]string{"A": "1", "SCION_HUB_ENDPOINT": "https://hub"}}}
	markerOnly := &store.Agent{ID: tid("agent-held-null2"), ProjectID: tid("project-held-null"),
		AppliedConfig: &store.AgentAppliedConfig{Env: map[string]string{"A": "1", envNativeCloudRun: "1"}}}

	rec := httptest.NewRecorder()
	assert.False(t, srv.validateAgentConfigPatch(ctx, rec, reserved, &api.ScionConfig{}, true), "held null env removes a reserved key")
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Contains(t, rec.Body.String(), "SCION_HUB_ENDPOINT")
	assert.NotContains(t, rec.Body.String(), "https://")

	rec = httptest.NewRecorder()
	assert.True(t, srv.validateAgentConfigPatch(ctx, rec, markerOnly, &api.ScionConfig{}, true), rec.Body.String())
	rec = httptest.NewRecorder()
	assert.True(t, srv.validateAgentConfigPatch(ctx, rec, reserved, &api.ScionConfig{}, false), "live nil env changes nothing")
}

// A held edit with a null env removes the whole stored env: an allowed
// auto-expose key may go with it, a SCION_METADATA_ key may not.
func TestValidateAgentConfigPatch_HeldNullEnvAutoExposeKeys(t *testing.T) {
	ctx := context.Background()
	srv, _ := testServer(t)
	autoExpose := &store.Agent{ID: tid("agent-held-null-ae"), ProjectID: tid("project-held-null"),
		AppliedConfig: &store.AgentAppliedConfig{Env: map[string]string{"A": "1", "SCION_AUTO_EXPOSE_PORTS": "true"}}}
	withMetadata := &store.Agent{ID: tid("agent-held-null-ae2"), ProjectID: tid("project-held-null"),
		AppliedConfig: &store.AgentAppliedConfig{Env: map[string]string{"A": "1", "SCION_AUTO_EXPOSE_PORTS": "true", "SCION_METADATA_MODE": "block"}}}

	rec := httptest.NewRecorder()
	assert.True(t, srv.validateAgentConfigPatch(ctx, rec, autoExpose, &api.ScionConfig{}, true), rec.Body.String())

	rec = httptest.NewRecorder()
	assert.False(t, srv.validateAgentConfigPatch(ctx, rec, withMetadata, &api.ScionConfig{}, true), "held null env removes a metadata key")
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Contains(t, rec.Body.String(), "SCION_METADATA_MODE")
}

// config.env may add, change or remove the user-facing auto-expose keys
// (patchableReservedEnvKeys) with any value, as upstream stores them; every
// other reserved key, a near-miss name and a case variant that keeps the
// exact "SCION_" prefix stay refused. A fully lower-case name is not
// reserved (the prefix check is case-sensitive) and stays a plain user key.
func TestValidateAgentConfigPatch_AutoExposeKeysPatchable(t *testing.T) {
	stored := map[string]string{
		"A":                            "1",
		"SCION_AUTO_EXPOSE_PORTS":      "true",
		"SCION_AUTO_EXPOSE_MODE":       "allowlist",
		"SCION_AUTO_EXPOSE_PORTS_LIST": "80",
		"SCION_AUTO_EXPOSE_INTERVAL":   "3s",
	}
	with := func(edit func(m map[string]string)) map[string]string {
		m := maps.Clone(stored)
		edit(m)
		return m
	}
	for key := range patchableReservedEnvKeys {
		without := with(func(m map[string]string) { delete(m, key) })
		assert.Empty(t, reservedEnvEditKey(without, stored), "adding %s", key)
		assert.Empty(t, reservedEnvEditKey(stored, with(func(m map[string]string) { m[key] = "changed" })), "changing %s", key)
		assert.Empty(t, reservedEnvEditKey(stored, without), "removing %s", key)
	}
	for _, tc := range []struct{ key, value string }{
		{"SCION_AUTO_EXPOSE_PORTS", "true"},
		{"SCION_AUTO_EXPOSE_PORTS", "yes"},
		{"SCION_AUTO_EXPOSE_PORTS_LIST", ""},
		{"SCION_AUTO_EXPOSE_INTERVAL", "9s"},
		{"SCION_AUTO_EXPOSE_MODE", "denylist"},
	} {
		assert.Empty(t, reservedEnvEditKey(stored, with(func(m map[string]string) { m[tc.key] = tc.value })), "%s=%q", tc.key, tc.value)
	}

	// The same auto-expose edit with a metadata key added is refused,
	// naming the metadata key.
	mixed := with(func(m map[string]string) {
		m["SCION_AUTO_EXPOSE_PORTS"] = "false"
		m["SCION_METADATA_MODE"] = "passthrough"
	})
	assert.Equal(t, "SCION_METADATA_MODE", reservedEnvEditKey(stored, mixed))

	for _, key := range []string{
		"SCION_METADATA_MODE",
		"SCION_METADATA_SA_EMAIL",
		envNativeCloudRun,
		"SCION_HUB_ENDPOINT",
		"GCE_METADATA_HOST",
		"SCION_AUTO_EXPOSE_MIN_PORT",
		"SCION_AUTO_EXPOSE_PORTSX",
		"SCION_AUTO_EXPOSE_PORTS_",
		"SCION_AUTO_EXPOSE_",
		"SCION_AUTO_EXPOSE_LIST",
		"SCION_AUTO_EXPOSE_ports",
		"SCION_Auto_Expose_Ports",
	} {
		assert.Equal(t, key, reservedEnvEditKey(stored, with(func(m map[string]string) { m[key] = "v" })), "adding %s", key)
	}
	assert.Empty(t, reservedEnvEditKey(stored, with(func(m map[string]string) { m["scion_auto_expose_ports"] = "true" })),
		"a fully lower-case name is a plain user key")
}

// Every key the hub's dispatch seal writes (sealMetadataEnv, and
// applyHubGCPMetadataModeEnv within and on its own) is patch-refused as an
// add, as are the native Cloud Run marker and a SCION_METADATA_ name no
// code writes yet: the patch check is default deny for reserved keys.
func TestValidateAgentConfigPatch_SealedKeysRefused(t *testing.T) {
	ctx := context.Background()
	start := &store.GCPIdentityConfig{MetadataMode: store.GCPMetadataModeAssign, ServiceAccountEmail: crListedGSA, ProjectID: "example-project"}
	local := &store.GCPIdentityConfig{MetadataMode: store.GCPMetadataModePassthrough, RequireLocalRuntime: true}
	written := map[string]bool{}
	collect := func(env map[string]string) {
		for k := range env {
			written[k] = true
		}
	}
	for _, shape := range []struct {
		ctx   context.Context
		mode  string
		start *store.GCPIdentityConfig
	}{
		{ctx, store.GCPMetadataModeAssign, start},
		{ctx, store.GCPMetadataModePassthrough, local},
		{ctx, store.GCPMetadataModeBlock, nil},
		{ctx, "", nil},
		{withCloudRunNative(ctx), store.GCPMetadataModeAssign, start},
		{withCloudRunNative(ctx), store.GCPMetadataModeAssign, nil},
	} {
		env := map[string]string{}
		cls := map[string]api.EnvKind{}
		sealMetadataEnv(shape.ctx, env, &cls, shape.mode, shape.start)
		collect(env)
	}
	for _, mode := range []string{"", store.GCPMetadataModeAssign, store.GCPMetadataModeBlock, store.GCPMetadataModePassthrough} {
		env := map[string]string{}
		cls := map[string]api.EnvKind{}
		applyHubGCPMetadataModeEnv(env, &cls, mode)
		collect(env)
	}
	for _, k := range []string{
		"SCION_METADATA_MODE", "SCION_METADATA_MODE_SOURCE", "SCION_METADATA_SA_EMAIL",
		"SCION_METADATA_PROJECT_ID", "SCION_METADATA_REQUIRE_LOCAL_RUNTIME", envNativeCloudRun,
	} {
		require.True(t, written[k], "the seal is expected to write %s", k)
	}

	base := map[string]string{"A": "1"}
	keys := maps.Clone(written)
	for _, k := range []string{envNativeCloudRun, "SCION_METADATA_MODE_SOURCE", "SCION_METADATA_FUTURE_KEY"} {
		keys[k] = true
	}
	for k := range keys {
		requested := maps.Clone(base)
		requested[k] = "v"
		assert.Equal(t, k, reservedEnvEditKey(base, requested), "%s must be patch-refused", k)
	}
}

// The patchable reserved keys are exactly the auto-expose keys the
// configure page owns (autoExposeEnvKeys). A new key in either set fails
// this test until it is added to the other by name.
func TestPatchableReservedEnvKeysMatchAutoExposeForm(t *testing.T) {
	assert.Equal(t, autoExposeEnvKeys, patchableReservedEnvKeys)
}

// A configure PATCH may toggle and change the auto-expose keys, and a row
// edit may omit a stamped SCION_AUTO_EXPOSE_PORTS; a PATCH that also adds a
// SCION_METADATA_ key is refused with 400 and changes nothing.
func TestAgentConfigPatch_AutoExposeContract(t *testing.T) {
	const ports = "SCION_AUTO_EXPOSE_PORTS"
	setup := func(t *testing.T) (*Server, store.Store, *store.Agent) {
		srv, s, _, agent := setupAutoExposePatchAgent(t, autoExposePatchAgent{
			appliedEnv:   map[string]string{"TEMPLATE_KEY": "x", ports: "true"},
			createInputs: ciWithEnv(nil),
		})
		return srv, s, agent
	}
	t.Run("toggle and change the auto-expose keys", func(t *testing.T) {
		srv, s, agent := setup(t)
		updated := patchAndReload(t, srv, s, agent.ID, rowEditBody(t, map[string]interface{}{
			"TEMPLATE_KEY":                 "x",
			ports:                          "false",
			"SCION_AUTO_EXPOSE_MODE":       "denylist",
			"SCION_AUTO_EXPOSE_PORTS_LIST": "22",
			"SCION_AUTO_EXPOSE_INTERVAL":   "9s",
		}))
		assert.Equal(t, "false", updated.AppliedConfig.Env[ports])
		assert.Equal(t, "denylist", updated.AppliedConfig.Env["SCION_AUTO_EXPOSE_MODE"])
		assert.Equal(t, "22", updated.AppliedConfig.Env["SCION_AUTO_EXPOSE_PORTS_LIST"])
		assert.Equal(t, "9s", updated.AppliedConfig.Env["SCION_AUTO_EXPOSE_INTERVAL"])
	})
	t.Run("a row edit omitting the stamped key", func(t *testing.T) {
		srv, s, agent := setup(t)
		updated := patchAndReload(t, srv, s, agent.ID, configureRowEditBody(t))
		assert.Equal(t, "true", updated.AppliedConfig.Env[ports])
	})
	t.Run("an added metadata key is refused", func(t *testing.T) {
		srv, s, agent := setup(t)
		before, err := s.GetAgent(context.Background(), agent.ID)
		require.NoError(t, err)
		rec := patchAgentConfig(t, srv, agent.ID, rowEditBody(t, map[string]interface{}{
			"TEMPLATE_KEY":        "x",
			ports:                 "false",
			"SCION_METADATA_MODE": "passthrough",
		}))
		require.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
		var resp struct {
			Error struct {
				Details map[string]interface{} `json:"details"`
			} `json:"error"`
		}
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp), rec.Body.String())
		assert.Equal(t, "env", resp.Error.Details["field"])
		assert.Equal(t, "SCION_METADATA_MODE", resp.Error.Details["value"])
		after, err := s.GetAgent(context.Background(), agent.ID)
		require.NoError(t, err)
		assert.Equal(t, before.AppliedConfig, after.AppliedConfig, "the whole applied config is unchanged")
		assert.Equal(t, before.StateVersion, after.StateVersion, "nothing was written")
	})
}
