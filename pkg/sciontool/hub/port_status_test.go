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

func TestPortMethods_ReturnStatusError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "nope", http.StatusUnauthorized)
	}))
	defer srv.Close()
	c := NewClientWithConfig(srv.URL, "agent-credential", "agent-1")
	ctx := context.Background()

	_, listErr := c.ListPorts(ctx)
	_, regErr := c.RegisterPort(ctx, RegisterPortRequest{Port: 3000})
	delErr := c.DeletePort(ctx, 3000)
	for name, err := range map[string]error{"ListPorts": listErr, "RegisterPort": regErr, "DeletePort": delErr} {
		if !IsUnauthorized(err) {
			t.Errorf("%s: IsUnauthorized(%v) = false, want true", name, err)
		}
		if want := "hub returned error 401: nope\n"; err == nil || err.Error() != want {
			t.Errorf("%s: error = %q, want %q", name, err, want)
		}
	}
}

func TestIsUnauthorized(t *testing.T) {
	tests := []struct {
		err  error
		want bool
	}{
		{nil, false},
		{errors.New("hub returned error 401: x"), false},
		{&StatusError{StatusCode: http.StatusForbidden}, false},
		{&StatusError{StatusCode: http.StatusUnauthorized}, true},
		{fmt.Errorf("wrapped: %w", &StatusError{StatusCode: http.StatusUnauthorized}), true},
	}
	for _, tt := range tests {
		if got := IsUnauthorized(tt.err); got != tt.want {
			t.Errorf("IsUnauthorized(%v) = %v, want %v", tt.err, got, tt.want)
		}
	}
}
