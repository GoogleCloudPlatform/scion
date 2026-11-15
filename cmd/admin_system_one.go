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

package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"
)

const (
	adminSystemOneDefaultTimeout     = 60 * time.Second
	maxAdminSystemOneAPIKeyReadBytes = 16 * 1024
	maxAdminSystemOneProbeReadBytes  = 512 * 1024
)

var systemOneAdminClientLoader = func() (hubclient.SystemOneService, error) {
	_, client, err := loadHubClient()
	if err != nil {
		return nil, err
	}
	return client.SystemOne(), nil
}

// adminSystemOneCmd groups Hub System One administrative operations.
var adminSystemOneCmd = &cobra.Command{
	Use:   "system-one",
	Short: "Manage Hub System One model provider settings",
	Long: `Administrative operations for Hub-managed System One provider instances.

These commands talk to the running Scion Hub over its administrative API and
require a session-authenticated Hub admin or super-admin identity (user access
tokens and agent credentials are refused).`,
}

// adminSystemOneInstanceCmd groups Hub System One provider instance operations.
var adminSystemOneInstanceCmd = &cobra.Command{
	Use:   "instance",
	Short: "Manage Hub System One provider instances",
	Long: `Create, list, delete, and probe Hub-managed System One provider instances.

Credentials supplied when creating an instance are stored in the Hub's internal
secret backend and are never returned in command output or responses.`,
}

func init() {
	adminSystemOneCmd.AddCommand(adminSystemOneInstanceCmd)
	adminCmd.AddCommand(adminSystemOneCmd)
}

func systemOneAdminService(cmd *cobra.Command, timeout time.Duration) (hubclient.SystemOneService, context.Context, context.CancelFunc, error) {
	svc, err := systemOneAdminClientLoader()
	if err != nil {
		return nil, nil, nil, err
	}
	parent := cmd.Context()
	if parent == nil {
		parent = context.Background()
	}
	ctx, cancel := context.WithTimeout(parent, timeout)
	return svc, ctx, cancel, nil
}

func parseAdminSystemOneKeyValues(entries []string, flagName string) (map[string]string, error) {
	if len(entries) == 0 {
		return nil, nil
	}
	out := make(map[string]string, len(entries))
	for _, entry := range entries {
		k, v, ok := strings.Cut(entry, "=")
		k = strings.TrimSpace(k)
		if !ok || k == "" {
			return nil, fmt.Errorf("invalid %s entry: expected KEY=VALUE", flagName)
		}
		out[k] = v
	}
	return out, nil
}

func decodeYAMLOrJSONToRawMessage(data []byte) (json.RawMessage, error) {
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) == 0 {
		return nil, fmt.Errorf("spec document is empty")
	}
	if json.Valid(trimmed) {
		var compact bytes.Buffer
		if err := json.Compact(&compact, trimmed); err != nil {
			return nil, fmt.Errorf("compact JSON spec: %w", err)
		}
		return json.RawMessage(compact.Bytes()), nil
	}

	var doc any
	if err := yaml.Unmarshal(trimmed, &doc); err != nil {
		return nil, fmt.Errorf("parse spec as JSON or YAML: %w", err)
	}
	norm, err := normalizeYAMLDocumentValue(doc)
	if err != nil {
		return nil, err
	}
	if norm == nil {
		return nil, fmt.Errorf("spec document is empty")
	}
	encoded, err := json.Marshal(norm)
	if err != nil {
		return nil, fmt.Errorf("encode normalized spec as JSON: %w", err)
	}
	return json.RawMessage(encoded), nil
}

func normalizeYAMLDocumentValue(v any) (any, error) {
	switch x := v.(type) {
	case map[string]any:
		m := make(map[string]any, len(x))
		for k, val := range x {
			nv, err := normalizeYAMLDocumentValue(val)
			if err != nil {
				return nil, err
			}
			m[k] = nv
		}
		return m, nil
	case map[any]any:
		m := make(map[string]any, len(x))
		for k, val := range x {
			ks, ok := k.(string)
			if !ok {
				return nil, fmt.Errorf("YAML mapping keys must be strings")
			}
			nv, err := normalizeYAMLDocumentValue(val)
			if err != nil {
				return nil, err
			}
			m[ks] = nv
		}
		return m, nil
	case []any:
		s := make([]any, len(x))
		for i, item := range x {
			nv, err := normalizeYAMLDocumentValue(item)
			if err != nil {
				return nil, err
			}
			s[i] = nv
		}
		return s, nil
	default:
		return v, nil
	}
}
