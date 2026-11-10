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
	"crypto/tls"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
)

// requestBaseURLWith honours X-Forwarded-Host and X-Forwarded-Proto only from
// a peer in a non-empty trusted proxy list; otherwise the URL comes from the
// request's Host and TLS state, whatever the client sent.
func TestRequestBaseURLWith_ForwardedHeadersOnlyFromTrustedProxies(t *testing.T) {
	_, proxyNet, err := net.ParseCIDR("10.20.0.0/16")
	if err != nil {
		t.Fatal(err)
	}
	trusted := []*net.IPNet{proxyNet}
	forged := map[string][]string{"X-Forwarded-Host": {"attacker.example"}, "X-Forwarded-Proto": {"https"}}
	cases := []struct {
		name    string
		remote  string
		tls     bool
		headers map[string][]string
		trusted []*net.IPNet
		want    string
	}{
		{name: "no headers, plain", remote: "192.0.2.1:1", want: "http://hub.example.com"},
		{name: "no headers, TLS", remote: "192.0.2.1:1", tls: true, want: "https://hub.example.com"},
		{name: "forged headers, empty list", remote: "192.0.2.1:1", headers: forged, want: "http://hub.example.com"},
		{name: "forged headers, loopback peer, empty list", remote: "127.0.0.1:1", headers: forged, want: "http://hub.example.com"},
		{name: "forged headers, peer outside the list", remote: "192.0.2.1:1", headers: forged, trusted: trusted, want: "http://hub.example.com"},
		{name: "forged http proto over TLS, empty list", remote: "192.0.2.1:1", tls: true,
			headers: map[string][]string{"X-Forwarded-Proto": {"http"}}, want: "https://hub.example.com"},
		{name: "trusted proxy", remote: "10.20.3.4:1", headers: map[string][]string{"X-Forwarded-Host": {"hub.public.example"}, "X-Forwarded-Proto": {"https"}},
			trusted: trusted, want: "https://hub.public.example"},
		{name: "trusted proxy, proto only", remote: "10.20.3.4:1", headers: map[string][]string{"X-Forwarded-Proto": {"https"}},
			trusted: trusted, want: "https://hub.example.com"},
		{name: "trusted proxy, appended host list ignored", remote: "10.20.3.4:1",
			headers: map[string][]string{"X-Forwarded-Host": {"attacker.example, hub.public.example"}}, trusted: trusted, want: "http://hub.example.com"},
		{name: "trusted proxy, odd proto ignored", remote: "10.20.3.4:1",
			headers: map[string][]string{"X-Forwarded-Proto": {"javascript"}}, trusted: trusted, want: "http://hub.example.com"},
		{name: "trusted proxy, two proto lines ignored", remote: "10.20.3.4:1",
			headers: map[string][]string{"X-Forwarded-Proto": {"https", "http"}}, trusted: trusted, want: "http://hub.example.com"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodGet, "/api/v1/templates/t1/upload", nil)
			r.Host = "hub.example.com"
			r.RemoteAddr = tc.remote
			r.TLS = nil
			if tc.tls {
				r.TLS = &tls.ConnectionState{}
			}
			for k, v := range tc.headers {
				r.Header[http.CanonicalHeaderKey(k)] = v
			}
			if got := requestBaseURLWith(r, tc.trusted); got != tc.want {
				t.Errorf("requestBaseURLWith = %q, want %q", got, tc.want)
			}
		})
	}
}
