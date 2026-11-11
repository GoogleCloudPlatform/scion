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

import "testing"

// TestRelayServesHost: the dialer's Host matches the relay's public
// endpoint by hostname (any case) and port, where no port and the scheme's
// default port are the same; anything else is refused.
func TestRelayServesHost(t *testing.T) {
	cases := []struct {
		name, endpoint, host string
		want                 bool
	}{
		{"exact", "https://relay-7.example.com", "relay-7.example.com", true},
		{"endpoint without port, host with default port", "https://relay-7.example.com", "relay-7.example.com:443", true},
		{"endpoint with default port, host without port", "https://relay-7.example.com:443", "relay-7.example.com", true},
		{"http default port", "http://relay-7.example.com", "relay-7.example.com:80", true},
		{"explicit port on both", "https://relay-7.example.com:8443", "relay-7.example.com:8443", true},
		{"mixed case", "https://Relay-7.Example.com", "RELAY-7.example.COM:443", true},
		{"different port", "https://relay-7.example.com", "relay-7.example.com:8443", false},
		{"other scheme's default port", "https://relay-7.example.com", "relay-7.example.com:80", false},
		{"explicit port vs none", "https://relay-7.example.com:8443", "relay-7.example.com", false},
		{"different hostname", "https://relay-7.example.com", "relay-8.example.com", false},
		{"lookalike suffix", "https://relay-7.example.com", "relay-7.example.com.evil.example", false},
		{"lookalike prefix", "https://relay-7.example.com", "evil-relay-7.example.com", false},
		{"parent domain", "https://relay-7.example.com", "example.com", false},
		{"empty host", "https://relay-7.example.com", "", false},
		{"empty endpoint", "", "relay-7.example.com", false},
		{"unparsable endpoint", "https://relay-7.example.com:bad", "relay-7.example.com", false},
		{"endpoint without scheme", "relay-7.example.com", "relay-7.example.com", false},
		{"non-numeric host port", "https://relay-7.example.com", "relay-7.example.com:bad", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := relayServesHost(tc.endpoint, tc.host); got != tc.want {
				t.Fatalf("relayServesHost(%q, %q) = %v, want %v", tc.endpoint, tc.host, got, tc.want)
			}
		})
	}
}
