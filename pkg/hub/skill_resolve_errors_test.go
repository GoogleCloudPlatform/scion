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

//go:build !no_sqlite

package hub

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/store/enttest"
	"github.com/GoogleCloudPlatform/scion/pkg/util/logging"
)

// Skill resolve errors return a fixed reason phrase, the code, the
// upstream status and a correlation id; the full error is in the
// server-side log record with the same id (ptone/scion#5079). These tests
// read the response as JSON and use literal phrases, so they exercise the
// wire contract.

// skillResolveLogMsg is the message of the log record holding the full
// error of a skill resolve failure.
const skillResolveLogMsg = "Skill resolve failed"

// captureJSONSlog swaps slog's default logger for a JSON handler and
// returns a function that reads the records written so far. The previous
// default is restored on test cleanup.
func captureJSONSlog(t *testing.T) func() []map[string]any {
	t.Helper()
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return func() []map[string]any {
		var recs []map[string]any
		sc := bufio.NewScanner(bytes.NewReader(buf.Bytes()))
		sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
		for sc.Scan() {
			var rec map[string]any
			if err := json.Unmarshal(sc.Bytes(), &rec); err != nil {
				t.Fatalf("log line is not JSON: %v: %s", err, sc.Text())
			}
			recs = append(recs, rec)
		}
		return recs
	}
}

// postSkillsResolve calls the batch resolve handler for uris with request
// id reqID on the request context, as the request log middleware sets it,
// and returns the raw text of the response and its decoded errors.
func postSkillsResolve(t *testing.T, srv *Server, reqID string, uris ...string) (string, []map[string]any) {
	t.Helper()
	refs := make([]map[string]string, len(uris))
	for i, u := range uris {
		refs[i] = map[string]string{"uri": u}
	}
	body, err := json.Marshal(map[string]any{"skills": refs})
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/api/v1/skills/resolve", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if reqID != "" {
		req = req.WithContext(logging.ContextWithRequestMeta(req.Context(), &logging.RequestMeta{RequestID: reqID}))
	}
	rr := httptest.NewRecorder()
	srv.handleSkillsResolve(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body %s", rr.Code, rr.Body.String())
	}
	var resp struct {
		Errors []map[string]any `json:"errors"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	return rr.Body.String(), resp.Errors
}

// wantResolveError is the expected per-skill error of a resolve response.
type wantResolveError struct {
	uri, code, message string
	status             int
}

// assertResolveError checks e is exactly want, with correlation id id:
// no other fields, and no status field when want.status is 0.
func assertResolveError(t *testing.T, e map[string]any, want wantResolveError, id string) {
	t.Helper()
	exp := map[string]any{
		"uri":           want.uri,
		"code":          want.code,
		"message":       want.message,
		"correlationId": id,
	}
	if want.status != 0 {
		exp["status"] = float64(want.status)
	}
	if len(e) != len(exp) {
		t.Errorf("error fields = %v, want %v", e, exp)
	}
	for k, v := range exp {
		if e[k] != v {
			t.Errorf("error %s = %v, want %v (error %v)", k, e[k], v, e)
		}
	}
}

// findResolveLog returns the one skill resolve failure record with
// correlation id id and code code.
func findResolveLog(t *testing.T, recs []map[string]any, id, code string) map[string]any {
	t.Helper()
	var found []map[string]any
	for _, r := range recs {
		if r["msg"] == skillResolveLogMsg && r["correlation_id"] == id && r["code"] == code {
			found = append(found, r)
		}
	}
	if len(found) != 1 {
		t.Fatalf("found %d %q records with correlation_id %q and code %q, want 1; records %v",
			len(found), skillResolveLogMsg, id, code, recs)
	}
	return found[0]
}

// addFederationRegistry adds a registry named name with endpoint, type,
// trust level and status.
func addFederationRegistry(t *testing.T, s store.Store, name, endpoint, typ, trust, status string) *store.SkillRegistry {
	t.Helper()
	reg := &store.SkillRegistry{
		Name:        name,
		Endpoint:    endpoint,
		Type:        typ,
		TrustLevel:  trust,
		ResolvePath: "/api/v1/skills/resolve",
		Status:      status,
	}
	if err := s.CreateSkillRegistry(t.Context(), reg); err != nil {
		t.Fatalf("create registry: %v", err)
	}
	return reg
}

func TestSkillsResolve_FederationErrorsReturnFixedPhrase(t *testing.T) {
	const marker = "zq-detail-5079"
	remoteBody := func(code, msg string, status int) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"resolved": []any{},
				"errors": []map[string]any{{
					"uri": "skill:///core/s@1.0", "code": code, "message": msg,
					"status": status, "correlationId": "remote-" + marker,
				}},
			})
		}
	}
	resolvedWithHash := func(hash string) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{"resolved": []map[string]any{{
				"uri": "skill:///core/s@1.0", "name": "s", "resolvedVersion": "1.0.0", "contentHash": hash,
			}}})
		}
	}
	cases := []struct {
		name    string
		handler http.HandlerFunc
		// setup adds the registry "reg" (or not) and returns the URI to resolve.
		setup   func(t *testing.T, s store.Store, endpoint string) string
		want    wantResolveError
		logText string // text the log record's error must hold
	}{
		{
			name: "registry not configured",
			setup: func(t *testing.T, s store.Store, endpoint string) string {
				return "skill://reg/core/s@1.0"
			},
			want:    wantResolveError{code: "unknown_registry", message: "skill registry not configured"},
			logText: `registry "reg" is not configured`,
		},
		{
			name: "registry disabled",
			setup: func(t *testing.T, s store.Store, endpoint string) string {
				addFederationRegistry(t, s, "reg", endpoint, "hub", "trusted", "disabled")
				return "skill://reg/core/s@1.0"
			},
			want:    wantResolveError{code: "registry_disabled", message: "skill registry disabled"},
			logText: `registry "reg" is disabled`,
		},
		{
			name: "wrong registry type",
			setup: func(t *testing.T, s store.Store, endpoint string) string {
				addFederationRegistry(t, s, "reg", endpoint, "gcp", "trusted", "active")
				return "skill://reg/core/s@1.0"
			},
			want:    wantResolveError{code: "wrong_registry_type", message: "unsupported skill registry type"},
			logText: `registry "reg" is type "gcp", not hub`,
		},
		{
			name: "registry returns a non-OK status",
			handler: func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusBadGateway)
				_, _ = w.Write([]byte("upstream page " + marker))
			},
			setup: func(t *testing.T, s store.Store, endpoint string) string {
				addFederationRegistry(t, s, "reg", endpoint, "hub", "trusted", "active")
				return "skill://reg/core/s@1.0"
			},
			want:    wantResolveError{code: "federation_error", message: "skill registry request failed", status: http.StatusBadGateway},
			logText: `registry "reg" returned 502: upstream page ` + marker,
		},
		{
			name: "registry response does not decode",
			handler: func(w http.ResponseWriter, r *http.Request) {
				_, _ = w.Write([]byte("not json " + marker))
			},
			setup: func(t *testing.T, s store.Store, endpoint string) string {
				addFederationRegistry(t, s, "reg", endpoint, "hub", "trusted", "active")
				return "skill://reg/core/s@1.0"
			},
			want:    wantResolveError{code: "federation_error", message: "skill registry request failed"},
			logText: `failed to decode response from registry "reg"`,
		},
		{
			name:    "registry reports a per-skill error",
			handler: remoteBody("not_found", "remote text "+marker, http.StatusNotFound),
			setup: func(t *testing.T, s store.Store, endpoint string) string {
				addFederationRegistry(t, s, "reg", endpoint, "hub", "trusted", "active")
				return "skill://reg/core/s@1.0"
			},
			want:    wantResolveError{code: "not_found", message: "skill not found", status: http.StatusNotFound},
			logText: `registry "reg": remote text ` + marker,
		},
		{
			name:    "registry reports an invalid code",
			handler: remoteBody("bad code "+marker, "remote text "+marker, 0),
			setup: func(t *testing.T, s store.Store, endpoint string) string {
				addFederationRegistry(t, s, "reg", endpoint, "hub", "trusted", "active")
				return "skill://reg/core/s@1.0"
			},
			want:    wantResolveError{code: "federation_error", message: "skill registry request failed"},
			logText: `registry "reg": remote text ` + marker,
		},
		{
			name:    "registry reports an unlisted code in the bounded form",
			handler: remoteBody("remote_specific_"+marker, "remote text "+marker, 0),
			setup: func(t *testing.T, s store.Store, endpoint string) string {
				addFederationRegistry(t, s, "reg", endpoint, "hub", "trusted", "active")
				return "skill://reg/core/s@1.0"
			},
			want:    wantResolveError{code: "remote_specific_" + marker, message: "resolver error"},
			logText: `registry "reg": remote text ` + marker,
		},
		{
			name: "registry resolves nothing",
			handler: func(w http.ResponseWriter, r *http.Request) {
				_, _ = w.Write([]byte(`{"resolved":[]}`))
			},
			setup: func(t *testing.T, s store.Store, endpoint string) string {
				addFederationRegistry(t, s, "reg", endpoint, "hub", "trusted", "active")
				return "skill://reg/core/s@1.0"
			},
			want:    wantResolveError{code: "not_found", message: "skill not found"},
			logText: `skill not found in registry "reg"`,
		},
		{
			name:    "no pinned hash",
			handler: resolvedWithHash("sha256:" + marker),
			setup: func(t *testing.T, s store.Store, endpoint string) string {
				addFederationRegistry(t, s, "reg", endpoint, "hub", "pinned", "active")
				return "skill://reg/core/s@1.0"
			},
			want:    wantResolveError{code: "trust_violation", message: "skill content hash not trusted"},
			logText: `no pinned hash for "skill://reg/core/s@1.0" in registry "reg"`,
		},
		{
			name:    "content hash mismatch",
			handler: resolvedWithHash("sha256:" + marker),
			setup: func(t *testing.T, s store.Store, endpoint string) string {
				reg := addFederationRegistry(t, s, "reg", endpoint, "hub", "pinned", "active")
				if err := s.PinSkillHash(t.Context(), reg.ID, "skill://reg/core/s@1.0", "sha256:pinned"); err != nil {
					t.Fatal(err)
				}
				return "skill://reg/core/s@1.0"
			},
			want:    wantResolveError{code: "trust_violation", message: "skill content hash not trusted"},
			logText: "expected sha256:pinned, got sha256:" + marker,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			handler := tc.handler
			if handler == nil {
				handler = func(w http.ResponseWriter, r *http.Request) {}
			}
			mock := newFederationMockRegistry(t, handler)
			srv, s := newFederationTestServer(t, mock)
			uri := tc.setup(t, s, mock.URL)
			logs := captureJSONSlog(t)

			const reqID = "req-5079-federation"
			raw, errs := postSkillsResolve(t, srv, reqID, uri)
			if len(errs) != 1 {
				t.Fatalf("errors = %v, want 1", errs)
			}
			want := tc.want
			want.uri = uri
			assertResolveError(t, errs[0], want, reqID)
			if strings.Contains(raw, marker) && !strings.Contains(tc.want.code, marker) {
				t.Errorf("response holds the upstream detail: %s", raw)
			}

			rec := findResolveLog(t, logs(), reqID, tc.want.code)
			if got, _ := rec["error"].(string); !strings.Contains(got, tc.logText) {
				t.Errorf("log error = %q, want it to hold %q", got, tc.logText)
			}
			if rec["skill_uri"] != uri || rec["source"] != "federation" || rec["level"] != "WARN" {
				t.Errorf("log record = %v, want skill_uri %q, source federation, level WARN", rec, uri)
			}
			if rec["status"] != float64(tc.want.status) {
				t.Errorf("log status = %v, want %d", rec["status"], tc.want.status)
			}
		})
	}
}

// The fields of a registry's per-skill error are bounded: the code is
// returned only when it is at most 64 characters of [A-Za-z0-9._-] (else
// federation_error), the status only when it is in 100..599, and the log
// keeps the message cut to 1024 bytes and the code and correlation id only
// in their bounded form.
func TestSkillsResolve_FederationPeerFieldsBounded(t *testing.T) {
	const marker = "zq-peer-5079"
	long := strings.Repeat("a", 65)
	cases := []struct {
		name                       string
		code, id, msg              string
		status                     int
		want                       wantResolveError
		logRemoteCode, logRemoteID string
		logStatus                  int
	}{
		{
			name: "invalid code and id, out-of-range status",
			code: "bad code!", id: long, msg: "peer text " + marker, status: 999,
			want:          wantResolveError{code: "federation_error", message: "skill registry request failed"},
			logRemoteCode: "(invalid)", logRemoteID: "(invalid)",
		},
		{
			name: "oversized code, id with a space, status below range",
			code: long, id: "id with space", msg: "peer text " + marker, status: 42,
			want:          wantResolveError{code: "federation_error", message: "skill registry request failed"},
			logRemoteCode: "(invalid)", logRemoteID: "(invalid)",
		},
		{
			name: "status 99, below the range",
			code: "not_found", id: "ok", msg: "peer text " + marker, status: 99,
			want:          wantResolveError{code: "not_found", message: "skill not found"},
			logRemoteCode: "not_found", logRemoteID: "ok",
		},
		{
			name: "status 100, the range start",
			code: "not_found", id: "ok", msg: "peer text " + marker, status: 100,
			want:          wantResolveError{code: "not_found", message: "skill not found", status: 100},
			logRemoteCode: "not_found", logRemoteID: "ok", logStatus: 100,
		},
		{
			name: "status 599, the range end",
			code: "not_found", id: "ok", msg: "peer text " + marker, status: 599,
			want:          wantResolveError{code: "not_found", message: "skill not found", status: 599},
			logRemoteCode: "not_found", logRemoteID: "ok", logStatus: 599,
		},
		{
			name: "status 600, above the range",
			code: "not_found", id: "ok", msg: "peer text " + marker, status: 600,
			want:          wantResolveError{code: "not_found", message: "skill not found"},
			logRemoteCode: "not_found", logRemoteID: "ok",
		},
		{
			name: "message cut inside a multi-byte character",
			code: "not_found", id: "ok", msg: marker + strings.Repeat("x", 1023-len(marker)) + "é", status: 404,
			want:          wantResolveError{code: "not_found", message: "skill not found", status: 404},
			logRemoteCode: "not_found", logRemoteID: "ok", logStatus: 404,
		},
		{
			name: "valid unlisted code and id, oversized message",
			code: "Some.Code-1", id: "peer-id_1.2", msg: marker + strings.Repeat("x", 5000), status: 404,
			want:          wantResolveError{code: "Some.Code-1", message: "resolver error", status: 404},
			logRemoteCode: "Some.Code-1", logRemoteID: "peer-id_1.2", logStatus: 404,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			mock := newFederationMockRegistry(t, func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_ = json.NewEncoder(w).Encode(map[string]any{
					"resolved": []any{},
					"errors": []map[string]any{{
						"uri": "skill:///core/s@1.0", "code": tc.code, "message": tc.msg,
						"status": tc.status, "correlationId": tc.id,
					}},
				})
			})
			srv, s := newFederationTestServer(t, mock)
			addFederationRegistry(t, s, "reg", mock.URL, "hub", "trusted", "active")
			logs := captureJSONSlog(t)

			const uri = "skill://reg/core/s@1.0"
			const reqID = "req-5079-peer"
			raw, errs := postSkillsResolve(t, srv, reqID, uri)
			if len(errs) != 1 {
				t.Fatalf("errors = %v, want 1", errs)
			}
			want := tc.want
			want.uri = uri
			assertResolveError(t, errs[0], want, reqID)
			if strings.Contains(raw, marker) {
				t.Errorf("response holds the peer message: %s", raw)
			}

			rec := findResolveLog(t, logs(), reqID, tc.want.code)
			if rec["remote_code"] != tc.logRemoteCode || rec["remote_correlation_id"] != tc.logRemoteID ||
				rec["status"] != float64(tc.logStatus) || rec["remote_status"] != float64(tc.status) {
				t.Errorf("log record = %v, want remote_code %q, remote_correlation_id %q, status %d, remote_status %d",
					rec, tc.logRemoteCode, tc.logRemoteID, tc.logStatus, tc.status)
			}
			got, _ := rec["error"].(string)
			if prefix := `registry "reg": `; !strings.HasPrefix(got, prefix+marker) && !strings.HasPrefix(got, prefix+"peer text "+marker) {
				t.Errorf("log error = %q, want the peer message", got)
			}
			if limit := len(`registry "reg": `) + 1024; len(got) > limit {
				t.Errorf("log error is %d bytes, want at most %d", len(got), limit)
			}
			if !utf8.ValidString(got) {
				t.Errorf("log error %q is not valid UTF-8", got)
			}
		})
	}
}

// A non-OK registry response keeps its status only when it is in
// 100..599; the log keeps the raw status as remote_status.
func TestSkillsResolve_FederationNonOKStatusRange(t *testing.T) {
	for _, tc := range []struct {
		status, want int
	}{
		{status: 599, want: 599},
		{status: 600, want: 0},
	} {
		t.Run("status_"+strconv.Itoa(tc.status), func(t *testing.T) {
			mock := newFederationMockRegistry(t, func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte("upstream page"))
			})
			srv, s := newFederationTestServer(t, mock)
			addFederationRegistry(t, s, "reg", mock.URL, "hub", "trusted", "active")
			logs := captureJSONSlog(t)

			const uri = "skill://reg/core/s@1.0"
			const reqID = "req-5079-nonok"
			_, errs := postSkillsResolve(t, srv, reqID, uri)
			if len(errs) != 1 {
				t.Fatalf("errors = %v, want 1", errs)
			}
			assertResolveError(t, errs[0], wantResolveError{
				uri: uri, code: "federation_error", message: "skill registry request failed", status: tc.want,
			}, reqID)
			rec := findResolveLog(t, logs(), reqID, "federation_error")
			if rec["status"] != float64(tc.want) || rec["remote_status"] != float64(tc.status) {
				t.Errorf("log record = %v, want status %d, remote_status %d", rec, tc.want, tc.status)
			}
		})
	}
}

func TestSkillsResolve_FederationConnectFailure(t *testing.T) {
	mock := newFederationMockRegistry(t, func(w http.ResponseWriter, r *http.Request) {})
	srv, s := newFederationTestServer(t, mock)
	addFederationRegistry(t, s, "reg", mock.URL, "hub", "trusted", "active")
	mock.Close()
	logs := captureJSONSlog(t)

	const reqID = "req-5079-connect"
	_, errs := postSkillsResolve(t, srv, reqID, "skill://reg/core/s@1.0")
	if len(errs) != 1 {
		t.Fatalf("errors = %v, want 1", errs)
	}
	assertResolveError(t, errs[0], wantResolveError{
		uri: "skill://reg/core/s@1.0", code: "federation_error", message: "skill registry request failed",
	}, reqID)
	rec := findResolveLog(t, logs(), reqID, "federation_error")
	got, _ := rec["error"].(string)
	if !strings.Contains(got, `failed to connect to registry "reg"`) {
		t.Errorf("log error = %q, want the connect failure", got)
	}
}

// A reference that is redacted for the log is logged in its redacted form,
// and the full error is withheld from the record (it can quote the
// reference); the response still carries the fixed phrase and the id.
func TestSkillsResolve_FederationRedactedReference(t *testing.T) {
	mock := newFederationMockRegistry(t, func(w http.ResponseWriter, r *http.Request) {})
	srv, _ := newFederationTestServer(t, mock)
	logs := captureJSONSlog(t)

	const uri = "skill://zquser:zqpass@reg/core/s@1.0"
	const reqID = "req-5079-redacted"
	raw, errs := postSkillsResolve(t, srv, reqID, uri)
	if len(errs) != 1 {
		t.Fatalf("errors = %v, want 1", errs)
	}
	assertResolveError(t, errs[0], wantResolveError{
		uri: uri, code: "unknown_registry", message: "skill registry not configured",
	}, reqID)
	if strings.Count(raw, "zqpass") != 1 {
		t.Errorf("response quotes the reference beyond its uri field: %s", raw)
	}
	rec := findResolveLog(t, logs(), reqID, "unknown_registry")
	if rec["skill_uri"] != "(redacted URL)" || rec["error"] != "(resolver message withheld)" {
		t.Errorf("log record = %v, want the reference redacted and the error withheld", rec)
	}
	for _, r := range logs() {
		b, _ := json.Marshal(r)
		if strings.Contains(string(b), "zqpass") {
			t.Errorf("log record holds the declared reference: %s", b)
		}
	}
}

// randomCorrelationID matches the random correlation id used when there is
// no request id.
var randomCorrelationID = regexp.MustCompile(`^[0-9a-f]{16}$`)

// resolveAsUser posts a batch resolve request as user through the Hub's
// handler chain and returns the raw response text, its decoded errors and
// the correlation id each error must carry: the X-Request-ID of the
// response when there is one, else the error's own id, which must then be
// a random id (the test server installs no request log middleware).
func resolveAsUser(t *testing.T, srv *Server, user *store.User, req ResolveSkillsRequest) (string, []map[string]any, []string) {
	t.Helper()
	rec := doRequestAsUser(t, srv, user, http.MethodPost, "/api/v1/skills/resolve", req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body %s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Errors []map[string]any `json:"errors"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if len(resp.Errors) == 0 {
		t.Fatalf("no errors in response %s", rec.Body.String())
	}
	reqID := rec.Header().Get("X-Request-ID")
	ids := make([]string, len(resp.Errors))
	for i, e := range resp.Errors {
		ids[i] = reqID
		if reqID == "" {
			ids[i], _ = e["correlationId"].(string)
			if !randomCorrelationID.MatchString(ids[i]) {
				t.Fatalf("error %d correlationId = %q, want the request id or a random 16-hex id", i, ids[i])
			}
		}
	}
	return rec.Body.String(), resp.Errors, ids
}

// newFixedStatusGitHub serves every GitHub API request with status and a body
// holding body.
func newFixedStatusGitHub(t *testing.T, status int, body string) *httptest.Server {
	t.Helper()
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(ts.Close)
	return ts
}

func TestSkillsResolve_GitHubErrorsReturnFixedPhrase(t *testing.T) {
	const marker = "zq-gh-detail-5079"
	const uri = "gh://acme/private-repo/skills/s"
	cases := []struct {
		name    string
		status  int
		want    wantResolveError
		logText string
	}{
		{
			name:    "GitHub reports not found",
			status:  http.StatusNotFound,
			want:    wantResolveError{code: "not_found", message: "skill not found", status: http.StatusNotFound},
			logText: "GitHub API error 404 resolving private-repo@",
		},
		{
			name:    "GitHub reports a server error",
			status:  http.StatusInternalServerError,
			want:    wantResolveError{code: "resolve_failed", message: "resolver error", status: http.StatusInternalServerError},
			logText: "GitHub API error 500 resolving private-repo@",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv, _, alice, _, project := setupSkillAuthzTest(t)
			srv.ghResolutionStore = NewGitHubResolutionStore(enttest.NewClient(t))
			gh := newFixedStatusGitHub(t, tc.status, `{"message":"`+marker+`"}`)
			srv.config.GitHubAppConfig.APIBaseURL = gh.URL
			srv.config.GitHubAppConfig.RawBaseURL = gh.URL
			logs := captureJSONSlog(t)

			raw, errs, ids := resolveAsUser(t, srv, alice, ResolveSkillsRequest{
				Skills: []ResolveSkillRef{{URI: uri}}, ProjectID: project.ID,
			})
			id := ids[0]
			want := tc.want
			want.uri = uri
			if len(errs) != 1 {
				t.Fatalf("errors = %v, want 1", errs)
			}
			assertResolveError(t, errs[0], want, id)
			if strings.Contains(raw, marker) {
				t.Errorf("response holds the upstream detail: %s", raw)
			}
			rec := findResolveLog(t, logs(), id, tc.want.code)
			got, _ := rec["error"].(string)
			if !strings.Contains(got, tc.logText) || !strings.Contains(got, marker) {
				t.Errorf("log error = %q, want it to hold %q and the upstream body", got, tc.logText)
			}
			if rec["skill_uri"] != uri || rec["source"] != "github" || rec["status"] != float64(tc.status) {
				t.Errorf("log record = %v, want skill_uri %q, source github, status %d", rec, uri, tc.status)
			}
		})
	}
}

func TestSkillsResolve_GitHubForbiddenProjectReturnsFixedPhrase(t *testing.T) {
	srv, _, _, bob, project := setupSkillAuthzTest(t)
	logs := captureJSONSlog(t)

	const uri = "gh://acme/private-repo/skills/s"
	_, errs, ids := resolveAsUser(t, srv, bob, ResolveSkillsRequest{
		Skills: []ResolveSkillRef{{URI: uri}}, ProjectID: project.ID,
	})
	id := ids[0]
	if len(errs) != 1 {
		t.Fatalf("errors = %v, want 1", errs)
	}
	assertResolveError(t, errs[0], wantResolveError{uri: uri, code: "forbidden", message: "access denied"}, id)
	rec := findResolveLog(t, logs(), id, "forbidden")
	if rec["skill_uri"] != uri || rec["source"] != "github" ||
		rec["error"] != "caller may not resolve GitHub skills for this project" {
		t.Errorf("log record = %v", rec)
	}
}

// A gh:// reference with a query is logged in its redacted form, and the
// full error is withheld from the record.
func TestSkillsResolve_GitHubRedactedReference(t *testing.T) {
	srv, _, alice, _, project := setupSkillAuthzTest(t)
	logs := captureJSONSlog(t)

	const uri = "gh://acme/private-repo/skills/s?token=ZQ_TOKEN_NAME"
	raw, errs, ids := resolveAsUser(t, srv, alice, ResolveSkillsRequest{
		Skills: []ResolveSkillRef{{URI: uri}}, ProjectID: project.ID,
	})
	id := ids[0]
	if len(errs) != 1 {
		t.Fatalf("errors = %v, want 1", errs)
	}
	assertResolveError(t, errs[0], wantResolveError{uri: uri, code: "resolve_failed", message: "resolver error"}, id)
	if strings.Count(raw, "ZQ_TOKEN_NAME") != 1 {
		t.Errorf("response quotes the reference beyond its uri field: %s", raw)
	}
	rec := findResolveLog(t, logs(), id, "resolve_failed")
	if rec["skill_uri"] != "(redacted URL)" || rec["error"] != "(resolver message withheld)" {
		t.Errorf("log record = %v, want the reference redacted and the error withheld", rec)
	}
}

func TestSkillsResolve_InvalidURIReturnsFixedPhrase(t *testing.T) {
	srv, _, alice, _, project := setupSkillAuthzTest(t)
	logs := captureJSONSlog(t)

	const uri = "skill://project/a/b/c"
	_, errs, ids := resolveAsUser(t, srv, alice, ResolveSkillsRequest{
		Skills: []ResolveSkillRef{{URI: uri}}, ProjectID: project.ID,
	})
	id := ids[0]
	if len(errs) != 1 {
		t.Fatalf("errors = %v, want 1", errs)
	}
	assertResolveError(t, errs[0], wantResolveError{uri: uri, code: "invalid_uri", message: "invalid skill URI"}, id)
	rec := findResolveLog(t, logs(), id, "invalid_uri")
	if got, _ := rec["error"].(string); !strings.Contains(got, "too many path segments") {
		t.Errorf("log error = %q, want the parse error", got)
	}
}

func TestSkillsResolve_RegistryErrorsReturnFixedPhrase(t *testing.T) {
	srv, s, alice, _, project := setupSkillAuthzTest(t)
	skill := createTestSkill(t, s, "zq-registry-skill", store.SkillScopeGlobal, "", alice.ID)
	publishTestSkillVersion(t, s, skill)
	logs := captureJSONSlog(t)

	const (
		missing    = "skill://scion/global/zq-does-not-exist"
		badVersion = "skill://scion/global/zq-registry-skill@9.9.9"
	)
	_, errs, ids := resolveAsUser(t, srv, alice, ResolveSkillsRequest{
		Skills: []ResolveSkillRef{{URI: missing}, {URI: badVersion}}, ProjectID: project.ID,
	})
	if len(errs) != 2 {
		t.Fatalf("errors = %v, want 2", errs)
	}
	assertResolveError(t, errs[0], wantResolveError{uri: missing, code: "not_found", message: "skill not found"}, ids[0])
	assertResolveError(t, errs[1], wantResolveError{uri: badVersion, code: "not_found", message: "skill not found"}, ids[1])

	recs := logs()
	wantLog := map[string]string{
		missing:    `skill "zq-does-not-exist" not found in any scope`,
		badVersion: `skill "zq-registry-skill" found but version "9.9.9" could not be resolved`,
	}
	for i, uri := range []string{missing, badVersion} {
		var got []string
		for _, r := range recs {
			if r["msg"] == skillResolveLogMsg && r["correlation_id"] == ids[i] && r["skill_uri"] == uri {
				if r["source"] != "registry" {
					t.Errorf("log record = %v, want source registry", r)
				}
				text, _ := r["error"].(string)
				got = append(got, text)
			}
		}
		if len(got) != 1 || !strings.Contains(got[0], wantLog[uri]) {
			t.Errorf("log errors for %s = %q, want one holding %q", uri, got, wantLog[uri])
		}
	}
}

// A storage failure is logged at ERROR with the skill name, version and
// storage error as attributes of their own, also for a reference that is
// redacted in the log (one that pins a version by content hash); the
// response carries only the phrase.
func TestSkillsResolve_RegistryStorageErrorReturnsFixedPhrase(t *testing.T) {
	hash := "sha256:" + strings.Repeat("ab", 32)
	cases := []struct {
		name     string
		uri      string
		redacted bool
	}{
		{name: "reference without a version", uri: "skill://scion/global/zq-stored-skill"},
		{name: "content hash reference", uri: "skill://scion/global/zq-stored-skill@" + hash, redacted: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv, s, alice, _, project := setupSkillAuthzTest(t)
			srv.SetStorage(signErrStorage{newMockStorage("zq-bucket")})
			skill := createTestSkill(t, s, "zq-stored-skill", store.SkillScopeGlobal, "", alice.ID)
			if err := s.CreateSkillVersion(t.Context(), &store.SkillVersion{
				ID:          api.NewUUID(),
				SkillID:     skill.ID,
				Version:     "1.0.0",
				ContentHash: hash,
				Status:      store.SkillVersionStatusPublished,
				Files:       []store.TemplateFile{{Path: "SKILL.md", Size: 1, Hash: "sha256:f"}},
				Created:     time.Now(),
			}); err != nil {
				t.Fatal(err)
			}
			logs := captureJSONSlog(t)

			raw, errs, ids := resolveAsUser(t, srv, alice, ResolveSkillsRequest{
				Skills: []ResolveSkillRef{{URI: tc.uri}}, ProjectID: project.ID,
			})
			id := ids[0]
			if len(errs) != 1 {
				t.Fatalf("errors = %v, want 1", errs)
			}
			assertResolveError(t, errs[0], wantResolveError{uri: tc.uri, code: "storage_error", message: "skill files unavailable"}, id)
			if strings.Contains(raw, "injected signed URL fault") {
				t.Errorf("response holds the storage error: %s", raw)
			}

			rec := findResolveLog(t, logs(), id, "storage_error")
			if rec["level"] != "ERROR" || rec["skill"] != "zq-stored-skill" || rec["version"] != "1.0.0" {
				t.Errorf("log record = %v, want level ERROR, skill zq-stored-skill, version 1.0.0", rec)
			}
			if got, _ := rec["storage_error"].(string); !strings.Contains(got, "injected signed URL fault") {
				t.Errorf("log storage_error = %q, want the storage error", got)
			}
			got, _ := rec["error"].(string)
			if tc.redacted {
				if rec["skill_uri"] != "(redacted URL)" || got != "(resolver message withheld)" {
					t.Errorf("log record = %v, want the reference redacted and the error withheld", rec)
				}
			} else if !strings.Contains(got, "injected signed URL fault") {
				t.Errorf("log error = %q, want the storage error", got)
			}
		})
	}
}

// When there is no request id (for example dispatch outside a request),
// each error carries a random correlation id, and the log record holds the
// same id.
func TestPreResolveAgentSkills_ErrorsCarryFallbackCorrelationID(t *testing.T) {
	srv, _, alice, _, project := setupSkillAuthzTest(t)
	logs := captureJSONSlog(t)

	const uri = "skill://scion/global/zq-dispatch-missing"
	resp := srv.preResolveAgentSkills(context.Background(), dispatchTestAgent(alice.ID, project.ID, uri))
	if resp == nil {
		t.Fatal("no pre-resolution response")
	}
	b, err := json.Marshal(resp)
	if err != nil {
		t.Fatal(err)
	}
	var wire struct {
		Errors []map[string]any `json:"errors"`
	}
	if err := json.Unmarshal(b, &wire); err != nil {
		t.Fatal(err)
	}
	if len(wire.Errors) != 1 {
		t.Fatalf("errors = %s, want 1", b)
	}
	id, _ := wire.Errors[0]["correlationId"].(string)
	if !randomCorrelationID.MatchString(id) {
		t.Fatalf("correlationId = %q, want a random 16-hex id", id)
	}
	assertResolveError(t, wire.Errors[0], wantResolveError{uri: uri, code: "not_found", message: "skill not found"}, id)
	rec := findResolveLog(t, logs(), id, "not_found")
	if got, _ := rec["error"].(string); !strings.Contains(got, `skill "zq-dispatch-missing" not found in any scope`) {
		t.Errorf("log error = %q", got)
	}
}
