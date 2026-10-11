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
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// capturingHandler is a minimal slog.Handler that records every log record
// it receives, for tests to inspect fields on without depending on JSON
// serialization or a particular io.Writer.
type capturingHandler struct {
	mu      sync.Mutex
	records []slog.Record
}

// attrMap flattens a slog.Record's top-level attributes (including nested
// groups, dotted) into a map for easy lookup in assertions.
func recordAttrs(r slog.Record) map[string]any {
	out := map[string]any{}
	var walk func(prefix string, a slog.Attr)
	walk = func(prefix string, a slog.Attr) {
		key := a.Key
		if prefix != "" {
			key = prefix + "." + a.Key
		}
		// Resolve LogValuer values (e.g. CredentialDecoration.LogValue())
		// before inspecting Kind: a captured record from a bare handler like
		// capturingHandler never resolves them itself (only the standard
		// Text/JSON handlers do that internally when formatting).
		a.Value = a.Value.Resolve()
		if a.Value.Kind() == slog.KindGroup {
			for _, ga := range a.Value.Group() {
				walk(key, ga)
			}
			return
		}
		out[key] = a.Value.Any()
	}
	r.Attrs(func(a slog.Attr) bool {
		walk("", a)
		return true
	})
	return out
}

// setupUATProjectAndOwner creates a project and an owner user with a real
// project-owner role binding (via createRS1Project), so the owner has actual
// authority to mint a scoped UAT with real scopes (RS4's issuer-ceiling check
// requires this — a project record with no role binding mints nothing).
func setupUATProjectAndOwner(t *testing.T, s store.Store, name string) (projectID, ownerID string) {
	t.Helper()
	projectID = tid(name + "-project")
	ownerID = tid(name + "-owner")
	createRS1Project(t, s, projectID, ownerID)
	return projectID, ownerID
}

func findRecord(records []slog.Record, msg string) (slog.Record, bool) {
	for i := len(records) - 1; i >= 0; i-- {
		if records[i].Message == msg {
			return records[i], true
		}
	}
	return slog.Record{}, false
}

func doRequestWithBearer(srv *Server, bearer string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, "/api/v1/projects/anything", nil)
	req.Header.Set("Authorization", "Bearer "+bearer)
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, req)
	return rr
}

func (h *capturingHandler) Enabled(context.Context, slog.Level) bool { return true }

func (h *capturingHandler) Handle(_ context.Context, r slog.Record) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.records = append(h.records, r.Clone())
	return nil
}

func (h *capturingHandler) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h *capturingHandler) WithGroup(string) slog.Handler      { return h }

func (h *capturingHandler) all() []slog.Record {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make([]slog.Record, len(h.records))
	copy(out, h.records)
	return out
}
