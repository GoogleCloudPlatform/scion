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

package apiclient

import (
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
)

func newTestResponse(status int, body string) *http.Response {
	return &http.Response{
		StatusCode: status,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(strings.NewReader(body)),
	}
}

func TestDecodeRequired(t *testing.T) {
	type resource struct {
		ID string `json:"id"`
	}

	tests := []struct {
		name        string
		status      int
		body        string
		wantID      string
		wantErr     string
		wantNoBody  bool
		wantAPIErr  bool
		wantDecoded bool
	}{
		{name: "200 with body", status: http.StatusOK, body: `{"id":"r1"}`, wantID: "r1", wantDecoded: true},
		{name: "201 with body", status: http.StatusCreated, body: `{"id":"r2"}`, wantID: "r2", wantDecoded: true},
		{name: "204", status: http.StatusNoContent, wantErr: "server returned no content (status: 204)", wantNoBody: true},
		{name: "empty 200", status: http.StatusOK, wantErr: "server returned no content (status: 200)", wantNoBody: true},
		{name: "malformed 200", status: http.StatusOK, body: `{`, wantErr: "failed to decode response: unexpected EOF"},
		{name: "404", status: http.StatusNotFound, body: `{"error":{"code":"not_found","message":"gone"}}`, wantAPIErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := DecodeRequired[resource](newTestResponse(tt.status, tt.body))
			if tt.wantDecoded {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				if got == nil || got.ID != tt.wantID {
					t.Fatalf("result = %+v, want id %q", got, tt.wantID)
				}
				return
			}
			if err == nil {
				t.Fatalf("expected an error, got result %+v", got)
			}
			if got != nil {
				t.Errorf("expected nil result, got %+v", got)
			}
			if errors.Is(err, ErrNoContent) != tt.wantNoBody {
				t.Errorf("errors.Is(err, ErrNoContent) = %v, want %v (err: %v)", !tt.wantNoBody, tt.wantNoBody, err)
			}
			var apiErr *APIError
			if errors.As(err, &apiErr) != tt.wantAPIErr {
				t.Errorf("errors.As(err, *APIError) = %v, want %v (err: %v)", !tt.wantAPIErr, tt.wantAPIErr, err)
			}
			if tt.wantErr != "" && err.Error() != tt.wantErr {
				t.Errorf("error = %q, want %q", err.Error(), tt.wantErr)
			}
		})
	}
}

// TestDecodeResponse_NoContentUnchanged pins that DecodeResponse still
// returns (nil, nil) on a 204, for callers that treat no body as valid.
func TestDecodeResponse_NoContentUnchanged(t *testing.T) {
	got, err := DecodeResponse[struct{}](newTestResponse(http.StatusNoContent, ""))
	if err != nil || got != nil {
		t.Fatalf("DecodeResponse on 204 = (%v, %v), want (nil, nil)", got, err)
	}
}
