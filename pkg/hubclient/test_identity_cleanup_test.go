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
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/apiclient"
)

// Tests for the issuer cleanup and provenance client (ptone/scion#4240,
// Phase 3c).

// Cleanup posts with no body (dryRun only in the query), is sent once even
// with a retrying client, and decodes the report.
func TestTestIdentityCleanup_SendsNoBodyOnce(t *testing.T) {
	for _, dry := range []bool{false, true} {
		var calls atomic.Int32
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			calls.Add(1)
			if r.Method != http.MethodPost || r.URL.Path != "/api/v1/test-identities/cleanup" {
				t.Errorf("got %s %s", r.Method, r.URL.Path)
			}
			wantQuery := ""
			if dry {
				wantQuery = "dryRun=true"
			}
			if r.URL.RawQuery != wantQuery {
				t.Errorf("query %q, want %q", r.URL.RawQuery, wantQuery)
			}
			body, _ := io.ReadAll(r.Body)
			if len(body) != 0 {
				t.Errorf("cleanup sent a body: %q", body)
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"runId":"run-1","dryRun":false,"scope":"issuer","complete":true,` +
				`"identities":[{"id":"fx-1","state":"expired"}],` +
				`"targets":[{"type":"agent","id":"a-1","fixtureRootId":"fx-1","projectId":"p-1","status":"deleted"}],` +
				`"deleted":{"agents":1,"schedules":0,"groups":0,"projects":0},"identitiesDeleted":1,"liveSkipped":2}`))
		}))
		c, err := New(srv.URL, WithRetry(3, 0))
		if err != nil {
			t.Fatal(err)
		}
		r, err := c.TestIdentities().Cleanup(context.Background(), dry)
		srv.Close()
		if err != nil {
			t.Fatal(err)
		}
		if calls.Load() != 1 {
			t.Errorf("cleanup sent %d times, want 1", calls.Load())
		}
		if r.RunID != "run-1" || r.Scope != "issuer" || !r.Complete || len(r.Targets) != 1 || r.Targets[0].Status != "deleted" ||
			r.Targets[0].FixtureRootID != "fx-1" || r.Deleted.Agents != 1 || r.IdentitiesDeleted != 1 || r.LiveSkipped != 2 {
			t.Errorf("unexpected report %+v", r)
		}
	}
}

// An incomplete cleanup (409 purge_incomplete) returns both its report and
// the error; a 500 is not retried; a 403 is a forbidden APIError and no
// report.
func TestTestIdentityCleanup_Failures(t *testing.T) {
	var calls atomic.Int32
	status := http.StatusConflict
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		switch status {
		case http.StatusConflict:
			_, _ = w.Write([]byte(`{"error":{"code":"conflict","message":"did not complete","details":{"reason":"purge_incomplete",` +
				`"report":{"runId":"run-2","scope":"issuer","complete":false,"identities":[],"targets":[{"type":"agent","id":"a-2","fixtureRootId":"fx-2","status":"not_done","reason":"in_doubt"}],"deleted":{"agents":0,"schedules":0,"groups":0,"projects":0}}}}}`))
		case http.StatusForbidden:
			_, _ = w.Write([]byte(`{"error":{"code":"forbidden","message":"requires test_identity.issue permission"}}`))
		default:
			_, _ = w.Write([]byte(`{"error":{"code":"internal_error","message":"boom"}}`))
		}
	}))
	defer srv.Close()
	c, err := New(srv.URL, WithRetry(3, 0))
	if err != nil {
		t.Fatal(err)
	}

	r, err := c.TestIdentities().Cleanup(context.Background(), false)
	if err == nil || r == nil || r.Complete || r.RunID != "run-2" || len(r.Targets) != 1 || r.Targets[0].Status != "not_done" {
		t.Fatalf("incomplete: report %+v err %v", r, err)
	}

	status = http.StatusInternalServerError
	calls.Store(0)
	r, err = c.TestIdentities().Cleanup(context.Background(), false)
	if err == nil || r != nil {
		t.Fatalf("500: report %+v err %v", r, err)
	}
	if calls.Load() != 1 {
		t.Errorf("a failed cleanup was sent %d times, want 1", calls.Load())
	}

	status = http.StatusForbidden
	r, err = c.TestIdentities().Cleanup(context.Background(), false)
	if r != nil || !apiclient.IsForbiddenError(err) {
		t.Fatalf("403: report %+v err %v", r, err)
	}
}

// Provenance sends the resource as the only query parameter, escaped, and
// decodes both answers; an empty resource is refused before sending.
func TestTestIdentityProvenance(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.Method != http.MethodGet || r.URL.Path != "/api/v1/test-identities/provenance" {
			t.Errorf("got %s %s", r.Method, r.URL.Path)
		}
		q := r.URL.Query()
		if len(q) != 1 {
			t.Errorf("query %q", r.URL.RawQuery)
		}
		w.Header().Set("Content-Type", "application/json")
		switch q.Get("resource") {
		case "agent:a-1":
			_, _ = w.Write([]byte(`{"resource":"agent:a-1","proven":true,"fixtureRootId":"fx-1","fixtureIssuerId":"iss-1","fixtureState":"expired","reason":"fixture stamp"}`))
		case "project:p 1&x=y":
			_, _ = w.Write([]byte(`{"resource":"project:p 1&x=y","proven":false,"reason":"no fixture stamp"}`))
		default:
			t.Errorf("unexpected resource %q", q.Get("resource"))
		}
	}))
	defer srv.Close()
	c, err := New(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	p, err := c.TestIdentities().Provenance(context.Background(), "agent:a-1")
	if err != nil {
		t.Fatal(err)
	}
	if !p.Proven || p.FixtureRootID != "fx-1" || p.FixtureIssuerID != "iss-1" || p.FixtureState != "expired" {
		t.Errorf("proven: %+v", p)
	}
	p, err = c.TestIdentities().Provenance(context.Background(), "project:p 1&x=y")
	if err != nil {
		t.Fatal(err)
	}
	if p.Proven || p.Reason != "no fixture stamp" || p.FixtureRootID != "" {
		t.Errorf("unproven: %+v", p)
	}
	before := calls.Load()
	if _, err := c.TestIdentities().Provenance(context.Background(), ""); err == nil {
		t.Error("empty resource accepted")
	}
	if calls.Load() != before {
		t.Error("empty resource was sent")
	}
}
