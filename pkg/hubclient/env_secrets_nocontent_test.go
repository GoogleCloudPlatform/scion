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
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/apiclient"
)

// newNoContentServer answers GET path with 204 and no body.
func newNoContentServer(t *testing.T, path string) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != path {
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(server.Close)
	return server
}

func TestEnvGet_NoContent(t *testing.T) {
	server := newNoContentServer(t, "/api/v1/env/GONE")
	client, err := New(server.URL)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	envVar, err := client.Env().Get(t.Context(), "GONE", nil)
	if err == nil {
		t.Fatalf("expected an error for a 204 response, got %+v", envVar)
	}
	if envVar != nil {
		t.Errorf("expected nil env var, got %+v", envVar)
	}
	if want := `hub returned no content for "GONE"`; err.Error() != want {
		t.Errorf("error = %q, want %q", err.Error(), want)
	}
	if !errors.Is(err, apiclient.ErrNoContent) {
		t.Errorf("error %v should wrap apiclient.ErrNoContent", err)
	}
}

func TestSecretGet_NoContent(t *testing.T) {
	server := newNoContentServer(t, "/api/v1/secrets/GONE")
	client, err := New(server.URL)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	secret, err := client.Secrets().Get(t.Context(), "GONE", nil)
	if err == nil {
		t.Fatalf("expected an error for a 204 response, got %+v", secret)
	}
	if secret != nil {
		t.Errorf("expected nil secret, got %+v", secret)
	}
	if want := `hub returned no content for "GONE"`; err.Error() != want {
		t.Errorf("error = %q, want %q", err.Error(), want)
	}
	if !errors.Is(err, apiclient.ErrNoContent) {
		t.Errorf("error %v should wrap apiclient.ErrNoContent", err)
	}
}
