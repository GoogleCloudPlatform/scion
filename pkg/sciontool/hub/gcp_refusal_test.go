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
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
)

// The GCP credential fetches return a StatusError, so the metadata server
// can tell a hub refusal (403) from other failures (ptone/scion#6436).
func TestGCPFetches_ReturnStatusError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "refused", http.StatusForbidden)
	}))
	defer srv.Close()
	c := NewClientWithConfig(srv.URL, "agent-credential", "agent-1")
	ctx := context.Background()

	_, tokErr := c.FetchGCPToken(ctx, []string{"https://www.googleapis.com/auth/cloud-platform"})
	_, idErr := c.FetchGCPIdentityToken(ctx, "aud")
	for name, err := range map[string]error{"FetchGCPToken": tokErr, "FetchGCPIdentityToken": idErr} {
		if !IsForbidden(err) {
			t.Errorf("%s: IsForbidden(%v) = false, want true", name, err)
		}
		var sc interface{ HTTPStatusCode() int }
		if !errors.As(err, &sc) || sc.HTTPStatusCode() != http.StatusForbidden {
			t.Errorf("%s: HTTPStatusCode not 403: %v", name, err)
		}
		if want := "hub returned error 403: refused\n"; err == nil || err.Error() != want {
			t.Errorf("%s: error = %q, want %q", name, err, want)
		}
	}
}

func TestIsForbidden(t *testing.T) {
	tests := []struct {
		err  error
		want bool
	}{
		{nil, false},
		{errors.New("hub returned error 403: x"), false},
		{&StatusError{StatusCode: http.StatusUnauthorized}, false},
		{&StatusError{StatusCode: http.StatusForbidden}, true},
		{fmt.Errorf("wrapped: %w", &StatusError{StatusCode: http.StatusForbidden}), true},
	}
	for _, tt := range tests {
		if got := IsForbidden(tt.err); got != tt.want {
			t.Errorf("IsForbidden(%v) = %v, want %v", tt.err, got, tt.want)
		}
	}
}
