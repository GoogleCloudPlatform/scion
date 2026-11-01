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
	"fmt"
	"io"
	"log/slog"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
)

// RequiredSettingsFindings lists the settings a hub with server config cfg
// needs but does not set, as warning findings with their check name. It is
// the settings.transport_proxy rule (server.trusted_proxies not set while
// the public URL is https), shared by "scion server preflight" and the
// maintenance update operations. It never blocks: every finding is a
// warning.
func RequiredSettingsFindings(cfg *config.GlobalConfig) []Finding {
	out := transportProxyFindings(cfg)
	for i := range out {
		out[i].Check = TransportProxyCheckName
	}
	return out
}

// LoadRequiredSettingsFindings loads the server config at path (the one the
// restarted hub will read; "" is the default location) and returns
// RequiredSettingsFindings, or one warning finding when the config does not
// load. Only the load error's type is logged, never its text, which may
// quote settings values.
func LoadRequiredSettingsFindings(path string) []Finding {
	cfg, err := loadTransportProxyConfig(path)
	if err != nil {
		slog.Warn("settings check: the server config does not load", "error_type", fmt.Sprintf("%T", err))
		return []Finding{transportProxyConfigNotLoaded()}
	}
	return RequiredSettingsFindings(cfg)
}

// WriteSettingsCheck writes the "==> Settings check" section of a
// maintenance update operation's log: each finding as a warning with its
// remediation, or a line saying none is unset. It only reports; the
// operation goes on either way.
func WriteSettingsCheck(w io.Writer, findings []Finding) {
	_, _ = fmt.Fprintln(w, "==> Settings check (required settings that are unset; warnings only)")
	if len(findings) == 0 {
		_, _ = fmt.Fprintln(w, "No required settings are unset.")
		_, _ = fmt.Fprintln(w)
		return
	}
	for _, f := range findings {
		_, _ = fmt.Fprintf(w, "WARNING [%s]: %s\n", f.Code, f.Message)
		if f.Remediation != "" {
			_, _ = fmt.Fprintf(w, "  Remediation: %s\n", f.Remediation)
		}
	}
	_, _ = fmt.Fprintln(w)
}
