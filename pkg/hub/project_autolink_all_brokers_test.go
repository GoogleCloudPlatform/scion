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
	"fmt"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Project creation links every eligible auto-provide broker, not only the
// first page the broker list returns (ptone/scion#1998). The count is more
// than the store's default page of 50 and more than one auto-link page.
func TestCreateProject_AutoLinksEveryAutoProvideBroker(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	const n = autoProvideBrokersPageSize + 20
	require.Greater(t, n, 50)
	ids := make([]string, 0, n)
	for i := 0; i < n; i++ {
		name := fmt.Sprintf("autolink-broker-%03d", i)
		b := &store.RuntimeBroker{
			ID:          tid(name),
			Name:        name,
			Slug:        name,
			Status:      store.BrokerStatusOnline,
			AutoProvide: true,
		}
		require.NoError(t, s.CreateRuntimeBroker(ctx, b))
		ids = append(ids, b.ID)
	}
	notAuto := &store.RuntimeBroker{
		ID:     tid("autolink-broker-manual"),
		Name:   "autolink-broker-manual",
		Slug:   "autolink-broker-manual",
		Status: store.BrokerStatusOnline,
	}
	require.NoError(t, s.CreateRuntimeBroker(ctx, notAuto))

	project, _ := createTestHubManagedProject(t, srv, "Auto Link Every Broker")

	missing := 0
	for _, id := range ids {
		p, err := s.GetProjectProvider(ctx, project.ID, id)
		if err != nil {
			missing++
			continue
		}
		assert.Equal(t, autoProvideLinkedBy, p.LinkedBy)
	}
	assert.Zero(t, missing, "every auto-provide broker must be linked to the new project")

	_, err := s.GetProjectProvider(ctx, project.ID, notAuto.ID)
	assert.ErrorIs(t, err, store.ErrNotFound, "a broker without auto-provide is not linked")

	got, err := s.GetProject(ctx, project.ID)
	require.NoError(t, err)
	assert.Contains(t, ids, got.DefaultRuntimeBrokerID, "an auto-provide broker becomes the default")
}
