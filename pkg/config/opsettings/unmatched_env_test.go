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
	"os"
	"path/filepath"
	"reflect"
	"strconv"
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
		// field the hub ignores (ptone/scion#1081, ptone/scion#1284).
		"SCION_SERVER_HUB_ADMIN_EMAILS":           "SCION_SERVER_HUB_ADMINEMAILS",
		"SCION_SEED_SERVER_HUB_ADMIN_EMAILS":      "SCION_SEED_SERVER_HUB_ADMINEMAILS",
		"SCION_SERVER_HUB_READ_TIMEOUT":           "SCION_SERVER_HUB_READTIMEOUT",
		"SCION_SEED_SERVER_HUB_STALLED_THRESHOLD": "SCION_SEED_SERVER_HUB_STALLEDTHRESHOLD",
		"SCION_SEED_IMAGE_REGISTRY":               "SCION_SEED_IMAGEREGISTRY",
		// Former schema names.
		"SCION_SERVER_BROKER_PORT":           "SCION_SERVER_RUNTIMEBROKER_PORT",
		"SCION_SERVER_BROKER_BROKERID":       "SCION_SERVER_BROKER_BROKER_ID",
		"SCION_SERVER_HUB_ADMINEMAIL":        "SCION_SERVER_HUB_ADMINEMAILS",
		"SCION_SERVER_HUB_ID":                "SCION_SERVER_HUB_HUBID",
		"SCION_SERVER_AUTH_USER_ACCESS_MODE": "SCION_SERVER_AUTH_USERACCESSMODE",
		// VersionedSettings spellings of CORS keys the hub reads from the
		// flattened GlobalConfig fields.
		"SCION_SERVER_HUB_CORS_ENABLED":         "SCION_SERVER_HUB_CORSENABLED",
		"SCION_SERVER_HUB_CORS_MAX_AGE":         "SCION_SERVER_HUB_CORSMAXAGE",
		"SCION_SERVER_HUB_CORS_ALLOWED_ORIGINS": "SCION_SERVER_HUB_CORSALLOWEDORIGINS",
		"SCION_SERVER_BROKER_CORS_ENABLED":      "SCION_SERVER_RUNTIMEBROKER_CORSENABLED",
		"SCION_SERVER_BROKER_CORS_MAX_AGE":      "SCION_SERVER_RUNTIMEBROKER_CORSMAXAGE",
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
		"SCION_SERVER_ENV",           // binds in VersionedSettings, never read
		"SCION_SERVER_LOG_FORMAT",
		// No SEED spelling maps to auto_expose_ports.enabled (no snake-case
		// mapping for autoexposeports); seed it from settings.yaml.
		"SCION_SEED_AUTO_EXPOSE_PORTS_ENABLED",
		"SCION_SEED_SERVER_HUB_IMAGEREGISTRY", // image_registry is top-level
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
	for _, name := range config.DirectServerEnvNameList() {
		environ = append(environ, name+"=x")
	}
	environ = append(environ,
		"SCION_SERVER_OAUTH_WEB_GOOGLE_CLIENTID=x",
		"SCION_SERVER_DATABASE_URL=x",
		"SCION_SEED_SERVER_HUB_ADMINEMAILS=x",
		"SCION_SEED_SERVER_AUTH_DEFAULTUSERROLE=x",
		"SCION_SEED_TELEMETRY_ENABLED=x",
		"SCION_SEED_IMAGEREGISTRY=x",
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

// TestFindUnmatchedSettingsEnv_CORSHintsBind checks that the CORS hints are
// not just accepted by the detector but set the GlobalConfig fields the hub
// reads, on both load paths: CORSENABLED (set opposite to its baseline),
// CORSMAXAGE, and CORSALLOWEDORIGINS with a comma-separated list.
func TestFindUnmatchedSettingsEnv_CORSHintsBind(t *testing.T) {
	origins := []string{"https://a.example.com", "https://b.example.com"}
	for _, mode := range []string{"legacy", "settings"} {
		t.Run(mode, func(t *testing.T) {
			home := t.TempDir()
			t.Setenv("HOME", home)
			if mode == "settings" {
				if err := os.MkdirAll(filepath.Join(home, ".scion"), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(home, ".scion", "settings.yaml"),
					[]byte("schema_version: \"1\"\nserver:\n  mode: workstation\n"), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			configDir := t.TempDir()
			base, err := config.LoadGlobalConfig(configDir)
			if err != nil {
				t.Fatal(err)
			}
			wantHubEnabled := !base.Hub.CORSEnabled
			wantBrokerEnabled := !base.RuntimeBroker.CORSEnabled

			t.Setenv("SCION_SERVER_HUB_CORSENABLED", strconv.FormatBool(wantHubEnabled))
			t.Setenv("SCION_SERVER_RUNTIMEBROKER_CORSENABLED", strconv.FormatBool(wantBrokerEnabled))
			t.Setenv("SCION_SERVER_HUB_CORSMAXAGE", "4243")
			t.Setenv("SCION_SERVER_RUNTIMEBROKER_CORSMAXAGE", "4244")
			t.Setenv("SCION_SERVER_HUB_CORSALLOWEDORIGINS", strings.Join(origins, ","))
			t.Setenv("SCION_SERVER_RUNTIMEBROKER_CORSALLOWEDORIGINS", strings.Join(origins, ","))
			gc, err := config.LoadGlobalConfig(configDir)
			if err != nil {
				t.Fatal(err)
			}
			if gc.Hub.CORSEnabled != wantHubEnabled || gc.RuntimeBroker.CORSEnabled != wantBrokerEnabled {
				t.Errorf("CORSEnabled hub=%v broker=%v, want %v/%v", gc.Hub.CORSEnabled, gc.RuntimeBroker.CORSEnabled, wantHubEnabled, wantBrokerEnabled)
			}
			if gc.Hub.CORSMaxAge != 4243 || gc.RuntimeBroker.CORSMaxAge != 4244 {
				t.Errorf("CORSMaxAge hub=%d broker=%d, want 4243/4244", gc.Hub.CORSMaxAge, gc.RuntimeBroker.CORSMaxAge)
			}
			if !reflect.DeepEqual(gc.Hub.CORSAllowedOrigins, origins) {
				t.Errorf("Hub.CORSAllowedOrigins = %q, want %q", gc.Hub.CORSAllowedOrigins, origins)
			}
			if !reflect.DeepEqual(gc.RuntimeBroker.CORSAllowedOrigins, origins) {
				t.Errorf("RuntimeBroker.CORSAllowedOrigins = %q, want %q", gc.RuntimeBroker.CORSAllowedOrigins, origins)
			}
		})
	}
}

// TestSeedImageRegistry_ReachesBootstrap checks the documented seed name for
// image_registry (reference/admin-settings.md) lands on the registry key.
func TestSeedImageRegistry_ReachesBootstrap(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("SCION_SEED_IMAGEREGISTRY", "registry.example.com/scion")
	if got := config.LoadBootstrapKoanf().String("image_registry"); got != "registry.example.com/scion" {
		t.Errorf("bootstrap image_registry = %q, want registry.example.com/scion", got)
	}
	if !IsLayer1Key("image_registry") {
		t.Error("image_registry is not a Layer-1 key")
	}
}

// TestFindUnmatchedSettingsEnv_LogLevelNote checks the LOG_LEVEL warning
// does not overstate SCION_LOG_LEVEL: the hint names the reload-only
// setting spelling and the note gives the boot-time controls.
func TestFindUnmatchedSettingsEnv_LogLevelNote(t *testing.T) {
	u, ok := unmatchedByName([]string{"SCION_SERVER_LOG_LEVEL=debug"})["SCION_SERVER_LOG_LEVEL"]
	if !ok {
		t.Fatal("SCION_SERVER_LOG_LEVEL not flagged")
	}
	if u.Suggestion != "SCION_SERVER_LOGLEVEL" {
		t.Errorf("suggestion = %q, want SCION_SERVER_LOGLEVEL", u.Suggestion)
	}
	for _, want := range []string{"no boot-time override", "file-mode reload", "--debug", "SCION_LOG_LEVEL=debug"} {
		if !strings.Contains(u.Note, want) {
			t.Errorf("note %q lacks %q", u.Note, want)
		}
	}
	var buf bytes.Buffer
	config.WarnUnmatchedSettingsEnv(slog.New(slog.NewTextHandler(&buf, nil)), []string{"SCION_SERVER_LOG_LEVEL=debug"}, IsLayer1Key)
	if !strings.Contains(buf.String(), "note=") {
		t.Errorf("warning lacks the note:\n%s", buf.String())
	}
}
