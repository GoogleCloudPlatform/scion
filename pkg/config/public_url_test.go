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

func TestValidatePublicURL_Accepted(t *testing.T) {
	tests := []struct {
		raw  string
		want string
	}{
		{"", ""},
		{"https://hub.example.com", "https://hub.example.com"},
		{"https://hub.example.com/", "https://hub.example.com"},
		{"HTTPS://hub.example.com:8443/", "https://hub.example.com:8443"},
		{"http://localhost:8080", "http://localhost:8080"},
		{"http://hub.localhost/", "http://hub.localhost"},
		{"http://127.0.0.1:9810", "http://127.0.0.1:9810"},
		{"http://[::1]:9810", "http://[::1]:9810"},
		{"https://[2001:db8::1]", "https://[2001:db8::1]"},
	}
	for _, tt := range tests {
		got, err := ValidatePublicURL(tt.raw)
		if err != nil {
			t.Errorf("ValidatePublicURL(%q) error = %v, want nil", tt.raw, err)
			continue
		}
		if got != tt.want {
			t.Errorf("ValidatePublicURL(%q) = %q, want %q", tt.raw, got, tt.want)
		}
	}
}

func TestValidatePublicURL_Rejected(t *testing.T) {
	tests := []struct {
		name string
		raw  string
	}{
		{"relative", "hub.example.com"},
		{"no host", "https://"},
		{"opaque", "https:hub.example.com"},
		{"other scheme", "ftp://hub.example.com"},
		{"http non-loopback", "http://hub.example.com"},
		{"http private address", "http://10.0.0.5:8080"},
		{"userinfo", "https://user:pw@hub.example.com"},
		{"user only", "https://user@hub.example.com"},
		{"query", "https://hub.example.com/?a=b"},
		{"empty query", "https://hub.example.com/?"},
		{"fragment", "https://hub.example.com/#x"},
		{"empty fragment", "https://hub.example.com/#"},
		{"path", "https://hub.example.com/hub"},
		{"bad port", "https://hub.example.com:0"},
		{"port too large", "https://hub.example.com:70000"},
		{"ipv6 zone", "http://[fe80::1%25eth0]:80"},
		{"bad host chars", "https://hub!.example.com"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := ValidatePublicURL(tt.raw)
			if err == nil {
				t.Fatalf("ValidatePublicURL(%q) = nil error, want rejection", tt.raw)
			}
			if !strings.HasPrefix(err.Error(), PublicURLKey+":") {
				t.Errorf("error %q does not name %s", err, PublicURLKey)
			}
		})
	}
}
