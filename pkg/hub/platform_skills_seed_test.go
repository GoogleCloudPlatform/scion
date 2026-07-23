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
	"encoding/json"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// errUpsertStore wraps a store.Store and returns a fixed error for every
// UpsertHubSetting call.  Used to verify seedPlatformSkillInsertions propagates
// the error rather than panicking or silently succeeding.
type errUpsertStore struct {
	store.Store
	upsertErr error
}

func (e *errUpsertStore) UpsertHubSetting(ctx context.Context, section string, value json.RawMessage,
	updatedBy string, expectedRevision int64, origin string) (*store.HubSetting, error) {
	return nil, e.upsertErr
}

// TestSeedPlatformSkillInsertions_SetsSystemEntries verifies that calling
// seedPlatformSkillInsertions writes at least one system entry to
// hub_settings["injected_skills"] and that every entry has Optional=true.
func TestSeedPlatformSkillInsertions_SetsSystemEntries(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	err := srv.seedPlatformSkillInsertions(ctx)
	require.NoError(t, err)

	hs, err := s.GetHubSetting(ctx, "injected_skills")
	require.NoError(t, err, "hub_settings[injected_skills] must exist after seeding")

	var setting api.HubSkillInjectionSetting
	require.NoError(t, json.Unmarshal(hs.Value, &setting))

	assert.NotEmpty(t, setting.System, "system list must be non-empty after seeding")
	for _, ref := range setting.System {
		assert.True(t, ref.Optional,
			"every system entry must have Optional=true; got Optional=false for %q", ref.URI)
		assert.NotEmpty(t, ref.URI,
			"every system entry must have a non-empty URI")
		assert.Contains(t, ref.URI, platformSkillURIPrefix,
			"system entry URI %q must use the platform skill prefix", ref.URI)
	}
}

// TestSeedPlatformSkillInsertions_Idempotent verifies that calling
// seedPlatformSkillInsertions twice neither errors nor corrupts the stored value.
func TestSeedPlatformSkillInsertions_Idempotent(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	err := srv.seedPlatformSkillInsertions(ctx)
	require.NoError(t, err)

	hs1, err := s.GetHubSetting(ctx, "injected_skills")
	require.NoError(t, err)
	var first api.HubSkillInjectionSetting
	require.NoError(t, json.Unmarshal(hs1.Value, &first))
	firstCount := len(first.System)
	require.NotZero(t, firstCount)

	// Second call — must not fail.
	err = srv.seedPlatformSkillInsertions(ctx)
	require.NoError(t, err)

	hs2, err := s.GetHubSetting(ctx, "injected_skills")
	require.NoError(t, err)
	var second api.HubSkillInjectionSetting
	require.NoError(t, json.Unmarshal(hs2.Value, &second))

	assert.Equal(t, firstCount, len(second.System),
		"system entry count must be stable across multiple seed calls")
}

// TestSeedPlatformSkillInsertions_PreservesUserDefined verifies that an
// existing user_defined list in hub_settings["injected_skills"] is preserved
// after seeding — i.e. seeding never overwrites admin-managed skills.
func TestSeedPlatformSkillInsertions_PreservesUserDefined(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	// Pre-populate user_defined entries before the first seed.
	preExisting := api.HubSkillInjectionSetting{
		System: []api.SkillReference{},
		UserDefined: []api.SkillReference{
			{URI: "skill://scion/global/my-custom-skill", Optional: false},
			{URI: "skill://scion/global/another-skill", Optional: true},
		},
	}
	raw, err := json.Marshal(preExisting)
	require.NoError(t, err)
	_, err = s.UpsertHubSetting(ctx, "injected_skills", raw, "admin@example.com", -1, "managed")
	require.NoError(t, err)

	// Now run the seed — must populate system entries without touching user_defined.
	err = srv.seedPlatformSkillInsertions(ctx)
	require.NoError(t, err)

	hs, err := s.GetHubSetting(ctx, "injected_skills")
	require.NoError(t, err)
	var setting api.HubSkillInjectionSetting
	require.NoError(t, json.Unmarshal(hs.Value, &setting))

	// System entries must now be populated.
	assert.NotEmpty(t, setting.System, "seeding must populate system entries")

	// UserDefined must be unchanged.
	require.Len(t, setting.UserDefined, 2, "user_defined must be preserved after seeding")
	assert.Equal(t, "skill://scion/global/my-custom-skill", setting.UserDefined[0].URI)
	assert.Equal(t, "skill://scion/global/another-skill", setting.UserDefined[1].URI)
}

// TestSeedPlatformSkillInsertions_UpsertError verifies that when UpsertHubSetting
// returns an error, seedPlatformSkillInsertions propagates the error rather than
// panicking or silently succeeding.
func TestSeedPlatformSkillInsertions_UpsertError(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	// Pre-seed a setting with a stale system entry that does not match what the
	// embedded FS will produce.  This ensures the L2 equality check does not
	// short-circuit before reaching UpsertHubSetting.
	stale := api.HubSkillInjectionSetting{
		System: []api.SkillReference{{URI: "scion-platform://old-skill-not-in-binary"}},
	}
	raw, err := json.Marshal(stale)
	require.NoError(t, err)
	_, err = s.UpsertHubSetting(ctx, "injected_skills", raw, "seed", -1, "seeded")
	require.NoError(t, err)

	// Wrap the store so UpsertHubSetting always fails.
	wantErr := errors.New("store unavailable")
	srv.store = &errUpsertStore{Store: s, upsertErr: wantErr}

	seedErr := srv.seedPlatformSkillInsertions(ctx)
	require.Error(t, seedErr, "seedPlatformSkillInsertions must return an error when UpsertHubSetting fails")
	assert.ErrorContains(t, seedErr, "upsert",
		"error message must mention the upsert step")
}
