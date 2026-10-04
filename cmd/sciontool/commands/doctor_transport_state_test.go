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
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/hub"
	"github.com/GoogleCloudPlatform/scion/pkg/transportauth"
)

// isolateDoctorTransport points HOME and the token home at a temp dir and
// clears every transport-related variable. It returns the temp home.
func isolateDoctorTransport(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Cleanup(hub.SetTokenHome(home))
	for _, k := range []string{
		transportauth.EnvTransportToken,
		transportauth.EnvTransportTokenFile,
		transportauth.EnvTransportMode,
		transportauth.EnvTransportAudience,
		transportauth.EnvHubOIDCAudience,
	} {
		t.Setenv(k, "")
	}
	orig := transportauth.IsOnGCEFunc
	transportauth.IsOnGCEFunc = func() bool { return false }
	t.Cleanup(func() { transportauth.IsOnGCEFunc = orig })
	return home
}

func writeDoctorTransportFile(t *testing.T, home, tok string) string {
	t.Helper()
	path := filepath.Join(home, ".scion", transportauth.TransportTokenFileName)
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(tok), 0600); err != nil {
		t.Fatal(err)
	}
	return path
}

func runCheckTransportAuth(t *testing.T) (string, doctorDiag) {
	t.Helper()
	var diag doctorDiag
	out := captureStdout(t, func() { checkTransportAuth(&diag) })
	return out, diag
}

func assertNoTokenValues(t *testing.T, out string, toks ...string) {
	t.Helper()
	for _, tok := range toks {
		if tok != "" && strings.Contains(out, tok) {
			t.Fatalf("doctor output must never contain a token value:\n%s", out)
		}
	}
}

// Valid: the refreshed file is in use and current; the expired bootstrap
// value is shown alongside it, expiry only.
func TestCheckTransportAuth_ValidFileInUse(t *testing.T) {
	home := isolateDoctorTransport(t)
	envTok := makeDoctorTestJWT(time.Now().Add(-10 * time.Minute))
	fileTok := makeDoctorTestJWT(time.Now().Add(50 * time.Minute))
	path := writeDoctorTransportFile(t, home, fileTok)
	t.Setenv(transportauth.EnvTransportToken, envTok)
	t.Setenv(transportauth.EnvTransportMode, "iap")
	t.Setenv(transportauth.EnvTransportAudience, "test-audience.example")

	out, diag := runCheckTransportAuth(t)

	if diag.transportFailed() {
		t.Fatalf("expected no transport failure, diag=%+v\n%s", diag, out)
	}
	for _, want := range []string{
		"Mode: iap (header: Proxy-Authorization)",
		"Audience: test-a...xample (from SCION_TRANSPORT_AUDIENCE; shortened)",
		"[ OK ] Transport credential in use: refreshed file " + path,
		"env  (SCION_TRANSPORT_TOKEN): expired",
		"file (" + path + "): expires",
		"last written",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}
	assertNoTokenValues(t, out, envTok, fileTok)
	if strings.Contains(out, "test-audience.example") {
		t.Errorf("doctor output should not contain the full audience:\n%s", out)
	}
}

// Expired: only the expired bootstrap value is available. Doctor FAILs.
func TestCheckTransportAuth_ExpiredFails(t *testing.T) {
	isolateDoctorTransport(t)
	envTok := makeDoctorTestJWT(time.Now().Add(-20 * time.Minute))
	t.Setenv(transportauth.EnvTransportToken, envTok)

	out, diag := runCheckTransportAuth(t)

	if !diag.transportExpired || !diag.transportFailed() {
		t.Fatalf("expected an expired transport credential, diag=%+v\n%s", diag, out)
	}
	if !strings.Contains(out, "[FAIL] Transport credential in use: bootstrap value from SCION_TRANSPORT_TOKEN, EXPIRED") {
		t.Errorf("expected FAIL for expired credential:\n%s", out)
	}
	if !strings.Contains(out, "Mode: default (header: Authorization)") {
		t.Errorf("expected default mode line:\n%s", out)
	}
	if !strings.Contains(out, "): not present") {
		t.Errorf("expected file shown as not present:\n%s", out)
	}
	assertNoTokenValues(t, out, envTok)
}

// Missing: the file variable is set (as sciontool init sets it for its
// children) but the file is gone and there is no env value.
func TestCheckTransportAuth_MissingFails(t *testing.T) {
	home := isolateDoctorTransport(t)
	t.Setenv(transportauth.EnvTransportTokenFile, filepath.Join(home, ".scion", transportauth.TransportTokenFileName))

	out, diag := runCheckTransportAuth(t)

	if !diag.transportMissing || !diag.transportFailed() {
		t.Fatalf("expected a missing transport credential, diag=%+v\n%s", diag, out)
	}
	if !strings.Contains(out, "[FAIL] Transport credential: none available") {
		t.Errorf("expected FAIL for missing credential:\n%s", out)
	}
}

func TestCheckTransportAuth_ReportsLastRefreshOutcome(t *testing.T) {
	cases := []struct {
		status      hub.TransportRefreshStatus
		want        string
		wantProblem bool
	}{
		{hub.TransportRefreshStatus{At: time.Now(), Outcome: hub.TransportRefreshOutcomeRefreshed}, "[ OK ] Last refresh: new transport token received", false},
		{hub.TransportRefreshStatus{At: time.Now(), Outcome: hub.TransportRefreshOutcomeFailed, Error: "mint failed for test"}, "transport token not renewed: mint failed for test", true},
		{hub.TransportRefreshStatus{At: time.Now(), Outcome: hub.TransportRefreshOutcomeAbsent}, "the hub returned no transport token", true},
		{hub.TransportRefreshStatus{At: time.Now(), Outcome: hub.TransportRefreshOutcomeReset}, "[ OK ] Last update: fresh transport token installed by reset-auth", false},
	}
	for _, tc := range cases {
		t.Run(tc.status.Outcome, func(t *testing.T) {
			home := isolateDoctorTransport(t)
			writeDoctorTransportFile(t, home, makeDoctorTestJWT(time.Now().Add(time.Hour)))
			t.Setenv(transportauth.EnvTransportTokenFile, filepath.Join(home, ".scion", transportauth.TransportTokenFileName))
			if err := hub.WriteTransportRefreshStatus(tc.status, 0, 0); err != nil {
				t.Fatal(err)
			}
			out, diag := runCheckTransportAuth(t)
			if !strings.Contains(out, tc.want) {
				t.Errorf("output missing %q:\n%s", tc.want, out)
			}
			if (diag.transportRefreshProblem != "") != tc.wantProblem {
				t.Errorf("transportRefreshProblem=%q, wantProblem=%v", diag.transportRefreshProblem, tc.wantProblem)
			}
		})
	}
}

func TestClassifyRejection(t *testing.T) {
	mk := func(code int, hdr map[string]string) *http.Response {
		r := &http.Response{StatusCode: code, Header: http.Header{}}
		for k, v := range hdr {
			r.Header.Set(k, v)
		}
		return r
	}
	cases := []struct {
		name string
		resp *http.Response
		body string
		want string
	}{
		{"hub json 401", mk(401, nil), `{"error":{"code":"unauthorized","message":"invalid token"}}`, rejectedByHub},
		{"iap header", mk(401, map[string]string{"X-Goog-IAP-Generated-Response": "true"}), `Invalid IAP credentials`, rejectedByProxy},
		{"html 403", mk(403, nil), `<html>Your client does not have permission</html>`, rejectedByProxy},
		{"sign-in redirect", mk(302, map[string]string{"Location": "https://accounts.google.com/o/oauth2/v2/auth"}), ``, rejectedByProxy},
		{"other redirect", mk(302, map[string]string{"Location": "/elsewhere"}), ``, rejectedByNone},
		{"ok", mk(200, nil), `{}`, rejectedByNone},
		{"500", mk(500, nil), `oops`, rejectedByNone},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := classifyRejection(tc.resp, []byte(tc.body)); got != tc.want {
				t.Errorf("classifyRejection = %q, want %q", got, tc.want)
			}
		})
	}
}

// A proxy 401 is told apart from a hub 401, and remediation is tailored.
func TestCheckAuthentication_ProxyVersusHubRejection(t *testing.T) {
	cases := []struct {
		name        string
		handler     http.HandlerFunc
		wantBy      string
		wantRemedy  string
		avoidRemedy string
	}{
		{
			name: "proxy",
			handler: func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("X-Goog-IAP-Generated-Response", "true")
				w.WriteHeader(http.StatusUnauthorized)
				_, _ = fmt.Fprint(w, "Invalid IAP credentials: expired")
			},
			wantBy:      rejectedByProxy,
			wantRemedy:  "platform proxy (IAP / Cloud Run invoker) rejected the transport credential",
			avoidRemedy: "signing key",
		},
		{
			name: "hub",
			handler: func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusUnauthorized)
				_, _ = fmt.Fprint(w, `{"error":{"code":"unauthorized","message":"invalid agent token"}}`)
			},
			wantBy:      rejectedByHub,
			wantRemedy:  "signing key",
			avoidRemedy: "platform proxy",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			home := isolateDoctorTransport(t)
			if err := os.MkdirAll(filepath.Join(home, ".scion"), 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(home, ".scion", "scion-token"), []byte("test-app-value"), 0600); err != nil {
				t.Fatal(err)
			}
			t.Setenv("SCION_AGENT_ID", "test-agent")
			srv := httptest.NewServer(tc.handler)
			defer srv.Close()

			var diag doctorDiag
			failures := 0
			out := captureStdout(t, func() {
				if checkAuthentication(srv.URL, &failures, nil, &diag) {
					t.Error("expected authentication to fail")
				}
				printRemediation(time.Now().Add(time.Hour), "agent", false, diag)
			})
			if diag.authRejectedBy != tc.wantBy {
				t.Errorf("authRejectedBy=%q, want %q", diag.authRejectedBy, tc.wantBy)
			}
			if failures == 0 {
				t.Error("expected failures to be counted")
			}
			if !strings.Contains(out, tc.wantRemedy) {
				t.Errorf("remediation missing %q:\n%s", tc.wantRemedy, out)
			}
			if strings.Contains(out, tc.avoidRemedy) {
				t.Errorf("remediation should not mention %q:\n%s", tc.avoidRemedy, out)
			}
		})
	}
}

func TestCheckHubConnectivity_Healthz404Note(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	defer srv.Close()
	out := captureStdout(t, func() {
		if !checkHubConnectivity(srv.URL, nil) {
			t.Error("a 404 should still count as reachable")
		}
	})
	if !strings.Contains(out, "Some platforms reserve /healthz") {
		t.Errorf("expected reserved-path note:\n%s", out)
	}
}

func TestPrintRemediation_ExpiredTransport(t *testing.T) {
	diag := doctorDiag{transportConfigured: true, transportExpired: true, transportRefreshProblem: "mint failed for test"}
	out := captureStdout(t, func() {
		printRemediation(time.Now().Add(time.Hour), "agent", true, diag)
	})
	for _, want := range []string{
		"transport credential in use has expired",
		"The last refresh did not renew it: mint failed for test",
		"scion agent reset-auth <agent-name>  (also pushes a fresh transport token)",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("remediation missing %q:\n%s", want, out)
		}
	}
}

// Malformed: the file in use cannot be parsed (the bootstrap value is gone,
// as in agent children). Its expiry is unknown, so doctor FAILs.
func TestCheckTransportAuth_MalformedFileFails(t *testing.T) {
	home := isolateDoctorTransport(t)
	path := writeDoctorTransportFile(t, home, "not-a-jwt")
	t.Setenv(transportauth.EnvTransportTokenFile, path)

	out, diag := runCheckTransportAuth(t)

	if !diag.transportUnparseable || !diag.transportFailed() {
		t.Fatalf("expected an unparseable transport credential, diag=%+v\n%s", diag, out)
	}
	if !strings.Contains(out, "[FAIL] Transport credential in use: refreshed file "+path+" could not be parsed; expiry unknown") {
		t.Errorf("expected FAIL for malformed credential:\n%s", out)
	}
	if strings.Contains(out, "[ OK ] Transport credential") {
		t.Errorf("malformed credential must not be reported OK:\n%s", out)
	}
	rem := captureStdout(t, func() { printRemediation(time.Now().Add(time.Hour), "agent", true, diag) })
	for _, want := range []string{"could not be parsed", "scion agent reset-auth"} {
		if !strings.Contains(rem, want) {
			t.Errorf("remediation missing %q:\n%s", want, rem)
		}
	}
	assertNoTokenValues(t, out, "not-a-jwt")
}

// Near expiry: within the refresh margin the credential is WARN, never OK.
func TestCheckTransportAuth_NearExpiryWarns(t *testing.T) {
	home := isolateDoctorTransport(t)
	tok := makeDoctorTestJWT(time.Now().Add(2 * time.Minute))
	path := writeDoctorTransportFile(t, home, tok)
	t.Setenv(transportauth.EnvTransportTokenFile, path)

	out, diag := runCheckTransportAuth(t)

	if diag.transportFailed() {
		t.Fatalf("near expiry is not a failure, diag=%+v\n%s", diag, out)
	}
	if !strings.Contains(out, "[WARN] Transport credential in use: refreshed file "+path) ||
		!strings.Contains(out, "within refresh margin") {
		t.Errorf("expected WARN within refresh margin:\n%s", out)
	}
	if strings.Contains(out, "[ OK ] Transport credential") {
		t.Errorf("near-expiry credential must not be reported OK:\n%s", out)
	}
	assertNoTokenValues(t, out, tok)
}

// A redirect that is not a proxy sign-in page is never reported as a
// successful authentication, and only scheme://host is printed.
func TestCheckAuthentication_RedirectNotOK(t *testing.T) {
	home := isolateDoctorTransport(t)
	if err := os.MkdirAll(filepath.Join(home, ".scion"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, ".scion", "scion-token"), []byte("test-app-value"), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("SCION_AGENT_ID", "test-agent")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "https://other.example/login?state=opaque-test-state", http.StatusFound)
	}))
	defer srv.Close()

	var diag doctorDiag
	failures := 0
	out := captureStdout(t, func() {
		if checkAuthentication(srv.URL, &failures, nil, &diag) {
			t.Error("a redirect must not count as authenticated")
		}
		if !checkHubConnectivity(srv.URL, nil) {
			t.Error("a redirect still means the endpoint answered")
		}
		printRemediation(time.Now().Add(time.Hour), "agent", false, diag)
	})
	if diag.authRedirectedTo != "https://other.example" {
		t.Errorf("authRedirectedTo=%q", diag.authRedirectedTo)
	}
	// Unconfirmed authentication counts as failed, so the run cannot end
	// with "All checks passed".
	if failures != 2 {
		t.Errorf("failures=%d, want 2 (heartbeat and agent lookup unconfirmed)", failures)
	}
	if strings.Contains(out, "[ OK ]") {
		t.Errorf("redirect reported as OK:\n%s", out)
	}
	for _, want := range []string{
		"[FAIL] Heartbeat not confirmed: hub answered 302, redirected to https://other.example",
		"[FAIL] Agent lookup not confirmed: hub answered 302, redirected to https://other.example",
		"/healthz: redirected to https://other.example",
		"authentication could not be confirmed",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}
	for _, avoid := range []string{"opaque-test-state", "signing key"} {
		if strings.Contains(out, avoid) {
			t.Errorf("output should not contain %q:\n%s", avoid, out)
		}
	}
}

func TestShortenAudience(t *testing.T) {
	cases := map[string]string{
		"":           "",
		"abc":        "***",
		"abcdefghij": "abc...hij",
		"123456789012-abcdefghijklmnop.apps.googleusercontent.com": "123456...nt.com",
	}
	for in, want := range cases {
		if got := shortenAudience(in); got != want {
			t.Errorf("shortenAudience(%q) = %q, want %q", in, got, want)
		}
	}
}

// setupDoctorAuth writes an agent token and agent ID for checkAuthentication.
func setupDoctorAuth(t *testing.T) {
	t.Helper()
	home := isolateDoctorTransport(t)
	if err := os.MkdirAll(filepath.Join(home, ".scion"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, ".scion", "scion-token"), []byte("test-app-value"), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("SCION_AGENT_ID", "test-agent")
}

// A proxy sign-in redirect is reported as a proxy rejection showing only
// scheme://host: neither its body nor its Location query (client ID,
// state) is printed.
func TestCheckAuthentication_ProxyRedirectHidesQuery(t *testing.T) {
	setupDoctorAuth(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "https://accounts.google.com/o/oauth2/v2/auth?client_id=opaque-client-id&state=opaque-state", http.StatusFound)
	}))
	defer srv.Close()

	var diag doctorDiag
	failures := 0
	out := captureStdout(t, func() {
		if checkAuthentication(srv.URL, &failures, nil, &diag) {
			t.Error("a sign-in redirect must not count as authenticated")
		}
	})
	if diag.authRejectedBy != rejectedByProxy {
		t.Errorf("authRejectedBy=%q, want proxy", diag.authRejectedBy)
	}
	if !strings.Contains(out, "redirected to https://accounts.google.com") {
		t.Errorf("expected scheme://host of the redirect:\n%s", out)
	}
	for _, avoid := range []string{"opaque-client-id", "opaque-state", "client_id", "<a href"} {
		if strings.Contains(out, avoid) {
			t.Errorf("output should not contain %q:\n%s", avoid, out)
		}
	}
}

// A Location header that cannot be parsed is reported with a fixed
// message, not Go's error (which quotes the raw header).
func TestCheckAuthentication_UnparseableLocationFixedMessage(t *testing.T) {
	setupDoctorAuth(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Location", "https://other.example/%zz?client_id=opaque-client-id")
		w.WriteHeader(http.StatusFound)
	}))
	defer srv.Close()

	var diag doctorDiag
	failures := 0
	out := captureStdout(t, func() {
		if checkAuthentication(srv.URL, &failures, nil, &diag) {
			t.Error("expected authentication to fail")
		}
	})
	if failures == 0 {
		t.Error("expected a failure")
	}
	if !strings.Contains(out, "Location header could not be parsed") {
		t.Errorf("expected the fixed message:\n%s", out)
	}
	if strings.Contains(out, "opaque-client-id") || strings.Contains(out, "%zz") {
		t.Errorf("raw Location printed:\n%s", out)
	}
}

// Proxy mode, no transport token received yet (dispatch-time mint failed):
// doctor reports the missing credential instead of "none".
func TestCheckTransportAuth_ProxyModeNoneReceivedFails(t *testing.T) {
	home := isolateDoctorTransport(t)
	t.Setenv(transportauth.EnvTransportMode, "iap")
	// Doctor run with a different HOME (e.g. exec'd as root) still names
	// the scion user's file.
	t.Setenv("HOME", t.TempDir())
	scionPath := filepath.Join(home, ".scion", transportauth.TransportTokenFileName)

	out, diag := runCheckTransportAuth(t)

	if !diag.transportMissing || !diag.transportConfigured {
		t.Fatalf("expected a missing transport credential, diag=%+v\n%s", diag, out)
	}
	for _, want := range []string{
		"Transport Auth: hub-provided token (awaiting first token)",
		"Mode: iap (header: Proxy-Authorization)",
		"[FAIL] Transport credential: none received yet",
		"no file at " + scionPath + ")",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "Transport Auth: none") {
		t.Errorf("proxy mode must not be reported as no transport auth:\n%s", out)
	}
}

// Proxy mode, the token arrived later through the file: doctor shows the
// file-backed source in effect with its expiry, never the value.
func TestCheckTransportAuth_ProxyModeLateFileInUse(t *testing.T) {
	home := isolateDoctorTransport(t)
	t.Setenv(transportauth.EnvTransportMode, "iap")
	fileTok := makeDoctorTestJWT(time.Now().Add(50 * time.Minute))
	path := writeDoctorTransportFile(t, home, fileTok)

	out, diag := runCheckTransportAuth(t)

	if diag.transportFailed() {
		t.Fatalf("expected no transport failure, diag=%+v\n%s", diag, out)
	}
	for _, want := range []string{
		"Transport Auth: hub-provided token",
		"[ OK ] Transport credential in use: refreshed file " + path,
		"env  (SCION_TRANSPORT_TOKEN): not set in this process",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}
	assertNoTokenValues(t, out, fileTok)
}

// Proxy mode, the token arrived later in the scion user's file, and doctor
// runs with a different HOME (e.g. exec'd as root): the file is reported
// through the file-backed source, not as missing.
func TestCheckTransportAuth_ProxyModeLateFileOtherHome(t *testing.T) {
	home := isolateDoctorTransport(t)
	t.Setenv(transportauth.EnvTransportMode, "iap")
	fileTok := makeDoctorTestJWT(time.Now().Add(50 * time.Minute))
	path := writeDoctorTransportFile(t, home, fileTok)
	t.Setenv("HOME", t.TempDir())

	out, diag := runCheckTransportAuth(t)

	if diag.transportFailed() || diag.transportMissing {
		t.Fatalf("expected no transport failure, diag=%+v\n%s", diag, out)
	}
	if !diag.transportConfigured {
		t.Fatalf("expected transport configured, diag=%+v\n%s", diag, out)
	}
	for _, want := range []string{
		"Transport Auth: hub-provided token",
		"[ OK ] Transport credential in use: refreshed file " + path,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}
	for _, bad := range []string{"none received yet", "awaiting first token"} {
		if strings.Contains(out, bad) {
			t.Errorf("output must not contain %q:\n%s", bad, out)
		}
	}
	assertNoTokenValues(t, out, fileTok)
}

// Without a proxy mode nothing changes: no transport auth is reported.
func TestCheckTransportAuth_NoProxyModeNone(t *testing.T) {
	home := isolateDoctorTransport(t)
	writeDoctorTransportFile(t, home, makeDoctorTestJWT(time.Now().Add(50*time.Minute)))

	out, diag := runCheckTransportAuth(t)
	if diag.transportConfigured || diag.transportFailed() {
		t.Fatalf("expected no transport auth, diag=%+v\n%s", diag, out)
	}
	if !strings.Contains(out, "[INFO] Transport Auth: none") {
		t.Errorf("expected none:\n%s", out)
	}
}
