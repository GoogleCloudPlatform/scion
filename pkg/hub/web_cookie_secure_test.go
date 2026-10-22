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
	"testing"
)

// The session cookie's Secure flag is decided where it is used, from the
// base URL's scheme compared case-insensitively, so a direct caller of
// NewWebServer cannot get a non-Secure cookie from "HTTPS://"
// (ptone/scion#5364).
func TestNewWebServer_SessionCookieSecureFollowsScheme(t *testing.T) {
	for _, tc := range []struct {
		baseURL string
		secure  bool
	}{
		{"https://hub.example.com", true},
		{"HTTPS://hub.example.com", true},
		{"Https://hub.example.com", true},
		{"http://hub.example.com", false},
		{"HTTP://hub.example.com", false},
		{"", false},
		{"://bad", false},
		// An https URL that does not parse: Secure (fail closed).
		{"https://localhost:8443/%zz", true},
		{"HTTPS://host:abc", true},
		// A single slash: Secure. This row tells a scheme match apart from
		// an "https://" prefix test, which would fall open.
		{"https:/host", true},
		// Unparsable and not https: not Secure.
		{"http://host:abc", false},
	} {
		t.Run(tc.baseURL, func(t *testing.T) {
			if got := baseURLIsHTTPS(tc.baseURL); got != tc.secure {
				t.Errorf("baseURLIsHTTPS(%q) = %v, want %v", tc.baseURL, got, tc.secure)
			}
			ws := NewWebServer(WebServerConfig{BaseURL: tc.baseURL, Host: "127.0.0.1"})
			store := ws.sessionStore
			if store == nil || store.Options == nil {
				t.Fatal("NewWebServer built no session cookie store")
			}
			if store.Options.Secure != tc.secure {
				t.Errorf("NewWebServer(BaseURL %q): cookie Secure = %v, want %v", tc.baseURL, store.Options.Secure, tc.secure)
			}
		})
	}
}
