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
