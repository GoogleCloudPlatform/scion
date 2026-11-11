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
	"path/filepath"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/require"
)

// fixtureProviderCheckout returns the local checkout path for a remote test
// broker's provider row. Fixtures that dispatch a hub-managed project to a
// remote (non-embedded) broker give that provider row this path: a remote
// broker serves such a project from its own checkout, and one without a
// path is refused at agent create (ptone/scion#5813). Tests of that refusal
// leave the path empty on purpose.
func fixtureProviderCheckout(t testing.TB) string {
	t.Helper()
	return filepath.Join(t.TempDir(), "checkout")
}

// giveProviderCheckout sets fixtureProviderCheckout on the existing provider
// row of brokerID for projectID, for a test whose shared fixture registers a
// remote broker without one but whose agent create must be accepted.
func giveProviderCheckout(t testing.TB, s store.Store, projectID, brokerID string) {
	t.Helper()
	ctx := context.Background()
	provider, err := s.GetProjectProvider(ctx, projectID, brokerID)
	require.NoError(t, err)
	provider.LocalPath = fixtureProviderCheckout(t)
	require.NoError(t, s.AddProjectProvider(ctx, provider))
}
