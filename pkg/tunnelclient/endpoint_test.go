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

package tunnelclient

import (
	"io"
	"strings"
	"testing"
)

func TestNormalizeEndpoint(t *testing.T) {
	cases := map[string]string{
		"https://hub.example.com":                  "https://hub.example.com",
		"https://hub.example.com/":                 "https://hub.example.com",
		"  https://Hub.Example.COM//  ":            "https://hub.example.com",
		"HTTPS://hub.example.com:443":              "https://hub.example.com",
		"http://hub.example.com:80/":               "http://hub.example.com",
		"http://localhost:8080":                    "http://localhost:8080",
		"https://hub.example.com:8443/x/":          "https://hub.example.com:8443/x",
		"http://[::1]:80":                          "http://[::1]",
		"http://[::1]:9000":                        "http://[::1]:9000",
		"https://hub.example.com:80":               "https://hub.example.com:80",
		"not a url":                                "the configured hub endpoint",
		"https://u:p@Hub.Example.com/x/?token=1#f": "https://hub.example.com/x",
	}
	for in, want := range cases {
		if got := NormalizeEndpoint(in); got != want {
			t.Errorf("NormalizeEndpoint(%q) = %q, want %q", in, got, want)
		}
	}
}

// Spellings that normalise the same share one session key.
func TestSessionKeySharedAcrossSpellings(t *testing.T) {
	id := Identity{Kind: CredentialOAuth, Subject: "u1", Scope: ScopeHub}
	base := KeyFor("https://hub.example.com", id)
	for _, ep := range []string{"https://hub.example.com/", "https://HUB.example.com", "https://hub.example.com:443", " https://hub.example.com// "} {
		if k := KeyFor(ep, id); k != base {
			t.Errorf("KeyFor(%q) = %+v, want %+v", ep, k, base)
		}
	}
	if KeyFor("https://hub.example.com:8443", id) == base {
		t.Error("a different port shares the session key")
	}
}

// Different identities on the same endpoint never share a session key.
func TestSessionKeyDiffersByIdentity(t *testing.T) {
	ep := "https://hub.example.com"
	a := KeyFor(ep, Identity{Kind: CredentialOAuth, Subject: "u1"})
	for _, id := range []Identity{
		{Kind: CredentialOAuth, Subject: "u2"},
		{Kind: CredentialToken, Subject: "u1"},
		{Kind: CredentialDev, Subject: "u1"},
	} {
		if KeyFor(ep, id) == a {
			t.Errorf("identity %+v shares the session key of %+v", id, a)
		}
	}
	// The scope is reported, not part of the identity.
	if KeyFor(ep, Identity{Kind: CredentialOAuth, Subject: "u1", Scope: ScopeUnknown}) != a {
		t.Error("scope changed the session key")
	}
}

// The credential lookup is exact: the resolver sees the endpoint as given,
// never its normalised form.
func TestCredentialLookupUsesRawEndpoint(t *testing.T) {
	h := newFakeHub(t)
	raw := strings.Replace(h.srv.URL, "http://", "HTTP://", 1) + "/"
	cfg := testConfig(h, newFakeREST(), nil)
	cfg.Endpoint = raw
	var seen []string
	cfg.ResolveCredential = func() Credential {
		seen = append(seen, cfg.Endpoint)
		return oauth("tok1")
	}
	if err := Run(runCtx(t), cfg, stdioReq(strings.NewReader("x"), io.Discard)); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(seen) != 1 || seen[0] != raw {
		t.Fatalf("credential resolved for %v, want exactly %q", seen, raw)
	}
}

func TestConduitURL(t *testing.T) {
	cases := map[string]string{
		"https://hub.example.com":      "wss://hub.example.com/api/v1/conduit",
		"https://hub.example.com/":     "wss://hub.example.com/api/v1/conduit",
		"http://localhost:8080/base/":  "ws://localhost:8080/base/api/v1/conduit",
		"https://hub.example.com/?q=1": "wss://hub.example.com/api/v1/conduit",
	}
	for in, want := range cases {
		got, err := conduitURL(in)
		if err != nil || got != want {
			t.Errorf("conduitURL(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	for _, bad := range []string{"", "hub.example.com", "ftp://hub.example.com"} {
		if _, err := conduitURL(bad); err == nil {
			t.Errorf("conduitURL(%q) accepted", bad)
		}
	}
}

func TestDefaultScope(t *testing.T) {
	cases := map[string]string{CredentialOAuth: ScopeHub, CredentialDev: ScopeUnknown, CredentialToken: ScopeUnknown, "": ScopeUnknown}
	for kind, want := range cases {
		if got := (DefaultScope{}).Scope(Credential{Kind: kind, Token: "secret"}); got != want {
			t.Errorf("Scope(%q) = %q, want %q", kind, got, want)
		}
	}
}

func TestRedactEndpoint(t *testing.T) {
	cases := map[string]string{
		"https://hub.example.com":                          "https://hub.example.com",
		"https://user:pw@Hub.example.com:8443/base/?t=1#x": "https://Hub.example.com:8443/base",
		"http://user@127.0.0.1:9810/":                      "http://127.0.0.1:9810",
		"hub.example.com":                                  "the configured hub endpoint",
		"":                                                 "the configured hub endpoint",
	}
	for in, want := range cases {
		if got := RedactEndpoint(in); got != want {
			t.Errorf("RedactEndpoint(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestScrubEndpoint(t *testing.T) {
	ep := "https://alice:s3cret@hub.example.com/?token=TOK123#frag"
	in := `Get "https://alice:***@hub.example.com/api/v1/auth/me?token=TOK123": dial tcp: refused (alice:s3cret@ s3cret TOK123)`
	got := scrubEndpoint(in, ep)
	for _, secret := range []string{"alice", "s3cret", "TOK123"} {
		if strings.Contains(got, secret) {
			t.Errorf("scrubbed text %q still contains %q", got, secret)
		}
	}
}
