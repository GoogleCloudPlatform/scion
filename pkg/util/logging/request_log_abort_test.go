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

package logging

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestRequestLogMiddleware_AbortedResponse: a handler that aborts its
// response (panic with http.ErrAbortHandler) is still logged, marked
// aborted, and the abort reaches the server; any other panic passes
// through untouched and a normal request carries no aborted mark.
func TestRequestLogMiddleware_AbortedResponse(t *testing.T) {
	for _, tc := range []struct {
		name        string
		handler     http.HandlerFunc
		wantPanic   any
		wantLogged  bool
		wantAborted bool
	}{
		{
			name: "aborted mid-stream",
			handler: func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = io.WriteString(w, "data: 1\n\n")
				panic(http.ErrAbortHandler)
			},
			wantPanic:   http.ErrAbortHandler,
			wantLogged:  true,
			wantAborted: true,
		},
		{
			name:      "other panic passes through",
			handler:   func(http.ResponseWriter, *http.Request) { panic("boom") },
			wantPanic: "boom",
		},
		{
			name:       "completed",
			handler:    func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, "ok") },
			wantLogged: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			logger := slog.New(slog.NewJSONHandler(&buf, nil))
			h := RequestLogMiddleware(logger, "hub", nil, 0)(tc.handler)
			serve := func() {
				h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/api/v1/x", nil))
			}
			if tc.wantPanic != nil {
				assert.PanicsWithValue(t, tc.wantPanic, serve)
			} else {
				assert.NotPanics(t, serve)
			}
			if !tc.wantLogged {
				assert.Zero(t, buf.Len(), "a non-abort panic is not logged here")
				return
			}
			var line map[string]any
			require.NoError(t, json.Unmarshal(buf.Bytes(), &line), buf.String())
			aborted, ok := line[AttrAborted]
			assert.Equal(t, tc.wantAborted, ok, buf.String())
			if tc.wantAborted {
				assert.Equal(t, true, aborted)
			}
		})
	}
}
