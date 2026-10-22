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
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/hub"
)

// --- evaluator table ---

var (
	evalNow = time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC)
	idC1    = hub.CredentialIdentity{JTI: "c1", FP: "11111111", IssuedAt: evalNow.Add(-time.Hour), Expiry: evalNow.Add(time.Hour)}
	idC2    = hub.CredentialIdentity{JTI: "c2", FP: "22222222", IssuedAt: evalNow.Add(-time.Minute), Expiry: evalNow.Add(2 * time.Hour)}
	idPrev  = hub.CredentialIdentity{JTI: "c0", FP: "00000000"}
)

func rec(outcome string, status int, id hub.CredentialIdentity, by string) *hub.OutcomeRecord {
	return &hub.OutcomeRecord{At: evalNow.Add(-10 * time.Second), Outcome: outcome, Status: status, FP: id.FP, RefusedBy: by}
}

func hbRefused(id hub.CredentialIdentity, by string) *hub.OutcomeRecord {
	return rec(hub.OutcomeRefused, 401, id, by)
}

// evalCase describes one verdict-table row: the inputs, then the expected
// failures, outcome and lines.
type evalCase struct {
	name    string
	status  string // state read status; "" = ok
	cur     hub.CredentialIdentity
	hb      *hub.OutcomeRecord
	refresh *hub.OutcomeRecord
	check   *hub.OutcomeRecord
	stale   bool
	fileOK  bool
	file    hub.CredentialIdentity
	age     time.Duration
	lookup  lookupVerdict

	fails   int
	outcome initOutcome
	want    []string
	notWant []string
}

func (tc evalCase) input() initAuthInput {
	status := tc.status
	if status == "" {
		status = hub.CredentialStateOK
	}
	updated := evalNow.Add(-5 * time.Second)
	if tc.stale {
		updated = evalNow.Add(-5 * time.Minute)
	}
	return initAuthInput{
		stateStatus: status,
		stateReason: "the file is not owned by root or is writable by others",
		state: hub.CredentialState{
			Version: 1, UpdatedAt: updated, Credential: tc.cur,
			Heartbeat: tc.hb, Refresh: tc.refresh, Check: tc.check,
		},
		fileOK:  tc.fileOK,
		file:    tc.file,
		fileAge: tc.age,
		lookup:  tc.lookup,
		now:     evalNow,
	}
}

const (
	failRefused  = "[FAIL] Init's requests with its credential are refused (401/403)"
	warnSwitch   = "[WARN] Init is switching to the new credential file"
	okNoRefusal  = "[ OK ] No refusal recorded for init's credential"
	okMatch      = "[ OK ] Init credential matches the credential file"
	outdatedWhy  = "the init process holds an outdated credential"
	warnDiffer   = "[WARN] Init and the credential file hold different credentials (init: jti=c1, file: jti=c2)"
	failExpired  = "[FAIL] Init credential expired"
	warnUnavail  = "[WARN] Init credential state unavailable"
	warnUntrust  = "[WARN] Init credential state not trusted"
	warnMissing  = "[WARN] Credential file missing; cannot compare"
	warnSuperset = "[WARN] Init's start-up check of this credential was refused; a lookup with the same credential is now accepted"
)

func TestEvaluateInitCredential_VerdictTable(t *testing.T) {
	age30, age3 := 30*time.Second, 3*time.Second
	expired := idC1
	expired.Expiry = evalNow.Add(-time.Minute)
	noExp := idC1
	noExp.Expiry = time.Time{}

	cases := []evalCase{
		{name: "1 unavailable", status: hub.CredentialStateUnavailable, fileOK: true, file: idC1, lookup: lookupAccepted,
			want: []string{warnUnavail}, notWant: []string{"[ OK ]", "[FAIL]"}},
		{name: "2 untrusted", status: hub.CredentialStateUntrusted, fileOK: true, file: idC1, lookup: lookupAccepted,
			want: []string{warnUntrust}, notWant: []string{"[ OK ]", "[FAIL]"}},
		{name: "3 match no refusal", cur: idC1, hb: rec(hub.OutcomeSent, 200, idC1, ""), fileOK: true, file: idC1, age: age30, lookup: lookupAccepted,
			want: []string{okMatch, okNoRefusal, "Lookup with this credential accepted", "sent (hub answered 200)"}},
		{name: "3 match lookup unchecked", cur: idC1, fileOK: true, file: idC1, lookup: lookupUnchecked,
			want: []string{okMatch, okNoRefusal, "not confirmed"}},
		{name: "4 match lookup hub-refused", cur: idC1, fileOK: true, file: idC1, lookup: lookupHubRefused,
			want: []string{"which the hub refused"}, notWant: []string{okNoRefusal}},
		{name: "5 mismatch outdated no refusal", cur: idC1, fileOK: true, file: idC2, age: age30, lookup: lookupAccepted,
			want: []string{warnDiffer + "; init holds an outdated credential", okNoRefusal}},
		{name: "5b mismatch young no refusal", cur: idC1, fileOK: true, file: idC2, age: age3, lookup: lookupUnchecked,
			want: []string{warnDiffer, okNoRefusal}, notWant: []string{"outdated"}},
		{name: "6 acceptance", cur: idC1, hb: hbRefused(idC1, hub.RefusedByHub), fileOK: true, file: idC2, age: age30, lookup: lookupAccepted,
			fails: 1, outcome: initOutcomeHint, want: []string{failRefused + " by the hub: " + outdatedWhy}},
		{name: "7 window accepted", cur: idC1, hb: hbRefused(idC1, hub.RefusedByHub), fileOK: true, file: idC2, age: age3, lookup: lookupAccepted,
			outcome: initOutcomeQuiet, want: []string{warnSwitch, "written 3s ago"}, notWant: []string{"[FAIL]"}},
		{name: "8 window unchecked", cur: idC1, hb: hbRefused(idC1, hub.RefusedByHub), fileOK: true, file: idC2, age: age3, lookup: lookupUnchecked,
			outcome: initOutcomeQuiet, want: []string{warnSwitch}, notWant: []string{"[FAIL]"}},
		{name: "9 window not run", cur: idC1, hb: hbRefused(idC1, hub.RefusedByHub), fileOK: true, file: idC2, age: age3, lookup: lookupNotRun,
			outcome: initOutcomeQuiet, want: []string{warnSwitch}, notWant: []string{"[FAIL]"}},
		{name: "10 not run outside window", cur: idC1, hb: hbRefused(idC1, hub.RefusedByHub), fileOK: true, file: idC2, age: age30, lookup: lookupNotRun,
			fails: 1, outcome: initOutcomeHint, want: []string{failRefused}},
		{name: "11 young file hub-refused", cur: idC1, hb: hbRefused(idC1, hub.RefusedByHub), fileOK: true, file: idC2, age: age3, lookup: lookupHubRefused,
			fails: 1, outcome: initOutcomeHint, want: []string{failRefused}},
		{name: "12 proxy refusal", cur: idC1, hb: hbRefused(idC1, hub.RefusedByProxy), fileOK: true, file: idC2, age: age30, lookup: lookupAccepted,
			fails: 1, outcome: initOutcomeQuiet, want: []string{failRefused + " by the proxy"}},
		{name: "12 proxy refusal matched", cur: idC1, hb: hbRefused(idC1, hub.RefusedByProxy), fileOK: true, file: idC1, lookup: lookupAccepted,
			fails: 1, outcome: initOutcomeQuiet, want: []string{failRefused + " by the proxy"}},
		{name: "13 check refused, lookup hub-refused", cur: idC1, check: rec(hub.OutcomeRefused, 0, idC1, hub.RefusedByUnknown), fileOK: true, file: idC1, lookup: lookupHubRefused,
			fails: 1, outcome: initOutcomeHint, want: []string{failRefused + " by the hub or proxy"}},
		{name: "14a check refused superseded", cur: idC1, check: rec(hub.OutcomeRefused, 0, idC1, hub.RefusedByUnknown), fileOK: true, file: idC1, lookup: lookupAccepted,
			want: []string{warnSuperset, okNoRefusal, "Init start-up check: refused by the hub or proxy at"}, notWant: []string{"[FAIL]", "refused (0)"}},
		{name: "14b check refused, mismatch", cur: idC1, check: rec(hub.OutcomeRefused, 0, idC1, hub.RefusedByUnknown), fileOK: true, file: idC2, age: age30, lookup: lookupAccepted,
			fails: 1, outcome: initOutcomeQuiet, want: []string{failRefused}},
		{name: "15 check refused, lookup unchecked", cur: idC1, check: rec(hub.OutcomeRefused, 0, idC1, hub.RefusedByUnknown), fileOK: true, file: idC1, lookup: lookupUnchecked,
			fails: 1, outcome: initOutcomeQuiet, want: []string{failRefused}},
		{name: "16 check refused, lookup not run", cur: idC1, check: rec(hub.OutcomeRefused, 0, idC1, hub.RefusedByUnknown), fileOK: true, file: idC1, lookup: lookupNotRun,
			fails: 1, outcome: initOutcomeQuiet, want: []string{failRefused}},
		{name: "17 paused current", cur: idC1, hb: rec(hub.OutcomePaused, 0, idC1, hub.RefusedByHub), fileOK: true, file: idC1, lookup: lookupAccepted,
			fails: 1, outcome: initOutcomeHint, want: []string{failRefused, "paused after the credential was refused"}},
		{name: "17b paused previous", cur: idC1, hb: rec(hub.OutcomePaused, 0, idPrev, hub.RefusedByHub), fileOK: true, file: idC1, lookup: lookupAccepted,
			want: []string{okNoRefusal, "(for a previous credential)"}},
		{name: "18 refusal previous only", cur: idC1, hb: hbRefused(idPrev, hub.RefusedByHub), refresh: rec(hub.OutcomeRefused, 401, idPrev, hub.RefusedByHub), fileOK: true, file: idC1, lookup: lookupAccepted,
			want: []string{okNoRefusal}, notWant: []string{"[FAIL]"}},
		{name: "19 refresh refused", cur: idC1, refresh: rec(hub.OutcomeRefused, 401, idC1, hub.RefusedByHub), fileOK: true, file: idC1, lookup: lookupAccepted,
			fails: 1, outcome: initOutcomeHint, want: []string{failRefused}},
		{name: "19c refreshed shown without previous", cur: idC1, refresh: rec(hub.OutcomeRefreshed, 200, idPrev, ""), fileOK: true, file: idC1, lookup: lookupAccepted,
			want: []string{"Init last refresh: new credential received at", okNoRefusal}, notWant: []string{"previous credential"}},
		{name: "19b refresh failed", cur: idC1, refresh: rec(hub.OutcomeFailed, 502, idC1, ""), fileOK: true, file: idC1, lookup: lookupAccepted,
			want: []string{"[WARN] Init last refresh: failed (status 502)", okNoRefusal}},
		{name: "20a expired", cur: expired, fileOK: true, file: expired, lookup: lookupAccepted,
			fails: 1, outcome: initOutcomeHint, want: []string{failExpired}},
		{name: "20b expired in window", cur: expired, hb: hbRefused(expired, hub.RefusedByHub), fileOK: true, file: idC2, age: age3, lookup: lookupAccepted,
			fails: 1, outcome: initOutcomeQuiet, want: []string{failExpired}},
		{name: "21 stale with refusal", cur: idC1, hb: hbRefused(idC1, hub.RefusedByHub), stale: true, fileOK: true, file: idC1, lookup: lookupAccepted,
			fails: 1, outcome: initOutcomeHint, want: []string{"state last updated 5m0s ago", failRefused}},
		{name: "22 file missing", cur: idC1, lookup: lookupNotRun,
			want: []string{warnMissing, okNoRefusal}},
		{name: "22b file missing with refusal", cur: idC1, hb: hbRefused(idC1, hub.RefusedByHub), lookup: lookupNotRun,
			fails: 1, outcome: initOutcomeHint, want: []string{warnMissing, failRefused}, notWant: []string{outdatedWhy}},
		{name: "23 file time in the future", cur: idC1, hb: hbRefused(idC1, hub.RefusedByHub), fileOK: true, file: idC2, age: -30 * time.Second, lookup: lookupAccepted,
			fails: 1, outcome: initOutcomeHint, want: []string{failRefused}},
		{name: "24 heartbeat redirected to 200", cur: idC1, hb: rec(hub.OutcomeSent, 200, idC1, ""), fileOK: true, file: idC1, lookup: lookupAccepted,
			want: []string{"heartbeat: sent (hub answered 200)"}},
		{name: "25 mixed proxy and unknown", cur: idC1, hb: hbRefused(idC1, hub.RefusedByProxy), check: rec(hub.OutcomeRefused, 0, idC1, hub.RefusedByUnknown), fileOK: true, file: idC2, age: age30, lookup: lookupHubRefused,
			fails: 1, outcome: initOutcomeQuiet, want: []string{failRefused + " by the hub or proxy"}},
		{name: "26 no expiry claim", cur: noExp, fileOK: true, file: noExp, lookup: lookupAccepted,
			want: []string{"no expiry claim", okNoRefusal}, notWant: []string{"[FAIL]"}},
		{name: "27 mismatch hub refusal lookup unchecked", cur: idC1, hb: hbRefused(idC1, hub.RefusedByHub), fileOK: true, file: idC2, age: age30, lookup: lookupUnchecked,
			fails: 1, outcome: initOutcomeHint, want: []string{failRefused}},
		{name: "28 mismatch hub refusal lookup refused", cur: idC1, hb: hbRefused(idC1, hub.RefusedByHub), fileOK: true, file: idC2, age: age30, lookup: lookupHubRefused,
			fails: 1, outcome: initOutcomeHint, want: []string{failRefused}},
		{name: "29 match hub refusal lookup unchecked", cur: idC1, hb: hbRefused(idC1, hub.RefusedByHub), fileOK: true, file: idC1, lookup: lookupUnchecked,
			fails: 1, outcome: initOutcomeHint, want: []string{failRefused}, notWant: []string{outdatedWhy}},
		{name: "30 match hub refusal lookup not run", cur: idC1, hb: hbRefused(idC1, hub.RefusedByHub), fileOK: true, file: idC1, lookup: lookupNotRun,
			fails: 1, outcome: initOutcomeHint, want: []string{failRefused}},
		{name: "31 match hub refusal lookup refused", cur: idC1, hb: hbRefused(idC1, hub.RefusedByHub), fileOK: true, file: idC1, lookup: lookupHubRefused,
			fails: 1, outcome: initOutcomeHint, want: []string{failRefused}},
		{name: "32 paused by proxy", cur: idC1, hb: rec(hub.OutcomePaused, 0, idC1, hub.RefusedByProxy), fileOK: true, file: idC1, lookup: lookupAccepted,
			fails: 1, outcome: initOutcomeQuiet, want: []string{failRefused + " by the proxy"}},
		{name: "33 paused by hub", cur: idC1, hb: rec(hub.OutcomePaused, 0, idC1, hub.RefusedByHub), fileOK: true, file: idC1, lookup: lookupAccepted,
			fails: 1, outcome: initOutcomeHint, want: []string{failRefused + " by the hub"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			res := evaluateInitCredential(tc.input())
			out := strings.Join(res.lines, "\n")
			if res.failures != tc.fails {
				t.Errorf("failures=%d, want %d\n%s", res.failures, tc.fails, out)
			}
			if res.outcome != tc.outcome {
				t.Errorf("outcome=%d, want %d\n%s", res.outcome, tc.outcome, out)
			}
			for _, w := range tc.want {
				if !strings.Contains(out, w) {
					t.Errorf("missing %q in:\n%s", w, out)
				}
			}
			for _, w := range tc.notWant {
				if strings.Contains(out, w) {
					t.Errorf("unexpected %q in:\n%s", w, out)
				}
			}
			if strings.Contains(out, "reset-auth") {
				t.Errorf("the evaluator lines never carry the reset-auth hint:\n%s", out)
			}
		})
	}
}

// Hints follow the outcome: reset-auth only for hint, never for quiet.
func TestInitRemediationAndGCPHint_FollowTheOutcome(t *testing.T) {
	for _, tc := range []struct {
		name      string
		r         initAuthResult
		resetAuth bool
		want      string
	}{
		{"hint mismatch", initAuthResult{outcome: initOutcomeHint, initID: "jti=c1", fileID: "jti=c2"}, true, "holds an outdated credential (jti=c1; the credential file has jti=c2)"},
		{"hint match", initAuthResult{outcome: initOutcomeHint, initID: "jti=c1", fileID: "jti=c1", match: true}, true, "is expired or refused"},
		{"quiet window", initAuthResult{outcome: initOutcomeQuiet, window: true}, false, "See Init Process Credential above"},
		{"quiet proxy", initAuthResult{outcome: initOutcomeQuiet, proxy: true}, false, "See Init Process Credential above"},
		{"quiet unclear", initAuthResult{outcome: initOutcomeQuiet, unclear: true}, false, "See Init Process Credential above"},
		{"none", initAuthResult{}, true, "scion reset-auth <agent-name>"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out := captureStdout(t, func() { printGCPInitHint(tc.r); printInitRemediation(tc.r) })
			if !strings.Contains(out, tc.want) {
				t.Errorf("GCP hint missing %q:\n%s", tc.want, out)
			}
			if strings.Contains(out, "reset-auth") != tc.resetAuth {
				t.Errorf("GCP hint reset-auth=%v, want %v:\n%s", !tc.resetAuth, tc.resetAuth, out)
			}
			if strings.Contains(out, "scion agent") {
				t.Errorf("names a command that does not exist:\n%s", out)
			}
		})
	}
}

func TestClassifyLookup(t *testing.T) {
	mk := func(status int, hdr map[string]string) *http.Response {
		r := &http.Response{StatusCode: status, Header: http.Header{}}
		for k, v := range hdr {
			r.Header.Set(k, v)
		}
		return r
	}
	for _, tc := range []struct {
		name string
		resp *http.Response
		body string
		want lookupVerdict
	}{
		{"own record", mk(200, nil), `{"id":"agent-1"}`, lookupAccepted},
		{"foreign record", mk(200, nil), `{"id":"agent-2"}`, lookupUnchecked},
		{"non-JSON 200", mk(200, nil), `<html>portal</html>`, lookupUnchecked},
		{"hub 401", mk(401, nil), `{"error":{"code":"unauthorized"}}`, lookupHubRefused},
		{"iap 401", mk(401, map[string]string{"X-Goog-IAP-Generated-Response": "true"}), `{"error":"x"}`, lookupUnchecked},
		{"html 403", mk(403, nil), `<html>`, lookupUnchecked},
		{"redirect", mk(302, map[string]string{"Location": "https://example.com"}), ``, lookupUnchecked},
		{"500", mk(500, nil), `{"id":"agent-1"}`, lookupUnchecked},
		{"too large", mk(200, nil), `{"id":"agent-1","pad":"` + strings.Repeat("a", lookupRecordMaxBytes) + `"}`, lookupUnchecked},
		{"no response", nil, ``, lookupUnchecked},
	} {
		if got := classifyLookup(tc.resp, []byte(tc.body), "agent-1"); got != tc.want {
			t.Errorf("%s: got %d, want %d", tc.name, got, tc.want)
		}
	}
}

// --- runDoctor-level tests ---

// agentJWT returns a well-formed HS256-shaped agent credential.
func agentJWT(t *testing.T, jti string, exp time.Time) string {
	t.Helper()
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"HS256","typ":"JWT"}`))
	claimsJSON, _ := json.Marshal(map[string]any{"sub": "agent-1", "jti": jti, "iat": time.Now().Add(-time.Minute).Unix(), "exp": exp.Unix()})
	sig := make([]byte, 32)
	_, _ = rand.Read(sig)
	return header + "." + base64.RawURLEncoding.EncodeToString(claimsJSON) + "." + base64.RawURLEncoding.EncodeToString(sig)
}

// doctorHub is a test hub that accepts only the credentials in accept and
// refuses the rest, as the hub (JSON) or as a proxy (HTML).
type doctorHub struct {
	accept  map[string]bool
	byProxy bool
}

func (h *doctorHub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/healthz" {
		w.WriteHeader(http.StatusOK)
		return
	}
	if !h.accept[r.Header.Get("X-Scion-Agent-Token")] {
		if h.byProxy {
			w.Header().Set("Content-Type", "text/html")
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = fmt.Fprint(w, "<html>Unauthorized</html>")
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = fmt.Fprint(w, `{"error":{"code":"unauthorized","message":"invalid credential"}}`)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = fmt.Fprint(w, `{"id":"agent-1"}`)
}

// metadataStub answers the health check and returns 502 for GCP tokens,
// as the metadata server does when init's credential is refused.
func metadataStub(t *testing.T) int {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/" {
			w.WriteHeader(http.StatusOK)
			return
		}
		http.Error(w, "token generation failed", http.StatusBadGateway)
	}))
	t.Cleanup(srv.Close)
	_, port, _ := net.SplitHostPort(strings.TrimPrefix(srv.URL, "http://"))
	var p int
	_, _ = fmt.Sscanf(port, "%d", &p)
	return p
}

// doctorScenario sets up doctor's environment: a hub URL (may be ""), the
// init process's published state built by a real client holding initTok
// (through the constructor, never SetToken) after one heartbeat against
// hubURL, and the credential file holding fileTok (if not "") written
// fileAge ago.
type doctorScenario struct {
	hubURL   string
	initTok  string
	fileTok  string
	fileAge  time.Duration
	metadata bool
	// checkOnly: init records its start-up check of initTok instead of a
	// heartbeat.
	checkOnly bool
}

func (s doctorScenario) setup(t *testing.T) {
	t.Helper()
	home := isolateDoctorTransport(t)
	for _, k := range []string{"SCION_HUB_URL", "SCION_AGENT_MODE", "SCION_LAUNCH_ID", "SCION_TELEMETRY_ENABLED",
		"SCION_WORKSPACE_GIT", "SCION_HARNESS_PID", hub.EnvGitHubAppEnabled, "SCION_METADATA_PORT"} {
		t.Setenv(k, "")
	}
	t.Setenv("SCION_HUB_ENDPOINT", s.hubURL)
	t.Setenv("SCION_AGENT_ID", "agent-1")
	t.Setenv("SCION_METADATA_MODE", "")
	if s.metadata {
		t.Setenv("SCION_METADATA_MODE", "assign")
		t.Setenv("SCION_METADATA_PORT", fmt.Sprint(metadataStub(t)))
	} else {
		_ = os.Unsetenv("SCION_METADATA_MODE")
	}

	base := t.TempDir()
	if err := os.Mkdir(filepath.Join(base, "run"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(hub.SetCredentialStateDirForTest(filepath.Join(base, "run", "scion"), base))
	initURL := s.hubURL
	if initURL == "" {
		initURL = "http://127.0.0.1:1"
	}
	c := hub.NewClientWithConfig(initURL, s.initTok, "agent-1")
	if s.checkOnly {
		_ = c.CheckCurrentCredentialAccepted(context.Background(), s.initTok)
	} else {
		_ = c.Heartbeat(context.Background())
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	if !hub.PublishCredentialState(ctx, c) {
		t.Fatal("state not published")
	}

	if s.fileTok != "" {
		path := filepath.Join(home, ".scion", "scion-token")
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(s.fileTok), 0o600); err != nil {
			t.Fatal(err)
		}
		when := time.Now().Add(-s.fileAge)
		_ = os.Chtimes(path, when, when)
	}
}

func runDoctorCaptured(t *testing.T) (string, int) {
	t.Helper()
	var rc int
	out := captureStdout(t, func() { rc = runDoctor() })
	return out, rc
}

// Acceptance: init holds C1 since start (constructor, no SetToken), the hub
// refuses C1, and reset-auth wrote C2 (accepted) half a minute ago. Doctor
// FAILs the init credential and says why; the GCP 502 gets the hint.
func TestRunDoctor_OutdatedInitCredentialFails(t *testing.T) {
	c1 := agentJWT(t, "jti-c1", time.Now().Add(time.Hour))
	c2 := agentJWT(t, "jti-c2", time.Now().Add(2*time.Hour))
	srv := httptest.NewServer(&doctorHub{accept: map[string]bool{c2: true}})
	defer srv.Close()
	doctorScenario{hubURL: srv.URL, initTok: c1, fileTok: c2, fileAge: 30 * time.Second, metadata: true}.setup(t)

	out, rc := runDoctorCaptured(t)
	if rc == 0 {
		t.Fatalf("doctor passed in the outdated-credential state:\n%s", out)
	}
	for _, want := range []string{
		"[ OK ] Authenticated successfully (heartbeat accepted)",
		"--- Init Process Credential ---",
		"[INFO] Init credential: jti=jti-c1",
		"Init last heartbeat: refused (401) by the hub",
		"Init and the credential file hold different credentials (init: jti=jti-c1, file: jti=jti-c2); init holds an outdated credential",
		failRefused + " by the hub: " + outdatedWhy,
		"[!] The init process holds an outdated credential (jti=jti-c1; the credential file has jti=jti-c2), so it cannot get GCP credentials from the hub.",
		"scion reset-auth <agent-name>",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
	if strings.Contains(out, "scion agent") {
		t.Errorf("names a command that does not exist:\n%s", out)
	}
	assertNoTokenValues(t, out, c1, c2)
	for _, seg := range append(strings.Split(c1, "."), strings.Split(c2, ".")[1:]...) {
		if len(seg) > 20 && strings.Contains(out, seg) {
			t.Fatalf("output contains a credential segment:\n%s", out)
		}
	}
}

// Row 7 with a GCP 502: init is switching to a file written 3s ago. No
// reset-auth text appears anywhere in the output.
func TestRunDoctor_ReloadWindowNeverSuggestsResetAuth(t *testing.T) {
	c1 := agentJWT(t, "jti-c1", time.Now().Add(time.Hour))
	c2 := agentJWT(t, "jti-c2", time.Now().Add(2*time.Hour))
	srv := httptest.NewServer(&doctorHub{accept: map[string]bool{c2: true}})
	defer srv.Close()
	doctorScenario{hubURL: srv.URL, initTok: c1, fileTok: c2, fileAge: 3 * time.Second, metadata: true}.setup(t)

	out, _ := runDoctorCaptured(t)
	if !strings.Contains(out, warnSwitch) || !strings.Contains(out, "See Init Process Credential above") {
		t.Errorf("expected the switching lines:\n%s", out)
	}
	if strings.Contains(out, "reset-auth") {
		t.Errorf("reset-auth suggested during a reload:\n%s", out)
	}
}

// Row 12 with a GCP 502: a proxy refused init's requests. No reset-auth
// text anywhere; the transport remediation is shown.
func TestRunDoctor_ProxyRefusalNeverSuggestsResetAuth(t *testing.T) {
	c1 := agentJWT(t, "jti-c1", time.Now().Add(time.Hour))
	c2 := agentJWT(t, "jti-c2", time.Now().Add(2*time.Hour))
	h := &doctorHub{accept: map[string]bool{c2: true}, byProxy: true}
	srv := httptest.NewServer(h)
	defer srv.Close()
	doctorScenario{hubURL: srv.URL, initTok: c1, fileTok: c2, fileAge: 30 * time.Second, metadata: true}.setup(t)

	out, rc := runDoctorCaptured(t)
	if rc == 0 {
		t.Errorf("expected a failure:\n%s", out)
	}
	for _, want := range []string{failRefused + " by the proxy", "refused by the platform proxy", "See Init Process Credential above"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
	if strings.Contains(out, "reset-auth") {
		t.Errorf("reset-auth suggested for a proxy refusal:\n%s", out)
	}
}

// Rows 14b-16 with a GCP 502: init's start-up check was refused, by the
// hub or a proxy, and nothing shows it was the hub. No reset-auth text
// appears anywhere.
func TestRunDoctor_UnclearRefusalNeverSuggestsResetAuth(t *testing.T) {
	c1 := agentJWT(t, "jti-c1", time.Now().Add(time.Hour))
	c2 := agentJWT(t, "jti-c2", time.Now().Add(2*time.Hour))
	srv := httptest.NewServer(&doctorHub{accept: map[string]bool{c2: true}})
	defer srv.Close()
	doctorScenario{hubURL: srv.URL, initTok: c1, fileTok: c2, fileAge: 30 * time.Second, metadata: true, checkOnly: true}.setup(t)

	out, rc := runDoctorCaptured(t)
	if rc == 0 {
		t.Errorf("expected a failure:\n%s", out)
	}
	for _, want := range []string{failRefused + " by the hub or proxy", "not clearly by the hub", "See Init Process Credential above"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
	if strings.Contains(out, "reset-auth") {
		t.Errorf("reset-auth suggested for a refusal not known to be by the hub:\n%s", out)
	}
}

// The init block compares the token file as read for the Authentication
// section, not a second read.
func TestCheckInitCredential_UsesTheSingleRead(t *testing.T) {
	c1 := agentJWT(t, "jti-c1", time.Now().Add(time.Hour))
	c2 := agentJWT(t, "jti-c2", time.Now().Add(2*time.Hour))
	srv := httptest.NewServer(&doctorHub{accept: map[string]bool{c1: true}})
	defer srv.Close()
	doctorScenario{hubURL: srv.URL, initTok: c1, fileTok: c1, fileAge: time.Hour}.setup(t)
	// The file on disk holds c1; the read doctor made holds c2.
	diag := doctorDiag{lookup: lookupAccepted, file: doctorTokenFile{read: true, ok: true, token: c2, modTime: time.Now().Add(-time.Hour)}}
	var res initAuthResult
	out := captureStdout(t, func() { res = checkInitCredential(&diag) })
	if res.fileID != "jti=jti-c2" || !strings.Contains(out, "file: jti=jti-c2") {
		t.Errorf("init block did not use doctor's read (fileID %q):\n%s", res.fileID, out)
	}
	diag.file = doctorTokenFile{read: true}
	out = captureStdout(t, func() { res = checkInitCredential(&diag) })
	if !strings.Contains(out, warnMissing) {
		t.Errorf("a missing file in doctor's read must not be re-read:\n%s", out)
	}
}

// checkAuthentication presents the token file as doctor read it once
// (diag.file), not a second read.
func TestCheckAuthentication_UsesTheSingleRead(t *testing.T) {
	c1 := agentJWT(t, "jti-c1", time.Now().Add(time.Hour))
	c2 := agentJWT(t, "jti-c2", time.Now().Add(2*time.Hour))
	var presented []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		presented = append(presented, r.Header.Get("X-Scion-Agent-Token"))
		_, _ = fmt.Fprint(w, `{"id":"agent-1"}`)
	}))
	defer srv.Close()
	home := isolateDoctorTransport(t)
	_ = os.MkdirAll(filepath.Join(home, ".scion"), 0o700)
	_ = os.WriteFile(filepath.Join(home, ".scion", "scion-token"), []byte(c1), 0o600)
	t.Setenv("SCION_AGENT_ID", "agent-1")
	t.Setenv(hub.EnvLaunchID, "")
	failures := 0
	diag := doctorDiag{file: doctorTokenFile{read: true, ok: true, token: c2}}
	_ = captureStdout(t, func() { checkAuthentication(srv.URL, &failures, nil, &diag) })
	if len(presented) != 2 || presented[0] != c2 || presented[1] != c2 {
		t.Fatalf("checkAuthentication presented %d values; want doctor's read twice", len(presented))
	}
	assertNoTokenValues(t, strings.Join(presented, ""), c1)
}

// The init block runs when the hub is unreachable or not configured.
func TestRunDoctor_InitBlockRunsWithoutTheHub(t *testing.T) {
	c1 := agentJWT(t, "jti-c1", time.Now().Add(time.Hour))
	c2 := agentJWT(t, "jti-c2", time.Now().Add(2*time.Hour))
	srv := httptest.NewServer(&doctorHub{accept: map[string]bool{c2: true}})
	url := srv.URL
	defer srv.Close()

	t.Run("unreachable", func(t *testing.T) {
		// The hub refuses C1 while init records it, then goes away.
		doctorScenario{hubURL: url, initTok: c1, fileTok: c2, fileAge: 30 * time.Second}.setup(t)
		t.Setenv("SCION_HUB_ENDPOINT", "http://127.0.0.1:1")
		out, _ := runDoctorCaptured(t)
		if !strings.Contains(out, "--- Init Process Credential ---") || !strings.Contains(out, failRefused) {
			t.Errorf("init block missing or not failing:\n%s", out)
		}
	})
	t.Run("no hub URL", func(t *testing.T) {
		doctorScenario{hubURL: url, initTok: c1, fileTok: c2, fileAge: 3 * time.Second}.setup(t)
		t.Setenv("SCION_HUB_ENDPOINT", "")
		out, _ := runDoctorCaptured(t)
		if !strings.Contains(out, warnSwitch) {
			t.Errorf("expected the switching WARN (lookup not run):\n%s", out)
		}
		if strings.Contains(out, "reset-auth") {
			t.Errorf("reset-auth suggested during a reload:\n%s", out)
		}
	})
}

// A missing credential file with a refused init credential: two failures,
// one from the Authentication section and one from the init block.
func TestRunDoctor_MissingFileWithRefusal(t *testing.T) {
	c1 := agentJWT(t, "jti-c1", time.Now().Add(time.Hour))
	srv := httptest.NewServer(&doctorHub{accept: map[string]bool{}})
	defer srv.Close()
	doctorScenario{hubURL: srv.URL, initTok: c1}.setup(t)

	out, _ := runDoctorCaptured(t)
	for _, want := range []string{warnMissing, failRefused, "[RESULT] 2 check(s) FAILED"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
}

// A healthy agent: init holds the file's credential and the hub accepts it.
func TestRunDoctor_HealthyInitCredential(t *testing.T) {
	c1 := agentJWT(t, "jti-c1", time.Now().Add(time.Hour))
	srv := httptest.NewServer(&doctorHub{accept: map[string]bool{c1: true}})
	defer srv.Close()
	doctorScenario{hubURL: srv.URL, initTok: c1, fileTok: c1, fileAge: time.Hour}.setup(t)

	out, rc := runDoctorCaptured(t)
	if rc != 0 {
		t.Errorf("healthy agent failed:\n%s", out)
	}
	for _, want := range []string{okMatch, "Lookup with this credential accepted", okNoRefusal} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
	if strings.Contains(out, "--- Remediation ---") {
		t.Errorf("unexpected remediation:\n%s", out)
	}
}

// With no published state (init not root), doctor WARNs and does not fail.
func TestRunDoctor_StateUnavailable(t *testing.T) {
	c1 := agentJWT(t, "jti-c1", time.Now().Add(time.Hour))
	srv := httptest.NewServer(&doctorHub{accept: map[string]bool{c1: true}})
	defer srv.Close()
	doctorScenario{hubURL: srv.URL, initTok: c1, fileTok: c1, fileAge: time.Hour}.setup(t)
	if err := os.Remove(hub.CredentialStatePath()); err != nil {
		t.Fatal(err)
	}
	out, rc := runDoctorCaptured(t)
	if rc != 0 || !strings.Contains(out, warnUnavail) {
		t.Errorf("expected a WARN only (rc=%d):\n%s", rc, out)
	}
}

// Doctor's lookup sends the run id the same way init does.
func TestCheckAuthentication_SendsRunID(t *testing.T) {
	c1 := agentJWT(t, "jti-c1", time.Now().Add(time.Hour))
	var gotRunID string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			gotRunID = r.Header.Get(hub.RunIDHeader)
		}
		_, _ = fmt.Fprint(w, `{"id":"agent-1"}`)
	}))
	defer srv.Close()
	home := isolateDoctorTransport(t)
	_ = os.MkdirAll(filepath.Join(home, ".scion"), 0o700)
	_ = os.WriteFile(filepath.Join(home, ".scion", "scion-token"), []byte(c1), 0o600)
	t.Setenv("SCION_AGENT_ID", "agent-1")
	t.Setenv(hub.EnvLaunchID, "run-123")
	failures := 0
	var diag doctorDiag
	_ = captureStdout(t, func() { checkAuthentication(srv.URL, &failures, nil, &diag) })
	if gotRunID != "run-123" {
		t.Errorf("run id header %q, want run-123", gotRunID)
	}
	if diag.lookup != lookupAccepted {
		t.Errorf("lookup=%d, want accepted", diag.lookup)
	}
}
