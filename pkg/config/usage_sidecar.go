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
	"os"
	"path"
	"path/filepath"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/telemetrycontract"
	"gopkg.in/yaml.v3"
)

// UsageSidecarFile is the harness-config bundle file that declares usage
// metering capabilities (Track 1 live metering and, in later phases, Track 2
// trajectory normalization metadata).
//
// It lives next to config.yaml instead of inside config.yaml for the same
// reason as AgentPluginsSidecarFile and NoAuthEnvSidecarFile: deployed
// binaries validate config.yaml against a strict schema that rejects unknown
// keys (ptone/scion#5196). Older binaries ignore unknown bundle files, and the
// file travels with the bundle on seeding and harness-config upgrade without
// requiring image rebuilds.
const UsageSidecarFile = "usage.yaml"

// UsageSidecarSchemaVersion is the supported schema_version of usage.yaml.
const UsageSidecarSchemaVersion = 1

// usageSidecarFileDoc is the on-disk shape of UsageSidecarFile. Unknown keys
// (including the future usage.trajectory section) are ignored by yaml.Unmarshal
// so newer bundle additions do not break older binaries.
type usageSidecarFileDoc struct {
	SchemaVersion int              `yaml:"schema_version"`
	Usage         usageSidecarBody `yaml:"usage"`
}

type usageSidecarBody struct {
	Live api.HarnessUsageLiveCapabilities `yaml:"live"`
}

// ParseUsageSidecar strictly parses and validates the bytes of a
// UsageSidecarFile (schema_version: 1). Unknown keys at any level (including
// usage.trajectory) are ignored for forward compatibility. Runtime loaders do
// not fail on its error (they warn and leave the capability nil/unknown); tests
// of the bundled files call it directly so a bad bundled file fails CI.
func ParseUsageSidecar(data []byte) (*api.HarnessUsageCapabilities, error) {
	var doc usageSidecarFileDoc
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return nil, fmt.Errorf("invalid %s: %w", UsageSidecarFile, err)
	}
	if doc.SchemaVersion != UsageSidecarSchemaVersion {
		return nil, fmt.Errorf("invalid %s: schema_version must be %d, got %d", UsageSidecarFile, UsageSidecarSchemaVersion, doc.SchemaVersion)
	}
	live := doc.Usage.Live
	if !api.ValidUsageLiveSource(live.Source) {
		return nil, fmt.Errorf("invalid %s: usage.live.source must be one of native, hooks, sqlite_native, file_tailer, none; got %q", UsageSidecarFile, live.Source)
	}

	seenTokens, err := validateUsageTokenTypeList("usage.live.token_types", live.TokenTypes)
	if err != nil {
		return nil, fmt.Errorf("invalid %s: %w", UsageSidecarFile, err)
	}
	seenNA, err := validateUsageTokenTypeList("usage.live.not_applicable_token_types", live.NotApplicableTokenTypes)
	if err != nil {
		return nil, fmt.Errorf("invalid %s: %w", UsageSidecarFile, err)
	}
	for tt := range seenNA {
		if seenTokens[tt] {
			return nil, fmt.Errorf("invalid %s: token type %q cannot appear in both token_types and not_applicable_token_types", UsageSidecarFile, tt)
		}
	}

	if live.Source == api.UsageLiveSourceNone {
		if len(live.TokenTypes) > 0 {
			return nil, fmt.Errorf("invalid %s: usage.live.token_types must be empty when source is none", UsageSidecarFile)
		}
		if live.CountsFailedCalls {
			return nil, fmt.Errorf("invalid %s: usage.live.counts_failed_calls must be false when source is none", UsageSidecarFile)
		}
	}

	out := &api.HarnessUsageCapabilities{
		SchemaVersion: doc.SchemaVersion,
		Live: api.HarnessUsageLiveCapabilities{
			Source:            live.Source,
			CountsFailedCalls: live.CountsFailedCalls,
		},
	}
	if len(live.TokenTypes) > 0 {
		out.Live.TokenTypes = append([]string(nil), live.TokenTypes...)
	}
	if len(live.NotApplicableTokenTypes) > 0 {
		out.Live.NotApplicableTokenTypes = append([]string(nil), live.NotApplicableTokenTypes...)
	}
	return out, nil
}

func validateUsageTokenTypeList(field string, list []string) (map[string]bool, error) {
	seen := make(map[string]bool, len(list))
	for _, tt := range list {
		if !telemetrycontract.ValidTokenType(tt) {
			return nil, fmt.Errorf("%s contains unknown token type %q", field, tt)
		}
		if seen[tt] {
			return nil, fmt.Errorf("%s contains duplicate token type %q", field, tt)
		}
		seen[tt] = true
	}
	return seen, nil
}

// ApplyUsageSidecarBytes parses raw usage.yaml bytes and records the
// capability on entry. Invalid or unsupported-version content logs a
// deduplicated warning and leaves entry.UsageSidecar nil (unknown capability);
// it never fails the harness-config load.
func ApplyUsageSidecarBytes(entry *HarnessConfigEntry, data []byte, where string) {
	caps, err := ParseUsageSidecar(data)
	if err != nil {
		warnUsageSidecar(where, err)
		entry.UsageSidecar = nil
		return
	}
	entry.UsageSidecar = caps
}

// ApplyUsageSidecarError records an unreadable/oversize usage.yaml error on
// entry by logging a deduplicated warning and leaving entry.UsageSidecar nil.
func ApplyUsageSidecarError(entry *HarnessConfigEntry, err error, where string) {
	warnUsageSidecar(where, fmt.Errorf("invalid %s: %w", UsageSidecarFile, err))
	entry.UsageSidecar = nil
}

// ApplyUsageSidecarDir records dir's UsageSidecarFile on entry. A missing file
// leaves entry unchanged; an unreadable or invalid one warns and leaves
// UsageSidecar nil.
func ApplyUsageSidecarDir(entry *HarnessConfigEntry, dir string) {
	p := filepath.Join(dir, UsageSidecarFile)
	data, err := os.ReadFile(p)
	if errors.Is(err, fs.ErrNotExist) {
		return
	}
	if err != nil {
		ApplyUsageSidecarError(entry, err, p)
		return
	}
	ApplyUsageSidecarBytes(entry, data, p)
}

// ApplyUsageSidecarFS is ApplyUsageSidecarDir for a bundle directory dir
// inside fsys (for example the embedded harnesses).
func ApplyUsageSidecarFS(entry *HarnessConfigEntry, fsys fs.FS, dir string) {
	p := path.Join(dir, UsageSidecarFile)
	data, err := fs.ReadFile(fsys, p)
	if errors.Is(err, fs.ErrNotExist) {
		return
	}
	if err != nil {
		ApplyUsageSidecarError(entry, err, p)
		return
	}
	ApplyUsageSidecarBytes(entry, data, p)
}

func applyUsageSidecarFromReader(entry *HarnessConfigEntry, read func(name string) ([]byte, error), where string) {
	data, err := read(UsageSidecarFile)
	if errors.Is(err, fs.ErrNotExist) {
		return
	}
	if err != nil {
		ApplyUsageSidecarError(entry, err, where)
		return
	}
	ApplyUsageSidecarBytes(entry, data, where)
}

func warnUsageSidecar(where string, err error) {
	if _, seen := invalidSidecarWarned.LoadOrStore(where+"\x00"+err.Error(), true); !seen {
		slog.Warn("ignoring invalid harness-config usage.yaml; usage capability treated as unknown",
			"path", where, "error", err)
	}
}
