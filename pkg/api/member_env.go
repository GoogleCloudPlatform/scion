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

package api

// Member-settable reserved env names.
//
// Env names starting with SCION_ or GCE_METADATA_ are reserved for scion's
// own control-plane variables. Config written by project members may not
// set them on a hosted server (pkg/hostedguard) and an agent config edit may
// not add, change or remove them (the Hub's PATCH check), except for the
// names in memberSettableEnv, each for the sources its scope lists. This
// list is the one source of truth for both checks; a new reserved name is
// refused for members until it is added here, with its reason.

// ReservedEnvPrefixes returns the env name prefixes reserved for scion's own
// control-plane variables. It is the one list: pkg/secret (env secret
// targets, the Hub's config-edit check) and pkg/hostedguard use it.
func ReservedEnvPrefixes() []string {
	return []string{"SCION_", "GCE_METADATA_"}
}

// MemberEnvScope is a set of sources from which members may set a reserved
// env name.
type MemberEnvScope uint8

const (
	// MemberEnvCreate is agent config written at create (the create
	// request's config.env and the agent's stored inline config).
	MemberEnvCreate MemberEnvScope = 1 << iota
	// MemberEnvEdit is an agent config edit (PATCH and held edits).
	MemberEnvEdit
	// MemberEnvHarnessConfig is a member-written harness config (project
	// scope, or bundled in a template).
	MemberEnvHarnessConfig
)

// MemberSettableEnvName is one reserved env name members may set.
type MemberSettableEnvName struct {
	Name   string
	Reason string
	Scope  MemberEnvScope
}

var memberSettableEnv = []MemberSettableEnvName{
	{EnvAutoExposePorts, "auto-expose control (configure page, create config)", MemberEnvCreate | MemberEnvEdit | MemberEnvHarnessConfig},
	{"SCION_AUTO_EXPOSE_MODE", "auto-expose control", MemberEnvCreate | MemberEnvEdit | MemberEnvHarnessConfig},
	{"SCION_AUTO_EXPOSE_PORTS_LIST", "auto-expose control", MemberEnvCreate | MemberEnvEdit | MemberEnvHarnessConfig},
	{"SCION_AUTO_EXPOSE_INTERVAL", "auto-expose control", MemberEnvCreate | MemberEnvEdit | MemberEnvHarnessConfig},
	// Written by the CLI into the create request's config.env
	// (startAgentViaHub in cmd/common.go).
	{"SCION_LOG_LEVEL", "CLI --agent-log-level", MemberEnvCreate},
	{"SCION_TELEMETRY_ENABLED", "CLI --enable-telemetry / --disable-telemetry", MemberEnvCreate},
	// Set by the shipped Codex harness config (harnesses/codex/config.yaml);
	// it tunes only the agent's own liveness sampling.
	{"SCION_LIVENESS_HARNESS_ROOT_DEPTH", "Codex harness config liveness depth", MemberEnvHarnessConfig},
}

// MemberSettableEnv returns a copy of the member-settable reserved env
// names.
func MemberSettableEnv() []MemberSettableEnvName {
	return append([]MemberSettableEnvName(nil), memberSettableEnv...)
}

// IsMemberSettableEnv reports whether members may set the reserved env name
// from a source in scope.
func IsMemberSettableEnv(name string, scope MemberEnvScope) bool {
	for _, e := range memberSettableEnv {
		if e.Name == name {
			return e.Scope&scope != 0
		}
	}
	return false
}

// MemberSettableEnvNames returns the names members may set from a source in
// scope, as a set.
func MemberSettableEnvNames(scope MemberEnvScope) map[string]bool {
	out := map[string]bool{}
	for _, e := range memberSettableEnv {
		if e.Scope&scope != 0 {
			out[e.Name] = true
		}
	}
	return out
}
