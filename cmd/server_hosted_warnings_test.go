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
	"log"
	"log/slog"
	"os"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/stretchr/testify/assert"
)

// captureStartupLogs runs fn with the standard logger and the default slog
// logger writing to a buffer, and returns what they logged.
func captureStartupLogs(t *testing.T, fn func()) string {
	t.Helper()
	var buf bytes.Buffer
	prevOut, prevFlags := log.Writer(), log.Flags()
	prevSlog := slog.Default()
	log.SetOutput(&buf)
	log.SetFlags(0)
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	defer func() {
		log.SetOutput(prevOut)
		log.SetFlags(prevFlags)
		slog.SetDefault(prevSlog)
	}()
	fn()
	return buf.String()
}

// TestValidateHostedBasic_BrokerOnlySkipsSessionSecretWarning covers #3605:
// a broker-only daemon (runtime-broker start runs server start --hosted
// --enable-runtime-broker) must not warn about the hub/web session secret.
func TestValidateHostedBasic_BrokerOnlySkipsSessionSecretWarning(t *testing.T) {
	prevHosted, prevHub, prevWeb, prevSecret := hostedMode, enableHub, enableWeb, webSessionSecret
	t.Cleanup(func() { hostedMode, enableHub, enableWeb, webSessionSecret = prevHosted, prevHub, prevWeb, prevSecret })
	t.Setenv("SCION_SERVER_SESSION_SECRET", "")
	t.Setenv("SESSION_SECRET", "")
	_ = os.Unsetenv("SCION_SERVER_SESSION_SECRET")
	_ = os.Unsetenv("SESSION_SECRET")
	hostedMode, webSessionSecret = true, ""
	cfg := &config.GlobalConfig{}

	enableHub, enableWeb = false, false
	logs := captureStartupLogs(t, func() { validateHostedBasic(cfg) })
	assert.NotContains(t, logs, "session secret", "broker-only startup")

	enableHub, enableWeb = true, false
	logs = captureStartupLogs(t, func() { validateHostedBasic(cfg) })
	assert.Contains(t, logs, "no session secret set", "hub startup still warns")

	enableHub, enableWeb = false, true
	logs = captureStartupLogs(t, func() { validateHostedBasic(cfg) })
	assert.Contains(t, logs, "no session secret set", "web startup still warns")
}
