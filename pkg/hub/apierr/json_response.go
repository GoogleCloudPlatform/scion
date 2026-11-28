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

package apierr

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
)

// DefaultMaxJSONBodyBytes is the default request body size bound (1 MiB)
// applied by ReadJSON and DecodeJSON. Endpoints that legitimately accept
// larger JSON payloads must opt in via ReadJSONWithLimit or
// DecodeJSONWithLimit.
const DefaultMaxJSONBodyBytes int64 = 1 << 20

const defaultBodyTooLargeMsg = "request body exceeds maximum size"

// WriteJSON writes a JSON response.
func WriteJSON(w http.ResponseWriter, statusCode int, data interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(statusCode)
	_ = json.NewEncoder(w).Encode(data)
}

// IsMaxBytesError reports whether err (or any wrapped error) is an
// *http.MaxBytesError returned when a request body exceeds its limit.
func IsMaxBytesError(err error) bool {
	var maxBytesErr *http.MaxBytesError
	return errors.As(err, &maxBytesErr)
}

// ReadJSON reads JSON from the request body with DefaultMaxJSONBodyBytes
// enforced via http.MaxBytesReader.
func ReadJSON(r *http.Request, v interface{}) error {
	return ReadJSONWithLimit(nil, r, DefaultMaxJSONBodyBytes, v)
}

// ReadJSONWithLimit caps r.Body at maxBytes via http.MaxBytesReader and
// decodes JSON into v. When maxBytes <= 0, DefaultMaxJSONBodyBytes is used.
func ReadJSONWithLimit(w http.ResponseWriter, r *http.Request, maxBytes int64, v interface{}) error {
	if r.Body == nil {
		return fmt.Errorf("empty request body")
	}
	if maxBytes <= 0 {
		maxBytes = DefaultMaxJSONBodyBytes
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxBytes)
	return json.NewDecoder(r.Body).Decode(v)
}

// DecodeJSON decodes a JSON request body bounded by DefaultMaxJSONBodyBytes.
// On failure it writes 413 payload_too_large (for *http.MaxBytesError) or
// 400 invalid_request and returns false.
func DecodeJSON(w http.ResponseWriter, r *http.Request, v interface{}) bool {
	return DecodeJSONWithLimit(w, r, DefaultMaxJSONBodyBytes, defaultBodyTooLargeMsg, v)
}

// DecodeJSONWithLimit decodes a JSON request body bounded by maxBytes. On
// failure it writes 413 payload_too_large with tooLargeMsg (or a default
// message if empty) for *http.MaxBytesError, or 400 invalid_request for other
// decode errors, and returns false.
func DecodeJSONWithLimit(w http.ResponseWriter, r *http.Request, maxBytes int64, tooLargeMsg string, v interface{}) bool {
	if err := ReadJSONWithLimit(w, r, maxBytes, v); err != nil {
		if IsMaxBytesError(err) {
			if tooLargeMsg == "" {
				tooLargeMsg = defaultBodyTooLargeMsg
			}
			WriteError(w, http.StatusRequestEntityTooLarge, "payload_too_large", tooLargeMsg, nil)
			return false
		}
		BadRequest(w, "Invalid request body: "+err.Error())
		return false
	}
	return true
}
