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
	"strings"
	"testing"
)

func TestRedactURLsIn(t *testing.T) {
	tests := []struct {
		name, in, want string
	}{
		{"text 1", "nothing to see", "nothing to see"},
		{"text 2", "Get https://hub.example.com/api: refused", "Get https://hub.example.com/api: refused"},
		{"text 3", "see https://hub.example.com/u/@me and a@b", "see https://hub.example.com/u/@me and a@b"},
		{"text 4", "dial https://alice:s3cret@hub.example.com/x", "dial https://alice:xxxxx@hub.example.com/x"},
		{"text 5", `Get "https://tok3n@hub.example.com/api/v1/agents": refused`, `Get "https://xxxxx@hub.example.com/api/v1/agents": refused`},
		{"text 6", `Get "https://alice:xxxxx@hub.example.com/healthz"`, `Get "https://alice:xxxxx@hub.example.com/healthz"`},
		{"text 7", "at HTTP://tok3n@hub.example.com", "at http://xxxxx@hub.example.com"},
		{"text 8", "hub at https://tok3n@hub.example.com:port is down", "hub at https://xxxxx@hub.example.com:port is down"},
		{"text 9", "https://a@s3cret@hub.example.com:port/x", "https://xxxxx@hub.example.com:port/x"},
		{
			"text 10",
			"https://alice:s3cret@one.example.com and http://tok3n@two.example.com?q=1, then https://three.example.com",
			"https://alice:xxxxx@one.example.com and http://xxxxx@two.example.com?q=1, then https://three.example.com",
		},
		{"text 11", `invalid hub endpoint "https://tok3n@hub.example.com": no`, `invalid hub endpoint "https://xxxxx@hub.example.com": no`},
		{"text 12", "https://tok3n@hub.example.com#frag", "https://xxxxx@hub.example.com#frag"},
		{"text 13", "dial https://alice:s3@cret@hub.example.com/x", "dial https://alice:xxxxx@hub.example.com/x"},
		{"text 14", "dial https://alice:s3@cret@hub.example.com:port/x", "dial https://xxxxx@hub.example.com:port/x"},
		{
			"text 15",
			`parse "https://alice:s3\"cret@hub.example.com": net/url: invalid userinfo`,
			`parse "https://xxxxx@hub.example.com": net/url: invalid userinfo`,
		},
		{"text 16", `"https://tok3n\u00e9@hub.example.com"`, `"https://xxxxx@hub.example.com"`},
		{"text 17", "hub at https://t'ok3n@hub.example.com is down", "hub at https://xxxxx@hub.example.com is down"},
		{"text 18", `Get \"https://tok3n@hub.example.com\": refused`, `Get \"https://xxxxx@hub.example.com\": refused`},
	}
	for _, tc := range tests {
		got := RedactURLsIn(tc.in)
		if got != tc.want {
			t.Errorf("%s: RedactURLsIn(%q) = %q, want %q", tc.name, tc.in, got, tc.want)
		}
		for _, secret := range []string{"s3cret", "s3@cret", "cret@", "tok3n"} {
			if strings.Contains(got, secret) {
				t.Errorf("%s: RedactURLsIn(%q) = %q, still shows %q", tc.name, tc.in, got, secret)
			}
		}
	}
}

// The whole user info is replaced.
func TestRedactURL_WholeUserInfo(t *testing.T) {
	got := RedactURL("https://a@s3cret@hub.example.com:port/x")
	if want := "https://xxxxx@hub.example.com:port/x"; got != want {
		t.Errorf("RedactURL = %q, want %q", got, want)
	}
}

// A URL with a user name and no password.
func TestRedactURLsIn_MasksUserName(t *testing.T) {
	got := RedactURLsIn(`Get "https://tok3n@hub.example.com/api": refused`)
	if want := `Get "https://xxxxx@hub.example.com/api": refused`; got != want {
		t.Errorf("RedactURLsIn = %q, want %q", got, want)
	}
}

// A hub URL in an error text is shown without its user info.
func TestRedactURLsIn_ErrorText(t *testing.T) {
	got := RedactURLsIn(`invalid hub URL "https://alice:s3\"cret@hub.example.com"`)
	if want := `invalid hub URL "https://xxxxx@hub.example.com"`; got != want {
		t.Errorf("RedactURLsIn = %q, want %q", got, want)
	}
}

// User info of any valid form is shown masked.
func TestRedactURLsIn_UserInfoCharacters(t *testing.T) {
	const password = "s3!$&'()*+,;=%41cret"
	const token = "t0k!$&'()*+,;=%41en"
	for _, tc := range []struct{ in, secret string }{
		{"hub at https://u:" + password + "@hub.example.com is not responding", password},
		{"hub at https://al'ice!$&()*+,;=:s3cret@hub.example.com is not responding", "s3cret"},
		{"hub at https://" + token + "@hub.example.com is not responding", token},
		{`Get "https://u:` + password + `@hub.example.com/api": refused`, password},
	} {
		got := RedactURLsIn(tc.in)
		if !strings.Contains(got, "xxxxx@hub.example.com") {
			t.Errorf("RedactURLsIn(%q) = %q, want the user info masked", tc.in, got)
		}
		for i := 0; i+4 <= len(tc.secret); i++ {
			if strings.Contains(got, tc.secret[i:i+4]) {
				t.Errorf("RedactURLsIn(%q) = %q, shows %q", tc.in, got, tc.secret[i:i+4])
			}
		}
	}
}
