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
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/experiments"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func setAttachBrokerPolicyState(t *testing.T, srv *Server, s store.Store, expOn, policyOn bool) {
	t.Helper()
	ctx := context.Background()
	expDoc, err := json.Marshal(map[string]any{
		"overrides": map[string]bool{
			experiments.AdminManagedBrokers: expOn,
		},
	})
	require.NoError(t, err)
	_, err = s.UpsertHubSetting(ctx, "experiments", json.RawMessage(expDoc), "test", -1, "managed")
	require.NoError(t, err)

	policyDoc := json.RawMessage(`{"admin_managed_only":false}`)
	if policyOn {
		policyDoc = json.RawMessage(`{"admin_managed_only":true}`)
	}
	_, err = s.UpsertHubSetting(ctx, "broker_policy", policyDoc, "test", -1, "managed")
	require.NoError(t, err)

	ops := NewOperationalSettings(s, emptyKoanf(), emptyKoanf())
	_, err = ops.Refresh(ctx)
	require.NoError(t, err)
	ApplySnapshot(srv, ops.Snapshot())
	srv.SetOperationalSettings(ops)
}

func assertAttachBrokerPolicyDenial(t *testing.T, rec *httptest.ResponseRecorder, broker *store.RuntimeBroker) {
	t.Helper()
	require.Equal(t, http.StatusForbidden, rec.Code, "body: %s", rec.Body.String())
	var env struct {
		Error struct {
			Code    string                 `json:"code"`
			Message string                 `json:"message"`
			Details map[string]interface{} `json:"details"`
		} `json:"error"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &env), "body: %s", rec.Body.String())
	assert.Equal(t, ErrCodeBrokerPolicyAdminManagedOnly, env.Error.Code)
	assert.Equal(t, brokerPolicySettingKey, env.Error.Details["setting"])
	assert.Equal(t, brokerPolicyOpAttach, env.Error.Details["operation"])
	assert.Equal(t, broker.ID, env.Error.Details["brokerId"])
	assert.Contains(t, env.Error.Message, brokerPolicySettingKey)
	assert.Contains(t, env.Error.Message, broker.Name)
	assert.NotContains(t, env.Error.Message, brokerProvideDeniedMessage)
}

func captureBrokerPolicyWarnLogs(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn})))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return &buf
}
