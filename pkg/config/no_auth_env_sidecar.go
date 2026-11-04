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

package config

import (
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"maps"

	"gopkg.in/yaml.v3"
)

// NoAuthEnvSidecarFile is the harness-config bundle file that declares the
// environment added to the container only when the agent runs in no-auth
// mode: a mapping with one key, `env`, a map of variable names to literal
// values. It fills HarnessNoAuthConfig.Env.
//
// It lives next to config.yaml instead of in config.yaml's no_auth block for
// the same reason as AgentPluginsSidecarFile: binaries built before it
// validate config.yaml against a schema whose no_auth block rejects unknown
// keys (ptone/scion#5196). Those binaries never read other bundle files, so
// they ignore it.
const NoAuthEnvSidecarFile = "no-auth-env.yaml"

// noAuthEnvSidecar is the on-disk shape of NoAuthEnvSidecarFile. Unknown keys
// are ignored so a later addition does not break this binary.
type noAuthEnvSidecar struct {
	Env map[string]string `yaml:"env"`
}

// applyNoAuthEnvSidecar reads the bundle's NoAuthEnvSidecarFile with read
// and records its env on entry.NoAuthConfig.Env. where locates the file in
// messages. A missing file leaves entry unchanged. An unreadable or invalid
// file, or one in a bundle without a no_auth block, is ignored with a
// warning: it never fails the load, and no env is added.
func applyNoAuthEnvSidecar(entry *HarnessConfigEntry, read func(name string) ([]byte, error), where string) {
	data, err := read(NoAuthEnvSidecarFile)
	if errors.Is(err, fs.ErrNotExist) {
		return
	}
	if err == nil {
		var s noAuthEnvSidecar
		if err = yaml.Unmarshal(data, &s); err == nil {
			if len(s.Env) == 0 {
				return
			}
			if entry.NoAuthConfig == nil {
				warnNoAuthEnvSidecar(where, errors.New("config.yaml has no no_auth block"))
				return
			}
			nac := *entry.NoAuthConfig
			nac.Env = maps.Clone(s.Env)
			entry.NoAuthConfig = &nac
			return
		}
	}
	warnNoAuthEnvSidecar(where, fmt.Errorf("invalid %s: %w", NoAuthEnvSidecarFile, err))
}

// warnNoAuthEnvSidecar logs, once per path and error, that a bundle's
// no-auth-env.yaml is ignored (see invalidSidecarWarned).
func warnNoAuthEnvSidecar(where string, err error) {
	if _, seen := invalidSidecarWarned.LoadOrStore(where+"\x00"+err.Error(), true); !seen {
		slog.Warn("ignoring harness-config no-auth-env.yaml; no no-auth env is added",
			"path", where, "error", err)
	}
}
