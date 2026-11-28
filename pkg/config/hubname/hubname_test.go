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

package hubname

import "testing"

func TestNormalizeEndpoint(t *testing.T) {
	cases := map[string]string{
		"  HTTPS://Hub.Example.COM:443/// ": "https://hub.example.com",
		"http://example.com:80/":            "http://example.com",
		"http://example.com:8080/":          "http://example.com:8080",
		"https://example.com:0443/":         "https://example.com:0443",
		"https://example.com/Path/Kept/":    "https://example.com/Path/Kept",
		"http://[::1]:80/":                  "http://[::1]",
		"http://[::1]:9810/":                "http://[::1]:9810",
		"https://\u212a.example.com":        "https://%E2%84%AA.example.com",
		"https://hub.example.com./":         "https://hub.example.com.",
		"  not-a-url///  ":                  "not-a-url",
	}
	for in, want := range cases {
		if got := NormalizeEndpoint(in); got != want {
			t.Errorf("NormalizeEndpoint(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestDeriveHubName(t *testing.T) {
	cases := map[string]string{
		"https://hub.example.com":       "hub",
		"https://Prod.Example.com:443/": "prod",
		"http://localhost:8080":         "localhost",
		"http://127.0.0.1:8080":         "127-0-0-1",
		"http://[::1]:8080":             "1",
		"https://my_hub.example.com":    "my-hub",
		"not a url":                     "hub",
		"":                              "hub",
	}
	for in, want := range cases {
		if got := DeriveHubName(in); got != want {
			t.Errorf("DeriveHubName(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestValidHubName(t *testing.T) {
	for _, valid := range []string{"a", "hub", "my-hub-2", "127-0-0-1"} {
		if !ValidHubName(valid) {
			t.Errorf("ValidHubName(%q) = false, want true", valid)
		}
	}
	for _, invalid := range []string{"", "-hub", "Hub", "a/b", "a:b", "my_hub"} {
		if ValidHubName(invalid) {
			t.Errorf("ValidHubName(%q) = true, want false", invalid)
		}
	}
}
