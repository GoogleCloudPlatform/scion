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

package substrate

import (
	"fmt"
	"os"
	"strings"

	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/rootexec"
	"github.com/GoogleCloudPlatform/scion/pkg/substrateenv"
)

// execCandidateEnv is execAsUserCmd's source of the pre-su environment, as
// a package var so a test can supply a synthetic environment without
// mutating the real process environment via os.Setenv.
var execCandidateEnv = os.Environ

// execResolve is execAsUserCmd's own source of a bare command name's
// verified, absolute path — a package var (rather than calling
// rootexec.Resolve directly) so a test can substitute a deterministic
// stand-in for "su" without needing a real recorder binary to sit under one
// of rootexec.SearchPath's fixed, non-test-injectable directories.
// Production code never reassigns it.
var execResolve = rootexec.Resolve

// SetExecResolveForTest overrides execAsUserCmd's own means of resolving
// "sh", "su", and "whoami" to absolute paths, for the duration of a test —
// including a test in another package that drives a real exec through this
// package's Server via NewServer (e.g. pkg/runtime's own real-control-server
// tests), which has no other way to reach this package's unexported
// execResolve var. Mirrors SetPrivateRootTmpDirForTest's shape exactly.
// Production code never calls this. Returns a cleanup function that
// restores the previous value.
func SetExecResolveForTest(resolve func(name string) (string, error)) func() {
	orig := execResolve
	execResolve = resolve
	return func() { execResolve = orig }
}

// execAsUserCmd is a deliberate, near-verbatim copy of
// pkg/runtime.ExecAsUserCmd's wrapper script (see that file's doc comment
// for the full PAM/su rationale — the reasoning is identical here), with
// one intentional divergence: it conditionally passes `su -w <list>`.
//
// It is duplicated rather than imported: pkg/runtime transitively imports
// pkg/config, and sciontool must never import pkg/config (project path
// resolution has no business inside an agent container — see
// TestInitProjectDataIsolation in cmd/sciontool/commands/init_test.go,
// which fails the build if that boundary is crossed). That import boundary
// rules out a shared leaf package for the whole function; only the
// CA-bundle var names (substrateenv.TrustBundleVarNames) are actually
// shared, via a small dependency-free package both sides can import (see
// that package's doc comment).
//
// "sh", "su", and "whoami" are all resolved via execResolve (production:
// rootexec.Resolve) rather than left as bare names for the eventual shell to
// look up on its own PATH: this whole wrapper runs as root before any
// privilege drop, and a bare name here would otherwise be resolved against
// root's own inherited PATH, which on substrate includes a directory the
// workload owns outright (see the rootexec package doc comment). Resolving
// all three up front, and embedding the resulting absolute paths directly
// in the generated script, means the script's own behavior no longer
// depends on whatever PATH the resulting *exec.Cmd happens to run with —
// runExec still gives it rootexec.Env's fixed, from-scratch PATH as well,
// but only as defense in depth.
//
// The divergence: whenever the target user differs from the caller,
// execAsUserCmd runs `su - <user> -c <cmd>`, a login shell — and `su -`
// discards the entire inherited environment, including the CA-bundle env
// vars buildActorTemplate (pkg/runtime/substrate_template.go) sets on the
// container when egress_trust_bundle is configured. Left unfixed, any
// exec-invoked command (the broker exec endpoint, `scion look`, or
// `/scion/v1/exec` directly) that makes a TLS request loses the gateway CA
// even though the harness itself trusts it fine.
//
// The mechanism is util-linux `su`'s `-w`/`--whitelist-environment` flag: a
// comma-separated list of variable names to copy from the pre-su
// environment into the post-su one, on top of `su -`'s own minimal login
// set. `-w <list>` is passed ONLY when at least one of the candidate names
// is actually set (to a non-empty value) in the pre-su environment — never
// unconditionally — so a plain (non-sdsmint) install, where none of them
// is ever set, gets the exact `su - "$1" -c "$2"` this wrapper has always
// run: byte-identical script, byte-identical argv. `<list>` names only the
// candidates that ARE set, in substrateenv.TrustBundleVarNames's fixed
// order — the same slice pkg/runtime's buildActorTemplate builds the
// container's Env from — so the two lists cannot drift apart (see
// TestExecAsUserCmd_CandidateNamesMatchTemplateEnvNames).
//
// pkg/runtime.ExecAsUserCmd (used by every other runtime) is deliberately
// left untouched: only Substrate actors run under sdsmint, `-w` is a
// util-linux-specific flag (>= 2.35; scion's images are Debian trixie,
// which satisfies this) not guaranteed present on every other runtime's
// image, and this whole mechanism only exists to counter Substrate's own
// env-propagation shape.
func execAsUserCmd(user, cmd string) ([]string, error) {
	suFlag := "-"
	if list := trustBundleWhitelist(execCandidateEnv()); list != "" {
		suFlag = "-w " + list + " -"
	}
	shPath, err := execResolve("sh")
	if err != nil {
		return nil, fmt.Errorf("substrate: resolve sh: %w", err)
	}
	suPath, err := execResolve("su")
	if err != nil {
		return nil, fmt.Errorf("substrate: resolve su: %w", err)
	}
	whoamiPath, err := execResolve("whoami")
	if err != nil {
		return nil, fmt.Errorf("substrate: resolve whoami: %w", err)
	}
	script := fmt.Sprintf(`if [ "$(%s)" = "$1" ]; then exec %s -c "$2"; else exec %s %s "$1" -c "$2"; fi`,
		whoamiPath, shPath, suPath, suFlag)
	return []string{shPath, "-c", script, "exec-as-user", user, cmd}, nil
}

// trustBundleEnvPairs returns "NAME=value" for each CA-bundle candidate
// present with a non-empty value in env, in substrateenv.TrustBundleVarNames's
// fixed order — the actual values a from-scratch child environment
// (rootexec.Env, which carries nothing over from the ambient environment by
// design) must be given explicitly for `su -w` to have anything to copy
// across the login shell's own environment reset. Shares its "present with
// a non-empty value" rule with trustBundleWhitelist, driven off the same
// env argument.
func trustBundleEnvPairs(env []string) []string {
	set := make(map[string]string, len(env))
	for _, kv := range env {
		if i := strings.IndexByte(kv, '='); i > 0 {
			set[kv[:i]] = kv[i+1:]
		}
	}
	var pairs []string
	for _, name := range substrateenv.TrustBundleVarNames {
		if v := set[name]; v != "" {
			pairs = append(pairs, name+"="+v)
		}
	}
	return pairs
}

// trustBundleWhitelist returns the comma-separated names, in
// substrateenv.TrustBundleVarNames's fixed order, of every candidate
// CA-bundle var with a non-empty value in env (KEY=VALUE strings, as from
// os.Environ()). Returns "" when none are set (the plain-install case, and
// the signal execAsUserCmd uses to omit `-w` entirely). An empty-string
// value counts as unset, matching `su -w`'s own behavior: there is nothing
// useful to copy for a key present but empty.
func trustBundleWhitelist(env []string) string {
	set := make(map[string]string, len(env))
	for _, kv := range env {
		if i := strings.IndexByte(kv, '='); i > 0 {
			set[kv[:i]] = kv[i+1:]
		}
	}
	var names []string
	for _, name := range substrateenv.TrustBundleVarNames {
		if set[name] != "" {
			names = append(names, name)
		}
	}
	return strings.Join(names, ",")
}
