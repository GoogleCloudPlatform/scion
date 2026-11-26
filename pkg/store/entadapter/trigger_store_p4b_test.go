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

package entadapter_test

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/store/entadapter"
	"github.com/GoogleCloudPlatform/scion/pkg/store/enttest"
)

func TestTriggerStore_WebhookEndpointRoutingHintsRoundTrip(t *testing.T) {
	ctx := context.Background()
	client := enttest.NewClient(t)
	t.Logf("dialect=%s", client.Driver().Dialect())
	cs := entadapter.NewCompositeStore(client)

	projID := uuid.NewString()
	require.NoError(t, cs.CreateProject(ctx, &store.Project{
		ID:   projID,
		Name: "rh-proj",
		Slug: "rh-" + projID[:8],
	}))

	ep := &store.WebhookEndpoint{
		ID:                  uuid.NewString(),
		ProjectID:           projID,
		Name:                "stripe-hooks",
		OwnerPrincipalKind:  store.DelegationPrincipalUser,
		OwnerPrincipalID:    uuid.NewString(),
		OwnerCredentialKind: store.InitiatorCredentialKindSession,
		SignatureMode:       store.WebhookSignatureModeStripeV1,
		RoutingHints: &store.WebhookRoutingHints{
			TypePath:    "/type",
			SubjectPath: "/data/object/customer",
			DedupPath:   "/id",
		},
	}
	require.NoError(t, cs.CreateWebhookEndpoint(ctx, ep))
	require.NotNil(t, ep.RoutingHints)
	assert.Equal(t, "/type", ep.RoutingHints.TypePath)
	assert.Equal(t, "/data/object/customer", ep.RoutingHints.SubjectPath)
	assert.Equal(t, "/id", ep.RoutingHints.DedupPath)

	loaded, err := cs.GetWebhookEndpoint(ctx, ep.ID)
	require.NoError(t, err)
	require.NotNil(t, loaded.RoutingHints)
	assert.Equal(t, *ep.RoutingHints, *loaded.RoutingHints)

	// Update routing hints and signature mode.
	loaded.SignatureMode = store.WebhookSignatureModeGitHubV1
	loaded.RoutingHints = &store.WebhookRoutingHints{SubjectPath: "/repository/full_name"}
	require.NoError(t, cs.UpdateWebhookEndpoint(ctx, loaded, store.WebhookEndpointFieldMask{
		SignatureMode: true,
		RoutingHints:  true,
	}))
	assert.Equal(t, store.WebhookSignatureModeGitHubV1, loaded.SignatureMode)
	require.NotNil(t, loaded.RoutingHints)
	assert.Equal(t, "", loaded.RoutingHints.TypePath)
	assert.Equal(t, "/repository/full_name", loaded.RoutingHints.SubjectPath)

	// Clear routing hints completely.
	loaded.RoutingHints = nil
	require.NoError(t, cs.UpdateWebhookEndpoint(ctx, loaded, store.WebhookEndpointFieldMask{
		RoutingHints: true,
	}))
	assert.Nil(t, loaded.RoutingHints)
}
