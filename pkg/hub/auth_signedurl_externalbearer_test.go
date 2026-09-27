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
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestSignedSkillFileURL_NeverReachesExternalBearer covers the interaction
// between upstream's credential-less skill-file capability URL
// (isSignedSkillFileRequest, Step 3c in UnifiedAuthMiddleware) and the
// external-bearer path this branch adds (Step 4's default arm,
// serveExternalBearer). A capability-URL request carries no Authorization
// header at all, so Step 3's extractBearerToken returns "" and control
// short-circuits into Step 3c long before Step 4 (where serveExternalBearer
// is called) is ever reached — but that ordering is exactly the kind of
// thing a future refactor could invert silently. This test wires the
// external-bearer path fully (a non-nil GoogleValidator and a matching
// Google trust entry) and proves: (1) the request is still admitted via
// AuthTypeSignedURL, and (2) the Google validator is never invoked.
func TestSignedSkillFileURL_NeverReachesExternalBearer(t *testing.T) {
	validator := &countingGoogleValidator{
		fakeGoogleValidator: fakeGoogleValidator{accessTokenResult: validGmailIdentity()},
	}
	resolver := NewGoogleIdentityResolver(newFakeUserStore(), newMemExtIDStore(), alwaysAuthorized, nil, nil)
	cfg := newExternalBearerConfig(t, validator, resolver)
	fake := attachExternalBearerMetrics(&cfg)

	middleware := UnifiedAuthMiddleware(cfg)
	result := &probeResult{}
	handler := middleware(probeHandler(result))

	// Shape isSignedSkillFileRequest requires: GET, canonical-UUID skill ID,
	// /files/<path>, and both sig= and exp= present. The signature does not
	// need to actually verify here — the middleware only checks the request
	// SHAPE at Step 3c; cryptographic verification happens later, in
	// handleSkillFileRead, which this test does not reach (there is no
	// registered mux, just the middleware wrapping a probe handler).
	target := "/api/v1/skills/11111111-1111-1111-1111-111111111111/files/SKILL.md" +
		"?raw=1&version=1.0.0&exp=9999999999&sig=deadbeef"
	req := httptest.NewRequest(http.MethodGet, target, nil)
	// Deliberately NO Authorization header: this is the capability-URL shape.

	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if !result.reached {
		t.Fatalf("expected the next handler to be reached; status=%d body=%s", w.Code, w.Body.String())
	}
	if result.authType != AuthTypeSignedURL {
		t.Errorf("authType = %q, want %q", result.authType, AuthTypeSignedURL)
	}
	if got := validator.totalCalls(); got != 0 {
		t.Errorf("Google validator called %d time(s), want 0: a signed-url capability request must never reach serveExternalBearer", got)
	}
	if calls := fake.allCalls(); len(calls) != 0 {
		t.Errorf("external-bearer metrics recorded %d call(s), want 0: %+v", len(calls), calls)
	}
}
