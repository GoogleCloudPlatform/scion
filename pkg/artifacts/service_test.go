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

func TestServiceAnswers404ForEverything(t *testing.T) {
	h := NewService(nil).Handler()
	for _, tc := range []struct{ method, path string }{
		{http.MethodGet, "/api/v1/artifacts"},
		{http.MethodPost, "/api/v1/artifacts"},
		{http.MethodGet, "/api/v1/artifacts/abc"},
		{http.MethodDelete, "/api/v1/artifacts/abc"},
		{http.MethodGet, "/api/v1/artifacts/shared/token"},
	} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(tc.method, tc.path, nil))
		if rec.Code != http.StatusNotFound {
			t.Errorf("%s %s: status %d, want 404", tc.method, tc.path, rec.Code)
		}
		var body errorResponse
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil || body.Error.Code != "not_found" {
			t.Errorf("%s %s: body %q, want not_found JSON error", tc.method, tc.path, rec.Body.String())
		}
	}
}

// TestHostIsStringOnly pins D13: every Host method takes a context plus
// strings and returns strings and bools only, so it can be served remotely.
func TestHostIsStringOnly(t *testing.T) {
	ctxType := reflect.TypeOf((*context.Context)(nil)).Elem()
	host := reflect.TypeOf((*Host)(nil)).Elem()
	if host.NumMethod() != 3 {
		t.Fatalf("Host has %d methods, want 3 (Principal, Authorize, Permits)", host.NumMethod())
	}
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
			if k := m.Type.Out(j).Kind(); k != reflect.String && k != reflect.Bool {
				t.Errorf("%s: result %d is %v, want string or bool", m.Name, j, m.Type.Out(j))
			}
		}
	}
}

// TestNoHubImports keeps the package extractable: it must not import any
// hub package (D2, D13). The hub adapter lives in pkg/hub instead.
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
