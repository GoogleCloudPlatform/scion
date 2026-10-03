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
	"os/user"
	"strconv"
	"strings"
	"testing"
)

// withExecCandidateEnv swaps runExec's CA-bundle env source for the duration
// of the test, restoring it on cleanup -- never touching the real process
// environment via os.Setenv.
func withExecCandidateEnv(t *testing.T, env []string) {
	t.Helper()
	orig := execCandidateEnv
	execCandidateEnv = func() []string { return env }
	t.Cleanup(func() { execCandidateEnv = orig })
}

// TestTrustBundleEnvPairs_AllCAVarsSet is test (b): with all 5 candidate
// vars set, every one of them is carried through, in
// substrateenv.TrustBundleVarNames's order.
func TestTrustBundleEnvPairs_AllCAVarsSet(t *testing.T) {
	env := []string{
		"NODE_EXTRA_CA_CERTS=/run/ate/trust-bundle.pem",
		"GIT_SSL_CAINFO=/run/ate/trust-bundle.pem",
		"SSL_CERT_FILE=/run/ate/trust-bundle.pem",
		"CURL_CA_BUNDLE=/run/ate/trust-bundle.pem",
		"SSL_CERT_DIR=/run/ate",
	}
	got := trustBundleEnvPairs(env)
	gotNames := namesOf(got)
	want := "NODE_EXTRA_CA_CERTS,GIT_SSL_CAINFO,SSL_CERT_FILE,CURL_CA_BUNDLE,SSL_CERT_DIR"
	if gotNames != want {
		t.Errorf("trustBundleEnvPairs names = %q, want %q", gotNames, want)
	}
}

// TestTrustBundleEnvPairs_SubsetOfCAVarsSet is test (c): with only 2 of the
// 5 candidate vars set, only those 2 are carried, in the fixed order (not
// the order they appear in env).
func TestTrustBundleEnvPairs_SubsetOfCAVarsSet(t *testing.T) {
	env := []string{
		"SSL_CERT_FILE=/run/ate/trust-bundle.pem",
		"NODE_EXTRA_CA_CERTS=/run/ate/trust-bundle.pem",
	}
	got := trustBundleEnvPairs(env)
	gotNames := namesOf(got)
	want := "NODE_EXTRA_CA_CERTS,SSL_CERT_FILE"
	if gotNames != want {
		t.Errorf("trustBundleEnvPairs names = %q, want %q", gotNames, want)
	}
}

// TestTrustBundleEnvPairs_EmptyValueCountsAsUnset is test (d): a candidate
// name present in the environment with an empty value must be treated the
// same as absent.
func TestTrustBundleEnvPairs_EmptyValueCountsAsUnset(t *testing.T) {
	env := []string{
		"SSL_CERT_FILE=",
		"NODE_EXTRA_CA_CERTS=/run/ate/trust-bundle.pem",
	}
	got := trustBundleEnvPairs(env)
	gotNames := namesOf(got)
	want := "NODE_EXTRA_CA_CERTS"
	if gotNames != want {
		t.Errorf("trustBundleEnvPairs names = %q, want %q (SSL_CERT_FILE= must count as unset)", gotNames, want)
	}
}

func namesOf(pairs []string) string {
	names := make([]string, 0, len(pairs))
	for _, kv := range pairs {
		name, _, _ := strings.Cut(kv, "=")
		names = append(names, name)
	}
	return strings.Join(names, ",")
}

// TestExecUserCredential_SetsHomeUserLognameShellForScion is the direct
// regression test for A2: for a target user with a passwd entry distinct
// from this process's own, execUserCredential must return HOME, USER,
// LOGNAME, and SHELL explicitly, and a non-nil Credential naming that
// user's uid/gid — never leave any of the four unset the way inheriting an
// ambient environment might.
func TestExecUserCredential_SetsHomeUserLognameShellForScion(t *testing.T) {
	// Deliberately NOT a fixed literal like 1002/1003: this test's whole
	// point is that these differ from whatever uid/gid the test process
	// actually runs under, and some environments (this sandbox among
	// them) run tests as a real "scion" account whose own uid/gid would
	// otherwise coincidentally collide with a hardcoded fake one,
	// defeating the "must request a real drop" assertion below.
	fakeUID, fakeGID := os.Geteuid()+1000, os.Getegid()+1000
	fakeUser := &user.User{Uid: strconv.Itoa(fakeUID), Gid: strconv.Itoa(fakeGID), Username: "scion", HomeDir: "/home/scion"}
	restore := SetExecUserLookupForTest(func(username string) (*user.User, error) {
		if username != "scion" {
			t.Fatalf("execUserLookup called with %q, want \"scion\"", username)
		}
		return fakeUser, nil
	})
	defer restore()

	const shPath = "/bin/sh"
	envPairs, cred, err := execUserCredential("scion", shPath)
	if err != nil {
		t.Fatalf("execUserCredential: %v", err)
	}

	want := map[string]string{
		"HOME":    "/home/scion",
		"USER":    "scion",
		"LOGNAME": "scion",
		"SHELL":   shPath,
	}
	got := make(map[string]string, len(envPairs))
	for _, kv := range envPairs {
		k, v, _ := strings.Cut(kv, "=")
		got[k] = v
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("env %s = %q, want %q (got envPairs = %v)", k, got[k], v, envPairs)
		}
	}
	if len(got) != len(want) {
		t.Errorf("envPairs = %v, want exactly HOME/USER/LOGNAME/SHELL, nothing else", envPairs)
	}

	// fakeUID/fakeGID are constructed above to differ from this process's
	// own euid/egid, so a real credential drop must be requested.
	if cred == nil {
		t.Fatal("cred = nil, want a non-nil *syscall.Credential for a different uid/gid")
	}
	if cred.Uid != uint32(fakeUID) || cred.Gid != uint32(fakeGID) {
		t.Errorf("cred = %+v, want Uid=%d Gid=%d", cred, fakeUID, fakeGID)
	}
}

// TestExecUserCredential_SameIdentitySkipsCredential proves the "already
// this identity" shortcut: when the resolved target user's uid/gid already
// match this process's own (euid/egid), execUserCredential must return a
// nil Credential rather than one naming the same uid/gid. This matters
// beyond optimization: Go's exec implementation calls setgroups() whenever
// Credential is non-nil (unless NoSetGroups is set), which requires
// CAP_SETGID even to set an unchanged group list — a privilege an
// unprivileged test process asking to "become" the user it already is does
// not have and must not need.
func TestExecUserCredential_SameIdentitySkipsCredential(t *testing.T) {
	me, err := user.Current()
	if err != nil {
		t.Skipf("could not resolve current user: %v", err)
	}
	restore := SetExecUserLookupForTest(func(username string) (*user.User, error) {
		return me, nil
	})
	defer restore()

	_, cred, err := execUserCredential(me.Username, "/bin/sh")
	if err != nil {
		t.Fatalf("execUserCredential: %v", err)
	}
	if cred != nil {
		t.Errorf("cred = %+v, want nil when the target user is this process's own current identity", cred)
	}
}

// TestExecUserCredential_LookupFailureIsRefused proves a user-resolution
// failure is returned as an error, never silently papered over with a
// zero-value credential.
func TestExecUserCredential_LookupFailureIsRefused(t *testing.T) {
	restore := SetExecUserLookupForTest(func(username string) (*user.User, error) {
		return nil, fmt.Errorf("boom: %s", username)
	})
	defer restore()

	if _, _, err := execUserCredential("scion", "/bin/sh"); err == nil {
		t.Fatal("expected execUserCredential to fail when execUserLookup fails, got nil error")
	}
}

// TestExecUserCredential_RealScionUser is a confidence test beyond the
// minimum required: it resolves the REAL "scion" system user (expected to
// exist on any image this control server actually runs in, and confirmed
// present in this sandbox) via the real execUserLookup (no override),
// closing the gap between "the fake passwd entry behaves as expected" and
// "a real os/user.Lookup call does too."
func TestExecUserCredential_RealScionUser(t *testing.T) {
	if _, err := user.Lookup("scion"); err != nil {
		t.Skipf("no real \"scion\" user on this machine: %v", err)
	}
	envPairs, _, err := execUserCredential("scion", "/bin/sh")
	if err != nil {
		t.Fatalf("execUserCredential: %v", err)
	}
	got := make(map[string]string, len(envPairs))
	for _, kv := range envPairs {
		k, v, _ := strings.Cut(kv, "=")
		got[k] = v
	}
	if got["USER"] != "scion" || got["LOGNAME"] != "scion" {
		t.Errorf("envPairs = %v, want USER and LOGNAME = scion", envPairs)
	}
	if got["HOME"] == "" {
		t.Errorf("envPairs = %v, want a non-empty HOME", envPairs)
	}
	if got["SHELL"] != "/bin/sh" {
		t.Errorf("envPairs SHELL = %q, want %q (the resolved sh path, not the passwd-configured login shell)", got["SHELL"], "/bin/sh")
	}
}
