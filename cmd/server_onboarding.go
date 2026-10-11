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

// brokerSkippedForRegistry reports whether a workstation server that was
// asked to run its co-located broker started without it because no
// image_registry is configured (see workstationBrokerRegistryDegrade). It
// uses the server's own registry predicate (localCheck, i.e.
// requireImageRegistryForBroker), which covers every source the server
// checks. When the hub answered and reports an embedded broker, the broker
// is running and there is nothing to report.
func brokerSkippedForRegistry(brokerEnabled bool, status onboardingStatusResult, statusErr error, localCheck func() error) bool {
	if !brokerEnabled {
		return false
	}
	if statusErr == nil && status.EmbeddedBrokerID != "" {
		return false
	}
	return localCheck() != nil
}

// brokerSkippedNotice is what `scion server start` prints next to the web URL
// when the workstation broker was not started for lack of an image registry.
// The broker cannot be started in place once the registry is set (the
// runtime reload hook only swaps the engine of a broker that is already
// running), so the fix ends with a restart.
const brokerSkippedNotice = "Runtime broker not started: image_registry is not configured.\n" +
	"  Set it in the setup wizard, or run one of:\n" +
	"    scion config set --global image_registry <your-registry>\n" +
	"    export SCION_IMAGE_REGISTRY=<your-registry>\n" +
	"  then run 'scion server restart' to start the broker."

// workstationBrokerRegistryDegrade decides what a workstation server does
// when its co-located broker is enabled but registryCheck
// (requireImageRegistryForBroker) fails. The caller runs it in workstation
// mode only; hosted mode keeps its fail-fast at broker start.
//   - hub or web enabled: return disableBroker=true and no error; the hub and
//     web still start so the onboarding wizard can set the registry, and the
//     broker is not started.
//   - broker-only (nothing else would be left running): return the error;
//     the server fails fast as before.
//
// It is a no-op when the broker is not enabled or the check passes.
func workstationBrokerRegistryDegrade(brokerEnabled, hubOrWebEnabled bool, registryCheck func() error) (disableBroker bool, err error) {
	if !brokerEnabled {
		return false, nil
	}
	checkErr := registryCheck()
	if checkErr == nil {
		return false, nil
	}
	if !hubOrWebEnabled {
		return false, checkErr
	}
	return true, nil
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

// Test seams for printWorkstationQuickstart, so unit tests never wait on or
// talk to a real server.
var (
	quickstartWaitReady        = waitForServerReady
	quickstartReadyTimeout     = 20 * time.Second
	quickstartOnboardingStatus = func(baseURL, token string) (onboardingStatusResult, error) {
		return fetchOnboardingStatus(&http.Client{Timeout: 5 * time.Second}, baseURL, token)
	}
	quickstartBrowserAllowed = browserAutoOpenAllowed
	quickstartOpenBrowser    = util.OpenBrowser
)
