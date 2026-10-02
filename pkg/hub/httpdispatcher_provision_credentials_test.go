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

package hub

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/secret"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// provisionCredsBackend serves a fixed project-scope listing; Get fails for
// names in failGet and returns values otherwise.
type provisionCredsBackend struct {
	secret.SecretBackend
	listErr error
	metas   []secret.SecretMeta
	values  map[string]string
	failGet map[string]bool
}

func (b *provisionCredsBackend) List(context.Context, secret.Filter) ([]secret.SecretMeta, error) {
	return b.metas, b.listErr
}

func (b *provisionCredsBackend) Get(_ context.Context, name, _, _ string) (*secret.SecretWithValue, error) {
	if b.failGet[name] {
		return nil, errors.New("backend unavailable")
	}
	return &secret.SecretWithValue{SecretMeta: secret.SecretMeta{Name: name}, Value: b.values[name]}, nil
}

// lockedBuffer is a goroutine-safe log sink: Get failures are logged from
// errgroup goroutines.
type lockedBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuffer) records(t *testing.T) []map[string]any {
	t.Helper()
	l.mu.Lock()
	defer l.mu.Unlock()
	var out []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(l.b.String()), "\n") {
		if line == "" {
			continue
		}
		var rec map[string]any
		require.NoError(t, json.Unmarshal([]byte(line), &rec))
		out = append(out, rec)
	}
	return out
}

func (l *lockedBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

func newProvisionCredsDispatcher(backend secret.SecretBackend) (*HTTPAgentDispatcher, *lockedBuffer) {
	buf := &lockedBuffer{}
	// debug=false: the failure warnings and the count must not depend on it.
	d := NewHTTPAgentDispatcherWithClient(nil, nil, false, slog.New(slog.NewJSONHandler(buf, &slog.HandlerOptions{Level: slog.LevelInfo})))
	d.secretBackend = backend
	return d, buf
}

func findProvisionCredsRecord(recs []map[string]any, level, msgSuffix string) map[string]any {
	for _, r := range recs {
		if r["level"] == level && strings.HasSuffix(r["msg"].(string), msgSuffix) {
			return r
		}
	}
	return nil
}

func TestResolveProvisionCredentials_ListFailureWarns(t *testing.T) {
	d, buf := newProvisionCredsDispatcher(&provisionCredsBackend{listErr: errors.New("backend unavailable")})
	agent := &store.Agent{ID: "agent-1", ProjectID: "project-1"}

	creds := d.resolveProvisionCredentials(context.Background(), agent, "buildCreateRequest")
	assert.Nil(t, creds)

	rec := findProvisionCredsRecord(buf.records(t), "WARN", "failed to list project secrets for ProvisionCredentials")
	require.NotNil(t, rec, "a List failure must be logged at Warn without debug; log: %s", buf.String())
	assert.Equal(t, "agent-1", rec["agent_id"])
	assert.Equal(t, "project-1", rec["project_id"])
}

func TestResolveProvisionCredentials_GetFailureWarnsWithNameOnly(t *testing.T) {
	const value = "value-that-must-not-be-logged"
	backend := &provisionCredsBackend{
		metas: []secret.SecretMeta{
			{Name: "GH_SCION_FRONTIERS", SecretType: "environment"},
			{Name: "GH_OTHER", SecretType: "environment"},
		},
		values:  map[string]string{"GH_OTHER": value},
		failGet: map[string]bool{"GH_SCION_FRONTIERS": true},
	}
	d, buf := newProvisionCredsDispatcher(backend)
	agent := &store.Agent{ID: "agent-1", ProjectID: "project-1"}

	creds := d.resolveProvisionCredentials(context.Background(), agent, "buildCreateRequest")
	assert.Equal(t, map[string]string{"GH_OTHER": value}, creds)

	recs := buf.records(t)
	warn := findProvisionCredsRecord(recs, "WARN", "failed to get project secret for ProvisionCredentials")
	require.NotNil(t, warn, "a Get failure must be logged at Warn without debug; log: %s", buf.String())
	assert.Equal(t, "GH_SCION_FRONTIERS", warn["secret"])
	assert.Equal(t, "project-1", warn["project_id"])

	info := findProvisionCredsRecord(recs, "INFO", "ProvisionCredentials resolved")
	require.NotNil(t, info, "the credential count must be logged at Info; log: %s", buf.String())
	assert.EqualValues(t, 1, info["count"])

	assert.NotContains(t, buf.String(), value, "secret values must never be logged")
}

func TestResolveProvisionCredentials_CountWhenNone(t *testing.T) {
	d, buf := newProvisionCredsDispatcher(&provisionCredsBackend{})
	agent := &store.Agent{ID: "agent-1", ProjectID: "project-1"}

	assert.Nil(t, d.resolveProvisionCredentials(context.Background(), agent, "DispatchAgentStart"))
	info := findProvisionCredsRecord(buf.records(t), "INFO", "ProvisionCredentials resolved")
	require.NotNil(t, info, "log: %s", buf.String())
	assert.EqualValues(t, 0, info["count"])
}
