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
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Nested request loggers (ptone/scion#7367): the combined web and hub
// server wraps the hub chain, which has its own RequestLogMiddleware, in
// the web chain's RequestLogMiddleware built with DeferToNestedRequestLog.

func newNestedTestLogger() (*slog.Logger, *bytes.Buffer) {
	var buf bytes.Buffer
	return slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})), &buf
}

func requestLogLines(t *testing.T, buf *bytes.Buffer) []map[string]any {
	t.Helper()
	var lines []map[string]any
	sc := bufio.NewScanner(bytes.NewReader(buf.Bytes()))
	for sc.Scan() {
		var entry map[string]any
		require.NoError(t, json.Unmarshal(sc.Bytes(), &entry), "raw: %s", sc.Text())
		lines = append(lines, entry)
	}
	require.NoError(t, sc.Err())
	return lines
}

func lineStatus(t *testing.T, entry map[string]any) float64 {
	t.Helper()
	httpReq, ok := entry["httpRequest"].(map[string]any)
	require.True(t, ok, "missing httpRequest group: %v", entry)
	status, _ := httpReq["status"].(float64)
	return status
}

// TestRequestLogMiddleware_DeferToNested_OneLineWithInnerAuth: a request
// that reaches the nested logger is logged once, by the nested logger,
// with the auth attribution set inside it, the inner status, and the
// request ID the response carries.
func TestRequestLogMiddleware_DeferToNested_OneLineWithInnerAuth(t *testing.T) {
	logger, buf := newNestedTestLogger()
	inner := RequestLogMiddleware(logger, "hub", HubPathPatterns(), 0)(
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			SetRequestAuth(r.Context(), "uat", slog.String("user_id", "user-1"))
			w.WriteHeader(http.StatusCreated)
			_, _ = io.WriteString(w, "created")
		}),
	)
	outer := RequestLogMiddleware(logger, "web", nil, 0, DeferToNestedRequestLog())(inner)

	rec := httptest.NewRecorder()
	outer.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/v1/projects/p1/agents", nil))

	lines := requestLogLines(t, buf)
	require.Len(t, lines, 1, "want exactly one request-log line: %s", buf.String())
	line := lines[0]
	assert.Equal(t, "hub", line["component"])
	assert.Equal(t, "uat", line["auth_type"])
	assert.Equal(t, "user-1", line["user_id"])
	assert.Equal(t, "p1", line["project_id"])
	assert.Equal(t, float64(http.StatusCreated), lineStatus(t, line))
	assert.Equal(t, rec.Header().Get("X-Request-ID"), line["request_id"])
}

// TestRequestLogMiddleware_DeferToNested_OuterChainRejects: a request the
// outer chain answers before any nested logger runs is logged by the
// deferring logger, with the status the outer chain wrote.
func TestRequestLogMiddleware_DeferToNested_OuterChainRejects(t *testing.T) {
	logger, buf := newNestedTestLogger()
	innerRan := false
	inner := RequestLogMiddleware(logger, "hub", HubPathPatterns(), 0)(
		http.HandlerFunc(func(http.ResponseWriter, *http.Request) { innerRan = true }),
	)
	reject := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") == "" {
			http.Error(w, "proxy authentication failed", http.StatusUnauthorized)
			return
		}
		inner.ServeHTTP(w, r)
	})
	outer := RequestLogMiddleware(logger, "web", nil, 0, DeferToNestedRequestLog())(reject)

	rec := httptest.NewRecorder()
	outer.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/projects/p1", nil))

	require.False(t, innerRan)
	lines := requestLogLines(t, buf)
	require.Len(t, lines, 1, "want exactly one request-log line: %s", buf.String())
	assert.Equal(t, "web", lines[0]["component"])
	assert.Equal(t, float64(http.StatusUnauthorized), lineStatus(t, lines[0]))
	assert.Equal(t, rec.Header().Get("X-Request-ID"), lines[0]["request_id"])
}

// TestRequestLogMiddleware_NestedWithoutDefer_BothLog pins the hub's own
// re-entry (conduit relay admission runs the hub chain again on a request
// derived from the outer one, for a different principal): without
// DeferToNestedRequestLog both loggers log, with distinct request IDs, and
// the inner chain's auth does not overwrite the outer line's.
func TestRequestLogMiddleware_NestedWithoutDefer_BothLog(t *testing.T) {
	logger, buf := newNestedTestLogger()
	inner := RequestLogMiddleware(logger, "hub", HubPathPatterns(), 0)(
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			SetRequestAuth(r.Context(), "uat", slog.String("user_id", "dialer"))
			w.WriteHeader(http.StatusOK)
		}),
	)
	outer := RequestLogMiddleware(logger, "hub", HubPathPatterns(), 0)(
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			SetRequestAuth(r.Context(), "relay", slog.String("user_id", "relay-peer"))
			fwd, err := http.NewRequestWithContext(r.Context(), http.MethodGet, "http://hub.example/api/v1/conduit", nil)
			require.NoError(t, err)
			inner.ServeHTTP(w, fwd)
		}),
	)

	outer.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/api/v1/relay/admit", nil))

	lines := requestLogLines(t, buf)
	require.Len(t, lines, 2, "want both request-log lines: %s", buf.String())
	innerLine, outerLine := lines[0], lines[1]
	assert.NotEqual(t, innerLine["request_id"], outerLine["request_id"])
	assert.Equal(t, "uat", innerLine["auth_type"])
	assert.Equal(t, "dialer", innerLine["user_id"])
	assert.Equal(t, "relay", outerLine["auth_type"])
	assert.Equal(t, "relay-peer", outerLine["user_id"])
}

// TestRequestLogMiddleware_DeferToNested_OneLevelOnly: deferral reaches
// only the logger directly enclosing the one that logged. With a deferring
// logger around the hub chain, and the hub chain re-entered inside itself,
// the two hub lines stay and only the deferring logger is silent.
func TestRequestLogMiddleware_DeferToNested_OneLevelOnly(t *testing.T) {
	logger, buf := newNestedTestLogger()
	innermost := RequestLogMiddleware(logger, "hub", nil, 0)(
		http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) }),
	)
	middle := RequestLogMiddleware(logger, "hub", nil, 0)(innermost)
	outer := RequestLogMiddleware(logger, "web", nil, 0, DeferToNestedRequestLog())(middle)

	outer.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/api/v1/conduit", nil))

	lines := requestLogLines(t, buf)
	require.Len(t, lines, 2, "want the two hub lines only: %s", buf.String())
	for _, line := range lines {
		assert.Equal(t, "hub", line["component"])
	}
	assert.NotEqual(t, lines[0]["request_id"], lines[1]["request_id"])
}

// TestRequestLogMiddleware_DeferToNested_Aborted: a response aborted
// inside the nested chain is logged once, marked aborted, and the abort
// still reaches the server.
func TestRequestLogMiddleware_DeferToNested_Aborted(t *testing.T) {
	logger, buf := newNestedTestLogger()
	inner := RequestLogMiddleware(logger, "hub", nil, 0)(
		http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = io.WriteString(w, "data: 1\n\n")
			panic(http.ErrAbortHandler)
		}),
	)
	outer := RequestLogMiddleware(logger, "web", nil, 0, DeferToNestedRequestLog())(inner)

	var recovered any
	func() {
		defer func() { recovered = recover() }()
		outer.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/api/v1/events", nil))
	}()

	assert.Equal(t, http.ErrAbortHandler, recovered)
	lines := requestLogLines(t, buf)
	require.Len(t, lines, 1, "want exactly one request-log line: %s", buf.String())
	assert.Equal(t, "hub", lines[0]["component"])
	assert.Equal(t, true, lines[0][AttrAborted])
}

// TestRequestLogMiddleware_DeferToNested_SingleLayerUnchanged: with no
// nested logger, a logger behaves the same with or without the option.
func TestRequestLogMiddleware_DeferToNested_SingleLayerUnchanged(t *testing.T) {
	for _, tc := range []struct {
		name string
		opts []RequestLogOption
	}{
		{name: "without option"},
		{name: "with option", opts: []RequestLogOption{DeferToNestedRequestLog()}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			logger, buf := newNestedTestLogger()
			h := RequestLogMiddleware(logger, "web", nil, 0, tc.opts...)(
				http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					SetRequestAuth(r.Context(), "session")
					w.WriteHeader(http.StatusNotFound)
				}),
			)

			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/missing", nil))

			lines := requestLogLines(t, buf)
			require.Len(t, lines, 1, "want exactly one request-log line: %s", buf.String())
			assert.Equal(t, "web", lines[0]["component"])
			assert.Equal(t, "session", lines[0]["auth_type"])
			assert.Equal(t, float64(http.StatusNotFound), lineStatus(t, lines[0]))
			assert.Equal(t, rec.Header().Get("X-Request-ID"), lines[0]["request_id"])
		})
	}
}

// TestRequestLogMiddleware_DeferToNested_OneSlowRequestLine: the
// slow-request line follows the request line. A deferring logger whose
// nested logger has logged the request writes no slow-request line of its
// own; the nested logger writes the only one. The outer chain strips a
// path prefix, so the two loggers would report different paths.
func TestRequestLogMiddleware_DeferToNested_OneSlowRequestLine(t *testing.T) {
	var slowBuf bytes.Buffer
	orig := slog.Default()
	t.Cleanup(func() { slog.SetDefault(orig) })
	slog.SetDefault(slog.New(slog.NewJSONHandler(&slowBuf, nil)))

	logger, _ := newNestedTestLogger()
	inner := RequestLogMiddleware(logger, "hub", nil, time.Nanosecond)(
		http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			time.Sleep(time.Millisecond)
			w.WriteHeader(http.StatusOK)
		}),
	)
	outer := RequestLogMiddleware(logger, "web", nil, time.Nanosecond, DeferToNestedRequestLog())(
		http.StripPrefix("/web", inner),
	)

	outer.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/web/api/v1/slow", nil))

	var slow []map[string]any
	for _, line := range requestLogLines(t, &slowBuf) {
		if line["msg"] == "Slow request" {
			slow = append(slow, line)
		}
	}
	require.Len(t, slow, 1, "want exactly one slow-request line: %s", slowBuf.String())
	assert.Equal(t, "/api/v1/slow", slow[0]["path"], "the slow-request line must be the nested logger's")
}
