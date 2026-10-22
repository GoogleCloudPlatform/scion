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
	"net/url"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/util/urlredact/urlredacttest"
)

// endpointErrors returns the error texts a failed conduit dial to endpoint
// may produce: quoting the endpoint, or the conduit URL built from it, as
// url.URL writes it and as net/http's errors write it (a password as
// "***").
func endpointErrors(t *testing.T, endpoint string) []string {
	t.Helper()
	texts := []string{`Get "` + endpoint + `": dial tcp: refused`}
	cu, err := conduitURL(endpoint)
	if err != nil {
		return texts
	}
	texts = append(texts, "websocket dial "+cu+": refused")
	if u, err := url.Parse(cu); err == nil && u.User != nil {
		if _, ok := u.User.Password(); ok {
			texts = append(texts, `Get "`+strings.Replace(u.String(), u.User.String()+"@", u.User.Username()+":***@", 1)+`": refused`)
		}
	}
	return texts
}

// No part of the credential of any shape shows in the endpoint display or
// in a scrubbed dial error.
func TestEndpointRedactors_Shapes(t *testing.T) {
	for _, s := range urlredacttest.Shapes {
		if got := RedactEndpoint(s.URL); urlredacttest.SecretFragment(got, s.Secret) != "" {
			t.Errorf("%s: RedactEndpoint(%q) = %q shows part of the secret", s.Name, s.URL, got)
		}
		for _, text := range endpointErrors(t, s.URL) {
			if got := scrubEndpoint(text, s.URL); urlredacttest.SecretFragment(got, s.Secret) != "" {
				t.Errorf("%s: scrubEndpoint(%q) = %q shows part of the secret", s.Name, text, got)
			}
		}
	}
}

// An ordinary or token-bearing endpoint keeps its host and path.
func TestEndpointRedactors_KeepHostAndPath(t *testing.T) {
	for _, k := range urlredacttest.KeptURLs {
		if got := RedactEndpoint(k.URL); got != k.Want {
			t.Errorf("%s: RedactEndpoint(%q) = %q, want %q", k.Name, k.URL, got, k.Want)
		}
		text := `Get "` + k.URL + `": refused`
		if got, want := scrubEndpoint(text, k.URL), `Get "`+k.Want+`": refused`; got != want {
			t.Errorf("%s: scrubEndpoint(%q) = %q, want %q", k.Name, text, got, want)
		}
		for _, text := range endpointErrors(t, k.URL) {
			got := scrubEndpoint(text, k.URL)
			if k.Secret != "" && urlredacttest.SecretFragment(got, k.Secret) != "" {
				t.Errorf("%s: scrubEndpoint(%q) = %q shows part of the secret", k.Name, text, got)
			}
			if !strings.Contains(got, strings.TrimPrefix(k.Want, "https://")) {
				t.Errorf("%s: scrubEndpoint(%q) = %q lost the host and path", k.Name, text, got)
			}
		}
	}
}

// An endpoint that does not parse no longer leaves the error text whole.
func TestScrubEndpoint_Unparsed(t *testing.T) {
	const ep = "https://alice:Zq7m Wx2@hub.example.com/api"
	got := scrubEndpoint(`Get "`+ep+`": refused`, ep)
	if want := `Get "https://***": refused`; got != want {
		t.Errorf("scrubEndpoint = %q, want %q", got, want)
	}
}
