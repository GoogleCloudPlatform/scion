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

package urlredact

import (
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/util/urlredact/urlredacttest"
)

// HostAndPath and RedactURLIn on every endpoint shape, as a caller that
// knows the endpoint uses them: HostAndPath for the display (a fixed label
// when it rejects the endpoint), RedactURLIn for an error text about it.
// Each output is pinned whole, and none shows a 4-byte part of the
// credential, Residual shapes included.
func TestEndpointShapes_KnownEndpoint(t *testing.T) {
	const shown, label = "[hub]", "(hub URL not shown)"
	display := map[string]string{
		"password, token query and fragment": "https://hub.example.com/p",
	}
	for _, s := range urlredacttest.EndpointShapes {
		gotDisplay := label
		if u, ok := HostAndPath(s.URL); ok {
			gotDisplay = u.String()
		}
		wantDisplay := label
		if d, ok := display[s.Name]; ok {
			wantDisplay = d
		}
		asGiven, quoted := urlredacttest.EndpointTexts(s.URL)
		for _, c := range []struct{ form, in, got, want string }{
			{"HostAndPath", s.URL, gotDisplay, wantDisplay},
			{"RedactURLIn, as given", asGiven, RedactURLIn(asGiven, s.URL, shown, Mask), `Get "[hub]": refused`},
			{"RedactURLIn, %q", quoted, RedactURLIn(quoted, s.URL, shown, Mask), `parse "[hub]": refused`},
		} {
			if c.got != c.want {
				t.Errorf("%s: %s(%q) = %q, want %q", s.Name, c.form, c.in, c.got, c.want)
			}
			if frag := urlredacttest.SecretFragment(c.got, s.Secret); frag != "" {
				t.Errorf("%s: %s(%q) = %q shows %q of the credential", s.Name, c.form, c.in, c.got, frag)
			}
		}
	}
}

func TestAuthorityIsHost(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want bool
	}{
		{"", true},
		{"hub.example.com", true},
		{"hub.example.com:8443", true},
		{"[::1]:8443", true},
		{"alice:pw@hub.example.com", true},
		{"hub.example.com:80a", false},
		{"alice:Vb8k", false},
		{"alice:", true},
		{":8443", false},
		{"hub_example", false},
	} {
		if got := AuthorityIsHost(tc.in); got != tc.want {
			t.Errorf("AuthorityIsHost(%q) = %v, want %v", tc.in, got, tc.want)
		}
	}
}
