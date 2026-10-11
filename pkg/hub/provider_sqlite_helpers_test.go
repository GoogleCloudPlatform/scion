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

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/require"
)

// brokerGlobalDir is a broker's global scion directory as provide sent it
// when run from the broker user's home directory. The hub never initializes
// it in these tests: it is either rejected or written straight to the store.
const brokerGlobalDir = "/home/brokeruser/.scion"

func newLocalPathTestBroker(t *testing.T, s store.Store, name string) *store.RuntimeBroker {
	t.Helper()
	broker := &store.RuntimeBroker{
		ID:     tid(name),
		Name:   name,
		Slug:   name,
		Status: store.BrokerStatusOnline,
	}
	require.NoError(t, s.CreateRuntimeBroker(context.Background(), broker))
	return broker
}

func registerWithBroker(t *testing.T, srv *Server, name, brokerID, path string) (*RegisterProjectResponse, int, string) {
	t.Helper()
	body := map[string]interface{}{"name": name, "brokerId": brokerID}
	if path != "" {
		body["path"] = path
	}
	rec := doRequest(t, srv, http.MethodPost, "/api/v1/projects/register", body)
	if rec.Code != http.StatusOK {
		return nil, rec.Code, rec.Body.String()
	}
	var resp RegisterProjectResponse
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&resp))
	return &resp, rec.Code, rec.Body.String()
}
