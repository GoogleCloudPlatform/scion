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
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	goruntime "runtime"
	"strings"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/brokercredentials"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
	scionruntime "github.com/GoogleCloudPlatform/scion/pkg/runtime"
	"github.com/GoogleCloudPlatform/scion/pkg/util"
	"github.com/spf13/cobra"
)

var doctorCmd = &cobra.Command{
	Use:   "doctor",
	Short: "Check system prerequisites and runtime configuration",
	Long: `Run diagnostic checks to verify that your system is properly configured
for running scion agents. Checks include runtime availability, connectivity,
permissions, and required dependencies.

For Kubernetes runtimes, this includes cluster connectivity, namespace access,
RBAC permissions, and CSI driver availability.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		return runDoctor()
	},
}

func init() {
	rootCmd.AddCommand(doctorCmd)
}

func runDoctor() error {
	fmt.Printf("%sScion Doctor%s\n\n", util.Bold, util.Reset)

	// General checks
	fmt.Printf("%sGeneral%s\n", util.Bold, util.Reset)
	checkGit()
	checkTmux()

	// Hub Health checks
	fmt.Printf("\n%sHub Health%s\n", util.Bold, util.Reset)
	var hubChecks []scionruntime.CheckResult

	// Resolve hub endpoint
	hubEP := hubEndpoint
	if hubEP == "" {
		hubEP = os.Getenv("SCION_HUB_ENDPOINT")
	}
	if hubEP == "" {
		hubEP = os.Getenv("SCION_HUB_URL")
	}

	// D1: Hub Connectivity
	d1 := checkDoctorHubConnectivity(hubEP)
	hubChecks = append(hubChecks, d1)
	printCheck(d1.Name, d1.Status, d1.Message, d1.Remediation)

	hubConnected := d1.Status == "pass" || d1.Status == "warn"

	// Create authenticated client for D2-D5 if Hub is reachable
	var hubClient hubclient.Client
	if hubEP != "" && hubConnected {
		settings, _ := config.LoadSettings(projectPath)
		if c, err := getHubClient(settings); err == nil {
			hubClient = c
		}
	}

	// D2: Hub Authentication
	d2 := checkDoctorHubAuth(hubEP, hubConnected, hubClient)
	hubChecks = append(hubChecks, d2)
	printCheck(d2.Name, d2.Status, d2.Message, d2.Remediation)

	// D3: Broker Connectivity
	d3 := checkDoctorBrokerConnectivity(hubEP, hubConnected, hubClient)
	hubChecks = append(hubChecks, d3)
	printCheck(d3.Name, d3.Status, d3.Message, d3.Remediation)

	// D4: Agent Health Summary
	d4 := checkDoctorAgentHealth(hubEP, hubConnected, hubClient)
	hubChecks = append(hubChecks, d4)
	printCheck(d4.Name, d4.Status, d4.Message, d4.Remediation)

	// D5: NFS Mount Status
	d5 := checkDoctorNFSMounts(hubEP, hubConnected, hubClient)
	hubChecks = append(hubChecks, d5)
	printCheck(d5.Name, d5.Status, d5.Message, d5.Remediation)

	// D6: Telemetry Pipeline (local check)
	d6 := checkDoctorTelemetry()
	hubChecks = append(hubChecks, d6)
	printCheck(d6.Name, d6.Status, d6.Message, d6.Remediation)

	// D7: Container Images (local check)
	d7 := checkDoctorContainerImages()
	hubChecks = append(hubChecks, d7)
	printCheck(d7.Name, d7.Status, d7.Message, d7.Remediation)

	// D8: Credential Validity (local check)
	d8 := checkDoctorCredentialValidity()
	hubChecks = append(hubChecks, d8)
	printCheck(d8.Name, d8.Status, d8.Message, d8.Remediation)

	// Resolve the active runtime
	fmt.Printf("\n%sRuntime%s\n", util.Bold, util.Reset)

	resolved, err := resolveActiveProjectPath()
	if err != nil {
		printCheck("project", "warn", "No project found — skipping runtime checks", "Run 'scion init' to create a project.")
		if outputFormat == "json" {
			return outputDoctorJSON(nil, hubChecks)
		}
		return nil
	}

	rt := scionruntime.GetRuntime(resolved, profile)
	rtName := rt.Name()
	printCheck("runtime", "pass", fmt.Sprintf("Active runtime: %s", rtName), "")

	// Runtime-specific diagnostics
	if diag, ok := rt.(scionruntime.Diagnosable); ok {
		// Load settings for runtime config
		var namespace string
		var gkeMode bool
		projectDir, _ := config.GetResolvedProjectDir(resolved)
		if vs, _, err := config.LoadEffectiveSettings(projectDir); err == nil && vs != nil {
			rtConfig, _, rtErr := vs.ResolveRuntime(profile)
			if rtErr == nil {
				namespace = rtConfig.Namespace
				gkeMode = rtConfig.GKE
			}
		}

		opts := scionruntime.DiagnosticOpts{
			Namespace: namespace,
			GKEMode:   gkeMode,
		}

		fmt.Printf("\n%sRuntime Diagnostics (%s)%s\n", util.Bold, rtName, util.Reset)
		report := diag.RunDiagnostics(opts)

		if outputFormat == "json" {
			return outputDoctorJSON(&report, hubChecks)
		}

		for _, check := range report.Checks {
			printCheck(check.Name, check.Status, check.Message, check.Remediation)
		}

		// Summary
		fmt.Println()
		passes, warns, fails := 0, 0, 0
		for _, c := range report.Checks {
			switch c.Status {
			case "pass":
				passes++
			case "warn":
				warns++
			case "fail":
				fails++
			}
		}

		if fails > 0 {
			fmt.Printf("%s%d checks passed, %d warnings, %d failures%s\n",
				util.Red, passes, warns, fails, util.Reset)
			return fmt.Errorf("%d diagnostic check(s) failed", fails)
		}
		if warns > 0 {
			fmt.Printf("%s%d checks passed, %d warnings%s\n",
				util.Yellow, passes, warns, util.Reset)
		} else {
			fmt.Printf("%s%d checks passed%s\n",
				util.Green, passes, util.Reset)
		}
	} else {
		// Non-diagnosable runtimes get basic checks
		switch rtName {
		case "docker":
			checkDockerOrPodman("docker")
		case "podman":
			checkDockerOrPodman("podman")
		case "container":
			checkContainerCLI()
		}
	}

	return nil
}

func resolveActiveProjectPath() (string, error) {
	if projectPath != "" {
		return projectPath, nil
	}
	resolved, _, err := config.RequireProjectPath("")
	if err != nil {
		return "", err
	}
	return resolved, nil
}

func checkGit() {
	path, err := exec.LookPath("git")
	if err != nil {
		printCheck("git", "fail", "git not found in PATH", "Install git: https://git-scm.com/downloads")
		return
	}
	out, err := exec.Command("git", "--version").Output()
	if err != nil {
		printCheck("git", "warn", fmt.Sprintf("git found at %s but version check failed", path), "")
		return
	}
	printCheck("git", "pass", trimNewline(string(out)), "")
}

func checkTmux() {
	_, err := exec.LookPath("tmux")
	if err != nil {
		if goruntime.GOOS == "darwin" || goruntime.GOOS == "linux" {
			printCheck("tmux", "warn", "tmux not found locally (required inside agent containers)", "")
		} else {
			printCheck("tmux", "skip", "tmux check skipped on this platform", "")
		}
		return
	}
	out, err := exec.Command("tmux", "-V").Output()
	if err != nil {
		printCheck("tmux", "pass", "tmux found", "")
		return
	}
	printCheck("tmux", "pass", trimNewline(string(out)), "")
}

func checkDockerOrPodman(name string) {
	_, err := exec.LookPath(name)
	if err != nil {
		printCheck(name, "fail", fmt.Sprintf("%s not found in PATH", name), fmt.Sprintf("Install %s", name))
		return
	}
	out, err := exec.Command(name, "--version").Output()
	if err != nil {
		printCheck(name, "warn", fmt.Sprintf("%s found but version check failed", name), "")
		return
	}
	printCheck(name, "pass", trimNewline(string(out)), "")

	// Check daemon connectivity
	_, err = exec.Command(name, "info").Output()
	if err != nil {
		printCheck(name+"-daemon", "fail",
			fmt.Sprintf("%s daemon is not running or not accessible", name),
			fmt.Sprintf("Start the %s daemon and try again.", name))
		return
	}
	printCheck(name+"-daemon", "pass", fmt.Sprintf("%s daemon is running", name), "")
}

func checkContainerCLI() {
	if goruntime.GOOS != "darwin" {
		printCheck("container", "skip", "Apple container CLI is only available on macOS", "")
		return
	}
	_, err := exec.LookPath("container")
	if err != nil {
		printCheck("container", "fail", "container CLI not found in PATH", "Install the container CLI for macOS")
		return
	}
	printCheck("container", "pass", "container CLI found", "")
}

func printCheck(name, status, message, remediation string) {
	var icon string
	switch status {
	case "pass":
		icon = fmt.Sprintf("%s✓%s", util.Green, util.Reset)
	case "warn":
		icon = fmt.Sprintf("%s!%s", util.Yellow, util.Reset)
	case "fail":
		icon = fmt.Sprintf("%s✗%s", util.Red, util.Reset)
	case "skip":
		icon = fmt.Sprintf("%s-%s", util.Gray, util.Reset)
	}
	fmt.Printf("  %s %s: %s\n", icon, name, message)
	if remediation != "" && status != "pass" {
		fmt.Printf("    → %s\n", remediation)
	}
}

func outputDoctorJSON(report *scionruntime.DiagnosticReport, hubChecks []scionruntime.CheckResult) error {
	out := map[string]interface{}{}
	if report != nil {
		out["runtime"] = report.Runtime
		out["checks"] = report.Checks
	}
	if len(hubChecks) > 0 {
		out["hubChecks"] = hubChecks
	}
	data, err := json.MarshalIndent(out, "", "  ")
	if err != nil {
		return err
	}
	_, _ = fmt.Fprintln(os.Stdout, string(data))
	return nil
}

// checkDoctorHubConnectivity performs D1: Hub connectivity check via /healthz.
func checkDoctorHubConnectivity(hubEP string) scionruntime.CheckResult {
	if hubEP == "" {
		return scionruntime.CheckResult{
			Name:        "hub-connectivity",
			Status:      "skip",
			Message:     "No Hub endpoint configured",
			Remediation: "Set SCION_HUB_ENDPOINT or use --hub flag",
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, hubEP+"/healthz", nil)
	if err != nil {
		return scionruntime.CheckResult{
			Name:        "hub-connectivity",
			Status:      "fail",
			Message:     fmt.Sprintf("Invalid Hub endpoint: %v", err),
			Remediation: "Check the Hub endpoint URL",
		}
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return scionruntime.CheckResult{
			Name:        "hub-connectivity",
			Status:      "fail",
			Message:     fmt.Sprintf("Cannot reach Hub at %s: %v", hubEP, err),
			Remediation: "Verify the Hub is running and the endpoint is correct",
		}
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return scionruntime.CheckResult{
			Name:        "hub-connectivity",
			Status:      "fail",
			Message:     fmt.Sprintf("Hub returned HTTP %d", resp.StatusCode),
			Remediation: "Check Hub server logs",
		}
	}

	var healthResp struct {
		Status string `json:"status"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&healthResp); err != nil {
		return scionruntime.CheckResult{
			Name:    "hub-connectivity",
			Status:  "warn",
			Message: "Hub reachable but health response could not be parsed",
		}
	}

	if healthResp.Status == "degraded" {
		return scionruntime.CheckResult{
			Name:    "hub-connectivity",
			Status:  "warn",
			Message: fmt.Sprintf("Hub at %s is degraded", hubEP),
		}
	}

	return scionruntime.CheckResult{
		Name:    "hub-connectivity",
		Status:  "pass",
		Message: fmt.Sprintf("Hub at %s is healthy", hubEP),
	}
}

// checkDoctorHubAuth performs D2: Hub authentication check.
func checkDoctorHubAuth(hubEP string, hubConnected bool, client hubclient.Client) scionruntime.CheckResult {
	if hubEP == "" {
		return scionruntime.CheckResult{
			Name:        "hub-auth",
			Status:      "skip",
			Message:     "No Hub endpoint configured",
			Remediation: "Set SCION_HUB_ENDPOINT or use --hub flag",
		}
	}
	if !hubConnected {
		return scionruntime.CheckResult{
			Name:    "hub-auth",
			Status:  "skip",
			Message: "Skipped (Hub connectivity check failed)",
		}
	}
	if client == nil {
		return scionruntime.CheckResult{
			Name:        "hub-auth",
			Status:      "fail",
			Message:     "Failed to create authenticated Hub client",
			Remediation: "Run 'scion hub auth login' to authenticate",
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	_, err := client.Auth().Me(ctx)
	if err != nil {
		return scionruntime.CheckResult{
			Name:        "hub-auth",
			Status:      "fail",
			Message:     fmt.Sprintf("Authentication failed: %v", err),
			Remediation: "Run 'scion hub auth login' to authenticate",
		}
	}

	return scionruntime.CheckResult{
		Name:    "hub-auth",
		Status:  "pass",
		Message: "Authenticated with Hub",
	}
}

// checkDoctorBrokerConnectivity performs D3: Broker connectivity check.
func checkDoctorBrokerConnectivity(hubEP string, hubConnected bool, client hubclient.Client) scionruntime.CheckResult {
	if hubEP == "" {
		return scionruntime.CheckResult{
			Name:        "broker-connectivity",
			Status:      "skip",
			Message:     "No Hub endpoint configured",
			Remediation: "Set SCION_HUB_ENDPOINT or use --hub flag",
		}
	}
	if !hubConnected {
		return scionruntime.CheckResult{
			Name:    "broker-connectivity",
			Status:  "skip",
			Message: "Skipped (Hub unreachable)",
		}
	}
	if client == nil {
		return scionruntime.CheckResult{
			Name:    "broker-connectivity",
			Status:  "skip",
			Message: "Skipped (Hub client not available)",
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	resp, err := client.RuntimeBrokers().List(ctx, nil)
	if err != nil {
		return scionruntime.CheckResult{
			Name:        "broker-connectivity",
			Status:      "fail",
			Message:     fmt.Sprintf("Failed to query brokers: %v", err),
			Remediation: "Check Hub server logs",
		}
	}

	if len(resp.Brokers) == 0 {
		return scionruntime.CheckResult{
			Name:        "broker-connectivity",
			Status:      "fail",
			Message:     "No runtime brokers registered",
			Remediation: "Register a broker with 'scion server start'",
		}
	}

	onlineCount := 0
	for _, broker := range resp.Brokers {
		if broker.Status == "online" {
			onlineCount++
		}
	}

	if onlineCount == 0 {
		return scionruntime.CheckResult{
			Name:        "broker-connectivity",
			Status:      "fail",
			Message:     fmt.Sprintf("%d broker(s) registered but none online", len(resp.Brokers)),
			Remediation: "Start a broker or check broker connectivity",
		}
	}

	return scionruntime.CheckResult{
		Name:    "broker-connectivity",
		Status:  "pass",
		Message: fmt.Sprintf("%d/%d broker(s) online", onlineCount, len(resp.Brokers)),
	}
}

// checkDoctorAgentHealth performs D4: Agent health summary check.
func checkDoctorAgentHealth(hubEP string, hubConnected bool, client hubclient.Client) scionruntime.CheckResult {
	if hubEP == "" {
		return scionruntime.CheckResult{
			Name:        "agent-health",
			Status:      "skip",
			Message:     "No Hub endpoint configured",
			Remediation: "Set SCION_HUB_ENDPOINT or use --hub flag",
		}
	}
	if !hubConnected {
		return scionruntime.CheckResult{
			Name:    "agent-health",
			Status:  "skip",
			Message: "Skipped (Hub unreachable)",
		}
	}
	if client == nil {
		return scionruntime.CheckResult{
			Name:    "agent-health",
			Status:  "skip",
			Message: "Skipped (Hub client not available)",
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	resp, err := client.Agents().List(ctx, nil)
	if err != nil {
		return scionruntime.CheckResult{
			Name:        "agent-health",
			Status:      "fail",
			Message:     fmt.Sprintf("Failed to query agents: %v", err),
			Remediation: "Check Hub server logs",
		}
	}

	if len(resp.Agents) == 0 {
		return scionruntime.CheckResult{
			Name:    "agent-health",
			Status:  "pass",
			Message: "No agents registered",
		}
	}

	stalled, crashed, errored := 0, 0, 0
	for _, agent := range resp.Agents {
		switch {
		case agent.Activity == "stalled":
			stalled++
		case agent.Activity == "crashed":
			crashed++
		case agent.Phase == "error":
			errored++
		}
	}

	unhealthy := stalled + crashed + errored
	if unhealthy > 0 {
		var parts []string
		if stalled > 0 {
			parts = append(parts, fmt.Sprintf("%d stalled", stalled))
		}
		if crashed > 0 {
			parts = append(parts, fmt.Sprintf("%d crashed", crashed))
		}
		if errored > 0 {
			parts = append(parts, fmt.Sprintf("%d in error", errored))
		}
		return scionruntime.CheckResult{
			Name:    "agent-health",
			Status:  "warn",
			Message: fmt.Sprintf("%d/%d agent(s) unhealthy: %s", unhealthy, len(resp.Agents), strings.Join(parts, ", ")),
		}
	}

	return scionruntime.CheckResult{
		Name:    "agent-health",
		Status:  "pass",
		Message: fmt.Sprintf("All %d agent(s) healthy", len(resp.Agents)),
	}
}

// checkDoctorNFSMounts performs D5: NFS mount status check.
func checkDoctorNFSMounts(hubEP string, hubConnected bool, client hubclient.Client) scionruntime.CheckResult {
	if hubEP == "" {
		return scionruntime.CheckResult{
			Name:        "nfs-mounts",
			Status:      "skip",
			Message:     "No Hub endpoint configured",
			Remediation: "Set SCION_HUB_ENDPOINT or use --hub flag",
		}
	}
	if !hubConnected {
		return scionruntime.CheckResult{
			Name:    "nfs-mounts",
			Status:  "skip",
			Message: "Skipped (Hub unreachable)",
		}
	}
	if client == nil {
		return scionruntime.CheckResult{
			Name:    "nfs-mounts",
			Status:  "skip",
			Message: "Skipped (Hub client not available)",
		}
	}

	// NFS health info would come from broker capabilities if reported.
	// Currently the broker heartbeat protocol does not expose NFS-specific
	// health data, so we cannot evaluate this check.
	return scionruntime.CheckResult{
		Name:    "nfs-mounts",
		Status:  "skip",
		Message: "NFS health not yet reported by broker API",
	}
}

// checkDoctorTelemetry performs D6: Telemetry pipeline check (local).
func checkDoctorTelemetry() scionruntime.CheckResult {
	enabled := os.Getenv("SCION_TELEMETRY_ENABLED")
	if enabled == "" || enabled == "false" || enabled == "0" {
		return scionruntime.CheckResult{
			Name:    "telemetry",
			Status:  "pass",
			Message: "Telemetry not enabled",
		}
	}

	endpoint := os.Getenv("SCION_OTEL_ENDPOINT")
	if endpoint == "" {
		endpoint = "localhost:4317"
	}

	conn, err := net.DialTimeout("tcp", endpoint, 3*time.Second)
	if err != nil {
		return scionruntime.CheckResult{
			Name:        "telemetry",
			Status:      "fail",
			Message:     fmt.Sprintf("Telemetry endpoint %s unreachable: %v", endpoint, err),
			Remediation: "Start the OpenTelemetry collector or check SCION_OTEL_ENDPOINT",
		}
	}
	_ = conn.Close()

	return scionruntime.CheckResult{
		Name:    "telemetry",
		Status:  "pass",
		Message: fmt.Sprintf("Telemetry endpoint %s is reachable", endpoint),
	}
}

// checkDoctorContainerImages performs D7: Container images check (local).
// Note: This is a simplified check that scans for any images with "scion" in
// the name. The design specifies checking per-configured-harness images, but
// that requires loading the project's harness config which adds complexity.
// A future enhancement could load harness configs and verify each image.
func checkDoctorContainerImages() scionruntime.CheckResult {
	// Find a container CLI (docker or podman)
	var containerCLI string
	for _, cli := range []string{"docker", "podman"} {
		if _, err := exec.LookPath(cli); err == nil {
			containerCLI = cli
			break
		}
	}

	if containerCLI == "" {
		return scionruntime.CheckResult{
			Name:    "container-images",
			Status:  "skip",
			Message: "No container runtime CLI found (docker/podman)",
		}
	}

	imgCtx, imgCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer imgCancel()
	out, err := exec.CommandContext(imgCtx, containerCLI, "images", "--format", "{{.Repository}}:{{.Tag}}").Output()
	if err != nil {
		return scionruntime.CheckResult{
			Name:    "container-images",
			Status:  "skip",
			Message: fmt.Sprintf("Could not list %s images: %v", containerCLI, err),
		}
	}

	imageOutput := strings.TrimSpace(string(out))
	if imageOutput == "" {
		return scionruntime.CheckResult{
			Name:        "container-images",
			Status:      "warn",
			Message:     fmt.Sprintf("No scion container images found via %s", containerCLI),
			Remediation: "Pull scion images or check image_registry configuration",
		}
	}

	images := strings.Split(imageOutput, "\n")
	scionImageCount := 0
	for _, img := range images {
		if strings.Contains(img, "scion") {
			scionImageCount++
		}
	}

	if scionImageCount == 0 {
		return scionruntime.CheckResult{
			Name:        "container-images",
			Status:      "warn",
			Message:     fmt.Sprintf("No scion container images found via %s", containerCLI),
			Remediation: "Pull scion images or check image_registry configuration",
		}
	}

	return scionruntime.CheckResult{
		Name:    "container-images",
		Status:  "pass",
		Message: fmt.Sprintf("%d scion image(s) found via %s", scionImageCount, containerCLI),
	}
}

// checkDoctorCredentialValidity performs D8: Broker credential validity check (local).
func checkDoctorCredentialValidity() scionruntime.CheckResult {
	store := brokercredentials.NewStore("")
	if !store.Exists() {
		return scionruntime.CheckResult{
			Name:    "broker-credentials",
			Status:  "skip",
			Message: "No broker credentials found (not a broker host)",
		}
	}

	creds, err := store.Load()
	if err != nil {
		return scionruntime.CheckResult{
			Name:        "broker-credentials",
			Status:      "fail",
			Message:     fmt.Sprintf("Failed to load broker credentials: %v", err),
			Remediation: "Re-register the broker with 'scion server start'",
		}
	}

	if creds.SecretKey == "" {
		return scionruntime.CheckResult{
			Name:        "broker-credentials",
			Status:      "fail",
			Message:     "Broker HMAC key is empty",
			Remediation: "Re-register the broker with 'scion server start'",
		}
	}

	return scionruntime.CheckResult{
		Name:    "broker-credentials",
		Status:  "pass",
		Message: "Broker credentials valid",
	}
}

func trimNewline(s string) string {
	if len(s) > 0 && s[len(s)-1] == '\n' {
		return s[:len(s)-1]
	}
	return s
}
