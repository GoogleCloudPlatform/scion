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
	"crypto/ed25519"
	"crypto/rand"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/conduit/registry"
	"github.com/GoogleCloudPlatform/scion/pkg/conduit/relay"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/store/entadapter"
	"github.com/GoogleCloudPlatform/scion/pkg/store/enttest"
)

// keyedTestPeer is a node enrolled with its own key in these tests.
type keyedTestPeer struct {
	id   string
	role relay.PeerRole
	pub  ed25519.PublicKey
	priv ed25519.PrivateKey
}

func newKeyedTestPeer(t *testing.T, id string, role relay.PeerRole) keyedTestPeer {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	return keyedTestPeer{id: id, role: role, pub: pub, priv: priv}
}

// keyedPeerAuth is the relay-peer authenticator of self, signing with its
// own enrolled key and verifying against peers.
func keyedPeerAuth(t *testing.T, self keyedTestPeer, peers ...keyedTestPeer) relay.PeerAuth {
	t.Helper()
	var es []relay.PeerEnrollment
	for _, p := range peers {
		es = append(es, relay.PeerEnrollment{ID: p.id, Role: p.role, Keys: []relay.PeerKey{{ID: "k1", PublicKey: p.pub}}})
	}
	auth, _, err := relay.NewConduitPeerAuth(relay.ConduitPeerAuthOptions{
		Mode: config.ConduitPeerAuthHMAC, SelfID: self.id, PeerSecret: string(externalPeerSecret),
		Peers: es, PeerKey: self.priv,
	})
	require.NoError(t, err)
	return auth
}

// TestHubInternal_EnrolledRoles: the hub internal API accepts a per-peer
// key signature from a peer enrolled as a relay end to end (it reaches the
// operation table), and refuses one from a peer enrolled as a hub.
func TestHubInternal_EnrolledRoles(t *testing.T) {
	relay0 := newKeyedTestPeer(t, "relay-0", relay.PeerRoleRelay)
	otherHub := newKeyedTestPeer(t, "hub-other", relay.PeerRoleHub)
	self := newKeyedTestPeer(t, "hub-ext", relay.PeerRoleHub)
	all := []keyedTestPeer{relay0, otherHub, self}

	f := &externalFixture{conduitFixture: newConduitFixture(t)}
	f.regStore = entadapter.NewConduitRegistryStore(enttest.NewClient(t))
	f.reg = registry.New(f.regStore, registry.Config{})
	opts := externalOptions(t, f)
	opts.PeerAuth = keyedPeerAuth(t, self, all...)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	f.srv.config.SharedSigningSecret = testRelaySessionSecret
	require.NoError(t, f.srv.StartConduitExternal(ctx, opts))
	srv := httptest.NewServer(f.srv.ConduitInternalHandler())
	t.Cleanup(srv.Close)
	body := []byte(`{}`)

	accepted := signedHubRequest(t, keyedPeerAuth(t, relay0, all...), srv.URL, hubInternalTestPath, relay.HubPeerTarget, 0, body)
	require.Equal(t, "3", accepted.Header.Get(relay.HeaderPeerSignatureVersion), "fixture: a per-peer key signature")
	assert.Equal(t, http.StatusNotFound, doRaw(t, accepted).StatusCode,
		"a relay's per-peer key signature passes every check and reaches the operation table")

	refused := doRaw(t, signedHubRequest(t, keyedPeerAuth(t, otherHub, all...), srv.URL, hubInternalTestPath, relay.HubPeerTarget, 0, body))
	assert.Equal(t, http.StatusForbidden, refused.StatusCode, "a peer enrolled as a hub may not call the hub internal API")
}
