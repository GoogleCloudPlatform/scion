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
	"net/http/httptest"
	"testing"
)

func mustTrusted(t *testing.T, entries ...string) []*net.IPNet {
	t.Helper()
	nets, err := ParseTrustedProxies(entries)
	if err != nil {
		t.Fatal(err)
	}
	return nets
}

func TestArrivalRefusal(t *testing.T) {
	remoteProxy := mustTrusted(t, "10.20.0.0/16")
	local := mustTrusted(t, "127.0.0.1", "::1")
	cases := []struct {
		name    string
		remote  string
		tls     bool
		headers map[string][]string
		trusted []*net.IPNet
		want    string
	}{
		{name: "accepted: TLS", remote: "192.0.2.1:1234", tls: true, want: ""},
		{name: "accepted: loopback without forwarding headers", remote: "127.0.0.1:5000", want: ""},
		{name: "accepted: trusted local proxy says https", remote: "127.0.0.1:5000", trusted: local,
			headers: map[string][]string{"X-Forwarded-Proto": {"https"}}, want: ""},

		{name: "local proxy, empty list", remote: "127.0.0.1:5000",
			headers: map[string][]string{"X-Forwarded-Proto": {"https"}}, want: ArrivalRefusalUntrustedLocalProxy},
		{name: "local IPv6 proxy, empty list, XFF only", remote: "[::1]:5000",
			headers: map[string][]string{"X-Forwarded-For": {"198.51.100.7"}}, want: ArrivalRefusalUntrustedLocalProxy},
		{name: "local proxy, list names another proxy", remote: "127.0.0.1:5000", trusted: remoteProxy,
			headers: map[string][]string{"X-Forwarded-Proto": {"https"}}, want: ArrivalRefusalLocalProxyNotTrusted},
		{name: "trusted remote proxy says http", remote: "10.20.3.4:1234", trusted: remoteProxy,
			headers: map[string][]string{"X-Forwarded-Proto": {"http"}}, want: ArrivalRefusalProxyProtoNotHTTPS},
		{name: "trusted local proxy appends the protocol", remote: "127.0.0.1:5000", trusted: local,
			headers: map[string][]string{"X-Forwarded-Proto": {"https, http"}}, want: ArrivalRefusalProxyProtoNotHTTPS},
		{name: "trusted remote proxy without forwarded protocol", remote: "10.20.3.4:1234", trusted: remoteProxy,
			want: ArrivalRefusalProxyProtoNotHTTPS},
		{name: "network peer, plain http", remote: "192.0.2.1:1234", want: ArrivalRefusalNotTLS},
		{name: "network peer forging XFP https", remote: "192.0.2.1:1234", trusted: local,
			headers: map[string][]string{"X-Forwarded-Proto": {"https"}}, want: ArrivalRefusalNotTLS},
		{name: "unparseable remote address", remote: "not-an-address", want: ArrivalRefusalNotTLS},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest("GET", "/api/v1/runtime-brokers/connect", nil)
			r.RemoteAddr = tc.remote
			if tc.tls {
				r.TLS = &tls.ConnectionState{}
			} else {
				r.TLS = nil
			}
			for k, vs := range tc.headers {
				for _, v := range vs {
					r.Header.Add(k, v)
				}
			}
			if got := ArrivalRefusal(r, tc.trusted); got != tc.want {
				t.Errorf("ArrivalRefusal = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestArrivalRefusal_ParityWithConfidentialArrival checks, over a grid of
// peers, headers, TLS states and trusted lists, that ArrivalRefusal returns
// "" exactly when ConfidentialArrival accepts, so the reason a broker is
// told can never disagree with the decision.
func TestArrivalRefusal_ParityWithConfidentialArrival(t *testing.T) {
	peers := []string{"127.0.0.1:1", "127.9.9.9:1", "[::1]:1", "10.20.3.4:1", "192.0.2.1:1", "[2001:db8::1]:1", "garbage"}
	headerSets := []map[string][]string{
		nil,
		{"X-Forwarded-Proto": {"https"}},
		{"X-Forwarded-Proto": {"http"}},
		{"X-Forwarded-Proto": {"https, http"}},
		{"X-Forwarded-Proto": {""}},
		{"X-Forwarded-Proto": {"https", "https"}},
		{"Forwarded": {"proto=https"}},
		{"Forwarded": {"proto=https, proto=https"}},
		{"X-Forwarded-Proto": {"https"}, "Forwarded": {"proto=http"}},
		{"X-Forwarded-For": {"198.51.100.7"}},
		{"X-Forwarded-Host": {"hub.example.com"}},
		{"X-Real-Ip": {"198.51.100.7"}},
	}
	trustedLists := map[string][]*net.IPNet{
		"none":   nil,
		"local":  mustTrusted(t, "127.0.0.1", "::1"),
		"remote": mustTrusted(t, "10.20.0.0/16"),
		"both":   mustTrusted(t, "127.0.0.1", "10.20.0.0/16", "2001:db8::/32"),
	}
	n := 0
	for _, peer := range peers {
		for hi, hs := range headerSets {
			for tname, trusted := range trustedLists {
				for _, withTLS := range []bool{false, true} {
					r := httptest.NewRequest("GET", "/", nil)
					r.RemoteAddr = peer
					r.TLS = nil
					if withTLS {
						r.TLS = &tls.ConnectionState{}
					}
					for k, vs := range hs {
						for _, v := range vs {
							r.Header.Add(k, v)
						}
					}
					accepted := ConfidentialArrival(r, trusted)
					reason := ArrivalRefusal(r, trusted)
					if accepted != (reason == "") {
						t.Errorf("peer %s headers #%d trusted %s tls %t: ConfidentialArrival=%t but ArrivalRefusal=%q",
							peer, hi, tname, withTLS, accepted, reason)
					}
					n++
				}
			}
		}
	}
	if n == 0 {
		t.Fatal("empty grid")
	}
}

func TestIsLocalProxyRefusal(t *testing.T) {
	for reason, want := range map[string]bool{
		ArrivalRefusalUntrustedLocalProxy:  true,
		ArrivalRefusalLocalProxyNotTrusted: true,
		ArrivalRefusalProxyProtoNotHTTPS:   false,
		ArrivalRefusalNotTLS:               false,
		"":                                 false,
	} {
		if got := IsLocalProxyRefusal(reason); got != want {
			t.Errorf("IsLocalProxyRefusal(%q) = %t, want %t", reason, got, want)
		}
	}
}
