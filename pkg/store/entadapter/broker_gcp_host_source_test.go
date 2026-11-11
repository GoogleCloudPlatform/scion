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

package entadapter

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/store/enttest"
	"github.com/GoogleCloudPlatform/scion/pkg/store/storetest"
)

func TestRuntimeBrokerGCPHostWriters_Conformance(t *testing.T) {
	storetest.RuntimeBrokerGCPHostWritersConformance(t, compositeTestFactory)
}

// TestRuntimeBrokerGCPHostWriters_NullAndEmpty checks that the conditional
// writers treat SQL NULL and "" alike for the email, project and source.
func TestRuntimeBrokerGCPHostWriters_NullAndEmpty(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name string
		prep func(t *testing.T, ps *ProjectStore, id uuid.UUID)
	}{
		{"null columns", func(t *testing.T, ps *ProjectStore, id uuid.UUID) {
			_, err := ps.client.RuntimeBroker.UpdateOneID(id).
				ClearGcpHostServiceAccountEmail().ClearGcpHostProjectID().ClearGcpHostSource().Save(ctx)
			require.NoError(t, err)
		}},
		{"empty strings", func(t *testing.T, ps *ProjectStore, id uuid.UUID) {
			_, err := ps.client.RuntimeBroker.UpdateOneID(id).
				SetGcpHostServiceAccountEmail("").SetGcpHostProjectID("").SetGcpHostSource("").Save(ctx)
			require.NoError(t, err)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ps := newTestProjectStore(t)
			b := newBroker()
			require.NoError(t, ps.CreateRuntimeBroker(ctx, b))
			tc.prep(t, ps, uuid.MustParse(b.ID))

			applied, err := ps.SetRuntimeBrokerGCPHostIfUnset(ctx, b.ID, "sa@p.iam.gserviceaccount.com", "p", store.GCPHostSourceRegistration)
			require.NoError(t, err)
			assert.True(t, applied, "fill when unset")

			b2 := newBroker()
			require.NoError(t, ps.CreateRuntimeBroker(ctx, b2))
			tc.prep(t, ps, uuid.MustParse(b2.ID))
			applied, err = ps.SetRuntimeBrokerGCPHostDetected(ctx, b2.ID, "sa@p.iam.gserviceaccount.com", "p")
			require.NoError(t, err)
			assert.True(t, applied, "detected applies without an update-API source")
		})
	}
}

// TestBackfillRuntimeBrokerGCPHostSource checks the one-time source
// backfill for rows written before the column existed.
func TestBackfillRuntimeBrokerGCPHostSource(t *testing.T) {
	ctx := context.Background()
	cs := NewCompositeStore(enttest.NewClient(t))
	t.Cleanup(func() { _ = cs.Close() })

	mk := func(name, email, createdBy string, labels map[string]string, flat bool) string {
		id := uuid.NewString()
		b := &store.RuntimeBroker{
			ID: id, Name: name, Slug: name, Labels: labels, CreatedBy: createdBy,
			GCPHostServiceAccountEmail: email,
		}
		if email != "" {
			b.GCPHostProjectID = "p"
		}
		if flat {
			b.RuntimeTarget = &api.RuntimeTargetDescriptor{ID: "t-" + id[:8], Type: "docker", DisplayName: "t"}
		}
		require.NoError(t, cs.CreateRuntimeBroker(ctx, b))
		// Rows written before the column existed carry no source.
		_, err := cs.client.RuntimeBroker.UpdateOneID(uuid.MustParse(id)).ClearGcpHostSource().Save(ctx)
		require.NoError(t, err)
		return id
	}
	embedded := map[string]string{store.BrokerRoleLabel: store.BrokerRoleEmbedded}
	colocated := mk("colocated", "host@p.iam.gserviceaccount.com", "", embedded, false)
	ownedEmbedded := mk("owned-embedded", "host@p.iam.gserviceaccount.com", "user-1", embedded, false)
	remote := mk("remote", "remote@p.iam.gserviceaccount.com", "user-1", nil, false)
	flat := mk("flat", "flat@p.iam.gserviceaccount.com", "", embedded, true)
	unset := mk("unset", "", "user-1", nil, false)

	require.NoError(t, cs.BackfillRuntimeBrokerGCPHostSource(ctx))

	want := map[string]string{
		colocated:     store.GCPHostSourceDetected,
		ownedEmbedded: store.GCPHostSourceAdmin,
		remote:        store.GCPHostSourceAdmin,
		flat:          store.GCPHostSourceAdmin,
		unset:         "",
	}
	for id, source := range want {
		got, err := cs.GetRuntimeBroker(ctx, id)
		require.NoError(t, err)
		assert.Equal(t, source, got.GCPHostSource, got.Name)
	}

	// Once only: a source released through the update API stays released.
	_, err := cs.SetRuntimeBrokerGCPHostSource(ctx, remote, "")
	require.NoError(t, err)
	require.NoError(t, cs.BackfillRuntimeBrokerGCPHostSource(ctx))
	got, err := cs.GetRuntimeBroker(ctx, remote)
	require.NoError(t, err)
	assert.Empty(t, got.GCPHostSource)
}
