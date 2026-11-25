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

package messages

import (
	"strings"
	"testing"
)

func TestCommandRequestCanonicalHash(t *testing.T) {
	h1 := CommandRequestCanonicalHash("echo a && echo b > /tmp/x", "", 600)
	h2 := CommandRequestCanonicalHash("echo a && echo b > /tmp/x", "", 600)
	if h1 != h2 {
		t.Fatalf("hash not deterministic: %s != %s", h1, h2)
	}
	if len(h1) != 64 {
		t.Fatalf("hash len = %d, want 64", len(h1))
	}
	// Changing any of script, workdir, or timeout changes the hash.
	if h1 == CommandRequestCanonicalHash("echo a && echo c > /tmp/x", "", 600) {
		t.Errorf("hash did not change when script changed")
	}
	if h1 == CommandRequestCanonicalHash("echo a && echo b > /tmp/x", "/home/scion", 600) {
		t.Errorf("hash did not change when workdir changed")
	}
	if h1 == CommandRequestCanonicalHash("echo a && echo b > /tmp/x", "", 300) {
		t.Errorf("hash did not change when timeout changed")
	}
}

func TestNormalizeCommandRequest(t *testing.T) {
	t.Run("valid defaults", func(t *testing.T) {
		got, err := NormalizeCommandRequest("command_request", &CommandRequestInput{
			Key:         "Deploy-Preview",
			Description: "Deploy PR 42",
			Script:      "gcloud config list\n",
		})
		if err != nil {
			t.Fatalf("NormalizeCommandRequest err = %v", err)
		}
		if got.KeyNorm != "deploy-preview" {
			t.Errorf("KeyNorm = %q, want deploy-preview", got.KeyNorm)
		}
		if got.TimeoutS != DefaultCommandRequestTimeoutS {
			t.Errorf("TimeoutS = %d, want %d", got.TimeoutS, DefaultCommandRequestTimeoutS)
		}
		if got.CommandSHA256 != CommandRequestCanonicalHash("gcloud config list\n", "", DefaultCommandRequestTimeoutS) {
			t.Errorf("CommandSHA256 mismatch")
		}
	})

	t.Run("rejects control and bidi chars in script", func(t *testing.T) {
		cases := []struct {
			name   string
			script string
		}{
			{"nul", "echo \x00 hi"},
			{"cr", "echo hi\r\n"},
			{"esc", "echo \x1b[31mred"},
			{"del", "echo \x7f"},
			{"bidi_rlo", "echo \u202ehi"},
			{"bidi_lri", "echo \u2066hi"},
			{"bidi_alm", "echo \u061chi"},
			{"invalid_utf8", string([]byte{0xff, 0xfe})},
			{"too_long", strings.Repeat("a", MaxCommandRequestScriptBytes+1)},
		}
		for _, tc := range cases {
			_, err := NormalizeCommandRequest("command_request", &CommandRequestInput{
				Key:    "k1",
				Script: tc.script,
			})
			if err == nil {
				t.Errorf("%s: expected validation error, got nil", tc.name)
			}
		}
	})

	t.Run("allows tabs newlines and non-ascii in script", func(t *testing.T) {
		_, err := NormalizeCommandRequest("command_request", &CommandRequestInput{
			Key:    "k1",
			Script: "echo 'héllo'\n\techo 'world'\n",
		})
		if err != nil {
			t.Fatalf("unexpected err: %v", err)
		}
	})

	t.Run("workdir validation", func(t *testing.T) {
		for _, bad := range []string{"relative/path", "/a/../b", "/a/./b", "/a//b", "/a/b/", "/a\nb", strings.Repeat("/a", 300)} {
			_, err := NormalizeCommandRequest("command_request", &CommandRequestInput{
				Key:     "k1",
				Script:  "pwd",
				Workdir: bad,
			})
			if err == nil {
				t.Errorf("workdir %q: expected error, got nil", bad)
			}
		}
		for _, good := range []string{"", "/", "/home/scion/repo"} {
			_, err := NormalizeCommandRequest("command_request", &CommandRequestInput{
				Key:     "k1",
				Script:  "pwd",
				Workdir: good,
			})
			if err != nil {
				t.Errorf("workdir %q: unexpected error %v", good, err)
			}
		}
	})

	t.Run("timeout range", func(t *testing.T) {
		for _, bad := range []int{-1, MaxCommandRequestTimeoutS + 1} {
			_, err := NormalizeCommandRequest("command_request", &CommandRequestInput{
				Key:      "k1",
				Script:   "true",
				TimeoutS: bad,
			})
			if err == nil {
				t.Errorf("timeout %d: expected error", bad)
			}
		}
	})
}
