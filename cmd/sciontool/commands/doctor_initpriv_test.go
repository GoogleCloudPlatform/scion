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

package commands

import (
	"errors"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/hub"
)

// stubInitPrivilege makes doctor see p as the init process's privilege for
// the rest of the test.
func stubInitPrivilege(t *testing.T, p initPrivilege) {
	t.Helper()
	orig := readInitPrivilegeFn
	readInitPrivilegeFn = func() (initPrivilege, error) { return p, nil }
	t.Cleanup(func() { readInitPrivilegeFn = orig })
}

const (
	infoNotApplicable = "[INFO] Init credential check not applicable: init (PID 1) runs as a non-root user"
	warnUnavailRoot   = "[WARN] Init credential state unavailable: PID 1 runs with effective uid 0, so init should have published it"
	warnUnavailOld    = "[WARN] Init credential state unavailable (init is not running as root, or could not publish it; see the init log)"
)

func TestParseInitStatusUIDs(t *testing.T) {
	status := func(uid string) []byte {
		return []byte("Name:\tsciontool\nState:\tS (sleeping)\nPid:\t1\n" + uid + "Gid:\t1000\t1000\t1000\t1000\n")
	}
	cases := []struct {
		name string
		data []byte
		want initPrivilege
	}{
		{"all non-zero", status("Uid:\t1000\t1000\t1000\t1000\n"), initPrivNonRoot},
		{"spaces", status("Uid:  1000 1000  1000 1000\n"), initPrivNonRoot},
		{"all zero", status("Uid:\t0\t0\t0\t0\n"), initPrivRoot},
		{"effective 0", status("Uid:\t1000\t0\t1000\t1000\n"), initPrivRoot},
		{"real 0, effective not", status("Uid:\t0\t1000\t1000\t1000\n"), initPrivUnknown},
		{"saved 0, effective not", status("Uid:\t1000\t1000\t0\t1000\n"), initPrivUnknown},
		{"fs 0, effective not", status("Uid:\t1000\t1000\t1000\t0\n"), initPrivUnknown},
		{"no Uid line", status(""), initPrivUnknown},
		{"three fields", status("Uid:\t1000\t1000\t1000\n"), initPrivUnknown},
		{"five fields", status("Uid:\t1000\t1000\t1000\t1000\t1000\n"), initPrivUnknown},
		{"not decimal", status("Uid:\t1000\tx\t1000\t1000\n"), initPrivUnknown},
		{"signed", status("Uid:\t+1000\t1000\t1000\t1000\n"), initPrivUnknown},
		{"two Uid lines", status("Uid:\t1000\t1000\t1000\t1000\nUid:\t0\t0\t0\t0\n"), initPrivUnknown},
		{"empty", nil, initPrivUnknown},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := parseInitStatusUIDs(tc.data); got != tc.want {
				t.Errorf("parseInitStatusUIDs = %d, want %d", got, tc.want)
			}
		})
	}
}

func TestUIDMapIsIdentity(t *testing.T) {
	cases := []struct {
		data string
		want bool
	}{
		{"         0          0 4294967295\n", true},
		{"0\t0\t4294967295", true},
		{"0 1000 1\n", false},
		{"0 0 65536\n", false},
		{"0 0 4294967295\n1 1 1\n", false},
		{"", false},
		{"0 0 4294967295 0\n", false},
		{"0 0 x\n", false},
	}
	for _, tc := range cases {
		if got := uidMapIsIdentity([]byte(tc.data)); got != tc.want {
			t.Errorf("uidMapIsIdentity(%q) = %v, want %v", tc.data, got, tc.want)
		}
	}
}

// Rule 1 with no published state follows the init privilege.
func TestEvaluateInitCredential_UnavailableFollowsInitPrivilege(t *testing.T) {
	cases := []struct {
		name    string
		priv    initPrivilege
		want    string
		notWant []string
	}{
		{"non-root", initPrivNonRoot, infoNotApplicable, []string{"[WARN]", "[FAIL]", "[ OK ]"}},
		{"root", initPrivRoot, warnUnavailRoot, []string{"[INFO]", "[FAIL]", "[ OK ]"}},
		{"unknown", initPrivUnknown, warnUnavailOld, []string{"[INFO]", "[FAIL]", "[ OK ]"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			in := evalCase{status: hub.CredentialStateUnavailable, fileOK: true, file: idC1, lookup: lookupAccepted}.input()
			in.initPriv = tc.priv
			res := evaluateInitCredential(in)
			out := strings.Join(res.lines, "\n")
			if len(res.lines) != 1 || !strings.Contains(out, tc.want) {
				t.Errorf("want exactly one line containing %q, got:\n%s", tc.want, out)
			}
			for _, w := range tc.notWant {
				if strings.Contains(out, w) {
					t.Errorf("unexpected %q in:\n%s", w, out)
				}
			}
			if res.failures != 0 || res.outcome != initOutcomeNone {
				t.Errorf("failures=%d outcome=%d, want 0 and none", res.failures, res.outcome)
			}
		})
	}
}

// An unknown privilege keeps the existing WARN line unchanged and adds
// the reason on its own line.
func TestEvaluateInitCredential_UnknownShowsReason(t *testing.T) {
	in := evalCase{status: hub.CredentialStateUnavailable, fileOK: true, file: idC1, lookup: lookupAccepted}.input()
	in.initPriv = initPrivUnknown
	in.initPrivWhy = "1/exe: not this binary"
	res := evaluateInitCredential(in)
	want := []string{warnUnavailOld, "[INFO] Init privilege not determined: 1/exe: not this binary"}
	if strings.Join(res.lines, "\n") != strings.Join(want, "\n") {
		t.Errorf("lines:\n%s\nwant:\n%s", strings.Join(res.lines, "\n"), strings.Join(want, "\n"))
	}
	if res.failures != 0 || res.outcome != initOutcomeNone {
		t.Errorf("failures=%d outcome=%d, want 0 and none", res.failures, res.outcome)
	}
}

// The reason reaches the output from the privilege read.
func TestCheckInitCredential_ShowsUnknownReason(t *testing.T) {
	c1 := agentJWT(t, "jti-c1", time.Now().Add(time.Hour))
	srv := httptest.NewServer(&doctorHub{accept: map[string]bool{c1: true}})
	defer srv.Close()
	doctorScenario{hubURL: srv.URL, initTok: c1, fileTok: c1, fileAge: time.Hour}.setup(t)
	orig := readInitPrivilegeFn
	readInitPrivilegeFn = func() (initPrivilege, error) {
		return initPrivUnknown, errors.New("statx \"\": the kernel does not report mount IDs")
	}
	t.Cleanup(func() { readInitPrivilegeFn = orig })
	if err := os.Remove(hub.CredentialStatePath()); err != nil {
		t.Fatal(err)
	}
	var diag doctorDiag
	out := captureStdout(t, func() { checkInitCredential(&diag) })
	if !strings.Contains(out, warnUnavailOld) || !strings.Contains(out, "[INFO] Init privilege not determined: statx \"\": the kernel does not report mount IDs") {
		t.Errorf("output:\n%s", out)
	}
}

// The privilege only matters when no state is published: an untrusted
// state stays a WARN even for a non-root init.
func TestEvaluateInitCredential_UntrustedIgnoresInitPrivilege(t *testing.T) {
	in := evalCase{status: hub.CredentialStateUntrusted, fileOK: true, file: idC1, lookup: lookupAccepted}.input()
	in.initPriv = initPrivNonRoot
	out := strings.Join(evaluateInitCredential(in).lines, "\n")
	if !strings.Contains(out, warnUntrust) || strings.Contains(out, "not applicable") {
		t.Errorf("untrusted state with a non-root init:\n%s", out)
	}
}

// Under a non-root init (Kubernetes), doctor reports the init credential
// check as not applicable instead of warning.
func TestRunDoctor_StateUnavailable_NonRootInit(t *testing.T) {
	c1 := agentJWT(t, "jti-c1", time.Now().Add(time.Hour))
	srv := httptest.NewServer(&doctorHub{accept: map[string]bool{c1: true}})
	defer srv.Close()
	doctorScenario{hubURL: srv.URL, initTok: c1, fileTok: c1, fileAge: time.Hour}.setup(t)
	stubInitPrivilege(t, initPrivNonRoot)
	if err := os.Remove(hub.CredentialStatePath()); err != nil {
		t.Fatal(err)
	}
	out, rc := runDoctorCaptured(t)
	if rc != 0 || !strings.Contains(out, infoNotApplicable) || strings.Contains(out, "Init credential state unavailable") {
		t.Errorf("expected the not-applicable INFO and no unavailable WARN (rc=%d):\n%s", rc, out)
	}
}

// The signal is read only when no state is published.
func TestCheckInitCredential_ReadsPrivilegeOnlyWhenUnavailable(t *testing.T) {
	c1 := agentJWT(t, "jti-c1", time.Now().Add(time.Hour))
	srv := httptest.NewServer(&doctorHub{accept: map[string]bool{c1: true}})
	defer srv.Close()
	doctorScenario{hubURL: srv.URL, initTok: c1, fileTok: c1, fileAge: time.Hour}.setup(t)
	calls := 0
	orig := readInitPrivilegeFn
	readInitPrivilegeFn = func() (initPrivilege, error) { calls++; return initPrivNonRoot, nil }
	t.Cleanup(func() { readInitPrivilegeFn = orig })
	var diag doctorDiag
	_ = captureStdout(t, func() { checkInitCredential(&diag) })
	if calls != 0 {
		t.Errorf("privilege read %d times with a published state, want 0", calls)
	}
	if err := os.Remove(hub.CredentialStatePath()); err != nil {
		t.Fatal(err)
	}
	out := captureStdout(t, func() { checkInitCredential(&diag) })
	if calls != 1 || !strings.Contains(out, infoNotApplicable) {
		t.Errorf("calls=%d, want 1; output:\n%s", calls, out)
	}
}
