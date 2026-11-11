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
	"encoding/json"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/config/opsettings"
	"github.com/GoogleCloudPlatform/scion/pkg/ent/hubsetting"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/store/enttest"
	"github.com/stretchr/testify/require"
)

// opsettings.EffectiveOrigin (which the rebuild preflight uses on rows as
// they stand) agrees with BackfillOrigin's update on the same rows,
// including a NULL updated_by (ptone/scion#5057).
func TestBackfillOrigin_AgreesWithEffectiveOrigin(t *testing.T) {
	client := enttest.NewClient(t)
	s := NewHubSettingStore(client)
	ctx := context.Background()
	str := func(v string) *string { return &v }
	rows := []struct {
		section   string
		origin    string
		updatedBy *string // nil: NULL
	}{
		{"telemetry", "seeded", str("seed")},
		{"access", "seeded", str("admin@example.com")},
		{"profiles", "seeded", nil},
		{"runtimes", "seeded", str("")},
		{"harness_configs", "managed", str("seed")},
		{"notifications", "managed", nil},
		{"_meta", "seeded", str("admin@example.com")},
		// The reserved revision row is exempt by name, even with an
		// updated_by set (ptone/scion#5666).
		{store.HubSettingsRevisionSection, "seeded", str("admin@example.com")},
	}
	for _, r := range rows {
		c := client.HubSetting.Create().SetSection(r.section).SetValue(json.RawMessage(`{}`)).SetOrigin(hubsetting.Origin(r.origin))
		if r.updatedBy != nil {
			c.SetUpdatedBy(*r.updatedBy)
		}
		_, err := c.Save(ctx)
		require.NoError(t, err)
	}

	require.NoError(t, s.BackfillOrigin(ctx))

	for _, r := range rows {
		got, err := client.HubSetting.Query().Where(hubsetting.SectionEQ(r.section)).Only(ctx)
		require.NoError(t, err)
		want := opsettings.EffectiveOrigin(r.section, r.origin, r.updatedBy)
		if string(got.Origin) != want {
			t.Fatalf("section %q (origin %q, updated_by %v): BackfillOrigin gives %q, EffectiveOrigin gives %q",
				r.section, r.origin, r.updatedBy, got.Origin, want)
		}
	}
}

// The opsettings copy of the reserved revision section name matches the
// store's (ptone/scion#5666).
func TestSettingsRevisionSectionNameAgrees(t *testing.T) {
	require.Equal(t, store.HubSettingsRevisionSection, opsettings.SettingsRevisionSection)
}
