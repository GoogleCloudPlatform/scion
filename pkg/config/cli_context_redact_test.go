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
	"fmt"
	"strings"
	"testing"
)

// passwordFragmentIn returns the first n-character fragment of secret found
// in out, or "" when out holds none (so no longer fragment either).
func passwordFragmentIn(out, secret string, n int) string {
	for i := 0; i+n <= len(secret); i++ {
		if strings.Contains(out, secret[i:i+n]) {
			return secret[i : i+n]
		}
	}
	return ""
}

// TestRedactURL_NoPasswordFragment: no 4-character fragment of the password
// survives RedactURL, its %q form or ValidateHubURL's error, for passwords
// with one or several '@' and percent-encoded ones, in URLs url.Parse
// accepts and in URLs it rejects.
func TestRedactURL_NoPasswordFragment(t *testing.T) {
	for _, tc := range []struct {
		in      string
		secrets []string
	}{
		{"http://user:s3@cret@127.0.0.1:99999x", []string{"s3@cret"}},
		{"http://user:pa@ss@w0rd@127.0.0.1:99999x", []string{"pa@ss@w0rd"}},
		{"http://us@er:pa@ss@127.0.0.1:99999x", []string{"pa@ss"}},
		{"http://user:s3%40cret@127.0.0.1:99999x", []string{"s3%40cret", "s3@cret"}},
		{"http://user:s3@cret@hub.example.com/\x01", []string{"s3@cret"}},
		{"https://user:s3@cret@hub.example.com", []string{"s3@cret"}},
		{"https://user:s3%40cret@hub.example.com", []string{"s3%40cret", "s3@cret"}},
	} {
		got := RedactURL(tc.in)
		forms := []string{got, fmt.Sprintf("%q", got)}
		if err := ValidateHubURL(tc.in); err != nil {
			forms = append(forms, err.Error())
		} else {
			t.Errorf("ValidateHubURL(%q) accepted a URL with user info", tc.in)
		}
		for _, f := range forms {
			for _, s := range tc.secrets {
				if frag := passwordFragmentIn(f, s, 4); frag != "" {
					t.Errorf("RedactURL(%q): %q shows %q", tc.in, f, frag)
				}
			}
		}
	}
}

// TestRedactURL_LastAtOfTheAuthority: for a URL url.Parse rejects, the user
// info is masked up to the last '@' before the host, so a password that
// contains '@' leaves nothing behind.
func TestRedactURL_LastAtOfTheAuthority(t *testing.T) {
	if got := RedactURL("http://user:s3@cret@127.0.0.1:99999x"); got != "http://xxxxx@127.0.0.1:99999x" {
		t.Fatalf("RedactURL = %q, want %q", got, "http://xxxxx@127.0.0.1:99999x")
	}
}
