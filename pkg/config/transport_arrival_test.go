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

package config

import (
	"crypto/tls"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestConfidentialArrival(t *testing.T) {
	proxy, err := ParseTrustedProxies([]string{"10.20.0.0/16"})
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name    string
		remote  string
		tls     bool
		headers map[string][]string
		trusted []*net.IPNet
		want    bool
	}{
		{name: "TLS terminated by the hub", remote: "192.0.2.1:1234", tls: true, want: true},
		{name: "loopback IPv4, no forwarding headers", remote: "127.0.0.1:5000", want: true},
		{name: "loopback IPv6, no forwarding headers", remote: "[::1]:5000", want: true},
		{name: "loopback 127/8, no forwarding headers", remote: "127.8.9.10:5000", want: true},
		{name: "network peer, plain http", remote: "192.0.2.1:1234", want: false},
		{name: "network peer, plain http, trusted list set", remote: "192.0.2.1:1234", trusted: proxy, want: false},

		// A forwarded protocol is never trusted with an empty list.
		{name: "forged XFP https, empty list", remote: "192.0.2.1:1234",
			headers: map[string][]string{"X-Forwarded-Proto": {"https"}}, want: false},
		{name: "forged Forwarded proto=https, empty list", remote: "192.0.2.1:1234",
			headers: map[string][]string{"Forwarded": {"for=198.51.100.7;proto=https"}}, want: false},
		{name: "loopback peer with XFP https, empty list", remote: "127.0.0.1:5000",
			headers: map[string][]string{"X-Forwarded-Proto": {"https"}}, want: false},
		{name: "loopback peer with X-Forwarded-For only, empty list", remote: "127.0.0.1:5000",
			headers: map[string][]string{"X-Forwarded-For": {"198.51.100.7"}}, want: false},

		// Non-empty list: only a listed peer's header counts.
		{name: "XFP https from a peer outside the list", remote: "192.0.2.1:1234", trusted: proxy,
			headers: map[string][]string{"X-Forwarded-Proto": {"https"}}, want: false},
		{name: "XFP https from a trusted proxy", remote: "10.20.3.4:1234", trusted: proxy,
			headers: map[string][]string{"X-Forwarded-Proto": {"https"}}, want: true},
		{name: "XFP HTTPS (case) from a trusted proxy", remote: "10.20.3.4:1234", trusted: proxy,
			headers: map[string][]string{"X-Forwarded-Proto": {"HTTPS"}}, want: true},
		{name: "Forwarded proto=https from a trusted proxy", remote: "10.20.3.4:1234", trusted: proxy,
			headers: map[string][]string{"Forwarded": {`for="198.51.100.7";proto="https"`}}, want: true},
		{name: "XFP and Forwarded both https from a trusted proxy", remote: "10.20.3.4:1234", trusted: proxy,
			headers: map[string][]string{"X-Forwarded-Proto": {"https"}, "Forwarded": {"proto=https"}}, want: true},
		{name: "XFP http from a trusted proxy", remote: "10.20.3.4:1234", trusted: proxy,
			headers: map[string][]string{"X-Forwarded-Proto": {"http"}}, want: false},
		{name: "no forwarded protocol from a trusted proxy", remote: "10.20.3.4:1234", trusted: proxy,
			headers: map[string][]string{"X-Forwarded-For": {"198.51.100.7"}}, want: false},
		{name: "appended XFP list from a trusted proxy", remote: "10.20.3.4:1234", trusted: proxy,
			headers: map[string][]string{"X-Forwarded-Proto": {"https, http"}}, want: false},
		{name: "two XFP lines from a trusted proxy", remote: "10.20.3.4:1234", trusted: proxy,
			headers: map[string][]string{"X-Forwarded-Proto": {"https", "https"}}, want: false},
		{name: "empty XFP from a trusted proxy", remote: "10.20.3.4:1234", trusted: proxy,
			headers: map[string][]string{"X-Forwarded-Proto": {""}}, want: false},
		{name: "XFP and Forwarded disagree from a trusted proxy", remote: "10.20.3.4:1234", trusted: proxy,
			headers: map[string][]string{"X-Forwarded-Proto": {"https"}, "Forwarded": {"proto=http"}}, want: false},
		{name: "two Forwarded elements from a trusted proxy", remote: "10.20.3.4:1234", trusted: proxy,
			headers: map[string][]string{"Forwarded": {"proto=https, proto=https"}}, want: false},
		{name: "Forwarded with two proto pairs from a trusted proxy", remote: "10.20.3.4:1234", trusted: proxy,
			headers: map[string][]string{"Forwarded": {"proto=http;proto=https"}}, want: false},

		{name: "unparseable RemoteAddr", remote: "not-an-address", want: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodPost, "/api/v1/brokers/join", nil)
			r.RemoteAddr = tc.remote
			if tc.tls {
				r.TLS = &tls.ConnectionState{}
			} else {
				r.TLS = nil
			}
			for k, v := range tc.headers {
				r.Header[http.CanonicalHeaderKey(k)] = v
			}
			if got := ConfidentialArrival(r, tc.trusted); got != tc.want {
				t.Errorf("ConfidentialArrival = %v, want %v", got, tc.want)
			}
		})
	}
}
