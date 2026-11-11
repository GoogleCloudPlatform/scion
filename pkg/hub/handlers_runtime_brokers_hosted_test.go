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
	"net/http"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// hostedTestBroker stores an online Runtime Broker with the given ID.
func hostedTestBroker(t *testing.T, s store.Store, id, name string) {
	t.Helper()
	require.NoError(t, s.CreateRuntimeBroker(context.Background(), &store.RuntimeBroker{
		ID:      id,
		Name:    name,
		Slug:    name,
		Status:  store.BrokerStatusOnline,
		Created: time.Now(),
		Updated: time.Now(),
	}))
}

// rawBrokerRows decodes a broker list body into one raw JSON object per
// row, keyed by broker ID, so a test can tell an absent key from false.
func rawBrokerRows(t *testing.T, body []byte) map[string]map[string]json.RawMessage {
	t.Helper()
	var resp struct {
		Brokers []map[string]json.RawMessage `json:"brokers"`
	}
	require.NoError(t, json.Unmarshal(body, &resp))
	rows := make(map[string]map[string]json.RawMessage, len(resp.Brokers))
	for _, row := range resp.Brokers {
		var id string
		require.NoError(t, json.Unmarshal(row["id"], &id))
		rows[id] = row
	}
	return rows
}

// T1: in the unfiltered list, only the embedded (co-located legacy) broker
// carries hostedByThisServer; a remote broker's row has no such key.
func TestListRuntimeBrokers_HostedByThisServer_EmbeddedLegacy(t *testing.T) {
	srv, s := testServer(t)
	embeddedID := tid("hosted-legacy-embedded")
	remoteID := tid("hosted-legacy-remote")
	hostedTestBroker(t, s, embeddedID, "hosted-legacy-embedded")
	hostedTestBroker(t, s, remoteID, "hosted-legacy-remote")
	srv.SetEmbeddedBrokerID(embeddedID)

	rec := doRequest(t, srv, http.MethodGet, "/api/v1/runtime-brokers", nil)
	require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())
	rows := rawBrokerRows(t, rec.Body.Bytes())

	require.Contains(t, rows, embeddedID)
	require.Contains(t, rows, remoteID)
	assert.JSONEq(t, `true`, string(rows[embeddedID]["hostedByThisServer"]))
	assert.NotContains(t, rows[remoteID], "hostedByThisServer", "a remote broker must omit the key")
}

// T2: every co-located flat instance of the process is marked.
func TestListRuntimeBrokers_HostedByThisServer_EmbeddedFlatInstances(t *testing.T) {
	srv, s := testServer(t)
	flatA := tid("hosted-flat-a")
	flatB := tid("hosted-flat-b")
	remoteID := tid("hosted-flat-remote")
	hostedTestBroker(t, s, flatA, "hosted-flat-a")
	hostedTestBroker(t, s, flatB, "hosted-flat-b")
	hostedTestBroker(t, s, remoteID, "hosted-flat-remote")
	srv.mu.Lock()
	srv.recordEmbeddedFlatActivatedLocked("docker-a", flatA)
	srv.recordEmbeddedFlatActivatedLocked("k8s-b", flatB)
	srv.mu.Unlock()

	rec := doRequest(t, srv, http.MethodGet, "/api/v1/runtime-brokers", nil)
	require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())
	rows := rawBrokerRows(t, rec.Body.Bytes())

	for _, id := range []string{flatA, flatB} {
		require.Contains(t, rows, id)
		assert.JSONEq(t, `true`, string(rows[id]["hostedByThisServer"]), "flat instance %s", id)
	}
	require.Contains(t, rows, remoteID)
	assert.NotContains(t, rows[remoteID], "hostedByThisServer")
}

// T3: GET by ID reports the field for an embedded broker and omits it for
// a remote one.
func TestGetRuntimeBroker_HostedByThisServer(t *testing.T) {
	srv, s := testServer(t)
	embeddedID := tid("hosted-get-embedded")
	remoteID := tid("hosted-get-remote")
	hostedTestBroker(t, s, embeddedID, "hosted-get-embedded")
	hostedTestBroker(t, s, remoteID, "hosted-get-remote")
	srv.SetEmbeddedBrokerID(embeddedID)

	get := func(id string) map[string]json.RawMessage {
		t.Helper()
		rec := doRequest(t, srv, http.MethodGet, "/api/v1/runtime-brokers/"+id, nil)
		require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())
		var row map[string]json.RawMessage
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &row))
		return row
	}

	assert.JSONEq(t, `true`, string(get(embeddedID)["hostedByThisServer"]))
	assert.NotContains(t, get(remoteID), "hostedByThisServer")
}

// T4: the project-filtered list (?projectId=) is unchanged: it never
// carries the key, even for the embedded broker.
func TestListRuntimeBrokers_HostedByThisServer_NotOnProjectFilteredList(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()
	embeddedID := tid("hosted-project-embedded")
	projectID := tid("hosted-project")
	hostedTestBroker(t, s, embeddedID, "hosted-project-embedded")
	require.NoError(t, s.CreateProject(ctx, &store.Project{
		ID: projectID, Name: "hosted-project", Slug: "hosted-project",
	}))
	require.NoError(t, s.AddProjectProvider(ctx, &store.ProjectProvider{
		ProjectID: projectID, BrokerID: embeddedID, BrokerName: "hosted-project-embedded",
		Status: store.BrokerStatusOnline,
	}))
	srv.SetEmbeddedBrokerID(embeddedID)

	rec := doRequest(t, srv, http.MethodGet, "/api/v1/runtime-brokers?projectId="+projectID, nil)
	require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())
	rows := rawBrokerRows(t, rec.Body.Bytes())

	require.Contains(t, rows, embeddedID, "precondition: the embedded broker is a provider of the project")
	assert.NotContains(t, rows[embeddedID], "hostedByThisServer")
}

// The field round-trips through the response type's custom JSON methods.
func TestRuntimeBrokerWithCapabilities_HostedByThisServerRoundTrip(t *testing.T) {
	in := RuntimeBrokerWithCapabilities{
		RuntimeBroker:      store.RuntimeBroker{ID: "b1", Name: "b1"},
		HostedByThisServer: true,
	}
	data, err := json.Marshal(in)
	require.NoError(t, err)
	var out RuntimeBrokerWithCapabilities
	require.NoError(t, json.Unmarshal(data, &out))
	assert.True(t, out.HostedByThisServer)

	data, err = json.Marshal(RuntimeBrokerWithCapabilities{RuntimeBroker: store.RuntimeBroker{ID: "b2"}})
	require.NoError(t, err)
	assert.NotContains(t, string(data), "hostedByThisServer", "false must be omitted")
}
