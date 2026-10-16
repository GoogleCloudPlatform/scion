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

package hub

import (
	"context"
	"log/slog"
	"sort"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// ImageRegistryProvenance reports where the image_registry in effect comes
// from (ptone/scion#3916). It is read-only: the editable image_registry
// field of the admin settings shows the saved value, else this replica's
// settings-file value, never an environment value.
type ImageRegistryProvenance struct {
	// Source and Value are the hub-scope winner (config.ImageRegistrySource:
	// node_env, hub_db, file or default): what hub-wide reads use, with the
	// hub process's own SCION_IMAGE_REGISTRY as the node env.
	Source string `json:"source"`
	Value  string `json:"value,omitempty"`
	// NodeEnvOverrides lists every Runtime Broker that reports a
	// SCION_IMAGE_REGISTRY: it beats every settings value for the agents that
	// broker runs. Differs marks a value other than Value.
	NodeEnvOverrides []ImageRegistryNodeEnvOverride `json:"node_env_overrides,omitempty"`
	// Profiles maps each profile of the catalog in effect to the registry an
	// agent with that profile resolves to on a broker without a node env.
	Profiles map[string]ImageRegistryProfileProvenance `json:"profiles,omitempty"`
}

// ImageRegistryNodeEnvOverride is a Runtime Broker's reported node env.
type ImageRegistryNodeEnvOverride struct {
	BrokerID   string `json:"broker_id"`
	BrokerName string `json:"broker_name,omitempty"`
	Value      string `json:"value"`
	Differs    bool   `json:"differs"`
}

// ImageRegistryProfileProvenance is a profile's image_registry in effect
// (without a node env) and its settings-file value.
type ImageRegistryProfileProvenance struct {
	Source string `json:"source"`
	Value  string `json:"value,omitempty"`
	// FileValue is the profile's settings-file value: what it inherits
	// when no value is saved for it in the hub.
	FileValue string `json:"file_value,omitempty"`
}

// imageRegistryProvenancePageSize is the broker listing page size for
// node env overrides.
const imageRegistryProvenancePageSize = 200

// imageRegistryProvenance builds the provenance report. A broker listing
// failure is logged and leaves the overrides partial.
func (s *Server) imageRegistryProvenance(ctx context.Context, ops *OperationalSettings) *ImageRegistryProvenance {
	if ops == nil {
		return nil
	}
	top := s.hubScopeImageRegistry()
	p := &ImageRegistryProvenance{Source: string(top.Source), Value: top.Value}

	in := ops.ImageRegistryHubInputs()
	for _, name := range in.Catalog {
		r := config.ResolveImageRegistryLayers(in.Layers(name, ""))
		if p.Profiles == nil {
			p.Profiles = map[string]ImageRegistryProfileProvenance{}
		}
		p.Profiles[name] = ImageRegistryProfileProvenance{
			Source:    string(r.Source),
			Value:     r.Value,
			FileValue: in.FileTier.ProfileValue(name, in.Catalog),
		}
	}

	if s.store == nil {
		return p
	}
	cursor := ""
	for {
		page, err := s.store.ListRuntimeBrokers(ctx, store.RuntimeBrokerFilter{},
			store.ListOptions{Limit: imageRegistryProvenancePageSize, Cursor: cursor, SkipTotalCount: true})
		if err != nil {
			slog.Warn("image_registry provenance: listing brokers failed; node env overrides are partial", "error", err)
			break
		}
		for _, b := range page.Items {
			if b.Capabilities == nil || b.Capabilities.NodeImageRegistry == "" {
				continue
			}
			v := b.Capabilities.NodeImageRegistry
			p.NodeEnvOverrides = append(p.NodeEnvOverrides, ImageRegistryNodeEnvOverride{
				BrokerID: b.ID, BrokerName: b.Name, Value: v, Differs: v != top.Value,
			})
		}
		if page.NextCursor == "" || len(page.Items) == 0 {
			break
		}
		cursor = page.NextCursor
	}
	sort.Slice(p.NodeEnvOverrides, func(i, j int) bool {
		a, b := p.NodeEnvOverrides[i], p.NodeEnvOverrides[j]
		if a.BrokerName != b.BrokerName {
			return a.BrokerName < b.BrokerName
		}
		return a.BrokerID < b.BrokerID
	})
	return p
}
