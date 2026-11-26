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

// UsageLiveSource identifies the Track 1 live usage metering mechanism
// declared by a harness bundle's usage.yaml sidecar.
type UsageLiveSource string

const (
	UsageLiveSourceNative       UsageLiveSource = "native"
	UsageLiveSourceHooks        UsageLiveSource = "hooks"
	UsageLiveSourceSQLiteNative UsageLiveSource = "sqlite_native"
	UsageLiveSourceFileTailer   UsageLiveSource = "file_tailer"
	UsageLiveSourceNone         UsageLiveSource = "none"
)

// ValidUsageLiveSource reports whether s is a known usage.live.source value.
func ValidUsageLiveSource(s UsageLiveSource) bool {
	switch s {
	case UsageLiveSourceNative,
		UsageLiveSourceHooks,
		UsageLiveSourceSQLiteNative,
		UsageLiveSourceFileTailer,
		UsageLiveSourceNone:
		return true
	default:
		return false
	}
}

// HarnessUsageLiveCapabilities describes Track 1 live usage metering
// capabilities declared under usage.live in a harness bundle's usage.yaml.
type HarnessUsageLiveCapabilities struct {
	Source                  UsageLiveSource `json:"source" yaml:"source" koanf:"source"`
	TokenTypes              []string        `json:"token_types,omitempty" yaml:"token_types,omitempty" koanf:"token_types"`
	NotApplicableTokenTypes []string        `json:"not_applicable_token_types,omitempty" yaml:"not_applicable_token_types,omitempty" koanf:"not_applicable_token_types"`
	CountsFailedCalls       bool            `json:"counts_failed_calls" yaml:"counts_failed_calls" koanf:"counts_failed_calls"`
}

// HarnessUsageCapabilities describes usage metering capabilities declared in
// a harness bundle's usage.yaml sidecar (config.UsageSidecarFile).
type HarnessUsageCapabilities struct {
	SchemaVersion int                          `json:"schema_version" yaml:"schema_version" koanf:"schema_version"`
	Live          HarnessUsageLiveCapabilities `json:"live" yaml:"live" koanf:"live"`
}
