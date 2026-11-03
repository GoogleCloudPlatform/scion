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
	"fmt"
	"os"

	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/hub"
	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/log"
	"github.com/GoogleCloudPlatform/scion/pkg/stagedsecrets"
)

// envHubTokenStaged marks, across init's environ-purge re-exec, that this
// init wrote the agent token file from the staged payload (so it removes
// it at a clean exit). It carries no credential; the runtimes reserve it.
const envHubTokenStaged = stagedsecrets.HubTokenStagedEnvVar

// stageHubToken writes the staged hub credential to the canonical token
// file through hub.WriteTokenFile: the parent directory is opened without
// following links, the temporary file is created O_CREAT|O_EXCL|O_NOFOLLOW
// at 0600, fchown'd to the agent user and renamed over the leaf.
//
// On a restart that reuses the payload baked in at instance creation, the
// token file may already hold a credential the refresh loop got later. It
// is kept (kept=true) only when both it and the staged value are readable
// three-part tokens and its iat is strictly newer. Anything else (no file,
// an unreadable or malformed file, a staged value without an iat) writes
// the staged value; the comparison itself never fails the start.
func stageHubToken(staged string, uid, gid int) (kept bool, err error) {
	if existing := hub.ReadTokenFile(); existing != "" {
		existingIAT, okExisting := hub.TokenIssuedAt(existing)
		stagedIAT, okStaged := hub.TokenIssuedAt(staged)
		if okExisting && okStaged && existingIAT.After(stagedIAT) {
			return true, nil
		}
	}
	// An existing file that cannot be read loses. The write below refuses
	// to replace a leaf it cannot open, so a regular single-link file the
	// guarded reader classed as unreadable is unlinked first (fstatat and
	// unlinkat on the parent, never following the leaf). A link or a
	// non-regular entry is not removed: the write refuses it and the start
	// fails closed.
	if hub.CheckTokenFile() == hub.TokenFileUnreadable {
		if err := hub.RemoveUnreadableRegularTokenFile(); err != nil {
			return false, fmt.Errorf("remove the unreadable token file: %w", err)
		}
	}
	if err := runWriteTokenFile(staged, uid, gid); err != nil {
		return false, err
	}
	return false, nil
}

// runWriteTokenFile is hub.WriteTokenFile; a test may replace it.
var runWriteTokenFile = hub.WriteTokenFile

// finishStagedHubToken runs as init returns, after the final status
// report, for a token file this init staged. After a clean final outcome (a
// clean exit or a requested stop, which the platform does not restart) it
// removes the file. Otherwise it keeps it: a container restart re-stages
// the creation-time credential, and stageHubToken then keeps this file
// when the refresh loop made it newer. A kept file stays on the agent's
// own home until the next start replaces it or the agent is deleted.
func finishStagedHubToken(cleanExit bool) {
	if !cleanExit {
		log.Info("Kept the staged hub credential file for a restart (the agent did not exit cleanly)")
		return
	}
	if err := hub.RemoveTokenFile(); err != nil {
		log.Error("Failed to remove the staged hub credential file: %v", err)
		return
	}
	log.Info("Removed the staged hub credential file")
}

// logHubOperatingMode logs the operating mode once the hub client is
// built: an ERROR naming what is missing when a hub endpoint is set but
// the client is not configured, otherwise the mode line.
func logHubOperatingMode(client *hub.Client) {
	isErr, line := hubModeLogLine(hub.OperatingMode(), client != nil && client.IsConfigured(), hubEndpointForLog(), hub.HubClientUnconfiguredReason)
	if isErr {
		log.Error("%s", line)
		return
	}
	log.Info("%s", line)
}

func hubEndpointForLog() string {
	if v := os.Getenv(hub.EnvHubEndpoint); v != "" {
		return v
	}
	return os.Getenv(hub.EnvHubURL)
}

// hubModeLogLine builds the operating-mode log line. "hub-connected" (or
// "hosted") is claimed only for a configured client; with an endpoint set
// and no configured client the line is an error naming the missing inputs
// (reason never returns a value, only conditions).
func hubModeLogLine(mode hub.Mode, configured bool, endpoint string, reason func() string) (isError bool, line string) {
	if mode == hub.ModeLocal {
		return false, "Operating mode: local (no hub configured)"
	}
	if !configured {
		why := reason()
		if why == "" {
			why = "the hub client could not be built"
		}
		return true, fmt.Sprintf("Hub endpoint is set (%s) but the hub client is not configured: %s. "+
			"Hub status, heartbeat, readiness reporting and the conduit are disabled", endpoint, why)
	}
	return false, fmt.Sprintf("Operating mode: %s (endpoint: %s)", mode, endpoint)
}
