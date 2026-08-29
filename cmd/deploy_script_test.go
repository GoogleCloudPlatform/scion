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

package cmd

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ---------------------------------------------------------------------------
// Helpers: invoke bash functions from deploy.sh in a subprocess
// ---------------------------------------------------------------------------

// deployScriptPath returns the absolute path to scripts/single-node/deploy.sh.
func deployScriptPath(t *testing.T) string {
	t.Helper()
	return filepath.Join(repoRoot(t), "scripts", "single-node", "deploy.sh")
}

// scrubbedEnv returns the caller's environment with deploy.sh's test-only
// seams removed. Without this, a developer with _DI_API_BASE exported in their
// shell would silently redirect the script under test, and any assertion about
// a DEFAULT endpoint would be defeatable by an ambient variable. Tests that
// want a seam set do it explicitly, as a shell assignment in the setup prelude.
func scrubbedEnv() []string {
	var out []string
	for _, kv := range os.Environ() {
		if strings.HasPrefix(kv, "_DI_") {
			continue
		}
		out = append(out, kv)
	}
	return out
}

// shellQuote renders s as a single POSIX shell word.
//
// Go's %q is GO quoting, not SHELL quoting, and the two disagree on exactly
// the inputs this file cares about: %q turns a real tab into the two
// characters `\t`, which bash inside double quotes then hands to the function
// as a literal backslash-t. The validator table feeds control characters in on
// purpose — with %q those rows would silently be testing a backslash and would
// pass for the wrong reason, which is the same class of defect as the m4 false
// pin. Single quotes pass every byte through unchanged.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// testBash names the interpreter every harness in this file runs the script
// under. It exists because of a real escape: five review rounds, 42 tests, 62
// shellcheck files and a live deploy all passed on Linux bash 5, and the script
// still died on line 286 of a stock Mac, which ships bash 3.2.57. Nothing here
// could see that, because the interpreter was hardcoded.
//
// Point SCION_TEST_BASH at another bash and the WHOLE existing suite runs under
// it — that is a gate that executes rather than reads. shellcheck cannot do this
// job: it has no bash-version targeting.
//
// Know the limit before trusting it: "bad substitution" is a RUNTIME error, so
// running under an old bash only catches lines the suite actually EXECUTES.
// See TestScriptLowercasingIsReachedByTheSuite, which pins that coverage
// directly and does not need an old bash to do it.
func testBash() string {
	if b := os.Getenv("SCION_TEST_BASH"); b != "" {
		return b
	}
	return "bash"
}

// runBashFunc sources deploy.sh and calls the named function with args.
// Returns stdout, stderr, and the exit code.
func runBashFunc(t *testing.T, funcName string, args ...string) (string, string, int) {
	t.Helper()
	scriptPath := deployScriptPath(t)

	// Build a bash command that matches production: di_main runs
	// "set -euo pipefail", so every function it calls inherits those
	// options. Without this, tests are structurally blind to failures
	// caused by set -e killing the script before its own error handling.
	bashCmd := fmt.Sprintf("set -euo pipefail; source %q && %s", scriptPath, funcName)
	for _, a := range args {
		bashCmd += " " + shellQuote(a)
	}

	cmd := exec.Command(testBash(), "-c", bashCmd)
	cmd.Env = scrubbedEnv()
	var stdout, stderr strings.Builder
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	err := cmd.Run()
	exitCode := 0
	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			exitCode = exitErr.ExitCode()
		} else {
			t.Fatalf("failed to run bash function %s: %v", funcName, err)
		}
	}

	return stdout.String(), stderr.String(), exitCode
}

// ---------------------------------------------------------------------------
// Helpers: invoke bash functions with custom setup (e.g. mock gcloud)
// ---------------------------------------------------------------------------

// runBashFuncWithSetup is like runBashFunc but injects setup commands
// between sourcing deploy.sh and calling the function. This allows
// mocking gcloud or setting environment variables for testing.
func runBashFuncWithSetup(t *testing.T, setup, funcName string, args ...string) (string, string, int) {
	t.Helper()
	scriptPath := deployScriptPath(t)

	bashCmd := fmt.Sprintf("set -euo pipefail; source %q; %s; %s", scriptPath, setup, funcName)
	for _, a := range args {
		bashCmd += " " + shellQuote(a)
	}

	cmd := exec.Command(testBash(), "-c", bashCmd)
	cmd.Env = scrubbedEnv()
	var stdout, stderr strings.Builder
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	err := cmd.Run()
	exitCode := 0
	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			exitCode = exitErr.ExitCode()
		} else {
			t.Fatalf("failed to run bash function %s: %v", funcName, err)
		}
	}

	return stdout.String(), stderr.String(), exitCode
}

// ---------------------------------------------------------------------------
// Preflight: ADC credential check tests
//
// Two rules hold for every test in this section, and both were learned the
// hard way in review r1:
//
//  1. HERMETIC. Every test points BOTH _DI_TOKENINFO_URL and _DI_API_BASE at a
//     local stub. An earlier version of TestScriptPreflightFailsWithoutADC set
//     neither, so it minted a real 1024-character access token, sent it to the
//     real oauth2.googleapis.com/tokeninfo in a query string, called the real
//     Cloud Run API — and then passed anyway, because the generic remedy string
//     happened to appear in stderr. A unit test must never put a live
//     cloud-platform credential on the network.
//
//  2. THE gcloud STUB ANSWERS ONLY THE ADC FORM, AND RECORDS ITS ARGV. Mocks
//     of the shape `gcloud() { echo "ya29.fake"; }` answer any invocation, so
//     they cannot tell `gcloud auth application-default print-access-token`
//     (correct) from `gcloud auth print-access-token` (the bug being fixed).
//     All four original tests passed against the buggy source. Recording argv
//     and asserting on it is what pins the token source.
// ---------------------------------------------------------------------------

// gcloudArgvLog returns a path in the test's temp dir for the gcloud stub to
// record its argv to, one invocation per line.
func gcloudArgvLog(t *testing.T) string {
	t.Helper()
	return filepath.Join(t.TempDir(), "gcloud-argv.log")
}

// readGcloudArgvLog reads the argv recorded by a gcloud stub. It fails the
// test if the stub was never invoked at all.
func readGcloudArgvLog(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	require.NoError(t, err, "the gcloud stub recorded nothing — gcloud was never called")
	return string(data)
}

// adcGcloudStub builds a bash gcloud() mock that records every invocation to
// argvLog and answers ONLY `gcloud auth application-default print-access-token`.
// Any other invocation is an error, which is what makes tests using this stub
// fail if deploy.sh reverts to the non-ADC credential store.
func adcGcloudStub(argvLog string) string {
	return fmt.Sprintf(`gcloud() {
  printf '%%s\n' "$*" >> %q
  if [[ "$*" == "auth application-default print-access-token" ]]; then
    printf '%%s\n' "ya29.fake-test-token"
    return 0
  fi
  echo "test stub: unexpected gcloud invocation: gcloud $*" >&2
  return 1
}`, argvLog)
}

// brokenADCGcloudStub is adcGcloudStub with the ADC store unavailable: it
// records argv, then fails whatever it is asked for. It simulates a machine
// where `gcloud auth application-default login` has never been run.
func brokenADCGcloudStub(argvLog string) string {
	return fmt.Sprintf(`gcloud() {
  printf '%%s\n' "$*" >> %q
  if [[ "$*" == "auth application-default print-access-token" ]]; then
    echo "ERROR: Application Default Credentials are not available." >&2
    return 1
  fi
  echo "test stub: unexpected gcloud invocation: gcloud $*" >&2
  return 1
}`, argvLog)
}

// newPreflightStub serves both endpoints the preflight talks to: tokeninfo
// (identified by the /tokeninfo path or access_token query parameter) and the
// Cloud Run v2 instances API. The returned counter records how many requests
// arrived, so a test can assert that nothing was sent at all.
//
// The path check covers the POST+body form (token in -d @file, not in the URL);
// the query-param check covers the legacy GET+query form for backward compat.
func newPreflightStub(t *testing.T, tokeninfoJSON string, apiStatus int, apiBody string) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var hits atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		if r.URL.Path == "/tokeninfo" || r.URL.Query().Get("access_token") != "" {
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, tokeninfoJSON)
			return
		}
		w.WriteHeader(apiStatus)
		_, _ = io.WriteString(w, apiBody)
	}))
	t.Cleanup(server.Close)
	return server, &hits
}

// stubTokeninfoURL gives the stub's tokeninfo endpoint a path of its own. Both
// seams point at the same server, so without a distinct path an assertion
// about the tokeninfo URL would also be satisfied by the API URL — which
// shares the host and port. Mutation-testing caught exactly that: deleting the
// tokeninfo echo left the assertion passing on the "GET <api url>" line.
func stubTokeninfoURL(serverURL string) string {
	return serverURL + "/tokeninfo"
}

// preflightSetup composes the bash prelude for a di_main-level test: the
// gcloud stub, plus both test-only URL seams set so di_main resolves them.
// Only di_main reads these variables; every function below it takes the
// endpoints as parameters. These two assignments are therefore the only place
// the environment seam itself is exercised, which is deliberate.
//
// shellQuote, not %q, for the same reason as the argv channel — and here the
// stakes are higher. %q into a bash DOUBLE-QUOTED context does not merely lose
// a tab: `$(...)` and backticks are EXECUTED while the prelude is being set
// up. A hostile row added to prove the validator rejects it would run instead,
// and then pass. See TestScriptHostileOverrideValuesArriveAsDataNotCode.
func preflightSetup(gcloudStub, serverURL string) string {
	return seamSetup(gcloudStub, serverURL, stubTokeninfoURL(serverURL))
}

// seamSetup emits a bash prelude that sets both seams to exactly the bytes
// given, executing nothing.
func seamSetup(gcloudStub, apiBase, tokeninfoURL string) string {
	return fmt.Sprintf("%s\n_DI_API_BASE=%s\n_DI_TOKENINFO_URL=%s",
		gcloudStub, shellQuote(apiBase), shellQuote(tokeninfoURL))
}

// preflightArgs builds the argument list for a DIRECT call to
// di_preflight_rest_credential. The API base and tokeninfo URL are its last
// two parameters; the function reads no environment at all.
func preflightArgs(gcloudAccount, serverURL string) []string {
	return []string{
		gcloudAccount, "us-east4", "test-project",
		serverURL, stubTokeninfoURL(serverURL),
	}
}

func TestScriptPreflightFailsWithoutADC(t *testing.T) {
	// The stub server must never be touched: with no token there is nothing
	// to validate, so the preflight has to abort before any request goes out.
	server, hits := newPreflightStub(t, `{}`, http.StatusOK, `{}`)
	argvLog := gcloudArgvLog(t)

	_, stderr, exitCode := runBashFuncWithSetup(t,
		brokenADCGcloudStub(argvLog),
		"di_preflight_rest_credential",
		preflightArgs("user@example.com", server.URL)...)

	assert.NotEqual(t, 0, exitCode, "must fail when ADC is unavailable")
	assert.Contains(t, stderr, "gcloud auth application-default login",
		"error must name the exact remedy: gcloud auth application-default login")
	assert.Contains(t, readGcloudArgvLog(t, argvLog), "auth application-default print-access-token",
		"the token must be minted from Application Default Credentials — "+
			"`gcloud auth print-access-token` reads a different credential store "+
			"and returns a token type the Cloud Run v2 API rejects "+
			"(ACCESS_TOKEN_TYPE_UNSUPPORTED)")
	assert.Equal(t, int32(0), hits.Load(),
		"nothing may be sent over the wire when no token could be minted")
}

func TestScriptPreflightAbortsOnNon2xxGet(t *testing.T) {
	// tokeninfo answers; the instances API rejects with 403.
	server, _ := newPreflightStub(t,
		`{"email":"user@example.com","email_verified":"true"}`,
		http.StatusForbidden,
		`{"error":{"code":403,"message":"permission denied"}}`)
	argvLog := gcloudArgvLog(t)

	_, stderr, exitCode := runBashFuncWithSetup(t,
		adcGcloudStub(argvLog),
		"di_preflight_rest_credential",
		preflightArgs("user@example.com", server.URL)...)

	assert.NotEqual(t, 0, exitCode,
		"must fail when validating GET returns non-2xx — this abort prevents "+
			"step 3a from creating an Instance that step 3b cannot configure")
	assert.Contains(t, stderr, "403",
		"error must include the HTTP status code")
	assert.Contains(t, stderr, "run.instances.list",
		"a 403 means the credential is valid but unauthorized — the message must "+
			"name the missing permission, not just tell the operator to re-login")
	assert.Contains(t, stderr, "gcloud auth application-default login",
		"error must name the remedy")
	assert.Contains(t, readGcloudArgvLog(t, argvLog), "auth application-default print-access-token",
		"the validated token must come from Application Default Credentials")
}

func TestScriptPreflightWarnsOnIdentityMismatch(t *testing.T) {
	// tokeninfo reports a different email than the active gcloud account.
	server, _ := newPreflightStub(t,
		`{"email":"adc-user@example.com","email_verified":"true"}`,
		http.StatusOK, `{}`)
	argvLog := gcloudArgvLog(t)

	_, stderr, exitCode := runBashFuncWithSetup(t,
		adcGcloudStub(argvLog),
		"di_preflight_rest_credential",
		preflightArgs("gcloud-user@example.com", server.URL)...)

	assert.Equal(t, 0, exitCode,
		"identity mismatch is a warning, not a failure — a deliberate mismatch "+
			"is legitimate; stderr: %s", stderr)
	assert.Contains(t, stderr, "WARNING",
		"must emit a warning")
	assert.Contains(t, stderr, "gcloud-user@example.com",
		"warning must name the gcloud account")
	assert.Contains(t, stderr, "adc-user@example.com",
		"warning must name the ADC identity")
	assert.Contains(t, readGcloudArgvLog(t, argvLog), "auth application-default print-access-token",
		"the compared identity must be the ADC identity")
}

// TestScriptPreflightSkipsComparisonWhenTokeninfoOmitsEmail pins the fix for
// the false positive found in review r1. A service-account ADC token scoped to
// cloud-platform gets a tokeninfo response with azp/aud/scope and NO email.
// azp is a numeric client ID, so comparing it against the gcloud account's
// email address can never match — the warning fired on every single
// service-account run (metadata server, GCE, Cloud Shell, CI), which is alarm
// fatigue on the exact signal the warning exists to carry.
func TestScriptPreflightSkipsComparisonWhenTokeninfoOmitsEmail(t *testing.T) {
	// Measured response shape from a real service-account ADC token.
	server, _ := newPreflightStub(t,
		`{"azp":"110532853671892060667","aud":"110532853671892060667",`+
			`"scope":"https://www.googleapis.com/auth/cloud-platform","access_type":"online"}`,
		http.StatusOK, `{}`)
	argvLog := gcloudArgvLog(t)

	stdout, stderr, exitCode := runBashFuncWithSetup(t,
		adcGcloudStub(argvLog),
		"di_preflight_rest_credential",
		preflightArgs("operator@example.com", server.URL)...)

	assert.Equal(t, 0, exitCode, "must succeed; stderr: %s", stderr)
	assert.NotContains(t, stderr, "WARNING",
		"must NOT warn: a numeric client ID can never equal an email address, so "+
			"comparing them is a guaranteed false positive on every service-account ADC")
	assert.Contains(t, stdout, "110532853671892060667",
		"the client ID is still worth reporting")
	assert.Contains(t, stdout, "skipped",
		"the operator must be told the comparison was skipped, not left to infer "+
			"a mismatch from two values that were never comparable")
}

func TestScriptPreflightSucceedsWithMatchingIdentity(t *testing.T) {
	server, _ := newPreflightStub(t,
		`{"email":"user@example.com","email_verified":"true"}`,
		http.StatusOK, `{}`)
	argvLog := gcloudArgvLog(t)

	stdout, stderr, exitCode := runBashFuncWithSetup(t,
		adcGcloudStub(argvLog),
		"di_preflight_rest_credential",
		preflightArgs("user@example.com", server.URL)...)

	assert.Equal(t, 0, exitCode, "must succeed when all checks pass; stderr: %s", stderr)
	assert.NotContains(t, stderr, "WARNING",
		"must NOT warn when identities match")
	assert.Contains(t, stdout, "ADC credential validated successfully",
		"must confirm successful validation")
	assert.Contains(t, stdout, stubTokeninfoURL(server.URL),
		"the tokeninfo URL must be echoed so a redirected endpoint is never "+
			"invisible in the output")
	assert.Contains(t, readGcloudArgvLog(t, argvLog), "auth application-default print-access-token",
		"the token must be minted from Application Default Credentials")
}

// ---------------------------------------------------------------------------
// The host rule for the test-only URL seams
//
// di_validate_override_url is the newest and most security-sensitive function
// in this script, and until review r2 it had NO direct test — it was reached
// only through the preflight, with exactly one rejected input per seam. That
// is precisely why a shallow bypass survived a mutation battery that caught
// eight other defects: mutation testing proves the tests you have can detect
// the defects you thought of, and says nothing about a function no test
// addresses. Coverage of the caller is not coverage of the rule.
//
// So the rule gets a table, and the table is where new cases go.
// ---------------------------------------------------------------------------

// TestScriptValidateOverrideURL is the direct, table-driven test of the host
// rule. The rejected rows are an evasion suite, not a formality: the `?` and
// `#` rows are the live bypass found in review r2, where
// `https://evil.example?.googleapis.com` passed the check and curl then
// delivered `Authorization: Bearer <ADC token>` to evil.example, because curl
// connects to the host before the `?`.
//
// Three later classes are pinned here too, each of which the host allowlist
// alone does NOT catch:
//   - a PERMITTED host with a path and a trailing `&z=`, which retargets the
//     step 3b PATCH at another project's Instance (review r3);
//   - strings that end in a permitted suffix but are not hostnames at all
//     (whitespace, `%2f`, a non-numeric port) — rejected today only by curl's
//     parser, which is not where the rule is supposed to live;
//   - non-http(s) schemes, safe today only because curl sends no
//     Authorization header on them.
func TestScriptValidateOverrideURL(t *testing.T) {
	allowed := []struct{ name, url string }{
		{"regional Cloud Run endpoint (the real default)", "https://us-east4-run.googleapis.com"},
		{"tokeninfo endpoint (the real default)", "https://oauth2.googleapis.com/tokeninfo"},
		{"loopback stub", "http://127.0.0.1:45607"},
		{"loopback stub with a path", "http://127.0.0.1:45607/tokeninfo"},
		{"localhost", "http://localhost:8080"},
		{"IPv6 loopback with port and path", "http://[::1]:9000/x"},
		// DO NOT DELETE THE NEXT TWO ROWS AS REDUNDANT. They look like
		// nice-to-haves and they are not: they are the only two rows in this
		// table that still detect a regression of the host EXTRACTION to the
		// old path-strip-only form. Measured — with `host="${host%%/*}"`
		// restored, every reject row keeps its correct verdict, because the
		// '?'/'#' guard and the host-shape assertion added later catch that
		// class first. These two go red (uppercase is no longer folded; the
		// userinfo is no longer stripped, so `a@b@…` fails the shape check).
		// Delete them and the extraction has no pin at all.
		{"uppercase host — hostnames are case-insensitive", "https://FOO.GOOGLEAPIS.COM"},
		// Also the one input where this rule is deliberately MORE permissive
		// than curl: curl refuses to parse a double-`@` authority at all,
		// while the host after the last `@` really is permitted. Pinned so the
		// intent is recorded rather than rediscovered (review r3, Nit 3).
		{"double userinfo — host after the LAST @ is permitted", "https://a@b@oauth2.googleapis.com"},
	}
	for _, tc := range allowed {
		t.Run("allow/"+tc.name, func(t *testing.T) {
			_, stderr, exitCode := runBashFunc(t, "di_validate_override_url", "_DI_API_BASE", tc.url)
			assert.Equal(t, 0, exitCode,
				"%q is a legitimate value and must be accepted; stderr: %s", tc.url, stderr)
		})
	}

	rejected := []struct{ name, url string }{
		// The r2 bypass. curl connects to the host before the '?' or '#'.
		{"query suffix (r2 bypass)", "https://evil.example?.googleapis.com"},
		{"fragment suffix (r2 bypass)", "https://evil.example#.googleapis.com"},
		{"query suffix after a port", "https://evil.tld:8080?.googleapis.com"},
		{"query parameter suffix", "https://evil.tld?x=.googleapis.com"},
		{"backslash suffix", `https://evil.example\.googleapis.com`},
		// Prefix/suffix confusion.
		{"hyphen instead of dot", "https://evil-googleapis.com"},
		{"googleapis.com as a subdomain label", "https://googleapis.com.evil.tld"},
		{"path suffix", "https://evil.tld/x.googleapis.com"},
		{"loopback as a subdomain label", "https://127.0.0.1.evil.tld"},
		// Userinfo: curl resolves the host after the LAST '@'.
		{"userinfo", "https://x@evil.tld"},
		{"permitted host as userinfo", "https://foo.googleapis.com@evil.tld"},
		{"userinfo with a colon and a port", "https://user:pass@evil.tld:8080"},
		// Fail-closed, documented in review r2 Nit 5.
		{"trailing-dot FQDN (fail-closed, known)", "https://oauth2.googleapis.com."},
		{"plain attacker host", "https://evil.example"},
		// A host allowlist is not a URL allowlist. These two rows are caught
		// ONLY by the '?'/'#' guard — the host really is permitted. The first
		// is the measured payload from review r3: the trailing `&z=` swallows
		// the path di_build_iap_patch_url appends, so the PATCH lands on
		// another project's Instance with the operator's live token and a
		// valid updateMask, and the operator's own Instance keeps IAP off.
		{"permitted host retargeting the PATCH via a query",
			"https://us-east4-run.googleapis.com/v2/projects/victim/locations/us-east4/instances/victim?updateMask=iapEnabled&z="},
		{"permitted host with a fragment", "https://oauth2.googleapis.com/tokeninfo#x"},
		// Not hostnames at all. Each ends in a permitted suffix and passes the
		// allowlist; only the positive host-shape assertion rejects them. curl
		// refuses all ten (exit 3, code 000) — that is the point: the rule must
		// not depend on the client for its own postcondition.
		{"space in the host", "https://evil.example .googleapis.com"},
		{"tab in the host", "https://evil.example\t.googleapis.com"},
		{"newline in the host", "https://evil.example\n.googleapis.com"},
		{"carriage return in the host", "https://evil.example\r.googleapis.com"},
		{"percent-encoded slash in the host", "https://evil.example%2f.googleapis.com"},
		{"percent-encoded hash in the host", "https://evil.example%23.googleapis.com"},
		{"percent-encoded question mark in the host", "https://evil.example%3f.googleapis.com"},
		{"semicolon in the host", "https://evil.example;.googleapis.com"},
		{"comma in the host", "https://evil.example,.googleapis.com"},
		{"non-numeric port", "https://evil.example:8x.googleapis.com"},
		// Scheme. Only http(s) can carry the Bearer header, but the rule says
		// so itself rather than leaning on curl to decline.
		{"non-http scheme on a permitted host", "dict://x.googleapis.com"},
		{"file scheme", "file:///etc/passwd"},
		{"no scheme", "evil.example"},
		{"scheme-relative", "//evil.example"},
		// TRAILING WHITESPACE. THESE ROWS EXIST TO STOP THE LOWERCASING BEING
		// "SIMPLIFIED". deploy.sh lowercases with
		//     v="$(printf '%s' "$v" | tr '[:upper:]' '[:lower:]'; printf x)"
		//     v="${v%x}"
		// and the `; printf x` / `%x` look like noise someone can remove. They
		// cannot: command substitution strips ALL trailing newlines, and the
		// lowercasing runs BEFORE the host-shape assertion that rejects these.
		//
		// Measured, per site, against the pre-macOS-fix original:
		//   plain form at the HOST site   — 3 of these flip REJECT -> ALLOW
		//   plain form at the SCHEME site — 2 of these flip REJECT -> ALLOW
		//   sentinel form at both         — byte-identical on all 48 inputs
		// Without these rows the whole table stays GREEN on the plain form, so
		// a portability edit would silently reopen the class R2 closed. Note
		// \r rows are NOT redundant with \n: substitution strips only newlines,
		// so the CR rows pin that the fix did not over-reach either.
		{"trailing newline on a permitted host", "https://oauth2.googleapis.com\n"},
		{"trailing newlines on a permitted host", "https://oauth2.googleapis.com\n\n\n"},
		{"trailing newline on an uppercase permitted host", "https://OAUTH2.GOOGLEAPIS.COM\n"},
		{"trailing newline inside the scheme", "https\n://oauth2.googleapis.com"},
		{"trailing newline inside an uppercase scheme", "HTTPS\n://oauth2.googleapis.com"},
		{"trailing carriage return on a permitted host", "https://oauth2.googleapis.com\r"},
	}
	for _, tc := range rejected {
		t.Run("reject/"+tc.name, func(t *testing.T) {
			_, stderr, exitCode := runBashFunc(t, "di_validate_override_url", "_DI_API_BASE", tc.url)
			require.NotEqual(t, 0, exitCode,
				"%q must be REJECTED. An accepted value here means the ADC token is "+
					"delivered to that host — as a Bearer header on the API base, and "+
					"in a query string on tokeninfo, where the receiver logs it.", tc.url)
			assert.Contains(t, stderr, "_DI_API_BASE",
				"the rejection must name the variable at fault")
		})
	}
}

// TestScriptLowercasingIsReachedByTheSuite proves the suite EXECUTES both
// lowercasing sites, which is the precondition for any old-bash CI job being
// worth anything.
//
// Why this exists. deploy.sh used ${v,,} at two sites. That is bash 4.0+, and
// macOS ships 3.2.57, so the documented one-command deploy died on line 286 of
// a stock Mac — after five review rounds, 42 tests and a live deploy, all on
// Linux bash 5. The fix is to run the suite under an old bash (SCION_TEST_BASH).
// But "bad substitution" is a RUNTIME error, so that job only catches lines the
// suite actually runs. A job that executes neither line would pass forever and
// look like protection.
//
// The instrument. ${v@Z} is NOT a simulation of bash 3.2 — it is an invalid
// parameter transformation that fails in the SAME CLASS at the SAME MOMENT as
// ${v,,} on 3.2: bash -n parses it clean, and it dies at expansion time with
// "bad substitution". That is exactly and only what the coverage question needs,
// and it needs no old bash, which matters because bash 3.2 cannot be built in
// every environment (this one has no route to the source).
func TestScriptLowercasingIsReachedByTheSuite(t *testing.T) {
	original, err := os.ReadFile(deployScriptPath(t))
	require.NoError(t, err)

	sites := []struct {
		name    string
		real    string
		poisonY string
	}{
		{
			name:    "scheme site",
			real:    "  scheme_lc=\"$(printf '%s' \"$scheme\" | tr '[:upper:]' '[:lower:]'; printf x)\"",
			poisonY: "  scheme_lc=\"${scheme@Z}\"",
		},
		{
			name:    "host site",
			real:    "  host=\"$(printf '%s' \"$host\" | tr '[:upper:]' '[:lower:]'; printf x)\"",
			poisonY: "  host=\"${host@Z}\"",
		},
	}

	// probe poisons one line, runs the validator against a plain permitted URL,
	// and reports whether bash complained.
	probe := func(t *testing.T, real, poison, url string) string {
		t.Helper()
		poisoned := strings.Replace(string(original), real, poison, 1)
		require.NotEqual(t, string(original), poisoned,
			"the poison did not apply — this test cannot observe anything and would "+
				"pass vacuously. The target line was reformatted; update it to match "+
				"deploy.sh. Target was:\n%s", real)

		path := filepath.Join(t.TempDir(), "deploy.sh")
		require.NoError(t, os.WriteFile(path, []byte(poisoned), 0o700))

		bashCmd := fmt.Sprintf(
			"set -euo pipefail; source %q && di_validate_override_url _DI_API_BASE %s",
			path, shellQuote(url))
		cmd := exec.Command(testBash(), "-c", bashCmd)
		cmd.Env = scrubbedEnv()
		var stderr strings.Builder
		cmd.Stderr = &stderr
		_ = cmd.Run()
		return stderr.String()
	}

	// NEGATIVE CONTROL, AND IT IS NOT CEREMONY. Everything below assumes ${v@Z}
	// is a RUNTIME error, so it fires only when its line executes. That is
	// measured on bash 5 — but this test also runs under SCION_TEST_BASH, and on
	// an interpreter where ${v@Z} were a PARSE error instead, every subtest below
	// would pass without executing anything and the coverage gate would be a
	// decoration. So: poison a line that a clean URL never reaches, and require
	// silence. If this fires, the instrument is measuring presence, not
	// execution, and the results below mean nothing.
	t.Run("negative control — the instrument reports execution, not presence", func(t *testing.T) {
		unreachable := `    echo "Error: $var_name must not contain '?' or '#'; it is an endpoint, not a query." >&2`
		quiet := probe(t, unreachable, `    echo "${var_name@Z}" >&2`, "https://oauth2.googleapis.com")
		require.NotContains(t, quiet, "bad substitution",
			"poisoning an UNREACHED line produced an error, so this instrument fires on "+
				"presence rather than execution and cannot prove coverage of anything")

		// ...and the same poisoned line must fire when a URL does reach it.
		loud := probe(t, unreachable, `    echo "${var_name@Z}" >&2`, "https://oauth2.googleapis.com/x?y")
		require.Contains(t, loud, "bad substitution",
			"the instrument failed to fire on a line that IS executed, so a silent "+
				"result below would be meaningless")
	})

	for _, site := range sites {
		t.Run(site.name, func(t *testing.T) {
			// A plain, permitted value: it must reach BOTH sites.
			stderr := probe(t, site.real, site.poisonY, "https://oauth2.googleapis.com")
			assert.Contains(t, stderr, "bad substitution",
				"the suite must EXECUTE this lowercasing line, or an old-bash CI job "+
					"cannot detect a bash-4-ism here and would be green for no reason")
		})
	}
}

// TestScriptHostileOverrideValuesArriveAsDataNotCode pins the property that
// every other test in this file quietly assumes: an override value reaches
// di_validate_override_url as the bytes the test wrote, and is never executed
// on the way there.
//
// The table above exists to feed hostile strings to the validator. Both routes
// from Go to bash used fmt.Sprintf("%q"), which is Go quoting, not shell
// quoting. Interpolated into a bash double-quoted context, `$(...)` and
// backticks RUN. A row added to prove the validator rejects a command
// substitution would instead execute it during setup, then observe the
// resulting empty-ish string being rejected, and report a pass. The row would
// be evidence of nothing.
//
// So the assertion is not "the value was rejected" — that stays true either
// way, which is exactly why the defect is invisible. It is "a sentinel the
// substitution would have created does NOT exist". The sentinel lives in this
// test's own t.TempDir(), so nothing else in the suite can create it, remove
// it, or tidy it away between the run and the assertion; a shared path would
// hand back a green that means nothing, which is the m5/m8 weak-pin shape.
//
// Both channels are covered because both were defective and they fail
// independently: the argv channel (runBashFunc's arguments) and the seam
// channel (the _DI_* assignments in a setup prelude).
func TestScriptHostileOverrideValuesArriveAsDataNotCode(t *testing.T) {
	// hostileURL returns a URL whose execution is observable: if any layer
	// between Go and the validator evaluates it, the sentinel appears.
	hostileURL := func(sentinel string) string {
		return "https://$(touch " + sentinel + ").googleapis.com"
	}

	t.Run("argv channel", func(t *testing.T) {
		sentinel := filepath.Join(t.TempDir(), "argv-channel-executed")

		_, stderr, exitCode := runBashFunc(t, "di_validate_override_url",
			"_DI_API_BASE", hostileURL(sentinel))

		require.NotEqual(t, 0, exitCode, "the value must be rejected; stderr: %s", stderr)
		assert.NoFileExists(t, sentinel,
			"the override value was EXECUTED on its way to the validator. The "+
				"rejection above is worthless: it judged whatever the shell "+
				"produced, not the string this test wrote.")
	})

	t.Run("seam assignment channel", func(t *testing.T) {
		sentinel := filepath.Join(t.TempDir(), "seam-channel-executed")
		argvLog := gcloudArgvLog(t)
		setup := seamSetup(fullGcloudStub(argvLog),
			hostileURL(sentinel),
			"https://oauth2.googleapis.com/tokeninfo") // permitted; never reached

		_, stderr, exitCode := runBashFuncWithSetup(t, setup, "di_main",
			"--name", "test-name", "--project", "test-project",
			"--image", "ghcr.io/example/scion-omni:latest", "--region", "us-east4")

		require.NotEqual(t, 0, exitCode, "the value must be rejected; stderr: %s", stderr)
		assert.NoFileExists(t, sentinel,
			"the seam value was EXECUTED while the prelude was being set up, "+
				"before di_main ever ran. Every di_main-level pin that sets a "+
				"seam is asserting against a string the shell rewrote.")
		assert.NoFileExists(t, argvLog,
			"and the rejection must still happen before any side effect")
	})
}

// fullGcloudStub records argv and answers every gcloud call di_main makes up
// to and including the ADC mint, then refuses the deploy. The seam-rejection
// tests below use it deliberately: with a stub that fails earlier — at the SDK
// capability probe, say — deleting the host check would still turn those tests
// red, but for a reason that has nothing to do with the rule. This stub makes
// the mutation signal mean what it says: if validation is missing, di_main
// reaches the mint and the argv log exists.
func fullGcloudStub(argvLog string) string {
	return fmt.Sprintf(`gcloud() {
  printf '%%s\n' "$*" >> %q
  case "$*" in
    "beta run instances --help")                   return 0 ;;
    "config get account")                          printf '%%s\n' "operator@example.com" ;;
    "projects describe "*)                         printf '%%s\n' "123456789" ;;
    "auth application-default print-access-token") printf '%%s\n' "ya29.fake-test-token" ;;
    "beta run instances deploy "*)
      echo "test stub: refusing to really deploy" >&2
      return 1 ;;
    *)
      echo "test stub: unexpected gcloud invocation: gcloud $*" >&2
      return 1 ;;
  esac
}`, argvLog)
}

// TestScriptRejectsNonGoogleTokeninfoHost pins the _DI_TOKENINFO_URL seam at
// the di_main level. The token reaches tokeninfo as a URL query parameter,
// where the receiving host logs it, and the script is documented as curl-able
// — so `_DI_TOKENINFO_URL=https://evil.example bash <(curl ...)` is a
// plausible copy-paste accident that exfiltrates a live cloud-platform
// credential.
//
// This runs di_main, not the preflight, because di_main is now the only
// reader of the variable. That makes the assertion stronger than it was: the
// override is rejected before ANY side effect, so the gcloud stub records
// nothing at all — not even the SDK capability probe that runs first.
func TestScriptRejectsNonGoogleTokeninfoHost(t *testing.T) {
	argvLog := gcloudArgvLog(t)
	setup := seamSetup(fullGcloudStub(argvLog),
		"http://127.0.0.1:1", // permitted; never reached
		"https://evil.example/tokeninfo")

	_, stderr, exitCode := runBashFuncWithSetup(t, setup, "di_main",
		"--name", "test-name", "--project", "test-project",
		"--image", "ghcr.io/example/scion-omni:latest", "--region", "us-east4")

	// The exit code is the PREMISE, not a signal: a script that reached the
	// network and failed there also exits non-zero. require, so the assertions
	// that do discriminate are read against a run that actually failed.
	require.NotEqual(t, 0, exitCode,
		"must refuse to send an access token to a host outside googleapis.com")
	// The host must be named BY THE REJECTION, not merely echoed somewhere in
	// stderr — see the sibling test for why the bare host name carries no
	// signal on its own.
	assert.Contains(t, stderr, "refusing to send an access token to host 'evil.example'",
		"the rejection must name the offending host")
	assert.Contains(t, stderr, "_DI_TOKENINFO_URL",
		"the rejection must name the variable at fault")
	assert.NoFileExists(t, argvLog,
		"the check must run before ANY side effect — no gcloud call, no token, "+
			"nothing to leak and no Instance to strand")
}

// TestScriptRejectsNonGoogleAPIBase is the same pin on the other seam.
// _DI_API_BASE was originally unrestricted on the grounds that it only
// redirected a Bearer header on a read. That premise died when the seam was
// extended to the step 3b PATCH: a redirected base does not merely make the
// preflight lie, it no-ops the security-critical mutation and leaves a created
// Instance with IAP off. Both seams carry the token; both live under one rule.
func TestScriptRejectsNonGoogleAPIBase(t *testing.T) {
	argvLog := gcloudArgvLog(t)
	setup := seamSetup(fullGcloudStub(argvLog),
		"https://evil.example",
		"https://oauth2.googleapis.com/tokeninfo") // permitted; never reached

	_, stderr, exitCode := runBashFuncWithSetup(t, setup, "di_main",
		"--name", "test-name", "--project", "test-project",
		"--image", "ghcr.io/example/scion-omni:latest", "--region", "us-east4")

	// PREMISE, not signal. With the check deleted this run still exits
	// non-zero — di_main reaches the network and the connection to
	// evil.example fails — so a bare NotEqual cannot tell the two apart. It is
	// require rather than assert to say so in the code: it guards the
	// assertions below, it does not add to their count.
	require.NotEqual(t, 0, exitCode,
		"must refuse to send an access token to a host outside googleapis.com")
	// Assert the REJECTION MESSAGE, not the host name. `Contains(stderr,
	// "evil.example")` passes with the check deleted, because curl's
	// connection-failure message echoes the URL — review r3 measured it. Same
	// class as the weak stub this file already fixed: red for the wrong
	// reason is not signal, and an assertion that cannot fail inflates the
	// count of assertions that can.
	assert.Contains(t, stderr, "refusing to send an access token to host 'evil.example'",
		"the rejection must name the offending host")
	assert.Contains(t, stderr, "_DI_API_BASE",
		"the rejection must name the variable at fault — with two seams under "+
			"one rule, a message that does not say which one is a scavenger hunt")
	assert.NoFileExists(t, argvLog,
		"the check must run before ANY side effect — no gcloud call, no token, "+
			"and no Instance that step 3b could not then configure")
}

// TestScriptSeamsAreReadInExactlyOnePlace pins the half of the invariant that
// hoisting the resolution into di_main was supposed to buy, and which hoisting
// alone does NOT enforce: "nothing reads a seam without having been validated."
//
// Passing the endpoints as parameters makes an unvalidated value visible at
// the call site rather than invisible in the environment — but a future
// function could still add its own ${_DI_API_BASE:-...} read and silently
// reacquire the at-a-distance problem that review r2 flagged. Nothing else
// would fail. So the count is pinned directly.
//
// It is pinned by counting the BARE NAME in comment-stripped text, not by
// matching a read syntax. Review r3 probed the syntax version and walked past
// it five ways — indirection, `printenv`, `env | grep`, a computed name, and a
// `[[ -v VAR ]]` guard were all invisible to it — while an innocent comment
// containing `${_DI_API_BASE:-...}` turned it red for nothing, and a pin that
// fails on unrelated edits is a pin that gets deleted. Counting the name
// catches six of those seven and additionally goes red if the validation call
// is deleted, because that call is one of the two expected mentions. Only a
// deliberately string-concatenated name escapes, and a refactor guard does not
// need to stop an adversary.
//
// Expect exactly two mentions per seam: the `${SEAM:-default}` read inside its
// resolver, and the string literal naming it in the di_validate_override_url
// call in di_main.
func TestScriptSeamsAreReadInExactlyOnePlace(t *testing.T) {
	script := readDeployScript(t)
	code := regexp.MustCompile(`(?m)^\s*#.*$`).ReplaceAllString(script, "")

	for _, seam := range []struct{ variable, resolver string }{
		{"_DI_API_BASE", "di_resolve_api_base"},
		{"_DI_TOKENINFO_URL", "di_resolve_tokeninfo_url"},
	} {
		t.Run(seam.variable, func(t *testing.T) {
			mentions := regexp.MustCompile(`\b`+seam.variable+`\b`).FindAllString(code, -1)
			assert.Len(t, mentions, 2,
				"%s must appear exactly twice in executable text: once as the read "+
					"inside %s, once as the name passed to di_validate_override_url. "+
					"A third is a second reader, and that is how the validation gets "+
					"orphaned — resolved somewhere di_main's check cannot see, with no "+
					"other test failing. A first-and-only one means the validation call "+
					"itself is gone.",
				seam.variable, seam.resolver)
		})
	}
}

// ---------------------------------------------------------------------------
// Gate 2: Perimeter assertion tests (5 mandatory)
// ---------------------------------------------------------------------------

func TestScriptAssertPerimeter_IAPEnforcing(t *testing.T) {
	// Simulate IAP: 302 to accounts.google.com with IAP header
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Location", "https://accounts.google.com/signin?...")
		w.Header().Set("X-Goog-Iap-Generated-Response", "true")
		w.WriteHeader(http.StatusFound)
	}))
	defer server.Close()

	_, stderr, exitCode := runBashFunc(t, "di_assert_perimeter", server.URL)
	assert.Equal(t, 0, exitCode, "should succeed when IAP is enforcing; stderr: %s", stderr)
}

func TestScriptAssertPerimeter_AppAnswers(t *testing.T) {
	// Simulate no IAP: app answers directly with 200
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("Hello world"))
	}))
	defer server.Close()

	_, stderr, exitCode := runBashFunc(t, "di_assert_perimeter", server.URL)
	assert.NotEqual(t, 0, exitCode, "must FAIL when app answers directly")
	assert.Contains(t, stderr, "UNPROTECTED",
		"error message must clearly indicate the instance is unprotected")
}

func TestScriptAssertPerimeter_WrongRedirect(t *testing.T) {
	// 302 but not to accounts.google.com
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Location", "https://evil.example.com/phish")
		w.WriteHeader(http.StatusFound)
	}))
	defer server.Close()

	_, stderr, exitCode := runBashFunc(t, "di_assert_perimeter", server.URL)
	assert.NotEqual(t, 0, exitCode, "must fail when redirect is not to accounts.google.com")
	assert.Contains(t, stderr, "not to accounts.google.com")
}

func TestScriptAssertPerimeter_MissingIAPHeader(t *testing.T) {
	// 302 to accounts.google.com but missing the IAP header — still passes
	// because the redirect alone proves IAP is enforcing; the header is
	// a bonus check.
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Location", "https://accounts.google.com/signin?...")
		w.WriteHeader(http.StatusFound)
	}))
	defer server.Close()

	_, stderr, exitCode := runBashFunc(t, "di_assert_perimeter", server.URL)
	assert.Equal(t, 0, exitCode, "should pass even without IAP header if redirect is correct; stderr: %s", stderr)
}

func TestScriptAssertPerimeter_302NoLocationHeader(t *testing.T) {
	// 302 with NO Location header at all. Under set -euo pipefail, grep
	// for the header exits 1 and pipefail would kill the script before
	// it reaches its own SECURITY FAILURE message. The "|| location=''"
	// fix at :274 prevents this: the downstream check fires and fails
	// closed with a diagnostic.
	//
	// This test MUST assert the message, not just the exit code — both
	// the broken and fixed code exit 1, so an exit-code-only test would
	// pass on broken code.
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// 302 but deliberately no Location header
		w.WriteHeader(http.StatusFound)
	}))
	defer server.Close()

	_, stderr, exitCode := runBashFunc(t, "di_assert_perimeter", server.URL)
	assert.Equal(t, 1, exitCode, "must fail when 302 has no Location header")
	assert.Contains(t, stderr, "SECURITY FAILURE",
		"must print SECURITY FAILURE, not die silently from set -e")
	assert.Contains(t, stderr, "not to accounts.google.com",
		"must identify the problem as a wrong/missing redirect target")
}

func TestScriptAssertPerimeter_CloudRunErrorPage(t *testing.T) {
	// When the Instance is dead (wrong port, crash loop, missing binary),
	// Cloud Run returns its own error page (502 or 503) instead of the
	// IAP 302. The error message must mention Instance health so the
	// operator knows the problem is the container, not IAP.
	for _, code := range []int{http.StatusBadGateway, http.StatusServiceUnavailable} {
		t.Run(fmt.Sprintf("status_%d", code), func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(code)
				_, _ = w.Write([]byte("Cloud Run error page"))
			}))
			defer server.Close()

			_, stderr, exitCode := runBashFunc(t, "di_assert_perimeter", server.URL)
			assert.NotEqual(t, 0, exitCode, "must fail when Cloud Run returns %d", code)
			assert.Contains(t, stderr, "not be serving",
				"error message must mention the instance may not be serving")
			assert.Contains(t, stderr, "CMD",
				"error message must suggest checking the Dockerfile CMD")
		})
	}
}

// ---------------------------------------------------------------------------
// IAM member prefix tests
// ---------------------------------------------------------------------------

func TestScriptIAMMemberPrefix_UserEmail(t *testing.T) {
	stdout, _, exitCode := runBashFunc(t, "di_iam_member_prefix", "admin@example.com")
	assert.Equal(t, 0, exitCode)
	assert.Equal(t, "user:", strings.TrimSpace(stdout),
		"normal email must produce user: prefix")
}

func TestScriptIAMMemberPrefix_ServiceAccount(t *testing.T) {
	stdout, _, exitCode := runBashFunc(t, "di_iam_member_prefix", "deploy@my-project.iam.gserviceaccount.com")
	assert.Equal(t, 0, exitCode)
	assert.Equal(t, "serviceAccount:", strings.TrimSpace(stdout),
		"service account email must produce serviceAccount: prefix")
}

// ---------------------------------------------------------------------------
// Validate project number tests
// ---------------------------------------------------------------------------

func TestScriptValidateProjectNumber_Clean(t *testing.T) {
	for _, num := range []string{"123456789", "0", "721899303052"} {
		t.Run(num, func(t *testing.T) {
			_, _, exitCode := runBashFunc(t, "di_validate_project_number", num)
			assert.Equal(t, 0, exitCode,
				"valid project number %q must be accepted", num)
		})
	}
}

func TestScriptValidateProjectNumber_Contaminated(t *testing.T) {
	tests := []struct {
		name  string
		input string
	}{
		{
			name:  "impersonation warning prefix",
			input: "WARNING: This command is using service account impersonation. All API calls will be executed as [sa@proj.iam.gserviceaccount.com].\n721899303052",
		},
		{
			name:  "warning inline",
			input: "WARNING: 721899303052",
		},
		{
			name:  "letters mixed in",
			input: "72abc1899",
		},
		{
			name:  "whitespace",
			input: " 721899303052 ",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, _, exitCode := runBashFunc(t, "di_validate_project_number", tt.input)
			assert.NotEqual(t, 0, exitCode,
				"contaminated project number %q must be rejected", tt.input)
		})
	}
}

// TestScriptValidateProjectNumber_Empty is separated because the Go test
// used an empty string which bash handles differently (empty arg vs no arg).
func TestScriptValidateProjectNumber_Empty(t *testing.T) {
	_, _, exitCode := runBashFunc(t, "di_validate_project_number", "")
	assert.NotEqual(t, 0, exitCode,
		"empty project number must be rejected")
}

// ---------------------------------------------------------------------------
// Validate instance URL tests
// ---------------------------------------------------------------------------

func TestScriptValidateInstanceURL_Valid(t *testing.T) {
	_, _, exitCode := runBashFunc(t, "di_validate_instance_url", "https://my-instance-123456789.us-east4.run.app")
	assert.Equal(t, 0, exitCode)
}

func TestScriptValidateInstanceURL_Contaminated(t *testing.T) {
	tests := []struct {
		name  string
		input string
	}{
		{
			name:  "warning in host",
			input: "https://ssh-probe-WARNING: This command is using service account imperson....run.app",
		},
		{
			name:  "not https",
			input: "http://my-instance-123.us-east4.run.app",
		},
		{
			name:  "wrong domain",
			input: "https://my-instance-123.us-east4.example.com",
		},
		{
			name:  "empty string",
			input: "",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, _, exitCode := runBashFunc(t, "di_validate_instance_url", tt.input)
			assert.NotEqual(t, 0, exitCode,
				"invalid instance URL %q must be rejected", tt.input)
		})
	}
}

// ---------------------------------------------------------------------------
// Registry derivation tests
// ---------------------------------------------------------------------------

func TestScriptDeriveRegistry_Valid(t *testing.T) {
	tests := []struct {
		name  string
		image string
		want  string
	}{
		{
			name:  "ghcr with tag",
			image: "ghcr.io/ptone/scion-omni:latest",
			want:  "ghcr.io/ptone",
		},
		{
			name:  "ghcr with version tag",
			image: "ghcr.io/ptone/scion-omni:v1.2.3",
			want:  "ghcr.io/ptone",
		},
		{
			name:  "ghcr with digest",
			image: "ghcr.io/ptone/scion-omni@sha256:abcdef1234567890",
			want:  "ghcr.io/ptone",
		},
		{
			name:  "ghcr no tag",
			image: "ghcr.io/ptone/scion-omni",
			want:  "ghcr.io/ptone",
		},
		{
			name:  "gcr with nested path",
			image: "us-docker.pkg.dev/my-project/my-repo/scion-omni:latest",
			want:  "us-docker.pkg.dev/my-project/my-repo",
		},
		{
			name:  "localhost with port",
			image: "localhost:5000/myimage:latest",
			want:  "localhost:5000",
		},
		{
			name:  "tag with digest combined",
			image: "ghcr.io/ptone/scion-omni:v1@sha256:abcdef",
			want:  "ghcr.io/ptone",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			stdout, stderr, exitCode := runBashFunc(t, "di_derive_registry", tt.image)
			require.Equal(t, 0, exitCode, "di_derive_registry(%q) failed: %s", tt.image, stderr)
			assert.Equal(t, tt.want, strings.TrimSpace(stdout))
		})
	}
}

func TestScriptDeriveRegistry_Invalid(t *testing.T) {
	tests := []struct {
		name  string
		image string
	}{
		{
			name:  "bare image with tag",
			image: "nginx:latest",
		},
		{
			name:  "bare image no tag",
			image: "nginx",
		},
		{
			name:  "docker library path",
			image: "library/nginx:latest",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, _, exitCode := runBashFunc(t, "di_derive_registry", tt.image)
			assert.NotEqual(t, 0, exitCode,
				"should reject image %q with no derivable registry", tt.image)
		})
	}
}

// ---------------------------------------------------------------------------
// Admin email comma rejection tests
// ---------------------------------------------------------------------------

func TestScriptRejectsCommaInAdminEmail(t *testing.T) {
	tests := []struct {
		name      string
		email     string
		wantError bool
	}{
		{
			name:      "valid single email",
			email:     "admin@example.com",
			wantError: false,
		},
		{
			name:      "comma-separated emails rejected",
			email:     "alice@example.com,bob@example.com",
			wantError: true,
		},
		{
			name:      "trailing comma rejected",
			email:     "admin@example.com,",
			wantError: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, _, exitCode := runBashFunc(t, "di_validate_admin_email", tt.email)
			if tt.wantError {
				assert.NotEqual(t, 0, exitCode,
					"comma guard must reject %q", tt.email)
			} else {
				assert.Equal(t, 0, exitCode,
					"comma guard must accept %q", tt.email)
			}
		})
	}
}

func TestScriptCommaInEmailBreaksGcloud(t *testing.T) {
	// Demonstrate WHY the comma guard exists: gcloud --set-env-vars uses
	// commas as the delimiter between key=value pairs.
	envVarStr := fmt.Sprintf(
		"SCION_SERVER_AUTH_MODE=proxy,SCION_SERVER_HUB_ADMINEMAILS=%s",
		"alice@example.com,bob@example.com")

	parts := strings.Split(envVarStr, ",")
	assert.Equal(t, 3, len(parts),
		"comma in email value causes gcloud to see 3 env vars instead of 2")
	assert.Equal(t, "bob@example.com", parts[2],
		"the second email becomes a broken env var fragment")
}

// ---------------------------------------------------------------------------
// IAP enable PATCH body and URL tests (via stub server)
// ---------------------------------------------------------------------------

func TestScriptEnableIAPPatchBody(t *testing.T) {
	// Verify the PATCH body by inspecting what di_iap_patch_body returns.
	stdout, _, exitCode := runBashFunc(t, "di_iap_patch_body")
	require.Equal(t, 0, exitCode)

	body := strings.TrimSpace(stdout)
	assert.Contains(t, body, `"iapEnabled":true`,
		"PATCH body must contain iapEnabled:true")
	assert.Contains(t, body, `"invokerIamDisabled":true`,
		"PATCH body must contain invokerIamDisabled:true")

	// Count the number of keys — should be exactly 2
	// Simple check: count occurrences of ":"
	assert.Equal(t, 2, strings.Count(body, ":"),
		"PATCH body must contain exactly 2 fields (iapEnabled and invokerIamDisabled)")
}

func TestScriptEnableIAPUpdateMask(t *testing.T) {
	// Verify the PATCH URL contains the correct updateMask.
	stdout, _, exitCode := runBashFunc(t, "di_build_iap_patch_url",
		"https://us-east4-run.googleapis.com", "us-east4", "my-project", "my-instance")
	require.Equal(t, 0, exitCode)

	url := strings.TrimSpace(stdout)
	assert.Contains(t, url, "updateMask=",
		"PATCH URL must include updateMask")
	assert.Contains(t, url, "iapEnabled",
		"updateMask must include iapEnabled")
	assert.Contains(t, url, "invokerIamDisabled",
		"updateMask must include invokerIamDisabled")
}

// TestScriptDefaultAPIBaseIsTheRegionalEndpoint pins the DEFAULT endpoint —
// the value every real deploy uses and no other test touches. Review r2 found
// that dropping "-run" from it (https://run.googleapis.com) left the entire
// suite green: TestScriptEnableIAPUpdateMask was the only test of the PATCH
// URL and it asserts on the updateMask alone, never the host. The global
// run.googleapis.com host does not serve these v2 instance paths, so that
// mutation breaks every deploy and no test notices.
//
// The gap is pre-existing, but the base is now an environment-dependent
// expression rather than a literal, which is exactly when a default-branch pin
// starts earning its keep. runBashFunc scrubs _DI_* from the child environment
// (see scrubbedEnv), so an ambient override cannot defeat this.
func TestScriptDefaultAPIBaseIsTheRegionalEndpoint(t *testing.T) {
	stdout, stderr, exitCode := runBashFunc(t, "di_resolve_api_base", "us-east4")
	require.Equal(t, 0, exitCode, "stderr: %s", stderr)

	assert.Equal(t, "https://us-east4-run.googleapis.com", strings.TrimSpace(stdout),
		"the default API base must be the REGIONAL Cloud Run endpoint. The global "+
			"host does not serve /v2/projects/*/locations/*/instances, so a deploy "+
			"against it fails at the preflight GET and at the step 3b PATCH.")

	// And the region really is interpolated, not hardcoded.
	stdout, _, exitCode = runBashFunc(t, "di_resolve_api_base", "europe-west1")
	require.Equal(t, 0, exitCode)
	assert.Equal(t, "https://europe-west1-run.googleapis.com", strings.TrimSpace(stdout),
		"the API base must follow --region")
}

// TestScriptDefaultTokeninfoURL pins the other default for the same reason.
func TestScriptDefaultTokeninfoURL(t *testing.T) {
	stdout, stderr, exitCode := runBashFunc(t, "di_resolve_tokeninfo_url")
	require.Equal(t, 0, exitCode, "stderr: %s", stderr)
	assert.Equal(t, "https://oauth2.googleapis.com/tokeninfo", strings.TrimSpace(stdout),
		"the default tokeninfo endpoint must be Google's")
}

// ---------------------------------------------------------------------------
// Env var round-trip tests — read deploy.sh, extract env vars, validate
// through config.LoadGlobalConfig
// ---------------------------------------------------------------------------

// extractEnvVarsFromDeployScript reads deploy.sh and extracts the env var
// names from the --set-env-vars line. Returns a map of env var name → value
// template (with ${...} placeholders replaced by sample values).
func extractEnvVarsFromDeployScript(t *testing.T) map[string]string {
	t.Helper()
	script := readDeployScript(t)

	// Find the --set-env-vars line. It's a comma-delimited list of KEY=VALUE
	// pairs. The format in the script is:
	//   --set-env-vars "KEY1=val1,KEY2=val2,..."
	re := regexp.MustCompile(`--set-env-vars\s+"([^"]+)"`)
	match := re.FindStringSubmatch(script)
	require.NotNil(t, match, "could not find --set-env-vars in deploy.sh")

	envStr := match[1]
	// Split on commas, but respect ${...} which may contain commas (they don't
	// in this case, but be safe).
	pairs := strings.Split(envStr, ",")

	result := make(map[string]string)
	for _, pair := range pairs {
		eqIdx := strings.Index(pair, "=")
		require.Greater(t, eqIdx, 0, "malformed env var pair: %q", pair)
		key := pair[:eqIdx]
		val := pair[eqIdx+1:]
		result[key] = val
	}
	return result
}

// TestScriptDeployEnvVarsRoundTrip proves that the env vars deploy.sh sets
// load correctly through the config system into the structs the hub reads.
// The critical concern is that Auth.Proxy (*ProxyAuthConfig) and Proxy.IAP
// (*IAPAuthConfig) are pointer fields — if koanf/mapstructure doesn't
// allocate them, the audience is empty and the hub fails at startup.
func TestScriptDeployEnvVarsRoundTrip(t *testing.T) {
	envVars := extractEnvVarsFromDeployScript(t)

	// Verify the expected env var names are present in the script.
	require.Contains(t, envVars, "SCION_SERVER_MODE",
		"deploy.sh must set SCION_SERVER_MODE")
	require.Contains(t, envVars, "SCION_SERVER_AUTH_MODE",
		"deploy.sh must set SCION_SERVER_AUTH_MODE")
	require.Contains(t, envVars, "SCION_SERVER_AUTH_PROXY_PROVIDER",
		"deploy.sh must set SCION_SERVER_AUTH_PROXY_PROVIDER")
	require.Contains(t, envVars, "SCION_SERVER_AUTH_PROXY_IAP_AUDIENCE",
		"deploy.sh must set SCION_SERVER_AUTH_PROXY_IAP_AUDIENCE")
	require.Contains(t, envVars, "SCION_SERVER_HUB_ADMINEMAILS",
		"deploy.sh must set SCION_SERVER_HUB_ADMINEMAILS")
	require.Contains(t, envVars, "SCION_IMAGE_REGISTRY",
		"deploy.sh must set SCION_IMAGE_REGISTRY")

	// Use a clean HOME so no existing settings.yaml / server.yaml interfere.
	tmpDir := t.TempDir()
	t.Setenv("HOME", tmpDir)
	scionDir := filepath.Join(tmpDir, ".scion")
	require.NoError(t, os.MkdirAll(scionDir, 0755))

	// Set env vars with sample values matching what the script would produce.
	t.Setenv("SCION_SERVER_MODE", "hosted")
	t.Setenv("SCION_SERVER_AUTH_MODE", "proxy")
	t.Setenv("SCION_SERVER_AUTH_PROXY_PROVIDER", "iap")
	t.Setenv("SCION_SERVER_AUTH_PROXY_IAP_AUDIENCE",
		"/projects/123456789/locations/us-east4/services/test-instance")
	t.Setenv("SCION_SERVER_HUB_ADMINEMAILS", "admin@example.com")

	gc, err := config.LoadGlobalConfig("")
	require.NoError(t, err, "LoadGlobalConfig must succeed with deploy env vars")

	assert.Equal(t, "hosted", gc.Mode,
		"Mode must be 'hosted' — without this the server runs in workstation "+
			"mode, auto-enables dev auth, and crashes on a non-loopback host")
	assert.Equal(t, "proxy", gc.Auth.Mode,
		"Auth.Mode must be 'proxy'")
	require.NotNil(t, gc.Auth.Proxy,
		"Auth.Proxy pointer must be allocated by config loading")
	assert.Equal(t, "iap", gc.Auth.Proxy.Provider,
		"Auth.Proxy.Provider must be 'iap'")
	require.NotNil(t, gc.Auth.Proxy.IAP,
		"Auth.Proxy.IAP pointer must be allocated by config loading")
	assert.Equal(t,
		"/projects/123456789/locations/us-east4/services/test-instance",
		gc.Auth.Proxy.IAP.Audience,
		"Auth.Proxy.IAP.Audience must match the IAP audience path")

	// Admin email reaches cfg.Hub.AdminEmails
	assert.Contains(t, gc.Hub.AdminEmails, "admin@example.com",
		"SCION_SERVER_HUB_ADMINEMAILS must populate cfg.Hub.AdminEmails — "+
			"this is the field parseAdminEmails reads to set the admin role")
}

// TestScriptDeployHostedModeEnvRequired is a pinning test.
// When SCION_SERVER_MODE is absent, the server defaults to workstation mode.
// Workstation mode calls applyWorkstationDefaults which sets enableDevAuth=true.
// On Cloud Run the host is 0.0.0.0, so the non-loopback dev-auth guard fires
// and the server exits immediately. SCION_SERVER_MODE=hosted is the fix.
func TestScriptDeployHostedModeEnvRequired(t *testing.T) {
	envVars := extractEnvVarsFromDeployScript(t)
	require.Contains(t, envVars, "SCION_SERVER_MODE",
		"deploy.sh must set SCION_SERVER_MODE")
	require.Equal(t, "hosted", envVars["SCION_SERVER_MODE"],
		"deploy.sh must set SCION_SERVER_MODE=hosted")

	tmpDir := t.TempDir()
	t.Setenv("HOME", tmpDir)
	require.NoError(t, os.MkdirAll(filepath.Join(tmpDir, ".scion"), 0755))

	t.Setenv("SCION_SERVER_MODE", "hosted")

	gc, err := config.LoadGlobalConfig("")
	require.NoError(t, err)

	assert.Equal(t, "hosted", gc.Mode,
		"SCION_SERVER_MODE=hosted must map to cfg.Mode='hosted' — "+
			"without this, workstation defaults enable dev auth and crash the server")
}

// ---------------------------------------------------------------------------
// IAP PATCH — verify actual HTTP request via stub server
// ---------------------------------------------------------------------------

func TestScriptEnableIAPPatchBodyViaStubServer(t *testing.T) {
	var capturedBody []byte
	var capturedURL string
	var capturedMethod string
	var capturedContentType string

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedMethod = r.Method
		capturedURL = r.URL.String()
		capturedContentType = r.Header.Get("Content-Type")
		body, err := io.ReadAll(r.Body)
		if err == nil {
			capturedBody = body
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{}`))
	}))
	defer server.Close()

	// Call the stub server with the same body and URL structure deploy.sh uses
	patchBody, _, _ := runBashFunc(t, "di_iap_patch_body")
	patchBody = strings.TrimSpace(patchBody)

	// Use curl to send the PATCH to our stub server — mirrors what deploy.sh does
	bashCmd := fmt.Sprintf(`curl -s -o /dev/null -w "%%{http_code}" \
		-X PATCH \
		-H "Authorization: Bearer fake-token" \
		-H "Content-Type: application/json" \
		-d '%s' \
		"%s?updateMask=iapEnabled,invokerIamDisabled"`, patchBody, server.URL)

	cmd := exec.Command(testBash(), "-c", bashCmd)
	out, err := cmd.Output()
	require.NoError(t, err, "curl to stub server failed")
	assert.Equal(t, "200", strings.TrimSpace(string(out)))

	// Verify the request
	assert.Equal(t, "PATCH", capturedMethod)
	assert.Equal(t, "application/json", capturedContentType)
	assert.Contains(t, capturedURL, "updateMask=iapEnabled,invokerIamDisabled")
	assert.Contains(t, string(capturedBody), `"iapEnabled":true`)
	assert.Contains(t, string(capturedBody), `"invokerIamDisabled":true`)
}

// ---------------------------------------------------------------------------
// Preflight: gcloud capability check
// ---------------------------------------------------------------------------

// ---------------------------------------------------------------------------
// Shell-differential self-test: banner invocation count
// ---------------------------------------------------------------------------

// TestShellDifferentialSelfTestBannerCount asserts that running
// `shell-differential.sh --self-test` executes the self-test banner exactly
// once. Without the SHELL_DIFFERENTIAL_SELFTEST export at the top of the
// --self-test block, each of the four check() calls spawns a 4-argument child
// that hits the guard at :146 and triggers a redundant nested self-test — five
// total invocations instead of one.
//
// The visible stdout count is 1 either way, because check() redirects its
// children's output to /dev/null and the guard captures the nested self-test
// via command substitution. So this test measures TOTAL invocations via a
// sideband file: a patched copy of the script appends a marker on every
// banner execution, and the test counts markers.
func TestShellDifferentialSelfTestBannerCount(t *testing.T) {
	scriptPath := filepath.Join(repoRoot(t), "scripts", "dev", "shell-differential.sh")
	scriptContent, err := os.ReadFile(scriptPath)
	require.NoError(t, err)

	traceFile := filepath.Join(t.TempDir(), "banner-trace")
	patched := strings.Replace(
		string(scriptContent),
		`echo "self-test: ${SCION_TEST_BASH:-bash}"`,
		fmt.Sprintf(`echo "self-test: ${SCION_TEST_BASH:-bash}"; echo x >> %q`, traceFile),
		1,
	)
	patchedScript := filepath.Join(t.TempDir(), "shell-differential.sh")
	require.NoError(t, os.WriteFile(patchedScript, []byte(patched), 0755))

	cmd := exec.Command(patchedScript, "--self-test")
	output, err := cmd.CombinedOutput()
	require.NoError(t, err, "self-test must pass; output: %s", output)

	traceData, err := os.ReadFile(traceFile)
	require.NoError(t, err, "trace file must exist — the banner was never reached")
	count := strings.Count(string(traceData), "x")
	assert.Equal(t, 1, count,
		"self-test banner must execute exactly once; got %d — each extra "+
			"invocation is a redundant self-test spawned because "+
			"SHELL_DIFFERENTIAL_SELFTEST was not exported to check()'s children",
		count)
}

func TestScriptCheckGcloudInstances_FailureMessage(t *testing.T) {
	// On this container (gcloud 575.0.0), the preflight SHOULD fail.
	// On a container with gcloud 582+, skip this test.
	_, stderr, exitCode := runBashFunc(t, "di_check_gcloud_instances")

	if exitCode == 0 {
		t.Skip("gcloud beta run instances is available — cannot test failure path")
	}

	assert.Equal(t, 1, exitCode)
	assert.Contains(t, stderr, "gcloud beta run instances")
	assert.Contains(t, stderr, "575.0.0")
	assert.Contains(t, stderr, "582.0.0")
	assert.Contains(t, stderr, "gcloud components update")
	assert.Contains(t, stderr, "DO NOT use 'gcloud alpha run instances'")
	assert.Contains(t, stderr, "--sandbox-launcher")
}

// ---------------------------------------------------------------------------
// Token-not-in-argv: leak test + paired auth test (Rule 170)
//
// Rule 170: a safety fix needs a test that fails when it OVER-reaches, not
// only one that fails when it under-reaches.
//
// Test 1 (leak test): the token must not appear in curl's process argv.
// Test 2 (paired auth test): the request must still be authenticated after
// the token is moved out of argv.
//
// A redaction that removes the token AND also breaks the request passes every
// leak test. The paired test catches that: mutate the fix to drop the header
// entirely, and the paired test must go red.
// ---------------------------------------------------------------------------

// TestScriptTokenNotInCurlArgv pins the property that the ADC access token
// never appears in curl's argv.  A live token on the command line is readable
// by any local user via ps(1), leaks into shell history, and appears in
// set -x traces.
//
// The test wraps curl with a function that records every invocation's argv to
// a sideband file (the $* expansion — exactly what ps shows), then calls the
// real curl binary so the stub servers still answer.  After the full
// preflight + step 3b flow, the argv log is grepped for the fake token value.
//
// Three sites carried the token in argv in the original code:
//   - tokeninfo: the token was a URL query parameter (visible in ps as the URL arg)
//   - API validation GET: -H "Authorization: Bearer <token>"
//   - step 3b PATCH: -H "Authorization: Bearer <token>"
//
// The fix uses -K <configfile> for Bearer headers and -d @<file> for the
// tokeninfo POST, keeping the token out of the process table entirely.
func TestScriptTokenNotInCurlArgv(t *testing.T) {
	// The stub answers with 500 for the PATCH so di_main aborts at step 3b
	// rather than polling for IAP enforcement for three minutes.
	var patchSeen atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/tokeninfo" || r.URL.Query().Get("access_token") != "" {
			_, _ = io.WriteString(w, `{"email":"operator@example.com"}`)
			return
		}
		if r.Method == http.MethodPatch {
			patchSeen.Add(1)
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = io.WriteString(w, `{"error":"stub"}`)
			return
		}
		// API validation GET
		_, _ = io.WriteString(w, `{}`)
	}))
	t.Cleanup(server.Close)

	tmp := t.TempDir()
	argvLog := filepath.Join(tmp, "gcloud-argv.log")
	curlArgvLog := filepath.Join(tmp, "curl-argv.log")

	// A curl wrapper that records argv then calls the real binary.
	// `command -v curl` resolves the path to the external binary before
	// the function shadows the name.  NEVER log the token value itself —
	// this test is about a token leak, and creating one in the test output
	// would be the same class of defect.
	curlWrapper := fmt.Sprintf(`
_di_real_curl="$(command -v curl)"
curl() {
    printf '%%s\n' "$*" >> %q
    "$_di_real_curl" "$@"
}`, curlArgvLog)

	// fullGcloudStub refuses the deploy (step 3a), so di_main would never
	// reach step 3b.  This stub lets step 3a succeed; the PATCH server
	// returns 500 to stop di_main before the IAP polling loop.
	gcloudStub := fmt.Sprintf(`gcloud() {
  printf '%%s\n' "$*" >> %q
  case "$*" in
    "beta run instances --help")                   return 0 ;;
    "config get account")                          printf '%%s\n' "operator@example.com" ;;
    "projects describe "*)                         printf '%%s\n' "123456789" ;;
    "auth application-default print-access-token") printf '%%s\n' "ya29.fake-test-token" ;;
    "beta run instances deploy "*)                 return 0 ;;
    *)
      echo "test stub: unexpected gcloud invocation: gcloud $*" >&2
      return 1 ;;
  esac
}`, argvLog)
	setup := preflightSetup(gcloudStub+"\n"+curlWrapper, server.URL)

	_, stderr, exitCode := runBashFuncWithSetup(t, setup,
		"di_main",
		"--name", "test-name",
		"--project", "test-project",
		"--image", "ghcr.io/example/scion-omni:latest",
		"--region", "us-east4")

	// di_main should fail at the PATCH (server returns 500), but it must
	// have reached the PATCH — otherwise the test cannot see site 3.
	require.NotEqual(t, 0, exitCode,
		"di_main should fail at the stubbed PATCH; stderr: %s", stderr)
	require.Equal(t, int32(1), patchSeen.Load(),
		"di_main must reach the step 3b PATCH so this test covers all three "+
			"token-in-argv sites; stderr: %s", stderr)

	curlArgvData, err := os.ReadFile(curlArgvLog)
	require.NoError(t, err,
		"the curl wrapper was never invoked — it may not be intercepting curl "+
			"calls; stderr: %s", stderr)
	curlArgv := string(curlArgvData)

	// The gcloud stub returns "ya29.fake-test-token" as the access token.
	// This value MUST NOT appear anywhere in curl's argv.
	assert.NotContains(t, curlArgv, "ya29.fake-test-token",
		"the access token appears in curl's process argv, where any local user "+
			"can read it via ps(1).  Use -K <configfile> for Bearer headers and "+
			"-d @<file> for POST data to keep the token out of the process table.\n"+
			"curl argv log:\n%s", curlArgv)
}

// TestScriptTokenStillAuthenticatesRequests is the Rule 170 PAIRED test.
// It asserts the requests are still correctly authenticated after the token
// is moved out of argv.  Without this, a fix that removes the token AND
// removes the header passes every leak test.
//
// The stub server records whether the Authorization header arrived on the
// API validation GET and on the step 3b PATCH.  Both must carry
// "Bearer ya29.fake-test-token".
func TestScriptTokenStillAuthenticatesRequests(t *testing.T) {
	var mu sync.Mutex
	var getAuth, patchAuth string
	getHits := 0
	patchHits := 0

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/tokeninfo" || r.URL.Query().Get("access_token") != "" {
			_, _ = io.WriteString(w, `{"email":"operator@example.com"}`)
			return
		}
		if r.Method == http.MethodPatch {
			mu.Lock()
			patchAuth = r.Header.Get("Authorization")
			patchHits++
			mu.Unlock()
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = io.WriteString(w, `{"error":"stub"}`)
			return
		}
		// API validation GET — record the auth header
		mu.Lock()
		getAuth = r.Header.Get("Authorization")
		getHits++
		mu.Unlock()
		_, _ = io.WriteString(w, `{}`)
	}))
	t.Cleanup(server.Close)

	tmp := t.TempDir()
	argvLog := filepath.Join(tmp, "gcloud-argv.log")

	gcloudStub := fmt.Sprintf(`gcloud() {
  printf '%%s\n' "$*" >> %q
  case "$*" in
    "beta run instances --help")                   return 0 ;;
    "config get account")                          printf '%%s\n' "operator@example.com" ;;
    "projects describe "*)                         printf '%%s\n' "123456789" ;;
    "auth application-default print-access-token") printf '%%s\n' "ya29.fake-test-token" ;;
    "beta run instances deploy "*)                 return 0 ;;
    *)
      echo "test stub: unexpected gcloud invocation: gcloud $*" >&2
      return 1 ;;
  esac
}`, argvLog)
	setup := preflightSetup(gcloudStub, server.URL)

	_, stderr, exitCode := runBashFuncWithSetup(t, setup,
		"di_main",
		"--name", "test-name",
		"--project", "test-project",
		"--image", "ghcr.io/example/scion-omni:latest",
		"--region", "us-east4")

	// di_main fails at the PATCH (500), which is expected.
	require.NotEqual(t, 0, exitCode,
		"di_main should fail at the stubbed PATCH; stderr: %s", stderr)

	mu.Lock()
	gotGetAuth, gotPatchAuth := getAuth, patchAuth
	gotGetHits, gotPatchHits := getHits, patchHits
	mu.Unlock()

	require.Equal(t, 1, gotGetHits,
		"the API validation GET must reach the stub; stderr: %s", stderr)
	require.Equal(t, 1, gotPatchHits,
		"the step 3b PATCH must reach the stub; stderr: %s", stderr)

	assert.Equal(t, "Bearer ya29.fake-test-token", gotGetAuth,
		"the API validation GET must carry the Bearer token — the token was "+
			"removed from argv but must still be sent in the Authorization header "+
			"via -K <configfile>")
	assert.Equal(t, "Bearer ya29.fake-test-token", gotPatchAuth,
		"the step 3b PATCH must carry the Bearer token — the token was removed "+
			"from argv but must still be sent in the Authorization header via "+
			"-K <configfile>")
}
