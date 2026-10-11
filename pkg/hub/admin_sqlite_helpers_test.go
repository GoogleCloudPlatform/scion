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
	"net/http/httptest"
	"os"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/util/logging"
	"github.com/stretchr/testify/require"
	yamlv3 "gopkg.in/yaml.v3"
)

// adoptionAdmin creates a hub system admin (admin role and the system
// super-admin binding).
func adoptionAdmin(t *testing.T, s store.Store, name string) *store.User {
	t.Helper()
	id := tid(name)
	createTestUserWithRole(t, s, id, name+"@adopt.test", store.UserRoleAdmin, store.SystemRoleSuperAdmin)
	u, err := s.GetUser(context.Background(), id)
	require.NoError(t, err)
	return u
}

func decodeJSONBody(t *testing.T, rec *httptest.ResponseRecorder, v interface{}) {
	t.Helper()
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), v), rec.Body.String())
}

func withFingerprint(body map[string]interface{}, p delegationAdoptionPreviewResponse) map[string]interface{} {
	out := map[string]interface{}{"planFingerprint": p.PlanFingerprint, "planId": p.PlanID}
	for k, v := range body {
		out[k] = v
	}
	return out
}

func adoptBody(agentIDs ...string) map[string]interface{} {
	return map[string]interface{}{"operation": "adopt", "scope": map[string]interface{}{"agentIds": agentIDs}}
}

func newTestServerWithStore(t *testing.T) (*Server, store.Store) {
	t.Helper()
	s, err := newTestStore(t, ":memory:")
	if err != nil {
		t.Fatalf("failed to create sqlite store: %v", err)
	}
	srv := &Server{
		store:          s,
		maintenanceLog: logging.Subsystem("hub.maintenance"),
	}
	return srv, s
}

func newSQLiteHubInMode(t *testing.T, workstation bool, seed map[string]string) (*Server, store.Store, *OperationalSettings) {
	t.Helper()
	srv, st, ops := newSQLiteOpsServer(t, nil, seed)
	srv.workstation = workstation
	return srv, st, ops
}

func readYAMLMap(t *testing.T, path string) map[string]interface{} {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]interface{}
	if err := yamlv3.Unmarshal(data, &m); err != nil {
		t.Fatal(err)
	}
	return m
}

func yamlAt(m map[string]interface{}, path ...string) interface{} {
	var cur interface{} = m
	for _, p := range path {
		mm, ok := cur.(map[string]interface{})
		if !ok {
			return nil
		}
		cur = mm[p]
	}
	return cur
}
