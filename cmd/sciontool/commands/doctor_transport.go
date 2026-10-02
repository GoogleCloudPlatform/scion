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
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/hub"
	"github.com/GoogleCloudPlatform/scion/pkg/transportauth"
)

// Who rejected an authenticated doctor probe.
const (
	rejectedByNone  = ""
	rejectedByProxy = "proxy" // the platform guard in front of the hub (IAP / Cloud Run invoker)
	rejectedByHub   = "hub"   // the hub's own agent-token check
)

// doctorDiag collects findings that later sections (remediation) act on.
type doctorDiag struct {
	// transportConfigured is true when the agent uses a transport token.
	transportConfigured bool
	// transportMissing is true when transport auth is configured but no
	// credential is available.
	transportMissing bool
	// transportExpired is true when the credential in use has expired.
	transportExpired bool
	// transportRefreshProblem describes the last refresh's transport
	// outcome when it was not a fresh token ("" when fine or unknown).
	transportRefreshProblem string
	// authRejectedBy records who rejected the authentication probes.
	authRejectedBy string
}

// transportFailed reports whether the transport section found a failure.
func (d *doctorDiag) transportFailed() bool {
	return d.transportMissing || d.transportExpired
}

// fmtWhen formats t with a relative suffix, e.g. "2026-01-02T03:04:05Z (in 41m)".
func fmtWhen(t time.Time) string {
	now := time.Now()
	if t.After(now) {
		return fmt.Sprintf("%s (in %s)", t.UTC().Format(time.RFC3339), t.Sub(now).Truncate(time.Second))
	}
	return fmt.Sprintf("%s (%s ago)", t.UTC().Format(time.RFC3339), now.Sub(t).Truncate(time.Second))
}

// fmtExpiry formats an expiry as "expires <when>" or "expired <when>".
func fmtExpiry(t time.Time) string {
	if t.IsZero() {
		return "expiry unknown"
	}
	if time.Now().After(t) {
		return "expired " + fmtWhen(t)
	}
	return "expires " + fmtWhen(t)
}

// transportAudience returns the configured transport audience and the
// variable it came from.
func transportAudience() (string, string) {
	if a := os.Getenv(transportauth.EnvTransportAudience); a != "" {
		return a, transportauth.EnvTransportAudience
	}
	if a := os.Getenv(transportauth.EnvHubOIDCAudience); a != "" {
		return a, transportauth.EnvHubOIDCAudience
	}
	return "", ""
}

// printTransportModeAndAudience prints the header mode and audience.
func printTransportModeAndAudience() {
	modeName := os.Getenv(transportauth.EnvTransportMode)
	if modeName == "" {
		modeName = "default"
	}
	header := transportauth.ModeFromEnv().HeaderName()
	fmt.Printf("[INFO] Mode: %s (header: %s)\n", modeName, header)

	if aud, from := transportAudience(); aud != "" {
		fmt.Printf("[INFO] Audience: %s (from %s)\n", aud, from)
	} else {
		fmt.Println("[INFO] Audience: not set in the environment")
	}
}

// printExpiryLine prints the status line for the credential in use and
// updates diag. label describes where it came from.
func printExpiryLine(label string, expiry time.Time, diag *doctorDiag) {
	switch {
	case expiry.IsZero():
		fmt.Printf("[ OK ] Transport credential in use: %s (expiry unknown)\n", label)
	case time.Now().After(expiry):
		diag.transportExpired = true
		fmt.Printf("[FAIL] Transport credential in use: %s, EXPIRED at %s\n", label, fmtWhen(expiry))
	case time.Now().After(expiry.Add(-transportauth.RefreshMargin)):
		fmt.Printf("[WARN] Transport credential in use: %s, expires %s (within refresh margin)\n", label, fmtWhen(expiry))
	default:
		fmt.Printf("[ OK ] Transport credential in use: %s, expires %s\n", label, fmtWhen(expiry))
	}
}

// reportFileSource prints the diagnostics for a hub-provided transport
// token: which candidate is in use, the expiry of the bootstrap (env) and
// refreshed (file) values side by side, and the last refresh outcome.
// It never prints token values.
func reportFileSource(st transportauth.FileSourceStatus, diag *doctorDiag) {
	switch st.InUse {
	case "":
		diag.transportMissing = true
		fmt.Printf("[FAIL] Transport credential: none available (no %s value and no readable file at %s)\n",
			transportauth.EnvTransportToken, st.Path)
	case transportauth.SourceLabelFile:
		printExpiryLine("refreshed file "+st.Path, st.Expiry, diag)
	case transportauth.SourceLabelEnv:
		printExpiryLine("bootstrap value from "+transportauth.EnvTransportToken, st.Expiry, diag)
	default:
		printExpiryLine(st.InUse, st.Expiry, diag)
	}

	// Side by side: expiry only, never values.
	if st.EnvPresent {
		fmt.Printf("[INFO]   env  (%s): %s\n", transportauth.EnvTransportToken, fmtExpiry(st.EnvExpiry))
	} else {
		fmt.Printf("[INFO]   env  (%s): not set in this process\n", transportauth.EnvTransportToken)
	}
	switch {
	case st.FileError != nil:
		fmt.Printf("[WARN]   file (%s): unreadable: %v\n", st.Path, st.FileError)
	case st.FilePresent:
		fmt.Printf("[INFO]   file (%s): %s, last written %s\n",
			st.Path, fmtExpiry(st.FileExpiry), fmtWhen(st.FileModTime))
	default:
		fmt.Printf("[INFO]   file (%s): not present\n", st.Path)
	}

	reportTransportRefreshStatus(diag)
}

// reportTransportRefreshStatus prints the transport outcome of the most
// recent token refresh, as recorded by sciontool init.
func reportTransportRefreshStatus(diag *doctorDiag) {
	rs, ok := hub.ReadTransportRefreshStatus()
	if !ok {
		fmt.Println("[INFO] Last refresh: none recorded yet")
		return
	}
	switch rs.Outcome {
	case hub.TransportRefreshOutcomeRefreshed:
		fmt.Printf("[ OK ] Last refresh: new transport token received at %s\n", fmtWhen(rs.At))
	case hub.TransportRefreshOutcomeFailed:
		diag.transportRefreshProblem = rs.Error
		fmt.Printf("[WARN] Last refresh at %s: hub did not issue a transport token: %s\n", fmtWhen(rs.At), rs.Error)
	case hub.TransportRefreshOutcomeAbsent:
		diag.transportRefreshProblem = "the hub returned no transport token"
		fmt.Printf("[WARN] Last refresh at %s: the hub returned no transport token (is a transport minter configured on the hub?)\n", fmtWhen(rs.At))
	default:
		fmt.Printf("[INFO] Last refresh at %s: %s\n", fmtWhen(rs.At), rs.Outcome)
	}
}

// classifyRejection decides whether a 401/403 (or a redirect to a sign-in
// page) came from the platform guard in front of the hub or from the hub
// itself. The hub answers with a JSON error object; IAP and Cloud Run
// answer with their own (non-JSON) pages.
func classifyRejection(resp *http.Response, body []byte) string {
	if resp == nil {
		return rejectedByNone
	}
	if resp.StatusCode >= 300 && resp.StatusCode < 400 {
		if loc := resp.Header.Get("Location"); strings.Contains(loc, "accounts.google.com") {
			return rejectedByProxy
		}
		return rejectedByNone
	}
	if resp.StatusCode != http.StatusUnauthorized && resp.StatusCode != http.StatusForbidden {
		return rejectedByNone
	}
	if strings.EqualFold(resp.Header.Get("X-Goog-IAP-Generated-Response"), "true") {
		return rejectedByProxy
	}
	var hubErr struct {
		Error json.RawMessage `json:"error"`
	}
	if json.Unmarshal(body, &hubErr) == nil && len(hubErr.Error) > 0 {
		return rejectedByHub
	}
	return rejectedByProxy
}

// describeRejection returns a short label for a classified rejection.
func describeRejection(by string) string {
	switch by {
	case rejectedByProxy:
		return "platform proxy (IAP / Cloud Run) rejected the transport credential"
	case rejectedByHub:
		return "hub rejected the agent token"
	default:
		return "rejected"
	}
}

// printTransportRemediation prints remediation for transport problems.
// It returns true if it printed anything.
func printTransportRemediation(diag doctorDiag) bool {
	if !diag.transportFailed() && diag.authRejectedBy != rejectedByProxy {
		return false
	}
	switch {
	case diag.transportExpired:
		fmt.Println("[!] The transport credential in use has expired, so requests are stopped by the platform proxy before reaching the hub.")
	case diag.transportMissing:
		fmt.Println("[!] Transport auth is configured but no transport credential is available.")
	default:
		fmt.Println("[!] The platform proxy (IAP / Cloud Run invoker) rejected the transport credential; the agent token was not checked.")
	}
	if diag.transportRefreshProblem != "" {
		fmt.Printf("[!] The last refresh did not renew it: %s. Check the hub's transport minter configuration and logs.\n",
			diag.transportRefreshProblem)
	}
	fmt.Println("[!] Run from the host:  scion agent reset-auth <agent-name>  (also pushes a fresh transport token)")
	fmt.Println("[!] Or restart agent:   scion agent restart <agent-name>")
	if diag.authRejectedBy == rejectedByProxy && !diag.transportFailed() {
		fmt.Println("[!] If the credential is current, check that SCION_TRANSPORT_MODE matches the proxy " +
			"(iap: Proxy-Authorization, cloudrun_invoker: X-Serverless-Authorization) " +
			"and that the audience matches the proxy's expected audience.")
	}
	return true
}
