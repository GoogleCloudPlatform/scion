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
	"os"
	"strings"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/conduit/registry"
	"github.com/GoogleCloudPlatform/scion/pkg/store/entadapter"
	"github.com/GoogleCloudPlatform/scion/pkg/store/enttest"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestConduitInstanceID(t *testing.T) {
	host, err := os.Hostname()
	require.NoError(t, err)
	tests := []struct {
		name, configured, podName string
		want                      string // exact; "" = host name plus a random suffix
	}{
		{name: "configured wins", configured: "hub-east-1", podName: "hub-0", want: "hub-east-1"},
		{name: "pod name", podName: "hub-0", want: "hub-0"},
		{name: "host name with a random suffix"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("POD_NAME", tt.podName)
			got := ConduitInstanceID(tt.configured)
			if tt.want != "" {
				assert.Equal(t, tt.want, got)
				return
			}
			assert.True(t, strings.HasPrefix(got, host+"-"), "got %q", got)
			assert.Len(t, got, len(host)+1+12)
			assert.NotEqual(t, got, ConduitInstanceID(tt.configured), "each process gets its own id")
		})
	}
}

// TestConduitInstanceID_SameHostNoSupersede: two processes on the same host
// name register distinct relay ids, so neither supersedes the other; a
// shared id would (the second registration bumps the generation).
func TestConduitInstanceID_SameHostNoSupersede(t *testing.T) {
	t.Setenv("POD_NAME", "")
	ctx := context.Background()
	reg := registry.New(entadapter.NewConduitRegistryStore(enttest.NewClient(t)), registry.Config{})
	register := func(id string) int64 {
		t.Helper()
		now := time.Now()
		gen, err := reg.RegisterRelay(ctx, registry.RelayInstance{InstanceID: id, InternalEndpoint: "http://10.0.0.5:9810", StartedAt: now, LastSeen: now})
		require.NoError(t, err)
		return gen
	}

	idA, idB := ConduitInstanceID(""), ConduitInstanceID("")
	require.NotEqual(t, idA, idB)
	genA, genB := register(idA), register(idB)
	assert.NoError(t, reg.HeartbeatRelay(ctx, idA, genA), "process A is not superseded")
	assert.NoError(t, reg.HeartbeatRelay(ctx, idB, genB), "process B is not superseded")

	// The hazard the per-process suffix avoids.
	shared := "same-host"
	genOld := register(shared)
	register(shared)
	assert.ErrorIs(t, reg.HeartbeatRelay(ctx, shared, genOld), registry.ErrRelaySuperseded)
}
