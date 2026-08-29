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

package runtime

import (
	"strings"
	"testing"
)

func TestRedactEnvValues(t *testing.T) {
	tests := []struct {
		name    string
		in      string
		secrets map[string]string
		want    string
	}{
		{
			name:    "value is replaced with a marker naming its key",
			in:      "--env API_KEY=" + argvLeakSentinel,
			secrets: map[string]string{"API_KEY": argvLeakSentinel},
			want:    "--env API_KEY=[value of API_KEY redacted]",
		},
		{
			name:    "every occurrence is replaced, not just the first",
			in:      "a " + argvLeakSentinel + " b " + argvLeakSentinel,
			secrets: map[string]string{"K": argvLeakSentinel},
			want:    "a [value of K redacted] b [value of K redacted]",
		},
		{
			name:    "values below the floor are left alone",
			in:      "exit code 1 after 1 attempt, PATH=/tmp",
			secrets: map[string]string{"RETRIES": "1", "TMPDIR": "/tmp"},
			want:    "exit code 1 after 1 attempt, PATH=/tmp",
		},
		{
			name:    "a value exactly at the floor is redacted",
			in:      "token=12345678",
			secrets: map[string]string{"TOK": "12345678"},
			want:    "token=[value of TOK redacted]",
		},
		{
			name:    "a value one byte under the floor is not",
			in:      "token=1234567",
			secrets: map[string]string{"TOK": "1234567"},
			want:    "token=1234567",
		},
		{
			// Longest-first ordering. If the shorter value were replaced first,
			// the longer one would be left as a mangled fragment plus a marker,
			// and the fragment would still be part of a live credential.
			name: "a secret containing another secret is redacted whole",
			in:   "outer=SUPERSECRETVALUE-EXTRA inner=SUPERSECRETVALUE",
			secrets: map[string]string{
				"OUTER": "SUPERSECRETVALUE-EXTRA",
				"INNER": "SUPERSECRETVALUE",
			},
			want: "outer=[value of OUTER redacted] inner=[value of INNER redacted]",
		},
		{
			name:    "no secrets is a no-op",
			in:      "nothing to do here",
			secrets: map[string]string{},
			want:    "nothing to do here",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := redactEnvValues(tc.in, tc.secrets); got != tc.want {
				t.Errorf("redactEnvValues()\n got: %s\nwant: %s", got, tc.want)
			}
		})
	}
}

// THIS TEST IS THE JUSTIFICATION FOR SCOPING REDACTION BY PROVENANCE, and it is
// the measurement behind that design choice rather than an argument for it.
//
// The obvious reading of "redact the values the runtime put in argv" is: every
// value. envFor synthesises HOME=/home/scion (11 chars), SCION_WORKSPACE_PATH=
// /workspace (10) and PATH=... (65). All three clear an 8-character floor, none
// is a secret, and all three appear inside the mount specifications:
//
//	--mount type=bind,source=...,destination=/home/scion
//
// Redacting by value would rewrite those destinations into redaction markers.
// Mount failures are precisely what this output exists to diagnose -- the fake
// binary in the sibling test says "mount target busy" for that reason. So the
// naive reading destroys the diagnostic exactly where it is most needed, which
// is the failure mode #22 and #46 were spent eliminating.
func TestRedactEnvValues_KeepsSynthesisedPathsIntact(t *testing.T) {
	out := "sandbox: fatal: mount target busy\n" +
		"--mount type=bind,source=/scion/agents/a/home,destination=/home/scion\n" +
		"--mount type=bind,source=/scion/agents/a/workspace,destination=/workspace\n" +
		"--env ANTHROPIC_API_KEY=" + argvLeakSentinel + "\n"

	env := map[string]string{
		"HOME":                 sandboxAgentHome,
		"SCION_WORKSPACE_PATH": sandboxWorkspace,
		"PATH":                 "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin",
		"ANTHROPIC_API_KEY":    argvLeakSentinel,
	}
	cfg := RunConfig{Env: []string{"ANTHROPIC_API_KEY=" + argvLeakSentinel}}

	got := redactEnvValues(out, externalEnvValues(cfg, env))

	if strings.Contains(got, argvLeakSentinel) {
		t.Errorf("secret survived redaction:\n%s", got)
	}
	for _, keep := range []string{
		"destination=/home/scion",
		"destination=/workspace",
		"mount target busy",
	} {
		if !strings.Contains(got, keep) {
			t.Errorf("redaction destroyed diagnostic content %q:\n%s", keep, got)
		}
	}
}

func TestExternalEnvValues(t *testing.T) {
	t.Run("runtime-synthesised values are not treated as secrets", func(t *testing.T) {
		env := map[string]string{"HOME": sandboxAgentHome, "USER": "scion"}
		got := externalEnvValues(RunConfig{}, env)
		if len(got) != 0 {
			t.Errorf("want no external values, got %v", got)
		}
	})

	t.Run("broker-supplied values are", func(t *testing.T) {
		cfg := RunConfig{Env: []string{"API_KEY=" + argvLeakSentinel}}
		env := map[string]string{"API_KEY": argvLeakSentinel, "HOME": sandboxAgentHome}
		got := externalEnvValues(cfg, env)
		if got["API_KEY"] != argvLeakSentinel {
			t.Errorf("broker-supplied API_KEY not returned: %v", got)
		}
		if _, ok := got["HOME"]; ok {
			t.Errorf("synthesised HOME should not be returned: %v", got)
		}
	})

	t.Run("harness-supplied values are", func(t *testing.T) {
		cfg := RunConfig{Harness: &mockHarness{env: map[string]string{"H_TOKEN": argvLeakSentinel}}}
		env := map[string]string{"H_TOKEN": argvLeakSentinel}
		if got := externalEnvValues(cfg, env); got["H_TOKEN"] != argvLeakSentinel {
			t.Errorf("harness-supplied H_TOKEN not returned: %v", got)
		}
	})

	t.Run("a value overwritten before argv is not redacted", func(t *testing.T) {
		// envFor parses cfg.Env first, then overwrites some keys with
		// synthesised values. A value that never reached argv cannot leak, and
		// redacting it would strip a coincidentally matching string from the
		// message for no benefit.
		cfg := RunConfig{Env: []string{"HOME=/somewhere/else"}}
		env := map[string]string{"HOME": sandboxAgentHome}
		if got := externalEnvValues(cfg, env); len(got) != 0 {
			t.Errorf("overwritten value should not be redacted, got %v", got)
		}
	})

	t.Run("a malformed env entry without = is ignored", func(t *testing.T) {
		cfg := RunConfig{Env: []string{"NOT_A_PAIR"}}
		if got := externalEnvValues(cfg, map[string]string{}); len(got) != 0 {
			t.Errorf("want no values, got %v", got)
		}
	})
}
