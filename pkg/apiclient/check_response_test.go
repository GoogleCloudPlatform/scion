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
	"net/http"
	"strconv"
	"strings"
	"testing"
)

type checkedResponseBody struct {
	*strings.Reader
	closeCalls int
}

func (b *checkedResponseBody) Close() error {
	b.closeCalls++
	return nil
}

func TestCheckResponse_Status(t *testing.T) {
	for _, tt := range []struct {
		status  int
		wantErr bool
	}{
		{http.StatusSwitchingProtocols, true},
		{199, true},
		{http.StatusOK, false},
		{http.StatusCreated, false},
		{http.StatusNoContent, false},
		{299, false},
		{http.StatusMultipleChoices, true},
		{http.StatusFound, true},
		{http.StatusNotModified, true},
		{http.StatusTemporaryRedirect, true},
		{399, true},
		{http.StatusBadRequest, true},
		{http.StatusServiceUnavailable, true},
	} {
		t.Run(strconv.Itoa(tt.status), func(t *testing.T) {
			body := &checkedResponseBody{Reader: strings.NewReader(`{"error":{"code":"operation_failed","message":"request not completed"}}`)}
			resp := &http.Response{
				StatusCode: tt.status,
				Header:     http.Header{"X-Request-Id": {"request-123"}},
				Body:       body,
			}
			err := CheckResponse(resp)
			if body.closeCalls != 1 {
				t.Errorf("response body closed %d times, want 1", body.closeCalls)
			}
			if !tt.wantErr {
				if err != nil {
					t.Fatalf("successful response returned an error: %v", err)
				}
				return
			}
			var apiErr *APIError
			if !errors.As(err, &apiErr) {
				t.Fatalf("expected an APIError for status %d, got %v", tt.status, err)
			}
			if apiErr.StatusCode != tt.status || apiErr.RequestID != "request-123" ||
				apiErr.Code != "operation_failed" || apiErr.Message != "request not completed" {
				t.Errorf("response details not preserved: %+v", apiErr)
			}
		})
	}
}

func TestCheckResponse_NilResponse(t *testing.T) {
	if err := CheckResponse(nil); err == nil {
		t.Error("CheckResponse(nil) want error, got nil")
	}
}

func TestCheckResponse_NilBody(t *testing.T) {
	resp200 := &http.Response{StatusCode: http.StatusOK}
	if err := CheckResponse(resp200); err != nil {
		t.Errorf("CheckResponse(200 with nil body) = %v, want nil", err)
	}

	resp500 := &http.Response{StatusCode: http.StatusInternalServerError}
	if err := CheckResponse(resp500); err == nil {
		t.Error("CheckResponse(500 with nil body) want error, got nil")
	}
}
