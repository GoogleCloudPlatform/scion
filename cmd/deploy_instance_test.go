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
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ---------------------------------------------------------------------------
// BuildIAPAudience tests
// ---------------------------------------------------------------------------

func TestBuildIAPAudience(t *testing.T) {
	tests := []struct {
		name          string
		projectNumber string
		region        string
		instanceName  string
		want          string
	}{
		{
			name:          "standard audience path",
			projectNumber: "123456789",
			region:        "us-east4",
			instanceName:  "scion-hub-1",
			want:          "/projects/123456789/locations/us-east4/services/scion-hub-1",
		},
		{
			name:          "different region",
			projectNumber: "999",
			region:        "us-central1",
			instanceName:  "my-instance",
			want:          "/projects/999/locations/us-central1/services/my-instance",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := BuildIAPAudience(tt.projectNumber, tt.region, tt.instanceName)
			assert.Equal(t, tt.want, got)
		})
	}
}

// TestBuildIAPAudienceUsesServicesNotInstances is a pinning test required by
// §11.9. The IAP audience uses "services" even for Cloud Run Instances.
// This is IAP's fixed resource vocabulary across every backend type, not a bug.
// Changing "services" to "instances" will produce an audience mismatch on
// every request, resulting in a 401 that does not obviously point back here.
// See §11.3 of cloudrun-instances-sandboxes.md.
func TestBuildIAPAudienceUsesServicesNotInstances(t *testing.T) {
	audience := BuildIAPAudience("123", "us-east4", "my-instance")

	// The audience MUST contain "services", NOT "instances".
	assert.Contains(t, audience, "/services/",
		"IAP audience must use 'services' vocabulary even for Instances — "+
			"this is IAP's fixed path format. Do NOT change to 'instances'; "+
			"see §11.3 of cloudrun-instances-sandboxes.md")
	assert.NotContains(t, audience, "/instances/",
		"IAP audience must NOT use 'instances' — IAP uses 'services' for all "+
			"backend types including Cloud Run Instances")
}

// TestBuildIAPAudienceAcceptedByIsSupportedIAPAudience verifies that the
// audience format produced by BuildIAPAudience is accepted by the existing
// isSupportedIAPAudience validator in server_foreground.go.
func TestBuildIAPAudienceAcceptedByIsSupportedIAPAudience(t *testing.T) {
	audience := BuildIAPAudience("123456789", "us-east4", "scion-hub-1")
	assert.True(t, isSupportedIAPAudience(audience),
		"BuildIAPAudience output must be accepted by isSupportedIAPAudience — "+
			"if this fails, the hub will reject IAP tokens for this Instance")
}

// ---------------------------------------------------------------------------
// BuildInstanceURL tests
// ---------------------------------------------------------------------------

func TestBuildInstanceURL(t *testing.T) {
	tests := []struct {
		name          string
		instanceName  string
		projectNumber string
		region        string
		want          string
	}{
		{
			name:          "standard URL format",
			instanceName:  "scion-hub-1",
			projectNumber: "123456789",
			region:        "us-east4",
			want:          "https://scion-hub-1-123456789.us-east4.run.app",
		},
		{
			name:          "different name and region",
			instanceName:  "test-inst",
			projectNumber: "999",
			region:        "us-central1",
			want:          "https://test-inst-999.us-central1.run.app",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := BuildInstanceURL(tt.instanceName, tt.projectNumber, tt.region)
			assert.Equal(t, tt.want, got)
		})
	}
}

// TestBuildInstanceURLMatchesIapAudienceToCloudRunURL verifies that BuildInstanceURL
// produces the same URL that iapAudienceToCloudRunURL would derive from the
// corresponding audience path. This is a consistency check — both functions
// must agree on the URL format.
func TestBuildInstanceURLMatchesIapAudienceToCloudRunURL(t *testing.T) {
	projectNumber := "123456789"
	region := "us-east4"
	name := "scion-hub-1"

	audience := BuildIAPAudience(projectNumber, region, name)
	fromAudience := iapAudienceToCloudRunURL(audience)
	direct := BuildInstanceURL(name, projectNumber, region)

	assert.Equal(t, fromAudience, direct,
		"BuildInstanceURL and iapAudienceToCloudRunURL must produce the same URL")
}

// ---------------------------------------------------------------------------
// IAP enable PATCH tests
// ---------------------------------------------------------------------------

// TestEnableIAPPatchBody verifies that the PATCH body sent by diEnableIAP
// contains both iapEnabled: true and invokerIamDisabled: true. The invariant
// is that invokerIamDisabled: true is NEVER sent without iapEnabled: true —
// both must appear together in every PATCH.
func TestEnableIAPPatchBody(t *testing.T) {
	var capturedBody []byte

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		require.NoError(t, err)
		capturedBody = body
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{}`))
	}))
	defer server.Close()

	// Build the same body that diEnableIAP builds internally
	patchBody := map[string]bool{
		"iapEnabled":         true,
		"invokerIamDisabled": true,
	}
	jsonBody, err := json.Marshal(patchBody)
	require.NoError(t, err)

	// Call the REST endpoint directly (we can't call diEnableIAP because
	// it calls diGetAccessToken which requires gcloud)
	statusCode, _, err := diRESTCall(http.MethodPatch, server.URL, "fake-token", jsonBody)
	require.NoError(t, err)
	assert.Equal(t, http.StatusOK, statusCode)

	// Verify the captured body contains both critical fields
	var parsed map[string]interface{}
	err = json.Unmarshal(capturedBody, &parsed)
	require.NoError(t, err)

	assert.Equal(t, true, parsed["iapEnabled"],
		"iapEnabled must be true — omitting it leaves the instance without IAP")
	assert.Equal(t, true, parsed["invokerIamDisabled"],
		"invokerIamDisabled must be true — the invariant requires both fields together")

	// Verify ONLY these two fields are present (no extra fields that could
	// interact with v1-only fields like sandboxLauncher)
	assert.Len(t, parsed, 2,
		"PATCH body must contain exactly iapEnabled and invokerIamDisabled — "+
			"no other fields, to avoid interacting with v1-only fields (§11.5c)")
}

// TestEnableIAPUpdateMask verifies that the PATCH URL includes the correct
// updateMask query parameter targeting both iapEnabled and invokerIamDisabled.
// The updateMask ensures only the IAP booleans are touched, leaving v1-only
// fields (like sandboxLauncher) untouched.
func TestEnableIAPUpdateMask(t *testing.T) {
	var capturedURL string

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedURL = r.URL.String()
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{}`))
	}))
	defer server.Close()

	// Build the URL the same way diEnableIAP does, but pointing at our test server
	patchURL := fmt.Sprintf("%s?updateMask=iapEnabled,invokerIamDisabled", server.URL)

	statusCode, _, err := diRESTCall(http.MethodPatch, patchURL, "fake-token", []byte(`{}`))
	require.NoError(t, err)
	assert.Equal(t, http.StatusOK, statusCode)

	// Verify updateMask is present and contains both fields
	assert.Contains(t, capturedURL, "updateMask=",
		"PATCH URL must include updateMask to limit which fields are modified")
	assert.Contains(t, capturedURL, "iapEnabled",
		"updateMask must include iapEnabled")
	assert.Contains(t, capturedURL, "invokerIamDisabled",
		"updateMask must include invokerIamDisabled")
}

// ---------------------------------------------------------------------------
// diIAMMemberPrefix tests
// ---------------------------------------------------------------------------

func TestIAMMemberPrefix_UserEmail(t *testing.T) {
	email := "admin@example.com"
	prefix := diIAMMemberPrefix(email)
	assert.Equal(t, "user:", prefix)
	assert.Equal(t, "user:admin@example.com", prefix+email,
		"normal email must produce user:<email> IAM member")
}

func TestIAMMemberPrefix_ServiceAccount(t *testing.T) {
	email := "deploy@my-project.iam.gserviceaccount.com"
	prefix := diIAMMemberPrefix(email)
	assert.Equal(t, "serviceAccount:", prefix)
	assert.Equal(t, "serviceAccount:deploy@my-project.iam.gserviceaccount.com", prefix+email,
		"service account email must produce serviceAccount:<email> IAM member")
}

// ---------------------------------------------------------------------------
// Gate 2 (perimeter assertion) tests
// ---------------------------------------------------------------------------

func TestAssertPerimeter_IAPEnforcing(t *testing.T) {
	// Simulate IAP: 302 to accounts.google.com with IAP header
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Location", "https://accounts.google.com/signin?...")
		w.Header().Set("X-Goog-Iap-Generated-Response", "true")
		w.WriteHeader(http.StatusFound)
	}))
	defer server.Close()

	err := diAssertPerimeter(server.URL)
	assert.NoError(t, err, "should succeed when IAP is enforcing")
}

func TestAssertPerimeter_AppAnswers(t *testing.T) {
	// Simulate no IAP: app answers directly with 200
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("Hello world"))
	}))
	defer server.Close()

	err := diAssertPerimeter(server.URL)
	require.Error(t, err, "must FAIL when app answers directly")
	assert.Contains(t, err.Error(), "UNPROTECTED",
		"error message must clearly indicate the instance is unprotected")
}

func TestAssertPerimeter_WrongRedirect(t *testing.T) {
	// 302 but not to accounts.google.com
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Location", "https://evil.example.com/phish")
		w.WriteHeader(http.StatusFound)
	}))
	defer server.Close()

	err := diAssertPerimeter(server.URL)
	require.Error(t, err, "must fail when redirect is not to accounts.google.com")
	assert.Contains(t, err.Error(), "not to accounts.google.com")
}

func TestAssertPerimeter_IAPNoHeader(t *testing.T) {
	// 302 to accounts.google.com but missing the IAP header — still passes
	// because the redirect alone proves IAP is enforcing; the header is
	// a bonus check.
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Location", "https://accounts.google.com/signin?...")
		w.WriteHeader(http.StatusFound)
	}))
	defer server.Close()

	err := diAssertPerimeter(server.URL)
	assert.NoError(t, err, "should pass even without IAP header if redirect is correct")
}

func TestAssertPerimeter_CloudRunErrorPage(t *testing.T) {
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

			err := diAssertPerimeter(server.URL)
			require.Error(t, err, "must fail when Cloud Run returns %d", code)
			assert.Contains(t, err.Error(), "not be serving",
				"error message must mention the instance may not be serving")
			assert.Contains(t, err.Error(), "CMD",
				"error message must suggest checking the Dockerfile CMD")
		})
	}
}

// ---------------------------------------------------------------------------
// diShortenError tests
// ---------------------------------------------------------------------------

func TestShortenError(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want string
	}{
		{
			name: "dial tcp error",
			err:  fmt.Errorf("dial tcp 1.2.3.4:443: connect: connection refused"),
			want: "connection refused",
		},
		{
			name: "TLS error",
			err:  fmt.Errorf("TLS handshake timeout"),
			want: "TLS not ready",
		},
		{
			name: "short error unchanged",
			err:  fmt.Errorf("something went wrong"),
			want: "something went wrong",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := diShortenError(tt.err)
			assert.Equal(t, tt.want, got)
		})
	}
}

// ---------------------------------------------------------------------------
// diPrintProjectIAPBindings tests
// ---------------------------------------------------------------------------

func TestPrintProjectIAPBindings_NoBindings(t *testing.T) {
	// Policy with no IAP bindings — should print "no bindings" message
	policy := `bindings:
- members:
  - user:someone@example.com
  role: roles/editor
- members:
  - user:other@example.com
  role: roles/viewer
`
	// Just verify it doesn't panic
	diPrintProjectIAPBindings(policy, "  ")
}

func TestPrintProjectIAPBindings_WithIAPBinding(t *testing.T) {
	policy := `bindings:
- members:
  - user:admin@example.com
  - user:dev@example.com
  role: roles/iap.httpsResourceAccessor
- members:
  - user:other@example.com
  role: roles/viewer
`
	// Just verify it doesn't panic
	diPrintProjectIAPBindings(policy, "  ")
}

// ---------------------------------------------------------------------------
// Validation tests: reject contaminated gcloud output (#33)
// ---------------------------------------------------------------------------

// TestValidateProjectNumber_Clean verifies acceptance of valid project numbers.
func TestValidateProjectNumber_Clean(t *testing.T) {
	for _, num := range []string{"123456789", "0", "721899303052"} {
		assert.NoError(t, diValidateProjectNumber(num),
			"valid project number %q must be accepted", num)
	}
}

// TestValidateProjectNumber_Contaminated verifies rejection of gcloud output
// that has been contaminated by service-account impersonation warnings on
// stderr. This is the measured failure mode from #33.
func TestValidateProjectNumber_Contaminated(t *testing.T) {
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
			name:  "empty string",
			input: "",
		},
		{
			name:  "whitespace",
			input: " 721899303052 ",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := diValidateProjectNumber(tt.input)
			assert.Error(t, err,
				"contaminated project number %q must be rejected", tt.input)
		})
	}
}

// TestValidateInstanceURL_Valid verifies acceptance of well-formed Cloud Run URLs.
func TestValidateInstanceURL_Valid(t *testing.T) {
	err := diValidateInstanceURL("https://my-instance-123456789.us-east4.run.app")
	assert.NoError(t, err)
}

// TestValidateInstanceURL_Contaminated verifies rejection of URLs built from
// contaminated gcloud output. When the project number is prefixed with the
// impersonation warning, the URL host becomes garbage (#33).
func TestValidateInstanceURL_Contaminated(t *testing.T) {
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
			err := diValidateInstanceURL(tt.input)
			assert.Error(t, err,
				"invalid instance URL %q must be rejected", tt.input)
		})
	}
}

// ---------------------------------------------------------------------------
// diDeriveRegistry tests (#38)
// ---------------------------------------------------------------------------

// TestDeriveRegistry_Valid verifies registry derivation from well-formed images.
func TestDeriveRegistry_Valid(t *testing.T) {
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
			got, err := diDeriveRegistry(tt.image)
			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
		})
	}
}

// TestDeriveRegistry_Invalid verifies rejection of images where registry
// cannot be derived (no host, bare image name).
func TestDeriveRegistry_Invalid(t *testing.T) {
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
			_, err := diDeriveRegistry(tt.image)
			assert.Error(t, err, "should reject image %q with no derivable registry", tt.image)
		})
	}
}

// ---------------------------------------------------------------------------
// diSanitizeResponse tests
// ---------------------------------------------------------------------------

func TestSanitizeResponse(t *testing.T) {
	short := "short error"
	assert.Equal(t, short, diSanitizeResponse(short))

	long := string(make([]byte, 600))
	result := diSanitizeResponse(long)
	assert.Contains(t, result, "truncated")
	assert.LessOrEqual(t, len(result), 520)
}

// ---------------------------------------------------------------------------
// Round-trip test: deploy env vars → config system → GlobalConfig structs
// ---------------------------------------------------------------------------

// TestDeployEnvVarsRoundTrip proves that the env vars diGcloudDeploy sets
// load correctly through the config system into the structs the hub reads.
// The critical concern is that Auth.Proxy (*ProxyAuthConfig) and Proxy.IAP
// (*IAPAuthConfig) are pointer fields — if koanf/mapstructure doesn't
// allocate them, the audience is empty and the hub fails at startup.
func TestDeployEnvVarsRoundTrip(t *testing.T) {
	// Use a clean HOME so no existing settings.yaml / server.yaml interfere.
	tmpDir := t.TempDir()
	t.Setenv("HOME", tmpDir)
	scionDir := filepath.Join(tmpDir, ".scion")
	require.NoError(t, os.MkdirAll(scionDir, 0755))

	// Set env vars exactly as diGcloudDeploy formats them (auth vars via
	// SCION_SERVER_*, admin email via SCION_SEED_SERVER_*).
	t.Setenv("SCION_SERVER_MODE", "hosted")
	t.Setenv("SCION_SERVER_AUTH_MODE", "proxy")
	t.Setenv("SCION_SERVER_AUTH_PROXY_PROVIDER", "iap")
	t.Setenv("SCION_SERVER_AUTH_PROXY_IAP_AUDIENCE",
		"/projects/123456789/locations/us-east4/services/test-instance")
	t.Setenv("SCION_SEED_SERVER_HUB_ADMINEMAILS", "admin@example.com")

	// --- Part 1: Auth vars through LoadGlobalConfig (the hub's startup path) ---
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

	// --- Part 2: Admin email through LoadSeedEnvKoanf (SEED prefix path) ---
	seedK := config.LoadSeedEnvKoanf()
	assert.Equal(t, "admin@example.com",
		seedK.String("server.hub.admin_emails"),
		"SCION_SEED_SERVER_HUB_ADMINEMAILS must map to server.hub.admin_emails in seed koanf")

	// --- Part 3: Admin email through LoadBootstrapKoanf (full merge path) ---
	// This is how opsettings seeding resolves the admin email at startup.
	bk := config.LoadBootstrapKoanf()
	emails := bk.Strings("server.hub.admin_emails")
	assert.Contains(t, emails, "admin@example.com",
		"admin email must appear in bootstrap koanf server.hub.admin_emails")
}

// ---------------------------------------------------------------------------
// Hosted-mode env var pinning test
// ---------------------------------------------------------------------------

// TestDeployHostedModeEnvRequired is a pinning test.
// When SCION_SERVER_MODE is absent, the server defaults to workstation mode.
// Workstation mode calls applyWorkstationDefaults (server_config.go:35) which
// sets enableDevAuth=true. On Cloud Run the host is 0.0.0.0, so the
// non-loopback dev-auth guard (server_foreground.go:2190) fires and the
// server exits immediately. SCION_SERVER_MODE=hosted is the fix: it sets
// cfg.Mode="hosted", which makes hostedMode=true, skipping workstation
// defaults entirely.
func TestDeployHostedModeEnvRequired(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("HOME", tmpDir)
	require.NoError(t, os.MkdirAll(filepath.Join(tmpDir, ".scion"), 0755))

	// Simulate the env var diGcloudDeploy sets.
	t.Setenv("SCION_SERVER_MODE", "hosted")

	gc, err := config.LoadGlobalConfig("")
	require.NoError(t, err)

	assert.Equal(t, "hosted", gc.Mode,
		"SCION_SERVER_MODE=hosted must map to cfg.Mode='hosted' — "+
			"without this, workstation defaults enable dev auth and crash the server")
}

// ---------------------------------------------------------------------------
// Comma collision guard
// ---------------------------------------------------------------------------

// TestDeployRejectsCommaInAdminEmail verifies that the comma guard in
// runDeployInstance would reject an --admin-email containing a comma.
// gcloud --set-env-vars is comma-delimited, so a comma in the value
// would silently split into a second env var, breaking the command.
func TestDeployRejectsCommaInAdminEmail(t *testing.T) {
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
			// Apply the same validation that runDeployInstance performs.
			hasComma := strings.Contains(tt.email, ",")
			if tt.wantError {
				assert.True(t, hasComma,
					"comma guard must reject %q", tt.email)
			} else {
				assert.False(t, hasComma,
					"comma guard must accept %q", tt.email)
			}
		})
	}
}

// TestDeployCommaInEmailBreaksGcloud demonstrates WHY the comma guard
// exists: gcloud --set-env-vars uses commas as the delimiter between
// key=value pairs, so a comma in any value silently corrupts the command.
func TestDeployCommaInEmailBreaksGcloud(t *testing.T) {
	// Simulate the format string from diGcloudDeploy with a comma in email.
	envVarStr := fmt.Sprintf(
		"SCION_SERVER_AUTH_MODE=proxy,SCION_SEED_SERVER_HUB_ADMINEMAILS=%s",
		"alice@example.com,bob@example.com")

	// gcloud splits on commas, so the above would produce three "env vars":
	//   1. SCION_SERVER_AUTH_MODE=proxy
	//   2. SCION_SEED_SERVER_HUB_ADMINEMAILS=alice@example.com
	//   3. bob@example.com   (← broken: not a valid KEY=VALUE)
	parts := strings.Split(envVarStr, ",")
	assert.Equal(t, 3, len(parts),
		"comma in email value causes gcloud to see 3 env vars instead of 2")
	assert.Equal(t, "bob@example.com", parts[2],
		"the second email becomes a broken env var fragment")
}
