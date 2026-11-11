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
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/ent/runtimebroker"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// TestDeleteRuntimeBroker_FreesRuntimeTargetID: a flat Runtime Broker's
// runtime target is columns on its own row, so deleting the row (a never-
// joined registration included) leaves no target behind and frees the
// target ID for another Runtime Broker. While the row is live, the unique
// index refuses a second row with the same target.
func TestDeleteRuntimeBroker_FreesRuntimeTargetID(t *testing.T) {
	ctx := context.Background()
	fs := newFlatStores(t)
	target := dockerTarget()

	b1 := fs.createBroker(t, flatBroker("flat-one", store.BrokerStatusOffline, target))
	require.NoError(t, fs.client.BrokerTargetInventory.Create().
		SetBrokerID(b1.ID).SetTarget(target.ID).SetLastCompleteInventoryAt(time.Now()).
		Exec(ctx))

	b2 := flatBroker("flat-two", store.BrokerStatusOffline, target)
	err := fs.brokers.CreateRuntimeBroker(ctx, b2)
	require.ErrorIs(t, err, store.ErrAlreadyExists, "the target ID is unique while the first row is live")
	holder, err := store.RuntimeTargetHolder(ctx, fs.brokers, target.ID, b2.ID)
	require.NoError(t, err)
	require.NotNil(t, holder)
	assert.Equal(t, b1.ID, holder.ID)

	require.NoError(t, fs.brokers.DeleteRuntimeBroker(ctx, b1.ID))

	_, err = fs.brokers.GetRuntimeBroker(ctx, b1.ID)
	assert.ErrorIs(t, err, store.ErrNotFound)
	n, err := fs.client.RuntimeBroker.Query().Where(runtimebroker.RuntimeTargetIDEQ(target.ID)).Count(ctx)
	require.NoError(t, err)
	assert.Zero(t, n, "no row keeps the target ID")
	inv, err := fs.agents.ListBrokerTargetInventory(ctx, b1.ID)
	require.NoError(t, err)
	assert.Empty(t, inv, "the deleted broker's target inventory rows are gone")
	holder, err = store.RuntimeTargetHolder(ctx, fs.brokers, target.ID, "")
	require.NoError(t, err)
	assert.Nil(t, holder)

	b2.ID = uuid.NewString()
	require.NoError(t, fs.brokers.CreateRuntimeBroker(ctx, b2), "the freed target ID can be registered again")
	got, err := fs.brokers.GetRuntimeBroker(ctx, b2.ID)
	require.NoError(t, err)
	require.True(t, got.IsFlat())
	assert.Equal(t, target.ID, got.RuntimeTarget.ID)
}
