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
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

var nonComponentResourceNames = []string{"../x", "../../x", "a/b", `a\b`, "/abs/x", "..", ".", "", ":local:/x", "https://h/x"}

func TestTemplateStoreRefusesNonComponentName(t *testing.T) {
	ts := newTestTemplateStore(t)
	ctx := context.Background()

	ok := &store.Template{ID: uuid.New().String(), Name: "Tpl_One.v2", Slug: "tpl-one", Harness: "claude", Scope: store.TemplateScopeGlobal}
	require.NoError(t, ts.CreateTemplate(ctx, ok), "control: a one-component name is stored")

	for _, name := range nonComponentResourceNames {
		tmpl := &store.Template{ID: uuid.New().String(), Name: name, Slug: "tpl-x", Harness: "claude", Scope: store.TemplateScopeGlobal}
		err := ts.CreateTemplate(ctx, tmpl)
		require.Truef(t, errors.Is(err, store.ErrInvalidInput), "CreateTemplate(%q) err = %v", name, err)
		if name != "" {
			require.NotContains(t, err.Error(), name)
		}
		_, getErr := ts.GetTemplate(ctx, tmpl.ID)
		require.ErrorIs(t, getErr, store.ErrNotFound, "nothing stored for %q", name)

		upd := *ok
		upd.Name = name
		require.Truef(t, errors.Is(ts.UpdateTemplate(ctx, &upd), store.ErrInvalidInput), "UpdateTemplate(%q)", name)
	}
	got, err := ts.GetTemplate(ctx, ok.ID)
	require.NoError(t, err)
	require.Equal(t, "Tpl_One.v2", got.Name, "a refused update leaves the stored name")
}

func TestHarnessConfigStoreRefusesNonComponentName(t *testing.T) {
	ts := newTestTemplateStore(t)
	ctx := context.Background()

	ok := &store.HarnessConfig{ID: uuid.New().String(), Name: "hc-one", Slug: "hc-one", Harness: "claude", Scope: store.HarnessConfigScopeGlobal}
	require.NoError(t, ts.CreateHarnessConfig(ctx, ok), "control: a one-component name is stored")

	for _, name := range nonComponentResourceNames {
		hc := &store.HarnessConfig{ID: uuid.New().String(), Name: name, Slug: "hc-x", Harness: "claude", Scope: store.HarnessConfigScopeGlobal}
		err := ts.CreateHarnessConfig(ctx, hc)
		require.Truef(t, errors.Is(err, store.ErrInvalidInput), "CreateHarnessConfig(%q) err = %v", name, err)
		require.False(t, name != "" && strings.Contains(err.Error(), name), "error echoes the name")
		_, getErr := ts.GetHarnessConfig(ctx, hc.ID)
		require.ErrorIs(t, getErr, store.ErrNotFound, "nothing stored for %q", name)

		upd := *ok
		upd.Name = name
		require.Truef(t, errors.Is(ts.UpdateHarnessConfig(ctx, &upd), store.ErrInvalidInput), "UpdateHarnessConfig(%q)", name)
	}
	got, err := ts.GetHarnessConfig(ctx, ok.ID)
	require.NoError(t, err)
	require.Equal(t, "hc-one", got.Name, "a refused update leaves the stored name")
}
