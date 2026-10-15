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
	"fmt"
	"io"
	"sort"
	"strings"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/clitime"
)

// presentationCaption heads the reported defaults of a flat Runtime Broker:
// they are what the instance reported, defaults for new agents only, and
// not a check of cluster permissions.
const presentationCaption = "defaults for new agents only, not the configuration of existing agents and not a check of cluster permissions"

// writeRuntimeTargetSections writes the "Runtime target" and "Reported
// defaults" sections of `scion hub brokers info` for a flat Runtime Broker:
// the stored target descriptor, then the presentation the instance last
// reported (FC4-a fields only; environment variables by name only). Unset
// fields are left out, except safe_to_evict, which is always shown as
// unset, true or false.
func writeRuntimeTargetSections(w io.Writer, target *api.RuntimeTargetDescriptor, p *api.RuntimeTargetPresentation, observedAt *time.Time) {
	if target == nil {
		return
	}
	line := func(label, value string) {
		if value != "" {
			_, _ = fmt.Fprintf(w, "%-27s %s\n", label+":", value)
		}
	}
	_, _ = fmt.Fprintln(w, "Runtime target")
	_, _ = fmt.Fprintln(w, "--------------")
	line("ID", target.ID)
	line("Type", target.Type)
	line("Display name", target.DisplayName)
	if p != nil && p.Target != nil {
		line("Namespace", p.Target.Namespace)
	}

	_, _ = fmt.Fprintln(w)
	if p == nil {
		_, _ = fmt.Fprintln(w, "Reported defaults: none reported yet")
		return
	}
	_, _ = fmt.Fprintln(w, "Reported defaults")
	_, _ = fmt.Fprintln(w, "-----------------")
	if observedAt != nil {
		_, _ = fmt.Fprintf(w, "As reported by the instance at %s; %s.\n", clitime.Format(*observedAt, clitime.Full), presentationCaption)
	} else {
		_, _ = fmt.Fprintf(w, "As reported by the instance; %s.\n", presentationCaption)
	}
	if t := p.Target; t != nil {
		line("Safe to evict", triState(t.SafeToEvict))
		if t.GKE {
			line("GKE", "true")
		}
		line("Priority class", t.PriorityClassName)
		line("Shared dir storage class", t.SharedDirStorageClass)
		line("Shared dir size", t.SharedDirSize)
		line("Shared dir storage backend", t.SharedDirStorageBackend)
		line("Shared dir backends", sortedPairs(t.SharedDirStorageBackends))
		line("Home storage backend", t.HomeStorageBackend)
	}
	d := p.AgentDefaults
	if d == nil {
		return
	}
	line("Default harness config", d.DefaultHarnessConfig)
	line("Image registry", d.ImageRegistry)
	line("Clone depth", string(d.CloneDepth))
	line("Resources", formatResourceSpec(d.Resources))
	if len(d.Volumes) > 0 {
		_, _ = fmt.Fprintln(w, "Volumes:")
		for _, v := range d.Volumes {
			_, _ = fmt.Fprintf(w, "  - %s\n", formatPresentedVolume(v))
		}
	}
	if len(d.Secrets) > 0 {
		_, _ = fmt.Fprintln(w, "Secrets:")
		for _, s := range d.Secrets {
			text := s.Key
			if s.Type != "" {
				text += " (" + s.Type + ")"
			}
			if s.Description != "" {
				text += ": " + s.Description
			}
			_, _ = fmt.Fprintf(w, "  - %s\n", text)
		}
	}
	if len(d.HarnessOverrides) > 0 {
		_, _ = fmt.Fprintln(w, "Harness overrides:")
		names := make([]string, 0, len(d.HarnessOverrides))
		for name := range d.HarnessOverrides {
			names = append(names, name)
		}
		sort.Strings(names)
		for _, name := range names {
			_, _ = fmt.Fprintf(w, "  - %s\n", name)
			o := d.HarnessOverrides[name]
			sub := func(label, value string) {
				if value != "" {
					_, _ = fmt.Fprintf(w, "      %-18s %s\n", label+":", value)
				}
			}
			sub("Image", o.Image)
			sub("Image pull policy", o.ImagePullPolicy)
			sub("Auth type", o.AuthSelectedType)
			sub("User", o.User)
			sub("Env keys", strings.Join(o.EnvKeys, ", "))
			vols := make([]string, len(o.Volumes))
			for i, v := range o.Volumes {
				vols[i] = formatPresentedVolume(v)
			}
			sub("Volumes", strings.Join(vols, "; "))
			sub("Resources", formatResourceSpec(o.Resources))
		}
	}
}

// triState renders an optional bool as unset, true or false.
func triState(b *bool) string {
	switch {
	case b == nil:
		return "unset"
	case *b:
		return "true"
	default:
		return "false"
	}
}

func sortedPairs(m map[string]string) string {
	if len(m) == 0 {
		return ""
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, len(keys))
	for i, k := range keys {
		parts[i] = k + "=" + m[k]
	}
	return strings.Join(parts, ", ")
}

func formatPresentedVolume(v api.PresentedVolume) string {
	var attrs []string
	if v.Type != "" {
		attrs = append(attrs, v.Type)
	}
	if v.ReadOnly {
		attrs = append(attrs, "read-only")
	}
	if len(attrs) == 0 {
		return v.Target
	}
	return v.Target + " (" + strings.Join(attrs, ", ") + ")"
}

func formatResourceSpec(r *api.ResourceSpec) string {
	if r == nil {
		return ""
	}
	list := func(name string, l api.ResourceList) string {
		var parts []string
		if l.CPU != "" {
			parts = append(parts, "cpu="+l.CPU)
		}
		if l.Memory != "" {
			parts = append(parts, "memory="+l.Memory)
		}
		if len(parts) == 0 {
			return ""
		}
		return name + " " + strings.Join(parts, " ")
	}
	var parts []string
	for _, s := range []string{list("requests", r.Requests), list("limits", r.Limits)} {
		if s != "" {
			parts = append(parts, s)
		}
	}
	if r.Disk != "" {
		parts = append(parts, "disk "+r.Disk)
	}
	return strings.Join(parts, "; ")
}
