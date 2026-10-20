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
	"bytes"
	"context"
	"log"
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

const (
	detectedHostEmail = "detected@proj.iam.gserviceaccount.com"
	updateHostEmail   = "configured@proj.iam.gserviceaccount.com"
)

// stubHostDetection replaces the metadata server detection for the test.
func stubHostDetection(t *testing.T, d hostDetection) {
	t.Helper()
	orig := detectHostGCPIdentity
	t.Cleanup(func() { detectHostGCPIdentity = orig })
	detectHostGCPIdentity = func(context.Context) hostDetection { return d }
}

func captureBrokerStdLog(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	log.SetOutput(&buf)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })
	return &buf
}

func TestLegacyEmbedded_DetectionRespectsUpdateValue(t *testing.T) {
	ctx := context.Background()

	t.Run("refreshes a value not set through the update API", func(t *testing.T) {
		for _, source := range []string{"", store.GCPHostSourceDetected, store.GCPHostSourceRegistration} {
			s := newTestStore(t)
			b := &store.RuntimeBroker{ID: tid("detect-refresh"), Name: "detect-refresh", Slug: "detect-refresh",
				GCPHostServiceAccountEmail: "old@proj.iam.gserviceaccount.com", GCPHostProjectID: "proj", GCPHostSource: source}
			require.NoError(t, s.CreateRuntimeBroker(ctx, b))

			require.NoError(t, refreshDetectedGCPHost(ctx, s, b, detectedHostEmail, "proj"))

			got, err := s.GetRuntimeBroker(ctx, b.ID)
			require.NoError(t, err)
			assert.Equal(t, detectedHostEmail, got.GCPHostServiceAccountEmail, "source %q", source)
			assert.Equal(t, store.GCPHostSourceDetected, got.GCPHostSource)
		}
	})

	t.Run("keeps a value set through the update API and logs a warning", func(t *testing.T) {
		s := newTestStore(t)
		b := &store.RuntimeBroker{ID: tid("detect-keep"), Name: "detect-keep", Slug: "detect-keep"}
		require.NoError(t, s.CreateRuntimeBroker(ctx, b))
		_, err := s.SetRuntimeBrokerGCPHost(ctx, b.ID, updateHostEmail, "proj")
		require.NoError(t, err)
		b, err = s.GetRuntimeBroker(ctx, b.ID)
		require.NoError(t, err)
		logs := captureBrokerStdLog(t)

		require.NoError(t, refreshDetectedGCPHost(ctx, s, b, detectedHostEmail, "proj"))

		got, err := s.GetRuntimeBroker(ctx, b.ID)
		require.NoError(t, err)
		assert.Equal(t, updateHostEmail, got.GCPHostServiceAccountEmail)
		assert.Equal(t, store.GCPHostSourceAdmin, got.GCPHostSource)
		assert.Contains(t, logs.String(), updateHostEmail)
		assert.Contains(t, logs.String(), detectedHostEmail)
	})

	t.Run("nothing detected leaves the stored pair", func(t *testing.T) {
		s := newTestStore(t)
		b := &store.RuntimeBroker{ID: tid("detect-none"), Name: "detect-none", Slug: "detect-none",
			GCPHostServiceAccountEmail: "old@proj.iam.gserviceaccount.com", GCPHostProjectID: "proj", GCPHostSource: store.GCPHostSourceDetected}
		require.NoError(t, s.CreateRuntimeBroker(ctx, b))

		require.NoError(t, refreshDetectedGCPHost(ctx, s, b, "", ""))

		got, err := s.GetRuntimeBroker(ctx, b.ID)
		require.NoError(t, err)
		assert.Equal(t, "old@proj.iam.gserviceaccount.com", got.GCPHostServiceAccountEmail)
	})
}

// TestLegacyEmbedded_ReregistrationLeavesUpdateValue: a co-located restart
// keeps the GCP host pair set through the update API.
func TestLegacyEmbedded_ReregistrationLeavesUpdateValue(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	settings := &config.Settings{}
	// The host reports a different account than the one set through the
	// update API.
	stubHostDetection(t, hostDetection{Email: detectedHostEmail, ProjectID: "proj"})

	_, err := registerGlobalProjectAndBroker(ctx, s, tid("embedded-keeps"), "embedded-keeps", "http://localhost:9800", nil, false, settings, nil, nil)
	require.NoError(t, err)
	_, err = s.SetRuntimeBrokerGCPHost(ctx, tid("embedded-keeps"), updateHostEmail, "proj")
	require.NoError(t, err)

	_, err = registerGlobalProjectAndBroker(ctx, s, tid("embedded-keeps"), "embedded-keeps", "http://localhost:9800", nil, false, settings, nil, nil)
	require.NoError(t, err)

	got, err := s.GetRuntimeBroker(ctx, tid("embedded-keeps"))
	require.NoError(t, err)
	assert.Equal(t, updateHostEmail, got.GCPHostServiceAccountEmail)
	assert.Equal(t, "proj", got.GCPHostProjectID)
}

func TestLegacyEmbedded_AdoptsOnlyHubManagedRowByName(t *testing.T) {
	ctx := context.Background()
	settings := &config.Settings{}
	stubHostDetection(t, hostDetection{})

	t.Run("owned row is not adopted", func(t *testing.T) {
		s := newTestStore(t)
		owned := &store.RuntimeBroker{ID: tid("owned-by-user"), Name: "shared-name", Slug: "shared-name", CreatedBy: "user-1",
			Labels: map[string]string{store.BrokerRoleLabel: store.BrokerRoleEmbedded}}
		require.NoError(t, s.CreateRuntimeBroker(ctx, owned))

		effectiveID, err := registerGlobalProjectAndBroker(ctx, s, tid("colocated-new"), "shared-name", "http://localhost:9800", nil, false, settings, nil, nil)
		require.NoError(t, err)
		assert.Equal(t, tid("colocated-new"), effectiveID, "a row with a recorded owner is not adopted by name")

		got, err := s.GetRuntimeBroker(ctx, owned.ID)
		require.NoError(t, err)
		assert.Equal(t, "user-1", got.CreatedBy)
		assert.Equal(t, store.BrokerStatusOffline, got.Status, "the owned row is not updated")
	})

	t.Run("hub-managed row is adopted", func(t *testing.T) {
		s := newTestStore(t)
		_, err := registerGlobalProjectAndBroker(ctx, s, tid("colocated-old"), "shared-name", "http://localhost:9800", nil, false, settings, nil, nil)
		require.NoError(t, err)

		effectiveID, err := registerGlobalProjectAndBroker(ctx, s, tid("colocated-lost"), "shared-name", "http://localhost:9800", nil, false, settings, nil, nil)
		require.NoError(t, err)
		assert.Equal(t, tid("colocated-old"), effectiveID)
	})
}
