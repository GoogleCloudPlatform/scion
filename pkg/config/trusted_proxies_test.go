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
	"net"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestParseTrustedProxies_Accepts(t *testing.T) {
	nets, err := ParseTrustedProxies([]string{"10.0.0.0/8", "192.0.2.7", " 2001:db8::/32 ", "2001:db8::1", "127.0.0.1", "::ffff:10.0.0.7"})
	if err != nil {
		t.Fatal(err)
	}
	if len(nets) != 6 {
		t.Fatalf("got %d networks, want 6", len(nets))
	}
	if ones, bits := nets[1].Mask.Size(); ones != 32 || bits != 32 {
		t.Errorf("bare IPv4 entry mask = /%d of %d, want /32", ones, bits)
	}
	if ones, bits := nets[3].Mask.Size(); ones != 128 || bits != 128 {
		t.Errorf("bare IPv6 entry mask = /%d of %d, want /128", ones, bits)
	}
	if !nets[1].Contains(net.ParseIP("192.0.2.7")) || nets[1].Contains(net.ParseIP("192.0.2.8")) {
		t.Error("bare IPv4 entry does not match exactly its address")
	}
}

func TestParseTrustedProxies_EmptyMeansNone(t *testing.T) {
	for _, in := range [][]string{nil, {}} {
		nets, err := ParseTrustedProxies(in)
		if err != nil || len(nets) != 0 {
			t.Errorf("ParseTrustedProxies(%v) = %v, %v; want no networks and no error", in, nets, err)
		}
	}
}

func TestParseTrustedProxies_Refuses(t *testing.T) {
	for _, entry := range []string{
		"0.0.0.0/0",
		"::/0",
		"0.0.0.0/1",
		"10.0.0.0/7",
		"2001::/15",
		"::ffff:0:0/96",
		"::ffff:10.0.0.0/103",
		"::ffff:10.0.0.0/104",
		"::ffff:10.20.0.0/112",
		"proxy.example.com",
		"10.0.0.0/33",
		"10.0.0.1-10.0.0.9",
		"",
	} {
		_, err := ParseTrustedProxies([]string{"10.1.2.3", entry})
		if err == nil {
			t.Errorf("entry %q accepted, want a refusal", entry)
			continue
		}
		if !strings.Contains(err.Error(), TrustedProxiesKey) {
			t.Errorf("entry %q: error %q does not name %s", entry, err, TrustedProxiesKey)
		}
	}
}

// server.trusted_proxies reaches GlobalConfig from the settings file and
// from SCION_SERVER_TRUSTEDPROXIES (and its alias), comma-separated values
// split and trimmed.
func TestTrustedProxies_LoadFromFileAndEnv(t *testing.T) {
	var entry ServerEnvEntry
	for _, e := range ServerEnvEntries() {
		if e.Path == TrustedProxiesKey {
			entry = e
		}
	}
	if entry.Canonical != "SCION_SERVER_TRUSTEDPROXIES" {
		t.Fatalf("canonical env name = %q, want SCION_SERVER_TRUSTEDPROXIES", entry.Canonical)
	}
	for name, tc := range map[string]struct {
		file string
		env  map[string]string
		want []string
	}{
		"file list": {
			file: "schema_version: \"1\"\nserver:\n  trusted_proxies:\n    - 10.0.0.0/8\n    - 192.0.2.7\n",
			want: []string{"10.0.0.0/8", "192.0.2.7"},
		},
		"file comma string item": {
			file: "schema_version: \"1\"\nserver:\n  trusted_proxies:\n    - \"10.0.0.0/8, 192.0.2.7\"\n",
			want: []string{"10.0.0.0/8", "192.0.2.7"},
		},
		"env canonical": {
			file: "schema_version: \"1\"\n",
			env:  map[string]string{"SCION_SERVER_TRUSTEDPROXIES": "10.0.0.0/8, 192.0.2.7"},
			want: []string{"10.0.0.0/8", "192.0.2.7"},
		},
		"env alias": {
			file: "schema_version: \"1\"\n",
			env:  map[string]string{"SCION_SERVER_TRUSTED_PROXIES": "192.0.2.7"},
			want: []string{"192.0.2.7"},
		},
		"unset": {
			file: "schema_version: \"1\"\n",
			want: nil,
		},
	} {
		t.Run(name, func(t *testing.T) {
			home := t.TempDir()
			t.Setenv("HOME", home)
			configPath := filepath.Join(home, "settings.yaml")
			if err := os.WriteFile(configPath, []byte(tc.file), 0o600); err != nil {
				t.Fatal(err)
			}
			for k, v := range tc.env {
				t.Setenv(k, v)
			}
			gc, err := LoadGlobalConfig(configPath)
			if err != nil {
				t.Fatal(err)
			}
			if strings.Join(gc.TrustedProxies, "|") != strings.Join(tc.want, "|") {
				t.Errorf("TrustedProxies = %q, want %q", gc.TrustedProxies, tc.want)
			}
			back := ConvertV1ServerToGlobalConfig(ConvertGlobalToV1ServerConfig(gc))
			if strings.Join(back.TrustedProxies, "|") != strings.Join(tc.want, "|") {
				t.Errorf("after both conversions TrustedProxies = %q, want %q", back.TrustedProxies, tc.want)
			}
		})
	}
}

// An IPv4-mapped IPv6 range never matches a peer, so it is refused with a
// hint at the IPv4 form; a bare IPv4-mapped address is taken as its IPv4 /32.
func TestParseTrustedProxies_IPv4MappedRangeRefusedWithHint(t *testing.T) {
	_, err := ParseTrustedProxies([]string{"::ffff:10.20.0.0/112"})
	if err == nil || !strings.Contains(err.Error(), "10.20.0.0/16") {
		t.Errorf("error = %v, want a refusal naming the IPv4 form 10.20.0.0/16", err)
	}
	nets, err := ParseTrustedProxies([]string{"::ffff:10.0.0.7"})
	if err != nil {
		t.Fatal(err)
	}
	if !nets[0].Contains(net.ParseIP("10.0.0.7")) {
		t.Error("a bare IPv4-mapped address does not match its IPv4 peer")
	}
}

// clearTrustedProxiesEnv unsets every env name of server.trusted_proxies
// for the test, so an inherited value cannot mark it set.
func clearTrustedProxiesEnv(t *testing.T) {
	t.Helper()
	for _, name := range trustedProxiesEnvNames() {
		t.Setenv(name, "") // restored after the test
		if err := os.Unsetenv(name); err != nil {
			t.Fatal(err)
		}
	}
}

// server.trusted_proxies is "set" when any source names it: settings.yaml
// as a list (even []), legacy server.yaml (even as []), or an env name
// that is defined, even as an empty string. Otherwise it is not set. A
// null value in settings.yaml is not set: the hub's own rewrites of
// settings.yaml drop it (omitempty), so it could not stay set.
func TestTrustedProxies_SetSemantics(t *testing.T) {
	for name, tc := range map[string]struct {
		settings   string // settings.yaml content, "" for none
		serverYAML string // legacy server.yaml content, "" for none
		env        map[string]string
		wantSet    bool
		wantList   []string
	}{
		"settings list":           {settings: "schema_version: \"1\"\nserver:\n  trusted_proxies:\n    - 10.0.0.0/8\n", wantSet: true, wantList: []string{"10.0.0.0/8"}},
		"settings explicit empty": {settings: "schema_version: \"1\"\nserver:\n  trusted_proxies: []\n", wantSet: true},
		"settings null":           {settings: "schema_version: \"1\"\nserver:\n  trusted_proxies:\n"},
		"settings key absent":     {settings: "schema_version: \"1\"\nserver:\n  log_level: info\n"},
		"no server key":           {settings: "schema_version: \"1\"\n"},
		"env canonical empty":     {settings: "schema_version: \"1\"\nserver:\n  log_level: info\n", env: map[string]string{"SCION_SERVER_TRUSTEDPROXIES": ""}, wantSet: true},
		"env alias empty":         {settings: "schema_version: \"1\"\n", env: map[string]string{"SCION_SERVER_TRUSTED_PROXIES": ""}, wantSet: true},
		"env canonical list":      {settings: "schema_version: \"1\"\n", env: map[string]string{"SCION_SERVER_TRUSTEDPROXIES": "192.0.2.7"}, wantSet: true, wantList: []string{"192.0.2.7"}},
		"legacy explicit empty":   {serverYAML: "trustedProxies: []\n", wantSet: true},
		"legacy list":             {serverYAML: "trustedProxies:\n  - 192.0.2.7\n", wantSet: true, wantList: []string{"192.0.2.7"}},
		"legacy key absent":       {serverYAML: "logLevel: info\n"},
	} {
		t.Run(name, func(t *testing.T) {
			clearTrustedProxiesEnv(t)
			home := t.TempDir()
			t.Setenv("HOME", home)
			if tc.settings != "" {
				if err := os.WriteFile(filepath.Join(home, "settings.yaml"), []byte(tc.settings), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			if tc.serverYAML != "" {
				if err := os.WriteFile(filepath.Join(home, "server.yaml"), []byte(tc.serverYAML), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			for k, v := range tc.env {
				t.Setenv(k, v)
			}
			gc, err := LoadGlobalConfig(home)
			if err != nil {
				t.Fatal(err)
			}
			if gc.TrustedProxiesSet != tc.wantSet {
				t.Errorf("TrustedProxiesSet = %t, want %t", gc.TrustedProxiesSet, tc.wantSet)
			}
			if strings.Join(gc.TrustedProxies, "|") != strings.Join(tc.wantList, "|") {
				t.Errorf("TrustedProxies = %q, want %q", gc.TrustedProxies, tc.wantList)
			}
		})
	}
}

func netStrings(nets []*net.IPNet) []string {
	out := make([]string, 0, len(nets))
	for _, n := range nets {
		out = append(out, n.String())
	}
	return out
}

// Not set: loopback is trusted. Set (even empty): exactly the list.
func TestTransportTrustedProxies(t *testing.T) {
	nets, usedDefault, err := TransportTrustedProxies(nil, false)
	if err != nil || !usedDefault {
		t.Fatalf("not set: usedDefault=%t err=%v", usedDefault, err)
	}
	if got := strings.Join(netStrings(nets), ","); got != "127.0.0.1/32,::1/128" {
		t.Errorf("not set: nets = %s, want 127.0.0.1/32,::1/128", got)
	}

	nets, usedDefault, err = TransportTrustedProxies(nil, true)
	if err != nil || usedDefault || len(nets) != 0 {
		t.Errorf("explicit empty: nets=%v usedDefault=%t err=%v, want none", nets, usedDefault, err)
	}
	nets, usedDefault, err = TransportTrustedProxies([]string{}, true)
	if err != nil || usedDefault || len(nets) != 0 {
		t.Errorf("explicit empty slice: nets=%v usedDefault=%t err=%v, want none", nets, usedDefault, err)
	}

	nets, usedDefault, err = TransportTrustedProxies([]string{"10.20.0.0/16"}, true)
	if err != nil || usedDefault || strings.Join(netStrings(nets), ",") != "10.20.0.0/16" {
		t.Errorf("list: nets=%v usedDefault=%t err=%v", nets, usedDefault, err)
	}
	// A list without the set flag (a caller that did not track it) is
	// still the operator's list, never widened to the default.
	nets, usedDefault, err = TransportTrustedProxies([]string{"10.20.0.0/16"}, false)
	if err != nil || usedDefault || strings.Join(netStrings(nets), ",") != "10.20.0.0/16" {
		t.Errorf("list without flag: nets=%v usedDefault=%t err=%v", nets, usedDefault, err)
	}
	if _, _, err := TransportTrustedProxies([]string{"0.0.0.0/0"}, true); err == nil {
		t.Error("a catch-all entry is refused")
	}

	a, b := DefaultLocalTrustedProxies(), DefaultLocalTrustedProxies()
	a[0].IP[0] = 10
	if b[0].String() != "127.0.0.1/32" {
		t.Error("DefaultLocalTrustedProxies returns shared values")
	}
}

// Under the default (not set), a proxy on this host saying https is
// accepted; a peer on another host sending the same header is refused, and
// so is an appended or ambiguous forwarded protocol from the local proxy.
// With an explicit empty list, the local proxy is refused.
func TestTransportTrustedProxies_DefaultArrival(t *testing.T) {
	def, _, err := TransportTrustedProxies(nil, false)
	if err != nil {
		t.Fatal(err)
	}
	empty, _, err := TransportTrustedProxies(nil, true)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name    string
		remote  string
		headers map[string][]string
		trusted []*net.IPNet
		want    string
	}{
		{name: "local IPv4 proxy, https", remote: "127.0.0.1:5000", trusted: def,
			headers: map[string][]string{"X-Forwarded-Proto": {"https"}, "X-Forwarded-For": {"198.51.100.7"}}, want: ""},
		{name: "local IPv6 proxy, Forwarded https", remote: "[::1]:5000", trusted: def,
			headers: map[string][]string{"Forwarded": {"for=198.51.100.7;proto=https"}}, want: ""},
		{name: "remote peer with XFP https", remote: "192.0.2.10:5000", trusted: def,
			headers: map[string][]string{"X-Forwarded-Proto": {"https"}}, want: ArrivalRefusalNotTLS},
		{name: "local proxy appends the protocol", remote: "127.0.0.1:5000", trusted: def,
			headers: map[string][]string{"X-Forwarded-Proto": {"https, http"}}, want: ArrivalRefusalProxyProtoNotHTTPS},
		{name: "local proxy with an empty item", remote: "127.0.0.1:5000", trusted: def,
			headers: map[string][]string{"X-Forwarded-Proto": {"https,"}}, want: ArrivalRefusalProxyProtoNotHTTPS},
		{name: "local proxy with two XFP lines", remote: "127.0.0.1:5000", trusted: def,
			headers: map[string][]string{"X-Forwarded-Proto": {"https", "https"}}, want: ArrivalRefusalProxyProtoNotHTTPS},
		{name: "local proxy, Forwarded and XFP disagree", remote: "127.0.0.1:5000", trusted: def,
			headers: map[string][]string{"X-Forwarded-Proto": {"https"}, "Forwarded": {"proto=http"}}, want: ArrivalRefusalProxyProtoNotHTTPS},
		{name: "local proxy says http", remote: "127.0.0.1:5000", trusted: def,
			headers: map[string][]string{"X-Forwarded-Proto": {"http"}}, want: ArrivalRefusalProxyProtoNotHTTPS},
		{name: "explicit empty list, local proxy https", remote: "127.0.0.1:5000", trusted: empty,
			headers: map[string][]string{"X-Forwarded-Proto": {"https"}}, want: ArrivalRefusalUntrustedLocalProxy},
		{name: "explicit empty list, loopback client without headers", remote: "127.0.0.1:5000", trusted: empty, want: ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest("GET", "/api/v1/runtime-brokers/connect", nil)
			r.RemoteAddr = tc.remote
			r.TLS = nil
			for k, vs := range tc.headers {
				for _, v := range vs {
					r.Header.Add(k, v)
				}
			}
			if got := ArrivalRefusal(r, tc.trusted); got != tc.want {
				t.Errorf("ArrivalRefusal = %q, want %q", got, tc.want)
			}
			if got := ConfidentialArrival(r, tc.trusted); got != (tc.want == "") {
				t.Errorf("ConfidentialArrival = %t, want %t", got, tc.want == "")
			}
		})
	}
}

// The startup warning names the [] opt-out and says a proxy on another
// host must be listed.
func TestTransportTrustedProxiesDefaultWarning(t *testing.T) {
	for _, want := range []string{TrustedProxiesKey + " is not set", "on this host only", "127.0.0.1, ::1", "another host must be listed", "set it to []"} {
		if !strings.Contains(TransportTrustedProxiesDefaultWarning, want) {
			t.Errorf("warning %q lacks %q", TransportTrustedProxiesDefaultWarning, want)
		}
	}
}
