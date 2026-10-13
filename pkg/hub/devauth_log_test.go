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

package hub

import (
	"bytes"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestDevAuthMiddleware_MismatchLogOmitsTokens checks that the debug log
// line for a dev token mismatch contains no part of either token value.
func TestDevAuthMiddleware_MismatchLogOmitsTokens(t *testing.T) {
	// Made-up fixture values; each prefix is longer than any logged excerpt.
	const expected = "scion_dev_expectedfixture0123456789abcdef"
	const provided = "scion_dev_providedfixture0123456789abcdef"

	var logs bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })

	handler := DevAuthMiddlewareWithDebug(expected, DevUserConfig{}, true)(
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			t.Error("handler reached with a mismatched dev token")
		}))
	req := httptest.NewRequest(http.MethodGet, "/api/v1/agents", nil)
	req.Header.Set("Authorization", "Bearer "+provided)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusUnauthorized)
	}
	out := logs.String()
	if !strings.Contains(out, "dev token mismatch") {
		t.Fatalf("mismatch debug line not logged; log:\n%s", out)
	}
	for _, leak := range []string{"scion_dev_", "expectedfix", "providedfix"} {
		if strings.Contains(out, leak) {
			t.Errorf("log contains token text %q; log:\n%s", leak, out)
		}
	}
}
