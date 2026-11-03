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

package hubtestutil

import (
	"encoding/json"
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/stretchr/testify/require"
)

// DecodeJSONBody unmarshals the recorded response body into v, failing the
// test on a decode error.
func DecodeJSONBody(t *testing.T, rec *httptest.ResponseRecorder, v interface{}) {
	t.Helper()
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), v), rec.Body.String())
}

// DecodeObject unmarshals body as a JSON object of raw field values.
func DecodeObject(t *testing.T, body []byte) map[string]json.RawMessage {
	t.Helper()
	var m map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(body, &m), string(body))
	return m
}

// DecodeItems unmarshals raw as a JSON array of objects of raw field values.
func DecodeItems(t *testing.T, raw json.RawMessage) []map[string]json.RawMessage {
	t.Helper()
	var items []map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(raw, &items))
	return items
}

// ErrorBody decodes the standard error envelope.
func ErrorBody(t *testing.T, rec *httptest.ResponseRecorder) (string, map[string]interface{}) {
	t.Helper()
	var body struct {
		Error struct {
			Code    string                 `json:"code"`
			Details map[string]interface{} `json:"details"`
		} `json:"error"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body), rec.Body.String())
	return body.Error.Code, body.Error.Details
}

// DecodeErrorMessage returns the message of the standard error envelope in
// body.
func DecodeErrorMessage(t *testing.T, body []byte) string {
	t.Helper()
	var e struct {
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	require.NoError(t, json.Unmarshal(body, &e))
	return e.Error.Message
}

// DecodeAPIError unmarshals body as an APIErrorBody.
func DecodeAPIError(t *testing.T, body []byte) APIErrorBody {
	t.Helper()
	var resp APIErrorBody
	require.NoError(t, json.Unmarshal(body, &resp))
	return resp
}

// APIErrorBody mirrors the JSON shape written by pkg/hub's writeError, for assertions
// on the error code and message returned to the client.
type APIErrorBody struct {
	Error struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

// WithCursor appends a query-escaped cursor parameter to query.
func WithCursor(query, cursor string) string {
	return query + "&cursor=" + url.QueryEscape(cursor)
}
