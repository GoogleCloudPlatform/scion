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
	"bytes"
	"errors"
	"log/slog"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
)

// RequiredSettingsFindings is the settings.transport_proxy rule with the
// check name set, warnings only.
func TestRequiredSettingsFindings(t *testing.T) {
	if got := RequiredSettingsFindings(hubCfg("https://hub.example.com", true, nil)); len(got) != 0 {
		t.Fatalf("findings = %+v, want none when the setting is set", got)
	}
	got := RequiredSettingsFindings(hubCfg("https://hub.example.com", false, nil))
	if len(got) != 1 {
		t.Fatalf("findings = %+v, want one warning", got)
	}
	assertTransportProxyWarning(t, got[0])
	if got[0].Check != TransportProxyCheckName {
		t.Errorf("check = %q, want %q", got[0].Check, TransportProxyCheckName)
	}
}

// LoadRequiredSettingsFindings reads the given path through the seam, and
// a config that does not load is one warning that does not quote the error.
func TestLoadRequiredSettingsFindings(t *testing.T) {
	old := loadTransportProxyConfig
	t.Cleanup(func() { loadTransportProxyConfig = old })

	var gotPath string
	loadTransportProxyConfig = func(path string) (*config.GlobalConfig, error) {
		gotPath = path
		return hubCfg("https://hub.example.com", false, nil), nil
	}
	got := LoadRequiredSettingsFindings("/srv/scion")
	if gotPath != "/srv/scion" || len(got) != 1 {
		t.Fatalf("path %q findings %+v, want the given path and one warning", gotPath, got)
	}
	assertTransportProxyWarning(t, got[0])

	loadTransportProxyConfig = func(string) (*config.GlobalConfig, error) {
		return nil, errors.New("yaml: secret-value-xyz is not a list")
	}
	var logs bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })
	got = LoadRequiredSettingsFindings("")
	slog.SetDefault(prev)
	if strings.Contains(logs.String(), "secret-value-xyz") || !strings.Contains(logs.String(), "error_type=") {
		t.Errorf("log %q quotes the error text, or does not name its type", logs.String())
	}
	if len(got) != 1 || got[0].Severity != SeverityWarn || got[0].Code != TransportProxyCheckName+".config_not_loaded" {
		t.Fatalf("findings = %+v, want one config_not_loaded warning", got)
	}
	if strings.Contains(got[0].Message+got[0].Remediation, "secret-value-xyz") {
		t.Errorf("finding %+v quotes the underlying error", got[0])
	}
}

// The log section names each finding as a warning with its remediation,
// or says none is unset.
func TestWriteSettingsCheck(t *testing.T) {
	var none bytes.Buffer
	WriteSettingsCheck(&none, nil)
	if !strings.Contains(none.String(), "==> Settings check") || !strings.Contains(none.String(), "No required settings are unset.") {
		t.Errorf("empty section = %q", none.String())
	}

	var b bytes.Buffer
	WriteSettingsCheck(&b, RequiredSettingsFindings(hubCfg("https://hub.example.com", false, nil)))
	out := b.String()
	for _, want := range []string{
		"==> Settings check",
		"WARNING [" + TransportProxyCheckName + ".trusted_proxies_not_set]: ",
		config.TrustedProxiesKey,
		"  Remediation: ",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("section %q does not contain %q", out, want)
		}
	}
	if strings.Contains(out, "No required settings are unset.") {
		t.Errorf("section %q says none is unset", out)
	}
}
