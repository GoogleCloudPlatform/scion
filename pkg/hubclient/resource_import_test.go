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
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/apiclient"
)

func TestTemplateService_ImportResources_SuccessAndMarshal(t *testing.T) {
	var gotPath, gotMethod, gotAccept string
	var gotBody map[string]any

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotMethod = r.Method
		gotAccept = r.Header.Get("Accept")
		if err := json.NewDecoder(r.Body).Decode(&gotBody); err != nil {
			t.Errorf("decode request body: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"kind":     "template",
			"imported": []string{"alpha", "beta"},
			"count":    2,
		})
	}))
	defer srv.Close()

	c, err := New(srv.URL)
	if err != nil {
		t.Fatalf("New client: %v", err)
	}

	resp, err := c.Templates().ImportResources(context.Background(), &ImportResourcesRequest{
		Kind:      "template",
		Scope:     "project",
		ScopeID:   "proj-123",
		SourceURL: "https://github.com/org/repo/tree/main/.scion/templates",
		Names:     []string{"alpha", "beta"},
	})
	if err != nil {
		t.Fatalf("ImportResources failed: %v", err)
	}

	if gotMethod != http.MethodPost {
		t.Errorf("method = %q, want POST", gotMethod)
	}
	if gotPath != "/api/v1/resources/import" {
		t.Errorf("path = %q, want /api/v1/resources/import", gotPath)
	}
	if gotAccept == "application/x-ndjson" {
		t.Errorf("Accept header must not request ndjson stream, got %q", gotAccept)
	}

	wantKeys := map[string]any{
		"kind":      "template",
		"scope":     "project",
		"scopeId":   "proj-123",
		"sourceUrl": "https://github.com/org/repo/tree/main/.scion/templates",
		"names":     []any{"alpha", "beta"},
	}
	if !reflect.DeepEqual(gotBody, wantKeys) {
		t.Errorf("request body = %#v, want %#v", gotBody, wantKeys)
	}

	if resp.Kind != "template" || resp.Count != 2 || !reflect.DeepEqual(resp.Imported, []string{"alpha", "beta"}) || len(resp.Failed) != 0 {
		t.Errorf("unexpected response: %+v", resp)
	}
}

func TestTemplateService_ImportResources_OmitEmptyFields(t *testing.T) {
	var gotBody map[string]any

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&gotBody); err != nil {
			t.Errorf("decode request body: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"kind":     "template",
			"imported": []string{"alpha"},
			"count":    1,
		})
	}))
	defer srv.Close()

	c, err := New(srv.URL)
	if err != nil {
		t.Fatalf("New client: %v", err)
	}

	_, err = c.Templates().ImportResources(context.Background(), &ImportResourcesRequest{
		Kind:      "template",
		Scope:     "user",
		SourceURL: "https://github.com/org/repo/tree/main/.scion/templates",
	})
	if err != nil {
		t.Fatalf("ImportResources failed: %v", err)
	}

	if _, ok := gotBody["scopeId"]; ok {
		t.Errorf("scopeId should be omitted when empty, got %v", gotBody["scopeId"])
	}
	if _, ok := gotBody["names"]; ok {
		t.Errorf("names should be omitted when empty, got %v", gotBody["names"])
	}
}

func TestTemplateService_ImportResources_PartialFailure(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"kind":     "template",
			"imported": []string{"good"},
			"count":    1,
			"failed": []map[string]string{
				{"name": "bad", "reason": "template exists; agents can only create new templates"},
			},
		})
	}))
	defer srv.Close()

	c, err := New(srv.URL)
	if err != nil {
		t.Fatalf("New client: %v", err)
	}

	resp, err := c.Templates().ImportResources(context.Background(), &ImportResourcesRequest{
		Kind:      "template",
		Scope:     "project",
		ScopeID:   "proj-1",
		SourceURL: "https://github.com/org/repo/tree/main/.scion/templates",
	})
	if err != nil {
		t.Fatalf("ImportResources partial failure should return nil err, got: %v", err)
	}
	if resp.Count != 1 || !reflect.DeepEqual(resp.Imported, []string{"good"}) {
		t.Errorf("unexpected imported: %+v", resp)
	}
	wantFailed := []ResourceImportFailure{
		{Name: "bad", Reason: "template exists; agents can only create new templates"},
	}
	if !reflect.DeepEqual(resp.Failed, wantFailed) {
		t.Errorf("Failed = %+v, want %+v", resp.Failed, wantFailed)
	}
}

func TestTemplateService_ImportResources_AllFailedReturnsAPIError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"error": map[string]any{
				"code":    "import_failed",
				"message": "config validation failed: bad: invalid config",
			},
		})
	}))
	defer srv.Close()

	c, err := New(srv.URL)
	if err != nil {
		t.Fatalf("New client: %v", err)
	}

	_, err = c.Templates().ImportResources(context.Background(), &ImportResourcesRequest{
		Kind:      "template",
		Scope:     "project",
		ScopeID:   "proj-1",
		SourceURL: "https://github.com/org/repo/tree/main/.scion/templates",
	})
	if err == nil {
		t.Fatal("expected error for HTTP 400 import_failed, got nil")
	}
	var apiErr *apiclient.APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("expected *apiclient.APIError, got %T: %v", err, err)
	}
	if apiErr.StatusCode != http.StatusBadRequest || apiErr.Code != "import_failed" {
		t.Errorf("apiErr = {StatusCode:%d Code:%q}, want {400 import_failed}", apiErr.StatusCode, apiErr.Code)
	}
}

func TestTemplateService_DiscoverResources_SuccessAndMarshal(t *testing.T) {
	var gotPath, gotMethod string
	var gotBody map[string]any

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotMethod = r.Method
		if err := json.NewDecoder(r.Body).Decode(&gotBody); err != nil {
			t.Errorf("decode request body: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"resources": []string{"alpha", "beta"},
			"skipped":   []string{"README"},
			"count":     2,
		})
	}))
	defer srv.Close()

	c, err := New(srv.URL)
	if err != nil {
		t.Fatalf("New client: %v", err)
	}

	resp, err := c.Templates().DiscoverResources(context.Background(), &DiscoverResourcesRequest{
		Kind:      "template",
		Scope:     "project",
		ScopeID:   "proj-123",
		SourceURL: "https://github.com/org/repo/tree/main/.scion/templates",
	})
	if err != nil {
		t.Fatalf("DiscoverResources failed: %v", err)
	}

	if gotMethod != http.MethodPost {
		t.Errorf("method = %q, want POST", gotMethod)
	}
	if gotPath != "/api/v1/resources/discover" {
		t.Errorf("path = %q, want /api/v1/resources/discover", gotPath)
	}
	wantKeys := map[string]any{
		"kind":      "template",
		"scope":     "project",
		"scopeId":   "proj-123",
		"sourceUrl": "https://github.com/org/repo/tree/main/.scion/templates",
	}
	if !reflect.DeepEqual(gotBody, wantKeys) {
		t.Errorf("request body = %#v, want %#v", gotBody, wantKeys)
	}
	if resp.Count != 2 || !reflect.DeepEqual(resp.Resources, []string{"alpha", "beta"}) || !reflect.DeepEqual(resp.Skipped, []string{"README"}) {
		t.Errorf("unexpected response: %+v", resp)
	}
}

func TestTemplateService_DiscoverResources_OmitEmptyScopeID(t *testing.T) {
	var gotBody map[string]any

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&gotBody); err != nil {
			t.Errorf("decode request body: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"resources": []string{"alpha"},
			"count":     1,
		})
	}))
	defer srv.Close()

	c, err := New(srv.URL)
	if err != nil {
		t.Fatalf("New client: %v", err)
	}

	_, err = c.Templates().DiscoverResources(context.Background(), &DiscoverResourcesRequest{
		Kind:      "template",
		Scope:     "global",
		SourceURL: "https://github.com/org/repo/tree/main/.scion/templates",
	})
	if err != nil {
		t.Fatalf("DiscoverResources failed: %v", err)
	}
	if _, ok := gotBody["scopeId"]; ok {
		t.Errorf("scopeId should be omitted when empty, got %v", gotBody["scopeId"])
	}
}
