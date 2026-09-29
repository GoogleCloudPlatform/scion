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
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/rootexec"
	"github.com/GoogleCloudPlatform/scion/pkg/substrateenv"
)

// withExecCandidateEnv swaps execAsUserCmd's env source for the duration of
// the test, restoring it on cleanup -- never touching the real process
// environment via os.Setenv.
func withExecCandidateEnv(t *testing.T, env []string) {
	t.Helper()
	orig := execCandidateEnv
	execCandidateEnv = func() []string { return env }
	t.Cleanup(func() { execCandidateEnv = orig })
}

// resolvedTrio resolves "whoami", "sh", and "su" the same way execAsUserCmd
// itself does (via the real rootexec.Resolve, not a test stand-in), so a
// test can build its expected script text without hardcoding an
// image-specific absolute path (e.g. "/usr/bin/dash" vs "/bin/dash") that
// would make the test fragile across runtimes.
func resolvedTrio(t *testing.T) (whoamiPath, shPath, suPath string) {
	t.Helper()
	var err error
	if whoamiPath, err = rootexec.Resolve("whoami"); err != nil {
		t.Fatalf("resolve whoami: %v", err)
	}
	if shPath, err = rootexec.Resolve("sh"); err != nil {
		t.Fatalf("resolve sh: %v", err)
	}
	if suPath, err = rootexec.Resolve("su"); err != nil {
		t.Fatalf("resolve su: %v", err)
	}
	return whoamiPath, shPath, suPath
}

// TestExecAsUserCmd_PlainEnvHasResolvedAbsolutePaths: with none of the
// CA-bundle vars set, execAsUserCmd must produce a script built entirely
// from execResolve's (rootexec.Resolve's) own answers for "whoami", "sh",
// and "su" — never a bare name a shell would go on to look up on its own
// PATH — so an accidental reversion to a bare name is caught immediately on
// the plain-install path.
func TestExecAsUserCmd_PlainEnvHasResolvedAbsolutePaths(t *testing.T) {
	withExecCandidateEnv(t, []string{"PATH=/usr/bin", "HOME=/home/scion"})
	whoamiPath, shPath, suPath := resolvedTrio(t)

	got, err := execAsUserCmd("scion", "true")
	if err != nil {
		t.Fatalf("execAsUserCmd: %v", err)
	}
	want := []string{
		shPath, "-c",
		fmt.Sprintf(`if [ "$(%s)" = "$1" ]; then exec %s -c "$2"; else exec %s - "$1" -c "$2"; fi`, whoamiPath, shPath, suPath),
		"exec-as-user", "scion", "true",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("execAsUserCmd() = %#v, want %#v", got, want)
	}
}

// TestExecAsUserCmd_AllCAVarsSet is test (b): with all 5 candidate vars set,
// -w carries exactly those 5, in substrateenv.TrustBundleVarNames's order.
func TestExecAsUserCmd_AllCAVarsSet(t *testing.T) {
	withExecCandidateEnv(t, []string{
		"NODE_EXTRA_CA_CERTS=/run/ate/trust-bundle.pem",
		"GIT_SSL_CAINFO=/run/ate/trust-bundle.pem",
		"SSL_CERT_FILE=/run/ate/trust-bundle.pem",
		"CURL_CA_BUNDLE=/run/ate/trust-bundle.pem",
		"SSL_CERT_DIR=/run/ate",
	})
	whoamiPath, shPath, suPath := resolvedTrio(t)

	got, err := execAsUserCmd("scion", "true")
	if err != nil {
		t.Fatalf("execAsUserCmd: %v", err)
	}
	wantScript := fmt.Sprintf(`if [ "$(%s)" = "$1" ]; then exec %s -c "$2"; else exec %s -w NODE_EXTRA_CA_CERTS,GIT_SSL_CAINFO,SSL_CERT_FILE,CURL_CA_BUNDLE,SSL_CERT_DIR - "$1" -c "$2"; fi`,
		whoamiPath, shPath, suPath)
	if got[2] != wantScript {
		t.Errorf("script = %q, want %q", got[2], wantScript)
	}
}

// TestExecAsUserCmd_SubsetOfCAVarsSet is test (c): with only 2 of the 5
// candidate vars set, -w carries exactly those 2, in the fixed order (not
// the order they appear in env).
func TestExecAsUserCmd_SubsetOfCAVarsSet(t *testing.T) {
	withExecCandidateEnv(t, []string{
		"SSL_CERT_FILE=/run/ate/trust-bundle.pem",
		"NODE_EXTRA_CA_CERTS=/run/ate/trust-bundle.pem",
	})
	whoamiPath, shPath, suPath := resolvedTrio(t)

	got, err := execAsUserCmd("scion", "true")
	if err != nil {
		t.Fatalf("execAsUserCmd: %v", err)
	}
	wantScript := fmt.Sprintf(`if [ "$(%s)" = "$1" ]; then exec %s -c "$2"; else exec %s -w NODE_EXTRA_CA_CERTS,SSL_CERT_FILE - "$1" -c "$2"; fi`,
		whoamiPath, shPath, suPath)
	if got[2] != wantScript {
		t.Errorf("script = %q, want %q", got[2], wantScript)
	}
}

// TestExecAsUserCmd_EmptyValueCountsAsUnset is test (d): a candidate name
// present in the environment with an empty value must be treated the same
// as absent.
func TestExecAsUserCmd_EmptyValueCountsAsUnset(t *testing.T) {
	withExecCandidateEnv(t, []string{
		"SSL_CERT_FILE=",
		"NODE_EXTRA_CA_CERTS=/run/ate/trust-bundle.pem",
	})
	whoamiPath, shPath, suPath := resolvedTrio(t)

	got, err := execAsUserCmd("scion", "true")
	if err != nil {
		t.Fatalf("execAsUserCmd: %v", err)
	}
	wantScript := fmt.Sprintf(`if [ "$(%s)" = "$1" ]; then exec %s -c "$2"; else exec %s -w NODE_EXTRA_CA_CERTS - "$1" -c "$2"; fi`,
		whoamiPath, shPath, suPath)
	if got[2] != wantScript {
		t.Errorf("script = %q, want %q (SSL_CERT_FILE= must count as unset)", got[2], wantScript)
	}
}

// TestExecAsUserCmd_CandidateNamesMatchTemplateEnvNames confirms
// trustBundleWhitelist walks substrateenv.TrustBundleVarNames itself, in
// that slice's own order, rather than some independent, hard-coded copy of
// the names or a different (e.g. map-derived) order. It is not what
// protects against a name being dropped from the shared slice — with a
// name missing, both this test and its counterpart on the template side
// would agree on the shorter list and still pass. That drift protection
// comes from TestExecAsUserCmd_AllCAVarsSet's and
// TestBuildActorTemplate_EgressTrustBundleSet's hand-written literals,
// which name all 5 vars explicitly.
func TestExecAsUserCmd_CandidateNamesMatchTemplateEnvNames(t *testing.T) {
	env := make([]string, 0, len(substrateenv.TrustBundleVarNames))
	for _, name := range substrateenv.TrustBundleVarNames {
		env = append(env, name+"=x")
	}

	got := trustBundleWhitelist(env)
	want := strings.Join(substrateenv.TrustBundleVarNames, ",")
	if got != want {
		t.Errorf("trustBundleWhitelist() = %q, want %q (substrateenv.TrustBundleVarNames)", got, want)
	}
}

// TestTrustBundleEnvPairs_MatchesWhitelistNames proves trustBundleEnvPairs
// (which hands a from-scratch child environment the actual values su -w is
// about to ask to copy) and trustBundleWhitelist (which decides those same
// names for the -w flag itself) always agree on which names are present:
// this is the pairing that makes su -w meaningful again once the child no
// longer inherits the ambient environment wholesale.
func TestTrustBundleEnvPairs_MatchesWhitelistNames(t *testing.T) {
	env := []string{
		"SSL_CERT_FILE=/run/ate/trust-bundle.pem",
		"NODE_EXTRA_CA_CERTS=/run/ate/trust-bundle.pem",
		"UNRELATED=x",
	}
	wantNames := trustBundleWhitelist(env)
	pairs := trustBundleEnvPairs(env)

	gotNames := make([]string, 0, len(pairs))
	for _, kv := range pairs {
		name, _, _ := strings.Cut(kv, "=")
		gotNames = append(gotNames, name)
	}
	if strings.Join(gotNames, ",") != wantNames {
		t.Errorf("trustBundleEnvPairs names = %q, want %q (must match trustBundleWhitelist)", strings.Join(gotNames, ","), wantNames)
	}
}

// TestExecAsUserCmd_ResolveFailureIsRefused proves a resolution failure for
// any of "whoami", "sh", or "su" is returned as an error, never silently
// papered over with a bare name.
func TestExecAsUserCmd_ResolveFailureIsRefused(t *testing.T) {
	orig := execResolve
	execResolve = func(name string) (string, error) { return "", fmt.Errorf("boom: %s", name) }
	t.Cleanup(func() { execResolve = orig })

	if _, err := execAsUserCmd("scion", "true"); err == nil {
		t.Fatal("expected execAsUserCmd to fail when execResolve fails, got nil error")
	}
}

// TestExecAsUserCmd_RealShellInvokesSuWithExpectedArgv is a confidence test
// beyond the minimum required: the candidate list is built in Go (see
// execAsUserCmd's doc comment for why), and this repository has no existing
// convention that requires exercising a real shell for a wrapper script
// whose logic lives entirely in the Go string that generates it. It's
// cheap and closes the remaining gap between "the Go string looks right"
// and "a real shell parses it the way we expect": it substitutes execResolve
// so "su" resolves to a recorder script (rootexec.SearchPath is fixed and
// not test-injectable by design — this is the same seam execResolve exists
// for), then runs the actual generated script and confirms the recorder
// receives exactly the argv `su` would.
func TestExecAsUserCmd_RealShellInvokesSuWithExpectedArgv(t *testing.T) {
	dir := t.TempDir()
	recorderPath := filepath.Join(dir, "argv.txt")
	suScript := "#!/bin/sh\necho \"$@\" > " + recorderPath + "\n"
	suRecorder := filepath.Join(dir, "su")
	if err := os.WriteFile(suRecorder, []byte(suScript), 0o755); err != nil {
		t.Fatal(err)
	}

	withExecCandidateEnv(t, []string{
		"SSL_CERT_FILE=/run/ate/trust-bundle.pem",
		"NODE_EXTRA_CA_CERTS=/run/ate/trust-bundle.pem",
	})

	origResolve := execResolve
	execResolve = func(name string) (string, error) {
		if name == "su" {
			return suRecorder, nil
		}
		return origResolve(name)
	}
	t.Cleanup(func() { execResolve = origResolve })

	// A user guaranteed to differ from whatever this test process's real
	// UID resolves to, so the wrapper's whoami check takes the `su` branch
	// (the `sh -c` branch already inherits the environment unmodified and
	// is covered separately).
	argv, err := execAsUserCmd("nonexistent-user-for-test", "true")
	if err != nil {
		t.Fatalf("execAsUserCmd: %v", err)
	}
	cmd := exec.Command(argv[0], argv[1:]...)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("running wrapper: %v, output=%s", err, out)
	}

	got, err := os.ReadFile(recorderPath)
	if err != nil {
		t.Fatalf("su recorder was never invoked: %v", err)
	}
	want := "-w NODE_EXTRA_CA_CERTS,SSL_CERT_FILE - nonexistent-user-for-test -c true\n"
	if string(got) != want {
		t.Errorf("su recorder saw argv %q, want %q", string(got), want)
	}
}
