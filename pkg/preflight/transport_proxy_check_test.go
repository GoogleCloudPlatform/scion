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

package preflight

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
)

// The warning fires only for an https public URL with the setting not set.
func TestTransportProxyFindings(t *testing.T) {
	for name, tc := range map[string]struct {
		cfg      *config.GlobalConfig
		wantWarn bool
	}{
		"nil config":             {cfg: nil},
		"https, not set":         {cfg: hubCfg("https://hub.example.com", false, nil), wantWarn: true},
		"https upper case":       {cfg: hubCfg("HTTPS://hub.example.com", false, nil), wantWarn: true},
		"https padded":           {cfg: hubCfg("  https://hub.example.com  ", false, nil), wantWarn: true},
		"https, set to list":     {cfg: hubCfg("https://hub.example.com", true, []string{"10.0.0.0/8"})},
		"https, set to empty":    {cfg: hubCfg("https://hub.example.com", true, nil)},
		"http, not set":          {cfg: hubCfg("http://hub.example.com", false, nil)},
		"no public URL, not set": {cfg: hubCfg("", false, nil)},
		"unparseable URL":        {cfg: hubCfg("https://[::1", false, nil)},
		// A list without the flag (a config built in code) is still not
		// "set"; the check reads only the flag.
		"https, list without flag": {cfg: hubCfg("https://hub.example.com", false, []string{"10.0.0.0/8"}), wantWarn: true},
	} {
		t.Run(name, func(t *testing.T) {
			got := transportProxyFindings(tc.cfg)
			if !tc.wantWarn {
				if len(got) != 0 {
					t.Fatalf("findings = %+v, want none", got)
				}
				return
			}
			if len(got) != 1 {
				t.Fatalf("findings = %+v, want one warning", got)
			}
			assertTransportProxyWarning(t, got[0])
		})
	}
}

// The check loads the config through the seam; a load error is a warning
// finding, not an error (which the registry would turn into a blocking
// could_not_run), and does not quote the error text (it may quote
// settings values).
func TestRunTransportProxyCheck_Seam(t *testing.T) {
	old := loadTransportProxyConfig
	t.Cleanup(func() { loadTransportProxyConfig = old })

	var gotPath string
	loadTransportProxyConfig = func(path string) (*config.GlobalConfig, error) {
		gotPath = path
		return hubCfg("https://hub.example.com", false, nil), nil
	}
	got, err := runTransportProxyCheck(context.Background(), Env{ServerConfigPath: "/srv/scion"})
	if err != nil {
		t.Fatal(err)
	}
	if gotPath != "/srv/scion" {
		t.Errorf("loaded %q, want the env's ServerConfigPath", gotPath)
	}
	if len(got) != 1 {
		t.Fatalf("findings = %+v, want one warning", got)
	}
	assertTransportProxyWarning(t, got[0])

	loadTransportProxyConfig = func(string) (*config.GlobalConfig, error) {
		return nil, errors.New("yaml: secret-value-xyz is not a list")
	}
	got, err = runTransportProxyCheck(context.Background(), Env{})
	if err != nil {
		t.Fatalf("err = %v, want a warning finding instead", err)
	}
	if len(got) != 1 || got[0].Severity != SeverityWarn || got[0].Code != TransportProxyCheckName+".config_not_loaded" {
		t.Fatalf("findings = %+v, want one config_not_loaded warning", got)
	}
	if strings.Contains(got[0].Message, "secret-value-xyz") || strings.Contains(got[0].Remediation, "secret-value-xyz") {
		t.Errorf("finding %+v quotes the underlying error", got[0])
	}
}

// End to end through LoadGlobalConfig: the key's presence in settings.yaml,
// even as [], silences the warning.
func TestRunTransportProxyCheck_SettingsFile(t *testing.T) {
	for name, tc := range map[string]struct {
		server   string
		wantWarn bool
	}{
		"https, key absent": {server: "  hub:\n    public_url: https://hub.example.com\n", wantWarn: true},
		"https, key empty":  {server: "  hub:\n    public_url: https://hub.example.com\n  trusted_proxies: []\n"},
		"https, key list":   {server: "  hub:\n    public_url: https://hub.example.com\n  trusted_proxies:\n    - 127.0.0.1\n    - ::1\n"},
		"http, key absent":  {server: "  hub:\n    public_url: http://hub.example.com\n"},
	} {
		t.Run(name, func(t *testing.T) {
			dir := transportProxyHome(t, "schema_version: \"1\"\nserver:\n"+tc.server)
			got, err := runTransportProxyCheck(context.Background(), Env{GlobalDir: dir, ServerConfigPath: dir})
			if err != nil {
				t.Fatal(err)
			}
			if !tc.wantWarn {
				if len(got) != 0 {
					t.Fatalf("findings = %+v, want none", got)
				}
				return
			}
			if len(got) != 1 {
				t.Fatalf("findings = %+v, want one warning", got)
			}
			assertTransportProxyWarning(t, got[0])
		})
	}
}

func hubCfg(endpoint string, set bool, list []string) *config.GlobalConfig {
	cfg := &config.GlobalConfig{TrustedProxies: list, TrustedProxiesSet: set}
	cfg.Hub.Endpoint = endpoint
	return cfg
}

func assertTransportProxyWarning(t *testing.T, f Finding) {
	t.Helper()
	if f.Severity != SeverityWarn {
		t.Errorf("severity = %q, want warn (the check never blocks)", f.Severity)
	}
	if f.Code != TransportProxyCheckName+".trusted_proxies_not_set" {
		t.Errorf("code = %q", f.Code)
	}
	for _, want := range []string{config.TrustedProxiesKey, "this host", "another host"} {
		if !strings.Contains(f.Message, want) {
			t.Errorf("message %q does not mention %q", f.Message, want)
		}
	}
	if !strings.Contains(f.Remediation, "[]") {
		t.Errorf("remediation %q does not name the [] opt-out", f.Remediation)
	}
}

// transportProxyHome isolates HOME, the global settings overlay and the
// SCION_SERVER_ and SCION_HUB_ environment, writes settings to a global
// settings file and returns its directory.
func transportProxyHome(t *testing.T, settings string) string {
	t.Helper()
	old := config.GetGlobalSettingsOverlay()
	t.Cleanup(func() { config.SetGlobalSettingsOverlay(old) })
	config.SetGlobalSettingsOverlay(nil)
	for _, kv := range os.Environ() {
		k, v, _ := strings.Cut(kv, "=")
		if strings.HasPrefix(k, "SCION_SERVER_") || strings.HasPrefix(k, "SCION_HUB_") {
			t.Setenv(k, v) // restored at cleanup
			if err := os.Unsetenv(k); err != nil {
				t.Fatal(err)
			}
		}
	}
	home := t.TempDir()
	t.Setenv("HOME", home)
	dir := filepath.Join(home, ".scion")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "settings.yaml"), []byte(settings), 0o600); err != nil {
		t.Fatal(err)
	}
	return dir
}
