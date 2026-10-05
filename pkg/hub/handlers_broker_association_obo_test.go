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
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Associating a broker with a project, and removing an association, admit
// only user credentials. A broker request acting on a user's behalf is not
// admitted, whichever user it names. Every request below is HMAC-signed by
// a separate broker and runs through srv.Handler(), so the credential kind
// is the one BrokerAuthMiddleware records.

func TestBrokerAssociation_BrokerOnBehalfOfCannotLink(t *testing.T) {
	f := brokerAssocSetup(t, "assoc-obo-add")
	signer, key := newOnboardingSigningBroker(t, f.store, "assoc-obo-add")

	// The named user owns both the project and the broker.
	rec := doBrokerSignedRequest(t, f.srv, signer.ID, key, f.projectOwner.Email, http.MethodPost, f.providersPath(),
		AddProviderRequest{BrokerID: f.ownBroker.ID})

	require.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
	assert.Contains(t, rec.Body.String(), brokerProvideDeniedMessage)
	assertNoProvider(t, f.store, f.project.ID, f.ownBroker.ID)
	assertDefaultBroker(t, f.store, f.project.ID, "")
}

func TestBrokerAssociation_BrokerOnBehalfOfCannotLinkThroughRegister(t *testing.T) {
	f := brokerAssocSetup(t, "assoc-obo-register")
	signer, key := newOnboardingSigningBroker(t, f.store, "assoc-obo-register")

	rec := doBrokerSignedRequest(t, f.srv, signer.ID, key, f.projectOwner.Email, http.MethodPost, "/api/v1/projects/register",
		RegisterProjectRequest{ID: f.project.ID, Name: f.project.Name, BrokerID: f.ownBroker.ID})

	require.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
	assert.Contains(t, rec.Body.String(), brokerProvideDeniedMessage)
	assertNoProvider(t, f.store, f.project.ID, f.ownBroker.ID)
	assertDefaultBroker(t, f.store, f.project.ID, "")
}

func TestBrokerAssociation_BrokerOnBehalfOfCannotLinkThroughAgentCreate(t *testing.T) {
	f := brokerLinkAuthzSetup(t)
	ctx := context.Background()
	signer, key := newOnboardingSigningBroker(t, f.store, "assoc-obo-agent")
	f.srv.seedProjectCreatorMembership(ctx, f.proj)
	require.NoError(t, f.srv.createProjectOwnerRoleBinding(ctx, f.proj.ID, f.owner.ID))

	// The named user owns the project and the requested broker.
	rec := doBrokerSignedRequest(t, f.srv, signer.ID, key, f.owner.Email, http.MethodPost, "/api/v1/projects/"+f.proj.ID+"/agents",
		CreateAgentRequest{Name: "assoc-obo-agent", RuntimeBrokerID: f.unlinked.ID})

	require.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
	assertNoProvider(t, f.store, f.proj.ID, f.unlinked.ID)
	assertDefaultBroker(t, f.store, f.proj.ID, "")
	_, err := f.store.GetAgentBySlug(ctx, f.proj.ID, "assoc-obo-agent")
	assert.True(t, errors.Is(err, store.ErrNotFound), "no agent may be created, got %v", err)
}

// The explicit-broker link during agent creation applies the broker-side
// credential check itself: with the credential context a broker acting on
// the owner's behalf records, the link is denied before any write.
func TestBrokerAssociation_BrokerOnBehalfOfAgentCreateLinkDenied(t *testing.T) {
	f := brokerLinkAuthzSetup(t)
	f.srv.seedProjectCreatorMembership(context.Background(), f.proj)
	require.NoError(t, f.srv.createProjectOwnerRoleBinding(context.Background(), f.proj.ID, f.owner.ID))
	signer := NewBrokerIdentity(tid("assoc-obo-agent-link-signer"))
	owner := authUser(f.owner)
	userCtx := context.WithValue(contextWithIdentity(context.Background(), owner), userContextKey{}, owner)
	ctx := contextWithBrokerOnBehalfOf(userCtx, BrokerOnBehalfOf{Broker: signer, BrokerID: signer.ID()})
	ctx = contextWithCredentialContext(ctx, CredentialContext{Kind: CredentialKindBroker, ID: signer.ID(), Type: signer.Type()})

	w := httptest.NewRecorder()
	_, err := f.srv.resolveRuntimeBroker(ctx, w, f.unlinked.ID, f.proj)

	require.Error(t, err)
	assert.Equal(t, http.StatusForbidden, w.Code, w.Body.String())
	assert.Contains(t, w.Body.String(), brokerProvideDeniedMessage)
	assertNoProvider(t, f.store, f.proj.ID, f.unlinked.ID)
	assertDefaultBroker(t, f.store, f.proj.ID, "")

	// The same owner with an interactive credential links the broker.
	ctx = contextWithCredentialContext(userCtx, CredentialContext{Kind: CredentialKindInteractive, ID: f.owner.ID})
	w = httptest.NewRecorder()
	brokerID, err := f.srv.resolveRuntimeBroker(ctx, w, f.unlinked.ID, f.proj)
	require.NoError(t, err, w.Body.String())
	assert.Equal(t, f.unlinked.ID, brokerID)
}

func TestBrokerAssociation_BrokerOnBehalfOfCannotUnlink(t *testing.T) {
	cases := []struct {
		name  string
		actor func(f *brokerAssocFixture) *store.User
	}{
		// project.update arm: the named user owns the project.
		{"on behalf of the project owner", func(f *brokerAssocFixture) *store.User { return f.projectOwner }},
		// broker.update arm: the named user owns the broker.
		{"on behalf of the broker owner", func(f *brokerAssocFixture) *store.User { return f.brokerOwner }},
	}
	for i, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := brokerAssocSetup(t, "assoc-obo-unlink-"+string(rune('a'+i)))
			f.link(t, f.otherBroker)
			signer, key := newOnboardingSigningBroker(t, f.store, "assoc-obo-unlink-"+string(rune('a'+i)))

			rec := doBrokerSignedRequest(t, f.srv, signer.ID, key, tc.actor(f).Email, http.MethodDelete,
				f.providersPath()+"/"+f.otherBroker.ID, nil)

			require.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
			_, err := f.store.GetProjectProvider(context.Background(), f.project.ID, f.otherBroker.ID)
			assert.NoError(t, err, "the provider row stays")
		})
	}
}
