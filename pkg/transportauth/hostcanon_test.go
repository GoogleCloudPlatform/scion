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

package transportauth

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestCanonicalHost(t *testing.T) {
	for in, want := range map[string]string{
		"Hub.Example.COM":      "hub.example.com",
		"hub.example.com.":     "hub.example.com",
		"0:0:0:0:0:0:0:1":      "::1",
		"::ffff:127.0.0.1":     "127.0.0.1",
		"127.0.0.1":            "127.0.0.1",
		"2001:DB8::1":          "2001:db8::1",
		"host.docker.internal": "host.docker.internal",
		"LOCALHOST.":           "localhost",
	} {
		assert.Equal(t, want, CanonicalHost(in), in)
	}
}

// IsLoopbackHost matches pkg/hub.IsLoopbackHost and W4b's
// config.IsLoopbackHost exactly; one case per edge.
func TestIsLoopbackHost(t *testing.T) {
	cases := []struct {
		host string
		want bool
	}{
		{"localhost", true},
		{"127.0.0.1", true},
		{"127.1.2.3", true},
		{"::1", true},
		{"0:0:0:0:0:0:0:1", true},
		{"::ffff:127.0.0.1", true},
		{"0.0.0.0", false},
		{"::", false},
		{"hub.localhost", false},
		{"localhost.", false},
		{"LocalHost", false},
		{"localhost.evil.example", false},
		{"10.0.0.1", false},
		{"169.254.169.254", false},
		{"host.docker.internal", false},
		{"", false},
	}
	for _, tc := range cases {
		assert.Equal(t, tc.want, IsLoopbackHost(tc.host), tc.host)
	}
}
