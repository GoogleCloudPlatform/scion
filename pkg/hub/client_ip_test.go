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

package hub

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestTrustedClientIP(t *testing.T) {
	tests := []struct {
		name       string
		remoteAddr string
		trusted    []string
		xffLines   []string
		realIP     string
		want       string
	}{
		{
			name:       "untrusted peer with XFF and X-Real-IP returns peer",
			remoteAddr: "198.51.100.5:4321",
			trusted:    []string{"10.0.0.0/8"},
			xffLines:   []string{"203.0.113.9"},
			realIP:     "203.0.113.10",
			want:       "198.51.100.5",
		},
		{
			name:       "empty trusted list returns peer",
			remoteAddr: "10.0.0.1:4321",
			trusted:    nil,
			xffLines:   []string{"203.0.113.9"},
			realIP:     "203.0.113.10",
			want:       "10.0.0.1",
		},
		{
			name:       "trusted peer with single XFF returns client",
			remoteAddr: "10.0.0.1:4321",
			trusted:    []string{"10.0.0.0/8"},
			xffLines:   []string{"203.0.113.9"},
			want:       "203.0.113.9",
		},
		{
			name:       "trusted peer reads XFF right to left",
			remoteAddr: "10.0.0.1:4321",
			trusted:    []string{"10.0.0.0/8"},
			xffLines:   []string{"198.51.100.1, 203.0.113.9"},
			want:       "203.0.113.9",
		},
		{
			name:       "trusted peer joins multiple XFF header lines in order",
			remoteAddr: "10.0.0.1:4321",
			trusted:    []string{"10.0.0.0/8"},
			xffLines:   []string{"198.51.100.1", "203.0.113.9"},
			want:       "203.0.113.9",
		},
		{
			name:       "trusted peer walks past trusted hop on the right",
			remoteAddr: "10.0.0.1:4321",
			trusted:    []string{"10.0.0.0/8"},
			xffLines:   []string{"203.0.113.9, 10.0.0.2"},
			want:       "203.0.113.9",
		},
		{
			name:       "trusted peer walks across multiple lines past trusted hop",
			remoteAddr: "10.0.0.1:4321",
			trusted:    []string{"10.0.0.0/8"},
			xffLines:   []string{"198.51.100.1, 203.0.113.9", "10.0.0.2"},
			want:       "203.0.113.9",
		},
		{
			name:       "all XFF hops trusted falls back to peer",
			remoteAddr: "10.0.0.1:4321",
			trusted:    []string{"10.0.0.0/8"},
			xffLines:   []string{"10.0.0.3, 10.0.0.2"},
			want:       "10.0.0.1",
		},
		{
			name:       "malformed unknown hop right of client falls back to peer",
			remoteAddr: "10.0.0.1:4321",
			trusted:    []string{"10.0.0.0/8"},
			xffLines:   []string{"203.0.113.9, unknown"},
			want:       "10.0.0.1",
		},
		{
			name:       "malformed ip:port hop right of client falls back to peer",
			remoteAddr: "10.0.0.1:4321",
			trusted:    []string{"10.0.0.0/8"},
			xffLines:   []string{"203.0.113.9, 1.2.3.4:80"},
			want:       "10.0.0.1",
		},
		{
			name:       "empty hop right of client falls back to peer",
			remoteAddr: "10.0.0.1:4321",
			trusted:    []string{"10.0.0.0/8"},
			xffLines:   []string{"203.0.113.9, "},
			want:       "10.0.0.1",
		},
		{
			name:       "empty second XFF line falls back to peer",
			remoteAddr: "10.0.0.1:4321",
			trusted:    []string{"10.0.0.0/8"},
			xffLines:   []string{"203.0.113.9", ""},
			want:       "10.0.0.1",
		},
		{
			name:       "v4-mapped IPv6 peer and hop normalize to IPv4",
			remoteAddr: "[::ffff:10.0.0.1]:4321",
			trusted:    []string{"10.0.0.0/8"},
			xffLines:   []string{"::ffff:203.0.113.9"},
			want:       "203.0.113.9",
		},
		{
			name:       "IPv6 peer and hops",
			remoteAddr: "[2001:db8::1]:4321",
			trusted:    []string{"2001:db8::/64"},
			xffLines:   []string{"2001:db8:1::000a, 2001:db8::2"},
			want:       "2001:db8:1::a",
		},
		{
			name:       "X-Real-IP alone from trusted peer is ignored",
			remoteAddr: "10.0.0.1:4321",
			trusted:    []string{"10.0.0.0/8"},
			realIP:     "203.0.113.9",
			want:       "10.0.0.1",
		},
		{
			name:       "RemoteAddr without port is accepted",
			remoteAddr: "10.0.0.1",
			trusted:    []string{"10.0.0.0/8"},
			xffLines:   []string{"203.0.113.9"},
			want:       "203.0.113.9",
		},
		{
			name:       "untrusted RemoteAddr without port returns peer",
			remoteAddr: "198.51.100.5",
			trusted:    []string{"10.0.0.0/8"},
			xffLines:   []string{"203.0.113.9"},
			want:       "198.51.100.5",
		},
		{
			name:       "unparseable RemoteAddr returned as-is",
			remoteAddr: "conduit-relay:0",
			trusted:    []string{"10.0.0.0/8"},
			xffLines:   []string{"203.0.113.9"},
			want:       "conduit-relay",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/", nil)
			req.RemoteAddr = tt.remoteAddr
			for _, line := range tt.xffLines {
				req.Header.Add("X-Forwarded-For", line)
			}
			if tt.realIP != "" {
				req.Header.Set("X-Real-IP", tt.realIP)
			}

			got := trustedClientIP(req, parseTrustedProxies(tt.trusted))
			assert.Equal(t, tt.want, got)
		})
	}
}
