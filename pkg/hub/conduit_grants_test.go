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
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/conduit/grant"
	"github.com/GoogleCloudPlatform/scion/pkg/secret"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/go-jose/go-jose/v4/jwt"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ptyHeader is a pty stream header with the contract params (contracts §2).
func ptyHeader(cols, rows string) grant.StreamHeader {
	return grant.StreamHeader{Kind: grant.StreamKindPTY, Params: map[string]string{
		grant.ParamCols: cols, grant.ParamRows: rows, grant.ParamSession: "scion",
	}}
}

func TestConduitStreamAction_Mapping(t *testing.T) {
	cases := map[string]Action{
		grant.StreamKindPTY:    ActionAttach,
		grant.StreamKindSSH:    ActionAttach,
		grant.StreamKindTCP:    ActionPortAccess,
		grant.StreamKindLogs:   ActionRead,
		grant.StreamKindEvents: ActionRead,
		"shell":                "",
		"":                     "",
	}
	for kind, want := range cases {
		assert.Equal(t, want, conduitStreamAction(kind), "kind %q", kind)
	}
	a, p := conduitAuthzFor(ActionTunnel)
	assert.Equal(t, ActionPortAccess, a, "ActionTunnel is granted wherever agent.port_access is")
	assert.Equal(t, "agent.port_access", p)
	_, p = conduitAuthzFor(ActionDelete)
	assert.Empty(t, p)
}

func TestMintConduitGrant_ExperimentOff(t *testing.T) {
	f := newConduitFixture(t)
	setConduitExperiment(t, f.srv, false)
	_, _, err := f.mint(f.owner, tcpHeader("3000"))
	assert.ErrorIs(t, err, errConduitDisabled)
	_, err = f.srv.ConduitGrantPublicKeys(context.Background())
	assert.ErrorIs(t, err, errConduitDisabled)
	_, err = f.srv.RotateConduitGrantKey(context.Background())
	assert.ErrorIs(t, err, errConduitDisabled)
}

// TestMintConduitGrant_PortOnlyUserCannotGetPTYOrSSH: C16 "a port-only user
// requesting PTY/SSH (refused at the hub and at the target)".
func TestMintConduitGrant_PortOnlyUserCannotGetPTYOrSSH(t *testing.T) {
	f := newConduitFixture(t)

	// Port access works for the port-only token...
	tok, claims, err := f.mint(f.portOnly, tcpHeader("3000"))
	require.NoError(t, err)
	assert.Equal(t, "user:"+f.owner.ID(), claims.Subject)
	require.NoError(t, f.targetVerify(t, tok, tcpHeader("3000")))

	// ...but the hub refuses shell kinds,
	for _, kind := range []string{grant.StreamKindPTY, grant.StreamKindSSH} {
		_, _, err := f.mint(f.portOnly, grant.StreamHeader{Kind: kind})
		assert.ErrorIs(t, err, errConduitForbidden, "kind %s", kind)
	}
	// and the target refuses its tcp grant re-aimed at a pty/ssh open.
	for _, kind := range []string{grant.StreamKindPTY, grant.StreamKindSSH} {
		assert.ErrorIs(t, f.targetVerify(t, tok, grant.StreamHeader{Kind: kind, Params: tcpHeader("3000").Params}), grant.ErrStream)
	}

	// The full-access owner gets a pty grant that verifies.
	ptyTok, _, err := f.mint(f.owner, ptyHeader("80", "24"))
	require.NoError(t, err)
	require.NoError(t, f.targetVerify(t, ptyTok, ptyHeader("80", "24")))
}

func TestMintConduitGrant_Authorization(t *testing.T) {
	f := newConduitFixture(t)
	cases := []struct {
		name  string
		ident Identity
		h     grant.StreamHeader
		want  error
	}{
		{"owner tcp", f.owner, tcpHeader("3000"), nil},
		{"owner ssh", f.owner, grant.StreamHeader{Kind: grant.StreamKindSSH}, nil},
		{"owner logs", f.owner, grant.StreamHeader{Kind: grant.StreamKindLogs}, nil},
		{"owner pty", f.owner, ptyHeader("80", "24"), nil},
		{"stranger pty", f.stranger, ptyHeader("80", "24"), errConduitForbidden},
		{"stranger tcp", f.stranger, tcpHeader("3000"), errConduitForbidden},
		{"stranger logs (project read)", f.stranger, grant.StreamHeader{Kind: grant.StreamKindEvents}, nil},
		{"port-only logs", f.portOnly, grant.StreamHeader{Kind: grant.StreamKindLogs}, errConduitForbidden},
		{"unknown kind", f.owner, grant.StreamHeader{Kind: "shell"}, errConduitInvalid},
		{"nil identity", nil, tcpHeader("3000"), errConduitInvalid},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, _, err := f.mint(tc.ident, tc.h)
			if tc.want == nil {
				assert.NoError(t, err)
			} else {
				assert.ErrorIs(t, err, tc.want)
			}
		})
	}
}

func TestMintConduitGrant_TCPTargetRules(t *testing.T) {
	f := newConduitFixture(t)
	f.srv.config.ConduitTCPAllowedPorts = []int{8080, 9810, 18380}
	cases := []struct {
		name   string
		params map[string]string
		want   error
	}{
		{"exposed port", map[string]string{"host": "127.0.0.1", "port": "3000"}, nil},
		{"allow-listed port", map[string]string{"host": "127.0.0.1", "port": "8080"}, nil},
		{"not exposed or allow-listed", map[string]string{"host": "127.0.0.1", "port": "4000"}, errConduitForbidden},
		{"hub port denied even if allow-listed", map[string]string{"host": "127.0.0.1", "port": "9810"}, errConduitForbidden},
		{"metadata port denied even if allow-listed", map[string]string{"host": "127.0.0.1", "port": "18380"}, errConduitForbidden},
		{"non-loopback host", map[string]string{"host": "10.0.0.1", "port": "3000"}, errConduitInvalid},
		{"localhost name", map[string]string{"host": "localhost", "port": "3000"}, errConduitInvalid},
		{"extra param", map[string]string{"host": "127.0.0.1", "port": "3000", "x": "y"}, errConduitInvalid},
		{"missing host", map[string]string{"port": "3000"}, errConduitInvalid},
		{"non-canonical port", map[string]string{"host": "127.0.0.1", "port": "03000"}, errConduitInvalid},
		{"port out of range", map[string]string{"host": "127.0.0.1", "port": "70000"}, errConduitInvalid},
		{"agent_id on an agent target", map[string]string{"host": "127.0.0.1", "port": "3000", grant.ParamAgentID: "x"}, errConduitInvalid},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, _, err := f.mint(f.owner, grant.StreamHeader{Kind: grant.StreamKindTCP, Params: tc.params})
			if tc.want == nil {
				assert.NoError(t, err)
			} else {
				assert.ErrorIs(t, err, tc.want)
			}
		})
	}
}

func TestMintConduitGrant_TargetBinding(t *testing.T) {
	f := newConduitFixture(t)
	ctx := context.Background()
	req := conduitGrantRequest{Identity: f.owner, Agent: f.agent, Stream: tcpHeader("3000")}

	req.Target = f.target()
	req.Target.ID = "other-agent"
	_, _, err := f.srv.mintConduitGrant(ctx, req)
	assert.ErrorIs(t, err, errConduitInvalid)

	req.Target = f.brokerTarget()
	req.Target.ID = "broker-2"
	_, _, err = f.srv.mintConduitGrant(ctx, req)
	assert.ErrorIs(t, err, errConduitInvalid)

	// A broker grant carries the authorized agent; an agent grant does not
	// (the target is the agent itself).
	req.Target = f.brokerTarget()
	tok, claims, err := f.srv.mintConduitGrant(ctx, req)
	require.NoError(t, err)
	assert.NotEmpty(t, tok)
	assert.Equal(t, req.Target, claims.Target)
	assert.Equal(t, f.agent.ProjectID, claims.ProjectID)
	assert.LessOrEqual(t, claims.Expiry.Sub(claims.NotBefore), grant.MaxValidity)
	assert.Equal(t, map[string]string{grant.ParamHost: "127.0.0.1", grant.ParamPort: "3000", grant.ParamAgentID: f.agent.ID}, claims.Stream.Params)

	_, claims, err = f.mint(f.owner, tcpHeader("3000"))
	require.NoError(t, err)
	assert.NotContains(t, claims.Stream.Params, grant.ParamAgentID)

	req.Target.Kind = "user"
	_, _, err = f.srv.mintConduitGrant(ctx, req)
	assert.ErrorIs(t, err, errConduitInvalid)
}

// TestMintConduitGrant_BrokerGrantBoundToAgent: a broker serves many agents,
// so a broker grant names the agent it was authorized for. The caller cannot
// change it, and a target expecting another agent refuses the grant.
func TestMintConduitGrant_BrokerGrantBoundToAgent(t *testing.T) {
	f := newConduitFixture(t)
	ctx := context.Background()
	mintFor := func(h grant.StreamHeader) ([]byte, *grant.Claims, error) {
		return f.srv.mintConduitGrant(ctx, conduitGrantRequest{Identity: f.owner, Agent: f.agent, Stream: h, Target: f.brokerTarget()})
	}

	for _, tc := range []struct {
		name string
		h    grant.StreamHeader
		want error
	}{
		{"tcp, hub sets agent", tcpHeader("3000"), nil},
		{"tcp, caller repeats the agent", grant.StreamHeader{Kind: grant.StreamKindTCP, Params: map[string]string{grant.ParamHost: "127.0.0.1", grant.ParamPort: "3000", grant.ParamAgentID: f.agent.ID}}, nil},
		{"tcp, caller names another agent", grant.StreamHeader{Kind: grant.StreamKindTCP, Params: map[string]string{grant.ParamHost: "127.0.0.1", grant.ParamPort: "3000", grant.ParamAgentID: "other-agent"}}, errConduitInvalid},
		{"tcp, caller sends an empty agent", grant.StreamHeader{Kind: grant.StreamKindTCP, Params: map[string]string{grant.ParamHost: "127.0.0.1", grant.ParamPort: "3000", grant.ParamAgentID: ""}}, errConduitInvalid},
		{"logs, hub sets agent", grant.StreamHeader{Kind: grant.StreamKindLogs}, nil},
		{"pty, caller names another agent", grant.StreamHeader{Kind: grant.StreamKindPTY, Params: map[string]string{grant.ParamAgentID: "other-agent"}}, errConduitInvalid},
		// A pty grant carries exactly cols, rows and session: the hub-set
		// agent_id makes a broker-target pty grant unmintable (Phase 3).
		{"pty, hub-set agent is refused", ptyHeader("80", "24"), errConduitInvalid},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, claims, err := mintFor(tc.h)
			if tc.want != nil {
				assert.ErrorIs(t, err, tc.want)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, f.agent.ID, claims.Stream.Params[grant.ParamAgentID])
		})
	}

	// The broker, routing to agent Y, refuses a grant minted for agent X.
	tok, _, err := mintFor(tcpHeader("3000"))
	require.NoError(t, err)
	pubs, err := f.srv.ConduitGrantPublicKeys(ctx)
	require.NoError(t, err)
	keys, err := grant.NewKeySet(pubs...)
	require.NoError(t, err)
	verifyAs := func(agentID string) error {
		h := tcpHeader("3000")
		h.Params[grant.ParamAgentID] = agentID
		_, err := grant.Verify(ctx, tok, keys, grant.Expectation{
			Target: f.brokerTarget(), Header: h, ProjectID: f.agent.ProjectID, Issuer: conduitGrantIssuer,
		}, grant.NewMemoryReplayCache(f.clock.Now, 0), f.clock.Now())
		return err
	}
	assert.ErrorIs(t, verifyAs("other-agent"), grant.ErrStream)
	assert.NoError(t, verifyAs(f.agent.ID))
}

// TestMintConduitGrant_StreamParamAllowList: only tcp (host and port) and
// pty (cols, rows and session; TestMintConduitGrant_PTYParams) take caller
// params; every other kind takes none.
func TestMintConduitGrant_StreamParamAllowList(t *testing.T) {
	f := newConduitFixture(t)
	for _, tc := range []struct {
		name string
		h    grant.StreamHeader
		want error
	}{
		{"pty with a command", grant.StreamHeader{Kind: grant.StreamKindPTY, Params: map[string]string{"cmd": "sh"}}, errConduitInvalid},
		{"ssh with a user", grant.StreamHeader{Kind: grant.StreamKindSSH, Params: map[string]string{"user": "root"}}, errConduitInvalid},
		{"logs with a source", grant.StreamHeader{Kind: grant.StreamKindLogs, Params: map[string]string{"source": "/etc"}}, errConduitInvalid},
		{"events with a filter", grant.StreamHeader{Kind: grant.StreamKindEvents, Params: map[string]string{"filter": "*"}}, errConduitInvalid},
		{"pty with tcp params", grant.StreamHeader{Kind: grant.StreamKindPTY, Params: map[string]string{grant.ParamHost: "127.0.0.1", grant.ParamPort: "3000"}}, errConduitInvalid},
		{"tcp with an extra param", grant.StreamHeader{Kind: grant.StreamKindTCP, Params: map[string]string{grant.ParamHost: "127.0.0.1", grant.ParamPort: "3000", "x": "y"}}, errConduitInvalid},
		// agent_id is only for broker targets, even when it names this agent.
		{"logs with agent_id on an agent target", grant.StreamHeader{Kind: grant.StreamKindLogs, Params: map[string]string{grant.ParamAgentID: f.agent.ID}}, errConduitInvalid},
		{"pty with agent_id on an agent target", grant.StreamHeader{Kind: grant.StreamKindPTY, Params: map[string]string{grant.ParamAgentID: f.agent.ID}}, errConduitInvalid},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, claims, err := f.mint(f.owner, tc.h)
			if tc.want != nil {
				assert.ErrorIs(t, err, tc.want)
				return
			}
			require.NoError(t, err)
			assert.Empty(t, claims.Stream.Params)
		})
	}
}

// TestMintConduitGrant_PTYParams: a pty grant carries exactly cols, rows
// and session (contracts §2): cols and rows canonical base-10 in 1..4096,
// session "scion". Anything else is refused before a grant is minted.
func TestMintConduitGrant_PTYParams(t *testing.T) {
	f := newConduitFixture(t)
	with := func(mod func(map[string]string)) grant.StreamHeader {
		h := ptyHeader("120", "40")
		mod(h.Params)
		return h
	}
	for _, tc := range []struct {
		name string
		h    grant.StreamHeader
		want error
	}{
		{"valid", ptyHeader("120", "40"), nil},
		{"bounds", ptyHeader("1", "4096"), nil},
		{"no params", grant.StreamHeader{Kind: grant.StreamKindPTY}, errConduitInvalid},
		{"missing cols", with(func(p map[string]string) { delete(p, grant.ParamCols) }), errConduitInvalid},
		{"missing rows", with(func(p map[string]string) { delete(p, grant.ParamRows) }), errConduitInvalid},
		{"missing session", with(func(p map[string]string) { delete(p, grant.ParamSession) }), errConduitInvalid},
		{"extra key", with(func(p map[string]string) { p["cmd"] = "sh" }), errConduitInvalid},
		{"leading zero", with(func(p map[string]string) { p[grant.ParamCols] = "080" }), errConduitInvalid},
		{"sign", with(func(p map[string]string) { p[grant.ParamRows] = "+24" }), errConduitInvalid},
		{"not a number", with(func(p map[string]string) { p[grant.ParamCols] = "wide" }), errConduitInvalid},
		{"empty value", with(func(p map[string]string) { p[grant.ParamRows] = "" }), errConduitInvalid},
		{"zero", with(func(p map[string]string) { p[grant.ParamCols] = "0" }), errConduitInvalid},
		{"4097", with(func(p map[string]string) { p[grant.ParamRows] = "4097" }), errConduitInvalid},
		{"other session", with(func(p map[string]string) { p[grant.ParamSession] = "other" }), errConduitInvalid},
		{"agent_id", with(func(p map[string]string) { p[grant.ParamAgentID] = f.agent.ID }), errConduitInvalid},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tok, claims, err := f.mint(f.owner, tc.h)
			if tc.want != nil {
				assert.ErrorIs(t, err, tc.want)
				assert.Nil(t, tok)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.h.Params, claims.Stream.Params, "the grant carries exactly the requested params")
			require.NoError(t, f.targetVerify(t, tok, tc.h))
		})
	}
}

// TestMintConduitGrant_PermissionCheckedBeforeTarget: a caller without port
// access gets the same refusal whether or not the port is exposed, so it
// cannot probe which ports are.
func TestMintConduitGrant_PermissionCheckedBeforeTarget(t *testing.T) {
	f := newConduitFixture(t)
	_, _, errExposed := f.mint(f.stranger, tcpHeader("3000"))
	_, _, errUnexposed := f.mint(f.stranger, tcpHeader("4000"))
	_, _, errReserved := f.mint(f.stranger, tcpHeader("9810"))
	_, _, errMalformed := f.mint(f.stranger, tcpHeader("03000"))
	for _, err := range []error{errExposed, errUnexposed, errReserved, errMalformed} {
		require.ErrorIs(t, err, errConduitForbidden)
		assert.Equal(t, errExposed.Error(), err.Error())
	}
}

func TestMintConduitGrant_ViaTunnelRequiresTunnelAction(t *testing.T) {
	f := newConduitFixture(t)
	ctx := context.Background()
	for _, tc := range []struct {
		name  string
		ident Identity
		want  error
	}{
		{"owner", f.owner, nil},
		{"port-only token (tunnel = port access)", f.portOnly, nil},
		{"stranger with project read", f.stranger, errConduitForbidden},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, _, err := f.srv.mintConduitGrant(ctx, conduitGrantRequest{
				Identity: tc.ident, Agent: f.agent, Stream: grant.StreamHeader{Kind: grant.StreamKindLogs}, Target: f.target(), ViaTunnel: true,
			})
			if tc.ident == Identity(f.portOnly) {
				// Logs needs agent.read, which the port-only token lacks.
				assert.ErrorIs(t, err, errConduitForbidden)
				return
			}
			if tc.want == nil {
				assert.NoError(t, err)
			} else {
				assert.ErrorIs(t, err, tc.want)
			}
		})
	}
	// Tunnel path for tcp works with the port-only token.
	_, _, err := f.srv.mintConduitGrant(ctx, conduitGrantRequest{
		Identity: f.portOnly, Agent: f.agent, Stream: tcpHeader("3000"), Target: f.target(), ViaTunnel: true,
	})
	assert.NoError(t, err)
}

func TestMintConduitGrant_AgentCaller(t *testing.T) {
	f := newConduitFixture(t)
	self := newTestAgentIdentity(f.agent.ID, f.agent.ProjectID, []AgentTokenScope{ScopeProjectRead})
	other := newTestAgentIdentity("other-agent", f.agent.ProjectID, []AgentTokenScope{ScopeProjectRead})
	foreign := newTestAgentIdentity("foreign-agent", "other-project", []AgentTokenScope{ScopeProjectRead})

	tok, claims, err := f.mint(self, tcpHeader("3000"))
	require.NoError(t, err)
	assert.Equal(t, "agent:"+f.agent.ID, claims.Subject)
	assert.NotEmpty(t, tok)

	_, _, err = f.mint(other, tcpHeader("3000"))
	assert.ErrorIs(t, err, errConduitForbidden, "agents only reach their own ports")
	_, _, err = f.mint(foreign, grant.StreamHeader{Kind: grant.StreamKindLogs})
	assert.ErrorIs(t, err, errConduitForbidden, "cross-project agent")
	_, _, err = f.mint(other, grant.StreamHeader{Kind: grant.StreamKindPTY})
	assert.ErrorIs(t, err, errConduitForbidden, "agent without lifecycle scope cannot attach")
}

// decideSpy counts AuthzService.Decide calls through the decision audit
// hook (every decision is audited at sample rate 1).
type decideSpy struct {
	mu    sync.Mutex
	calls int
}

func (d *decideSpy) EmitDecisionAudit(context.Context, *store.DecisionAuditRecord) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.calls++
}

func (d *decideSpy) count() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.calls
}

// TestMintConduitGrant_AgentReadAndPortSkipKernel: with a real agent token
// identity, the agent rules alone decide read and port streams; neither
// path reaches Decide. Reads are denied, as on the agent logs and events
// routes, where no agent scope grants agent.read.
func TestMintConduitGrant_AgentReadAndPortSkipKernel(t *testing.T) {
	f := newConduitFixture(t)
	spy := &decideSpy{}
	f.srv.authzService.DecisionAuditSampleRate = 1.0
	f.srv.authzService.SetDecisionAuditEmitter(spy)
	agentToken := func(id, projectID string, scopes ...AgentTokenScope) AgentIdentity {
		return &agentIdentityWrapper{&AgentTokenClaims{
			Claims:      jwt.Claims{Subject: id},
			ProjectID:   projectID,
			Scopes:      scopes,
			Ancestry:    f.agent.Ancestry,
			ScopeSchema: CurrentAgentScopeSchema,
		}}
	}
	self := agentToken(f.agent.ID, f.agent.ProjectID, ScopeProjectRead)
	peer := agentToken(tid("peer-agent"), f.agent.ProjectID, ScopeProjectRead)
	foreign := agentToken(tid("foreign-agent"), "other-project", ScopeProjectRead)
	noScope := agentToken(f.agent.ID, f.agent.ProjectID)

	// The kernel denies the equivalent logs-route read for the same token.
	require.False(t, f.srv.authzService.CheckAccess(context.Background(), self, agentResource(f.agent), ActionRead).Allowed)
	require.Equal(t, 1, spy.count(), "spy observes Decide")

	tests := []struct {
		name    string
		ident   AgentIdentity
		header  grant.StreamHeader
		allowed bool
	}{
		{"self logs", self, grant.StreamHeader{Kind: grant.StreamKindLogs}, false},
		{"self events", self, grant.StreamHeader{Kind: grant.StreamKindEvents}, false},
		{"peer logs", peer, grant.StreamHeader{Kind: grant.StreamKindLogs}, false},
		{"peer events", peer, grant.StreamHeader{Kind: grant.StreamKindEvents}, false},
		{"logs without project:read", noScope, grant.StreamHeader{Kind: grant.StreamKindLogs}, false},
		{"foreign logs", foreign, grant.StreamHeader{Kind: grant.StreamKindLogs}, false},
		{"self tcp", self, tcpHeader("3000"), true},
		{"peer tcp", peer, tcpHeader("3000"), false},
		{"foreign tcp", foreign, tcpHeader("3000"), false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			before := spy.count()
			_, _, err := f.mint(tc.ident, tc.header)
			if tc.allowed {
				require.NoError(t, err)
			} else {
				require.ErrorIs(t, err, errConduitForbidden)
			}
			assert.Equal(t, before, spy.count(), "agent read/port streams must not reach Decide")
		})
	}
}

func newTestDBGrantKeyStore(t *testing.T, s store.SecretStore) *dbConduitGrantKeyStore {
	t.Helper()
	db, err := newDBConduitGrantKeyStore(s, testGrantEncryptionKey)
	require.NoError(t, err)
	return db
}

// TestConduitGrantKeys_SharedAcrossNodes: hub nodes on one database sign
// with the same key set, including when they bootstrap concurrently.
func TestConduitGrantKeys_SharedAcrossNodes(t *testing.T) {
	_, s := testServer(t)
	ctx := context.Background()
	clock := &conduitTestClock{now: time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)}
	nodes := make([]*conduitGrantKeys, 8)
	for i := range nodes {
		nodes[i] = newConduitGrantKeys(newTestDBGrantKeyStore(t, s), clock.Now)
	}
	kids := make([]string, len(nodes))
	var wg sync.WaitGroup
	for i, n := range nodes {
		wg.Add(1)
		go func() {
			defer wg.Done()
			signer, err := n.signer(ctx)
			if err != nil {
				t.Errorf("node %d: %v", i, err)
				return
			}
			kids[i] = signer.KeyID
		}()
	}
	wg.Wait()
	for i := range kids {
		assert.Equal(t, kids[0], kids[i], "node %d signs with a different key", i)
	}

	// Stored encrypted; no private material in plaintext.
	raw, err := s.GetSecretValue(ctx, conduitGrantKeySecretName, store.ScopeHub, conduitGrantKeyScopeID)
	require.NoError(t, err)
	assert.True(t, strings.HasPrefix(raw, "enc:"), "ring must be encrypted at rest")
	assert.NotContains(t, raw, "seed")
}

// TestConduitGrantKeys_BootstrapKeyIsRandom: the first key is not derived
// from any hub secret, and independent bootstraps differ.
func TestConduitGrantKeys_BootstrapKeyIsRandom(t *testing.T) {
	const sharedSecret = "test-shared-secret"
	clock := &conduitTestClock{now: time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)}
	derived := deriveSharedSigningKey(sharedSecret, conduitGrantKeySecretName+":bootstrap")
	var seeds [][]byte
	for range 2 {
		_, s := testServer(t) // separate databases, same shared secret
		db, err := newDBConduitGrantKeyStore(s, secret.DeriveLocalEncryptionKey(sharedSecret))
		require.NoError(t, err)
		k := newConduitGrantKeys(db, clock.Now)
		_, err = k.signer(context.Background())
		require.NoError(t, err)
		ring, _, err := db.Load(context.Background())
		require.NoError(t, err)
		require.Len(t, ring.Keys, 1)
		assert.NotEqual(t, derived, ring.Keys[0].Seed, "bootstrap key must not be derived from the shared secret")
		seeds = append(seeds, ring.Keys[0].Seed)
	}
	assert.NotEqual(t, seeds[0], seeds[1], "independent bootstraps must differ")
}

// TestConduitGrantKeySet_EphemeralWithoutEncryption: with no encryption at
// rest configured the ring is never written to the secret store; it works
// from memory, and a restart (or another node) gets a different ring.
func TestConduitGrantKeySet_EphemeralWithoutEncryption(t *testing.T) {
	ctx := context.Background()
	_, err := newDBConduitGrantKeyStore(nil, nil)
	require.Error(t, err, "persistence requires an encryption key")

	srv, s := testServer(t)
	srv.encryptionKey = nil
	setConduitExperiment(t, srv, true)
	restart := func() {
		srv.conduitGrantsOnce = sync.Once{}
		srv.conduitGrants = nil
	}
	var kids []string
	for range 2 { // one hub instance, then the same hub restarted
		restart()
		k := srv.conduitGrantKeySet()
		_, ok := k.store.(*memoryConduitGrantKeyStore)
		require.True(t, ok, "no encryption key: ring must be held in memory")
		signer, err := k.signer(ctx)
		require.NoError(t, err)
		rot, err := srv.RotateConduitGrantKey(ctx)
		require.NoError(t, err)
		assert.NotEqual(t, signer.KeyID, rot.KeyID)
		pubs, err := srv.ConduitGrantPublicKeys(ctx)
		require.NoError(t, err)
		assert.Len(t, pubs, 2, "rotation works in memory")
		kids = append(kids, signer.KeyID)
	}
	assert.NotEqual(t, kids[0], kids[1], "a restarted hub gets a new ring")

	_, err = s.GetSecret(ctx, conduitGrantKeySecretName, store.ScopeHub, conduitGrantKeyScopeID)
	assert.ErrorIs(t, err, store.ErrNotFound, "nothing is written to the secret store")

	// With encryption configured the ring is persisted.
	srv.encryptionKey = testGrantEncryptionKey
	restart()
	_, err = srv.ConduitGrantPublicKeys(ctx)
	require.NoError(t, err)
	_, ok := srv.conduitGrantKeySet().store.(*dbConduitGrantKeyStore)
	assert.True(t, ok)
	_, err = s.GetSecret(ctx, conduitGrantKeySecretName, store.ScopeHub, conduitGrantKeyScopeID)
	assert.NoError(t, err)
}

// TestConduitGrantKeySet_EphemeralLogs: the memory-only ring is announced
// once per server at INFO, with a one-time WARN only where nodes must share
// signing keys, and no key material in either.
func TestConduitGrantKeySet_EphemeralLogs(t *testing.T) {
	for _, tc := range []struct {
		name       string
		stableKeys bool
		wantWarn   bool
	}{
		{"single node", false, false},
		{"nodes must share keys", true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, _ := testServer(t)
			srv.encryptionKey = nil
			srv.config.RequireStableSigningKey = tc.stableKeys
			srv.conduitGrantsOnce = sync.Once{}
			srv.conduitGrants = nil
			buf := captureSlogDefault(t)

			ctx := context.Background()
			for range 3 {
				_, err := srv.conduitGrantKeySet().signer(ctx)
				require.NoError(t, err)
			}
			logs := buf.String()
			assert.Equal(t, 1, strings.Count(logs, "Conduit grant key ring is ephemeral"), logs)
			assert.Contains(t, logs, "level=INFO msg=\"Conduit grant key ring is ephemeral")
			wantWarns := 0
			if tc.wantWarn {
				wantWarns = 1
			}
			assert.Equal(t, wantWarns, strings.Count(logs, "level=WARN msg=\"Conduit grant key ring is per node"), logs)

			ring, _, err := srv.conduitGrantKeySet().store.Load(ctx)
			require.NoError(t, err)
			assertNoKeyMaterial(t, logs, ring)
		})
	}
}

// TestConduitGrantKeys_UnreadableRingFailsClosedAndLogs: a stored ring this
// node cannot decrypt (for example a changed shared signing secret) is not
// replaced; mint fails closed and an actionable ERROR naming the secret is
// logged at most once per refresh interval.
func TestConduitGrantKeys_UnreadableRingFailsClosedAndLogs(t *testing.T) {
	_, s := testServer(t)
	ctx := context.Background()
	clock := &conduitTestClock{now: time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)}
	writer := newConduitGrantKeys(newTestDBGrantKeyStore(t, s), clock.Now)
	_, err := writer.signer(ctx)
	require.NoError(t, err)

	other, err := newDBConduitGrantKeyStore(s, secret.DeriveLocalEncryptionKey("a-different-secret"))
	require.NoError(t, err)
	node := newConduitGrantKeys(other, clock.Now)
	buf := captureSlogDefault(t)

	const logLine = "Conduit grant key ring cannot be read"
	for range 3 {
		_, err = node.signer(ctx)
		require.ErrorIs(t, err, errConduitGrantRingUnreadable)
	}
	logs := buf.String()
	assert.Equal(t, 1, strings.Count(logs, logLine), "logged once per refresh interval")
	assert.Contains(t, logs, "level=ERROR")
	assert.Contains(t, logs, "secret_key="+conduitGrantKeySecretName)
	assert.Contains(t, logs, "scope_id="+conduitGrantKeyScopeID)
	assert.Contains(t, logs, "Discarding the ring is safe")

	clock.Advance(conduitGrantKeyRefresh - time.Second)
	_, err = node.publicKeys(ctx)
	require.ErrorIs(t, err, errConduitGrantRingUnreadable)
	assert.Equal(t, 1, strings.Count(buf.String(), logLine))
	clock.Advance(time.Second)
	_, err = node.publicKeys(ctx)
	require.ErrorIs(t, err, errConduitGrantRingUnreadable)
	assert.Equal(t, 2, strings.Count(buf.String(), logLine), "logged again after the refresh interval")

	// Not replaced: the stored ring is still the writer's.
	ring, _, err := newTestDBGrantKeyStore(t, s).Load(ctx)
	require.NoError(t, err)
	wsigner, err := writer.signer(ctx)
	require.NoError(t, err)
	assert.Equal(t, wsigner.KeyID, ring.Keys[0].KeyID)
	assertNoKeyMaterial(t, buf.String(), ring)
}

// TestConduitGrantKeys_Rotation: rotation by kid propagates to other nodes;
// the new key is published before it signs and the old key keeps verifying
// until its not_after.
func TestConduitGrantKeys_Rotation(t *testing.T) {
	_, s := testServer(t)
	ctx := context.Background()
	clock := &conduitTestClock{now: time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)}
	nodeA := newConduitGrantKeys(newTestDBGrantKeyStore(t, s), clock.Now)
	nodeB := newConduitGrantKeys(newTestDBGrantKeyStore(t, s), clock.Now)
	first, err := nodeA.signer(ctx)
	require.NoError(t, err)
	_, err = nodeB.signer(ctx)
	require.NoError(t, err)

	_, err = nodeA.rotate(ctx, time.Second, time.Hour)
	require.Error(t, err, "activation shorter than the refresh interval")

	activate := 10 * time.Minute
	rot, err := nodeA.rotate(ctx, activate, time.Hour)
	require.NoError(t, err)
	kid := rot.KeyID
	require.NotEqual(t, first.KeyID, kid)

	// Node B picks up the new ring after its refresh interval and publishes
	// both keys, still signing with the old one.
	clock.Advance(conduitGrantKeyRefresh)
	pubs, err := nodeB.publicKeys(ctx)
	require.NoError(t, err)
	require.Len(t, pubs, 2)
	sB, err := nodeB.signer(ctx)
	require.NoError(t, err)
	assert.Equal(t, first.KeyID, sB.KeyID)

	clock.Advance(activate)
	sB, err = nodeB.signer(ctx)
	require.NoError(t, err)
	assert.Equal(t, kid, sB.KeyID, "new key signs after activation")
	assert.False(t, pubs[0].NotAfter.IsZero(), "old key has a retirement time")

	clock.Advance(2 * time.Hour)
	pubs, err = nodeB.publicKeys(ctx)
	require.NoError(t, err)
	require.Len(t, pubs, 1)
	assert.Equal(t, kid, pubs[0].KeyID)
}

// TestConduitGrantKeys_ConcurrentRotationsLoseNoKey: rotations racing on
// several nodes against one store never overwrite each other: exactly one
// lands, every other one is refused because its fresh reload sees the
// winner's key pending, and the ring holds the bootstrap key plus the
// winner's.
func TestConduitGrantKeys_ConcurrentRotationsLoseNoKey(t *testing.T) {
	for _, tc := range []struct {
		name string
		st   func(t *testing.T) conduitGrantKeyStore
	}{
		{"database", func(t *testing.T) conduitGrantKeyStore { _, s := testServer(t); return newTestDBGrantKeyStore(t, s) }},
		{"memory", func(*testing.T) conduitGrantKeyStore { return &memoryConduitGrantKeyStore{} }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			clock := &conduitTestClock{now: time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)}
			st := tc.st(t)
			first, err := newConduitGrantKeys(st, clock.Now).signer(ctx)
			require.NoError(t, err)

			const n = conduitGrantKeyRotateAttempts - 1
			kids := make([]string, n)
			errs := make([]error, n)
			start := make(chan struct{})
			var wg sync.WaitGroup
			for i := range n {
				node := newConduitGrantKeys(st, clock.Now)
				wg.Add(1)
				go func() {
					defer wg.Done()
					<-start
					rot, err := node.rotate(ctx, conduitGrantKeyDefaultActivation, time.Hour)
					kids[i], errs[i] = rot.KeyID, err
				}()
			}
			close(start)
			wg.Wait()

			winner := ""
			for i, err := range errs {
				if err == nil {
					require.Empty(t, winner, "more than one concurrent rotation landed")
					winner = kids[i]
					continue
				}
				assert.ErrorIs(t, err, errConduitGrantKeyPending, "rotation %d", i)
			}
			require.NotEmpty(t, winner, "no rotation landed")

			ring, _, err := st.Load(ctx)
			require.NoError(t, err)
			have := map[string]bool{}
			for _, k := range ring.Keys {
				have[k.KeyID] = true
			}
			assert.True(t, have[first.KeyID], "bootstrap key lost")
			assert.True(t, have[winner], "winning rotation's key lost")
			assert.Len(t, ring.Keys, 2)
		})
	}
}

// TestConduitGrantKeys_RotationRefusedUntilActivated (ptone/scion#3653): a
// rotation while the previously rotated-in key has not activated returns
// errConduitGrantKeyPending and leaves the stored ring byte-for-byte
// unchanged; from the activation instant on, rotation succeeds as before.
func TestConduitGrantKeys_RotationRefusedUntilActivated(t *testing.T) {
	for _, tc := range []struct {
		name string
		// st returns the store and a raw snapshot of what it holds.
		st func(t *testing.T) (conduitGrantKeyStore, func() string)
	}{
		{"database", func(t *testing.T) (conduitGrantKeyStore, func() string) {
			_, s := testServer(t)
			raw := func() string {
				rec, err := s.GetSecret(context.Background(), conduitGrantKeySecretName, store.ScopeHub, conduitGrantKeyScopeID)
				require.NoError(t, err)
				return fmt.Sprintf("v%d:%s", rec.Version, rec.EncryptedValue)
			}
			return newTestDBGrantKeyStore(t, s), raw
		}},
		{"memory", func(*testing.T) (conduitGrantKeyStore, func() string) {
			m := &memoryConduitGrantKeyStore{}
			raw := func() string {
				m.mu.Lock()
				defer m.mu.Unlock()
				b, err := json.Marshal(m.ring)
				require.NoError(t, err)
				return fmt.Sprintf("v%d:%s", m.rev, b)
			}
			return m, raw
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			clock := &conduitTestClock{now: time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)}
			st, raw := tc.st(t)
			k := newConduitGrantKeys(st, clock.Now)
			_, err := k.signer(ctx)
			require.NoError(t, err)

			const activate = 10 * time.Minute
			rot, err := k.rotate(ctx, activate, time.Hour)
			require.NoError(t, err)

			// Load before snapshotting, as a reader would: the snapshot must
			// also hold the decoded ring and revision.
			ringBefore, revBefore, err := st.Load(ctx)
			require.NoError(t, err)
			jsonBefore, err := json.Marshal(ringBefore)
			require.NoError(t, err)
			before := raw()

			for _, d := range []time.Duration{0, time.Minute, activate - time.Minute - time.Nanosecond} {
				clock.Advance(d)
				require.True(t, clock.Now().Before(rot.ActivateAt))
				// Another node too: the check reads the stored ring.
				for name, node := range map[string]*conduitGrantKeys{"same node": k, "other node": newConduitGrantKeys(st, clock.Now)} {
					_, err := node.rotate(ctx, activate, time.Hour)
					require.ErrorIs(t, err, errConduitGrantKeyPending, "%s at %s", name, clock.Now())
					assert.Equal(t, before, raw(), "stored ring changed by a refused rotation")
					ring, rev, err := st.Load(ctx)
					require.NoError(t, err)
					got, err := json.Marshal(ring)
					require.NoError(t, err)
					assert.Equal(t, string(jsonBefore), string(got))
					assert.Equal(t, revBefore, rev)
				}
			}

			clock.Advance(time.Nanosecond)
			require.True(t, clock.Now().Equal(rot.ActivateAt))
			next, err := k.rotate(ctx, activate, time.Hour)
			require.NoError(t, err, "rotation at activation succeeds")
			assert.NotEqual(t, rot.KeyID, next.KeyID)
			ring, _, err := st.Load(ctx)
			require.NoError(t, err)
			assert.Len(t, ring.Keys, 3, "bootstrap key (in overlap), the activated key and the new one")
		})
	}
}

// casLosingStore always loses the compare-and-swap.
type casLosingStore struct{ *memoryConduitGrantKeyStore }

func (casLosingStore) CompareAndSwap(context.Context, *grant.KeyRing, int) (bool, error) {
	return false, nil
}

func TestConduitGrantKeys_RotationGivesUpAfterBoundedRetries(t *testing.T) {
	clock := &conduitTestClock{now: time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)}
	k := newConduitGrantKeys(casLosingStore{&memoryConduitGrantKeyStore{}}, clock.Now)
	_, err := k.rotate(context.Background(), conduitGrantKeyDefaultActivation, time.Hour)
	require.Error(t, err)
}

func TestServer_ConduitGrantKeyActivationSetting(t *testing.T) {
	srv := &Server{}
	assert.Equal(t, conduitGrantKeyDefaultActivation, srv.conduitGrantKeyActivation())
	srv.config.ConduitGrantKeyActivation = 2 * time.Hour
	assert.Equal(t, 2*time.Hour, srv.conduitGrantKeyActivation())
}

// nilSecretStore returns (nil, nil) from GetSecret, as a store
// implementation may for a missing row.
type nilSecretStore struct{ store.SecretStore }

func (nilSecretStore) GetSecret(context.Context, string, string, string) (*store.Secret, error) {
	return nil, nil
}

func TestDBConduitGrantKeyStore_NilRecordIsNotFound(t *testing.T) {
	db := newTestDBGrantKeyStore(t, nilSecretStore{})
	_, _, err := db.Load(context.Background())
	assert.ErrorIs(t, err, store.ErrNotFound)
}

type failingGrantKeyStore struct{}

func (failingGrantKeyStore) Load(context.Context) (*grant.KeyRing, int, error) {
	return nil, 0, errors.New("db down")
}
func (failingGrantKeyStore) Create(context.Context, *grant.KeyRing) error { return nil }
func (failingGrantKeyStore) CompareAndSwap(context.Context, *grant.KeyRing, int) (bool, error) {
	return true, nil
}

func TestMintConduitGrant_KeyStoreErrorFailsClosed(t *testing.T) {
	f := newConduitFixture(t)
	f.srv.conduitGrants = newConduitGrantKeys(failingGrantKeyStore{}, f.clock.Now)
	_, _, err := f.mint(f.owner, tcpHeader("3000"))
	require.Error(t, err)
}

func TestConduitGrantKeysEndpoint(t *testing.T) {
	f := newConduitFixture(t)

	setConduitExperiment(t, f.srv, false)
	rec := doRequest(t, f.srv, http.MethodGet, "/api/v1/conduit/grant-keys", nil)
	assert.Equal(t, http.StatusNotFound, rec.Code, "gated by the experiment")

	setConduitExperiment(t, f.srv, true)
	rec = doRequest(t, f.srv, http.MethodGet, "/api/v1/conduit/grant-keys", nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var body conduitGrantKeysResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	require.Len(t, body.Keys, 1)
	assert.Len(t, body.Keys[0].PublicKey, 32)

	// Public halves only: neither the seed nor the expanded private key
	// appears in the response.
	ring, _, err := f.srv.conduitGrants.store.Load(context.Background())
	require.NoError(t, err)
	seed := ring.Keys[0].Seed
	for _, enc := range []string{base64.StdEncoding.EncodeToString(seed), base64.RawURLEncoding.EncodeToString(seed), fmt.Sprintf("%x", seed)} {
		assert.NotContains(t, rec.Body.String(), enc)
	}
	assert.NotContains(t, rec.Body.String(), "seed")

	rec = doRequest(t, f.srv, http.MethodPost, "/api/v1/conduit/grant-keys", nil)
	assert.Equal(t, http.StatusMethodNotAllowed, rec.Code)
}
