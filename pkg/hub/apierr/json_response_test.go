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
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func exactJSONPayload(t *testing.T, targetBytes int) []byte {
	t.Helper()
	const prefix = `{"data":"`
	const suffix = `"}`
	padLen := targetBytes - len(prefix) - len(suffix)
	if padLen < 0 {
		t.Fatalf("targetBytes %d too small for framing", targetBytes)
	}
	out := make([]byte, targetBytes)
	copy(out, prefix)
	for i := len(prefix); i < targetBytes-len(suffix); i++ {
		out[i] = 'a'
	}
	copy(out[targetBytes-len(suffix):], suffix)
	return out
}

func TestReadJSON_DefaultBound(t *testing.T) {
	type payload struct {
		Data string `json:"data"`
	}

	t.Run("exact default limit succeeds", func(t *testing.T) {
		raw := exactJSONPayload(t, int(DefaultMaxJSONBodyBytes))
		req := httptest.NewRequest(http.MethodPost, "/", bytes.NewReader(raw))
		var dst payload
		if err := ReadJSON(req, &dst); err != nil {
			t.Fatalf("ReadJSON at exact default limit failed: %v", err)
		}
		if len(dst.Data) == 0 {
			t.Fatal("expected decoded data")
		}
	})

	t.Run("one byte over default limit returns MaxBytesError", func(t *testing.T) {
		raw := exactJSONPayload(t, int(DefaultMaxJSONBodyBytes)+1)
		req := httptest.NewRequest(http.MethodPost, "/", bytes.NewReader(raw))
		var dst payload
		err := ReadJSON(req, &dst)
		if err == nil {
			t.Fatal("expected error for body exceeding DefaultMaxJSONBodyBytes")
		}
		if !IsMaxBytesError(err) {
			t.Fatalf("expected *http.MaxBytesError, got %T: %v", err, err)
		}
	})

	t.Run("nil body returns error", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodPost, "/", nil)
		req.Body = nil
		var dst payload
		if err := ReadJSON(req, &dst); err == nil {
			t.Fatal("expected error for nil body")
		}
	})
}

func TestReadJSONWithLimit(t *testing.T) {
	type payload struct {
		Data string `json:"data"`
	}

	const customLimit = 256

	t.Run("exact custom limit succeeds", func(t *testing.T) {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/", bytes.NewReader(exactJSONPayload(t, customLimit)))
		var dst payload
		if err := ReadJSONWithLimit(rec, req, customLimit, &dst); err != nil {
			t.Fatalf("ReadJSONWithLimit at exact limit failed: %v", err)
		}
	})

	t.Run("one byte over custom limit returns MaxBytesError", func(t *testing.T) {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/", bytes.NewReader(exactJSONPayload(t, customLimit+1)))
		var dst payload
		err := ReadJSONWithLimit(rec, req, customLimit, &dst)
		if !IsMaxBytesError(err) {
			t.Fatalf("expected *http.MaxBytesError, got %T: %v", err, err)
		}
	})

	t.Run("non-positive limit falls back to DefaultMaxJSONBodyBytes", func(t *testing.T) {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/", bytes.NewReader(exactJSONPayload(t, int(DefaultMaxJSONBodyBytes)+1)))
		var dst payload
		err := ReadJSONWithLimit(rec, req, 0, &dst)
		if !IsMaxBytesError(err) {
			t.Fatalf("expected *http.MaxBytesError for fallback limit, got %T: %v", err, err)
		}
	})
}

func TestDecodeJSONWithLimit(t *testing.T) {
	type payload struct {
		Data string `json:"data"`
	}

	const customLimit = 512

	t.Run("exact limit succeeds", func(t *testing.T) {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/", bytes.NewReader(exactJSONPayload(t, customLimit)))
		var dst payload
		if !DecodeJSONWithLimit(rec, req, customLimit, "too big", &dst) {
			t.Fatalf("expected decode to succeed, got status %d: %s", rec.Code, rec.Body.String())
		}
	})

	t.Run("over limit writes 413 payload_too_large", func(t *testing.T) {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/", bytes.NewReader(exactJSONPayload(t, customLimit+1)))
		var dst payload
		if DecodeJSONWithLimit(rec, req, customLimit, "custom too large message", &dst) {
			t.Fatal("expected decode to fail over limit")
		}
		if rec.Code != http.StatusRequestEntityTooLarge {
			t.Fatalf("status = %d, want 413: %s", rec.Code, rec.Body.String())
		}
		var env struct {
			Error struct {
				Code    string `json:"code"`
				Message string `json:"message"`
			} `json:"error"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
			t.Fatalf("invalid error JSON: %v", err)
		}
		if env.Error.Code != "payload_too_large" {
			t.Fatalf("code = %q, want payload_too_large", env.Error.Code)
		}
		if env.Error.Message != "custom too large message" {
			t.Fatalf("message = %q, want custom too large message", env.Error.Message)
		}
	})

	t.Run("malformed JSON writes 400 invalid_request", func(t *testing.T) {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/", bytes.NewBufferString("{bad"))
		var dst payload
		if DecodeJSON(rec, req, &dst) {
			t.Fatal("expected malformed JSON to fail")
		}
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400: %s", rec.Code, rec.Body.String())
		}
	})
}
