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

package hubprofile

import (
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/util/urlredact/urlredacttest"
)

// NormalizeEndpoint refuses every endpoint shape with an error that shows
// the endpoint by its scheme, host and path only, or not at all. The whole
// error text is pinned, and none shows a 4-byte part of the credential.
func TestNormalizeEndpoint_EndpointShapes(t *testing.T) {
	const notShown = "invalid endpoint (endpoint not shown): no user info or '@'"
	want := map[string]string{
		"password, token query and fragment": `invalid endpoint "https://hub.example.com/p": no user info or '@'`,
	}
	for _, s := range urlredacttest.EndpointShapes {
		got, err := NormalizeEndpoint(s.URL)
		if err == nil {
			t.Errorf("%s: NormalizeEndpoint(%q) = %q, want an error", s.Name, s.URL, got)
			continue
		}
		w := notShown
		if v, ok := want[s.Name]; ok {
			w = v
		}
		if err.Error() != w {
			t.Errorf("%s: NormalizeEndpoint(%q) error = %q, want %q", s.Name, s.URL, err.Error(), w)
		}
		if frag := urlredacttest.SecretFragment(err.Error(), s.Secret); frag != "" {
			t.Errorf("%s: NormalizeEndpoint(%q) error = %q shows %q of the credential", s.Name, s.URL, err.Error(), frag)
		}
	}
}

// The other NormalizeEndpoint and CheckNotOwnHub errors show no query,
// fragment or text that does not parse. Every value is made up.
func TestNormalizeEndpoint_ErrorText(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"https://hub.example.com/p?token=Rw5nYp2c", `invalid endpoint "https://hub.example.com/p": no user info, query or fragment`},
		{"https://hub.example.com/p#Rw5nYp2c", `invalid endpoint "https://hub.example.com/p": no user info, query or fragment`},
		{"https://hub.example.com/%zz?token=Rw5nYp2c", "invalid endpoint (endpoint not shown): must be an absolute URL"},
		{"hub.example.com/p", `invalid endpoint "hub.example.com/p": must be an absolute URL`},
		{"ftp://hub.example.com/p", `invalid endpoint "ftp://hub.example.com/p": https is required`},
		{"http://hub.example.com/p", `invalid endpoint "http://hub.example.com/p": https is required`},
	} {
		_, err := NormalizeEndpoint(tc.in)
		if err == nil || err.Error() != tc.want {
			t.Errorf("NormalizeEndpoint(%q) error = %v, want %q", tc.in, err, tc.want)
		}
	}
	none := func(string) string { return "" }
	for _, tc := range []struct{ in, want string }{
		{"https://alice:Vb8k Tq3m@hub.example.com/p", "invalid endpoint (endpoint not shown): not an absolute URL with a host"},
		{"https://alice:Vb8kTq3m@hub.example.com/p?k=Rw5nYp2c", `invalid endpoint "https://hub.example.com/p": user info is not allowed`},
	} {
		err := CheckNotOwnHub(tc.in, none)
		if err == nil || err.Error() != tc.want {
			t.Errorf("CheckNotOwnHub(%q) error = %v, want %q", tc.in, err, tc.want)
		}
	}
}
