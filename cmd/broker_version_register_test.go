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

	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/hub"
	"github.com/GoogleCloudPlatform/scion/pkg/runtime"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The default (flag-off) co-located registration writes the hub binary's
// build token on create and on every re-registration, rewriting an old
// 0.1.0 placeholder (ptone/scion#5972).
func TestRegisterGlobalProjectAndBroker_WritesHubBuildToken(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	settings := &config.Settings{}
	rt := &runtime.MockRuntime{NameFunc: func() string { return "docker" }}
	brokerID := tid("broker-version-register")

	_, err := registerGlobalProjectAndBroker(ctx, s, brokerID, "version-register-broker", "http://localhost:9800", rt, true, settings, nil, nil)
	require.NoError(t, err)
	b, err := s.GetRuntimeBroker(ctx, brokerID)
	require.NoError(t, err)
	assert.Equal(t, hub.HubBuildToken(), b.Version, "create writes the hub's build token")

	b.Version = "0.1.0"
	require.NoError(t, s.UpdateRuntimeBroker(ctx, b))
	_, err = registerGlobalProjectAndBroker(ctx, s, brokerID, "version-register-broker", "http://localhost:9800", rt, true, settings, nil, nil)
	require.NoError(t, err)
	b, err = s.GetRuntimeBroker(ctx, brokerID)
	require.NoError(t, err)
	assert.Equal(t, hub.HubBuildToken(), b.Version, "re-registration rewrites the placeholder")
}
