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

// Package hub — start-time delivery of the owner's shared user secrets
// follows the runtime read's chain verdict: an agent whose delegation chain
// is not recorded is withheld them at create, start, resume, restart and
// as_needed (chain_unrecorded), as the runtime read refuses them.
package hub

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/secret"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// sharedUnrecordedShapes builds owner-path agents (ancestry and owner the
// fixture user) whose chain is not recorded. depth0 marks a shape whose own
// edge is the unrecorded hop, so the runtime verdict is chain_unrecorded.
func sharedUnrecordedShapes() []struct {
	name   string
	depth0 bool
	build  func(t *testing.T, f *sdFixture) (string, string)
} {
	return []struct {
		name   string
		depth0 bool
		build  func(t *testing.T, f *sdFixture) (string, string)
	}{
		{"no own edge", true, func(t *testing.T, f *sdFixture) (string, string) {
			return f.direct(t, "sh-noedge", false)
		}},
		{"edge without recorded provenance", true, func(t *testing.T, f *sdFixture) (string, string) {
			id, tok := f.direct(t, "sh-unrec", false)
			createDCEdge(t, f.Store, store.DelegationPrincipalUser, f.UserID, store.DelegationPrincipalAgent, id,
				store.RoleScopeProject, f.ProjectID, string(AgentRoleFull))
			return id, tok
		}},
		{"migration sentinel", true, func(t *testing.T, f *sdFixture) (string, string) {
			id, tok := f.direct(t, "sh-sentinel", false)
			f.recordEdge(t, store.DelegationPrincipalUser, migrationDelegatorID, id, store.EffectCeiling{Kind: store.EffectCeilingUnrecorded}, func(e *store.DelegationEdge) {
				e.ProvenanceVersion = 0
			})
			return id, tok
		}},
		{"recorded principal does not type-check", true, func(t *testing.T, f *sdFixture) (string, string) {
			id, tok := f.direct(t, "sh-badprincipal", false)
			f.recordEdge(t, store.DelegationPrincipalUser, f.UserID, id, store.EffectCeiling{Kind: store.EffectCeilingPrincipal}, func(e *store.DelegationEdge) {
				e.SourcePrincipalKind = store.DelegationPrincipalAgent
			})
			return id, tok
		}},
		{"unknown provenance version", true, func(t *testing.T, f *sdFixture) (string, string) {
			id, tok := f.direct(t, "sh-unknownver", false)
			f.recordEdge(t, store.DelegationPrincipalUser, f.UserID, id, store.EffectCeiling{Kind: store.EffectCeilingPrincipal}, func(e *store.DelegationEdge) {
				e.ProvenanceVersion = 2
			})
			return id, tok
		}},
		{"recorded own edge, unrecorded hop above", false, func(t *testing.T, f *sdFixture) (string, string) {
			parent, _ := f.agent(t, "sh-above-parent", []string{f.UserID})
			id, tok := f.direct(t, "sh-above", false)
			f.recordEdge(t, store.DelegationPrincipalAgent, parent, id, store.EffectCeiling{Kind: store.EffectCeilingPrincipal}, nil)
			return id, tok
		}},
	}
}

// TestStartDelivery_UnrecordedChainWithholdsSharedRows (LEGACY DATA): for
// every chain without recorded provenance the owner's shared user secret is
// withheld at create (resolveSecrets), as_needed, and the start and restart
// launch selection, and the runtime read refuses it: start equals the read.
// A depth-0 shape is counted as withheld with reason chain_unrecorded and
// logged at Warn without a key or value.
func TestStartDelivery_UnrecordedChainWithholdsSharedRows(t *testing.T) {
	for _, c := range sharedUnrecordedShapes() {
		t.Run(c.name, func(t *testing.T) {
			f := newSDFixture(t, "sh-unrec")
			id, tok := c.build(t, f)
			m := &omcMetrics{}
			f.Server.SetOwnerMaterialChainMetrics(m)
			var buf bytes.Buffer
			f.disp.log = slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo}))

			got := sdStartDelivery(t, f.disp, f.Store, id)
			assert.False(t, got.always, "create and the launch selection withhold the shared row")
			assert.False(t, got.asNeeded, "as_needed withholds the shared row")
			assert.False(t, f.read(t, id, tok, sdAlwaysKey), "the runtime read refuses it")
			assert.False(t, f.read(t, id, tok, sdAsNeededKey), "the runtime read refuses it")
			assert.False(t, got.private, "the owner's unshared row is withheld too")

			if !c.depth0 {
				return
			}
			assert.True(t, omcOutcomes(m)[ownerRowOutcomeWithheld+"/"+ReasonChainUnrecorded], "counted: %v", omcOutcomes(m))
			recs := omcRecords(t, buf.String(), omcWithheldMsg)
			var shared bool
			for _, r := range recs {
				if r["reason"] == ReasonChainUnrecorded {
					shared = true
					assert.Equal(t, id, r["agent_id"])
				}
			}
			assert.True(t, shared, "the shared-row withhold is logged; log: %s", buf.String())
			for _, s := range []string{sdAlwaysKey, sdAlwaysValue, sdAsNeededKey, sdAsNeededValue} {
				assert.NotContains(t, buf.String(), s, "a withheld row's key or value was logged")
			}
		})
	}
}

// TestStartDelivery_UnrecordedChainDispatchParity: a real create, start,
// resume and restart of an agent with no own edge send neither the owner's
// shared secret nor its unshared rows; an agent whose chain is recorded to
// the owner gets both.
func TestStartDelivery_UnrecordedChainDispatchParity(t *testing.T) {
	for _, c := range []struct {
		name string
		edge bool
	}{{"recorded", true}, {"no own edge", false}} {
		t.Run(c.name, func(t *testing.T) {
			f := newOMCFixture(t, "sh-dispatch")
			id, _ := f.direct(t, "sh-dispatch", c.edge)
			create, start, restart, _ := omcDispatch(t, f, id, false)
			for name, d := range map[string]omcDispatched{"create": create, "start": start, "restart": restart} {
				assert.Equal(t, c.edge, d.sharedSecret, "%s: shared secret", name)
				assert.Equal(t, c.edge, d.privateSecret, "%s: unshared secret", name)
			}
			for _, sent := range omcStartResumeRestart(t, f, id) {
				_, has := sent.env[sdAlwaysKey]
				assert.Equal(t, c.edge, has, "%s: shared secret", sent.name)
			}
		})
	}
}

// TestStartDelivery_RecordedChainsKeepSharedRows: chains recorded to the
// owner keep the owner's shared secret at start: a session edge, a token
// edge (the intrinsic set) and a descendant whose edge was recorded through
// parentDeliverEligibility on the progeny path.
func TestStartDelivery_RecordedChainsKeepSharedRows(t *testing.T) {
	t.Run("session", func(t *testing.T) {
		f := newSDFixture(t, "sh-rec-session")
		id, _ := f.direct(t, "sh-rec-session", true)
		got := sdStartDelivery(t, f.disp, f.Store, id)
		assert.True(t, got.always && got.asNeeded, "%+v", got)
	})
	t.Run("token", func(t *testing.T) {
		f := newSDFixture(t, "sh-rec-token")
		id, _ := f.uiDirect(t, "sh-rec-token", nil)
		got := sdStartDelivery(t, f.disp, f.Store, id)
		assert.True(t, got.always && got.asNeeded, "%+v", got)
	})
	t.Run("recorded descendant", func(t *testing.T) {
		f := newSDFixture(t, "sh-rec-child")
		child, _, _ := f.progeny(t, "sh-rec-child", true)
		got := sdStartDelivery(t, f.disp, f.Store, child)
		assert.True(t, got.always, "the progeny path delivers the shared row: %+v", got)
	})
}

// TestStartDelivery_SharedVerdictOncePerEntryPoint: one create, start,
// resume and restart through the real dispatch entry points each compute the runtime
// chain verdict for the agent once, whatever the number of shared rows and
// sites (the per-dispatch memo).
func TestStartDelivery_SharedVerdictOncePerEntryPoint(t *testing.T) {
	f := newOMCFixture(t, "sh-memo")
	id, _ := f.direct(t, "sh-memo", true)

	var mu sync.Mutex
	counts := 0
	observe := func(agentID string) {
		if agentID == id {
			mu.Lock()
			counts++
			mu.Unlock()
		}
	}
	userSecretChainVerdictObserver.Store(&observe)
	t.Cleanup(func() { userSecretChainVerdictObserver.Store(nil) })
	take := func() int {
		mu.Lock()
		defer mu.Unlock()
		n := counts
		counts = 0
		return n
	}

	ctx := context.Background()
	d, agent, _ := omcDispatcher(t, f, id, false)
	take()
	_, err := d.DispatchAgentCreate(ctx, agent)
	require.NoError(t, err)
	assert.Equal(t, 1, take(), "create computes the verdict once")
	require.NoError(t, d.DispatchAgentStart(ctx, agent, "", false))
	assert.Equal(t, 1, take(), "start computes the verdict once")
	require.NoError(t, d.DispatchAgentStart(ctx, agent, "", true))
	assert.Equal(t, 1, take(), "resume computes the verdict once")
	require.NoError(t, d.DispatchAgentRestart(ctx, agent))
	assert.Equal(t, 1, take(), "restart computes the verdict once")
}

// TestStartDelivery_ReportsFollowSharedSecretDecision (review M2): the
// required-key probe (hasAnyKey) and the env-gather response report the
// owner's shared secret as present exactly where start delivers it, for an
// agent with a stored ancestry and one with none, and for a progeny agent
// that sees it through its ancestry. Where start withholds it, the probe
// reports the key missing, env-gather does not label it "secret", and its
// cross-check gives no user-scope warning. The views count nothing and log
// nothing: they run on the Server, which logs through slog.Default, so the
// test captures that logger for the view calls (usaCaptureDefaultLog; this
// test does not run in parallel).
func TestStartDelivery_ReportsFollowSharedSecretDecision(t *testing.T) {
	for _, c := range []struct {
		name      string
		want      bool // start delivers the shared secret
		envGather bool // the agent is on the owner path (env-gather lists the owner's secrets)
		build     func(t *testing.T, f *sdFixture) string
	}{
		{"stored ancestry, recorded", true, true, func(t *testing.T, f *sdFixture) string {
			id, _ := f.direct(t, "rep-anc-rec", true)
			return id
		}},
		{"stored ancestry, no own edge", false, true, func(t *testing.T, f *sdFixture) string {
			id, _ := f.direct(t, "rep-anc-noedge", false)
			return id
		}},
		{"no ancestry, recorded", true, true, func(t *testing.T, f *sdFixture) string {
			id := omcAgentWith(t, f, "rep-noanc-rec", nil, f.UserID)
			f.recordEdge(t, store.DelegationPrincipalUser, f.UserID, id, store.EffectCeiling{Kind: store.EffectCeilingPrincipal}, nil)
			return id
		}},
		{"no ancestry, no own edge", false, true, func(t *testing.T, f *sdFixture) string {
			return omcAgentWith(t, f, "rep-noanc-noedge", nil, f.UserID)
		}},
		{"progeny, recorded", true, false, func(t *testing.T, f *sdFixture) string {
			child, _, _ := f.progeny(t, "rep-prog-rec", true)
			return child
		}},
		{"progeny, no own edge", false, false, func(t *testing.T, f *sdFixture) string {
			child, _, _ := f.progeny(t, "rep-prog-noedge", false)
			return child
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := newSDFixture(t, "rep-shared")
			id := c.build(t, f)
			ctx := context.Background()
			agent, err := f.Store.GetAgent(ctx, id)
			require.NoError(t, err)
			m := &omcMetrics{}
			f.Server.SetOwnerMaterialChainMetrics(m)
			defaultLog := usaCaptureDefaultLog(t)

			found, err := f.Server.hasAnyKey(ctx, agent, []string{sdAlwaysKey})
			require.NoError(t, err)
			assert.Equal(t, c.want, found, "the probe reports the shared secret as start decides it")

			if c.envGather {
				resp := f.Server.buildEnvGatherResponse(ctx, agent, &RemoteEnvRequirementsResponse{
					HubHas: []string{sdAlwaysKey}, Needs: []string{sdAlwaysKey},
				})
				require.Len(t, resp.HubHas, 1)
				assert.Equal(t, c.want, resp.HubHas[0].Scope == "secret", "the env-gather label: %q", resp.HubHas[0].Scope)
				userWarning := false
				for _, w := range resp.HubWarnings {
					if strings.Contains(w, "(user scope)") {
						userWarning = true
					}
				}
				assert.Equal(t, c.want, userWarning, "the env-gather user-scope warning: %v", resp.HubWarnings)
			}
			assert.Empty(t, omcOutcomes(m), "the views count nothing")
			viewLog := defaultLog.String()
			assert.NotContains(t, viewLog, omcWithheldMsg, "the views log no withhold")
			assert.NotContains(t, viewLog, "level=WARN", "the views log no warning: %s", viewLog)
			omcAssertCaptureSeesWarn(t, ctx, defaultLog)

			got := sdStartDelivery(t, f.disp, f.Store, id)
			assert.Equal(t, c.want, got.always, "start delivers the shared secret as the views report it")
		})
	}
}

// TestStartDelivery_UnrecordedChainWithholdsSharedCloneToken: the NoAuth
// clone-token lookup reads the owner's GITHUB_TOKEN through the same start
// check, so an agent whose chain is not recorded does not get the owner's
// shared token at create either; a recorded chain does. The same holds for
// an agent with no stored ancestry (review H1): its shared token follows the
// owner material chain verdict.
func TestStartDelivery_UnrecordedChainWithholdsSharedCloneToken(t *testing.T) {
	const token = "sh-owner-shared-clone-token"
	for _, c := range []struct {
		name       string
		edge       bool
		noAncestry bool
	}{
		{"recorded", true, false}, {"no own edge", false, false},
		{"no ancestry, recorded", true, true}, {"no ancestry, no own edge", false, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := newOMCFixture(t, "sh-clone")
			_, _, err := f.Server.secretBackend.Set(context.Background(), &secret.SetSecretInput{
				Name: "GITHUB_TOKEN", Value: token, SecretType: store.SecretTypeEnvironment, Target: "GITHUB_TOKEN",
				Scope: store.ScopeUser, ScopeID: f.UserID, InjectionMode: store.InjectionModeAlways,
				AllowProgeny: true, CreatedBy: f.UserID, UpdatedBy: f.UserID,
			})
			require.NoError(t, err)
			var id string
			if c.noAncestry {
				id = omcAgentWith(t, f, "sh-clone-noanc", nil, f.UserID)
				if c.edge {
					f.recordEdge(t, store.DelegationPrincipalUser, f.UserID, id, store.EffectCeiling{Kind: store.EffectCeilingPrincipal}, nil)
				}
			} else {
				id, _ = f.direct(t, "sh-clone", c.edge)
			}
			_, _, _, client := omcDispatch(t, f, id, true)
			assert.Equal(t, c.edge, client.lastCreateReq.ResolvedEnv["GITHUB_TOKEN"] == token,
				"the shared clone token reaches the agent only on a recorded chain")
		})
	}
}
