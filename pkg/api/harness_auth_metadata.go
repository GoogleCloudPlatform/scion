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

// HarnessAuthMetadata contains declarative auth preflight metadata.
type HarnessAuthMetadata struct {
	DefaultType string                             `json:"default_type,omitempty" yaml:"default_type,omitempty" koanf:"default_type"`
	Types       map[string]HarnessAuthTypeMetadata `json:"types,omitempty" yaml:"types,omitempty" koanf:"types"`
	Autodetect  HarnessAuthAutodetect              `json:"autodetect,omitempty" yaml:"autodetect,omitempty" koanf:"autodetect"`
}

type HarnessAuthTypeMetadata struct {
	RequiredEnv   []HarnessAuthEnvRequirement  `json:"required_env,omitempty" yaml:"required_env,omitempty" koanf:"required_env"`
	RequiredFiles []HarnessAuthFileRequirement `json:"required_files,omitempty" yaml:"required_files,omitempty" koanf:"required_files"`
}

type HarnessAuthEnvRequirement struct {
	AnyOf []string `json:"any_of,omitempty" yaml:"any_of,omitempty" koanf:"any_of"`
}

type HarnessAuthFileRequirement struct {
	Name        string `json:"name,omitempty" yaml:"name,omitempty" koanf:"name"`
	Type        string `json:"type,omitempty" yaml:"type,omitempty" koanf:"type"`
	Description string `json:"description,omitempty" yaml:"description,omitempty" koanf:"description"`
	// TargetSuffix is the in-container projection target suffix. Used
	// together with the broker's home dir resolution, e.g. "/.claude/.credentials.json".
	TargetSuffix string `json:"target_suffix,omitempty" yaml:"target_suffix,omitempty" koanf:"target_suffix"`
	// Field maps this file requirement to the corresponding AuthConfig
	// struct field name (e.g. "ClaudeAuthFile"). Used by
	// OverlayFileSecretsFromConfig to set auth fields without hardcoded
	// switch statements.
	Field string `json:"field,omitempty" yaml:"field,omitempty" koanf:"field"`
	// AlternativeEnvKeys lists env vars that satisfy this file requirement
	// in lieu of the file itself (e.g. GOOGLE_APPLICATION_CREDENTIALS for
	// gcloud-adc).
	AlternativeEnvKeys []string `json:"alternative_env_keys,omitempty" yaml:"alternative_env_keys,omitempty" koanf:"alternative_env_keys"`
	// SkippedWhenGCPServiceAccountAssigned drops this requirement when a
	// GCP workload identity is attached, because the metadata server stands
	// in for the credential file.
	SkippedWhenGCPServiceAccountAssigned bool `json:"skipped_when_gcp_service_account_assigned,omitempty" yaml:"skipped_when_gcp_service_account_assigned,omitempty" koanf:"skipped_when_gcp_service_account_assigned"`
	// Required marks the file as a broker-side required secret: the broker
	// must locate it (via Hub secrets, CLI gather, or alternatives) before
	// dispatching the agent. When false the file is documentary -- the auth
	// type uses it, but the broker is not responsible for sourcing it (the
	// user mounts a locally-resolved file).
	Required bool `json:"required,omitempty" yaml:"required,omitempty" koanf:"required"`
}

type HarnessAuthAutodetect struct {
	Env   map[string]string `json:"env,omitempty" yaml:"env,omitempty" koanf:"env"`
	Files map[string]string `json:"files,omitempty" yaml:"files,omitempty" koanf:"files"`
}
