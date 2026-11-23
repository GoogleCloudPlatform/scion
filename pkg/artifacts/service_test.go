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

package artifacts

import (
	"context"
	"encoding/json"
	"go/parser"
	"go/token"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"
)

type recordingMux struct {
	patterns []string
	handlers map[string]http.Handler
}

func (m *recordingMux) Handle(pattern string, h http.Handler) {
	if m.handlers == nil {
		m.handlers = map[string]http.Handler{}
	}
	m.patterns = append(m.patterns, pattern)
	m.handlers[pattern] = h
}

func TestRegisterRoutesMountsEveryPatternThroughGuard(t *testing.T) {
	svc := NewService(nil)
	mux := &recordingMux{}
	var guarded []string
	svc.RegisterRoutes(mux, func(pattern string, h http.Handler) http.Handler {
		guarded = append(guarded, pattern)
		return h
	})
	if !slices.Equal(mux.patterns, RoutePatterns()) {
		t.Fatalf("mounted %v, want %v", mux.patterns, RoutePatterns())
	}
	if !slices.Equal(guarded, RoutePatterns()) {
		t.Fatalf("guarded %v, want %v", guarded, RoutePatterns())
	}
}

func TestRegisterRoutesOnServeMux(t *testing.T) {
	mux := http.NewServeMux()
	NewService(nil).RegisterRoutes(mux, nil)
	for _, path := range []string{"/api/v1/artifacts", "/api/v1/artifacts/abc", "/api/v1/artifacts/shared/tok"} {
		_, pattern := mux.Handler(httptest.NewRequest(http.MethodGet, path, nil))
		if pattern == "" {
			t.Errorf("%s: no route mounted", path)
		}
	}
}

func TestUnconfiguredServiceRoutes(t *testing.T) {
	h := NewService(nil).Handler()
	for _, tc := range []struct {
		method, path string
		status       int
	}{
		{http.MethodGet, "/api/v1/artifacts/shared/token", http.StatusNotFound},
		{http.MethodGet, "/api/v1/artifacts/abc/unknown", http.StatusNotFound},
		{http.MethodGet, "/api/v1/artifacts", http.StatusBadRequest},
		{http.MethodGet, "/api/v1/artifacts?mine=1", http.StatusServiceUnavailable},
		{http.MethodPut, "/api/v1/artifacts", http.StatusMethodNotAllowed},
		{http.MethodPut, "/api/v1/artifacts/00000000-0000-4000-8000-000000000001", http.StatusMethodNotAllowed},
		{http.MethodDelete, "/api/v1/artifacts/00000000-0000-4000-8000-000000000001", http.StatusServiceUnavailable},
		{http.MethodDelete, "/api/v1/artifacts/abc", http.StatusNotFound},
	} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(tc.method, tc.path, nil))
		if rec.Code != tc.status {
			t.Errorf("%s %s: status %d, want %d", tc.method, tc.path, rec.Code, tc.status)
		}
		var body errorResponse
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil || body.Error.Code == "" {
			t.Errorf("%s %s: body %q, want a JSON error", tc.method, tc.path, rec.Body.String())
		}
	}
}

// TestHostIsStringOnly pins the string-only contract: every Host method
// takes a context plus strings and returns strings, string slices, bools
// and errors only, so it can be served remotely.
func TestHostIsStringOnly(t *testing.T) {
	host := reflect.TypeOf((*Host)(nil)).Elem()
	if host.NumMethod() != 6 {
		t.Fatalf("Host has %d methods, want 6 (Principal, Authorize, Permits, MemberScopes, SealCursor, OpenCursor)", host.NumMethod())
	}
	assertStringOnly(t, host)
	assertStringOnly(t, reflect.TypeOf((*ScopeExplainer)(nil)).Elem())
	assertStringOnly(t, reflect.TypeOf((*ScopeAdministrator)(nil)).Elem())
	// The batch lookups take a list of string ids and answer a map from
	// id to bool: still strings and bools only.
	for _, ext := range []reflect.Type{reflect.TypeOf((*PrincipalChecker)(nil)).Elem(), reflect.TypeOf((*ScopeChecker)(nil)).Elem()} {
		if ext.NumMethod() != 1 {
			t.Fatalf("%s has %d methods, want 1", ext.Name(), ext.NumMethod())
		}
		m := ext.Method(0).Type
		want := reflect.TypeOf(func(context.Context, []string) (map[string]bool, error) { return nil, nil })
		if m != want {
			t.Errorf("%s.%s is %v, want %v", ext.Name(), ext.Method(0).Name, m, want)
		}
	}
}

func assertStringOnly(t *testing.T, host reflect.Type) {
	t.Helper()
	ctxType := reflect.TypeOf((*context.Context)(nil)).Elem()
	errType := reflect.TypeOf((*error)(nil)).Elem()
	for i := 0; i < host.NumMethod(); i++ {
		m := host.Method(i)
		if m.Type.NumIn() == 0 || m.Type.In(0) != ctxType {
			t.Errorf("%s: first parameter must be context.Context", m.Name)
		}
		for j := 1; j < m.Type.NumIn(); j++ {
			if k := m.Type.In(j).Kind(); k != reflect.String {
				t.Errorf("%s: parameter %d is %v, want string", m.Name, j, m.Type.In(j))
			}
		}
		for j := 0; j < m.Type.NumOut(); j++ {
			out := m.Type.Out(j)
			switch {
			case out.Kind() == reflect.String, out.Kind() == reflect.Bool, out == errType:
			case out.Kind() == reflect.Slice && out.Elem().Kind() == reflect.String:
			default:
				t.Errorf("%s: result %d is %v, want string, []string, bool or error", m.Name, j, out)
			}
		}
	}
}

// TestNoHubImports keeps the package extractable: it must not import any
// hub package, so it can run outside the hub. The hub adapter lives in
// pkg/hub instead.
func TestNoHubImports(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	for _, name := range files {
		src, err := os.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		f, err := parser.ParseFile(fset, name, src, parser.ImportsOnly)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		for _, imp := range f.Imports {
			path, _ := strconv.Unquote(imp.Path.Value)
			if strings.HasPrefix(path, "github.com/GoogleCloudPlatform/scion/pkg/hub") ||
				strings.HasPrefix(path, "github.com/GoogleCloudPlatform/scion/pkg/store") ||
				strings.HasPrefix(path, "github.com/GoogleCloudPlatform/scion/pkg/ent") {
				t.Errorf("%s imports %s; pkg/artifacts must not depend on hub packages", name, path)
			}
		}
	}
}

// TestWriteMethodNotAllowedSetsAllow pins the helper every 405 in the
// service goes through: the status, the exact Allow list in call order, and
// the JSON error code (ptone/scion#4057).
func TestWriteMethodNotAllowedSetsAllow(t *testing.T) {
	for _, tc := range []struct {
		methods []string
		allow   string
	}{
		{[]string{http.MethodDelete}, "DELETE"},
		{[]string{http.MethodGet, http.MethodHead}, "GET, HEAD"},
		{[]string{http.MethodGet, http.MethodHead, http.MethodPost}, "GET, HEAD, POST"},
	} {
		rec := httptest.NewRecorder()
		writeMethodNotAllowed(rec, tc.methods[0], tc.methods[1:]...)
		if rec.Code != http.StatusMethodNotAllowed {
			t.Errorf("%v: status %d, want 405", tc.methods, rec.Code)
		}
		if got := rec.Header().Get("Allow"); got != tc.allow {
			t.Errorf("%v: Allow %q, want %q", tc.methods, got, tc.allow)
		}
		var body map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatalf("%v: body not JSON: %v", tc.methods, err)
		}
		if !strings.Contains(rec.Body.String(), "method_not_allowed") {
			t.Errorf("%v: body %s lacks method_not_allowed", tc.methods, rec.Body.String())
		}
	}
}

type initTestHost struct{}

func (initTestHost) Principal(context.Context) (string, string, string, bool) {
	return "user", "user-1", "project:p1", true
}
func (initTestHost) Authorize(context.Context, string, string) bool { return true }
func (initTestHost) Permits(context.Context, string, string) bool   { return true }
func (initTestHost) MemberScopes(context.Context) ([]string, error) {
	return []string{"project:p1"}, nil
}
func (initTestHost) SealCursor(context.Context, string, string) (string, error) {
	return "", nil
}
func (initTestHost) OpenCursor(context.Context, string, string) (string, error) {
	return "", nil
}

// TestInitializingBackendReturnsRetryAfter verifies ptone/scion#5712: while
// the artifact store is initializing at startup (Backend.Initializing=true and
// Store==nil), artifact routes answer 503 with a starting-up message and a
// Retry-After header rather than claiming storage is not configured.
func TestInitializingBackendReturnsRetryAfter(t *testing.T) {
	svc := NewService(initTestHost{})
	initializing := true
	svc.SetBackendProvider(func() Backend {
		return Backend{Initializing: initializing}
	})
	h := svc.Handler()

	// 43-char base64url token accepted by validLinkToken.
	validToken := strings.Repeat("a", 43)
	cases := []struct {
		name   string
		method string
		path   string
		body   string
	}{
		{"list", http.MethodGet, "/api/v1/artifacts?mine=1", ""},
		{"get", http.MethodGet, "/api/v1/artifacts/00000000-0000-4000-8000-000000000001", ""},
		{"delete", http.MethodDelete, "/api/v1/artifacts/00000000-0000-4000-8000-000000000001", ""},
		{"publish", http.MethodPost, "/api/v1/artifacts?name=notes.md", "hello"},
		{"create", http.MethodPost, "/api/v1/artifacts", `{"key":"doc","entry":"notes.md","files":[{"path":"notes.md","size":5}]}`},
		{"view", http.MethodGet, "/api/v1/artifacts/view/cap/index.html", ""},
		{"shared", http.MethodGet, "/api/v1/artifacts/shared/" + validToken, ""},
	}

	for _, tc := range cases {
		t.Run("initializing/"+tc.name, func(t *testing.T) {
			req := httptest.NewRequest(tc.method, tc.path, strings.NewReader(tc.body))
			if tc.method == http.MethodPost && tc.body != "" && strings.HasPrefix(tc.body, "{") {
				req.Header.Set("Content-Type", "application/json")
			}
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			if rec.Code != http.StatusServiceUnavailable {
				t.Fatalf("status %d, want 503; body=%s", rec.Code, rec.Body.String())
			}
			ra := rec.Header().Get("Retry-After")
			if n, err := strconv.Atoi(ra); err != nil || n < 1 {
				t.Errorf("Retry-After = %q, want positive integer", ra)
			}
			var body errorResponse
			if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
				t.Fatalf("body not JSON: %v (%q)", err, rec.Body.String())
			}
			if body.Error.Code != "unavailable" {
				t.Errorf("code = %q, want unavailable", body.Error.Code)
			}
			if !strings.Contains(body.Error.Message, "starting up") {
				t.Errorf("message = %q, want starting up", body.Error.Message)
			}
			if strings.Contains(body.Error.Message, "not configured") {
				t.Errorf("message = %q, must not say not configured while starting up", body.Error.Message)
			}
		})
	}

	// Once not initializing (genuinely unconfigured), the response reverts to
	// "not configured" with no Retry-After header.
	initializing = false
	for _, tc := range cases {
		t.Run("unconfigured/"+tc.name, func(t *testing.T) {
			req := httptest.NewRequest(tc.method, tc.path, strings.NewReader(tc.body))
			if tc.method == http.MethodPost && tc.body != "" && strings.HasPrefix(tc.body, "{") {
				req.Header.Set("Content-Type", "application/json")
			}
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			if rec.Code != http.StatusServiceUnavailable {
				t.Fatalf("status %d, want 503; body=%s", rec.Code, rec.Body.String())
			}
			if ra := rec.Header().Get("Retry-After"); ra != "" {
				t.Errorf("Retry-After = %q, want empty when unconfigured", ra)
			}
			var body errorResponse
			if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
				t.Fatalf("body not JSON: %v (%q)", err, rec.Body.String())
			}
			if !strings.Contains(body.Error.Message, "not configured") {
				t.Errorf("message = %q, want not configured", body.Error.Message)
			}
		})
	}
}
