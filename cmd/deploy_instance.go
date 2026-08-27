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
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os/exec"
	"regexp"
	"strings"
	"time"

	"github.com/spf13/cobra"
)

// deployInstanceCmd deploys a Cloud Run Instance with IAP protection.
// Implements the §11.5 deploy flow: resolve identity → resolve project number →
// create Instance (gcloud v1 for sandboxLauncher) → enable IAP (REST v2 PATCH) →
// gate (wait for IAP reconcile) → bind IAP policy → print effective access →
// gate (assert perimeter) → print URL.
var deployInstanceCmd = &cobra.Command{
	Use:   "deploy-instance",
	Short: "Deploy a Cloud Run Instance with IAP protection",
	Long: `Deploy a Cloud Run Instance with sandbox launcher support and IAP protection.

This command performs the complete deploy flow:
  1. Resolves the deploying operator's identity
  2. Resolves the GCP project number
  3a. Creates or updates the Cloud Run Instance via gcloud (v1 surface,
      required because sandboxLauncher is a v1-only field)
  3b. Enables IAP via REST v2 PATCH (iapEnabled + invokerIamDisabled are
      v2-only fields)
  4. Waits for IAP enforcement to become active (30-75s)
  5. Binds the IAP access policy for the operator
  6. Prints effective access (project-level and region-level)
  7. Asserts the IAP perimeter is enforcing (fails loudly if not)
  8. Prints the Instance URL

The command is idempotent: re-running converges without duplication.
iapEnabled and invokerIamDisabled are sent on EVERY write to prevent
silent perimeter drops.

NOTE: Container images from ghcr.io must be publicly accessible, or the
instance will fail to pull. If using a private ghcr.io package, configure
a pull secret or make the package public.`,
	RunE: runDeployInstance,
}

// Flags for deploy-instance
var (
	diName           string
	diImage          string
	diProject        string
	diRegion         string
	diAdminEmail     string
	diServiceAccount string
	diMemory         string
	diCPU            string
	diImageRegistry  string
)

func init() {
	rootCmd.AddCommand(deployInstanceCmd)

	deployInstanceCmd.Flags().StringVar(&diName, "name", "", "Instance name (required)")
	deployInstanceCmd.Flags().StringVar(&diImage, "image", "", "Container image (required; ensure ghcr.io packages are public)")
	deployInstanceCmd.Flags().StringVar(&diProject, "project", "", "GCP project ID (required)")
	deployInstanceCmd.Flags().StringVar(&diRegion, "region", "us-east4", "GCP region")
	deployInstanceCmd.Flags().StringVar(&diAdminEmail, "admin-email", "", "Override admin email (for CI service account deploys)")
	deployInstanceCmd.Flags().StringVar(&diServiceAccount, "service-account", "", "GCP service account for the instance")
	deployInstanceCmd.Flags().StringVar(&diMemory, "memory", "8Gi", "Memory limit")
	deployInstanceCmd.Flags().StringVar(&diCPU, "cpu", "4", "CPU limit")
	deployInstanceCmd.Flags().StringVar(&diImageRegistry, "image-registry", "", "Override image registry (default: derived from --image)")

	_ = deployInstanceCmd.MarkFlagRequired("name")
	_ = deployInstanceCmd.MarkFlagRequired("image")
	_ = deployInstanceCmd.MarkFlagRequired("project")
}

func runDeployInstance(cmd *cobra.Command, args []string) error {
	ctx := cmd.Context()

	// Step 1: Resolve identity
	fmt.Println("==> Step 1: Resolving deployer identity...")
	operatorEmail, err := diResolveIdentity(ctx)
	if err != nil {
		return fmt.Errorf("failed to resolve deployer identity: %w", err)
	}
	fmt.Printf("    Deployer: %s\n", operatorEmail)

	// Determine admin email: use --admin-email override if provided, otherwise operator
	adminEmail := operatorEmail
	if diAdminEmail != "" {
		adminEmail = diAdminEmail
		fmt.Printf("    Admin override: %s\n", adminEmail)
	}

	// Guard: gcloud --set-env-vars is comma-delimited. A comma in the email
	// would silently split into a second env var, breaking the command.
	if strings.Contains(adminEmail, ",") {
		return fmt.Errorf("--admin-email value %q contains a comma, which would break gcloud --set-env-vars", adminEmail)
	}

	// Step 2: Resolve project number
	fmt.Println("==> Step 2: Resolving project number...")
	projectNumber, err := diResolveProjectNumber(ctx, diProject)
	if err != nil {
		return fmt.Errorf("failed to resolve project number: %w", err)
	}
	fmt.Printf("    Project: %s (number: %s)\n", diProject, projectNumber)

	// Compute the IAP audience and instance URL.
	// NOTE: The audience uses "services" even though this is an Instance.
	// This is IAP's fixed resource vocabulary across every backend type.
	// Do NOT change to "instances" — see §11.3.
	iapAudience := fmt.Sprintf("/projects/%s/locations/%s/services/%s",
		projectNumber, diRegion, diName)
	instanceURL := fmt.Sprintf("https://%s-%s.%s.run.app",
		diName, projectNumber, diRegion)

	// Validate the computed URL before using it in gates and deploy.
	if err := diValidateInstanceURL(instanceURL); err != nil {
		return fmt.Errorf("computed instance URL is invalid (project number may be contaminated): %w", err)
	}

	// Resolve the image registry: --image-registry override, or derived from --image.
	// The broker requires SCION_IMAGE_REGISTRY to pull agent images
	// (requireImageRegistryForBroker in server_foreground.go).
	imageRegistry := diImageRegistry
	if imageRegistry == "" {
		imageRegistry, err = diDeriveRegistry(diImage)
		if err != nil {
			return fmt.Errorf("failed to derive image registry from --image %q: %w\n"+
				"Use --image-registry to set it explicitly", diImage, err)
		}
	}
	fmt.Printf("    Image registry: %s\n", imageRegistry)

	// Step 3a: Create/update the Instance via gcloud (v1 surface).
	// gcloud speaks v1, which is the only surface that has sandboxLauncher.
	// REST v2 neither sets nor returns sandboxLauncher — it returns 400 on
	// create and silently omits it on read.
	fmt.Println("==> Step 3a: Creating/updating Cloud Run Instance (gcloud, v1 surface)...")
	if err := diGcloudDeploy(ctx, diName, diImage, diProject, diRegion,
		diServiceAccount, diMemory, diCPU, iapAudience, adminEmail, imageRegistry); err != nil {
		return fmt.Errorf("failed to deploy instance via gcloud: %w", err)
	}
	fmt.Println("    Instance deployed successfully.")

	// Step 3b: Enable IAP via REST v2 PATCH.
	// iapEnabled and invokerIamDisabled are v2-only fields. gcloud has no
	// --iap flag, so we flip both booleans with a single REST PATCH.
	fmt.Println("==> Step 3b: Enabling IAP (REST v2 PATCH)...")
	if err := diEnableIAP(ctx, diName, diProject, diRegion); err != nil {
		return fmt.Errorf("failed to enable IAP: %w", err)
	}
	fmt.Println("    IAP enabled on instance.")

	// Step 4: Gate 1 — Wait for IAP reconcile.
	// 30-75s observed. The API returns before enforcement is live.
	fmt.Println("==> Step 4: Waiting for IAP enforcement to activate...")
	fmt.Println("    (This typically takes 30-75 seconds)")
	if err := diWaitForIAP(instanceURL); err != nil {
		return fmt.Errorf("IAP reconcile gate failed: %w", err)
	}
	fmt.Println("    IAP enforcement is active.")

	// Step 5: Bind IAP access policy at the region level.
	fmt.Println("==> Step 5: Binding IAP access policy...")
	if err := diBindIAPPolicy(ctx, diProject, diRegion, operatorEmail); err != nil {
		return fmt.Errorf("failed to bind IAP access policy: %w", err)
	}
	fmt.Printf("    IAP access granted to %s\n", operatorEmail)

	// If admin-email differs from operator, also bind for the admin
	if diAdminEmail != "" && diAdminEmail != operatorEmail {
		if err := diBindIAPPolicy(ctx, diProject, diRegion, diAdminEmail); err != nil {
			return fmt.Errorf("failed to bind IAP access policy for admin: %w", err)
		}
		fmt.Printf("    IAP access granted to %s\n", diAdminEmail)
	}

	// Step 6: Read back and print effective access.
	// Both project-level and region-level, because project-level grants
	// inherit invisibly and a tool that only prints its own writes misleads.
	fmt.Println("==> Step 6: Reading effective access...")
	if err := diPrintEffectiveAccess(ctx, diProject, diRegion); err != nil {
		fmt.Printf("    Warning: could not read effective access: %v\n", err)
	}

	// Step 7: Gate 2 — Assert the perimeter.
	// Fetch with NO credential. Require 302 to accounts.google.com with
	// x-goog-iap-generated-response: true. FAIL the deploy if the app answers.
	// This is the guard for the single point of failure (§11.1).
	fmt.Println("==> Step 7: Asserting IAP perimeter enforcement...")
	if err := diAssertPerimeter(instanceURL); err != nil {
		return fmt.Errorf("SECURITY FAILURE: IAP perimeter is NOT enforcing — %w", err)
	}
	fmt.Println("    IAP perimeter verified: unauthenticated requests are blocked.")
	fmt.Println("    Instance is serving and IAP-protected.")

	// Step 8: Print the URL
	fmt.Println()
	fmt.Println("=== Deploy Complete ===")
	fmt.Printf("Instance URL: %s\n", instanceURL)
	fmt.Printf("Admin email:  %s\n", adminEmail)
	fmt.Println()
	fmt.Println("Open the URL in a browser to log in. The deployer is seeded as admin.")

	return nil
}

// ---------------------------------------------------------------------------
// Step 1: Resolve identity
// ---------------------------------------------------------------------------

// diResolveIdentity runs `gcloud config get account` to get the deploying operator's email.
func diResolveIdentity(ctx context.Context) (string, error) {
	out, err := diRunGcloud(ctx, "config", "get", "account")
	if err != nil {
		return "", err
	}
	email := strings.TrimSpace(out)
	if email == "" {
		return "", fmt.Errorf("gcloud returned empty account — is gcloud configured?")
	}
	return email, nil
}

// ---------------------------------------------------------------------------
// Step 2: Resolve project number
// ---------------------------------------------------------------------------

// diResolveProjectNumber runs `gcloud projects describe` to get the project number.
func diResolveProjectNumber(ctx context.Context, project string) (string, error) {
	out, err := diRunGcloud(ctx, "projects", "describe", project,
		"--format=value(projectNumber)")
	if err != nil {
		return "", err
	}
	number := strings.TrimSpace(out)
	if number == "" {
		return "", fmt.Errorf("gcloud returned empty project number for %q", project)
	}
	if err := diValidateProjectNumber(number); err != nil {
		return "", err
	}
	return number, nil
}

// ---------------------------------------------------------------------------
// Step 3a: Create/update the Instance via gcloud (v1 surface)
// ---------------------------------------------------------------------------

// diGcloudDeploy creates or updates a Cloud Run Instance via gcloud beta.
// gcloud speaks v1, which is the ONLY surface that has sandboxLauncher.
// REST v2 POST with sandboxLauncher returns 400 "Unknown name"; REST v2 GET
// does not return it even when set. gcloud deploy is idempotent (create-or-update)
// and handles operation polling internally.
//
// IAP booleans (iapEnabled, invokerIamDisabled) are NOT set here — gcloud has
// no --iap flag. They are set in a separate REST v2 PATCH (diEnableIAP).
// The Instance is born with invoker check ON (default) — it is closed from
// birth, and the IAP PATCH follows immediately.
func diGcloudDeploy(ctx context.Context, name, image, project, region, serviceAccount, memory, cpu, iapAudience, adminEmail, imageRegistry string) error {
	args := []string{
		"beta", "run", "instances", "deploy", name,
		"--image", image,
		"--sandbox-launcher",
		"--region", region,
		"--project", project,
		"--set-env-vars",
		fmt.Sprintf("SCION_SERVER_MODE=hosted,SCION_SERVER_AUTH_MODE=proxy,SCION_SERVER_AUTH_PROXY_PROVIDER=iap,SCION_SERVER_AUTH_PROXY_IAP_AUDIENCE=%s,SCION_SERVER_HUB_ADMINEMAILS=%s,SCION_IMAGE_REGISTRY=%s",
			iapAudience, adminEmail, imageRegistry),
	}

	if serviceAccount != "" {
		args = append(args, "--service-account", serviceAccount)
	}
	if memory != "" {
		args = append(args, "--memory", memory)
	}
	if cpu != "" {
		args = append(args, "--cpu", cpu)
	}

	fmt.Printf("    gcloud %s\n", strings.Join(args[:6], " "))
	_, err := diRunGcloud(ctx, args...)
	return err
}

// ---------------------------------------------------------------------------
// Step 3b: Enable IAP via REST v2 PATCH
// ---------------------------------------------------------------------------

// diEnableIAP enables IAP on the Instance via a REST v2 PATCH.
//
// v1/v2 hazard (§11.5c):
//   - This PATCH uses v2 because iapEnabled and invokerIamDisabled are v2-only
//     fields; the v1 surface does not expose them.
//   - The Instance create (diGcloudDeploy) uses gcloud which speaks v1, because
//     sandboxLauncher is a v1-only field. REST v2 POST with sandboxLauncher
//     returns 400 "Unknown name".
//   - HAZARD: v2 silently omits sandboxLauncher rather than erroring on read.
//     Anything that round-trips an Instance through v2 will drop sandboxLauncher
//     without complaint. Never do a full-body v2 PUT/PATCH that was populated
//     from a v2 GET — it will silently un-set sandboxLauncher.
//   - This PATCH is safe because it uses updateMask to touch ONLY the IAP
//     booleans, leaving all v1-only fields untouched.
//
// Invariant: invokerIamDisabled: true is NEVER sent without iapEnabled: true
// in the same body. Both are sent together with a single updateMask.
func diEnableIAP(ctx context.Context, name, project, region string) error {
	token, err := diGetAccessToken()
	if err != nil {
		return fmt.Errorf("failed to get access token: %w", err)
	}

	patchBody := map[string]bool{
		"iapEnabled":         true,
		"invokerIamDisabled": true,
	}
	jsonBody, err := json.Marshal(patchBody)
	if err != nil {
		return fmt.Errorf("failed to marshal IAP patch body: %w", err)
	}

	patchURL := fmt.Sprintf(
		"https://%s-run.googleapis.com/v2/projects/%s/locations/%s/instances/%s?updateMask=iapEnabled,invokerIamDisabled",
		region, project, region, name)
	fmt.Printf("    PATCH %s\n", patchURL)

	statusCode, respBody, err := diRESTCall(ctx, http.MethodPatch, patchURL, token, jsonBody)
	if err != nil {
		return fmt.Errorf("REST PATCH failed: %w", err)
	}

	if statusCode >= 300 {
		return fmt.Errorf("REST PATCH returned %d: %s", statusCode, diSanitizeResponse(respBody))
	}

	return nil
}

// diGetAccessToken obtains an access token. It tries gcloud first, which
// works both on a developer workstation and in CI with the metadata server.
// NEVER prints the token to stdout.
func diGetAccessToken() (string, error) {
	cmd := exec.Command("gcloud", "auth", "print-access-token")
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("gcloud auth print-access-token: %w", err)
	}
	token := strings.TrimSpace(string(out))
	if token == "" {
		return "", fmt.Errorf("gcloud returned empty access token")
	}
	return token, nil
}

// diRESTCall performs an authenticated HTTP request to the Cloud Run v2 API.
func diRESTCall(ctx context.Context, method, url, token string, body []byte) (int, string, error) {
	var bodyReader io.Reader
	if body != nil {
		bodyReader = bytes.NewReader(body)
	}

	req, err := http.NewRequestWithContext(ctx, method, url, bodyReader)
	if err != nil {
		return 0, "", err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")

	client := &http.Client{Timeout: 120 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return 0, "", err
	}
	defer func() { _ = resp.Body.Close() }()

	respBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		return resp.StatusCode, "", err
	}

	return resp.StatusCode, string(respBytes), nil
}

// ---------------------------------------------------------------------------
// Step 4: Gate 1 — Wait for IAP reconcile
// ---------------------------------------------------------------------------

// diWaitForIAP polls the instance URL with an unauthenticated HTTP client
// (no credentials, no redirect following) until IAP responds with a 302
// to accounts.google.com. The API returns before enforcement is live;
// proceeding before this gate passes is the failure this tier cannot have.
func diWaitForIAP(instanceURL string) error {
	client := diNoAuthClient()

	maxWait := 3 * time.Minute
	pollInterval := 5 * time.Second
	deadline := time.Now().Add(maxWait)

	// Track last-seen response for diagnostic on timeout.
	var lastStatus int
	var lastErr string

	for time.Now().Before(deadline) {
		resp, err := client.Get(instanceURL)
		if err != nil {
			lastStatus = 0
			lastErr = diShortenError(err)
			fmt.Printf("    Polling... (not ready yet: %v)\n", lastErr)
			time.Sleep(pollInterval)
			continue
		}
		_ = resp.Body.Close()

		if resp.StatusCode == http.StatusFound {
			location := resp.Header.Get("Location")
			if strings.Contains(location, "accounts.google.com") {
				return nil // IAP is enforcing
			}
		}

		lastStatus = resp.StatusCode
		lastErr = ""
		fmt.Printf("    Polling... (status %d, waiting for IAP 302)\n", resp.StatusCode)
		time.Sleep(pollInterval)
	}

	// Build a diagnostic message that helps distinguish "IAP is slow" from
	// "the Instance is dead" (wrong port, crash loop, missing binary).
	diag := fmt.Sprintf("timed out after %v waiting for IAP to enforce on %s", maxWait, instanceURL)
	if lastErr != "" {
		diag += fmt.Sprintf(" — last probe: %s (instance may not be serving on port 8080)", lastErr)
	} else if lastStatus == http.StatusBadGateway || lastStatus == http.StatusServiceUnavailable {
		diag += fmt.Sprintf(" — last seen: %d (instance may not be serving — check CMD, port, and container health)", lastStatus)
	} else if lastStatus != 0 {
		diag += fmt.Sprintf(" — last seen: HTTP %d", lastStatus)
	}
	return fmt.Errorf("%s", diag)
}

// ---------------------------------------------------------------------------
// Step 5: Bind IAP access policy
// ---------------------------------------------------------------------------

// diIAMMemberPrefix returns the correct IAM member prefix for the given email.
// Service accounts (ending in .gserviceaccount.com) use "serviceAccount:";
// all other emails use "user:". The --admin-email flag is documented for CI
// service account deploys, so this must handle both forms.
func diIAMMemberPrefix(email string) string {
	if strings.HasSuffix(email, ".gserviceaccount.com") {
		return "serviceAccount:"
	}
	return "user:"
}

// diBindIAPPolicy binds roles/iap.httpsResourceAccessor for the given email
// at the REGION level. Per §11.2, there is no per-Instance IAP path — a
// per-Instance request returns 404.
func diBindIAPPolicy(ctx context.Context, project, region, email string) error {
	_, err := diRunGcloud(ctx, "iap", "web", "add-iam-policy-binding",
		"--project="+project,
		"--region="+region,
		"--resource-type=cloud-run",
		"--member="+diIAMMemberPrefix(email)+email,
		"--role=roles/iap.httpsResourceAccessor",
	)
	return err
}

// ---------------------------------------------------------------------------
// Step 6: Print effective access
// ---------------------------------------------------------------------------

// diPrintEffectiveAccess reads and prints both region-level and project-level
// IAP bindings. This is critical: project-level grants inherit invisibly and
// do not appear in resource policies. A tool that only prints what it wrote
// would actively mislead an operator auditing access (§11.2).
func diPrintEffectiveAccess(ctx context.Context, project, region string) error {
	// Region-level IAP bindings
	fmt.Println("    --- Region-level IAP bindings ---")
	regionOut, err := diRunGcloud(ctx, "iap", "web", "get-iam-policy",
		"--project="+project,
		"--region="+region,
		"--resource-type=cloud-run",
	)
	if err != nil {
		fmt.Printf("    Warning: could not read region-level IAP policy: %v\n", err)
	} else {
		diPrintIndented(regionOut, "    ")
	}

	// Project-level bindings filtered for iap.httpsResourceAccessor
	fmt.Println("    --- Project-level IAP bindings (inherited) ---")
	projectOut, err := diRunGcloud(ctx, "projects", "get-iam-policy", project,
		"--format=yaml",
	)
	if err != nil {
		fmt.Printf("    Warning: could not read project-level IAM policy: %v\n", err)
	} else {
		diPrintProjectIAPBindings(projectOut, "    ")
	}

	return nil
}

// diPrintIndented prints non-empty lines with a prefix.
func diPrintIndented(output, indent string) {
	trimmed := strings.TrimSpace(output)
	if trimmed == "" {
		fmt.Printf("%s(no bindings)\n", indent)
		return
	}
	for _, line := range strings.Split(output, "\n") {
		if strings.TrimSpace(line) != "" {
			fmt.Printf("%s%s\n", indent, line)
		}
	}
}

// diPrintProjectIAPBindings filters a project IAM policy YAML to show only
// roles/iap.httpsResourceAccessor bindings.
//
// GCP IAM policy YAML has this structure:
//
//	bindings:
//	- members:
//	  - user:admin@example.com
//	  role: roles/iap.httpsResourceAccessor
//	- members:
//	  - user:other@example.com
//	  role: roles/viewer
//
// We scan for the role line, then collect the members from the same binding.
// Because role: may appear before or after members in a binding, we collect
// the entire binding first, then decide whether to print it.
func diPrintProjectIAPBindings(policyOutput, indent string) {
	lines := strings.Split(policyOutput, "\n")
	found := false

	var currentBinding []string
	hasIAPRole := false

	flushBinding := func() {
		if hasIAPRole && len(currentBinding) > 0 {
			found = true
			for _, l := range currentBinding {
				if strings.TrimSpace(l) != "" {
					fmt.Printf("%s%s\n", indent, l)
				}
			}
		}
		currentBinding = nil
		hasIAPRole = false
	}

	for _, line := range lines {
		trimmed := strings.TrimSpace(line)

		// A new binding starts with "- members:" or "- role:"
		if strings.HasPrefix(trimmed, "- members:") || strings.HasPrefix(trimmed, "- role:") {
			flushBinding()
		}

		if strings.HasPrefix(trimmed, "role:") &&
			strings.Contains(trimmed, "iap.httpsResourceAccessor") {
			hasIAPRole = true
		}

		// Skip the top-level "bindings:" key
		if trimmed == "bindings:" {
			continue
		}

		currentBinding = append(currentBinding, line)
	}
	flushBinding()

	if !found {
		fmt.Printf("%s(no project-level iap.httpsResourceAccessor bindings)\n", indent)
	}
}

// ---------------------------------------------------------------------------
// Step 7: Gate 2 — Assert the perimeter (MOST VALUABLE DELIVERABLE)
// ---------------------------------------------------------------------------

// diAssertPerimeter fetches the instance URL with NO credential and requires
// a 302 to accounts.google.com with x-goog-iap-generated-response: true.
// FAILS the deploy loudly if the app answers — this is the guard for the
// single point of failure (§11.1). With invoker IAM disabled, iapEnabled=false
// leaves the Instance open to the internet with nothing but hub session auth.
//
// This gate doubles as the post-deploy smoke check: if the Instance is dead
// (wrong port, crash loop, missing binary, bad CMD), Cloud Run returns its
// own error (502/503), NOT the IAP 302. So a passing gate 2 proves both
// that IAP is enforcing AND that the Instance is serving behind it.
func diAssertPerimeter(instanceURL string) error {
	client := diNoAuthClient()

	resp, err := client.Get(instanceURL)
	if err != nil {
		return fmt.Errorf("could not reach instance URL %s: %w", instanceURL, err)
	}
	defer func() { _ = resp.Body.Close() }()

	// Require a 302 to accounts.google.com
	if resp.StatusCode != http.StatusFound {
		// Distinguish "IAP not enforcing" (200 = app answered) from
		// "Instance is dead" (502/503 = Cloud Run error page).
		if resp.StatusCode == http.StatusBadGateway || resp.StatusCode == http.StatusServiceUnavailable {
			return fmt.Errorf(
				"expected 302 redirect but got %d — the instance may not be serving "+
					"(check Dockerfile CMD, port configuration, and container logs). "+
					"Cloud Run returns %d when the container is unhealthy or not listening on port 8080",
				resp.StatusCode, resp.StatusCode)
		}
		return fmt.Errorf(
			"expected 302 redirect but got %d — IAP may not be enforcing! "+
				"An unauthenticated request reached the app, which means "+
				"the instance is UNPROTECTED", resp.StatusCode)
	}

	location := resp.Header.Get("Location")
	if !strings.Contains(location, "accounts.google.com") {
		return fmt.Errorf(
			"got 302 but not to accounts.google.com (Location: %s) — "+
				"IAP may not be enforcing", location)
	}

	// Check for x-goog-iap-generated-response header
	iapHeader := resp.Header.Get("X-Goog-Iap-Generated-Response")
	if !strings.EqualFold(iapHeader, "true") {
		fmt.Printf("    Warning: x-goog-iap-generated-response header "+
			"not found or not 'true' (value: %q)\n", iapHeader)
		fmt.Println("    The 302 to accounts.google.com is present, " +
			"so IAP appears active.")
	}

	return nil
}

// ---------------------------------------------------------------------------
// Shared helpers
// ---------------------------------------------------------------------------

// diNoAuthClient returns an HTTP client with no credentials and no redirect
// following. Used for both IAP reconcile polling (step 4) and perimeter
// assertion (step 7).
func diNoAuthClient() *http.Client {
	return &http.Client{
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse
		},
		Timeout: 15 * time.Second,
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS12},
		},
	}
}

// diRunGcloud executes a gcloud command and returns its stdout.
// Stderr is discarded on success and included in the error on failure.
//
// Why stdout-only: gcloud writes diagnostic messages (notably the
// "WARNING: This command is using service account impersonation..."
// banner) to stderr. CombinedOutput mixes those into data values like
// project numbers and URLs, corrupting downstream consumers (#33).
func diRunGcloud(ctx context.Context, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "gcloud", args...)
	out, err := cmd.Output() // stdout only; stderr via ExitError
	if err != nil {
		// Limit how much of args we show to avoid leaking tokens
		showArgs := args
		if len(showArgs) > 4 {
			showArgs = showArgs[:4]
		}
		// Extract stderr from ExitError for diagnostic context.
		var exitErr *exec.ExitError
		stderrMsg := ""
		if errors.As(err, &exitErr) && len(exitErr.Stderr) > 0 {
			stderrMsg = "\n" + string(exitErr.Stderr)
		}
		return "", fmt.Errorf("gcloud %s: %w%s",
			strings.Join(showArgs, " "), err, stderrMsg)
	}
	return string(out), nil
}

// diSanitizeResponse removes potential access tokens from API response text
// before including it in error messages.
func diSanitizeResponse(resp string) string {
	if len(resp) > 500 {
		return resp[:500] + "... (truncated)"
	}
	return resp
}

// diShortenError shortens common network error messages for cleaner output.
func diShortenError(err error) string {
	msg := err.Error()
	if strings.Contains(msg, "dial tcp") {
		return "connection refused"
	}
	if strings.Contains(msg, "TLS handshake") {
		return "TLS not ready"
	}
	if len(msg) > 80 {
		return msg[:80] + "..."
	}
	return msg
}

// projectNumberRe validates that a string is a pure numeric GCP project number.
var projectNumberRe = regexp.MustCompile(`^[0-9]+$`)

// diValidateProjectNumber rejects anything that is not a pure numeric string.
// gcloud under service-account impersonation prepends a WARNING to stderr; if
// stderr leaks into the captured value, the project number becomes
// "WARNING: This command is using...721899303052" and every consumer silently
// embeds garbage (#33).
func diValidateProjectNumber(s string) error {
	if !projectNumberRe.MatchString(s) {
		return fmt.Errorf("project number %q is not purely numeric — "+
			"gcloud output may be contaminated (stderr mixed into stdout?)", s)
	}
	return nil
}

// diValidateInstanceURL rejects anything that does not parse as an https URL
// with a plausible Cloud Run host.
func diValidateInstanceURL(rawURL string) error {
	u, err := url.Parse(rawURL)
	if err != nil {
		return fmt.Errorf("could not parse instance URL %q: %w", rawURL, err)
	}
	if u.Scheme != "https" {
		return fmt.Errorf("instance URL %q has scheme %q, expected https", rawURL, u.Scheme)
	}
	if u.Host == "" {
		return fmt.Errorf("instance URL %q has no host", rawURL)
	}
	if !strings.HasSuffix(u.Host, ".run.app") {
		return fmt.Errorf("instance URL host %q does not end with .run.app", u.Host)
	}
	return nil
}

// diDeriveRegistry extracts the registry prefix from a container image
// reference. For example:
//
//	ghcr.io/ptone/scion-omni:latest  → ghcr.io/ptone
//	ghcr.io/ptone/scion-omni@sha256:abc  → ghcr.io/ptone
//	ghcr.io/ptone/scion-omni  → ghcr.io/ptone
//
// The registry is everything before the last path component (the image
// name), after stripping any tag or digest suffix. Returns an error when
// the image has no registry host (e.g. bare "nginx:latest").
func diDeriveRegistry(image string) (string, error) {
	// Strip tag (:...) or digest (@sha256:...).
	ref := image
	if idx := strings.Index(ref, "@"); idx != -1 {
		ref = ref[:idx]
	}
	if idx := strings.LastIndex(ref, ":"); idx != -1 {
		// Only strip if the colon is after the last slash (it's a tag,
		// not a port in the registry host like localhost:5000/img).
		if slashIdx := strings.LastIndex(ref, "/"); idx > slashIdx {
			ref = ref[:idx]
		}
	}

	// Split into path components. The last component is the image name;
	// everything before it is the registry.
	slashIdx := strings.LastIndex(ref, "/")
	if slashIdx <= 0 {
		return "", fmt.Errorf("cannot derive registry from image %q — "+
			"expected host/org/name format (e.g. ghcr.io/ptone/scion-omni)", image)
	}

	registry := ref[:slashIdx]

	// Sanity check: registry must contain a dot (hostname) or colon (port).
	if !strings.Contains(registry, ".") && !strings.Contains(registry, ":") {
		return "", fmt.Errorf("derived registry %q from image %q does not look like a "+
			"hostname — use --image-registry to override", registry, image)
	}

	return registry, nil
}

// BuildInstanceURL computes the legacy-format Cloud Run Instance URL.
// Exported for testing.
func BuildInstanceURL(name, projectNumber, region string) string {
	return fmt.Sprintf("https://%s-%s.%s.run.app", name, projectNumber, region)
}

// BuildIAPAudience computes the IAP audience path for a Cloud Run Instance.
// NOTE: Uses "services" (not "instances") — this is IAP's fixed vocabulary.
// See §11.3 of cloudrun-instances-sandboxes.md.
// Exported for testing.
func BuildIAPAudience(projectNumber, region, name string) string {
	return fmt.Sprintf("/projects/%s/locations/%s/services/%s",
		projectNumber, region, name)
}
