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
	"bytes"
	"context"
	"encoding/json"
	"errors"
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
	// FileValue is the profile's settings-file value.
	FileValue string `json:"file_value,omitempty"`
	// InheritedSource and InheritedValue are what the profile resolves to
	// without a value saved for it in the hub: what the editor shows as
	// inherited, also while an admin clears a saved value.
	InheritedSource string `json:"inherited_source"`
	InheritedValue  string `json:"inherited_value,omitempty"`
}

// imageRegistryProvenancePageSize is the broker listing page size for
// node env overrides (a variable so tests can page with few brokers).
var imageRegistryProvenancePageSize = 200

// imageRegistryProvenance builds the provenance report. A broker listing
// failure is logged and leaves the overrides partial.
func (s *Server) imageRegistryProvenance(ctx context.Context, ops *OperationalSettings) *ImageRegistryProvenance {
	if ops == nil {
		return nil
	}
	top := s.hubScopeImageRegistry()
	// Every value shown is redacted (config.RedactImageRegistry): a node env
	// is operator input that is never validated.
	p := &ImageRegistryProvenance{Source: string(top.Source), Value: config.RedactImageRegistry(top.Value)}

	in := ops.ImageRegistryHubInputs()
	for _, name := range in.Catalog {
		layers := in.Layers(name, "")
		r := config.ResolveImageRegistryLayers(layers)
		layers.HubDBProfile = ""
		inherited := config.ResolveImageRegistryLayers(layers)
		if p.Profiles == nil {
			p.Profiles = map[string]ImageRegistryProfileProvenance{}
		}
		p.Profiles[name] = ImageRegistryProfileProvenance{
			Source:    string(r.Source),
			Value:     config.RedactImageRegistry(r.Value),
			FileValue: config.RedactImageRegistry(in.FileTier.ProfileValue(name, in.Catalog)),

			InheritedSource: string(inherited.Source),
			InheritedValue:  config.RedactImageRegistry(inherited.Value),
		}
	}

	if s.store == nil {
		return p
	}
	cursor := ""
	for {
		page, err := s.store.ListRuntimeBrokers(ctx, store.RuntimeBrokerFilter{},
			store.ListOptions{Limit: imageRegistryProvenancePageSize, Cursor: cursor, SkipTotalCount: true})
		if err == nil && page == nil {
			err = errors.New("no result")
		}
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
				BrokerID: b.ID, BrokerName: b.Name, Value: config.RedactImageRegistry(v), Differs: v != top.Value,
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

// imageRegistryProvenanceKey is the response key of the provenance report.
const imageRegistryProvenanceKey = "image_registry_provenance"

// dropImageRegistryProvenance removes the read-only image_registry_provenance
// from a server-config PUT body, matched as the typed decode matches member
// names. A client that sends a GET body back gets a 200 even when the report
// changed in between (a broker heartbeat changes it at any time); the key is
// never stored. Any other body is returned as it is.
func dropImageRegistryProvenance(rawBody []byte) []byte {
	return dropTopLevelMember(rawBody, imageRegistryProvenanceKey)
}

// dropTopLevelMember returns rawBody without its top-level member key
// (matched as encoding/json matches names, foldJSONMemberName), or rawBody
// itself when it has none or does not decode as an object.
func dropTopLevelMember(rawBody []byte, key string) []byte {
	dec := json.NewDecoder(bytes.NewReader(rawBody))
	dec.UseNumber()
	var top map[string]json.RawMessage
	if err := dec.Decode(&top); err != nil || top == nil {
		return rawBody
	}
	want := foldJSONMemberName(key)
	dropped := false
	for name := range top {
		if foldJSONMemberName(name) == want {
			delete(top, name)
			dropped = true
		}
	}
	if !dropped {
		return rawBody
	}
	out, err := json.Marshal(top)
	if err != nil {
		return rawBody
	}
	return out
}

// brokerForDisplay returns b with the reported node image registry redacted
// for display (config.RedactImageRegistry). The capabilities are copied, so
// the stored broker and the value dispatch resolves with are unchanged.
// The version is bounded (brokerVersionToken), which also covers rows
// stored before the bound existed.
func brokerForDisplay(b store.RuntimeBroker) store.RuntimeBroker {
	b.Version = brokerVersionToken(b.Version)
	if b.Capabilities == nil || b.Capabilities.NodeImageRegistry == "" {
		return b
	}
	if r := config.RedactImageRegistry(b.Capabilities.NodeImageRegistry); r != b.Capabilities.NodeImageRegistry {
		c := *b.Capabilities
		c.NodeImageRegistry = r
		b.Capabilities = &c
	}
	return b
}
