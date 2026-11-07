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

import "testing"

// TestValidateContentURL is the condition 5 validation table for the
// value's own shape (kill check K10: accepting a path fails it).
func TestValidateContentURL(t *testing.T) {
	accepted := []struct{ raw, want string }{
		{"", ""},
		{"https://h", "https://h"},
		{"https://h:8443", "https://h:8443"},
		{"https://links.example.net", "https://links.example.net"},
		{"HTTPS://Links.Example.NET", "https://links.example.net"},
		{"https://links.example.net/", "https://links.example.net"},
		{"https://links.example.net:443", "https://links.example.net"},
		{"https://203.0.113.7:8443", "https://203.0.113.7:8443"},
		{"http://localhost:9000", "http://localhost:9000"},
		{"http://127.0.0.1:9000", "http://127.0.0.1:9000"},
		{"http://localhost:80", "http://localhost"},
	}
	for _, tc := range accepted {
		got, err := ValidateContentURL(tc.raw)
		if err != nil || got != tc.want {
			t.Errorf("ValidateContentURL(%q) = %q, %v; want %q", tc.raw, got, err, tc.want)
		}
	}
	rejected := map[string]string{
		"path":                 "https://h/share",
		"nested path":          "https://h/a/b",
		"encoded path":         "https://h/%2F",
		"query":                "https://h?x=1",
		"empty query":          "https://h?",
		"fragment":             "https://h#f",
		"empty fragment":       "https://h#",
		"userinfo":             "https://u:p@h",
		"user only":            "https://u@h",
		"http non-loopback":    "http://links.example.net",
		"empty host":           "https://",
		"empty host with port": "https://:8443",
		"no scheme":            "links.example.net",
		"other scheme":         "ftp://h",
		"opaque":               "https:h",
		"ipv6":                 "https://[2001:db8::1]",
		"underscore host":      "https://a_b.example.net",
		"empty label":          "https://a..b",
		"port zero":            "https://h:0",
		"port too large":       "https://h:65536",
		"port not numeric":     "https://h:x",
		"empty port":           "https://h:",
		"leading zero port":    "https://h:08443",
		"space":                "https://h x",
	}
	for name, raw := range rejected {
		if got, err := ValidateContentURL(raw); err == nil {
			t.Errorf("%s: ValidateContentURL(%q) = %q, want an error", name, raw, got)
		}
	}
}

// TestContentURLHubConflict: a value on the hub's own host name, whatever
// its scheme or port, is a conflict (refused on save, ignored at runtime;
// kill check K11), and one sharing the hub's registrable domain is flagged
// for the warning (condition 5).
func TestContentURLHubConflict(t *testing.T) {
	for _, tc := range []struct {
		name, cu, hub      string
		wantSame, wantSite bool
	}{
		{"same origin", "https://hub.example.com", "https://hub.example.com", true, true},
		{"hub URL with a path", "https://hub.example.com", "https://hub.example.com/", true, true},
		{"explicit default port", "https://hub.example.com", "https://hub.example.com:443", true, true},
		{"host case differs", "https://hub.example.com", "https://HUB.example.com", true, true},
		{"hub host with a trailing dot", "https://hub.example.com", "https://hub.example.com.", true, true},
		{"same host, other port", "https://hub.example.com:8443", "https://hub.example.com", true, true},
		{"same host, other scheme", "https://localhost:8080", "http://localhost:8080", true, true},
		{"same loopback host, other port", "http://localhost:9000", "http://localhost:8080", true, true},
		{"other loopback name", "http://127.0.0.1:9000", "http://localhost:8080", false, false},
		{"same site", "https://links.example.com", "https://hub.example.com", false, true},
		{"same site under a public suffix", "https://a.example.co.uk", "https://b.example.co.uk", false, true},
		{"different site", "https://links.example.net", "https://hub.example.com", false, false},
		{"different public suffix tenants", "https://a.github.io", "https://b.github.io", false, false},
		{"unparseable hub", "https://links.example.net", "::", false, false},
		{"empty hub", "https://links.example.net", "", false, false},
		{"empty content URL", "", "https://hub.example.com", false, false},
		{"invalid content URL", "https://hub.example.com/x", "https://hub.example.com", false, false},
	} {
		same, site := ContentURLHubConflict(tc.cu, tc.hub)
		if same != tc.wantSame || site != tc.wantSite {
			t.Errorf("%s: ContentURLHubConflict(%q, %q) = %v, %v; want %v, %v",
				tc.name, tc.cu, tc.hub, same, site, tc.wantSame, tc.wantSite)
		}
	}
}

// TestParseContentURL: the one parse splits a valid value into host, port
// and default port, and refuses what ValidateContentURL refuses.
func TestParseContentURL(t *testing.T) {
	for raw, want := range map[string]ContentURLParts{
		"https://links.example.net":      {Host: "links.example.net", Port: "443", DefaultPort: "443"},
		"HTTPS://Links.Example.NET:443/": {Host: "links.example.net", Port: "443", DefaultPort: "443"},
		"https://links.example.net:8443": {Host: "links.example.net", Port: "8443", DefaultPort: "443"},
		"http://localhost:9000":          {Host: "localhost", Port: "9000", DefaultPort: "80"},
	} {
		got, ok := ParseContentURL(raw)
		if !ok || got != want {
			t.Errorf("ParseContentURL(%q) = %+v, %v; want %+v", raw, got, ok, want)
		}
	}
	for _, raw := range []string{"", "https://h/x", "http://links.example.net", "ftp://h", "https://u@h", "https://[::1]"} {
		if got, ok := ParseContentURL(raw); ok {
			t.Errorf("ParseContentURL(%q) = %+v, want refused", raw, got)
		}
	}
}

// TestRequestOnHubHost: a request Host on the hub's host name, at any port,
// is on the hub host; any other is not.
func TestRequestOnHubHost(t *testing.T) {
	hub := "https://hub.example.com"
	for host, want := range map[string]bool{
		"hub.example.com":      true,
		"HUB.example.com:8443": true,
		"hub.example.com.":     true,
		"links.example.net":    false,
		"hub.example.com.evil": false,
		"xhub.example.com":     false,
		"":                     false,
	} {
		if got := RequestOnHubHost(host, hub); got != want {
			t.Errorf("RequestOnHubHost(%q) = %v, want %v", host, got, want)
		}
	}
	if RequestOnHubHost("hub.example.com", "") {
		t.Error("an empty hub URL matched")
	}
}

// TestContentURLSameHost: a content URL on another URL's host name, whatever
// the scheme and port, matches; any other host does not. The hub uses it
// for server.hub.agent_endpoint (kill check K62).
func TestContentURLSameHost(t *testing.T) {
	for _, tc := range []struct {
		name, cu, other string
		want            bool
	}{
		{"agent endpoint host", "https://agents.example.net", "http://agents.example.net:9810", true},
		{"case and trailing dot", "https://Agents.Example.NET", "https://agents.example.net.", true},
		{"other host", "https://content.example.net", "http://agents.example.net:9810", false},
		{"subdomain is not the host", "https://a.agents.example.net", "https://agents.example.net", false},
		{"empty other", "https://content.example.net", "", false},
		{"invalid content URL", "https://agents.example.net/x", "https://agents.example.net", false},
	} {
		if got := ContentURLSameHost(tc.cu, tc.other); got != tc.want {
			t.Errorf("%s: ContentURLSameHost(%q, %q) = %v, want %v", tc.name, tc.cu, tc.other, got, tc.want)
		}
	}
}

// TestContentURLAllowSameSiteEnv: SCION_SERVER_HUB_CONTENTURLALLOWSAMESITE
// reaches GlobalConfig.Hub.ContentURLAllowSameSite and the opsettings key
// through the generic env mapping (no table entry), as
// SCION_SERVER_HUB_CONTENTURL does (ptone/scion#7107 phase B item 1).
func TestContentURLAllowSameSiteEnv(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("SCION_SERVER_HUB_CONTENTURLALLOWSAMESITE", "true")
	cfg, err := LoadGlobalConfig("")
	if err != nil {
		t.Fatalf("LoadGlobalConfig: %v", err)
	}
	if !cfg.Hub.ContentURLAllowSameSite {
		t.Errorf("SCION_SERVER_HUB_CONTENTURLALLOWSAMESITE=true did not reach GlobalConfig.Hub.ContentURLAllowSameSite")
	}
	if !LoadEnvKoanf().Bool(ContentURLAllowSameSiteKey) {
		t.Errorf("SCION_SERVER_HUB_CONTENTURLALLOWSAMESITE=true did not reach %s", ContentURLAllowSameSiteKey)
	}
}

// TestContentURLAllowSameSiteV1RoundTrip: settings.yaml (v1) and
// GlobalConfig carry the override both ways, true and absent.
func TestContentURLAllowSameSiteV1RoundTrip(t *testing.T) {
	on := true
	gc := ConvertV1ServerToGlobalConfig(&V1ServerConfig{Hub: &V1ServerHubConfig{ContentURLAllowSameSite: &on}})
	if !gc.Hub.ContentURLAllowSameSite {
		t.Fatalf("v1 -> GlobalConfig lost true")
	}
	back := ConvertGlobalToV1ServerConfig(gc)
	if back.Hub == nil || back.Hub.ContentURLAllowSameSite == nil || !*back.Hub.ContentURLAllowSameSite {
		t.Fatalf("GlobalConfig -> v1 lost true: %+v", back.Hub)
	}

	gc = ConvertV1ServerToGlobalConfig(&V1ServerConfig{Hub: &V1ServerHubConfig{ContentURL: "https://links.example.net"}})
	if gc.Hub.ContentURLAllowSameSite {
		t.Fatalf("v1 without the key -> GlobalConfig set it")
	}
	back = ConvertGlobalToV1ServerConfig(gc)
	if back.Hub == nil || back.Hub.ContentURLAllowSameSite != nil {
		t.Fatalf("GlobalConfig without the key -> v1 set it: %+v", back.Hub)
	}
}
