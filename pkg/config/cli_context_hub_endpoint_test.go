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
	"os"
	"path/filepath"
	"testing"
	"time"
)

// Spellings of one hub URL get one key; URLs of different hubs never do.
func TestNormalizeEndpoint_SameAndDifferentHubs(t *testing.T) {
	same := [][2]string{
		{"https://hub.example.com", "HTTPS://HUB.EXAMPLE.COM"},
		{"https://hub.example.com", "https://hub.example.com/"},
		{"https://hub.example.com", "https://hub.example.com:443"},
		{"http://hub.example.com", "http://hub.example.com:80/"},
		{"https://hub.example.com/base", "https://Hub.Example.com/base/"},
		{"http://[::1]:8080", "http://[::1]:8080/"},
		{"https://hub.example.com", "  https://hub.example.com  "},
		// One host spelt literally and percent-encoded is the same bytes.
		{"https://bücher.example", "https://b%C3%BCcher.example"},
	}
	for _, p := range same {
		if a, b := NormalizeEndpoint(p[0]), NormalizeEndpoint(p[1]); a != b {
			t.Errorf("NormalizeEndpoint(%q) = %q and NormalizeEndpoint(%q) = %q, want equal", p[0], a, p[1], b)
		}
	}

	different := [][2]string{
		// Scheme.
		{"http://hub.example.com", "https://hub.example.com"},
		// Ports: a non-default port is kept, and a default port belongs to
		// its own scheme only.
		{"https://hub.example.com", "https://hub.example.com:8443"},
		{"http://hub.example.com", "http://hub.example.com:443"},
		{"https://hub.example.com", "https://hub.example.com:80"},
		{"https://hub.example.com:443", "https://hub.example.com:0443"},
		// Path: case and escaping are kept.
		{"https://hub.example.com/Base", "https://hub.example.com/base"},
		{"https://hub.example.com/a/b", "https://hub.example.com/a%2Fb"},
		{"https://hub.example.com", "https://hub.example.com/base"},
		// Trailing dot: kept as written.
		{"https://hub.example.com", "https://hub.example.com."},
		// IDN and punycode spellings are different keys.
		{"https://bücher.example", "https://xn--bcher-kva.example"},
		// Unicode case mapping is not applied: KELVIN SIGN is not "k",
		// and a non-ASCII capital is not lowercased.
		{"https://\u212Aub.example.com", "https://kub.example.com"},
		{"https://Über.example", "https://über.example"},
		// Different hosts and IP spellings.
		{"https://hub.example.com", "https://hub.example.org"},
		{"http://[::1]:8080", "http://[0:0:0:0:0:0:0:1]:8080"},
		{"http://127.0.0.1:8080", "http://localhost:8080"},
	}
	for _, p := range different {
		if a, b := NormalizeEndpoint(p[0]), NormalizeEndpoint(p[1]); a == b {
			t.Errorf("NormalizeEndpoint(%q) and NormalizeEndpoint(%q) are both %q, want different", p[0], p[1], a)
		}
	}
}

// The trailing dot and a non-ASCII host are kept as written in the key.
func TestNormalizeEndpoint_KeepsTrailingDotAndNonASCIIHost(t *testing.T) {
	tests := map[string]string{
		"https://Hub.Example.COM.":         "https://hub.example.com.",
		"https://XN--BCHER-KVA.example":    "https://xn--bcher-kva.example",
		"https://\u212Aub.Example.com:443": NormalizeEndpoint("https://\u212Aub.example.com"),
	}
	for in, want := range tests {
		if got := NormalizeEndpoint(in); got != want {
			t.Errorf("NormalizeEndpoint(%q) = %q, want %q", in, got, want)
		}
	}
}

// SelectHubURL adds an entry named from the host, reuses an entry with the
// same normalised endpoint, numbers a name collision, and makes the hub
// current.
func TestCLIContextSelectHubURL(t *testing.T) {
	c := newCLIContext()
	name, created, err := c.SelectHubURL("https://Hub.Example.com/", "hub")
	if err != nil || name != "hub" || !created || c.CurrentHub != "hub" {
		t.Fatalf("SelectHubURL = %q, %v, %v (current %q); want hub, created, current", name, created, err, c.CurrentHub)
	}
	if got := c.Hubs["hub"].Endpoint; got != "https://hub.example.com" {
		t.Errorf("endpoint = %q", got)
	}

	c.CurrentHub = ""
	name, created, err = c.SelectHubURL("https://hub.example.com:443", "other")
	if err != nil || name != "hub" || created || c.CurrentHub != "hub" || len(c.Hubs) != 1 {
		t.Errorf("same endpoint: %q, %v, %v, hubs %v; want the existing entry reused and current", name, created, err, c.HubNames())
	}

	name, created, err = c.SelectHubURL("https://hub.example.org", "hub")
	if err != nil || name != "hub-2" || !created || c.CurrentHub != "hub-2" {
		t.Errorf("name collision: %q, %v, %v; want hub-2", name, created, err)
	}
}

// SelectHubURL applies the hub add URL rule and changes nothing on a
// refused URL.
func TestCLIContextSelectHubURL_RefusesWhatHubAddRefuses(t *testing.T) {
	refused := []string{
		"", "hub.example.com", "ftp://hub.example.com", "https://",
		"https://user:placeholder@hub.example.com",
		"https://token@hub.example.com",
		"https://hub.example.com/?x=1", "https://hub.example.com/?",
		"https://hub.example.com/#frag", "https://hub.example.com/#",
	}
	for _, u := range refused {
		c := newCLIContext()
		c.Hubs = map[string]*HubEntry{"prod": {Endpoint: "https://prod.example.com"}}
		c.CurrentHub = "prod"
		if _, _, err := c.SelectHubURL(u, "hub"); err == nil {
			t.Errorf("SelectHubURL(%q): nil error, want refused", u)
		}
		if addErr := newCLIContext().AddHub("x", u); addErr == nil {
			t.Errorf("AddHub(%q) accepted a URL SelectHubURL refuses", u)
		}
		if c.CurrentHub != "prod" || len(c.Hubs) != 1 {
			t.Errorf("SelectHubURL(%q) changed the context: %+v", u, c)
		}
	}
	// Plain http is accepted for any host, as hub add accepts it.
	for _, u := range []string{"http://hub.example.com", "http://127.0.0.1:8080"} {
		if _, _, err := newCLIContext().SelectHubURL(u, "hub"); err != nil {
			t.Errorf("SelectHubURL(%q): %v", u, err)
		}
		if err := newCLIContext().AddHub("x", u); err != nil {
			t.Errorf("AddHub(%q): %v", u, err)
		}
	}
}

// ClearCurrentHub makes no hub current and keeps the entry.
func TestCLIContextClearCurrentHub(t *testing.T) {
	c := linkTestContext()
	c.CurrentHub = "prod"
	if got := c.ClearCurrentHub(); got != "prod" {
		t.Errorf("ClearCurrentHub = %q, want prod", got)
	}
	if c.CurrentHub != "" || c.Hubs["prod"] == nil {
		t.Errorf("context = %+v; want no current hub and the entry kept", c)
	}
	if got := c.ClearCurrentHub(); got != "" {
		t.Errorf("second ClearCurrentHub = %q, want empty", got)
	}
}

// LinkPathToHubURL writes a link entry for the path, adding the hub entry
// when needed; a link to the same hub is kept with its ID; a link to
// another hub is replaced; the current hub never changes.
func TestCLIContextLinkPathToHubURL(t *testing.T) {
	now := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	c := linkTestContext()
	c.CurrentHub = "local"

	name, created, err := c.LinkPathToHubURL("https://new.example.com", "new", "proj-1", "/work/repo", now)
	if err != nil || name != "new" || !created {
		t.Fatalf("LinkPathToHubURL = %q, %v, %v", name, created, err)
	}
	p, h := c.LinkEntryFor("/work/repo")
	if p == nil || p.ID != "proj-1" || p.Hub != "new" || h.Endpoint != "https://new.example.com" {
		t.Fatalf("link entry = %+v, hub %+v", p, h)
	}
	if c.CurrentHub != "local" {
		t.Errorf("current hub = %q: linking must not change it", c.CurrentHub)
	}

	// The same hub, spelt differently: the link (and its ID) is kept.
	name, created, err = c.LinkPathToHubURL("HTTPS://new.example.com:443/", "x", "other-id", "/work/repo", now)
	if err != nil || name != "new" || created {
		t.Errorf("same hub: %q, %v, %v", name, created, err)
	}
	if p, _ := c.LinkEntryFor("/work/repo"); p == nil || p.ID != "proj-1" {
		t.Errorf("same hub: link entry = %+v, want proj-1 kept", p)
	}

	// An existing hub entry: reused, and the link replaced.
	name, created, err = c.LinkPathToHubURL("https://hub.example.com", "x", "proj-2", "/work/repo", now)
	if err != nil || name != "prod" || created {
		t.Errorf("existing hub: %q, %v, %v", name, created, err)
	}
	if p, _ := c.LinkEntryFor("/work/repo"); p == nil || p.Hub != "prod" || p.ID != "proj-2" {
		t.Errorf("replaced link entry = %+v", p)
	}
	links := 0
	for _, kp := range c.Projects {
		if kp.Source == KnownProjectSourceLink && kp.LocalPath == "/work/repo" {
			links++
		}
	}
	if links != 1 {
		t.Errorf("%d link entries for the path, want 1", links)
	}
}

// LinkPathToHubURL refuses a URL hub add refuses, a missing project ID and
// a missing path, and changes nothing then.
func TestCLIContextLinkPathToHubURL_Refused(t *testing.T) {
	now := time.Now()
	cases := map[string][3]string{
		"user info":  {"https://user:placeholder@hub.example.org", "id", "/work/repo"},
		"query":      {"https://hub.example.org/?x=1", "id", "/work/repo"},
		"no scheme":  {"hub.example.org", "id", "/work/repo"},
		"no ID":      {"https://hub.example.org", "", "/work/repo"},
		"no path":    {"https://hub.example.org", "id", ""},
		"empty URL":  {"", "id", "/work/repo"},
		"fragment":   {"https://hub.example.org/#x", "id", "/work/repo"},
		"ftp scheme": {"ftp://hub.example.org", "id", "/work/repo"},
	}
	for name, tc := range cases {
		c := linkTestContext()
		if _, _, err := c.LinkPathToHubURL(tc[0], "hub", tc[1], tc[2], now); err == nil {
			t.Errorf("%s: nil error", name)
		}
		if len(c.Hubs) != 2 || len(c.Projects) != 0 {
			t.Errorf("%s: context changed: %+v", name, c)
		}
	}
}

// ProjectLinkID prefers the hub project ID over the project ID.
func TestProjectLinkID(t *testing.T) {
	cliContextTestHome(t)
	t.Setenv("SCION_PROJECT_ID", "env-id")
	write := func(settings string) string {
		dir := filepath.Join(t.TempDir(), DotScion)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "settings.yaml"), []byte(settings), 0o644); err != nil {
			t.Fatal(err)
		}
		return dir
	}
	if got := ProjectLinkID(write("project_id: local-1\nhub:\n  projectId: hub-1\n")); got != "hub-1" {
		t.Errorf("hub ID: got %q, want hub-1", got)
	}
	if got := ProjectLinkID(write("project_id: local-1\n")); got != "local-1" {
		t.Errorf("project ID: got %q, want local-1", got)
	}
}
