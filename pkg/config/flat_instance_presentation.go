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
	"sort"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
)

// FlatInstancePresentation is the largest part of a flat Runtime Broker
// instance entry that may ever be shown outside the Runtime Broker
// (.design/flat-runtime-brokers/contract-appendix-p3.md FC4, table FC4-a).
// Its fields are exactly the "published" column of FC4-a. Local paths,
// kubeconfig contexts, service-account policy, environment values and
// secret projection details have no field here, so they cannot be
// published through it. A field added to the instance schema later is not
// publishable until FC4-a lists it and this type gains it.
//
// Build it only with PresentInstance. P3 publishes nothing new to the Hub;
// whether and how a later change sends this set is decided there.
type FlatInstancePresentation struct {
	Key           string                         `json:"key"`
	Name          string                         `json:"name"`
	RuntimeTarget *FlatRuntimeTargetPresentation `json:"runtime_target,omitempty"`
	AgentDefaults *FlatAgentDefaultsPresentation `json:"agent_defaults,omitempty"`
}

// FlatRuntimeTargetPresentation is the publishable part of an instance's
// runtime_target. context, kubeconfig, kubernetes_service_account_mappings,
// kubernetes_block_service_account and home_storage_leaf are never
// published.
type FlatRuntimeTargetPresentation struct {
	Type                     string            `json:"type"`
	DisplayName              string            `json:"display_name,omitempty"`
	Namespace                string            `json:"namespace,omitempty"`
	GKE                      bool              `json:"gke,omitempty"`
	PriorityClassName        string            `json:"priority_class_name,omitempty"`
	SharedDirStorageClass    string            `json:"shared_dir_storage_class,omitempty"`
	SharedDirSize            string            `json:"shared_dir_size,omitempty"`
	SafeToEvict              *bool             `json:"safe_to_evict,omitempty"`
	SharedDirStorageBackend  string            `json:"shared_dir_storage_backend,omitempty"`
	SharedDirStorageBackends map[string]string `json:"shared_dir_storage_backends,omitempty"`
	HomeStorageBackend       string            `json:"home_storage_backend,omitempty"`
}

// FlatAgentDefaultsPresentation is the publishable part of an instance's
// agent_defaults.
type FlatAgentDefaultsPresentation struct {
	DefaultHarnessConfig string                                     `json:"default_harness_config,omitempty"`
	ImageRegistry        string                                     `json:"image_registry,omitempty"`
	CloneDepth           api.CloneDepth                             `json:"clone_depth,omitempty"`
	Resources            *api.ResourceSpec                          `json:"resources,omitempty"`
	Volumes              []FlatVolumePresentation                   `json:"volumes,omitempty"`
	Secrets              []FlatSecretPresentation                   `json:"secrets,omitempty"`
	HarnessOverrides     map[string]FlatHarnessOverridePresentation `json:"harness_overrides,omitempty"`
}

// FlatVolumePresentation is the publishable part of a volume mount: where
// it is mounted, whether read-only, and its kind. source, bucket, prefix,
// server, mode and volume_name are node-local layout and never published.
type FlatVolumePresentation struct {
	Target   string `json:"target"`
	ReadOnly bool   `json:"read_only,omitempty"`
	Type     string `json:"type,omitempty"`
}

// FlatSecretPresentation is the publishable part of a secret declaration.
// A declaration never carries a value; target and alternative_env_keys are
// never published.
type FlatSecretPresentation struct {
	Key         string `json:"key"`
	Type        string `json:"type,omitempty"`
	Description string `json:"description,omitempty"`
}

// FlatHarnessOverridePresentation is the publishable part of one harness
// override. env is published as its sorted keys only, never its values,
// which may hold credentials.
type FlatHarnessOverridePresentation struct {
	Image            string                   `json:"image,omitempty"`
	ImagePullPolicy  string                   `json:"image_pull_policy,omitempty"`
	AuthSelectedType string                   `json:"auth_selected_type,omitempty"`
	User             string                   `json:"user,omitempty"`
	EnvKeys          []string                 `json:"env_keys,omitempty"`
	Volumes          []FlatVolumePresentation `json:"volumes,omitempty"`
	Resources        *api.ResourceSpec        `json:"resources,omitempty"`
}

// PresentInstance returns the publishable form of inst (FC4-a). The result
// shares no map, slice or pointer with inst, so changing one never changes
// the other.
func PresentInstance(inst V1RuntimeBrokerInstanceConfig) FlatInstancePresentation {
	out := FlatInstancePresentation{Key: inst.Key, Name: inst.Name}
	if t := inst.RuntimeTarget; t != nil {
		out.RuntimeTarget = &FlatRuntimeTargetPresentation{
			Type:                     t.Type,
			DisplayName:              t.DisplayName,
			Namespace:                t.Namespace,
			GKE:                      t.GKE,
			PriorityClassName:        t.PriorityClassName,
			SharedDirStorageClass:    t.SharedDirStorageClass,
			SharedDirSize:            t.SharedDirSize,
			SafeToEvict:              copyBoolPtr(t.SafeToEvict),
			SharedDirStorageBackend:  t.SharedDirStorageBackend,
			SharedDirStorageBackends: copyStringMap(t.SharedDirStorageBackends),
			HomeStorageBackend:       t.HomeStorageBackend,
		}
	}
	if d := inst.AgentDefaults; d != nil {
		ad := &FlatAgentDefaultsPresentation{
			DefaultHarnessConfig: d.DefaultHarnessConfig,
			ImageRegistry:        d.ImageRegistry,
			CloneDepth:           d.CloneDepth,
			Resources:            copyResourceSpec(d.Resources),
			Volumes:              presentVolumes(d.Volumes),
		}
		for _, s := range d.Secrets {
			ad.Secrets = append(ad.Secrets, FlatSecretPresentation{Key: s.Key, Type: s.Type, Description: s.Description})
		}
		if len(d.HarnessOverrides) > 0 {
			ad.HarnessOverrides = make(map[string]FlatHarnessOverridePresentation, len(d.HarnessOverrides))
			for name, o := range d.HarnessOverrides {
				ad.HarnessOverrides[name] = FlatHarnessOverridePresentation{
					Image:            o.Image,
					ImagePullPolicy:  o.ImagePullPolicy,
					AuthSelectedType: o.AuthSelectedType,
					User:             o.User,
					EnvKeys:          sortedEnvKeys(o.Env),
					Volumes:          presentVolumes(o.Volumes),
					Resources:        copyResourceSpec(o.Resources),
				}
			}
		}
		out.AgentDefaults = ad
	}
	return out
}

func presentVolumes(vols []api.VolumeMount) []FlatVolumePresentation {
	if len(vols) == 0 {
		return nil
	}
	out := make([]FlatVolumePresentation, len(vols))
	for i, v := range vols {
		out[i] = FlatVolumePresentation{Target: v.Target, ReadOnly: v.ReadOnly, Type: v.Type}
	}
	return out
}

func sortedEnvKeys(env map[string]string) []string {
	if len(env) == 0 {
		return nil
	}
	keys := make([]string, 0, len(env))
	for k := range env {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
