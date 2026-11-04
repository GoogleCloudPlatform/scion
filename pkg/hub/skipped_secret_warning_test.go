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

package hub

import (
	"context"
	"errors"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/secret"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
)

const skippedWarnPrefix = "secrets not delivered because the secret store could not return them: "

func TestFormatSkippedSecretWarning(t *testing.T) {
	proj := func(n string) skippedSecret {
		return skippedSecret{Name: n, Scope: store.ScopeProject, Class: skipClassAccess}
	}
	user := func(n string) skippedSecret {
		return skippedSecret{Name: n, Scope: store.ScopeUser, Class: skipClassAccess}
	}
	hub := func(n string) skippedSecret {
		return skippedSecret{Name: n, Scope: store.ScopeHub, Class: skipClassNotFound}
	}
	broker := func(n string) skippedSecret {
		return skippedSecret{Name: n, Scope: store.ScopeRuntimeBroker, Class: skipClassMissing}
	}
	cases := []struct {
		name    string
		skipped []skippedSecret
		verb    string
		owner   bool
		want    string
	}{
		{"none", nil, "started", true, ""},
		{"spec example, owner", []skippedSecret{user("DB_PASSWORD"), proj("API_KEY")}, "started", true,
			skippedWarnPrefix + "API_KEY (project), DB_PASSWORD (user); the agent was started without them"},
		{"spec example, non-owner", []skippedSecret{user("DB_PASSWORD"), proj("API_KEY")}, "started", false,
			skippedWarnPrefix + "API_KEY (project), 1 user-scope secret; the agent was started without them"},
		{"spec example with a hub item, owner", []skippedSecret{hub("HUB_TOKEN"), user("DB_PASSWORD"), proj("API_KEY")}, "started", true,
			skippedWarnPrefix + "API_KEY (project), DB_PASSWORD (user), 1 hub-scope secret; the agent was started without them"},
		{"restarted verb", []skippedSecret{proj("API_KEY")}, "restarted", false,
			skippedWarnPrefix + "API_KEY (project); the agent was restarted without them"},
		{"created verb", []skippedSecret{proj("API_KEY")}, "created", false,
			skippedWarnPrefix + "API_KEY (project); the agent was created without them"},
		{"sorted by name then scope, de-duplicated", []skippedSecret{proj("B"), user("A"), proj("A"), proj("B"), user("A")}, "started", true,
			skippedWarnPrefix + "A (project), A (user), B (project); the agent was started without them"},
		{"counts follow names in the order hub, broker, user; plural", []skippedSecret{
			user("U1"), broker("K1"), user("U2"), hub("H1"), proj("P"), broker("K2"), hub("H1"),
		}, "started", false,
			skippedWarnPrefix + "P (project), 1 hub-scope secret, 2 broker-scope secrets, 2 user-scope secrets; the agent was started without them"},
		{"hub and broker are counts for the owner too", []skippedSecret{hub("H1"), broker("K1")}, "started", true,
			skippedWarnPrefix + "1 hub-scope secret, 1 broker-scope secret; the agent was started without them"},
		{"only counts", []skippedSecret{user("U1")}, "started", false,
			skippedWarnPrefix + "1 user-scope secret; the agent was started without them"},
		{"unknown scope is not rendered in the response", []skippedSecret{{Name: "X", Scope: "elsewhere", Class: skipClassAccess}}, "started", true, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, formatSkippedSecretWarning(tc.skipped, tc.verb, tc.owner))
		})
	}
}

func TestSkippedSecretLogItems_AllNamesSorted(t *testing.T) {
	got := skippedSecretLogItems([]skippedSecret{
		{Name: "B", Scope: store.ScopeUser, Class: skipClassAccess},
		{Name: "A", Scope: store.ScopeRuntimeBroker, Class: skipClassMissing},
		{Name: "A", Scope: store.ScopeHub, Class: skipClassNotFound},
		{Name: "B", Scope: store.ScopeUser, Class: skipClassAccess},
		{Name: "C", Scope: "elsewhere", Class: skipClassAccess},
	})
	assert.Equal(t, []skippedSecretLogItem{
		{Name: "A", Scope: "broker", Class: skipClassMissing},
		{Name: "A", Scope: "hub", Class: skipClassNotFound},
		{Name: "B", Scope: "user", Class: skipClassAccess},
		{Name: "C", Scope: "unknown", Class: skipClassAccess},
	}, got, "an item of an unknown scope is logged as scope unknown, never dropped")
}

func TestDeliverySkipClass(t *testing.T) {
	assert.Equal(t, skipClassMissing, deliverySkipClass(secret.FetchResult{}, false))
	assert.Equal(t, skipClassNotFound, deliverySkipClass(secret.FetchResult{Err: store.ErrNotFound}, true))
	assert.Equal(t, skipClassAccess, deliverySkipClass(secret.FetchResult{Err: secret.ErrValueAccess}, true))
	assert.Equal(t, "", deliverySkipClass(secret.FetchResult{Value: "v"}, true))
	assert.Equal(t, "", deliverySkipClass(secret.FetchResult{Err: errors.New("other")}, true),
		"any other error is not a skip: it fails the selection")
}

func TestSkippedSecretAudienceIsOwner(t *testing.T) {
	agent := &store.Agent{ID: "a1", OwnerID: "owner-1"}
	row := func(kind, id string) context.Context {
		return withDispatchRowInitiator(context.Background(), &store.BrokerDispatch{InitiatorPrincipalKind: kind, InitiatorPrincipalID: id})
	}
	cases := []struct {
		name string
		ctx  context.Context
		want bool
	}{
		{"no principal", context.Background(), false},
		{"row: user owner", row(string(PrincipalKindUser), "owner-1"), true},
		{"row: user non-owner", row(string(PrincipalKindUser), "other"), false},
		{"row: dev owner", row(string(PrincipalKindDev), "owner-1"), true},
		{"row: dev non-owner", row(string(PrincipalKindDev), "other"), false},
		{"row: agent principal with the owner's ID", row("agent", "owner-1"), false},
		{"row: legacy row without attribution", row("", ""), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, skippedSecretAudienceIsOwner(tc.ctx, agent))
		})
	}
	// The request principal, on the node that sends the start.
	devOwned := &store.Agent{ID: "a3", OwnerID: DevUserID}
	asIdentity := func(id Identity) context.Context { return contextWithIdentity(context.Background(), id) }
	assert.True(t, skippedSecretAudienceIsOwner(asIdentity(NewAuthenticatedUser("owner-1", "o@x.test", "O", "member", "web")), agent),
		"request: the owner user")
	assert.False(t, skippedSecretAudienceIsOwner(asIdentity(NewAuthenticatedUser("other", "x@x.test", "X", "admin", "web")), agent),
		"request: another user, even an admin")
	assert.True(t, skippedSecretAudienceIsOwner(asIdentity(NewDevUser(DevUserConfig{Username: "dev"})), devOwned),
		"request: the dev-auth user that owns the agent")
	assert.False(t, skippedSecretAudienceIsOwner(asIdentity(NewDevUser(DevUserConfig{Username: "dev"})), agent),
		"request: the dev-auth user that does not own the agent")
	assert.False(t, skippedSecretAudienceIsOwner(
		withDispatchRowInitiator(asIdentity(NewAuthenticatedUser("owner-1", "o@x.test", "O", "member", "web")),
			&store.BrokerDispatch{InitiatorPrincipalKind: "agent", InitiatorPrincipalID: "owner-1"}), agent),
		"on an owner node the row's initiator decides, not a context identity")

	assert.False(t, skippedSecretAudienceIsOwner(row(string(PrincipalKindUser), ""), &store.Agent{ID: "a2"}),
		"an agent without an owner has no owner audience")
}
