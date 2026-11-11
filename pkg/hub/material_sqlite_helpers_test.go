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
	"encoding/json"
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"
)

// assertProjectDenied issues both a fetch (multi-key POST) and a get
// (by-key GET) for key against agentID/token and asserts the full check-7
// deny contract: not_found with no value on both endpoints, no backend
// value read, and an audited denied_by_policy reason with a non-empty
// Detail.
func assertProjectDenied(t *testing.T, f *materialFixture, agentID, token, key string) {
	t.Helper()

	counting := &countingSecretBackend{SecretBackend: f.Server.secretBackend}
	f.Server.SetSecretBackend(counting)

	rec := newRecordingMaterialAuditor()
	f.Server.SetAuditLogger(rec)

	fetchRec := doRequestWithAgentToken(t, f.Server, http.MethodPost, "/api/v1/agent/secrets",
		secretFetchRequest{Keys: []string{key}}, token)
	if fetchRec.Code != http.StatusOK {
		t.Fatalf("fetch: expected 200 (per-item status, not a request-level error), got %d: %s", fetchRec.Code, fetchRec.Body.String())
	}
	var fetchResp secretFetchResponse
	require.NoError(t, json.NewDecoder(fetchRec.Body).Decode(&fetchResp))
	if len(fetchResp.Secrets) != 1 || fetchResp.Secrets[0].Status != "not_found" ||
		fetchResp.Secrets[0].Value != "" || fetchResp.Secrets[0].Error != "secret not found" {
		t.Fatalf("fetch: expected not_found/secret not found with no value, got %+v", fetchResp.Secrets)
	}

	getRec := doRequestWithAgentToken(t, f.Server, http.MethodGet, "/api/v1/agents/"+agentID+"/secrets/"+key, nil, token)
	if getRec.Code != http.StatusNotFound {
		t.Fatalf("get: expected 404, got %d: %s", getRec.Code, getRec.Body.String())
	}

	if counting.getCalls != 0 {
		t.Fatalf("expected no value read (Get) on a denied item, got %d Get calls", counting.getCalls)
	}

	if len(rec.events) != 2 {
		t.Fatalf("expected 2 material selection events (fetch and get), got %d", len(rec.events))
	}
	for _, e := range rec.events {
		if len(e.Items) != 1 {
			t.Fatalf("expected 1 item per event, got %d", len(e.Items))
		}
		item := e.Items[0]
		if item.Reason != ReasonDeniedByPolicy {
			t.Fatalf("expected reason %s, got %s", ReasonDeniedByPolicy, item.Reason)
		}
		if item.Detail == "" {
			t.Fatalf("expected a non-empty Detail carrying Decision.Reason verbatim")
		}
	}
}

// assertBackendErrorAudited asserts that the last MaterialSelectionEvent
// recorded by rec carries exactly one item with Reason == ReasonBackendError:
// the backend-fault tests assert the audited reason, not only the HTTP
// status.
func assertBackendErrorAudited(t *testing.T, rec *recordingMaterialAuditor) {
	t.Helper()
	if len(rec.events) == 0 {
		t.Fatalf("expected at least 1 material selection event")
	}
	e := rec.events[len(rec.events)-1]
	if len(e.Items) != 1 {
		t.Fatalf("expected 1 item, got %d", len(e.Items))
	}
	if e.Items[0].Reason != ReasonBackendError {
		t.Fatalf("expected reason %s, got %s", ReasonBackendError, e.Items[0].Reason)
	}
}
