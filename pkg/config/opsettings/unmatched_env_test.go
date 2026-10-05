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

package opsettings

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
)

func unmatchedByName(environ []string) map[string]config.UnmatchedEnvName {
	out := map[string]config.UnmatchedEnvName{}
	for _, u := range config.FindUnmatchedSettingsEnv(environ, IsLayer1Key) {
		out[u.Name] = u
	}
	return out
}

func TestFindUnmatchedSettingsEnv_FlagsWithHint(t *testing.T) {
	cases := map[string]string{
		// Underscored multi-word names bind only to a VersionedSettings
		// field the hub ignores (ptone/scion#1081, #1284).
		"SCION_SERVER_HUB_ADMIN_EMAILS":           "SCION_SERVER_HUB_ADMINEMAILS",
		"SCION_SEED_SERVER_HUB_ADMIN_EMAILS":      "SCION_SEED_SERVER_HUB_ADMINEMAILS",
		"SCION_SERVER_HUB_READ_TIMEOUT":           "SCION_SERVER_HUB_READTIMEOUT",
		"SCION_SEED_SERVER_HUB_STALLED_THRESHOLD": "SCION_SEED_SERVER_HUB_STALLEDTHRESHOLD",
		// Former schema names.
		"SCION_SERVER_BROKER_PORT":           "SCION_SERVER_RUNTIMEBROKER_PORT",
		"SCION_SERVER_BROKER_BROKERID":       "SCION_SERVER_BROKER_BROKER_ID",
		"SCION_SERVER_HUB_ADMINEMAIL":        "SCION_SERVER_HUB_ADMINEMAILS",
		"SCION_SERVER_AUTH_USER_ACCESS_MODE": "SCION_SERVER_AUTH_USERACCESSMODE",
	}
	var environ []string
	for name := range cases {
		environ = append(environ, name+"=secret-value")
	}
	got := unmatchedByName(environ)
	for name, want := range cases {
		u, ok := got[name]
		if !ok {
			t.Errorf("%s not flagged", name)
			continue
		}
		if u.Suggestion != want {
			t.Errorf("%s: suggestion = %q, want %q", name, u.Suggestion, want)
		}
	}
	// Every suggestion must itself be accepted.
	var suggestions []string
	for _, want := range cases {
		suggestions = append(suggestions, want+"=x")
	}
	for _, u := range config.FindUnmatchedSettingsEnv(suggestions, IsLayer1Key) {
		t.Errorf("suggested name %s is itself flagged", u.Name)
	}
}

func TestFindUnmatchedSettingsEnv_FlagsWithoutHint(t *testing.T) {
	names := []string{
		"SCION_SERVER_DATABASE_MAX_OPEN_CONNS",
		"SCION_SERVER_NO_SUCH_SETTING",
		"SCION_SEED_SERVER_HUB_PORT", // Layer-0: seed values only seed Layer-1
	}
	var environ []string
	for _, n := range names {
		environ = append(environ, n+"=1")
	}
	got := unmatchedByName(environ)
	for _, n := range names {
		if _, ok := got[n]; !ok {
			t.Errorf("%s not flagged", n)
		}
	}
}

func TestFindUnmatchedSettingsEnv_AcceptsValidNames(t *testing.T) {
	var environ []string
	// Every SCION_SERVER_* name the schema advertises.
	for _, e := range collectSchemaEnvVars(t) {
		if strings.HasPrefix(e.EnvVar, "SCION_SERVER_") {
			environ = append(environ, e.EnvVar+"=x")
		}
	}
	// Names read directly with os.Getenv.
	for name := range config.DirectServerEnvNames {
		environ = append(environ, name+"=x")
	}
	environ = append(environ,
		"SCION_SERVER_OAUTH_WEB_GOOGLE_CLIENTID=x",
		"SCION_SERVER_DATABASE_URL=x",
		"SCION_SEED_SERVER_HUB_ADMINEMAILS=x",
		"SCION_SEED_SERVER_AUTH_DEFAULTUSERROLE=x",
		"SCION_SEED_TELEMETRY_ENABLED=x",
		// Unrelated prefixes are ignored entirely.
		"SCION_PROJECT=x",
		"HOME=/tmp",
	)
	for _, u := range config.FindUnmatchedSettingsEnv(environ, IsLayer1Key) {
		t.Errorf("valid name %s flagged (suggestion %q)", u.Name, u.Suggestion)
	}
}

func TestWarnUnmatchedSettingsEnv_LogsNamesNotValues(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, nil))
	config.WarnUnmatchedSettingsEnv(logger, []string{
		"SCION_SERVER_HUB_ADMIN_EMAILS=topsecret@example.com",
		"SCION_SERVER_HUB_ADMINEMAILS=alsosecret@example.com",
	}, IsLayer1Key)
	out := buf.String()
	if strings.Count(out, "level=WARN") != 1 {
		t.Errorf("want exactly one warning, got:\n%s", out)
	}
	if !strings.Contains(out, "SCION_SERVER_HUB_ADMIN_EMAILS") || !strings.Contains(out, "did_you_mean=SCION_SERVER_HUB_ADMINEMAILS") {
		t.Errorf("warning lacks name or hint:\n%s", out)
	}
	if strings.Contains(out, "secret") {
		t.Errorf("warning leaks an env value:\n%s", out)
	}
}
