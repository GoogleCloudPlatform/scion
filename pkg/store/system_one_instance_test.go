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

package store

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestIsValidSystemOneInstanceName(t *testing.T) {
	valid := []string{
		"typesafe",
		"typesafe-prod",
		"gemini-2-5-flash",
		"a",
		"x1",
	}
	for _, s := range valid {
		assert.Truef(t, IsValidSystemOneInstanceName(s), "expected %q to be valid", s)
	}

	invalid := []string{
		"",
		"-leading",
		"trailing-",
		"double--dash",
		"UpperCase",
		"under_score",
		"../traversal",
		"a/b",
		"has space",
		"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", // 64 chars (> 63)
	}
	for _, s := range invalid {
		assert.Falsef(t, IsValidSystemOneInstanceName(s), "expected %q to be invalid", s)
	}
}

func TestIsValidSystemOneAuthKind(t *testing.T) {
	for _, k := range []string{
		SystemOneAuthKindNone,
		SystemOneAuthKindAPIKey,
		SystemOneAuthKindGoogleAccessToken,
		SystemOneAuthKindGoogleIDToken,
		SystemOneAuthKindAzureEntra,
	} {
		assert.True(t, IsValidSystemOneAuthKind(k), k)
	}
	for _, k := range []string{"", "bearer", "basic", "oauth2"} {
		assert.False(t, IsValidSystemOneAuthKind(k), k)
	}
}

func TestSystemOneRuntimeConfigChanged(t *testing.T) {
	base := &SystemOneInstance{
		ID:       "id-1",
		Name:     "typesafe-prod",
		Adapter:  "typesafe",
		Model:    "",
		Endpoint: "https://api.example.test",
		Location: "us-central1",
		Auth: SystemOneInstanceAuth{
			Kind:       SystemOneAuthKindAPIKey,
			SecretName: "TS_PROD_KEY",
			HeaderName: "X-API-Key",
			Scopes:     []string{"scope-a"},
			Audience:   "aud-1",
		},
		Options: map[string]string{"mode": "strict"},
		Limits: SystemOneInstanceLimits{
			MaxInFlight:    8,
			RatePerSec:     10.5,
			MaxInputTokens: 4096,
			CallTimeout:    5 * time.Second,
		},
		AllowPrivateNetwork: false,
		Enabled:             true,
		Revision:            1,
	}

	// Identical clone -> unchanged.
	same := base.Clone()
	assert.False(t, SystemOneRuntimeConfigChanged(base, same))

	// Nil vs empty slices/maps and default empty auth kind -> unchanged.
	emptyBase := &SystemOneInstance{
		Adapter:  "typesafe",
		Endpoint: "https://api.example.test",
		Auth:     SystemOneInstanceAuth{Kind: ""},
		Options:  nil,
	}
	emptyNext := &SystemOneInstance{
		Adapter:  "typesafe",
		Endpoint: "https://api.example.test",
		Auth:     SystemOneInstanceAuth{Kind: SystemOneAuthKindAPIKey, Scopes: []string{}},
		Options:  map[string]string{},
	}
	assert.False(t, SystemOneRuntimeConfigChanged(emptyBase, emptyNext))

	// Non-evaluator fields do NOT count as runtime config changes.
	metaChanged := base.Clone()
	metaChanged.Enabled = false
	metaChanged.AllowPrivateNetwork = true
	metaChanged.Location = "europe-west1"
	metaChanged.UpdatedBy = "user-2"
	assert.False(t, SystemOneRuntimeConfigChanged(base, metaChanged))

	// Every one of Adapter, Model, Endpoint, Auth, Options, Limits triggers true.
	mutations := []struct {
		name   string
		mutate func(*SystemOneInstance)
	}{
		{"adapter", func(i *SystemOneInstance) { i.Adapter = "openai-compat" }},
		{"model", func(i *SystemOneInstance) { i.Model = "gpt-4o-mini" }},
		{"endpoint", func(i *SystemOneInstance) { i.Endpoint = "https://other.example.test" }},
		{"auth.kind", func(i *SystemOneInstance) { i.Auth.Kind = SystemOneAuthKindNone }},
		{"auth.secretName", func(i *SystemOneInstance) { i.Auth.SecretName = "OTHER_SECRET" }},
		{"auth.headerName", func(i *SystemOneInstance) { i.Auth.HeaderName = "Authorization" }},
		{"auth.scopes", func(i *SystemOneInstance) { i.Auth.Scopes = []string{"scope-a", "scope-b"} }},
		{"auth.audience", func(i *SystemOneInstance) { i.Auth.Audience = "aud-2" }},
		{"options.value", func(i *SystemOneInstance) { i.Options["mode"] = "relaxed" }},
		{"options.added", func(i *SystemOneInstance) { i.Options["extra"] = "1" }},
		{"limits.maxInFlight", func(i *SystemOneInstance) { i.Limits.MaxInFlight = 16 }},
		{"limits.ratePerSec", func(i *SystemOneInstance) { i.Limits.RatePerSec = 20 }},
		{"limits.maxInputTokens", func(i *SystemOneInstance) { i.Limits.MaxInputTokens = 8192 }},
		{"limits.callTimeout", func(i *SystemOneInstance) { i.Limits.CallTimeout = 3 * time.Second }},
	}
	for _, tc := range mutations {
		t.Run(tc.name, func(t *testing.T) {
			m := base.Clone()
			tc.mutate(m)
			require.True(t, SystemOneRuntimeConfigChanged(base, m))
		})
	}
}
