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

package cmd

import (
	"encoding/json"
	"fmt"
	"net/url"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// secretFragmentIn returns the first n-character fragment of secret found
// in out, or "" when out holds none (so no longer fragment either).
func secretFragmentIn(out, secret string, n int) string {
	for i := 0; i+n <= len(secret); i++ {
		if strings.Contains(out, secret[i:i+n]) {
			return secret[i : i+n]
		}
	}
	return ""
}

// TestRedactEndpointIn_UnparseableEndpointNeverEchoed: for endpoints that
// url.Parse rejects, no 4-character fragment of the password survives in
// any rendering of a text that quotes the endpoint: as given, with %q, as
// url.Parse's own error (also through %v), JSON-encoded, and path- or
// query-escaped.
func TestRedactEndpointIn_UnparseableEndpointNeverEchoed(t *testing.T) {
	for _, tc := range []struct {
		name, endpoint string
		secrets        []string
	}{
		{"control character", "http://user:s3cret@hub.example.com/\x01", []string{"s3cret"}},
		{"@ in the password", "http://user:s3@cret@hub.example.com/\x01", []string{"s3@cret"}},
		{"several @ in the password", "http://user:pa@ss@w0rd@127.0.0.1:99999x", []string{"pa@ss@w0rd"}},
		{"@ in the user and the password", "http://us@er:pa@ss@127.0.0.1:99999x", []string{"pa@ss"}},
		{"percent-encoded password", "http://user:s3%40cret@127.0.0.1:99999x", []string{"s3%40cret", "s3@cret"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, perr := url.Parse(tc.endpoint)
			require.Error(t, perr, "the endpoint must be one url.Parse rejects")
			jsonText, err := json.Marshal(perr.Error())
			require.NoError(t, err)
			renderings := map[string]string{
				"verbatim":         "hub " + tc.endpoint + " failed",
				"%q":               fmt.Sprintf("hub %q failed", tc.endpoint),
				"url.Parse error":  perr.Error(),
				"%v of *url.Error": fmt.Sprintf("%v", perr),
				"JSON":             string(jsonText),
				"query-escaped":    "next=" + url.QueryEscape(tc.endpoint),
				"path-escaped":     "/p/" + url.PathEscape(tc.endpoint),
			}
			for name, text := range renderings {
				got := redactEndpointIn(text, tc.endpoint)
				for _, s := range tc.secrets {
					assert.Empty(t, secretFragmentIn(got, s, 4), "%s: %q", name, got)
				}
			}
		})
	}
}

// TestRedactEndpointIn_QueryEscapedEndpoint: the user info of an
// unparseable endpoint is also replaced in its query-escaped form, which
// neither the whole-endpoint replacement nor the URL pattern matches.
func TestRedactEndpointIn_QueryEscapedEndpoint(t *testing.T) {
	endpoint := "http://user:s3@cret@127.0.0.1:99999x"
	got := redactEndpointIn("next="+url.QueryEscape(endpoint), endpoint)
	require.Empty(t, secretFragmentIn(got, "s3@cret", 4), got)
}

// TestRedactEndpointIn_AtOutsideUserInfoNeverEchoed: an endpoint that
// parses but holds an '@' the parser does not read as user info (a
// password that reads as a port and a path, or a token that reads as a
// host, followed by '/', '?' or '#') is replaced whole, and no 4-character
// fragment of its secret survives in any rendering of a text that quotes
// it. Hub URLs must not contain '@', so such an endpoint can only come
// from a source that skipped the validator.
func TestRedactEndpointIn_AtOutsideUserInfoNeverEchoed(t *testing.T) {
	for _, tc := range []struct{ name, endpoint, secret string }{
		{"password read as a port and a path", "https://u:1234/x@hub.example.com", "1234/x"},
		{"token read as a host", "https://tok9/x@hub.example.com", "tok9/x"},
		{"password read as a port, then '?'", "https://u:12?34@hub.example.com", "12?34"},
		{"token read as a host, then '#'", "https://tok9#x@hub.example.com", "tok9#x"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			u, err := url.Parse(tc.endpoint)
			require.NoError(t, err, "the endpoint must be one url.Parse accepts")
			require.Nil(t, u.User, "the endpoint must parse without user info")
			jsonText, err := json.Marshal("hub " + tc.endpoint + " failed")
			require.NoError(t, err)
			renderings := map[string]string{
				"verbatim":      "hub " + tc.endpoint + " failed",
				"URL built":     `Get "` + tc.endpoint + `/api/v1/health": refused`,
				"%q":            fmt.Sprintf("hub %q failed", tc.endpoint),
				"JSON":          string(jsonText),
				"query-escaped": "next=" + url.QueryEscape(tc.endpoint),
				"path-escaped":  "/p/" + url.PathEscape(tc.endpoint),
			}
			for name, text := range renderings {
				got := redactEndpointIn(text, tc.endpoint)
				assert.Empty(t, secretFragmentIn(got, tc.secret, 4), "%s: %q", name, got)
			}
			assert.Equal(t, `Get "[redacted hub URL]/api/v1/health": refused`,
				redactEndpointIn(renderings["URL built"], tc.endpoint))
		})
	}
}
