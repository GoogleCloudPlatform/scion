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
	"log/slog"
	"os"
)

const (
	sessionSecretEnv       = "SCION_SERVER_SESSION_SECRET"
	legacySessionSecretEnv = "SESSION_SECRET"
)

// serverSecretEnvNames are the server secret env names that
// scrubServerSecretEnv removes from the process env once their values are in
// memory, so that no child process (git, container CLIs, plugins, agents)
// inherits them. Both peer secret spellings are listed; the conduit config is
// read from them by loadAndReconcileConfig, before the scrub.
var serverSecretEnvNames = []string{
	sessionSecretEnv,
	legacySessionSecretEnv,
	"SCION_SERVER_HUB_CONDUIT_PEERSECRET",
	"SCION_SERVER_HUB_CONDUIT_PEER_SECRET",
}

// capturedSessionSecret holds the session secret read from the env by
// scrubServerSecretEnv. resolveSessionSecret prefers it over the (by then
// empty) env.
var capturedSessionSecret string

// scrubServerSecretEnv reads the session secret from the env once, keeps it in
// memory, and then unsets every name in serverSecretEnvNames. It must run in
// the server process itself (never a daemon parent, whose child reads the
// secret from the env it inherits), after the config is loaded and before any
// child process is started.
func scrubServerSecretEnv() {
	capturedSessionSecret = sessionSecretFromEnv()
	for _, name := range serverSecretEnvNames {
		_ = os.Unsetenv(name)
	}
}

// sessionSecretFromEnv returns SCION_SERVER_SESSION_SECRET, falling back to
// the deprecated SESSION_SECRET with a warning.
func sessionSecretFromEnv() string {
	if v := os.Getenv(sessionSecretEnv); v != "" {
		return v
	}
	if v := os.Getenv(legacySessionSecretEnv); v != "" {
		slog.Warn("SESSION_SECRET is deprecated and will be removed; set SCION_SERVER_SESSION_SECRET instead")
		return v
	}
	return ""
}
