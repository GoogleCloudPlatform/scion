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

// Package hub — start-time delivery of user-scope secrets
// (startSecretResolveOpts) against the runtime read (check 8). The start
// sites are driven, resolveSecrets (the create-time dispatch paths),
// resolveAsNeededForKeys and the start and restart launch selection
// (selectDeliverySecrets, which must deliver exactly what resolveSecrets
// does of the owner's user-scope rows), over both user-scope paths:
//   - the progeny path (an agent created by an agent): start delivery
//     equals the runtime read;
//   - the owner path (an agent created by a user, whose OwnerID is that
//     user): for a row shared with progeny, start delivery equals the
//     user-scope secret parent ceiling verdict (the read's step 10); a row
//     not shared with progeny is unchanged.
//
// The two residual cases are a terminal user whose access constraint allows
// project.secret_read but not secret.use (S4b), and a chain whose terminal
// user is not the secret's owner at the head of the ancestry (S8), reached
// through a cross-user reincarnation.
package hub

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/hub/permissions"
	"github.com/GoogleCloudPlatform/scion/pkg/secret"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

const (
	// sdAlwaysKey is shared with progeny and injected at start
	// (resolveSecrets).
	sdAlwaysKey   = "SD_ALWAYS_KEY"
	sdAlwaysValue = "sd-always-user-value"
	// sdAsNeededKey is shared with progeny and as-needed
	// (resolveAsNeededForKeys).
	sdAsNeededKey   = "SD_AS_NEEDED_KEY"
	sdAsNeededValue = "sd-as-needed-user-value"
	// sdPrivateKey is not shared with progeny.
	sdPrivateKey   = "SD_PRIVATE_KEY"
	sdPrivateValue = "sd-private-user-value"
)

// sdShareOwnerSecrets writes the owner's three user-scope secrets, recorded
// as created by the owner (the attribution every writer records).
func sdShareOwnerSecrets(t *testing.T, backend secret.SecretBackend, ownerID string) {
	t.Helper()
	for _, in := range []*secret.SetSecretInput{
		{Name: sdAlwaysKey, Value: sdAlwaysValue, InjectionMode: store.InjectionModeAlways, AllowProgeny: true},
		{Name: sdAsNeededKey, Value: sdAsNeededValue, InjectionMode: store.InjectionModeAsNeeded, AllowProgeny: true},
		{Name: sdPrivateKey, Value: sdPrivateValue, InjectionMode: store.InjectionModeAlways},
	} {
		in.SecretType = store.SecretTypeEnvironment
		in.Target = in.Name
		in.Scope = store.ScopeUser
		in.ScopeID = ownerID
		in.CreatedBy = ownerID
		in.UpdatedBy = ownerID
		_, _, err := backend.Set(context.Background(), in)
		require.NoError(t, err)
	}
}

// sdDispatcher is a production dispatcher over the server's store, secret
// backend and authorization service.
func sdDispatcher(srv *Server, s store.Store) *HTTPAgentDispatcher {
	d := NewHTTPAgentDispatcherWithClient(s, &mockRuntimeBrokerClient{}, false, slog.Default())
	d.SetSecretBackend(srv.secretBackend)
	d.SetAuthzService(srv.authzService)
	return d
}

// sdDelivery is what start time delivers of the owner's user-scope secrets:
// always is sdAlwaysKey from resolveSecrets, asNeeded is sdAsNeededKey from
// resolveAsNeededForKeys, private is sdPrivateKey from resolveSecrets. Each
// counts only the user-scope row's value. sdStartDelivery also checks that
// the start and restart launch selection deliver always and private
// exactly as resolveSecrets does (as-needed rows are never part of it).
type sdDelivery struct {
	always   bool
	asNeeded bool
	private  bool
}

func sdStartDelivery(t *testing.T, d *HTTPAgentDispatcher, s store.Store, agentID string) sdDelivery {
	t.Helper()
	ctx := context.Background()
	agent, err := s.GetAgent(ctx, agentID)
	require.NoError(t, err)
	var out sdDelivery
	resolved, _, err := d.resolveSecrets(ctx, agent)
	require.NoError(t, err)
	for _, r := range resolved {
		if r.Source != store.ScopeUser {
			continue
		}
		switch r.Name {
		case sdAlwaysKey:
			out.always = r.Value == sdAlwaysValue
		case sdPrivateKey:
			out.private = r.Value == sdPrivateValue
		}
	}
	got, err := d.resolveAsNeededForKeys(ctx, agent, []string{sdAsNeededKey}, nil)
	require.NoError(t, err)
	out.asNeeded = got[sdAsNeededKey] == sdAsNeededValue

	for _, purpose := range []MaterialPurpose{PurposeDispatch, PurposeRestart} {
		selected, err := d.selectDeliverySecrets(ctx, newDeliverySelector(d.authzService, agent, purpose), agent)
		require.NoError(t, err)
		var always, private bool
		for _, r := range selected {
			if r.Source != store.ScopeUser {
				continue
			}
			switch r.Name {
			case sdAlwaysKey:
				always = r.Value == sdAlwaysValue
			case sdAsNeededKey:
				assert.Fail(t, "an as-needed row is never part of the launch selection", "purpose %s", purpose)
			case sdPrivateKey:
				private = r.Value == sdPrivateValue
			}
		}
		assert.Equal(t, out.always, always, "launch selection (%s) delivers %s as resolveSecrets does", purpose, sdAlwaysKey)
		assert.Equal(t, out.private, private, "launch selection (%s) delivers %s as resolveSecrets does", purpose, sdPrivateKey)
	}
	return out
}

// sdFixture is the material fixture with the backfill complete, the owner's
// secrets written and a production dispatcher.
type sdFixture struct {
	*userScopeCeilingFixture
	disp *HTTPAgentDispatcher
}

func newSDFixture(t *testing.T, name string) *sdFixture {
	t.Helper()
	f := &userScopeCeilingFixture{materialFixture: newMaterialFixture(t, name)}
	setBackfillCompleted(t, f.Store)
	sdShareOwnerSecrets(t, f.Server.secretBackend, f.UserID)
	return &sdFixture{userScopeCeilingFixture: f, disp: sdDispatcher(f.Server, f.Store)}
}

// agent stores a full-role agent with the given ancestry, created and owned
// by the last ancestry entry (as the create path records it), and returns
// its ID and a recorded token for it.
func (f *sdFixture) agent(t *testing.T, name string, ancestry []string) (string, string) {
	t.Helper()
	id := tid("sd-agent-" + name)
	creator := ancestry[len(ancestry)-1]
	require.NoError(t, f.Store.CreateAgent(context.Background(), &store.Agent{
		ID: id, Slug: "sd-" + id[:8], Name: "sd-" + id[:8], ProjectID: f.ProjectID, Phase: "running",
		CreatedBy: creator, OwnerID: creator, StateVersion: 1,
		AppliedConfig: &store.AgentAppliedConfig{AgentRole: string(AgentRoleFull)}, Ancestry: ancestry,
	}))
	return id, mintRecordedMaterialToken(t, f.Server, f.Store, id, f.ProjectID, []AgentTokenScope{ScopeProjectSecretRead}, ancestry)
}

// direct is an agent created by the fixture user (owner path), with a
// recorded principal edge from the user unless edge is false.
func (f *sdFixture) direct(t *testing.T, name string, edge bool) (string, string) {
	t.Helper()
	id, token := f.agent(t, name, []string{f.UserID})
	if edge {
		f.recordEdge(t, store.DelegationPrincipalUser, f.UserID, id, store.EffectCeiling{Kind: store.EffectCeilingPrincipal}, nil)
	}
	return id, token
}

// progeny is an agent C created by an agent P created by the fixture user
// (progeny path): U -> P with a recorded principal edge, and P -> C with a
// recorded principal edge unless childEdge is false. It returns C and P.
func (f *sdFixture) progeny(t *testing.T, name string, childEdge bool) (child, token, parent string) {
	t.Helper()
	parent, _ = f.agent(t, name+"-p", []string{f.UserID})
	f.recordEdge(t, store.DelegationPrincipalUser, f.UserID, parent, store.EffectCeiling{Kind: store.EffectCeilingPrincipal}, nil)
	child, token = f.agent(t, name+"-c", []string{f.UserID, parent})
	if childEdge {
		f.recordEdge(t, store.DelegationPrincipalAgent, parent, child, store.EffectCeiling{Kind: store.EffectCeilingPrincipal}, nil)
	}
	return child, token, parent
}

// read is the runtime by-key read (check 8) of key for the agent.
func (f *sdFixture) read(t *testing.T, agentID, token, key string) bool {
	t.Helper()
	code, _ := f.readAs(t, agentID, token, key)
	return code == http.StatusOK
}

// ceiling is the user-scope secret parent ceiling verdict for the agent on
// the owner's row of key.
func (f *sdFixture) ceiling(t *testing.T, agentID, key string) ParentCeilingDecision {
	t.Helper()
	ctx := context.Background()
	agent, err := f.Store.GetAgent(ctx, agentID)
	require.NoError(t, err)
	meta, err := f.Server.secretBackend.GetMeta(ctx, key, store.ScopeUser, f.UserID)
	require.NoError(t, err)
	ident := newFullAgentIdentity(agentID, f.ProjectID, agent.Ancestry, []AgentTokenScope{ScopeProjectSecretRead})
	return f.Server.authzService.EvaluateUserSecretParentCeiling(ctx, ident, userSecretResource(meta.ID, meta.ScopeID))
}

// constrainAllBut adds a system access constraint on the principal that
// allows every registered permission except the excluded one.
func constrainAllBut(t *testing.T, s store.Store, principalType, principalID, excluded string) {
	t.Helper()
	perms := make([]string, 0, len(permissions.Registry))
	for _, p := range permissions.Registry {
		if p.ID != excluded {
			perms = append(perms, p.ID)
		}
	}
	_, err := s.CreateAccessConstraint(context.Background(), &store.AccessConstraint{
		Name:                 "sd-" + principalType + "-" + principalID[:8],
		SubjectKind:          store.ConstraintSubjectPrincipal,
		SubjectPrincipalType: pvStrPtr(principalType),
		SubjectPrincipalID:   pvStrPtr(principalID),
		ScopeType:            store.RoleScopeSystem,
		MaximumPermissions:   perms,
		Purpose:              "sd test constraint",
		CreatedBy:            "sd-test",
	})
	require.NoError(t, err)
}

// TestStartDelivery_CleanChainsDeliver: on a clean recorded chain, start
// delivers the owner's shared secrets at both sites and the runtime read
// returns them, on the owner path (a direct agent, Ancestry [U]: the
// ceiling's progeny set holds the owner's own shared rows, so S5-S8 pass)
// and on the progeny path. The owner's unshared secret is delivered on the
// owner path only.
func TestStartDelivery_CleanChainsDeliver(t *testing.T) {
	f := newSDFixture(t, "sd-clean")

	direct, directToken := f.direct(t, "clean-direct", true)
	d := f.ceiling(t, direct, sdAlwaysKey)
	require.True(t, d.Allowed, "owner path: the ceiling allows the owner's shared row for the owner's direct agent; reason %q", d.Reason)
	assert.Equal(t, sdDelivery{always: true, asNeeded: true, private: true}, sdStartDelivery(t, f.disp, f.Store, direct))
	assert.True(t, f.read(t, direct, directToken, sdAlwaysKey))
	assert.True(t, f.read(t, direct, directToken, sdAsNeededKey))

	child, childToken, _ := f.progeny(t, "clean-progeny", true)
	assert.Equal(t, sdDelivery{always: true, asNeeded: true, private: false}, sdStartDelivery(t, f.disp, f.Store, child))
	assert.True(t, f.read(t, child, childToken, sdAlwaysKey))
	assert.True(t, f.read(t, child, childToken, sdAsNeededKey))
}

// TestStartDelivery_TerminalUserSecretUseConstraint (residual case (a),
// S4b): the terminal user holds an access constraint that allows
// project.secret_read but not secret.use. The runtime read refuses the
// shared secret, and start delivery refuses it too, at both sites, on the
// progeny and the owner path. Before the fix start delivered it (the
// progeny path checked project.secret_read only; the owner path checked
// nothing). The unshared row stays delivered on the owner path.
func TestStartDelivery_TerminalUserSecretUseConstraint(t *testing.T) {
	for _, tc := range []struct {
		name        string
		build       func(t *testing.T, f *sdFixture) (string, string)
		wantPrivate bool
	}{
		{"owner path", func(t *testing.T, f *sdFixture) (string, string) {
			return f.direct(t, "s4b-direct", true)
		}, true},
		{"progeny path", func(t *testing.T, f *sdFixture) (string, string) {
			c, tok, _ := f.progeny(t, "s4b-progeny", true)
			return c, tok
		}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newSDFixture(t, "sd-s4b-"+tc.name[:5])
			agentID, token := tc.build(t, f)
			constrainAllBut(t, f.Store, "user", f.UserID, secretUsePermission)

			assert.False(t, f.read(t, agentID, token, sdAlwaysKey), "the runtime read refuses under the secret.use constraint")
			assert.False(t, f.read(t, agentID, token, sdAsNeededKey), "the runtime read refuses under the secret.use constraint")
			d := f.ceiling(t, agentID, sdAlwaysKey)
			require.False(t, d.Allowed)
			assert.Equal(t, DenyCauseCeilingDelegatorLacksPermission, d.DenyCause, "S4b denies; reason %q", d.Reason)

			assert.Equal(t, sdDelivery{always: false, asNeeded: false, private: tc.wantPrivate},
				sdStartDelivery(t, f.disp, f.Store, agentID),
				"start delivery must refuse the shared secret at both sites, as the read does")
		})
	}
}

// TestStartDelivery_OwnerRowDeniedFallsBackToLowerScope: a shared owner row
// that the check refuses is skipped inside the resolver, so a project row
// with the same key is delivered instead of nothing.
func TestStartDelivery_OwnerRowDeniedFallsBackToLowerScope(t *testing.T) {
	f := newSDFixture(t, "sd-fallback")
	direct, _ := f.direct(t, "fallback-direct", true)
	_, _, err := f.Server.secretBackend.Set(context.Background(), &secret.SetSecretInput{
		Name: sdAlwaysKey, Value: "sd-project-value", SecretType: store.SecretTypeEnvironment, Target: sdAlwaysKey,
		Scope: store.ScopeProject, ScopeID: f.ProjectID, InjectionMode: store.InjectionModeAlways,
	})
	require.NoError(t, err)
	constrainAllBut(t, f.Store, "user", f.UserID, secretUsePermission)

	agent, err := f.Store.GetAgent(context.Background(), direct)
	require.NoError(t, err)
	resolved, _, err := f.disp.resolveSecrets(context.Background(), agent)
	require.NoError(t, err)
	var got *ResolvedSecret
	for i := range resolved {
		if resolved[i].Name == sdAlwaysKey {
			got = &resolved[i]
		}
	}
	require.NotNil(t, got, "the project row with the same key is still delivered")
	assert.Equal(t, store.ScopeProject, got.Source)
	assert.Equal(t, "sd-project-value", got.Value)

	// The start and restart launch selection skip the refused owner row the
	// same way, before its value is read. The project row is still subject
	// to its own delivery decision there, so only the owner row's absence
	// and, when the key is delivered, its source are asserted.
	for _, purpose := range []MaterialPurpose{PurposeDispatch, PurposeRestart} {
		selected, err := f.disp.selectDeliverySecrets(context.Background(), newDeliverySelector(f.disp.authzService, agent, purpose), agent)
		require.NoError(t, err)
		for _, r := range selected {
			if r.Name == sdAlwaysKey {
				assert.Equal(t, store.ScopeProject, r.Source, "launch selection (%s): the refused owner row is not delivered", purpose)
				assert.Equal(t, "sd-project-value", r.Value)
			}
		}
	}
}

// TestStartDelivery_ParityWithRuntimeRead runs one chain matrix through
// start delivery (both sites) and the runtime read. On the progeny path
// start equals the read. On the owner path start equals the ceiling verdict
// (the read's step 10) for a recorded chain; where the read is narrower
// than the ceiling (the agent's own access constraint, which only Decide
// applies) the case says so. A chain without recorded provenance keeps its
// legacy behaviour on both paths (ptone-gated, not changed here): progeny
// start refuses it (project.secret_read needs recorded provenance) while
// the read applies the progeny rule; owner start delivers it.
func TestStartDelivery_ParityWithRuntimeRead(t *testing.T) {
	type built struct{ agentID, token string }
	cases := []struct {
		name      string
		owner     bool // owner path (direct agent) rather than progeny path
		build     func(t *testing.T, f *sdFixture) built
		wantStart bool
		wantRead  bool
		// recorded is set for an owner-path case whose chain has recorded
		// provenance: start must equal the ceiling verdict.
		recorded bool
	}{
		// Owner path.
		{"owner/clean", true, func(t *testing.T, f *sdFixture) built {
			id, tok := f.direct(t, "pm-o-clean", true)
			return built{id, tok}
		}, true, true, true},
		{"owner/bounded without secret.use", true, func(t *testing.T, f *sdFixture) built {
			id, tok := f.direct(t, "pm-o-bounded", false)
			f.recordEdge(t, store.DelegationPrincipalUser, f.UserID, id, boundedCeiling("project.read", "project.secret_read"), nil)
			return built{id, tok}
		}, false, false, true},
		{"owner/bounded with secret.use", true, func(t *testing.T, f *sdFixture) built {
			id, tok := f.direct(t, "pm-o-bounded-use", false)
			f.recordEdge(t, store.DelegationPrincipalUser, f.UserID, id, boundedCeiling("project.read", "project.secret_read", "secret.use"), nil)
			return built{id, tok}
		}, true, true, true},
		{"owner/terminal user secret.use constraint (S4b)", true, func(t *testing.T, f *sdFixture) built {
			id, tok := f.direct(t, "pm-o-s4b", true)
			constrainAllBut(t, f.Store, "user", f.UserID, secretUsePermission)
			return built{id, tok}
		}, false, false, true},
		{"owner/edge from another user (S8)", true, func(t *testing.T, f *sdFixture) built {
			other := tid("pm-o-s8-other")
			createDCUser(t, f.Store, other, "pm-o-s8-other@test.com", f.ProjectID, store.ProjectRoleOwner)
			id, tok := f.direct(t, "pm-o-s8", false)
			f.recordEdge(t, store.DelegationPrincipalUser, other, id, store.EffectCeiling{Kind: store.EffectCeilingPrincipal}, nil)
			return built{id, tok}
		}, false, false, true},
		{"owner/agent secret.use constraint (read only)", true, func(t *testing.T, f *sdFixture) built {
			id, tok := f.direct(t, "pm-o-agentc", true)
			constrainAllBut(t, f.Store, "agent", id, secretUsePermission)
			return built{id, tok}
		}, true, false, true},
		{"owner/edge without provenance (legacy)", true, func(t *testing.T, f *sdFixture) built {
			id, tok := f.direct(t, "pm-o-unrec", false)
			createDCEdge(t, f.Store, store.DelegationPrincipalUser, f.UserID, store.DelegationPrincipalAgent, id,
				store.RoleScopeProject, f.ProjectID, string(AgentRoleFull))
			return built{id, tok}
		}, true, true, false},
		// Progeny path.
		{"progeny/clean", false, func(t *testing.T, f *sdFixture) built {
			c, tok, _ := f.progeny(t, "pm-p-clean", true)
			return built{c, tok}
		}, true, true, false},
		{"progeny/bounded without secret.use", false, func(t *testing.T, f *sdFixture) built {
			c, tok, p := f.progeny(t, "pm-p-bounded", false)
			f.recordEdge(t, store.DelegationPrincipalAgent, p, c, boundedCeiling("project.read", "project.secret_read"), nil)
			return built{c, tok}
		}, false, false, false},
		{"progeny/intermediate agent held", false, func(t *testing.T, f *sdFixture) built {
			c, tok, p := f.progeny(t, "pm-p-held", true)
			holdAgent(t, f.Store, p, f.ProjectID, f.UserID)
			return built{c, tok}
		}, false, false, false},
		{"progeny/terminal user secret.use constraint (S4b)", false, func(t *testing.T, f *sdFixture) built {
			c, tok, _ := f.progeny(t, "pm-p-s4b", true)
			constrainAllBut(t, f.Store, "user", f.UserID, secretUsePermission)
			return built{c, tok}
		}, false, false, false},
		{"progeny/edge from another user (S8)", false, func(t *testing.T, f *sdFixture) built {
			other := tid("pm-p-s8-other")
			createDCUser(t, f.Store, other, "pm-p-s8-other@test.com", f.ProjectID, store.ProjectRoleOwner)
			c, tok, _ := f.progeny(t, "pm-p-s8", false)
			f.recordEdge(t, store.DelegationPrincipalUser, other, c, store.EffectCeiling{Kind: store.EffectCeilingPrincipal}, nil)
			return built{c, tok}
		}, false, false, false},
		{"progeny/agent secret.use constraint", false, func(t *testing.T, f *sdFixture) built {
			c, tok, _ := f.progeny(t, "pm-p-agentc", true)
			constrainAllBut(t, f.Store, "agent", c, secretUsePermission)
			return built{c, tok}
		}, false, false, false},
		{"progeny/edge without provenance (legacy)", false, func(t *testing.T, f *sdFixture) built {
			c, tok, p := f.progeny(t, "pm-p-unrec", false)
			createDCEdge(t, f.Store, store.DelegationPrincipalAgent, p, store.DelegationPrincipalAgent, c,
				store.RoleScopeProject, f.ProjectID, string(AgentRoleFull))
			return built{c, tok}
		}, false, true, false},
	}
	for i, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newSDFixture(t, "sd-pm-"+string(rune('a'+i)))
			b := tc.build(t, f)

			got := sdStartDelivery(t, f.disp, f.Store, b.agentID)
			assert.Equal(t, tc.wantStart, got.always, "resolveSecrets")
			assert.Equal(t, tc.wantStart, got.asNeeded, "resolveAsNeededForKeys")
			assert.Equal(t, got.always, got.asNeeded, "the two start sites agree")
			assert.Equal(t, tc.owner, got.private, "the unshared row is delivered on the owner path only, unchanged")

			readAlways := f.read(t, b.agentID, b.token, sdAlwaysKey)
			readAsNeeded := f.read(t, b.agentID, b.token, sdAsNeededKey)
			assert.Equal(t, tc.wantRead, readAlways, "runtime read of %s", sdAlwaysKey)
			assert.Equal(t, tc.wantRead, readAsNeeded, "runtime read of %s", sdAsNeededKey)

			if tc.recorded {
				d := f.ceiling(t, b.agentID, sdAlwaysKey)
				assert.Equal(t, d.Allowed, got.always, "owner path: start equals the ceiling verdict (reason %q)", d.Reason)
			}
		})
	}
}

// sdReincarnation is a reincarnate-test server with a real secret backend,
// the creator's shared secrets and a production dispatcher.
type sdReincarnation struct {
	srv     *Server
	s       store.Store
	project *store.Project
	broker  *store.RuntimeBroker
	disp    *HTTPAgentDispatcher
	creator string
}

func newSDReincarnation(t *testing.T) *sdReincarnation {
	t.Helper()
	srv, s, project, broker := setupReincarnateTestServer(t, newReincarnateTestDispatcher())
	setBackfillCompleted(t, s)
	srv.SetSecretBackend(secret.NewLocalBackend(s, "test-hub-id", "test-secret"))
	// newReincarnateTestAgent records this user as the agent's creator. It
	// owns the project (its only built-in membership), so the later standing
	// checks find a project binding and add none.
	creator := tid("user-creator")
	ensureActiveUser(t, s, creator)
	grantProjectRole(t, s, creator, project.ID, store.ProjectRoleOwner)
	sdShareOwnerSecrets(t, srv.secretBackend, creator)
	return &sdReincarnation{srv: srv, s: s, project: project, broker: broker, disp: sdDispatcher(srv, s), creator: creator}
}

// reincarnateByOtherUser has a second user B, who may update and reincarnate
// agents in the project and holds project.secret_read there, reincarnate the
// agent with a role change, and asserts the reincarnation re-recorded the
// agent's edge to B. It returns B.
func (r *sdReincarnation) reincarnateByOtherUser(t *testing.T, agentID string, role AgentRole) *store.User {
	t.Helper()
	b := newReincarnateAuthzUser(t, r.s, tidSlugSafe(t.Name()))
	grantProjectRole(t, r.s, b.ID, r.project.ID, store.ProjectRoleOwner)
	grantAgentLifecycleAtProject(t, r.s, b.ID, r.project.ID)
	grantAgentDelegationAtProject(t, r.s, b.ID, r.project.ID)
	grantPermissionViaRoleBinding(t, r.s, b.ID, "agent.update", store.RoleScopeProject, r.project.ID)
	id := NewAuthenticatedUser(b.ID, b.Email, b.DisplayName, b.Role, "web")

	rec := httptest.NewRecorder()
	r.srv.handleReincarnateAgent(rec, reincarnateRequest(t, agentID, id, ReincarnateAgentRequest{Handoff: "h", Role: string(role)}), agentID)
	require.Equal(t, http.StatusAccepted, rec.Code, rec.Body.String())
	waitForReincarnationSettled(t, r.s, agentID)

	edges, err := r.s.GetDelegationEdgesForDelegate(context.Background(), store.DelegationPrincipalAgent, agentID)
	require.NoError(t, err)
	require.Len(t, edges, 1)
	require.Equal(t, store.DelegationPrincipalUser, edges[0].DelegatorType)
	require.Equal(t, b.ID, edges[0].DelegatorID, "the reincarnation re-records the agent's authority to B")
	agent, err := r.s.GetAgent(context.Background(), agentID)
	require.NoError(t, err)
	require.Equal(t, r.creator, agent.Ancestry[0], "the ancestry still starts at the creator")
	return b
}

// readAllows is check 8 for the agent on the creator's row of key, with the
// agent's stored ancestry and the given token scopes.
func (r *sdReincarnation) readAllows(t *testing.T, agentID, key string, scopes []AgentTokenScope) bool {
	t.Helper()
	ctx := context.Background()
	agent, err := r.s.GetAgent(ctx, agentID)
	require.NoError(t, err)
	ident := newFullAgentIdentity(agentID, agent.ProjectID, agent.Ancestry, scopes)
	facts := &TargetFacts{
		Agent: agent, ProjectID: agent.ProjectID, Ancestry: agent.Ancestry,
		Root: ProvenanceRoot{Kind: "user", ID: agent.Ancestry[0]}, Project: r.project,
	}
	return r.srv.authorizeRuntimeUserItem(ctx, ident, facts, key).Allowed
}

// ceiling is the parent ceiling verdict for the agent on the creator's row
// of key.
func (r *sdReincarnation) ceiling(t *testing.T, agentID, key string) ParentCeilingDecision {
	t.Helper()
	ctx := context.Background()
	agent, err := r.s.GetAgent(ctx, agentID)
	require.NoError(t, err)
	meta, err := r.srv.secretBackend.GetMeta(ctx, key, store.ScopeUser, r.creator)
	require.NoError(t, err)
	ident := newFullAgentIdentity(agentID, agent.ProjectID, agent.Ancestry, []AgentTokenScope{ScopeProjectSecretRead})
	return r.srv.authzService.EvaluateUserSecretParentCeiling(ctx, ident, userSecretResource(meta.ID, meta.ScopeID))
}

// TestStartDelivery_CrossUserReincarnation (residual case (b), S8): another
// user B reincarnates the creator's agent with a role change, which
// re-records the agent's edge to B while its ancestry still starts at the
// creator. The read refuses the creator's shared secret (the terminal user B
// is not its owner at the head of the ancestry), and start delivery refuses
// it too, at both sites. Before the fix start delivered it: on the owner
// path with no check, on the progeny path through B's project.secret_read.
func TestStartDelivery_CrossUserReincarnation(t *testing.T) {
	t.Run("owner path", func(t *testing.T) {
		r := newSDReincarnation(t)
		agent := newReincarnateTestAgent(t, r.s, r.project, r.broker, nil)
		require.Equal(t, r.creator, agent.OwnerID)
		seedAgentEdge(t, r.s, r.creator, agent)

		// Before: the creator's own chain delivers and the ceiling allows.
		require.True(t, r.ceiling(t, agent.ID, sdAlwaysKey).Allowed)
		require.Equal(t, sdDelivery{always: true, asNeeded: true, private: true}, sdStartDelivery(t, r.disp, r.s, agent.ID))

		r.reincarnateByOtherUser(t, agent.ID, AgentRoleReadOnly)

		d := r.ceiling(t, agent.ID, sdAlwaysKey)
		require.False(t, d.Allowed)
		assert.Equal(t, DenyCauseCeilingSourceNotAllowed, d.DenyCause, "S8 denies; reason %q", d.Reason)
		assert.False(t, r.readAllows(t, agent.ID, sdAlwaysKey, []AgentTokenScope{ScopeProjectSecretRead}), "the runtime read refuses")
		assert.Equal(t, sdDelivery{always: false, asNeeded: false, private: true}, sdStartDelivery(t, r.disp, r.s, agent.ID),
			"start refuses the shared secret at both sites; the unshared row is unchanged")
	})

	t.Run("progeny path", func(t *testing.T) {
		r := newSDReincarnation(t)
		ctx := context.Background()
		parentID := tid("sd-reinc-parent")
		require.NoError(t, r.s.CreateAgent(ctx, &store.Agent{
			ID: parentID, Slug: "sd-reinc-parent", Name: "SD Reinc Parent", ProjectID: r.project.ID,
			RuntimeBrokerID: r.broker.ID, Phase: "running", StateVersion: 1,
			CreatedBy: r.creator, OwnerID: r.creator, Ancestry: []string{r.creator},
			AppliedConfig: &store.AgentAppliedConfig{AgentRole: string(AgentRoleFull)},
		}))
		parent, err := r.s.GetAgent(ctx, parentID)
		require.NoError(t, err)
		seedAgentEdge(t, r.s, r.creator, parent)
		child := newReincarnateTestAgent(t, r.s, r.project, r.broker, func(a *store.Agent) {
			a.Ancestry = []string{r.creator, parentID}
			a.OwnerID = parentID
			a.CreatedBy = parentID
		})
		require.NoError(t, r.s.CreateDelegationEdge(ctx, &store.DelegationEdge{
			DelegatorType: store.DelegationPrincipalAgent, DelegatorID: parentID,
			DelegateType: store.DelegationPrincipalAgent, DelegateID: child.ID,
			ScopeType: store.RoleScopeProject, ScopeID: r.project.ID, Role: string(AgentRoleBaseline), Active: true,
			AuthorityProvenance: store.AuthorityProvenance{
				ProvenanceVersion:    store.ProvenanceVersionV1,
				SourcePrincipalKind:  store.DelegationPrincipalAgent,
				SourcePrincipalID:    parentID,
				SourceCredentialKind: store.SourceCredentialAgent,
			},
			EffectCeiling: store.EffectCeiling{Kind: store.EffectCeilingPrincipal},
		}))

		r.reincarnateByOtherUser(t, child.ID, AgentRoleFull)
		reincarnated, err := r.s.GetAgent(ctx, child.ID)
		require.NoError(t, err)
		require.NotNil(t, reincarnated.AppliedConfig)
		require.Equal(t, string(AgentRoleFull), reincarnated.AppliedConfig.AgentRole,
			"the reincarnated agent carries the full role, so project:secret:read is in its scopes")

		assert.False(t, r.readAllows(t, child.ID, sdAlwaysKey, ScopesForRole(AgentRoleFull)), "the runtime read refuses")
		assert.Equal(t, sdDelivery{}, sdStartDelivery(t, r.disp, r.s, child.ID),
			"start refuses the creator's shared secrets at both sites")
	})
}

// TestStartDelivery_WithheldOwnerRowLoggedAtWarn: a shared owner row the
// start-time check refuses (the terminal user's secret.use constraint, S4b) is
// withheld from the launch selection and logged with exactly ONE Warn line, the
// rate-limited withheld record (path owner, its cause), never its value.
func TestStartDelivery_WithheldOwnerRowLoggedAtWarn(t *testing.T) {
	f := newSDFixture(t, "sd-warn")
	direct, _ := f.direct(t, "warn-direct", true)
	constrainAllBut(t, f.Store, "user", f.UserID, secretUsePermission)
	var buf bytes.Buffer
	f.disp.log = slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo}))

	agent, err := f.Store.GetAgent(context.Background(), direct)
	require.NoError(t, err)
	selected, err := f.disp.selectDeliverySecrets(context.Background(), newDeliverySelector(f.disp.authzService, agent, PurposeRestart), agent)
	require.NoError(t, err)
	for _, r := range selected {
		assert.False(t, r.Name == sdAlwaysKey && r.Source == store.ScopeUser, "the refused owner row was delivered")
	}

	recs := sdWarnRecordsFor(t, buf.String(), sdAlwaysKey)
	require.Len(t, recs, 1, "exactly one Warn record for the withheld owner row; log: %s", buf.String())
	assert.Equal(t, sdWithheldMsg, recs[0]["msg"])
	assert.Equal(t, "owner", recs[0]["path"])
	assert.Equal(t, direct, recs[0]["agent_id"])
	assert.NotEmpty(t, recs[0]["deny_cause"])
	assert.NotContains(t, buf.String(), sdAlwaysValue, "a withheld secret value was logged")
}

// TestStartDelivery_WithheldProgenyRowLoggedOnce: on the progeny path, a
// user row that secret.use refuses is withheld with ONE Warn line, the
// rate-limited withheld record, and not also the deny line.
func TestStartDelivery_WithheldProgenyRowLoggedOnce(t *testing.T) {
	f := newSDFixture(t, "sd-warn-progeny")
	child, _, _ := f.progeny(t, "warn-progeny", true)
	constrainAllBut(t, f.Store, "user", f.UserID, secretUsePermission)
	var buf bytes.Buffer
	f.disp.log = slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo}))

	agent, err := f.Store.GetAgent(context.Background(), child)
	require.NoError(t, err)
	selected, err := f.disp.selectDeliverySecrets(context.Background(), newDeliverySelector(f.disp.authzService, agent, PurposeRestart), agent)
	require.NoError(t, err)
	for _, r := range selected {
		assert.False(t, r.Name == sdAlwaysKey && r.Source == store.ScopeUser, "the refused progeny row was delivered")
	}

	recs := sdWarnRecordsFor(t, buf.String(), sdAlwaysKey)
	require.Len(t, recs, 1, "exactly one Warn record for the withheld progeny row; log: %s", buf.String())
	assert.Equal(t, sdWithheldMsg, recs[0]["msg"])
	assert.Equal(t, "progeny", recs[0]["path"])
	assert.Equal(t, child, recs[0]["agent_id"])
	assert.NotContains(t, buf.String(), sdAlwaysValue, "a withheld secret value was logged")
}

// sdWithheldMsg is the rate-limited Warn of a user row authorization keeps
// from the agent (startSecretResolveOpts' withheld).
const sdWithheldMsg = "start-time user secret withheld by authorization; the agent starts without it"

// sdWarnRecordsFor returns the WARN records of a JSON log that name secret.
func sdWarnRecordsFor(t *testing.T, raw, secretName string) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(raw), "\n") {
		var rec map[string]any
		if line == "" || json.Unmarshal([]byte(line), &rec) != nil {
			continue
		}
		if rec["level"] == "WARN" && rec["secret"] == secretName {
			out = append(out, rec)
		}
	}
	return out
}
