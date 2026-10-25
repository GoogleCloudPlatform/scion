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
	"net/url"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/util/urlredact/urlredacttest"
)

// endpointShapeWant is the whole output of RedactURL on a shape (url), of
// RedactURLsIn on the shape's two error texts (asGiven, quoted; see
// urlredacttest.EndpointTexts), and of RedactURLsIn on the error url.Parse
// returns for the shape (parseErr, "" when it parses).
type endpointShapeWant struct{ url, asGiven, quoted, parseErr string }

// endpointShapeWants maps each urlredacttest.EndpointShapes name to its
// whole output.
var endpointShapeWants = map[string]endpointShapeWant{
	"slash in password, no path": {
		"https://xxxxx",
		`Get "https://xxxxx": refused`,
		`parse "https://xxxxx": refused`,
		`parse "https://xxxxx": invalid port ":xxxxx" after host`,
	},
	"password, token query and fragment": {
		"https://alice:xxxxx@hub.example.com/p?token=xxxxx#frag",
		`Get "https://alice:xxxxx@hub.example.com/p?token=xxxxx#frag": refused`,
		`parse "https://alice:xxxxx@hub.example.com/p?token=xxxxx#frag": refused`,
		"",
	},
	"password, bad port": {
		"https://xxxxx@hub.example.com:80a/p?k=Rw5nYp2c#frag",
		`Get "https://xxxxx@hub.example.com:80a/p?k=Rw5nYp2c#frag": refused`,
		`parse "https://xxxxx@hub.example.com:80a/p?k=Rw5nYp2c#frag": refused`,
		`parse "https://xxxxx@hub.example.com:80a/p?k=Rw5nYp2c": invalid port ":xxxxx" after host`,
	},
	"no scheme": {
		"alice:Vb8kTq3m@hub.example.com/p?k=Rw5nYp2c#frag",
		`Get "alice:Vb8kTq3m@hub.example.com/p?k=Rw5nYp2c#frag": refused`,
		`parse "alice:Vb8kTq3m@hub.example.com/p?k=Rw5nYp2c#frag": refused`,
		"",
	},
	"no scheme, bad escape in host": {
		"alice:Vb8kTq3m@%zz?k=Rw5nYp2c#frag",
		`Get "alice:Vb8kTq3m@%zz?k=Rw5nYp2c#frag": refused`,
		`parse "alice:Vb8kTq3m@%zz?k=Rw5nYp2c#frag": refused`,
		"",
	},
	"slash in password": {
		"https://xxxxx",
		`Get "https://xxxxx": refused`,
		`parse "https://xxxxx": refused`,
		`parse "https://xxxxx": invalid port ":xxxxx" after host`,
	},
	"question mark in password": {
		"https://xxxxx",
		`Get "https://xxxxx": refused`,
		`parse "https://xxxxx": refused`,
		`parse "https://xxxxx": invalid port ":xxxxx" after host`,
	},
	"hash in password": {
		"https://xxxxx",
		`Get "https://xxxxx": refused`,
		`parse "https://xxxxx": refused`,
		`parse "https://xxxxx": invalid port ":xxxxx" after host`,
	},
	"space in password": {
		"https://xxxxx@hub.example.com/p?k=Rw5nYp2c#frag",
		`Get "https://xxxxx": refused`,
		`parse "https://xxxxx": refused`,
		`parse "https://xxxxx": net/url: invalid userinfo`,
	},
	"password read as a port and a path": {
		"https://alice:4826/Tq3m@hub.example.com/p?k=Rw5nYp2c#frag",
		`Get "https://alice:4826/Tq3m@hub.example.com/p?k=Rw5nYp2c#frag": refused`,
		`parse "https://alice:4826/Tq3m@hub.example.com/p?k=Rw5nYp2c#frag": refused`,
		"",
	},
	"double quote in password": {
		"https://xxxxx",
		`Get "https://xxxxx": refused`,
		`parse "https://xxxxx": refused`,
		`parse "https://xxxxx": invalid port ":xxxxx" after host`,
	},
	"backslash in password": {
		"https://xxxxx",
		`Get "https://xxxxx": refused`,
		`parse "https://xxxxx": refused`,
		`parse "https://xxxxx": invalid port ":xxxxx" after host`,
	},
	"short piece before a slash": {
		"https://xxxxx",
		`Get "https://xxxxx": refused`,
		`parse "https://xxxxx": refused`,
		`parse "https://xxxxx": invalid port ":xxxxx" after host`,
	},
	"'@' in the path, no user info": {
		"https://hub.example.com/team@acme",
		`Get "https://hub.example.com/team@acme": refused`,
		`parse "https://hub.example.com/team@acme": refused`,
		"",
	},
}

// RedactURL and RedactURLsIn on every endpoint shape: the whole output,
// and no 4-byte part of the credential unless the shape is Residual.
func TestRedactURL_EndpointShapes(t *testing.T) {
	if len(endpointShapeWants) != len(urlredacttest.EndpointShapes) {
		t.Fatalf("endpointShapeWants has %d entries, urlredacttest.EndpointShapes %d", len(endpointShapeWants), len(urlredacttest.EndpointShapes))
	}
	for _, s := range urlredacttest.EndpointShapes {
		want, ok := endpointShapeWants[s.Name]
		if !ok {
			t.Errorf("%s: no entry in endpointShapeWants", s.Name)
			continue
		}
		asGiven, quoted := urlredacttest.EndpointTexts(s.URL)
		cases := []struct{ form, in, got, want string }{
			{"RedactURL", s.URL, RedactURL(s.URL), want.url},
			{"RedactURLsIn, as given", asGiven, RedactURLsIn(asGiven), want.asGiven},
			{"RedactURLsIn, %q", quoted, RedactURLsIn(quoted), want.quoted},
		}
		if _, err := url.Parse(s.URL); err != nil {
			cases = append(cases, struct{ form, in, got, want string }{"RedactURLsIn, url.Parse error", err.Error(), RedactURLsIn(err.Error()), want.parseErr})
		} else if want.parseErr != "" {
			t.Errorf("%s: url.Parse accepts the shape, but a parse error output is pinned", s.Name)
		}
		for _, c := range cases {
			if c.got != c.want {
				t.Errorf("%s: %s(%q) = %q, want %q", s.Name, c.form, c.in, c.got, c.want)
			}
			if frag := urlredacttest.SecretFragment(c.got, s.Secret); frag != "" && !s.Residual {
				t.Errorf("%s: %s(%q) = %q shows %q of the credential", s.Name, c.form, c.in, c.got, frag)
			}
		}
	}
}

// RedactURLsIn keeps ordinary text: a URL whose authority ends at white
// space or a quote is left alone when no '@' follows on its line, or when
// its authority ends in a host; and the text after a dropped span is kept.
func TestRedactURLsIn_AuthorityCutInText(t *testing.T) {
	for _, c := range []struct{ name, in, want string }{
		{
			"host, then an e-mail address",
			"see https://hub.example.com and write to ops@example.com",
			"see https://hub.example.com and write to ops@example.com",
		},
		{
			"host and port, then an e-mail address",
			`Get "https://hub.example.com:8443": write to ops@example.com`,
			`Get "https://hub.example.com:8443": write to ops@example.com`,
		},
		{
			"bad port, no '@' on the line: the URL is masked, the next line kept",
			"Get \"https://hub.example.com:abc\": refused\nwrite to ops@example.com",
			"Get \"https://xxxxx\": refused\nwrite to ops@example.com",
		},
		{
			"host that is not a DNS name, numeric port",
			`Get "https://scion_hub:8080/api": refused`,
			`Get "https://scion_hub:8080/api": refused`,
		},
		{
			"net/url port error",
			`parse "https://alice:Hx4q/Jp7w@hub.example.com/x": invalid port ":Hx4q" after host`,
			`parse "https://xxxxx": invalid port ":xxxxx" after host`,
		},
		{
			"net/url port error, escaped in JSON",
			`{"error":"parse \"https://alice:Hx4q/Jp7w@hub.example.com/x\": invalid port \":Hx4q\" after host"}`,
			`{"error":"parse \"https://xxxxx\": invalid port \":xxxxx\" after host"}`,
		},
		{
			"bad port, '@' on the line, text after the word kept",
			`dial https://alice:Hx4q Jp7w@hub.example.com/x, then retry`,
			`dial https://xxxxx, then retry`,
		},
		{
			"two URLs on a line, the second kept",
			`a https://alice:Hx4q Jp7w@hub.example.com/x b https://other.example.com/y`,
			`a https://xxxxx b https://other.example.com/y`,
		},
		{
			"empty port, '@' on the line",
			`Get "https://alice: Hx4qJp7w@hub.example.com/x": refused`,
			`Get "https://xxxxx": refused`,
		},
		{
			"host that is not a DNS name, '@' later on the line: masked to the end of that word",
			`Get "http://scion_hub:8080/api": refused, mail ops@example.com today`,
			`Get "http://xxxxx today`,
		},
		{
			"angle brackets",
			`<https://alice:Hx4q<Jp7w@hub.example.com/x>`,
			`<https://xxxxx>`,
		},
	} {
		got := RedactURLsIn(c.in)
		if got != c.want {
			t.Errorf("%s: RedactURLsIn(%q) = %q, want %q", c.name, c.in, got, c.want)
		}
		for _, secret := range []string{"Hx4q", "Jp7w"} {
			if strings.Contains(c.in, secret) && strings.Contains(got, secret) {
				t.Errorf("%s: RedactURLsIn(%q) = %q shows %q", c.name, c.in, got, secret)
			}
		}
	}
}

// The port net/url quotes in its "invalid port" error is masked whatever
// its length. Every value is made up.
func TestRedactURLsIn_LongPortError(t *testing.T) {
	segment := strings.Repeat("Kp3vZx8q", 40) // 320 bytes before the '/'
	raw := "https://alice:" + segment + "/Wd5n@hub.example.com/x"
	_, err := url.Parse(raw)
	if err == nil {
		t.Fatalf("url.Parse(%q) succeeded, want an invalid port error", raw)
	}
	got := RedactURLsIn(err.Error())
	if want := `parse "https://xxxxx": invalid port ":xxxxx" after host`; got != want {
		t.Errorf("RedactURLsIn(%q) = %q, want %q", err.Error(), got, want)
	}
	if frag := urlredacttest.SecretFragment(got, segment+"/Wd5n"); frag != "" {
		t.Errorf("RedactURLsIn(%q) = %q shows %q of the password", err.Error(), got, frag)
	}
}
