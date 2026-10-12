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
	"io/fs"
	"testing"
	"testing/fstest"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/resources"
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

	err := srv.seedPlatformSkillInsertions(ctx, resources.PlatformSkillsFS())
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

	err := srv.seedPlatformSkillInsertions(ctx, resources.PlatformSkillsFS())
	require.NoError(t, err)

	hs1, err := s.GetHubSetting(ctx, "injected_skills")
	require.NoError(t, err)
	var first api.HubSkillInjectionSetting
	require.NoError(t, json.Unmarshal(hs1.Value, &first))
	firstCount := len(first.System)
	require.NotZero(t, firstCount)

	// Second call — must not fail.
	err = srv.seedPlatformSkillInsertions(ctx, resources.PlatformSkillsFS())
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
	err = srv.seedPlatformSkillInsertions(ctx, resources.PlatformSkillsFS())
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

	seedErr := srv.seedPlatformSkillInsertions(ctx, resources.PlatformSkillsFS())
	require.Error(t, seedErr, "seedPlatformSkillInsertions must return an error when UpsertHubSetting fails")
	assert.ErrorContains(t, seedErr, "upsert",
		"error message must mention the upsert step")
}

// errReadDirFS is an fs.FS whose root cannot be read.
type errReadDirFS struct{ err error }

func (e errReadDirFS) Open(name string) (fs.File, error) {
	return nil, &fs.PathError{Op: "open", Path: name, Err: e.err}
}

func readInjectedSkillsSetting(t *testing.T, s store.Store) api.HubSkillInjectionSetting {
	t.Helper()
	hs, err := s.GetHubSetting(context.Background(), "injected_skills")
	require.NoError(t, err)
	var setting api.HubSkillInjectionSetting
	require.NoError(t, json.Unmarshal(hs.Value, &setting))
	return setting
}

// TestSeedPlatformSkillInsertions_UsesProvidedFS verifies that the system list
// is derived from the supplied FS: one optional entry per top-level directory
// that contains a SKILL.md, in directory order, with other entries skipped.
func TestSeedPlatformSkillInsertions_UsesProvidedFS(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	skillsFS := fstest.MapFS{
		"alpha/SKILL.md":        {Data: []byte("# alpha")},
		"beta/SKILL.md":         {Data: []byte("# beta")},
		"beta/extra/notes.md":   {Data: []byte("notes")},
		"no-skill-md/README.md": {Data: []byte("not a skill")},
		"top-level-file.md":     {Data: []byte("ignored")},
		"nested/inner/SKILL.md": {Data: []byte("not at the skill root")},
	}

	require.NoError(t, srv.seedPlatformSkillInsertions(ctx, skillsFS))

	setting := readInjectedSkillsSetting(t, s)
	assert.Equal(t, []api.SkillReference{
		{URI: platformSkillURIPrefix + "alpha", Optional: true},
		{URI: platformSkillURIPrefix + "beta", Optional: true},
	}, setting.System)
}

// TestSeedPlatformSkillInsertions_EmptyFS verifies that an empty FS clears a
// previously seeded system list while leaving user_defined entries intact.
func TestSeedPlatformSkillInsertions_EmptyFS(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	prev := api.HubSkillInjectionSetting{
		System:      []api.SkillReference{{URI: platformSkillURIPrefix + "old", Optional: true}},
		UserDefined: []api.SkillReference{{URI: "skill://scion/global/keep-me"}},
	}
	raw, err := json.Marshal(prev)
	require.NoError(t, err)
	_, err = s.UpsertHubSetting(ctx, "injected_skills", raw, "seed", -1, "seeded")
	require.NoError(t, err)

	require.NoError(t, srv.seedPlatformSkillInsertions(ctx, fstest.MapFS{}))

	setting := readInjectedSkillsSetting(t, s)
	assert.Empty(t, setting.System, "an empty platform skills FS must yield an empty system list")
	assert.Equal(t, prev.UserDefined, setting.UserDefined)
}

// TestSeedPlatformSkillInsertions_FSReadError verifies that a failure to read
// the FS root is returned and leaves the stored setting untouched.
func TestSeedPlatformSkillInsertions_FSReadError(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	prev := api.HubSkillInjectionSetting{
		System:      []api.SkillReference{{URI: platformSkillURIPrefix + "existing", Optional: true}},
		UserDefined: []api.SkillReference{{URI: "skill://scion/global/keep-me"}},
	}
	raw, err := json.Marshal(prev)
	require.NoError(t, err)
	_, err = s.UpsertHubSetting(ctx, "injected_skills", raw, "seed", -1, "seeded")
	require.NoError(t, err)

	readErr := errors.New("read failed")
	seedErr := srv.seedPlatformSkillInsertions(ctx, errReadDirFS{err: readErr})
	require.Error(t, seedErr)
	assert.ErrorIs(t, seedErr, readErr)
	assert.ErrorContains(t, seedErr, "read platform skills FS")

	assert.Equal(t, prev, readInjectedSkillsSetting(t, s), "stored setting must be unchanged after a read error")
}

// TestSeedPlatformSkillInsertions_NilFS verifies that a nil FS is reported as
// an error instead of panicking.
func TestSeedPlatformSkillInsertions_NilFS(t *testing.T) {
	srv, _ := testServer(t)
	require.Error(t, srv.seedPlatformSkillInsertions(context.Background(), nil))
}
