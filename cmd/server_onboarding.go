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
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/hubsync"
	"github.com/GoogleCloudPlatform/scion/pkg/util"
)

// onboardingStatusPath is the hub endpoint that reports whether the
// workstation onboarding wizard has been completed (pkg/hub/system_handlers.go).
const onboardingStatusPath = "/api/v1/system/status"

// onboardingWizardPath is the web path of the onboarding wizard.
const onboardingWizardPath = "/onboarding"

// errOnboardingStatusNotAsked stands in for the status error when the hub was
// not asked: the server was not up, or the hub component is disabled.
var errOnboardingStatusNotAsked = errors.New("onboarding status not requested: server not ready or hub disabled")

// onboardingStatusResult is the subset of the hub's onboarding status that
// `scion server start` uses.
type onboardingStatusResult struct {
	Complete         bool   `json:"complete"`
	EmbeddedBrokerID string `json:"embeddedBrokerID,omitempty"`
	// ImageRegistry is the hub's resolved registry, which counts a value
	// saved in the hub database (ptone/scion#3916).
	ImageRegistry string `json:"imageRegistry,omitempty"`
}

// fetchOnboardingStatus asks the hub at baseURL (no trailing slash) for its
// onboarding status. The endpoint is workstation-only and loopback-only, and
// authenticates with the workstation dev token, sent as a bearer token when
// non-empty. Any transport error, non-200 answer or undecodable body is
// returned as an error so the caller can fall back.
func fetchOnboardingStatus(client *http.Client, baseURL, token string) (onboardingStatusResult, error) {
	var result onboardingStatusResult
	req, err := http.NewRequest(http.MethodGet, baseURL+onboardingStatusPath, nil)
	if err != nil {
		return result, err
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := client.Do(req)
	if err != nil {
		return result, err
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return result, err
	}
	if resp.StatusCode != http.StatusOK {
		return result, fmt.Errorf("onboarding status: HTTP %d", resp.StatusCode)
	}
	if err := json.Unmarshal(body, &result); err != nil {
		return result, fmt.Errorf("onboarding status: %w", err)
	}
	return result, nil
}

// quickstartWebPath chooses the web path `scion server start` prints and
// opens:
//   - the hub answered: "/onboarding" when onboarding is not complete, ""
//     (the web root) when it is;
//   - the hub did not answer (statusErr != nil, or the server was not ready
//     so it was not asked): fall back to the legacy signal, "/onboarding"
//     only when the global settings file did not exist before start
//     (settingsMissingBeforeStart).
func quickstartWebPath(status onboardingStatusResult, statusErr error, settingsMissingBeforeStart bool) string {
	if statusErr != nil {
		if settingsMissingBeforeStart {
			return onboardingWizardPath
		}
		return ""
	}
	if !status.Complete {
		return onboardingWizardPath
	}
	return ""
}

// workstationBrokerRegistryCheck decides whether a workstation server may
// start without an image_registry configured. The caller runs it in
// workstation mode only; hosted mode keeps its fail-fast at broker start.
//   - broker disabled: no-op.
//   - hub or web enabled (co-located workstation server): allow startup even
//     without image_registry so the onboarding wizard can save it via
//     PUT /api/v1/system/registry and live-update the running broker through the
//     co-located settings overlay without requiring a server restart.
//   - broker-only (nothing else is running and no wizard is reachable):
//     require image_registry and fail fast.
func workstationBrokerRegistryCheck(brokerEnabled, hubOrWebEnabled bool, registryCheck func() error) error {
	if !brokerEnabled || hubOrWebEnabled {
		return nil
	}
	return registryCheck()
}

// browserAutoOpenAllowed is the rule for opening the browser from
// `scion server start`: SCION_NO_BROWSER is unset, stdout is an interactive
// terminal, and the environment is not headless.
func browserAutoOpenAllowed() bool {
	return browserAutoOpenAllowedFor(os.Getenv("SCION_NO_BROWSER"), util.IsTerminal(), util.IsHeadlessEnvironment())
}

// browserAutoOpenAllowedFor is browserAutoOpenAllowed with its inputs passed
// in, for tests.
func browserAutoOpenAllowedFor(noBrowserEnv string, interactive, headless bool) bool {
	return noBrowserEnv == "" && interactive && !headless
}

// quickstartStatusAllowed reports whether the onboarding status request,
// which carries the server's dev or settings token, may be sent to baseURL:
// only when the hub transport rule accepts it (a loopback or TLS URL).
func quickstartStatusAllowed(baseURL string) bool {
	return hubsync.CheckUserHubTransport(baseURL) == nil
}

// Test seams for printWorkstationQuickstart, so unit tests never wait on or
// talk to a real server.
var (
	quickstartWaitReady        = waitForServerReady
	quickstartReadyTimeout     = 20 * time.Second
	quickstartBrowserAllowed   = browserAutoOpenAllowed
	quickstartOpenBrowser      = util.OpenBrowser
	quickstartStdoutIsTerminal = stdoutIsTerminal
)

// quickstartDevSignIn sends the server's dev token to baseURL. It does not
// apply the transport rule itself; its caller checks quickstartStatusAllowed
// first (the hub constructor source scan in hub_transport_policy_test.go
// keeps it so).
var quickstartDevSignIn = func(baseURL, token, next string) (devSignInLink, error) {
	return requestDevSignInLink(&http.Client{Timeout: 5 * time.Second}, baseURL, token, next)
}

// quickstartOnboardingStatus sends the server's token to baseURL. It does not
// apply the transport rule itself; its caller checks quickstartStatusAllowed
// first (the hub constructor source scan in hub_transport_policy_test.go keeps
// it so).
var quickstartOnboardingStatus = func(baseURL, token string) (onboardingStatusResult, error) {
	return fetchOnboardingStatus(&http.Client{Timeout: 5 * time.Second}, baseURL, token)
}
