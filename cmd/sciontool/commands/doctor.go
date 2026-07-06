/*
Copyright 2026 The Scion Authors.
*/

package commands

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/hub"
)

var doctorCmd = &cobra.Command{
	Use:   "doctor",
	Short: "Diagnose agent health, auth, and hub connectivity",
	Long: `Runs a series of diagnostic checks on the agent's environment,
authentication tokens, hub connectivity, and ancillary services.

Checks performed:
  - Environment variables (SCION_HUB_ENDPOINT, SCION_AGENT_ID, etc.)
  - Token file presence, format, and expiry
  - Hub reachability (unauthenticated health check)
  - Token validity (authenticated status update)
  - Token refresh capability
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

	// --- Token ---
	tokenExpiry, tokenSubject := checkToken()

	// --- Hub Connectivity ---
	hubURL := resolveHubURL()
	hubReachable := false
	if hubURL != "" {
		hubReachable = checkHubConnectivity(hubURL)
	}

	// --- Authentication ---
	tokenValid := false
	if hubURL != "" && hubReachable {
		tokenValid = checkAuthentication(hubURL, &failures)
	}

	// --- GCP Metadata ---
	checkGCPMetadata(&failures)

	// --- GitHub Token ---
	checkGitHubToken(&failures)

	// --- Remediation ---
	printRemediation(tokenExpiry, tokenSubject, tokenValid)

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

func checkHubConnectivity(hubURL string) bool {
	fmt.Println("\n--- Hub Connectivity ---")

	client := &http.Client{Timeout: 5 * time.Second}
	healthURL := strings.TrimSuffix(hubURL, "/") + "/healthz"

	resp, err := client.Get(healthURL)
	if err != nil {
		fmt.Printf("[FAIL] Hub unreachable at %s: %v\n", hubURL, err)
		return false
	}
	_ = resp.Body.Close()

	if resp.StatusCode < 400 {
		fmt.Printf("[ OK ] Hub reachable at %s\n", hubURL)
		return true
	}

	fmt.Printf("[WARN] Hub returned %d at %s\n", resp.StatusCode, healthURL)
	return true
}

func checkAuthentication(hubURL string, failures *int) bool {
	fmt.Println("\n--- Authentication ---")

	agentID := os.Getenv("SCION_AGENT_ID")
	token := hub.ReadTokenFile()

	if token == "" || agentID == "" {
		fmt.Println("[FAIL] Cannot test auth: missing token or agent ID")
		*failures++
		return false
	}

	client := &http.Client{Timeout: 5 * time.Second}

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
		fmt.Printf("[FAIL] Auth check failed: %v\n", err)
		*failures++
		return false
	}
	respBody, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()

	if resp.StatusCode < 400 {
		fmt.Println("[ OK ] Authenticated successfully (heartbeat accepted)")
	} else if resp.StatusCode == 401 || resp.StatusCode == 403 {
		fmt.Printf("[FAIL] Token rejected by hub (%d): %s\n", resp.StatusCode, doctorTruncate(string(respBody), 120))
		*failures++
	} else {
		fmt.Printf("[WARN] Hub returned %d: %s\n", resp.StatusCode, doctorTruncate(string(respBody), 120))
	}

	// Test token refresh
	refreshURL := fmt.Sprintf("%s/api/v1/agents/%s/token/refresh",
		strings.TrimSuffix(hubURL, "/"), agentID)

	req, _ = http.NewRequest("POST", refreshURL, nil)
	req.Header.Set("X-Scion-Agent-Token", token)

	resp, err = client.Do(req)
	if err != nil {
		fmt.Printf("[FAIL] Token refresh check failed: %v\n", err)
		*failures++
		return false
	}
	respBody, _ = io.ReadAll(resp.Body)
	_ = resp.Body.Close()

	switch resp.StatusCode {
	case 200:
		fmt.Println("[ OK ] Token refresh works")
		return true
	case 401, 403:
		fmt.Printf("[FAIL] Token refresh rejected (%d): %s\n", resp.StatusCode, doctorTruncate(string(respBody), 120))
		*failures++
		return false
	default:
		fmt.Printf("[WARN] Token refresh returned %d: %s\n", resp.StatusCode, doctorTruncate(string(respBody), 120))
		return false
	}
}

func checkGCPMetadata(failures *int) {
	mode := os.Getenv("SCION_METADATA_MODE")
	if mode == "" {
		return
	}

	fmt.Println("\n--- GCP Metadata ---")

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
	if os.Getenv("SCION_GITHUB_APP_ENABLED") != "true" {
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

func printRemediation(tokenExpiry time.Time, tokenSubject string, tokenValid bool) {
	now := time.Now()

	// Only print remediation if there's a problem
	expired := !tokenExpiry.IsZero() && now.After(tokenExpiry)
	if !expired && tokenValid {
		return
	}

	fmt.Println("\n--- Remediation ---")

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
