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

package relay

import (
	"net/http"
	"testing"
)

// TestForwardCredentialHeaders (design §3.9 "Forwarded headers"): only the
// fixed allowlist crosses; signed-header expansion applies to broker
// requests only, and only to broker-signable names; proxy identity,
// addressing, cookie and X-Goog-* headers never cross, whatever lists them.
func TestForwardCredentialHeaders(t *testing.T) {
	h := http.Header{}
	h.Set("Authorization", "Bearer t")
	h.Set("X-Scion-Agent-Token", "a")
	h.Set("X-Forwarded-User-Id", "u")
	h.Set("X-Forwarded-User-Email", "u@example.com")
	h.Set("X-Forwarded-For", "10.0.0.1")
	h.Set("Forwarded", "for=10.0.0.1")
	h.Set("X-Real-Ip", "10.0.0.1")
	h.Set("Cookie", "s=1")
	h.Set("X-Goog-Iap-Jwt-Assertion", "x")
	h.Set("Content-Type", "application/json")
	h.Set("X-Unlisted", "v")
	h.Set("X-Scion-Signed-Headers", "X-Forwarded-User-Id;Cookie;X-Goog-Iap-Jwt-Assertion;Content-Type;X-Unlisted")

	got := ForwardCredentialHeaders(h)
	for _, k := range []string{"Authorization", "X-Scion-Agent-Token", "X-Scion-Signed-Headers"} {
		if len(got[k]) == 0 {
			t.Errorf("allowlisted %s not forwarded", k)
		}
	}
	for _, k := range []string{"X-Forwarded-User-Id", "X-Forwarded-User-Email", "X-Forwarded-For", "Forwarded", "X-Real-Ip", "Cookie", "X-Goog-Iap-Jwt-Assertion", "X-Unlisted", "Content-Type"} {
		if _, ok := got[k]; ok {
			t.Errorf("non-broker request: %s forwarded", k)
		}
	}

	h.Set("X-Scion-Broker-Id", "b1")
	got = ForwardCredentialHeaders(h)
	if got["Content-Type"] == nil {
		t.Error("broker request: a broker-signable listed header was not forwarded")
	}
	for _, k := range []string{"X-Forwarded-User-Id", "Cookie", "X-Goog-Iap-Jwt-Assertion", "X-Unlisted"} {
		if _, ok := got[k]; ok {
			t.Errorf("broker request: %s forwarded", k)
		}
	}
}
