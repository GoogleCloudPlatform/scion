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

package hubclient

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/apiclient"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRuntimeBrokerGetSettings_GETOnly(t *testing.T) {
	var method, path string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		method, path = r.Method, r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"brokerId":"b-1","settings":{"maxAgents":10},` +
			`"effective":{"maxAgents":{"value":10,"source":"broker","count":2,` +
			`"inherited":{"value":25,"source":"default"}}},"revision":3}`))
	}))
	t.Cleanup(srv.Close)
	c, err := New(srv.URL)
	require.NoError(t, err)

	getter, ok := c.RuntimeBrokers().(RuntimeBrokerSettingsGetter)
	require.True(t, ok, "the package's RuntimeBrokerService implements RuntimeBrokerSettingsGetter")
	got, err := getter.GetSettings(context.Background(), "b-1")
	require.NoError(t, err)

	assert.Equal(t, http.MethodGet, method)
	assert.Equal(t, "/api/v1/runtime-brokers/b-1/settings", path)
	require.NotNil(t, got.Settings.MaxAgents)
	assert.Equal(t, int64(10), *got.Settings.MaxAgents)
	require.NotNil(t, got.Effective.MaxAgents.Count)
	assert.Equal(t, int64(2), *got.Effective.MaxAgents.Count)
	require.NotNil(t, got.Effective.MaxAgents.Inherited.Value)
	assert.Equal(t, int64(25), *got.Effective.MaxAgents.Inherited.Value)
	assert.Equal(t, "default", got.Effective.MaxAgents.Inherited.Source)
}

func TestRuntimeBrokerGetSettings_UnsetOverride(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"brokerId":"b-1","settings":{},"effective":{"maxAgents":{"value":0,"source":"unlimited","inherited":{"value":0,"source":"unlimited"}}},"revision":0}`))
	}))
	t.Cleanup(srv.Close)
	c, err := New(srv.URL)
	require.NoError(t, err)

	got, err := c.RuntimeBrokers().(RuntimeBrokerSettingsGetter).GetSettings(context.Background(), "b-1")
	require.NoError(t, err)
	assert.Nil(t, got.Settings.MaxAgents)
}

func TestRuntimeBrokerGetSettings_Forbidden(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"error":{"code":"forbidden","message":"no"}}`))
	}))
	t.Cleanup(srv.Close)
	c, err := New(srv.URL)
	require.NoError(t, err)

	_, err = c.RuntimeBrokers().(RuntimeBrokerSettingsGetter).GetSettings(context.Background(), "b-1")
	require.Error(t, err)
	assert.True(t, apiclient.IsForbiddenError(err))
}
