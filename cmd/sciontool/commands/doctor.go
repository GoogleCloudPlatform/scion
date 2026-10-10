/*
Copyright 2026 The Scion Authors.
*/

package commands

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/hub"
	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/rootexec"
	"github.com/GoogleCloudPlatform/scion/pkg/transportauth"
	"github.com/GoogleCloudPlatform/scion/pkg/util"
)

var doctorCmd = &cobra.Command{
	Use:   "doctor",
	Short: "Diagnose agent health, auth, and hub connectivity",
	Long: `Runs a series of diagnostic checks on the agent's environment,
authentication tokens, hub connectivity, and ancillary services.

Checks performed:
  - Environment variables (SCION_HUB_ENDPOINT, SCION_AGENT_ID, etc.)
  - Transport credential (IAP / Cloud Run invoker): header mode, audience,
    source in effect, expiry of the injected and refreshed values, and the
    outcome of the last refresh (never the token itself)
  - Token file presence, format, and expiry
  - Hub reachability (unauthenticated health check)
  - Token validity (authenticated status update and a read-only agent lookup),
    telling a platform-proxy rejection apart from a hub rejection
  - GCP metadata server (if configured)
  - GitHub App token (if configured)

Exit code 0 means all critical checks passed; non-zero means at least one failed.`,
	Run: func(cmd *cobra.Command, args []string) {
		os.Exit(runDoctor())
	},
}

func init() {
	rootCmd.AddCommand(doctorCmd)
}

func runDoctor() int {
	failures := 0

	fmt.Println("=== Scion Agent Doctor ===")

	// --- Environment ---
	failures += checkEnvironment()

	// --- Transport Auth ---
	var diag doctorDiag
	transportSrc := checkTransportAuth(&diag)
	if diag.transportFailed() {
		failures++
	}

	// --- Token ---
	tokenExpiry, tokenSubject := checkToken()

	// --- Hub Connectivity ---
	hubURL := resolveHubURL()
	hubReachable := false
	if hubURL != "" {
		hubReachable = checkHubConnectivity(hubURL, transportSrc)
	}

	// --- Authentication ---
	tokenValid := false
	if hubURL != "" && hubReachable {
		tokenValid = checkAuthentication(hubURL, &failures, transportSrc, &diag)
	}

	// --- GCP Metadata ---
	checkGCPMetadata(&failures)

	// --- GitHub Token ---
	checkGitHubToken(&failures)

	// --- Telemetry Pipeline ---
	checkTelemetryPipeline(&failures)

	// --- Workspace/Git State ---
	checkWorkspaceGit(&failures)

	// --- Harness Process ---
	checkHarnessProcess(&failures)

	// --- Remediation ---
	printRemediation(tokenExpiry, tokenSubject, tokenValid, diag)

	if failures > 0 {
		fmt.Printf("\n[RESULT] %d check(s) FAILED\n", failures)
		return 1
	}
	fmt.Println("\n[RESULT] All checks passed")
	return 0
}

func checkEnvironment() int {
	failures := 0
	fmt.Println("\n--- Environment ---")

	envVars := []struct {
		name     string
		required bool
		fallback string
	}{
		{"SCION_HUB_ENDPOINT", true, "SCION_HUB_URL"},
		{"SCION_AGENT_ID", true, ""},
		{"SCION_AGENT_MODE", false, ""},
	}

	for _, ev := range envVars {
		val := os.Getenv(ev.name)
		if val == "" && ev.fallback != "" {
			val = os.Getenv(ev.fallback)
			if val != "" {
				fmt.Printf("[ OK ] %s = %s (via %s)\n", ev.name, val, ev.fallback)
				continue
			}
		}
		if val == "" {
			if ev.required {
				fmt.Printf("[FAIL] %s is not set\n", ev.name)
				failures++
			} else {
				fmt.Printf("[INFO] %s is not set\n", ev.name)
			}
		} else {
			fmt.Printf("[ OK ] %s = %s\n", ev.name, val)
		}
	}

	mode := hub.OperatingMode()
	fmt.Printf("[INFO] Operating mode: %s\n", mode)

	return failures
}

// checkToken inspects the token file and returns (expiry, subject).
// Expiry is zero-value if the token can't be parsed.
func checkToken() (time.Time, string) {
	fmt.Println("\n--- Token ---")

	tokenPath := hub.TokenFilePath()
	data, err := os.ReadFile(tokenPath)
	if err != nil {
		fmt.Printf("[FAIL] Token file not found: %s\n", tokenPath)
		return time.Time{}, ""
	}

	token := strings.TrimSpace(string(data))
	if token == "" {
		fmt.Printf("[FAIL] Token file is empty: %s\n", tokenPath)
		return time.Time{}, ""
	}

	fmt.Printf("[ OK ] Token file: %s (%d bytes)\n", tokenPath, len(token))

	// Parse JWT claims
	claims, err := parseJWTClaims(token)
	if err != nil {
		fmt.Printf("[WARN] Cannot parse token as JWT: %v\n", err)
		return time.Time{}, ""
	}

	subject, _ := claims["sub"].(string)
	if subject != "" {
		fmt.Printf("[INFO] Subject: %s\n", subject)
	}

	if iat, ok := claims["iat"].(float64); ok {
		issuedAt := time.Unix(int64(iat), 0)
		fmt.Printf("[INFO] Issued:  %s\n", issuedAt.Format(time.RFC3339))
	}

	exp, ok := claims["exp"].(float64)
	if !ok {
		fmt.Println("[WARN] Token has no expiry claim")
		return time.Time{}, subject
	}

	expiry := time.Unix(int64(exp), 0)
	now := time.Now()

	if now.After(expiry) {
		since := now.Sub(expiry).Truncate(time.Second)
		fmt.Printf("[FAIL] Token EXPIRED at %s (%s ago)\n", expiry.Format(time.RFC3339), since)
	} else {
		until := expiry.Sub(now).Truncate(time.Second)
		refreshWindow := expiry.Add(-2 * time.Hour)
		if now.After(refreshWindow) {
			fmt.Printf("[WARN] Token expires at %s (in %s, within refresh window)\n", expiry.Format(time.RFC3339), until)
		} else {
			fmt.Printf("[ OK ] Token expires at %s (in %s)\n", expiry.Format(time.RFC3339), until)
		}
	}

	return expiry, subject
}

func resolveHubURL() string {
	hubURL := os.Getenv("SCION_HUB_ENDPOINT")
	if hubURL == "" {
		hubURL = os.Getenv("SCION_HUB_URL")
	}
	return hubURL
}

// checkTransportAuth reports how the transport credential (IAP / Cloud Run
// invoker) is configured: the header mode, audience, which source is in
// effect and its expiry. It never prints token values. Findings are
// recorded in diag.
func checkTransportAuth(diag *doctorDiag) transportauth.TokenSource {
	fmt.Println("\n--- Transport Auth ---")

	// Both file-backed steps read with sciontool's guarded reader, as for
	// the agent token file.
	src, err := transportauth.FromEnvWithReader(hub.ReadTransportTokenFileGuarded)
	if err != nil {
		fmt.Printf("[WARN] Transport auth error: %v\n", err)
		return nil
	}
	if src == nil {
		src = scionHomeLateFileSource()
	}
	if src == nil {
		if mode := os.Getenv(transportauth.EnvTransportMode); transportauth.IsProxyMode(mode) {
			// A proxy guards the hub but no transport token has been
			// received yet (for example the dispatch-time mint failed).
			// A later refresh or reset-auth installs one.
			// The path shown is the scion user's file, which is where the
			// agent writes it, even when doctor runs with another HOME.
			diag.transportConfigured = true
			diag.transportMissing = true
			fmt.Println("[INFO] Transport Auth: hub-provided token (awaiting first token)")
			printTransportModeAndAudience()
			fmt.Printf("[FAIL] Transport credential: none received yet (no %s value and no file at %s)\n",
				transportauth.EnvTransportToken, hub.TransportTokenFilePath())
			reportTransportRefreshStatus(diag)
			return nil
		}
		fmt.Println("[INFO] Transport Auth: none")
		return nil
	}

	diag.transportConfigured = true

	switch s := src.(type) {
	case *transportauth.FileSource:
		fmt.Println("[INFO] Transport Auth: hub-provided token")
		printTransportModeAndAudience()
		reportFileSource(s.Status(), diag)
	case *transportauth.InjectedSource:
		fmt.Println("[INFO] Transport Auth: hub-provided token")
		printTransportModeAndAudience()
		printExpiryLine("injected value", src.Expiry(), diag)
	case *transportauth.MetadataSource:
		fmt.Println("[ OK ] Transport Auth: metadata server (self-refreshing)")
		printTransportModeAndAudience()
	default:
		fmt.Println("[ OK ] Transport Auth: active")
		printTransportModeAndAudience()
	}

	return src
}

// scionHomeLateFileSource returns a file-backed source for the scion
// user's transport token file when a proxy mode is set and that file
// exists. FromEnv looks under $HOME, so doctor run with another HOME (for
// example exec'd as root) would otherwise miss a token the agent received
// after start. The file is read with sciontool's guarded reader, as for
// the agent token file. It returns nil outside a proxy mode or when the
// file is absent.
func scionHomeLateFileSource() transportauth.TokenSource {
	if !transportauth.IsProxyMode(os.Getenv(transportauth.EnvTransportMode)) {
		return nil
	}
	path := hub.TransportTokenFilePath()
	if path == "" {
		return nil
	}
	if _, err := os.Lstat(path); err != nil {
		return nil
	}
	return hub.NewTransportTokenFileSource()
}

func wrapTransport(client *http.Client, src transportauth.TokenSource) {
	if src == nil {
		return
	}
	mode := transportauth.ModeFromEnv()
	if client.Transport == nil {
		client.Transport = http.DefaultTransport
	}
	client.Transport = transportauth.Wrap(client.Transport, src, mode)
}

func checkHubConnectivity(hubURL string, transportSrc transportauth.TokenSource) bool {
	fmt.Println("\n--- Hub Connectivity ---")

	client := newDoctorHTTPClient(transportSrc)
	healthURL := strings.TrimSuffix(hubURL, "/") + "/healthz"

	resp, err := client.Get(healthURL)
	if err != nil {
		fmt.Printf("[FAIL] Hub unreachable at %s: %s\n", hubURL, describeRequestError(err))
		return false
	}
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	_ = resp.Body.Close()

	if by := classifyRejection(resp, body); by == rejectedByProxy {
		fmt.Printf("[WARN] Hub endpoint answered %d at %s: %s\n", resp.StatusCode, healthURL, describeRejection(by))
		return true
	}
	if target, ok := redirectTarget(resp); ok {
		fmt.Printf("[WARN] Hub endpoint answered %d at %s: redirected to %s (redirects are not followed)\n",
			resp.StatusCode, healthURL, target)
		return true
	}

	if resp.StatusCode < 400 {
		fmt.Printf("[ OK ] Hub reachable at %s\n", hubURL)
		return true
	}

	fmt.Printf("[WARN] Hub returned %d at %s\n", resp.StatusCode, healthURL)
	if resp.StatusCode == http.StatusNotFound {
		fmt.Println("[INFO] Some platforms reserve /healthz (Cloud Run, for example, may answer it itself), " +
			"so a 404 here does not by itself mean the hub is down. The Authentication checks below use real hub routes.")
	}
	return true
}

// newDoctorHTTPClient returns a client that adds the transport credential
// (when configured) and does not follow redirects, so a platform proxy's
// redirect to a sign-in page is visible rather than followed.
func newDoctorHTTPClient(transportSrc transportauth.TokenSource) *http.Client {
	client := &http.Client{
		Timeout: 5 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	wrapTransport(client, transportSrc)
	return client
}

func checkAuthentication(hubURL string, failures *int, transportSrc transportauth.TokenSource, diag *doctorDiag) bool {
	fmt.Println("\n--- Authentication ---")

	agentID := os.Getenv("SCION_AGENT_ID")
	token := hub.ReadTokenFile()

	if token == "" || agentID == "" {
		fmt.Println("[FAIL] Cannot test auth: missing token or agent ID")
		*failures++
		return false
	}

	client := newDoctorHTTPClient(transportSrc)

	// Test with a heartbeat (least disruptive authenticated call)
	statusURL := fmt.Sprintf("%s/api/v1/agents/%s/status",
		strings.TrimSuffix(hubURL, "/"), agentID)
	body, _ := json.Marshal(map[string]interface{}{
		"heartbeat": true,
	})

	req, _ := http.NewRequest("POST", statusURL, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Scion-Agent-Token", token)

	resp, err := client.Do(req)
	if err != nil {
		fmt.Printf("[FAIL] Auth check failed: %s\n", describeRequestError(err))
		*failures++
		return false
	}
	respBody, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()

	if by := classifyRejection(resp, respBody); by != rejectedByNone {
		diag.authRejectedBy = by
		fmt.Printf("[FAIL] Heartbeat rejected (%d): %s: %s\n", resp.StatusCode, describeRejection(by), rejectionDetail(resp, respBody))
		*failures++
	} else if target, ok := redirectTarget(resp); ok {
		diag.authRedirectedTo = target
		fmt.Printf("[FAIL] Heartbeat not confirmed: hub answered %d, redirected to %s\n", resp.StatusCode, target)
		*failures++
	} else if resp.StatusCode < 400 {
		fmt.Println("[ OK ] Authenticated successfully (heartbeat accepted)")
	} else {
		fmt.Printf("[WARN] Hub returned %d: %s\n", resp.StatusCode, doctorTruncate(string(respBody), 120))
	}

	// Confirm the token a second way using a read-only lookup, deliberately
	// *not* the token-refresh endpoint: POST .../token/refresh mints a
	// replacement credential and revokes the one just presented (hub side:
	// pkg/hub/handlers_agents_core.go). Doctor has no way to hand a rotated
	// token back to the running agent process, so calling refresh here
	// discarded the replacement and left the agent holding a now-revoked
	// token — every subsequent heartbeat/status call 401'd until restart
	// (ptone/scion#1939). GET /api/v1/agents/{id} exercises the same
	// authenticated path (it's what `Client.GetSelf` / `scion whoami --full`
	// use) without mutating any credential state.
	selfURL := fmt.Sprintf("%s/api/v1/agents/%s",
		strings.TrimSuffix(hubURL, "/"), agentID)

	req, err = http.NewRequest("GET", selfURL, nil)
	if err != nil {
		fmt.Printf("[FAIL] Failed to create request: %v\n", err)
		*failures++
		return false
	}
	req.Header.Set("X-Scion-Agent-Token", token)

	resp, err = client.Do(req)
	if err != nil {
		fmt.Printf("[FAIL] Agent lookup check failed: %s\n", describeRequestError(err))
		*failures++
		return false
	}
	respBody, _ = io.ReadAll(resp.Body)
	_ = resp.Body.Close()

	by := classifyRejection(resp, respBody)
	target, redirected := redirectTarget(resp)
	switch {
	case by != rejectedByNone:
		diag.authRejectedBy = by
		fmt.Printf("[FAIL] Agent lookup rejected (%d): %s: %s\n", resp.StatusCode, describeRejection(by), rejectionDetail(resp, respBody))
		*failures++
		return false
	case redirected:
		diag.authRedirectedTo = target
		fmt.Printf("[FAIL] Agent lookup not confirmed: hub answered %d, redirected to %s\n", resp.StatusCode, target)
		*failures++
		return false
	case resp.StatusCode < 400:
		fmt.Println("[ OK ] Agent record accessible (read-only check; credentials untouched)")
		return true
	default:
		fmt.Printf("[WARN] Agent lookup returned %d: %s\n", resp.StatusCode, doctorTruncate(string(respBody), 120))
		return false
	}
}

func checkGCPMetadata(failures *int) {
	// os.LookupEnv, not os.Getenv: "nobody configured a metadata mode" and "a
	// mode is configured but unusable" are different conditions, and only the
	// first is a reason to skip the check silently. This mirrors ConfigFromEnv,
	// which resolves the same three cases the same way.
	rawMode, present := os.LookupEnv("SCION_METADATA_MODE")
	if !present {
		return
	}

	fmt.Println("\n--- GCP Metadata ---")

	// Passthrough runs no metadata server on purpose, so probing for one and
	// reporting it unreachable would fail a correctly configured agent.
	if rawMode == "passthrough" {
		fmt.Println("[ OK ] Mode: passthrough — no metadata server expected")
		return
	}

	mode := rawMode
	if mode == "" {
		fmt.Println("[WARN] SCION_METADATA_MODE is set but empty — the sidecar falls back to block mode")
		mode = "block"
	}

	port := 18380
	if p := os.Getenv("SCION_METADATA_PORT"); p != "" {
		_, _ = fmt.Sscanf(p, "%d", &port)
	}

	client := &http.Client{Timeout: 2 * time.Second}
	addr := fmt.Sprintf("http://127.0.0.1:%d/", port)

	resp, err := client.Get(addr)
	if err != nil {
		fmt.Printf("[FAIL] Metadata server unreachable at %s: %v\n", addr, err)
		*failures++
		return
	}
	_ = resp.Body.Close()

	if resp.StatusCode == http.StatusOK {
		fmt.Printf("[ OK ] Metadata server healthy at %s (mode=%s)\n", addr, mode)
	} else {
		fmt.Printf("[FAIL] Metadata server returned %d\n", resp.StatusCode)
		*failures++
		return
	}

	// In assign mode, verify we can actually acquire a GCP access token.
	// This is what gcloud auth print-access-token exercises end-to-end:
	// metadata server → hub token broker → GCP token.
	if mode == "assign" {
		checkGCPTokenAcquisition(port, failures)
	}
}

func checkGCPTokenAcquisition(port int, failures *int) {
	tokenURL := fmt.Sprintf("http://127.0.0.1:%d/computeMetadata/v1/instance/service-accounts/default/token", port)

	req, err := http.NewRequest("GET", tokenURL, nil)
	if err != nil {
		fmt.Printf("[FAIL] GCP token check: failed to create request: %v\n", err)
		*failures++
		return
	}
	req.Header.Set("Metadata-Flavor", "Google")

	// Token brokering involves a hub round-trip; use a longer timeout.
	tokenClient := &http.Client{Timeout: 10 * time.Second}
	resp, err := tokenClient.Do(req)
	if err != nil {
		fmt.Printf("[FAIL] GCP token check: request failed: %v\n", err)
		*failures++
		return
	}
	defer func() { _ = resp.Body.Close() }()

	body, _ := io.ReadAll(resp.Body)

	if resp.StatusCode != http.StatusOK {
		fmt.Printf("[FAIL] GCP token check: metadata server returned %d: %s\n",
			resp.StatusCode, doctorTruncate(string(body), 120))
		fmt.Println("[!] gcloud auth print-access-token will fail in this state")
		fmt.Println("[!] Run from the host:  scion agent reset-auth <agent-name>")
		*failures++
		return
	}

	var tokenResp struct {
		AccessToken string `json:"access_token"`
		ExpiresIn   int    `json:"expires_in"`
		TokenType   string `json:"token_type"`
	}
	if err := json.Unmarshal(body, &tokenResp); err != nil {
		fmt.Printf("[FAIL] GCP token check: invalid token response: %v\n", err)
		*failures++
		return
	}

	if tokenResp.AccessToken == "" {
		fmt.Println("[FAIL] GCP token check: response missing access_token")
		*failures++
		return
	}

	fmt.Printf("[ OK ] GCP access token retrievable (expires_in=%ds)\n", tokenResp.ExpiresIn)
}

func checkGitHubToken(failures *int) {
	if !hub.IsGitHubAppEnabled() {
		return
	}

	fmt.Println("\n--- GitHub Token ---")

	tokenPath := hub.GitHubTokenPath()
	token := hub.ReadGitHubTokenFile(tokenPath)
	if token == "" {
		fmt.Printf("[FAIL] GitHub token file missing or empty: %s\n", tokenPath)
		*failures++
		return
	}
	fmt.Printf("[ OK ] GitHub token file present: %s\n", tokenPath)

	if hub.IsGitHubTokenExpired(tokenPath) {
		expiry, err := hub.ReadGitHubTokenExpiry(tokenPath)
		if err != nil {
			fmt.Println("[FAIL] GitHub token expired (expiry file unreadable)")
		} else {
			fmt.Printf("[FAIL] GitHub token expired at %s\n", expiry.Format(time.RFC3339))
		}
		*failures++
	} else {
		expiry, err := hub.ReadGitHubTokenExpiry(tokenPath)
		if err != nil {
			fmt.Println("[ OK ] GitHub token present (expiry unknown)")
		} else {
			fmt.Printf("[ OK ] GitHub token valid until %s\n", expiry.Format(time.RFC3339))
		}
	}
}

func checkTelemetryPipeline(failures *int) {
	telemetryEnabled := os.Getenv("SCION_TELEMETRY_ENABLED")
	if telemetryEnabled == "" || telemetryEnabled == "false" || telemetryEnabled == "0" {
		return
	}

	fmt.Println("\n--- Telemetry Pipeline ---")

	addr := "localhost:4317"
	if endpoint := os.Getenv("SCION_OTEL_ENDPOINT"); endpoint != "" {
		addr = endpoint
	}

	conn, err := net.DialTimeout("tcp", addr, 2*time.Second)
	if err != nil {
		fmt.Printf("[FAIL] Telemetry pipeline unreachable at %s: %v\n", addr, err)
		*failures++
		return
	}
	_ = conn.Close()
	fmt.Printf("[ OK ] Telemetry pipeline healthy (port %s)\n", addr)
}

func checkWorkspaceGit(failures *int) {
	if !util.ParseBoolEnv("SCION_WORKSPACE_GIT", false) {
		return
	}

	fmt.Println("\n--- Workspace/Git State ---")

	if os.Geteuid() == 0 {
		// This check passes HOME through so `git status` picks up the
		// invoking user's own gitconfig (needed for a normal, non-root
		// doctor run against a shared workspace with a different owning
		// uid — see safe.directory below). Doing that as root would let a
		// workload-writable ~/.gitconfig ("safe.directory=*") or
		// /workspace/.git/config ("core.fsmonitor=<cmd>") run arbitrary
		// code as root; no shipped path runs doctor as root, so this is a
		// belt-and-suspenders refusal for a human invoking it that way
		// directly, not a case this binary needs to actually support.
		fmt.Println("[INFO] Running as root — skipping git workspace check (would trust a workload-controlled gitconfig)")
		return
	}

	gitCtx, gitCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer gitCancel()
	gitPath, err := rootexec.Resolve("git")
	if err != nil {
		fmt.Printf("[FAIL] could not resolve a trusted git binary: %v\n", err)
		*failures++
		return
	}
	cmd := exec.CommandContext(gitCtx, gitPath, "-C", "/workspace", "status", "--porcelain")
	cmd.Env = rootexec.Env("HOME=" + os.Getenv("HOME"))
	if err := cmd.Run(); err != nil {
		fmt.Printf("[FAIL] Git workspace corrupted: %v\n", err)
		*failures++
		return
	}
	fmt.Println("[ OK ] Git workspace intact")
}

func checkHarnessProcess(failures *int) {
	fmt.Println("\n--- Harness Process ---")

	pidStr := os.Getenv("SCION_HARNESS_PID")
	if pidStr != "" {
		pid, err := strconv.Atoi(pidStr)
		if err != nil {
			fmt.Printf("[WARN] SCION_HARNESS_PID invalid: %s\n", pidStr)
		} else {
			proc, err := os.FindProcess(pid)
			if err != nil {
				fmt.Printf("[FAIL] Harness process (PID %d) not found\n", pid)
				*failures++
				return
			}
			// Use platform-specific liveness check (signal 0 on Unix).
			if err := checkProcessAlive(proc); err != nil {
				fmt.Printf("[FAIL] Harness process (PID %d) not alive: %v\n", pid, err)
				*failures++
				return
			}
			fmt.Printf("[ OK ] Harness process alive (PID %d)\n", pid)
			return
		}
	}

	// Fallback: search for known harness process names.
	pgrepPath, err := rootexec.Resolve("pgrep")
	if err != nil {
		fmt.Println("[INFO] Cannot determine harness process status")
		return
	}
	cmd := exec.Command(pgrepPath, "-f", "claude|gemini|codex")
	cmd.Env = rootexec.Env()
	output, err := cmd.Output()
	if err != nil {
		fmt.Println("[INFO] Cannot determine harness process status")
		return
	}
	pids := strings.TrimSpace(string(output))
	if pids != "" {
		lines := strings.Split(pids, "\n")
		fmt.Printf("[ OK ] Harness process(es) found (%d match)\n", len(lines))
	} else {
		fmt.Println("[INFO] No harness process detected")
	}
}

func printRemediation(tokenExpiry time.Time, tokenSubject string, tokenValid bool, diag doctorDiag) {
	now := time.Now()

	// Only print remediation if there's a problem
	expired := !tokenExpiry.IsZero() && now.After(tokenExpiry)
	transportProblem := diag.transportFailed() || diag.authRejectedBy == rejectedByProxy
	if !expired && tokenValid && !transportProblem && diag.authRedirectedTo == "" {
		return
	}

	fmt.Println("\n--- Remediation ---")

	// A redirect means the probes never reached an endpoint that answered
	// them, so neither credential was checked.
	if diag.authRedirectedTo != "" && diag.authRejectedBy == rejectedByNone && !transportProblem {
		fmt.Printf("[!] The hub endpoint redirected authenticated requests to %s, so authentication could not be confirmed.\n",
			diag.authRedirectedTo)
		fmt.Println("[!] Check that SCION_HUB_ENDPOINT uses the hub's final URL (scheme and host), " +
			"and that nothing between the agent and the hub redirects API requests.")
		if expired {
			fmt.Println("[!] The agent token has also expired. Run from the host:  scion agent reset-auth <agent-name>")
		}
		return
	}

	// A platform proxy rejection means the agent token never reached the
	// hub, so hub-side advice (signing keys) would be misleading.
	if printTransportRemediation(diag) && diag.authRejectedBy != rejectedByHub && !expired {
		return
	}

	if expired && !tokenValid {
		fmt.Println("[!] Token is expired and cannot be refreshed.")
		fmt.Println("[!] Run from the host:  scion agent reset-auth <agent-name>")
		fmt.Println("[!] Or restart agent:   scion agent restart <agent-name>")
	} else if !tokenValid {
		fmt.Println("[!] Token is rejected by the hub (signing key may have changed).")
		fmt.Println("[!] Run from the host:  scion agent reset-auth <agent-name>")
		fmt.Println("[!] Or restart agent:   scion agent restart <agent-name>")
	}
}

// parseJWTClaims extracts claims from a JWT without validating the signature.
func parseJWTClaims(token string) (map[string]interface{}, error) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return nil, fmt.Errorf("invalid JWT format: expected 3 parts, got %d", len(parts))
	}

	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return nil, fmt.Errorf("failed to decode JWT payload: %w", err)
	}

	var claims map[string]interface{}
	if err := json.Unmarshal(payload, &claims); err != nil {
		return nil, fmt.Errorf("failed to parse JWT claims: %w", err)
	}

	return claims, nil
}

func doctorTruncate(s string, maxLen int) string {
	s = strings.TrimSpace(s)
	if len(s) <= maxLen {
		return s
	}
	return s[:maxLen] + "..."
}
