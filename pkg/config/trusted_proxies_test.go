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
