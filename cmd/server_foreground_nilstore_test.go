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

package cmd

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/hub"
)

// Fails if the nil-store guard at the top of initHubServer is removed: a
// serving hub must not start without a store, because the auth
// middleware's per-request user-row checks (account status and session
// generation) are skipped when it has none.
func TestInitHubServer_RefusesNilStore(t *testing.T) {
	srv, err := initHubServer(context.Background(), &config.GlobalConfig{}, nil, nil, hub.HubEndpoints{},
		"", nil, false, "", nil, nil, nil, t.TempDir(), nil, nil)
	require.Error(t, err)
	assert.Nil(t, srv)
	assert.Contains(t, err.Error(), "no store configured")
}
