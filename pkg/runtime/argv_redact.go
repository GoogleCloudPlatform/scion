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
	"sort"
	"strings"
)

// Redaction of credential values that the runtime placed in a subprocess argv,
// for use when that subprocess's failure is reported to a caller. See #1342.
//
// The runtime is in the strongest position available here: it KNOWS every value
// it passed. Nothing below pattern-matches for things that look like secrets,
// because guessing is both unnecessary and unreliable when the exact values are
// in hand.

// envValueRedactionFloor is the shortest value that will be redacted.
//
// WHY A FLOOR AT ALL. Redaction is substring replacement over the whole error
// message. A value of "1", "true" or "/tmp" occurs all over an unrelated
// diagnostic, and replacing every occurrence would destroy the message to
// protect a string that is not a secret.
//
// WHY 8. It sits in a gap whose edges are measured from this repository rather
// than guessed at:
//
//   - BELOW the shortest credential this project handles, with margin. The
//     shortest credential it MINTS is a dev token: DevTokenPrefix "scion_dev_"
//     plus hex(DevTokenLength=32) = 74 characters (pkg/apiclient/devauth.go).
//     The shortest it CONSUMES is bounded by pkg/harness/claude_provision.go,
//     which fingerprints an API key as its last 20 characters and would panic
//     on anything shorter -- so 20 is an existing, enforced floor on credential
//     length elsewhere in the codebase. 8 is under half of that.
//   - ABOVE the common short non-secret values that reach env in practice:
//     "1", "true", "scion", "1002", "/tmp", "prod".
//
// The gap between 8 and 20 is wide enough that the number is not delicate:
// anything in 8..16 would behave identically on every value this runtime
// actually passes. 8 is chosen at the low end because the cost of the two
// errors is asymmetric -- redacting a short non-secret costs one confusing
// line, and failing to redact a short secret costs a credential.
//
// KNOWN LIMITATION, stated rather than hidden: a credential shorter than 8
// characters is NOT redacted. No credential format in use here is that short --
// see the 20-character floor above -- but a future one could be, and this
// constant is where that assumption lives.
const envValueRedactionFloor = 8

// externalEnvValues returns the KEY=VALUE env entries that originated OUTSIDE
// the runtime -- broker-supplied cfg.Env and harness-supplied env -- and that
// survived into the final env map.
//
// WHY NOT SIMPLY EVERY VALUE THE RUNTIME PUT IN ARGV. Because that measurably
// destroys the diagnostic. envFor synthesises HOME=/home/scion,
// SCION_WORKSPACE_PATH=/workspace and PATH=..., all of which clear an
// 8-character floor, and all of which appear in the parts of the message an
// operator actually needs:
//
//	--mount type=bind,source=...,destination=/home/scion
//	--mount type=bind,source=...,destination=/workspace
//
// Redacting by value would rewrite those mount destinations into redaction
// markers -- and mount failures are exactly the failure this output exists to
// diagnose. See TestRedactEnvValues_KeepsSynthesisedPathsIntact.
//
// This is not a weakening. A runtime-synthesised constant cannot be a
// credential: it is built here from non-secret configuration. Everything that
// could carry a secret arrives from outside, and that is precisely the set
// returned.
func externalEnvValues(cfg RunConfig, env map[string]string) map[string]string {
	external := make(map[string]string)

	for _, e := range cfg.Env {
		if k, v, ok := strings.Cut(e, "="); ok {
			external[k] = v
		}
	}
	if cfg.Harness != nil {
		for k, v := range cfg.Harness.GetEnv(cfg.Name, sandboxAgentHome, cfg.UnixUsername) {
			external[k] = v
		}
	}

	// Keep only entries that actually reached argv with that value. envFor
	// overwrites some keys with synthesised values after parsing cfg.Env, and a
	// value that was never passed cannot leak -- redacting it would remove a
	// coincidentally matching string from the message for no benefit.
	live := make(map[string]string, len(external))
	for k, v := range external {
		if env[k] == v {
			live[k] = v
		}
	}
	return live
}

// redactEnvValues replaces each secret value in s with a marker naming its key.
//
// The marker names the KEY on purpose. A key name is not a credential, and
// "[value of ANTHROPIC_API_KEY redacted]" tells an operator that a value was
// removed and which one, where a silently shortened string is a puzzle.
func redactEnvValues(s string, secrets map[string]string) string {
	keys := make([]string, 0, len(secrets))
	for k, v := range secrets {
		if len(v) >= envValueRedactionFloor {
			keys = append(keys, k)
		}
	}

	// Longest value first. If one secret's value contains another's, replacing
	// the shorter one first would leave a fragment of the longer one behind.
	// Ties break on key name so the output is deterministic and diffable.
	sort.Slice(keys, func(i, j int) bool {
		li, lj := len(secrets[keys[i]]), len(secrets[keys[j]])
		if li != lj {
			return li > lj
		}
		return keys[i] < keys[j]
	})

	for _, k := range keys {
		s = strings.ReplaceAll(s, secrets[k], "[value of "+k+" redacted]")
	}
	return s
}
