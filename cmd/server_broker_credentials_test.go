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

package cmd

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// The co-located broker row, re-created with its fixed ID (for example
// after it was deleted and the hub's startup sweep did not run or failed),
// does not inherit a secret or join token left for that ID.
func TestRegisterColocatedBroker_RecreatedRowClearsLeftCredentials(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	id := tid("colocated-recreate-broker")
	const key = "colocated-left-secret-32-bytes!!"
	require.NoError(t, s.CreateBrokerSecret(ctx, &store.BrokerSecret{
		BrokerID: id, SecretKey: []byte(key),
		Algorithm: store.BrokerSecretAlgorithmHMACSHA256, Status: store.BrokerSecretStatusActive,
	}))
	require.NoError(t, s.CreateJoinToken(ctx, &store.BrokerJoinToken{
		BrokerID: id, TokenHash: "colocated-left-token-hash", ExpiresAt: time.Now().Add(time.Hour), CreatedBy: "dev",
	}))

	notPresent := func(context.Context, string) (bool, error) { return false, nil }
	got, err := registerGlobalProjectAndBrokerPresence(ctx, s, notPresent, id, "colocated-recreate", "http://localhost:9800", nil, false, &config.Settings{}, nil, nil)
	require.NoError(t, err)
	require.Equal(t, id, got)

	_, err = s.GetRuntimeBroker(ctx, id)
	require.NoError(t, err, "the co-located row is created")
	_, err = s.GetBrokerSecret(ctx, id)
	assert.ErrorIs(t, err, store.ErrNotFound, "the left secret is cleared with the create")
	_, err = s.GetJoinTokenByBrokerID(ctx, id)
	assert.ErrorIs(t, err, store.ErrNotFound, "the left join token is cleared with the create")
}
