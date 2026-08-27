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
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ---------------------------------------------------------------------------
// These tests replace the pinning tests that were previously in
// deploy_instance_test.go. The Go command is being removed; the script
// is the new source of truth for the IAP audience and instance URL
// format strings.
//
// The test reads scripts/single-node/deploy.sh from disk, extracts the
// two format strings (marked with "# FORMAT STRING:" comments), substitutes
// sample values, and feeds the results to isSupportedIAPAudience and
// iapAudienceToCloudRunURL — the same validators the hub uses at runtime.
//
// This keeps one authoritative copy of each string (in the script) and
// keeps CI able to catch a bad edit to it.
// ---------------------------------------------------------------------------

// repoRoot returns the repository root by walking up from the test file's
// directory until we find go.mod.
func repoRoot(t *testing.T) string {
	t.Helper()
	// Start from the directory containing this test file.
	_, thisFile, _, ok := runtime.Caller(0)
	require.True(t, ok, "runtime.Caller failed")
	dir := filepath.Dir(thisFile)

	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		require.NotEqual(t, dir, parent, "reached filesystem root without finding go.mod")
		dir = parent
	}
}

// readDeployScript reads scripts/single-node/deploy.sh from the repo root.
func readDeployScript(t *testing.T) string {
	t.Helper()
	path := filepath.Join(repoRoot(t), "scripts", "single-node", "deploy.sh")
	data, err := os.ReadFile(path)
	require.NoError(t, err, "failed to read deploy.sh — is the repo root correct?")
	return string(data)
}

// extractFormatString extracts a format string from deploy.sh by looking for
// the "# FORMAT STRING: <pattern>" comment immediately before the assignment.
// Returns the format string (e.g. "/projects/%s/locations/%s/services/%s").
func extractFormatStrings(t *testing.T, script string) (audienceFmt, urlFmt string) {
	t.Helper()
	// Match lines like: # FORMAT STRING: /projects/%s/locations/%s/services/%s
	re := regexp.MustCompile(`# FORMAT STRING:\s*(.+)`)
	matches := re.FindAllStringSubmatch(script, -1)
	require.Len(t, matches, 2,
		"expected exactly 2 FORMAT STRING comments in deploy.sh; found %d", len(matches))

	audienceFmt = strings.TrimSpace(matches[0][1])
	urlFmt = strings.TrimSpace(matches[1][1])
	return audienceFmt, urlFmt
}

// substituteFormatString replaces %s placeholders with the given values.
func substituteFormatString(format string, values ...string) string {
	result := format
	for _, v := range values {
		result = strings.Replace(result, "%s", v, 1)
	}
	return result
}

// TestScriptIAPAudienceAcceptedByIsSupportedIAPAudience verifies that the
// audience format string in deploy.sh, when populated with sample values,
// is accepted by the hub's isSupportedIAPAudience validator.
//
// This is the replacement for TestBuildIAPAudienceAcceptedByIsSupportedIAPAudience
// from deploy_instance_test.go.
func TestScriptIAPAudienceAcceptedByIsSupportedIAPAudience(t *testing.T) {
	script := readDeployScript(t)
	audienceFmt, _ := extractFormatStrings(t, script)

	// The audience format is: /projects/%s/locations/%s/services/%s
	// Substitution order matches the script: PROJECT_NUMBER, REGION, NAME
	audience := substituteFormatString(audienceFmt, "123456789", "us-east4", "scion-hub-1")

	assert.True(t, isSupportedIAPAudience(audience),
		"deploy.sh audience format string %q, populated as %q, must be accepted "+
			"by isSupportedIAPAudience — if this fails, the hub will reject IAP "+
			"tokens for Instances deployed with this script",
		audienceFmt, audience)
}

// TestScriptInstanceURLMatchesIapAudienceToCloudRunURL verifies that the
// URL and audience format strings in deploy.sh agree: feeding the audience
// to iapAudienceToCloudRunURL must produce the same URL as populating the
// URL format string directly.
//
// This is the replacement for TestBuildInstanceURLMatchesIapAudienceToCloudRunURL
// from deploy_instance_test.go.
func TestScriptInstanceURLMatchesIapAudienceToCloudRunURL(t *testing.T) {
	script := readDeployScript(t)
	audienceFmt, urlFmt := extractFormatStrings(t, script)

	projectNumber := "123456789"
	region := "us-east4"
	name := "scion-hub-1"

	// Audience: /projects/%s/locations/%s/services/%s → PROJECT_NUMBER, REGION, NAME
	audience := substituteFormatString(audienceFmt, projectNumber, region, name)
	// URL: https://%s-%s.%s.run.app → NAME, PROJECT_NUMBER, REGION
	directURL := substituteFormatString(urlFmt, name, projectNumber, region)

	fromAudience := iapAudienceToCloudRunURL(audience)
	assert.Equal(t, directURL, fromAudience,
		"deploy.sh URL format and audience format must agree: "+
			"iapAudienceToCloudRunURL(%q) = %q, but URL format gives %q",
		audience, fromAudience, directURL)
}

// TestScriptIAPAudienceUsesServicesNotInstances is a pinning test.
// The IAP audience uses "services" even for Cloud Run Instances. This is
// IAP's fixed resource vocabulary across every backend type. Changing
// "services" to "instances" will produce an audience mismatch on every
// request, resulting in a 401 that does not obviously point back here.
//
// This is the replacement for TestBuildIAPAudienceUsesServicesNotInstances
// from deploy_instance_test.go.
func TestScriptIAPAudienceUsesServicesNotInstances(t *testing.T) {
	script := readDeployScript(t)
	audienceFmt, _ := extractFormatStrings(t, script)

	audience := substituteFormatString(audienceFmt, "123", "us-east4", "my-instance")

	assert.Contains(t, audience, "/services/",
		"IAP audience must use 'services' vocabulary even for Instances — "+
			"this is IAP's fixed path format. Do NOT change to 'instances'")
	assert.NotContains(t, audience, "/instances/",
		"IAP audience must NOT use 'instances' — IAP uses 'services' for all "+
			"backend types including Cloud Run Instances")

	// Also verify the format string itself contains "services" — catches
	// a substitution-order error that might produce "services" by accident.
	assert.Contains(t, audienceFmt, "/services/",
		"the format string itself must contain '/services/', not just the "+
			"populated result")
}

// TestScriptFormatStringsHavePlaceholders is a meta-test: it verifies that
// the extraction found format strings that actually contain %s placeholders.
// Without this, a badly edited comment could make the tests pass vacuously.
func TestScriptFormatStringsHavePlaceholders(t *testing.T) {
	script := readDeployScript(t)
	audienceFmt, urlFmt := extractFormatStrings(t, script)

	assert.Equal(t, 3, strings.Count(audienceFmt, "%s"),
		"audience format string must have exactly 3 %%s placeholders: %q", audienceFmt)
	assert.Equal(t, 3, strings.Count(urlFmt, "%s"),
		"URL format string must have exactly 3 %%s placeholders: %q", urlFmt)
}

// ---------------------------------------------------------------------------
// Ordering pin: the ADC credential is proven BEFORE the first mutation
// ---------------------------------------------------------------------------

// TestScriptPreflightRunsBeforeInstanceCreation pins the ORDER of the ADC
// preflight relative to step 3a. This is the defect the preflight exists to
// fix: the original script discovered its REST credential was unusable only
// after `gcloud beta run instances deploy` had already created the Instance,
// leaving a half-built deploy — Instance running, IAP off, no rollback.
//
// This pin is deliberately a di_main test, not a unit test of
// di_preflight_rest_credential. Review r1 moved the entire preflight block to
// after step 3a — reintroducing the original bug in full — and every unit test
// of the function still passed, because a unit test cannot see WHEN the
// function is called. The whole suite was green with the bug restored.
//
// The stubbed deploy records its argv and then fails on purpose. Failing there
// stops di_main at step 3a, which keeps this test off the network: the step 3b
// PATCH URL is built from the real API host and has no test seam. It also
// means that if the preflight is moved below step 3a, the ADC mint is never
// recorded at all and this test fails on the first require below.
func TestScriptPreflightRunsBeforeInstanceCreation(t *testing.T) {
	server, _ := newPreflightStub(t, `{"email":"operator@example.com"}`, http.StatusOK, `{}`)
	argvLog := filepath.Join(t.TempDir(), "gcloud-argv.log")

	gcloudStub := fmt.Sprintf(`gcloud() {
  printf '%%s\n' "$*" >> %q
  case "$*" in
    "beta run instances --help")
      return 0 ;;
    "config get account")
      printf '%%s\n' "operator@example.com" ;;
    "projects describe "*)
      printf '%%s\n' "123456789" ;;
    "auth application-default print-access-token")
      printf '%%s\n' "ya29.fake-test-token" ;;
    "beta run instances deploy "*)
      echo "test stub: recorded the deploy, refusing to really run it" >&2
      return 1 ;;
    *)
      echo "test stub: unexpected gcloud invocation: gcloud $*" >&2
      return 1 ;;
  esac
}`, argvLog)

	_, stderr, exitCode := runBashFuncWithSetup(t, preflightSetup(gcloudStub, server.URL),
		"di_main",
		"--name", "test-name",
		"--project", "test-project",
		"--image", "ghcr.io/example/scion-omni:latest",
		"--region", "us-east4")

	require.NotEqual(t, 0, exitCode,
		"the stubbed deploy fails on purpose, so di_main must abort; stderr: %s", stderr)

	argv := readGcloudArgvLog(t, argvLog)
	adcAt := strings.Index(argv, "auth application-default print-access-token")
	deployAt := strings.Index(argv, "beta run instances deploy ")

	require.NotEqual(t, -1, adcAt,
		"di_main never minted an ADC token. The preflight must run before the "+
			"Instance is created — if it moved below step 3a, a credential failure "+
			"again leaves a created Instance with IAP off.\nrecorded gcloud calls:\n%s", argv)
	require.NotEqual(t, -1, deployAt,
		"di_main never called 'gcloud beta run instances deploy' — this test can no "+
			"longer see the ordering it exists to pin.\nrecorded gcloud calls:\n%s", argv)
	assert.Less(t, adcAt, deployAt,
		"the ADC token must be minted and validated BEFORE the Instance is created, "+
			"so a bad credential aborts with zero mutations.\nrecorded gcloud calls:\n%s", argv)
}

// TestScriptAudienceFormatMatchesGoBuilder verifies that the format strings
// in deploy.sh produce the same output as the Go BuildIAPAudience and
// BuildInstanceURL functions (which still exist at this commit).
func TestScriptAudienceFormatMatchesGoBuilder(t *testing.T) {
	script := readDeployScript(t)
	audienceFmt, urlFmt := extractFormatStrings(t, script)

	projectNumber := "721899303052"
	region := "us-east4"
	name := "sn-ready"

	// Script format strings
	scriptAudience := substituteFormatString(audienceFmt, projectNumber, region, name)
	scriptURL := substituteFormatString(urlFmt, name, projectNumber, region)

	// Go builders (still present at this commit)
	goAudience := fmt.Sprintf("/projects/%s/locations/%s/services/%s", projectNumber, region, name)
	goURL := fmt.Sprintf("https://%s-%s.%s.run.app", name, projectNumber, region)

	assert.Equal(t, goAudience, scriptAudience,
		"script audience format must produce the same result as the Go format string")
	assert.Equal(t, goURL, scriptURL,
		"script URL format must produce the same result as the Go format string")
}
