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

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/conduit/grant"
	"github.com/GoogleCloudPlatform/scion/pkg/secret"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// setConduitExperiment overrides hub.conduit on srv the way an admin toggle
// would; every other experiment keeps its production registry value.
func setConduitExperiment(t *testing.T, srv *Server, enabled bool) {
	t.Helper()
	fakeStore := newFakeHubSettingStore()
	fakeStore.seed("experiments", json.RawMessage(fmt.Sprintf(`{"overrides":{%q:%t}}`, conduitExperiment, enabled)))
	ops := NewOperationalSettings(fakeStore, emptyKoanf(), emptyKoanf())
	_, err := ops.Refresh(context.Background())
	require.NoError(t, err)
	srv.SetOperationalSettings(ops)
}

type conduitTestClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *conduitTestClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *conduitTestClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

type conduitFixture struct {
	srv      *Server
	store    store.Store
	clock    *conduitTestClock
	agent    *store.Agent
	owner    *AuthenticatedUser
	portOnly *ScopedUserIdentity
	stranger *AuthenticatedUser
}

func newConduitFixture(t *testing.T) *conduitFixture {
	t.Helper()
	srv, s := testServer(t)
	ctx := context.Background()
	f := &conduitFixture{srv: srv, store: s, clock: &conduitTestClock{now: time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)}}

	project := &store.Project{ID: tid("conduit-project"), Name: "Conduit", Slug: "conduit"}
	require.NoError(t, s.CreateProject(ctx, project))
	ownerID, strangerID := tid("conduit-owner"), tid("conduit-stranger")
	createTestUserWithProjectRole(t, s, ownerID, "owner@conduit.test", project.ID, store.ProjectRoleMember)
	createTestUserWithProjectRole(t, s, strangerID, "stranger@conduit.test", project.ID, store.ProjectRoleMember)
	ensureHubMembership(ctx, s, ownerID)
	ensureHubMembership(ctx, s, strangerID)
	f.owner = NewAuthenticatedUser(ownerID, "owner@conduit.test", "Owner", store.UserRoleMember, "api")
	f.stranger = NewAuthenticatedUser(strangerID, "stranger@conduit.test", "Stranger", store.UserRoleMember, "api")
	// A token holding only port access to the owner's agents: port
	// visibility without shell.
	f.portOnly = NewScopedUserIdentity(f.owner, project.ID, []string{store.UATScopeAgentPortAccess})

	f.agent = &store.Agent{
		ID: tid("conduit-agent"), Slug: "conduit-agent", Name: "Conduit Agent",
		ProjectID: project.ID, OwnerID: ownerID, Ancestry: []string{ownerID},
		RuntimeBrokerID: "broker-1", Phase: string(state.PhaseRunning),
	}
	require.NoError(t, s.CreateAgent(ctx, f.agent))
	require.NoError(t, s.UpdateAgentExposedPorts(ctx, f.agent.ID, []store.ExposedPort{{Port: 3000, Host: "127.0.0.1", Mode: "rw"}}))
	got, err := s.GetAgent(ctx, f.agent.ID)
	require.NoError(t, err)
	f.agent = got

	srv.conduitGrants = newConduitGrantKeys(&dbConduitGrantKeyStore{store: s, encryptionKey: srv.encryptionKey}, "", f.clock.Now)
	setConduitExperiment(t, srv, true)
	return f
}

func (f *conduitFixture) target() grant.Target {
	return grant.Target{Kind: grant.TargetKindAgent, ID: f.agent.ID, EndpointIncarnation: "inc-1", SessionID: "sess-1", ConnectionEpoch: 3}
}

func tcpHeader(port string) grant.StreamHeader {
	return grant.StreamHeader{Kind: grant.StreamKindTCP, Params: map[string]string{grant.ParamHost: "127.0.0.1", grant.ParamPort: port}}
}

func (f *conduitFixture) mint(ident Identity, h grant.StreamHeader) ([]byte, *grant.Claims, error) {
	return f.srv.mintConduitGrant(context.Background(), conduitGrantRequest{Identity: ident, Agent: f.agent, Stream: h, Target: f.target()})
}

// targetVerify plays the target: keys come from the hub's published set.
func (f *conduitFixture) targetVerify(t *testing.T, tok []byte, h grant.StreamHeader) error {
	t.Helper()
	pubs, err := f.srv.ConduitGrantPublicKeys(context.Background())
	require.NoError(t, err)
	keys, err := grant.NewKeySet(pubs...)
	require.NoError(t, err)
	_, err = grant.Verify(context.Background(), tok, keys, grant.Expectation{
		Target: f.target(), Header: h, ProjectID: f.agent.ProjectID, Issuer: conduitGrantIssuer,
	}, grant.NewMemoryReplayCache(f.clock.Now, 0), f.clock.Now())
	return err
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
	ptyTok, _, err := f.mint(f.owner, grant.StreamHeader{Kind: grant.StreamKindPTY})
	require.NoError(t, err)
	require.NoError(t, f.targetVerify(t, ptyTok, grant.StreamHeader{Kind: grant.StreamKindPTY}))
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
		{"stranger pty", f.stranger, grant.StreamHeader{Kind: grant.StreamKindPTY}, errConduitForbidden},
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

	req.Target = grant.Target{Kind: grant.TargetKindBroker, ID: "broker-2", EndpointIncarnation: "b", SessionID: "s", ConnectionEpoch: 1}
	_, _, err = f.srv.mintConduitGrant(ctx, req)
	assert.ErrorIs(t, err, errConduitInvalid)

	req.Target.ID = "broker-1"
	tok, claims, err := f.srv.mintConduitGrant(ctx, req)
	require.NoError(t, err)
	assert.NotEmpty(t, tok)
	assert.Equal(t, req.Target, claims.Target)
	assert.Equal(t, f.agent.ProjectID, claims.ProjectID)
	assert.LessOrEqual(t, claims.Expiry.Sub(claims.NotBefore), grant.MaxValidity)

	req.Target.Kind = "user"
	_, _, err = f.srv.mintConduitGrant(ctx, req)
	assert.ErrorIs(t, err, errConduitInvalid)
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

// TestConduitGrantKeys_SharedAcrossNodes: two hub nodes on one database sign
// with the same key set, including when they bootstrap concurrently.
func TestConduitGrantKeys_SharedAcrossNodes(t *testing.T) {
	_, s := testServer(t)
	ctx := context.Background()
	clock := &conduitTestClock{now: time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)}
	encKey := secret.DeriveLocalEncryptionKey("test-shared-secret")
	nodes := make([]*conduitGrantKeys, 8)
	for i := range nodes {
		nodes[i] = newConduitGrantKeys(&dbConduitGrantKeyStore{store: s, encryptionKey: encKey}, "", clock.Now)
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

func TestConduitGrantKeys_SharedSecretBootstrapIsDeterministic(t *testing.T) {
	clock := &conduitTestClock{now: time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)}
	var kids []string
	for range 2 {
		_, s := testServer(t) // separate databases
		k := newConduitGrantKeys(&dbConduitGrantKeyStore{store: s}, "shared", clock.Now)
		signer, err := k.signer(context.Background())
		require.NoError(t, err)
		kids = append(kids, signer.KeyID)
	}
	assert.Equal(t, kids[0], kids[1])
}

// TestConduitGrantKeys_Rotation: rotation by kid propagates to other nodes;
// the new key is published before it signs and the old key keeps verifying
// until its not_after.
func TestConduitGrantKeys_Rotation(t *testing.T) {
	_, s := testServer(t)
	ctx := context.Background()
	clock := &conduitTestClock{now: time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)}
	nodeA := newConduitGrantKeys(&dbConduitGrantKeyStore{store: s}, "", clock.Now)
	nodeB := newConduitGrantKeys(&dbConduitGrantKeyStore{store: s}, "", clock.Now)
	first, err := nodeA.signer(ctx)
	require.NoError(t, err)
	_, err = nodeB.signer(ctx)
	require.NoError(t, err)

	_, err = nodeA.rotate(ctx, time.Second, time.Hour)
	require.Error(t, err, "activation shorter than the refresh interval")

	activate := 10 * time.Minute
	kid, err := nodeA.rotate(ctx, activate, time.Hour)
	require.NoError(t, err)
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

type failingGrantKeyStore struct{}

func (failingGrantKeyStore) Load(context.Context) (*grant.KeyRing, error) {
	return nil, errors.New("db down")
}
func (failingGrantKeyStore) Create(context.Context, *grant.KeyRing) error { return nil }
func (failingGrantKeyStore) Update(context.Context, *grant.KeyRing) error { return nil }

func TestMintConduitGrant_KeyStoreErrorFailsClosed(t *testing.T) {
	f := newConduitFixture(t)
	f.srv.conduitGrants = newConduitGrantKeys(failingGrantKeyStore{}, "", f.clock.Now)
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
	ring, err := f.srv.conduitGrants.store.Load(context.Background())
	require.NoError(t, err)
	seed := ring.Keys[0].Seed
	for _, enc := range []string{base64.StdEncoding.EncodeToString(seed), base64.RawURLEncoding.EncodeToString(seed), fmt.Sprintf("%x", seed)} {
		assert.NotContains(t, rec.Body.String(), enc)
	}
	assert.NotContains(t, rec.Body.String(), "seed")

	rec = doRequest(t, f.srv, http.MethodPost, "/api/v1/conduit/grant-keys", nil)
	assert.Equal(t, http.StatusMethodNotAllowed, rec.Code)
}
