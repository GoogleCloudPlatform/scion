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
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/store/enttest"
)

func newTestSystemOneInstanceStore(t *testing.T) *SystemOneInstanceStore {
	t.Helper()
	return NewSystemOneInstanceStore(enttest.NewClient(t))
}

func sampleSystemOneInstance(name string) *store.SystemOneInstance {
	return &store.SystemOneInstance{
		Name:     name,
		Adapter:  "typesafe",
		Model:    "",
		Endpoint: "https://provider.example.test/v1",
		Location: "us-central1",
		Auth: store.SystemOneInstanceAuth{
			Kind:       store.SystemOneAuthKindAPIKey,
			SecretName: "TYPESAFE_API_KEY_REF",
			HeaderName: "X-API-Key",
		},
		Options: map[string]string{
			"profile": "strict",
		},
		Limits: store.SystemOneInstanceLimits{
			MaxInFlight:    8,
			RatePerSec:     25.5,
			MaxInputTokens: 4096,
			CallTimeout:    4 * time.Second,
		},
		AllowPrivateNetwork: false,
		Enabled:             true,
		CreatedBy:           "user-admin-1",
		UpdatedBy:           "user-admin-1",
	}
}

func TestSystemOneInstanceStore_CreateAndGet(t *testing.T) {
	s := newTestSystemOneInstanceStore(t)
	ctx := context.Background()

	inst := sampleSystemOneInstance("typesafe-prod")
	require.NoError(t, s.CreateSystemOneInstance(ctx, inst))

	require.NotEmpty(t, inst.ID)
	_, err := uuid.Parse(inst.ID)
	require.NoError(t, err)
	assert.Equal(t, int64(1), inst.Revision)
	assert.False(t, inst.CreatedAt.IsZero())
	assert.False(t, inst.UpdatedAt.IsZero())
	assert.Equal(t, time.UTC, inst.CreatedAt.Location())
	assert.Equal(t, time.UTC, inst.UpdatedAt.Location())

	// Mutating caller map after Create must not affect persisted state.
	inst.Options["profile"] = "mutated-locally"

	byName, err := s.GetSystemOneInstanceByName(ctx, "typesafe-prod")
	require.NoError(t, err)
	assert.Equal(t, inst.ID, byName.ID)
	assert.Equal(t, "typesafe-prod", byName.Name)
	assert.Equal(t, "typesafe", byName.Adapter)
	assert.Equal(t, "https://provider.example.test/v1", byName.Endpoint)
	assert.Equal(t, "us-central1", byName.Location)
	assert.Equal(t, store.SystemOneAuthKindAPIKey, byName.Auth.Kind)
	assert.Equal(t, "TYPESAFE_API_KEY_REF", byName.Auth.SecretName)
	assert.Equal(t, "X-API-Key", byName.Auth.HeaderName)
	assert.Equal(t, map[string]string{"profile": "strict"}, byName.Options)
	assert.Equal(t, 8, byName.Limits.MaxInFlight)
	assert.InDelta(t, 25.5, byName.Limits.RatePerSec, 1e-9)
	assert.Equal(t, 4096, byName.Limits.MaxInputTokens)
	assert.Equal(t, 4*time.Second, byName.Limits.CallTimeout)
	assert.False(t, byName.AllowPrivateNetwork)
	assert.True(t, byName.Enabled)
	assert.Equal(t, int64(1), byName.Revision)
	assert.Equal(t, "user-admin-1", byName.CreatedBy)
	assert.Equal(t, "user-admin-1", byName.UpdatedBy)

	byID, err := s.GetSystemOneInstance(ctx, inst.ID)
	require.NoError(t, err)
	assert.Equal(t, byName, byID)

	// Missing lookups return ErrNotFound.
	_, err = s.GetSystemOneInstanceByName(ctx, "does-not-exist")
	assert.ErrorIs(t, err, store.ErrNotFound)
	_, err = s.GetSystemOneInstance(ctx, uuid.New().String())
	assert.ErrorIs(t, err, store.ErrNotFound)
	_, err = s.GetSystemOneInstance(ctx, "not-a-uuid")
	assert.ErrorIs(t, err, store.ErrNotFound)
}

func TestSystemOneInstanceStore_AllAuthKindsRoundTrip(t *testing.T) {
	s := newTestSystemOneInstanceStore(t)
	ctx := context.Background()

	cases := []struct {
		slug string
		auth store.SystemOneInstanceAuth
	}{
		{
			slug: "auth-none",
			auth: store.SystemOneInstanceAuth{Kind: store.SystemOneAuthKindNone},
		},
		{
			slug: "auth-apikey",
			auth: store.SystemOneInstanceAuth{
				Kind:       store.SystemOneAuthKindAPIKey,
				SecretName: "HUB_SECRET_NAME",
				HeaderName: "Authorization",
			},
		},
		{
			slug: "auth-gcp-access",
			auth: store.SystemOneInstanceAuth{
				Kind:       store.SystemOneAuthKindGoogleAccessToken,
				SecretName: "gsa-id-123",
				Scopes:     []string{"https://www.googleapis.com/auth/cloud-platform"},
			},
		},
		{
			slug: "auth-gcp-id",
			auth: store.SystemOneInstanceAuth{
				Kind:       store.SystemOneAuthKindGoogleIDToken,
				SecretName: "gsa-id-456",
				Audience:   "https://guardrail.internal.example",
			},
		},
		{
			slug: "auth-azure",
			auth: store.SystemOneInstanceAuth{
				Kind:       store.SystemOneAuthKindAzureEntra,
				SecretName: "ENTRA_CLIENT_SECRET_NAME",
				Audience:   "api://guardrail-app",
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.slug, func(t *testing.T) {
			inst := sampleSystemOneInstance(tc.slug)
			inst.Auth = tc.auth
			require.NoError(t, s.CreateSystemOneInstance(ctx, inst))

			got, err := s.GetSystemOneInstanceByName(ctx, tc.slug)
			require.NoError(t, err)
			assert.Equal(t, tc.auth, got.Auth)
		})
	}
}

func TestSystemOneInstanceStore_UniquenessAndValidation(t *testing.T) {
	s := newTestSystemOneInstanceStore(t)
	ctx := context.Background()

	first := sampleSystemOneInstance("typesafe-prod")
	require.NoError(t, s.CreateSystemOneInstance(ctx, first))

	// Duplicate Name -> ErrAlreadyExists.
	dupName := sampleSystemOneInstance("typesafe-prod")
	assert.ErrorIs(t, s.CreateSystemOneInstance(ctx, dupName), store.ErrAlreadyExists)

	// Duplicate ID -> ErrAlreadyExists.
	dupID := sampleSystemOneInstance("typesafe-staging")
	dupID.ID = first.ID
	assert.ErrorIs(t, s.CreateSystemOneInstance(ctx, dupID), store.ErrAlreadyExists)

	// Invalid inputs -> ErrInvalidInput.
	invalidCases := []struct {
		name string
		inst *store.SystemOneInstance
	}{
		{"nil", nil},
		{"empty name", func() *store.SystemOneInstance { i := sampleSystemOneInstance("ok"); i.Name = ""; return i }()},
		{"non-slug name", func() *store.SystemOneInstance { i := sampleSystemOneInstance("ok"); i.Name = "../bad"; return i }()},
		{"uppercase name", func() *store.SystemOneInstance { i := sampleSystemOneInstance("ok"); i.Name = "BadName"; return i }()},
		{"bad uuid", func() *store.SystemOneInstance { i := sampleSystemOneInstance("ok"); i.ID = "bad-uuid"; return i }()},
		{"empty adapter", func() *store.SystemOneInstance { i := sampleSystemOneInstance("ok"); i.Adapter = ""; return i }()},
		{"empty endpoint", func() *store.SystemOneInstance { i := sampleSystemOneInstance("ok"); i.Endpoint = ""; return i }()},
		{"invalid auth kind", func() *store.SystemOneInstance {
			i := sampleSystemOneInstance("ok")
			i.Auth.Kind = "oauth1"
			return i
		}()},
		{"negative maxInFlight", func() *store.SystemOneInstance {
			i := sampleSystemOneInstance("ok")
			i.Limits.MaxInFlight = -1
			return i
		}()},
		{"negative ratePerSec", func() *store.SystemOneInstance {
			i := sampleSystemOneInstance("ok")
			i.Limits.RatePerSec = -0.1
			return i
		}()},
		{"negative maxInputTokens", func() *store.SystemOneInstance {
			i := sampleSystemOneInstance("ok")
			i.Limits.MaxInputTokens = -5
			return i
		}()},
		{"negative callTimeout", func() *store.SystemOneInstance {
			i := sampleSystemOneInstance("ok")
			i.Limits.CallTimeout = -time.Second
			return i
		}()},
	}
	for _, tc := range invalidCases {
		t.Run(tc.name, func(t *testing.T) {
			err := s.CreateSystemOneInstance(ctx, tc.inst)
			require.Truef(t, errors.Is(err, store.ErrInvalidInput), "got err: %v", err)
		})
	}
}

func TestSystemOneInstanceStore_ListAndFilter(t *testing.T) {
	s := newTestSystemOneInstanceStore(t)
	ctx := context.Background()

	b := sampleSystemOneInstance("beta-genai")
	b.Adapter = "google-genai"
	b.Enabled = false
	require.NoError(t, s.CreateSystemOneInstance(ctx, b))

	a := sampleSystemOneInstance("alpha-typesafe")
	a.Adapter = "typesafe"
	a.Enabled = true
	require.NoError(t, s.CreateSystemOneInstance(ctx, a))

	g := sampleSystemOneInstance("gamma-typesafe")
	g.Adapter = "typesafe"
	g.Enabled = true
	require.NoError(t, s.CreateSystemOneInstance(ctx, g))

	// Ordered by Name ascending.
	all, err := s.ListSystemOneInstances(ctx, store.SystemOneInstanceFilter{}, store.ListOptions{})
	require.NoError(t, err)
	assert.Equal(t, 3, all.TotalCount)
	require.Len(t, all.Items, 3)
	assert.Equal(t, []string{"alpha-typesafe", "beta-genai", "gamma-typesafe"}, []string{
		all.Items[0].Name, all.Items[1].Name, all.Items[2].Name,
	})

	// Filter by adapter.
	onlyTS, err := s.ListSystemOneInstances(ctx, store.SystemOneInstanceFilter{Adapter: "typesafe"}, store.ListOptions{})
	require.NoError(t, err)
	assert.Equal(t, 2, onlyTS.TotalCount)
	require.Len(t, onlyTS.Items, 2)

	// Filter by enabled.
	enabled := false
	onlyDisabled, err := s.ListSystemOneInstances(ctx, store.SystemOneInstanceFilter{Enabled: &enabled}, store.ListOptions{})
	require.NoError(t, err)
	assert.Equal(t, 1, onlyDisabled.TotalCount)
	require.Len(t, onlyDisabled.Items, 1)
	assert.Equal(t, "beta-genai", onlyDisabled.Items[0].Name)

	// Cursor pagination.
	page1, err := s.ListSystemOneInstances(ctx, store.SystemOneInstanceFilter{}, store.ListOptions{Limit: 2})
	require.NoError(t, err)
	assert.Equal(t, 3, page1.TotalCount)
	require.Len(t, page1.Items, 2)
	assert.Equal(t, "alpha-typesafe", page1.Items[0].Name)
	assert.Equal(t, "beta-genai", page1.Items[1].Name)
	assert.Equal(t, "beta-genai", page1.NextCursor)

	page2, err := s.ListSystemOneInstances(ctx, store.SystemOneInstanceFilter{}, store.ListOptions{Limit: 2, Cursor: page1.NextCursor})
	require.NoError(t, err)
	assert.Equal(t, 3, page2.TotalCount)
	require.Len(t, page2.Items, 1)
	assert.Equal(t, "gamma-typesafe", page2.Items[0].Name)
	assert.Empty(t, page2.NextCursor)

	_, err = s.ListSystemOneInstances(ctx, store.SystemOneInstanceFilter{}, store.ListOptions{Cursor: "../bad"})
	assert.ErrorIs(t, err, store.ErrInvalidCursor)
}

func TestSystemOneInstanceStore_UpdateBumpsRevisionSelectively(t *testing.T) {
	s := newTestSystemOneInstanceStore(t)
	ctx := context.Background()

	inst := sampleSystemOneInstance("typesafe-prod")
	require.NoError(t, s.CreateSystemOneInstance(ctx, inst))
	require.Equal(t, int64(1), inst.Revision)

	// 1. Toggling Enabled / AllowPrivateNetwork / Location / UpdatedBy does NOT bump Revision.
	toggle := inst.Clone()
	toggle.Enabled = false
	toggle.AllowPrivateNetwork = true
	toggle.Location = "europe-west4"
	toggle.UpdatedBy = "user-admin-2"
	require.NoError(t, s.UpdateSystemOneInstance(ctx, toggle))
	assert.Equal(t, int64(1), toggle.Revision)

	got, err := s.GetSystemOneInstanceByName(ctx, "typesafe-prod")
	require.NoError(t, err)
	assert.Equal(t, int64(1), got.Revision)
	assert.False(t, got.Enabled)
	assert.True(t, got.AllowPrivateNetwork)
	assert.Equal(t, "europe-west4", got.Location)
	assert.Equal(t, "user-admin-2", got.UpdatedBy)

	// 2. Each of Adapter, Model, Endpoint, Auth, Options, Limits bumps Revision by 1.
	wantRev := int64(1)
	steps := []struct {
		name   string
		mutate func(*store.SystemOneInstance)
	}{
		{"adapter", func(i *store.SystemOneInstance) { i.Adapter = "openai-compat" }},
		{"model", func(i *store.SystemOneInstance) { i.Model = "guard-v1" }},
		{"endpoint", func(i *store.SystemOneInstance) { i.Endpoint = "https://next.example.test/v1" }},
		{"auth", func(i *store.SystemOneInstance) { i.Auth.SecretName = "ROTATED_SECRET_REF" }},
		{"options", func(i *store.SystemOneInstance) { i.Options = map[string]string{"profile": "balanced"} }},
		{"limits", func(i *store.SystemOneInstance) { i.Limits.CallTimeout = 2 * time.Second }},
	}
	cur := got
	for _, step := range steps {
		t.Run(step.name, func(t *testing.T) {
			next := cur.Clone()
			step.mutate(next)
			require.NoError(t, s.UpdateSystemOneInstance(ctx, next))
			wantRev++
			assert.Equal(t, wantRev, next.Revision)

			reloaded, err := s.GetSystemOneInstance(ctx, cur.ID)
			require.NoError(t, err)
			assert.Equal(t, wantRev, reloaded.Revision)
			cur = reloaded
		})
	}

	// 3. Stale revision returns ErrRevisionConflict and leaves stored row untouched.
	stale := cur.Clone()
	stale.Revision = 1
	stale.Endpoint = "https://stale-write.example.test"
	assert.ErrorIs(t, s.UpdateSystemOneInstance(ctx, stale), store.ErrRevisionConflict)

	afterConflict, err := s.GetSystemOneInstance(ctx, cur.ID)
	require.NoError(t, err)
	assert.Equal(t, cur.Endpoint, afterConflict.Endpoint)
	assert.Equal(t, wantRev, afterConflict.Revision)

	// 4. Updating by Name (ID empty, Revision=0 unconstrained) also works and bumps Revision.
	byNameUpd := afterConflict.Clone()
	byNameUpd.ID = ""
	byNameUpd.Revision = 0
	byNameUpd.Model = "guard-v2"
	require.NoError(t, s.UpdateSystemOneInstance(ctx, byNameUpd))
	assert.Equal(t, wantRev+1, byNameUpd.Revision)
	assert.Equal(t, cur.ID, byNameUpd.ID)

	// 5. Updating missing row returns ErrNotFound.
	missing := sampleSystemOneInstance("missing-slug")
	missing.ID = uuid.New().String()
	assert.ErrorIs(t, s.UpdateSystemOneInstance(ctx, missing), store.ErrNotFound)
}

func TestSystemOneInstanceStore_Delete(t *testing.T) {
	s := newTestSystemOneInstanceStore(t)
	ctx := context.Background()

	one := sampleSystemOneInstance("inst-one")
	require.NoError(t, s.CreateSystemOneInstance(ctx, one))
	two := sampleSystemOneInstance("inst-two")
	require.NoError(t, s.CreateSystemOneInstance(ctx, two))

	require.NoError(t, s.DeleteSystemOneInstance(ctx, one.ID))
	assert.ErrorIs(t, s.DeleteSystemOneInstance(ctx, one.ID), store.ErrNotFound)
	assert.ErrorIs(t, s.DeleteSystemOneInstance(ctx, "not-a-uuid"), store.ErrNotFound)

	require.NoError(t, s.DeleteSystemOneInstanceByName(ctx, "inst-two"))
	assert.ErrorIs(t, s.DeleteSystemOneInstanceByName(ctx, "inst-two"), store.ErrNotFound)
}

func TestCompositeStore_SystemOneInstancesAndWithTx(t *testing.T) {
	cs := NewCompositeStore(enttest.NewClient(t))
	ctx := context.Background()

	// Rolled-back transaction leaves nothing behind.
	sentinel := errors.New("abort tx")
	err := cs.WithTx(ctx, func(tx store.Store) error {
		if err := tx.CreateSystemOneInstance(ctx, sampleSystemOneInstance("tx-rolled-back")); err != nil {
			return err
		}
		return sentinel
	})
	require.ErrorIs(t, err, sentinel)
	_, err = cs.GetSystemOneInstanceByName(ctx, "tx-rolled-back")
	assert.ErrorIs(t, err, store.ErrNotFound)

	// Committed transaction persists create + revision-bumping update.
	require.NoError(t, cs.WithTx(ctx, func(tx store.Store) error {
		inst := sampleSystemOneInstance("tx-committed")
		if err := tx.CreateSystemOneInstance(ctx, inst); err != nil {
			return err
		}
		inst.Endpoint = "https://updated-in-tx.example.test"
		return tx.UpdateSystemOneInstance(ctx, inst)
	}))

	got, err := cs.GetSystemOneInstanceByName(ctx, "tx-committed")
	require.NoError(t, err)
	assert.Equal(t, int64(2), got.Revision)
	assert.Equal(t, "https://updated-in-tx.example.test", got.Endpoint)
}

func TestSystemOneInstanceStore_ListSystemOneInstanceProjects(t *testing.T) {
	cs := NewCompositeStore(enttest.NewClient(t))
	ctx := context.Background()

	require.NoError(t, cs.CreateSystemOneInstance(ctx, sampleSystemOneInstance("inst-a")))
	require.NoError(t, cs.CreateSystemOneInstance(ctx, sampleSystemOneInstance("inst-b")))

	pZeta := &store.Project{ID: uuid.NewString(), Name: "Zeta Project", Slug: "zeta-proj"}
	pAlpha := &store.Project{ID: uuid.NewString(), Name: "Alpha Project", Slug: "alpha-proj"}
	pNone := &store.Project{ID: uuid.NewString(), Name: "Unbound Project", Slug: "unbound-proj"}
	require.NoError(t, cs.CreateProject(ctx, pZeta))
	require.NoError(t, cs.CreateProject(ctx, pAlpha))
	require.NoError(t, cs.CreateProject(ctx, pNone))

	// Initially empty.
	refs, err := cs.ListSystemOneInstanceProjects(ctx, "inst-a")
	require.NoError(t, err)
	assert.NotNil(t, refs)
	assert.Empty(t, refs)

	// Empty name returns empty slice.
	refs, err = cs.ListSystemOneInstanceProjects(ctx, "")
	require.NoError(t, err)
	assert.Empty(t, refs)

	// Bind inst-a to Zeta and Alpha; bind inst-b to Zeta only.
	_, _, err = cs.SetProjectSystemOneInstances(ctx, pZeta.ID, []string{"inst-a", "inst-b"})
	require.NoError(t, err)
	_, _, err = cs.AddProjectSystemOneInstance(ctx, pAlpha.ID, "inst-a")
	require.NoError(t, err)

	// Ordered by Name ASC.
	refs, err = cs.ListSystemOneInstanceProjects(ctx, "inst-a")
	require.NoError(t, err)
	assert.Equal(t, []store.SystemOneInstanceProjectRef{
		{ID: pAlpha.ID, Name: pAlpha.Name},
		{ID: pZeta.ID, Name: pZeta.Name},
	}, refs)

	refs, err = cs.ListSystemOneInstanceProjects(ctx, "inst-b")
	require.NoError(t, err)
	assert.Equal(t, []store.SystemOneInstanceProjectRef{
		{ID: pZeta.ID, Name: pZeta.Name},
	}, refs)
}
