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
